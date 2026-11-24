// Package mcp is a small Model Context Protocol server over stdio: JSON-RPC
// 2.0, one message per line, offering tools. That is all nightshift needs
// to be driven by Claude Code, Codex, Cursor and other agents, and small
// enough not to need a dependency.
//
// Specification: https://modelcontextprotocol.io/specification
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Versions are the protocol revisions this server speaks, newest first. A
// tools-only server is the same in all of them.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Tool is one tool the server offers.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *Annotations   `json:"annotations,omitempty"`
	// Handler runs the tool. Its result is returned as JSON text and as
	// structured content; an error is a tool error the model can read.
	Handler func(ctx context.Context, args json.RawMessage) (any, error) `json:"-"`
}

// Annotations are hints about a tool's behavior for the client.
type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// Server serves tools on one connection.
type Server struct {
	Name         string
	Title        string
	Version      string
	Instructions string
	Tools        []Tool

	mu  sync.Mutex
	out io.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from in and writes responses to out until in ends or
// ctx is done. Nothing else may write to out.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReaderSize(in, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handleLine(ctx, line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	line = bytes.TrimSpace(line)
	if line[0] == '[' { // a batch, from clients of the 2025-03-26 revision
		var batch []json.RawMessage
		if err := json.Unmarshal(line, &batch); err != nil {
			s.write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}})
			return
		}
		var out []response
		for _, m := range batch {
			if resp, ok := s.handle(ctx, m); ok {
				out = append(out, resp)
			}
		}
		if len(out) > 0 {
			s.write(out)
		}
		return
	}
	if resp, ok := s.handle(ctx, line); ok {
		s.write(resp)
	}
}

// handle answers one message; ok is false for notifications and responses,
// which get no answer.
func (s *Server) handle(ctx context.Context, msg []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(msg, &req); err != nil {
		return response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}}, true
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return response{}, false // a notification (initialized, canceled, ...)
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" || req.Method == "" {
		resp.Error = &rpcError{codeInvalidRequest, "invalid request"}
		return resp, true
	}
	var err *rpcError
	switch req.Method {
	case "initialize":
		resp.Result, err = s.initialize(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.Tools}
	case "tools/call":
		resp.Result, err = s.call(ctx, req.Params)
	default:
		err = &rpcError{codeMethodNotFound, "method not found: " + req.Method}
	}
	resp.Error = err
	return resp, true
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "invalid initialize params"}
		}
	}
	version := Versions[0]
	if slices.Contains(Versions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	info := map[string]any{"name": s.Name, "version": s.Version}
	if s.Title != "" {
		info["title"] = s.Title
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      info,
	}
	if s.Instructions != "" {
		result["instructions"] = s.Instructions
	}
	return result, nil
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeInvalidParams, "invalid tools/call params"}
	}
	i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name })
	if i < 0 {
		return nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name}
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	out, err := s.Tools[i].Handler(ctx, args)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Error: " + err.Error()}},
			"isError": true,
		}, nil
	}
	text, merr := json.MarshalIndent(out, "", "  ")
	if merr != nil {
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("encode result: %v", merr)}
	}
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}
	if isObject(text) {
		result["structuredContent"] = json.RawMessage(text)
	}
	return result, nil
}

func isObject(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{'
}

func (s *Server) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

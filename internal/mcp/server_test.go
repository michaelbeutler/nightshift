package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func serve(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("response %q is not JSON: %v", l, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func testServer() *Server {
	return &Server{
		Name: "nightshift", Version: "v0.0.0-test", Instructions: "be nice",
		Tools: []Tool{
			{
				Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"},
				Handler: func(_ context.Context, args json.RawMessage) (any, error) {
					var in struct{ Say string }
					_ = json.Unmarshal(args, &in)
					if in.Say == "fail" {
						return nil, errors.New("it failed")
					}
					return map[string]string{"said": in.Say}, nil
				},
			},
		},
	}
}

func TestHandshakeAndTools(t *testing.T) {
	t.Parallel()
	msgs := serve(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"say":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"say":"fail"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`not json`,
	)
	if len(msgs) != 8 {
		t.Fatalf("got %d responses, want 8 (the notification gets none): %v", len(msgs), msgs)
	}
	init := msgs[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["instructions"] != "be nice" {
		t.Errorf("initialize = %v", init)
	}
	if msgs[1]["id"] != "two" || len(msgs[1]["result"].(map[string]any)["tools"].([]any)) != 1 {
		t.Errorf("tools/list = %v", msgs[1])
	}
	call := msgs[2]["result"].(map[string]any)
	if call["structuredContent"].(map[string]any)["said"] != "hi" || call["isError"] != nil {
		t.Errorf("tools/call = %v", call)
	}
	failed := msgs[3]["result"].(map[string]any)
	if failed["isError"] != true || !strings.Contains(failed["content"].([]any)[0].(map[string]any)["text"].(string), "it failed") {
		t.Errorf("failing tool = %v", failed)
	}
	if msgs[4]["error"].(map[string]any)["code"] != float64(codeInvalidParams) {
		t.Errorf("unknown tool = %v", msgs[4])
	}
	if _, ok := msgs[5]["result"]; !ok {
		t.Errorf("ping = %v", msgs[5])
	}
	if msgs[6]["error"].(map[string]any)["code"] != float64(codeMethodNotFound) {
		t.Errorf("unknown method = %v", msgs[6])
	}
	if msgs[7]["error"].(map[string]any)["code"] != float64(codeParse) {
		t.Errorf("parse error = %v", msgs[7])
	}
}

func TestUnknownProtocolVersionGetsTheNewest(t *testing.T) {
	t.Parallel()
	msgs := serve(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if got := msgs[0]["result"].(map[string]any)["protocolVersion"]; got != Versions[0] {
		t.Errorf("protocolVersion = %v, want %s", got, Versions[0])
	}
}

func TestBatch(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	s := testServer()
	in := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]` + "\n"
	if err := s.Serve(t.Context(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var batch []map[string]any
	if err := json.Unmarshal(out.Bytes(), &batch); err != nil || len(batch) != 2 {
		t.Errorf("batch response %s (%v)", out.String(), err)
	}
}

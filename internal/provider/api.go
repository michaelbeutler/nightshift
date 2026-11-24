package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// maxBody caps how much of an API response is read.
const maxBody = 1 << 20

// APIError is a REST API call that did not succeed.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

type apiClient struct {
	base    string
	token   string
	headers map[string]string
	env     Env
}

// do sends a JSON request and decodes a JSON response into out (if not nil).
func (c *apiClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.env.UserAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.env.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, routeOf(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, routeOf(path), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: method, Path: routeOf(path), Status: resp.StatusCode, Message: apiMessage(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, routeOf(path), err)
		}
	}
	return nil
}

func routeOf(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

// apiMessage pulls the human part out of a GitHub or GitLab error body:
// {"message": "...", "errors": [{"message": "..."}]} or
// {"message": ["..."]} or {"error": "..."}.
func apiMessage(data []byte) string {
	var v struct {
		Message any `json:"message"`
		Error   any `json:"error"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return clean(string(data), 200)
	}
	var parts []string
	for _, m := range []any{v.Message, v.Error} {
		if s := flatten(m); s != "" {
			parts = append(parts, s)
		}
	}
	for _, e := range v.Errors {
		switch {
		case e.Message != "":
			parts = append(parts, e.Message)
		case e.Code != "":
			parts = append(parts, strings.TrimSpace(e.Field+" "+e.Code))
		}
	}
	return clean(strings.Join(parts, "; "), 300)
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var s []string
		for _, x := range t {
			s = append(s, flatten(x))
		}
		return strings.Join(s, ", ")
	case map[string]any:
		var s []string
		for _, k := range slices.Sorted(maps.Keys(t)) {
			s = append(s, k+" "+flatten(t[k]))
		}
		return strings.Join(s, ", ")
	}
	return ""
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpClient is a minimal REST client over the gateway's bearer-auth API.
// Lives here so auth.go, list.go, and call.go all share the same retry
// + error-rendering logic.
type httpClient struct {
	server string
	token  string
	hc     *http.Client
}

func newClient(server, token string) *httpClient {
	return &httpClient{
		server: strings.TrimRight(server, "/"),
		token:  token,
		hc:     &http.Client{Timeout: 60 * time.Second},
	}
}

// doJSON sends a JSON request (or no body if `in` is nil) and decodes
// the response JSON into `out` (if non-nil). Non-2xx responses are
// returned as an error containing the body's "error" field if present.
func (c *httpClient) doJSON(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.server+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Name the client so the gateway's audit rows record "cli" rather
	// than Go's default agent string.
	req.Header.Set("User-Agent", "toolyard-cli/"+version)
	req.Header.Set("x-toolyard-client", "cli")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s",
			method, path, resp.Status, errMessage(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// errMessage tries to extract a server-side error string from a JSON body
// like {"error":"...","detail":"..."} so the CLI doesn't dump raw JSON
// at the operator.
func errMessage(b []byte) string {
	if len(b) == 0 {
		return "(empty body)"
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err == nil {
		if s, ok := m["error"].(string); ok && s != "" {
			if d, ok := m["detail"].(string); ok && d != "" {
				return s + ": " + d
			}
			return s
		}
	}
	// Fallback: show the first line so we don't spam multi-line stacks.
	if i := bytes.IndexByte(b, '\n'); i > 0 {
		return string(b[:i])
	}
	return string(b)
}

// errAuthExpired is what doJSON normalizes 401s into so callers can show
// a "run `toolyard auth login` again" hint instead of a generic message.
var errAuthExpired = errors.New("agent token rejected (run `toolyard auth login <code>`)")

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// toolyardAPI is a dashboard-session client: it signs in with the local
// username and password and sends the Origin and X-Requested-With headers
// the dashboard routes require behind -public-url.
type toolyardAPI struct {
	base string
	hc   *http.Client
}

func newToolyardAPI(base string) *toolyardAPI {
	jar, _ := cookiejar.New(nil)
	return &toolyardAPI{
		base: strings.TrimRight(base, "/"),
		hc: &http.Client{
			Timeout: 90 * time.Second,
			Jar:     jar,
			// A redirect would resend the password or a secret elsewhere,
			// or turn a POST into a GET that "succeeds" on an HTML page.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// do sends body as JSON and decodes a JSON reply into out. The returned
// error carries the status and a trimmed reply, never the request body.
func (t *toolyardAPI) do(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, t.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Origin", t.base)
	req.Header.Set("X-Requested-With", "toolyard")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// toolyard errors are {"error": "..."}; decode them so the text is
		// plain (no \u0026-style escapes) before anyone scrubs it.
		msg := string(raw)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		// Not shortened here: a cut inside a secret would leave part of it
		// for scrub to miss. Callers scrub, then clip.
		msg = strings.Join(strings.Fields(msg), " ")
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode reply: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (t *toolyardAPI) login(user, password string) error {
	_, err := t.do(http.MethodPost, "/v1/auth/login", map[string]string{"username": user, "password": password}, nil)
	return err
}

func (t *toolyardAPI) serverNames() (map[string]bool, error) {
	var list []struct {
		Name string `json:"name"`
	}
	if _, err := t.do(http.MethodGet, "/v1/servers", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s.Name] = true
	}
	return out, nil
}

func (t *toolyardAPI) secretNames() (map[string]bool, error) {
	var list []struct {
		Name string `json:"name"`
	}
	if _, err := t.do(http.MethodGet, "/v1/secrets", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s.Name] = true
	}
	return out, nil
}

func (t *toolyardAPI) createSecret(name, value, description string) error {
	_, err := t.do(http.MethodPost, "/v1/secrets", map[string]string{
		"name": name, "value": value, "description": description,
	}, nil)
	return err
}

func (t *toolyardAPI) deleteSecret(name string) error {
	_, err := t.do(http.MethodDelete, "/v1/secrets/"+url.PathEscape(name), nil, nil)
	return err
}

// serverResult is the part of a created server the report shows.
type serverResult struct {
	LastStatus string `json:"last_status"`
	LastError  string `json:"last_error"`
	ToolCount  int    `json:"tool_count"`
}

// createServer adds one server. toolyard answers 200 with the server when it
// connected, and 202 with {server, warning} when the row was saved but the
// first connect failed; code is 0 when no reply came back at all.
func (t *toolyardAPI) createServer(s toolyardServer) (serverResult, string, int, error) {
	var reply json.RawMessage
	code, err := t.do(http.MethodPost, "/v1/servers", s, &reply)
	if err != nil {
		return serverResult{}, "", code, err
	}
	if code == http.StatusAccepted {
		var w struct {
			Server  serverResult `json:"server"`
			Warning string       `json:"warning"`
		}
		_ = json.Unmarshal(reply, &w)
		return w.Server, w.Warning, code, nil
	}
	var r serverResult
	_ = json.Unmarshal(reply, &r)
	return r, "", code, nil
}

// hasOAuthClient reports whether toolyard already holds an OAuth app for a
// server.
func (t *toolyardAPI) hasOAuthClient(name string) (bool, error) {
	var st struct {
		HasClient bool `json:"has_client"`
	}
	if _, err := t.do(http.MethodGet, "/v1/servers/"+url.PathEscape(name)+"/oauth", nil, &st); err != nil {
		return false, err
	}
	return st.HasClient, nil
}

// putManualClient stores an OAuth app for a server. toolyard fills in its own
// redirect URI.
func (t *toolyardAPI) putManualClient(name string, body map[string]any) error {
	_, err := t.do(http.MethodPost, "/v1/servers/"+url.PathEscape(name)+"/oauth/manual-client", body, nil)
	return err
}

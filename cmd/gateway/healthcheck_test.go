package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthcheck(t *testing.T) {
	answer := func(code int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	ready := answer(200, `{"status":"ready"}`)
	live := answer(200, `{"status":"live"}`)
	notReady := answer(503, `{"status":"not_ready","reason":"draining"}`)
	notJSON := answer(200, `<html>dashboard</html>`)
	oddCode := answer(500, `{"status":"ready"}`)

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"ready", []string{"-url", ready.URL + "/v1/readyz", "-expect", "ready"}, ""},
		{"live", []string{"-url", live.URL + "/v1/livez", "-expect", "live"}, ""},
		{"live is not ready", []string{"-url", live.URL, "-expect", "ready"}, `status "live", want "ready"`},
		{"not ready", []string{"-url", notReady.URL}, "reason draining"},
		{"html", []string{"-url", notJSON.URL}, "without a JSON status"},
		{"status with an error code", []string{"-url", oddCode.URL}, "HTTP 500"},
		{"bad expect", []string{"-url", ready.URL, "-expect", "up"}, "-expect must be ready or live"},
		{"bad url", []string{"-url", "ftp://example.invalid/x"}, "not an absolute http(s) URL"},
		{"stray argument", []string{"-url", ready.URL, "extra"}, "unexpected arguments"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runHealthcheck(c.args)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestHealthcheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	err := checkHealth(&http.Client{Timeout: 100 * time.Millisecond}, srv.URL, "ready")
	if err == nil {
		t.Fatal("a hung probe passed")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout not applied: %s", time.Since(start))
	}
	if healthcheckTimeout != 3*time.Second {
		t.Fatalf("healthcheck timeout is %s, want 3s", healthcheckTimeout)
	}
}

// The dashboard's SPA fallback must never answer an API, probe or metrics
// path with its HTML: a load balancer would read that as healthy.
func TestStaticHandlerNeverServesHTMLForProbeOrAPIPaths(t *testing.T) {
	h := staticHandler()
	for _, p := range []string{
		"/v1/nope", "/v1/livez/", "/v1/readyz/extra", "/healthz/", "/livez/", "/readyz/",
		"/metrics", "/metrics/", "/metrics/x",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" || strings.Contains(rec.Body.String(), "<") {
			t.Errorf("%s: %q %q, want a JSON 404", p, ct, rec.Body.String())
		}
	}
	// The SPA itself still answers unknown dashboard paths.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/dashboard/route", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("SPA fallback: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

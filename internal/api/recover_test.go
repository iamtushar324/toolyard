package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

func scrapeHTTPPanics(t *testing.T) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	opsmetrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "toolyard_http_panics_total "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("bad sample %q: %v", line, err)
			}
			return f
		}
	}
	return 0
}

// Recover answers 500 and counts the panic; an aborted response is not a
// panic and is not counted.
func TestRecoverCountsHTTPPanics(t *testing.T) {
	before := scrapeHTTPPanics(t)
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if after := scrapeHTTPPanics(t); after != before+1 {
		t.Fatalf("toolyard_http_panics_total = %v, want %v", after, before+1)
	}

	abort := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	if after := scrapeHTTPPanics(t); after != before+1 {
		t.Fatalf("an aborted response was counted: %v", after)
	}
}

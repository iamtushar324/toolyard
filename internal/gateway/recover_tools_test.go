package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

// httpPanics scrapes toolyard_http_panics_total (0 before the first panic).
func httpPanics(t *testing.T) float64 {
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

// A panicking tool handler still becomes an error answer, as with mcp-go's
// WithRecovery, and is now counted.
func TestRecoverToolPanicsAnswersAndCounts(t *testing.T) {
	before := httpPanics(t)
	h := recoverToolPanics(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		panic("boom")
	})
	var req mcp.CallToolRequest
	req.Params.Name = "alpha.first"
	res, err := h(context.Background(), req)
	if res != nil || err == nil || !strings.Contains(err.Error(), "panic recovered in alpha.first tool handler: boom") {
		t.Fatalf("got %v, %v; want the recovered-panic error", res, err)
	}
	if after := httpPanics(t); after != before+1 {
		t.Fatalf("toolyard_http_panics_total = %v, want %v", after, before+1)
	}
}

//go:build e2e && e2e_mempalace

// scripts/e2e_mempalace_test.go validates the MemPalace integration end to
// end against a live toolyard server. The shared helpers from e2e_test.go
// are reused, so this file is built with both tags together:
//
//	go test -tags='e2e e2e_mempalace' ./scripts -run TestMempalaceIngest \
//	    -toolyard=http://localhost:18787 -v
//
// The test skips if the mempalace-mcp binary is not on PATH (or the
// integration is otherwise unavailable) so CI runs that don't pre-install
// it don't spuriously fail.
package scripts

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMempalaceIngest(t *testing.T) {
	base := *toolyardURL

	// 1) Check status; skip if integration is disabled or unavailable.
	statusResp, err := http.Get(base + "/v1/mempalace/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer statusResp.Body.Close()
	body, _ := io.ReadAll(statusResp.Body)
	// /v1/mempalace/status is cookie-authenticated; an unauthenticated
	// caller gets 401, which is fine for this preflight — we only care
	// whether the route is registered and whether the env is configured.
	t.Logf("status %d: %s", statusResp.StatusCode, string(body))

	h := &httpClient{base: base}
	mustLogin(t, h)

	var st map[string]any
	h.raw(t, "GET", "/v1/mempalace/status", nil, &st)
	if enabled, _ := st["enabled"].(bool); !enabled {
		t.Skipf("mempalace integration disabled: %v", st)
	}
	if available, _ := st["available"].(bool); !available {
		t.Skipf("mempalace upstream not connected (binary missing?): %v", st)
	}

	// 2) Enroll an agent and grab its token.
	token := enrollAgent(t, h, "mempalace-e2e")

	// 3) POST /v1/mempalace/ingest with the bearer token.
	entry := "we picked GraphQL because REST chatter was killing latency"
	raw, _ := json.Marshal(map[string]any{
		"entry": entry,
		"topic": "architecture",
	})
	req, _ := http.NewRequest("POST", base+"/v1/mempalace/ingest", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest -> %d: %s", resp.StatusCode, string(rb))
	}
	t.Logf("ingest result: %s", string(rb))

	// Give the upstream a beat to index, then search.
	time.Sleep(500 * time.Millisecond)
	searchBody, _ := json.Marshal(map[string]any{
		"tool": "mempalace.search",
		"arguments": map[string]any{
			"_reason": "e2e: verify the ingested entry is searchable",
			"query":   "why did we pick GraphQL",
		},
	})
	req, _ = http.NewRequest("POST", base+"/v1/tools/run", bytes.NewReader(searchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	sb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search -> %d: %s", resp.StatusCode, string(sb))
	}
	if !strings.Contains(string(sb), "GraphQL") {
		t.Errorf("search did not return the entry; body=%s", string(sb))
	}
}

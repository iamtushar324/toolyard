package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newHookAPITestServer(t *testing.T) (*http.ServeMux, *store.DB, string, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api-hooks.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id := identity.New(db)
	user, err := id.CreateUser(context.Background(), "tester", "long-enough-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, ag, err := id.CreateAgentWithToken(context.Background(), user.ID, "hook-test")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	srv := New(context.Background(), Options{
		Identity:   id,
		Audit:      audit.New(db),
		Hooks:      hooks.New(db, nil),
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	return mux, db, token, ag.ID
}

func TestHooksIngestRequiresBearer(t *testing.T) {
	mux, _, _, _ := newHookAPITestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/ingest", bytes.NewBufferString(`{"event_name":"Stop"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHooksIngestPersistsAndAudits(t *testing.T) {
	mux, db, token, agentID := newHookAPITestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/ingest?source=codex", bytes.NewBufferString(`{"event_name":"Stop","text":"done"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if body["ok"] != true || body["agent_id"] != agentID || body["event_id"] == "" {
		t.Fatalf("unexpected response: %v", body)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM hook_events WHERE agent_id = ? AND source = 'codex'`, agentID).Scan(&n); err != nil {
		t.Fatalf("hook count: %v", err)
	}
	if n != 1 {
		t.Fatalf("hook rows = %d, want 1", n)
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = 'hook.ingest' AND agent_id = ?`, agentID).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

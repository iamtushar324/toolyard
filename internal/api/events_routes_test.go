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
	"github.com/tusharbhardwaj/toolyard/internal/events"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newEventsAPITestServer(t *testing.T) (*http.ServeMux, *events.Service) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ev-api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id := identity.New(db)
	evSvc := events.New(db)
	srv := New(context.Background(), Options{
		Identity:   id,
		Audit:      audit.New(db),
		Events:     evSvc,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	return mux, evSvc
}

// TestWebhookIngestBearerNoCSRF verifies the webhook ingest path works with a
// bearer source token and no cookie / X-Requested-With (it's CSRF-exempt), and
// that dedup returns 200 with {deduped:true}.
func TestWebhookIngestBearer(t *testing.T) {
	mux, evSvc := newEventsAPITestServer(t)
	_, token, err := evSvc.CreateSource(context.Background(), events.CreateSourceInput{Name: "ci", Kind: events.KindWebhook})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := post(`{"type":"deploy","title":"build 1","dedup_key":"d1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = post(`{"type":"deploy","title":"build 1","dedup_key":"d1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dedup ingest: status %d body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["deduped"] != true {
		t.Fatalf("expected deduped:true, got %v", body)
	}

	// Bad token → 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewBufferString(`{"type":"x"}`))
	req.Header.Set("Authorization", "Bearer evs_bogus.nope")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status %d", rec.Code)
	}
}

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/pricing"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

func TestPricingRoutesReadOnlyAndAuthenticated(t *testing.T) {
	e := newAccessTestServer(t)
	e.srv.pricing = pricing.New("")
	admin := e.cookieFor(t, e.admin.ID)
	if got := e.do(t, nil, http.MethodGet, "/v1/insights/pricing", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", got.Code)
	}
	if got := e.do(t, admin, http.MethodPost, "/v1/insights/pricing", "{}"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write: %d", got.Code)
	}
	for _, query := range []string{"limit=-1", "limit=101", "limit=bad", "offset=-1", "offset=bad"} {
		if got := e.do(t, admin, http.MethodGet, "/v1/insights/pricing?"+query, ""); got.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, got.Code)
		}
	}
	rec := e.do(t, admin, http.MethodGet, "/v1/insights/pricing?limit=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("prices: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["status"] != "unavailable" || got["source"] != "Models.dev" || got["unit"] != "per_million_tokens" || got["checked_at"] != nil {
		t.Fatalf("empty catalog: %v", got)
	}
}

func TestPricingDoesNotTurnPayloadsIntoSpend(t *testing.T) {
	e := newAccessTestServer(t)
	e.srv.metrics = metrics.NewReader(e.db)
	_, err := e.db.ExecContext(t.Context(), `INSERT INTO call_events(ts, upstream, short_name, tool_name, outcome, token_estimate_in, token_estimate_out) VALUES(?,?,?,?,?,?,?)`,
		time.Now().Add(-time.Minute).UnixMilli(), "test", "read", "test.read", "ok", 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, e.cookieFor(t, e.admin.ID), http.MethodGet, "/v1/insights/cost", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cost: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	rows := got["rows"].([]any)
	if got["status"] != "unmetered" || got["billed_usd"] != nil || len(rows) != 1 {
		t.Fatalf("unmetered cost: %v", got)
	}
	row := rows[0].(map[string]any)
	if row["usd_estimated"] != nil || row["tokens_in"] != float64(100) || row["tokens_out"] != float64(200) {
		t.Fatalf("payload row: %v", row)
	}
}

func TestPricingRejectsManualSettingsAtomically(t *testing.T) {
	e := newAccessTestServer(t)
	// Legacy values remain on disk for rollback, but are not exposed or used.
	_, err := e.db.ExecContext(t.Context(), `INSERT INTO system_settings(key,value,updated_at) VALUES(?,?,?)`, settings.CostInputUsdPerM, "999", time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.settings, err = settings.New(t.Context(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	admin := e.cookieFor(t, e.admin.ID)
	if _, exists := decodeJSON(t, e.do(t, admin, http.MethodGet, "/v1/settings", ""))[settings.CostInputUsdPerM]; exists {
		t.Fatal("legacy manual price exposed")
	}
	for _, key := range []string{settings.CostInputUsdPerM, settings.CostOutputUsdPerM} {
		rec := e.do(t, admin, http.MethodPatch, "/v1/settings", `{"surface_mode":"router_only","`+key+`":10}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("manual price write: %d %s", rec.Code, rec.Body.String())
		}
		if mode := e.srv.settings.GetString(settings.SurfaceMode, settings.SurfaceFull); mode != settings.SurfaceFull {
			t.Fatal("rejected patch changed unrelated settings")
		}
	}
}

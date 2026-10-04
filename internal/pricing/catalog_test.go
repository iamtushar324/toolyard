package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCatalog = `{
 "direct":{"name":"Direct provider","models":{
   "same-model":{"name":"Model","modalities":{"output":["text"]},"cost":{"input":2,"output":8,"cache_read":0.2,"cache_write":2.5,"tiers":[{"input":4}]}},
   "unknown":{"name":"Unknown","modalities":{"output":["text"]}},
   "free":{"name":"Free","modalities":{"output":["text"]},"cost":{"input":0,"output":0}},
   "image":{"modalities":{"output":["image"]},"cost":{"input":10,"output":20}}
 }},
 "reseller":{"name":"Reseller","models":{"same-model":{"name":"Model","modalities":{"output":["text"]},"cost":{"input":3,"output":12}}}}
}`

func TestCatalogRefreshCacheAndOutage(t *testing.T) {
	now := time.Now().UTC().Add(-6 * time.Hour)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "Toolyard/") {
			t.Error("missing source user agent")
		}
		if status == http.StatusNotModified && r.Header.Get("If-None-Match") != `"v1"` {
			t.Error("missing conditional request")
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(testCatalog))
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "prices.json")
	c := New(path)
	c.url = server.URL
	c.now = func() time.Time { return now }
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	v := c.View("direct", "same-model", 0, 30)
	if v.Status != "ready" || v.Total != 1 || v.ModelCount != 4 || len(v.Models) != 1 {
		t.Fatalf("view: %+v", v)
	}
	m := v.Models[0]
	if *m.Rates.Input != 2 || *m.Rates.Output != 8 || *m.Rates.CacheRead != 0.2 || *m.Rates.CacheWrite != 2.5 || !m.VariableRates {
		t.Fatalf("price: %+v", m)
	}
	if other := c.View("reseller", "same-model", 0, 1); *other.Models[0].Rates.Input != 3 {
		t.Fatal("mixed provider prices")
	}
	if unknown := c.View("direct", "unknown", 0, 1); unknown.Models[0].Rates.Input != nil {
		t.Fatal("missing price became zero")
	}
	if free := c.View("direct", "free", 0, 1); free.Models[0].Rates.Input == nil || *free.Models[0].Rates.Input != 0 {
		t.Fatal("published zero lost")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache permissions: %v %v", info, err)
	}
	loaded := New(path)
	if loaded.View("", "", 0, 100).ModelCount != 4 {
		t.Fatal("cache did not survive restart")
	}
	status = http.StatusNotModified
	now = now.Add(6 * time.Hour)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.View("", "", 0, 0).CheckedAt.Equal(now) {
		t.Fatal("304 did not update freshness")
	}
	status = http.StatusServiceUnavailable
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("failed source accepted")
	}
	v = c.View("direct", "same-model", 0, 1)
	if v.Status != "stale" || len(v.Models) != 1 || *v.Models[0].Rates.Input != 2 || v.LastError == "" {
		t.Fatalf("outage lost valid prices: %+v", v)
	}
	if loaded := New(path); loaded.View("", "", 0, 30).ModelCount != 4 {
		t.Fatal("outage overwrote cache")
	}
}

func TestCatalogRejectsInvalidSourceAndCache(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"bad":`, strings.Replace(testCatalog, `"input":2`, `"input":-2`, 1)} {
		if _, err := parse([]byte(body)); err == nil {
			t.Fatalf("accepted invalid catalog %s", body)
		}
	}
	c := New("")
	if v := c.View("", "", 0, 30); v.Status != "unavailable" || v.CheckedAt != nil || len(v.Models) != 0 {
		t.Fatalf("empty view: %+v", v)
	}
	now := time.Now()
	models, err := parse([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	c.data = snapshot{CheckedAt: now.Add(-25 * time.Hour), Models: models}
	if c.View("", "", 0, 1).Status != "stale" {
		t.Fatal("old prices appear current")
	}
	if v := c.View("", "", 1, 1); v.Total != 4 || len(v.Models) != 1 {
		t.Fatalf("pagination: %+v", v)
	}
	path := filepath.Join(t.TempDir(), "prices.json")
	corrupt, _ := json.Marshal(snapshot{CheckedAt: now.Add(24 * time.Hour), Models: models})
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if New(path).View("", "", 0, 1).Status != "unavailable" {
		t.Fatal("future-dated cache accepted")
	}
}

func TestCatalogCanceledRefreshRetainsSnapshot(t *testing.T) {
	c := New("")
	models, err := parse([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	c.data = snapshot{CheckedAt: time.Now(), Models: models}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Refresh(ctx); err == nil {
		t.Fatal("canceled request accepted")
	}
	if c.View("", "", 0, 1).ModelCount != 4 {
		t.Fatal("cancellation cleared prices")
	}
}

// Explicit opt-in checks the live data contract without network-dependent CI.
func TestCatalogLiveSource(t *testing.T) {
	if os.Getenv("TOOLYARD_TEST_ONLINE_PRICES") != "1" {
		t.Skip("online price check is opt-in")
	}
	c := New(filepath.Join(t.TempDir(), "prices.json"))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	v := c.View("openai", "", 0, 30)
	if v.Status != "ready" || v.ModelCount < 100 || v.Total == 0 || len(v.Models) == 0 {
		t.Fatalf("live source: %+v", v)
	}
	t.Logf("Online catalog: %d text models, %d providers, checked %s", v.ModelCount, len(v.Providers), v.CheckedAt.Format(time.RFC3339))
}

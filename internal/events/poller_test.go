package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExtractJSONPath(t *testing.T) {
	body := []byte(`{"a":{"b":[{"c":42},{"c":"hi"}]},"flag":true,"price":9.5}`)
	cases := []struct {
		path string
		want string
	}{
		{"a.b.0.c", "42"},
		{"a.b[1].c", "hi"},
		{"flag", "true"},
		{"price", "9.5"},
	}
	for _, c := range cases {
		got, err := extractJSONPath(body, c.path)
		if err != nil || got != c.want {
			t.Errorf("path %q = (%q,%v), want %q", c.path, got, err, c.want)
		}
	}
	if _, err := extractJSONPath(body, "a.missing"); err == nil {
		t.Error("expected error for missing key")
	}
}

// poll once helper: drive pollOne directly against a fixture, re-reading the
// source between calls so updated poller_state is observed.
func pollOnce(t *testing.T, s *Service, client *http.Client, srcID string) {
	t.Helper()
	src, err := s.GetSource(context.Background(), srcID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	s.pollOne(context.Background(), client, src)
}

func TestPollerHashFirstSilentThenChange(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	var body atomic.Value
	body.Store("hello")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	src, _, err := s.CreateSource(ctx, CreateSourceInput{
		Name: "watch", Kind: KindPoller,
		PollerCfg: &PollerConfig{URL: srv.URL, IntervalSec: 60, Mode: ModeHash},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	client := srv.Client()

	// First poll seeds silently — no event.
	pollOnce(t, s, client, src.ID)
	if evs, _ := s.Query(ctx, Filter{}); len(evs) != 0 {
		t.Fatalf("first poll should be silent, got %d events", len(evs))
	}
	// Same content → no event.
	pollOnce(t, s, client, src.ID)
	if evs, _ := s.Query(ctx, Filter{}); len(evs) != 0 {
		t.Fatalf("unchanged poll emitted %d events", len(evs))
	}
	// Change content → one content_changed event.
	body.Store("world")
	pollOnce(t, s, client, src.ID)
	evs, _ := s.Query(ctx, Filter{})
	if len(evs) != 1 || evs[0].Type != "content_changed" {
		t.Fatalf("expected 1 content_changed, got %+v", evs)
	}
}

func TestPollerHashFlapEmitsTwice(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	var body atomic.Value
	body.Store("A")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{
		Name: "flap", Kind: KindPoller,
		PollerCfg: &PollerConfig{URL: srv.URL, IntervalSec: 60, Mode: ModeHash},
	})
	client := srv.Client()
	pollOnce(t, s, client, src.ID) // seed A
	body.Store("B")
	pollOnce(t, s, client, src.ID) // A→B event
	body.Store("A")
	pollOnce(t, s, client, src.ID) // B→A event (flap must emit again; no dedup_key)
	if evs, _ := s.Query(ctx, Filter{}); len(evs) != 2 {
		t.Fatalf("flap should emit 2 events, got %d", len(evs))
	}
}

func TestPollerJSONFieldChange(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	var price atomic.Value
	price.Store(`{"price": 100}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(price.Load().(string)))
	}))
	defer srv.Close()
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{
		Name: "price", Kind: KindPoller,
		PollerCfg: &PollerConfig{URL: srv.URL, IntervalSec: 60, Mode: ModeJSONField, JSONPath: "price"},
	})
	client := srv.Client()
	pollOnce(t, s, client, src.ID) // seed
	if evs, _ := s.Query(ctx, Filter{}); len(evs) != 0 {
		t.Fatalf("json_field first poll should seed silently, got %d", len(evs))
	}
	price.Store(`{"price": 150}`)
	pollOnce(t, s, client, src.ID)
	evs, _ := s.Query(ctx, Filter{})
	if len(evs) != 1 || evs[0].Type != "value_changed" {
		t.Fatalf("expected value_changed, got %+v", evs)
	}
	// payload carries old/new.
	if !atLeastContains(string(evs[0].Payload), "100") || !atLeastContains(string(evs[0].Payload), "150") {
		t.Fatalf("payload should carry old/new: %s", evs[0].Payload)
	}
}

func TestPollerFailureBackoffAndRecover(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{
		Name: "flaky", Kind: KindPoller,
		PollerCfg: &PollerConfig{URL: srv.URL, IntervalSec: 60, Mode: ModeHash},
	})
	client := srv.Client()
	pollOnce(t, s, client, src.ID)
	got, _ := s.GetSource(ctx, src.ID)
	if got.LastError == "" || got.PollerState == nil || got.PollerState.ConsecutiveFailures != 1 {
		t.Fatalf("failure not recorded: err=%q state=%+v", got.LastError, got.PollerState)
	}
	// Recover.
	fail.Store(false)
	pollOnce(t, s, client, src.ID)
	got, _ = s.GetSource(ctx, src.ID)
	if got.LastError != "" || got.PollerState.ConsecutiveFailures != 0 {
		t.Fatalf("success should clear error/backoff: err=%q state=%+v", got.LastError, got.PollerState)
	}
}

func atLeastContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

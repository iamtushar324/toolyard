package events

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestIngestAndDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, token, err := s.CreateSource(ctx, CreateSourceInput{Name: "ci", Kind: KindWebhook})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if token == "" || !strings.HasPrefix(token, src.ID+".") {
		t.Fatalf("webhook token wrong: %q", token)
	}

	ev, deduped, err := s.Ingest(ctx, src, IngestInput{Type: "deploy", Title: "build 1", DedupKey: "k1"})
	if err != nil || deduped {
		t.Fatalf("ingest: err=%v deduped=%v", err, deduped)
	}
	if ev.Summary == "" {
		t.Fatal("expected templated summary")
	}
	// Same dedup_key → deduped.
	_, deduped, err = s.Ingest(ctx, src, IngestInput{Type: "deploy", Title: "build 1 again", DedupKey: "k1"})
	if err != nil || !deduped {
		t.Fatalf("expected dedup: err=%v deduped=%v", err, deduped)
	}
	// Different dedup_key → new row.
	_, deduped, _ = s.Ingest(ctx, src, IngestInput{Type: "deploy", DedupKey: "k2"})
	if deduped {
		t.Fatal("k2 should not dedup")
	}
	evs, _ := s.Query(ctx, Filter{})
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evs))
	}
}

func TestSummaryTemplating(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "mail", Kind: KindWebhook})
	ev, _, _ := s.Ingest(ctx, src, IngestInput{Type: "email", Title: "Invoice"})
	if ev.Summary != "New email from mail: Invoice" {
		t.Fatalf("summary = %q", ev.Summary)
	}
	// Caller-supplied summary wins.
	ev2, _, _ := s.Ingest(ctx, src, IngestInput{Type: "email", Summary: "custom one", DedupKey: "x"})
	if ev2.Summary != "custom one" {
		t.Fatalf("summary = %q", ev2.Summary)
	}
}

func TestTimestampClamping(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "x", Kind: KindWebhook})
	now := time.Now().UnixMilli()
	// Far future → clamped to ~now+5min.
	ev, _, _ := s.Ingest(ctx, src, IngestInput{Type: "t", Timestamp: now + 100*24*3600*1000})
	if ev.CreatedAt > now+clampFuture.Milliseconds()+1000 {
		t.Fatalf("future timestamp not clamped: %d vs now %d", ev.CreatedAt, now)
	}
	// Far past → clamped to ~now-7d.
	ev2, _, _ := s.Ingest(ctx, src, IngestInput{Type: "t", Timestamp: 1, DedupKey: "p"})
	if ev2.CreatedAt < now-clampPast.Milliseconds()-1000 {
		t.Fatalf("past timestamp not clamped: %d", ev2.CreatedAt)
	}
}

func TestPayloadCap(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "x", Kind: KindWebhook})
	big := make([]byte, MaxPayloadBytes+100)
	for i := range big {
		big[i] = 'a'
	}
	payload, _ := json.Marshal(map[string]string{"blob": string(big)})
	_, _, err := s.Ingest(ctx, src, IngestInput{Type: "t", Payload: payload})
	if err != ErrPayloadTooBig {
		t.Fatalf("expected ErrPayloadTooBig, got %v", err)
	}
}

func TestQueryFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	a, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "a", Kind: KindWebhook})
	b, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "b", Kind: KindWebhook})
	s.Ingest(ctx, a, IngestInput{Type: "deploy", Summary: "alpha shipped", DedupKey: "1"})
	s.Ingest(ctx, b, IngestInput{Type: "alert", Summary: "beta on fire", DedupKey: "2"})

	if evs, _ := s.Query(ctx, Filter{Type: "deploy"}); len(evs) != 1 || evs[0].SourceName != "a" {
		t.Fatalf("type filter wrong: %+v", evs)
	}
	if evs, _ := s.Query(ctx, Filter{SourceName: "b"}); len(evs) != 1 {
		t.Fatalf("source filter wrong")
	}
	if evs, _ := s.Query(ctx, Filter{Q: "fire"}); len(evs) != 1 {
		t.Fatalf("q filter wrong")
	}
}

func TestAckIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "x", Kind: KindWebhook})
	ev, _, _ := s.Ingest(ctx, src, IngestInput{Type: "t", DedupKey: "1"})
	if n, _ := s.Ack(ctx, []string{ev.ID}, "user:1"); n != 1 {
		t.Fatalf("first ack = %d, want 1", n)
	}
	if n, _ := s.Ack(ctx, []string{ev.ID}, "user:1"); n != 0 {
		t.Fatalf("second ack = %d, want 0", n)
	}
	if c, _ := s.UnackedCount(ctx); c != 0 {
		t.Fatalf("unacked = %d", c)
	}
}

func TestTokenVerifyAndDisable(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, token, _ := s.CreateSource(ctx, CreateSourceInput{Name: "x", Kind: KindWebhook})
	if got, err := s.VerifySourceToken(ctx, token); err != nil || got.ID != src.ID {
		t.Fatalf("verify: %v", err)
	}
	if _, err := s.VerifySourceToken(ctx, "bogus.token"); err != ErrBadToken {
		t.Fatalf("bogus verify = %v", err)
	}
	// Rotate invalidates the old token.
	newTok, err := s.RotateSourceToken(ctx, src.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.VerifySourceToken(ctx, token); err != ErrBadToken {
		t.Fatalf("old token still valid after rotate")
	}
	if _, err := s.VerifySourceToken(ctx, newTok); err != nil {
		t.Fatalf("new token: %v", err)
	}
	// Disable → reject.
	off := false
	s.UpdateSource(ctx, src.ID, UpdateSourceInput{Enabled: &off})
	if _, err := s.VerifySourceToken(ctx, newTok); err != ErrBadToken {
		t.Fatalf("disabled source still verifies")
	}
}

func TestBriefRendering(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "ci", Kind: KindWebhook})
	s.Ingest(ctx, src, IngestInput{Type: "deploy", Summary: "shipped v1", DedupKey: "1"})
	s.Ingest(ctx, src, IngestInput{Type: "deploy", Summary: "shipped v2", DedupKey: "2"})
	md, err := s.Brief(ctx, 0)
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	if !strings.Contains(md, "ci") || !strings.Contains(md, "deploy") || !strings.Contains(md, "shipped v1") {
		t.Fatalf("brief missing content:\n%s", md)
	}
	// Empty case.
	s2 := newTestService(t)
	md2, _ := s2.Brief(ctx, 0)
	if !strings.Contains(strings.ToLower(md2), "caught up") {
		t.Fatalf("empty brief = %q", md2)
	}
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	src, _, _ := s.CreateSource(ctx, CreateSourceInput{Name: "x", Kind: KindWebhook})
	// Old acked+synced event (should purge), old unacked (survives normal tier),
	// very old unacked (purged by hard floor).
	mkOld := func(dedup string, ageDays int, acked, synced bool) string {
		ev, _, _ := s.Ingest(ctx, src, IngestInput{Type: "t", DedupKey: dedup})
		ts := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).UnixMilli()
		ackVal := any(nil)
		if acked {
			ackVal = ts
		}
		syncVal := 0
		if synced {
			syncVal = 1
		}
		s.db.ExecContext(ctx, `UPDATE events SET received_at=?, acked_at=?, lake_synced=? WHERE id=?`,
			ts, ackVal, syncVal, ev.ID)
		return ev.ID
	}
	purgeable := mkOld("a", 100, true, true)
	unackedRecentish := mkOld("b", 100, false, true) // unacked → survives normal tier
	veryOld := mkOld("c", 400, false, false)         // hard floor (4*90=360d)

	n, err := s.purgeOnce(ctx, 90, true)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected >=2 purged, got %d", n)
	}
	if _, err := s.Get(ctx, purgeable); err == nil {
		t.Fatal("acked+synced old event should be purged")
	}
	if _, err := s.Get(ctx, veryOld); err == nil {
		t.Fatal("very old event should be purged by hard floor")
	}
	if _, err := s.Get(ctx, unackedRecentish); err != nil {
		t.Fatal("unacked event within hard floor should survive")
	}
}

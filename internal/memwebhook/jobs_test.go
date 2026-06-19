package memwebhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
)

func enqueue(t *testing.T, s *Service, wh *Webhook, reqID, body string) (*Job, error) {
	t.Helper()
	return s.Enqueue(context.Background(), wh, IngestRequest{
		RequestID:   reqID,
		Payload:     json.RawMessage(body),
		PayloadSize: int64(len(body)),
	})
}

func TestEnqueueReturnsQueuedJob(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "q", Wing: "meetings",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript"}})

	body := `{"transcript":"hello"}`
	job, err := enqueue(t, s, wh, "req_q1", body)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if job.Status != jobQueued {
		t.Fatalf("status = %q, want queued", job.Status)
	}
	if job.Wing != "meetings" || job.PayloadSize != int64(len(body)) || job.ID != "req_q1" {
		t.Fatalf("unexpected job: %+v", job)
	}
	// Nothing should have reached MemPalace yet — the worker hasn't run.
	if len(ing.calls) != 0 {
		t.Fatalf("enqueue must not call mempalace, got %d", len(ing.calls))
	}
}

func TestProcessSuccessWritesLedgerAndCounters(t *testing.T) {
	ctx := context.Background()
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "ok", Wing: "meetings",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript"}})

	job, err := ingest(t, s, wh, "req_ok1", `{"transcript":"we shipped TEC-482"}`)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if job.Status != jobSucceeded || !job.MempalaceOK {
		t.Fatalf("job = %+v, want succeeded + mempalace_ok", job)
	}
	if job.ChunkCount != 1 || job.ChunksDone != 1 || job.FinishedAt == 0 || job.StartedAt == 0 {
		t.Fatalf("job lifecycle fields wrong: %+v", job)
	}
	if len(ing.calls) != 1 || ing.calls[0].wing != "meetings" || ing.calls[0].agent != "webhook_ok" {
		t.Fatalf("upstream call wrong: %+v", ing.calls)
	}

	// Ledger 'ok' row + webhook counters mirror the old synchronous path.
	m, err := s.Metrics(ctx, 10)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.Totals.Total != 1 || m.Totals.OK != 1 {
		t.Fatalf("totals = %+v", m.Totals)
	}
	if len(m.PerWebhook) != 1 || m.PerWebhook[0].IngestCount != 1 || m.PerWebhook[0].FailCount != 0 {
		t.Fatalf("per-webhook = %+v", m.PerWebhook)
	}
	if len(m.Recent) != 1 || m.Recent[0].Status != statusOK || m.Recent[0].ID != "req_ok1" {
		t.Fatalf("recent = %+v", m.Recent)
	}
}

func TestTransientFailureRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	// ErrNotReady is treated as transient, so the job is retried up to
	// jobMaxAttempts before being marked failed.
	ing := &fakeIngester{available: true, err: mempalace.ErrNotReady}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "flaky", Wing: "w"})

	job, err := enqueue(t, s, wh, "req_f1", `{"transcript":"x"}`)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Each attempt requeues with a future backoff; force the gate open and
	// process again to exercise the retry budget.
	for i := 0; i < jobMaxAttempts; i++ {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE memory_webhook_jobs SET next_attempt_at=0 WHERE id=?`, job.ID); err != nil {
			t.Fatalf("reset gate: %v", err)
		}
		if _, err := s.processOnce(ctx); err != nil {
			t.Fatalf("processOnce: %v", err)
		}
	}

	final, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.Status != jobFailed {
		t.Fatalf("status = %q, want failed", final.Status)
	}
	if final.Attempts != jobMaxAttempts {
		t.Fatalf("attempts = %d, want %d", final.Attempts, jobMaxAttempts)
	}
	if final.Error == "" {
		t.Fatalf("failed job should carry an error reason")
	}
	if len(ing.calls) != jobMaxAttempts {
		t.Fatalf("expected %d upstream attempts, got %d", jobMaxAttempts, len(ing.calls))
	}
	// Exactly one terminal ledger row + one fail-counter bump.
	m, _ := s.Metrics(ctx, 10)
	if m.Totals.Failed != 1 || m.PerWebhook[0].FailCount != 1 {
		t.Fatalf("ledger/counters = %+v / %+v", m.Totals, m.PerWebhook)
	}
}

func TestLargePayloadChunked(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "big", Wing: "meetings",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript"}})

	// A multi-line transcript well above jobChunkBytes so it must split.
	transcript := strings.Repeat("a meeting transcript line with enough words to matter\n", 2000)
	body, _ := json.Marshal(map[string]string{"transcript": transcript})

	job, err := ingest(t, s, wh, "req_big1", string(body))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if job.Status != jobSucceeded {
		t.Fatalf("job = %+v, want succeeded", job)
	}
	if job.ChunkCount <= 1 {
		t.Fatalf("expected the payload to be chunked, chunk_count=%d", job.ChunkCount)
	}
	if job.ChunksDone != job.ChunkCount {
		t.Fatalf("chunks_done=%d != chunk_count=%d", job.ChunksDone, job.ChunkCount)
	}
	if len(ing.calls) != job.ChunkCount {
		t.Fatalf("upstream calls=%d, want chunk_count=%d", len(ing.calls), job.ChunkCount)
	}
	for i, c := range ing.calls {
		if c.wing != "meetings" {
			t.Fatalf("chunk %d wing=%q, want the locked wing", i, c.wing)
		}
		if len(c.entry) > jobChunkBytes+200 { // +200 leeway for the chunk marker
			t.Fatalf("chunk %d is %d bytes, exceeds cap %d", i, len(c.entry), jobChunkBytes)
		}
		if i > 0 && !strings.Contains(c.entry, fmt.Sprintf("chunk %d/%d", i+1, job.ChunkCount)) {
			t.Fatalf("chunk %d missing continuation marker: %.80q", i, c.entry)
		}
	}
}

func TestRecoverRunningOnBoot(t *testing.T) {
	ctx := context.Background()
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "boot", Wing: "w"})

	job, err := enqueue(t, s, wh, "req_b1", `{"transcript":"x"}`)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Simulate a crash mid-flight.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs SET status=? WHERE id=?`, jobRunning, job.ID); err != nil {
		t.Fatalf("force running: %v", err)
	}

	s.recoverRunningJobs(ctx)

	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != jobQueued {
		t.Fatalf("recovered status = %q, want queued", got.Status)
	}
	// And it now processes to success.
	drainJobs(t, s)
	if got, _ = s.GetJob(ctx, job.ID); got.Status != jobSucceeded {
		t.Fatalf("post-recovery status = %q, want succeeded", got.Status)
	}
}

func TestJobJSONHasNoTokenOrEntry(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, token := mustCreate(t, s, CreateInput{Name: "leak", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript"}})

	job, err := ingest(t, s, wh, "req_leak1", `{"transcript":"super secret transcript body"}`)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	b, _ := json.Marshal(job)
	js := string(b)
	if strings.Contains(js, token) || strings.Contains(js, hashToken(token)) {
		t.Fatalf("job JSON leaked token material: %s", js)
	}
	if strings.Contains(strings.ToLower(js), "token") {
		t.Fatalf("job JSON should carry no token field: %s", js)
	}
	// The mapped entry text must not be exposed via the status view.
	if strings.Contains(js, "super secret transcript body") || strings.Contains(js, "\"entry\"") {
		t.Fatalf("job JSON should not expose the entry text: %s", js)
	}
}

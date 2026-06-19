package memwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// fakeIngester records every IngestTagged call so tests can assert the wing
// that was actually written — the heart of the wing-isolation guarantee.
type fakeIngester struct {
	available bool
	fail      bool
	err       error
	calls     []ingestCall
}

type ingestCall struct{ entry, topic, wing, agent string }

func (f *fakeIngester) Available() bool { return f.available }

func (f *fakeIngester) IngestTagged(_ context.Context, entry, topic, wing, agent string) (*mempalace.IngestResult, error) {
	f.calls = append(f.calls, ingestCall{entry, topic, wing, agent})
	if f.err != nil {
		return nil, f.err
	}
	if f.fail {
		return &mempalace.IngestResult{OK: false, Detail: "upstream said no"}, nil
	}
	return &mempalace.IngestResult{OK: true, Detail: "stored"}, nil
}

func tempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mw.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newService(t *testing.T, ing *fakeIngester) *Service {
	t.Helper()
	db := tempDB(t)
	return New(db, ing, audit.New(db))
}

func mustCreate(t *testing.T, s *Service, in CreateInput) (*Webhook, string) {
	t.Helper()
	wh, token, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return wh, token
}

// ingest enqueues a payload and, when it is accepted, drains the worker so the
// job reaches a terminal state — collapsing the async path into a synchronous
// helper for tests. It returns the terminal job (nil when Enqueue rejected the
// payload outright) and the Enqueue error.
func ingest(t *testing.T, s *Service, wh *Webhook, reqID, body string) (*Job, error) {
	t.Helper()
	job, err := s.Enqueue(context.Background(), wh, IngestRequest{
		RequestID:   reqID,
		Payload:     json.RawMessage(body),
		PayloadSize: int64(len(body)),
	})
	if err != nil {
		return nil, err
	}
	drainJobs(t, s)
	final, gerr := s.GetJob(context.Background(), job.ID)
	if gerr != nil {
		t.Fatalf("get job %s: %v", job.ID, gerr)
	}
	return final, nil
}

// drainJobs runs the worker's claim+process step until no job is immediately
// claimable (a job requeued with a future backoff is intentionally left).
func drainJobs(t *testing.T, s *Service) {
	t.Helper()
	for {
		processed, err := s.processOnce(context.Background())
		if err != nil {
			t.Fatalf("processOnce: %v", err)
		}
		if !processed {
			return
		}
	}
}

func TestCreateAndTokenLifecycle(t *testing.T) {
	s := newService(t, &fakeIngester{available: true})
	ctx := context.Background()

	wh, token := mustCreate(t, s, CreateInput{Name: "ci", Wing: "meetings"})
	if token == "" || !strings.HasPrefix(token, wh.ID+".") {
		t.Fatalf("token should be <id>.<secret>, got %q", token)
	}

	// Duplicate name → ErrExists.
	if _, _, err := s.Create(ctx, CreateInput{Name: "ci", Wing: "x"}); !errors.Is(err, ErrExists) {
		t.Fatalf("dup name: want ErrExists, got %v", err)
	}
	// Missing wing → ErrInvalid.
	if _, _, err := s.Create(ctx, CreateInput{Name: "nowing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing wing: want ErrInvalid, got %v", err)
	}

	// VerifyToken accepts the good token.
	got, err := s.VerifyToken(ctx, token)
	if err != nil || got.ID != wh.ID {
		t.Fatalf("verify good token: %v / %+v", err, got)
	}
	// Bad token → ErrBadToken.
	if _, err := s.VerifyToken(ctx, wh.ID+".garbage"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("verify bad token: want ErrBadToken, got %v", err)
	}

	// Rotate invalidates the old token.
	newTok, err := s.RotateToken(ctx, wh.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.VerifyToken(ctx, token); !errors.Is(err, ErrBadToken) {
		t.Fatalf("old token after rotate: want ErrBadToken, got %v", err)
	}
	if _, err := s.VerifyToken(ctx, newTok); err != nil {
		t.Fatalf("new token after rotate: %v", err)
	}

	// Disable → token rejected even though it's otherwise valid.
	enabled := false
	if _, err := s.Update(ctx, wh.ID, UpdateInput{Enabled: &enabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.VerifyToken(ctx, newTok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("disabled webhook token: want ErrBadToken, got %v", err)
	}
}

func TestTokenNeverSerialized(t *testing.T) {
	s := newService(t, &fakeIngester{available: true})
	wh, token := mustCreate(t, s, CreateInput{Name: "leak", Wing: "w"})
	list, err := s.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v / %d", err, len(list))
	}
	b, _ := json.Marshal(list[0])
	if strings.Contains(string(b), token) || strings.Contains(string(b), hashToken(token)) {
		t.Fatalf("webhook JSON leaked token material: %s", b)
	}
	if strings.Contains(strings.ToLower(string(b)), "token") {
		t.Fatalf("webhook JSON should carry no token field: %s", b)
	}
	_ = wh
}

func TestWingIsLockedAtIngest(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "lock", Wing: "meetings"})

	// Payload tries to override the wing — must be ignored.
	job, err := ingest(t, s, wh, "req_1", `{"wing":"attacker","closet":"secret","transcript":"hi"}`)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if job.Status != jobSucceeded || job.Wing != "meetings" {
		t.Fatalf("job wing should be the bound wing, got %+v", job)
	}
	if len(ing.calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(ing.calls))
	}
	if ing.calls[0].wing != "meetings" {
		t.Fatalf("upstream wing = %q, want the locked wing 'meetings'", ing.calls[0].wing)
	}
	if ing.calls[0].agent != "webhook_lock" {
		t.Fatalf("agent tag = %q, want webhook_lock", ing.calls[0].agent)
	}
}

func TestUpdateCannotChangeWing(t *testing.T) {
	s := newService(t, &fakeIngester{available: true})
	wh, _ := mustCreate(t, s, CreateInput{Name: "immutable", Wing: "alpha"})
	src := "n8n"
	updated, err := s.Update(context.Background(), wh.ID, UpdateInput{Source: &src})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Wing != "alpha" {
		t.Fatalf("wing changed to %q; it must be immutable", updated.Wing)
	}
}

func TestSchemaFlexibility(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	payload := `{"title":"Standup","transcript":"we shipped TEC-481"}`
	noHeader := false

	whole, _ := mustCreate(t, s, CreateInput{Name: "whole", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeWhole, IncludeMetadataHeader: &noHeader}})
	field, _ := mustCreate(t, s, CreateInput{Name: "field", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript", IncludeMetadataHeader: &noHeader}})
	tmpl, _ := mustCreate(t, s, CreateInput{Name: "tmpl", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeTemplate, EntryTemplate: "{{title}} :: {{transcript}}", Topic: "daily", IncludeMetadataHeader: &noHeader}})

	if _, err := ingest(t, s, whole, "r1", payload); err != nil {
		t.Fatalf("whole: %v", err)
	}
	if _, err := ingest(t, s, field, "r2", payload); err != nil {
		t.Fatalf("field: %v", err)
	}
	if _, err := ingest(t, s, tmpl, "r3", payload); err != nil {
		t.Fatalf("template: %v", err)
	}
	if len(ing.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(ing.calls))
	}
	wholeEntry, fieldEntry, tmplEntry := ing.calls[0].entry, ing.calls[1].entry, ing.calls[2].entry

	if !strings.Contains(wholeEntry, "Standup") || !strings.Contains(wholeEntry, "transcript") {
		t.Fatalf("whole entry should contain the full payload: %q", wholeEntry)
	}
	if fieldEntry != "we shipped TEC-481" {
		t.Fatalf("field entry = %q, want the transcript value only", fieldEntry)
	}
	if tmplEntry != "Standup :: we shipped TEC-481" {
		t.Fatalf("template entry = %q", tmplEntry)
	}
	if ing.calls[2].topic != "daily" {
		t.Fatalf("template topic = %q, want daily", ing.calls[2].topic)
	}
	// Three different specs → three distinct entries from the same payload.
	if wholeEntry == fieldEntry || fieldEntry == tmplEntry || wholeEntry == tmplEntry {
		t.Fatalf("expected distinct entries per spec")
	}
}

func TestRequiredFieldsRejected(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "req", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeWhole, RequiredFields: []string{"transcript"}}})

	job, err := ingest(t, s, wh, "r1", `{"title":"no transcript here"}`)
	if !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("missing required field: want ErrSchemaViolation, got %v", err)
	}
	if job != nil {
		t.Fatalf("a rejected payload must not be queued, got job %+v", job)
	}
	if len(ing.calls) != 0 {
		t.Fatalf("rejected payload must not reach mempalace, got %d calls", len(ing.calls))
	}
	// The rejection is still recorded in the ledger as 'rejected'.
	m, err := s.Metrics(context.Background(), 10)
	if err != nil || m.Totals.Rejected != 1 {
		t.Fatalf("ledger should record one rejection: %+v (%v)", m.Totals, err)
	}
}

func TestJSONSchemaValidation(t *testing.T) {
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	schema := json.RawMessage(`{"type":"object","required":["transcript"],"properties":{"transcript":{"type":"string"}}}`)
	wh, _ := mustCreate(t, s, CreateInput{Name: "sch", Wing: "w",
		Spec: &PayloadSpec{Mode: ModeField, EntryField: "transcript", JSONSchema: schema}})

	if _, err := ingest(t, s, wh, "r1", `{"transcript":"valid string"}`); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if _, err := ingest(t, s, wh, "r2", `{"transcript":123}`); !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("wrong type: want ErrSchemaViolation, got %v", err)
	}
	if _, err := ingest(t, s, wh, "r3", `{"title":"no transcript"}`); !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("missing required: want ErrSchemaViolation, got %v", err)
	}
}

func TestInvalidSchemaRejectedAtCreate(t *testing.T) {
	s := newService(t, &fakeIngester{available: true})
	bad := json.RawMessage(`{"type": 12345}`) // type must be a string/array
	if _, _, err := s.Create(context.Background(), CreateInput{Name: "bad", Wing: "w",
		Spec: &PayloadSpec{JSONSchema: bad}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad schema at create: want ErrInvalid, got %v", err)
	}
}

func TestIngestMemUnavailable(t *testing.T) {
	// When MemPalace is entirely disabled, Enqueue fails fast (no doomed job is
	// queued) and the attempt is recorded in the ledger as 'failed'.
	ing := &fakeIngester{available: false}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "down", Wing: "w"})
	job, err := ingest(t, s, wh, "r1", `{"x":1}`)
	if !errors.Is(err, ErrMemUnavailable) {
		t.Fatalf("want ErrMemUnavailable, got %v", err)
	}
	if job != nil {
		t.Fatalf("no job should be queued when mempalace is disabled, got %+v", job)
	}
	if len(ing.calls) != 0 {
		t.Fatalf("disabled mempalace must not be called, got %d", len(ing.calls))
	}
	m, err := s.Metrics(context.Background(), 10)
	if err != nil || m.Totals.Failed != 1 {
		t.Fatalf("ledger should record one failure: %+v (%v)", m.Totals, err)
	}
}

func TestIngestUpstreamRejection(t *testing.T) {
	// A content rejection from MemPalace (OK=false) is discovered in the worker,
	// so the request is accepted (queued) and the job ends up 'failed'.
	ing := &fakeIngester{available: true, fail: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "rej", Wing: "w"})
	job, err := ingest(t, s, wh, "r1", `{"x":1}`)
	if err != nil {
		t.Fatalf("enqueue should accept the payload, got %v", err)
	}
	if job.Status != jobFailed || job.Error == "" {
		t.Fatalf("job should be failed with a reason, got %+v", job)
	}
	if job.MempalaceOK {
		t.Fatalf("mempalace_ok must be false on rejection")
	}
}

func TestLedgerAndMetrics(t *testing.T) {
	ctx := context.Background()
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	wh, _ := mustCreate(t, s, CreateInput{Name: "m", Wing: "meetings"})

	if _, err := ingest(t, s, wh, "ok1", `{"transcript":"a"}`); err != nil {
		t.Fatalf("ingest ok1: %v", err)
	}
	// A schema rejection (bad JSON) should be recorded as 'rejected'.
	if _, err := ingest(t, s, wh, "bad1", `{not json`); !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("bad json: want ErrSchemaViolation, got %v", err)
	}
	// Oversized rejection logged via the handler helper.
	s.RecordTooLarge(ctx, wh, "big1", "n8n", 9999)

	m, err := s.Metrics(ctx, 10)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.Totals.Total != 3 || m.Totals.OK != 1 || m.Totals.Rejected != 1 || m.Totals.TooLarge != 1 {
		t.Fatalf("totals = %+v", m.Totals)
	}
	if len(m.PerWing) != 1 || m.PerWing[0].Wing != "meetings" || m.PerWing[0].Count != 3 {
		t.Fatalf("per-wing = %+v", m.PerWing)
	}
	if len(m.PerWebhook) != 1 || m.PerWebhook[0].IngestCount != 3 || m.PerWebhook[0].FailCount != 2 {
		t.Fatalf("per-webhook = %+v", m.PerWebhook)
	}

	wings, err := s.KnownWings(ctx)
	if err != nil || len(wings) != 1 || wings[0] != "meetings" {
		t.Fatalf("known wings = %v (%v)", wings, err)
	}
}

func TestDeleteWebhook(t *testing.T) {
	s := newService(t, &fakeIngester{available: true})
	wh, _ := mustCreate(t, s, CreateInput{Name: "del", Wing: "w"})
	if err := s.Delete(context.Background(), wh.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(context.Background(), wh.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(context.Background(), wh.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

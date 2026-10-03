package inbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func keyedSubmission(key string) *Submission {
	s := deploySubmission()
	s.IdempotencyKey = key
	return s
}

func mustSubmitWithKey(t *testing.T, svc *Service, agent string, sub *Submission) *SubmitResult {
	t.Helper()
	r, err := svc.Submit(context.Background(), agent, sub)
	if err != nil || !r.OK {
		t.Fatalf("submit: %+v, %v", r, err)
	}
	return r
}

func TestIdempotentSubmissionKeysAndCanonicalPayload(t *testing.T) {
	e := newEnv(t)
	first := mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission("request-1"))
	retry := keyedSubmission("request-1")
	retry.Title = "  " + retry.Title + "  "
	retry.TTLSeconds = DefaultTTL
	retry.Tools[0].Params["env"] = map[string]any{"eq": "prod"}
	got := mustSubmitWithKey(t, e.svc, "ag_1", retry)
	if got.RequestID != first.RequestID || !got.Replayed {
		t.Fatalf("canonical retry: %+v, original %+v", got, first)
	}
	for _, field := range []string{"title", "message", "params", "invalid"} {
		t.Run(field, func(t *testing.T) {
			changed := keyedSubmission("request-1")
			switch field {
			case "title":
				changed.Title = "Ship a different release"
			case "message":
				changed.Message += " The scope changed."
			case "params":
				changed.Tools[0].Params["env"] = "staging"
			case "invalid":
				changed.Title = ""
			}
			if _, err := e.svc.Submit(context.Background(), "ag_1", changed); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
		})
	}
	ids := map[string]bool{first.RequestID: true}
	for _, c := range []struct{ agent, key string }{{"ag_1", "request-2"}, {"ag_2", "request-1"}, {"ag_1", ""}, {"ag_1", ""}} {
		r := mustSubmitWithKey(t, e.svc, c.agent, keyedSubmission(c.key))
		if ids[r.RequestID] || r.Replayed {
			t.Fatalf("separate request deduplicated: %+v", r)
		}
		ids[r.RequestID] = true
	}
}

func TestIdempotentSubmissionValidationAndCapacity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, key := range []string{" ", strings.Repeat("x", MaxIdempotencyKey+1)} {
		r, err := e.svc.Submit(ctx, "ag_1", keyedSubmission(key))
		if err != nil || r.OK || len(r.Problems) == 0 || r.Problems[0].Path != "idempotency_key" {
			t.Fatalf("bad key accepted: %+v, %v", r, err)
		}
	}
	bad := keyedSubmission("fixed-later")
	bad.Title = ""
	if r, err := e.svc.Submit(ctx, "ag_1", bad); err != nil || r.OK {
		t.Fatalf("invalid submission accepted: %+v, %v", r, err)
	}
	first := mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission("fixed-later"))
	for i := 1; i < MaxPendingPerAgent; i++ {
		mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission(fmt.Sprintf("request-%d", i)))
	}
	if _, err := e.svc.Submit(ctx, "ag_1", keyedSubmission("over-cap")); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("capacity not enforced: %v", err)
	}
	if replay := mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission("fixed-later")); replay.RequestID != first.RequestID {
		t.Fatalf("retry at capacity: %+v", replay)
	}

	// Revoking catalog access prevents new work, but does not hide an
	// already-recorded request or create new permissions when it is replayed.
	e.svc.Flush()
	e.svc.SetCatalog(fakeCatalog{"db.migrate": AccessDenied, "deploy.run": AccessDenied, "flags.set": AccessDenied})
	if replay := mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission("fixed-later")); replay.RequestID != first.RequestID {
		t.Fatalf("retry after policy change: %+v", replay)
	}
	if r, err := e.svc.Submit(ctx, "ag_2", keyedSubmission("new-denied")); err != nil || r.OK {
		t.Fatalf("new request bypassed authorization: %+v, %v", r, err)
	}
}

func TestIdempotentSubmissionDryRunDoesNotReserveKey(t *testing.T) {
	e := newEnv(t)
	dry := keyedSubmission("draft")
	dry.DryRun = true
	r := mustSubmitWithKey(t, e.svc, "ag_1", dry)
	if r.RequestID != "" || !r.DryRun {
		t.Fatalf("dry run created a request: %+v", r)
	}
	final := keyedSubmission("draft")
	final.Title = "Ship billing after a draft review"
	first := mustSubmitWithKey(t, e.svc, "ag_1", final)
	if first.Replayed {
		t.Fatal("dry run reserved the key")
	}
	if replay := mustSubmitWithKey(t, e.svc, "ag_1", final); replay.RequestID != first.RequestID {
		t.Fatalf("retry: %+v", replay)
	}
	var count int
	if err := e.svc.db.QueryRow(`SELECT COUNT(*) FROM inbox_dry_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry ran another dry run: %d, %v", count, err)
	}
}

func TestIdempotentSubmissionTerminalStatesAndGrants(t *testing.T) {
	for _, status := range []string{StatusApproved, StatusDenied, StatusReturned, StatusAnswered, StatusRead, StatusCancelled, StatusExpired} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			sub := keyedSubmission("terminal")
			if status == StatusAnswered || status == StatusRead {
				sub.Facts, sub.Tools = nil, nil
				if status == StatusRead {
					sub.Kind = KindUpdate
				} else {
					sub.Kind = KindQuestion
					sub.Options = []Option{{Label: "Yes"}, {Label: "No"}}
				}
			}
			first := mustSubmitWithKey(t, e.svc, "ag_1", sub)
			e.svc.Flush()
			var err error
			switch status {
			case StatusApproved:
				_, err = e.svc.Decide(ctx, first.RequestID, Decision{Action: "approve", Allow: []bool{true, true, false}, TTLSeconds: MinTTL,
					Params: map[int]map[string]Constraint{1: {"ref": {Op: "eq", Eq: "abc1234"}}}})
			case StatusDenied:
				_, err = e.svc.Decide(ctx, first.RequestID, Decision{Action: "deny"})
			case StatusReturned:
				_, err = e.svc.Decide(ctx, first.RequestID, Decision{Action: "return", Note: "Revise the plan"})
			case StatusAnswered:
				option := 0
				_, err = e.svc.Decide(ctx, first.RequestID, Decision{Action: "answer", Option: &option})
			case StatusRead:
				_, err = e.svc.Decide(ctx, first.RequestID, Decision{Action: "read"})
			case StatusCancelled:
				_, err = e.svc.Cancel(ctx, "ag_1", first.RequestID)
			case StatusExpired:
				e.advance(RequestTTL + time.Minute)
				e.svc.Sweep(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := mustSubmitWithKey(t, e.svc, "ag_1", sub)
			if got.RequestID != first.RequestID || got.Status != status || got.ExpiresAt != first.ExpiresAt {
				t.Fatalf("terminal request replaced: %+v", got)
			}
			if status == StatusApproved {
				views, err := e.svc.Status(ctx, "ag_1", []string{first.RequestID})
				if err != nil || views[0].Tools[0].Grant == "" {
					t.Fatalf("retry consumed original grant: %+v, %v", views, err)
				}
				mustSubmitWithKey(t, e.svc, "ag_1", sub)
				views, err = e.svc.Status(ctx, "ag_1", []string{first.RequestID})
				if err != nil || views[0].Tools[0].Grant != "" {
					t.Fatalf("retry reissued a grant: %+v, %v", views, err)
				}
				var count int
				if err := e.svc.db.QueryRow(`SELECT COUNT(*) FROM inbox_grants`).Scan(&count); err != nil || count != 2 {
					t.Fatalf("retry duplicated grants: %d, %v", count, err)
				}
			}
		})
	}
}

type countingJudge struct{ calls atomic.Int32 }

func (j *countingJudge) Review(context.Context, *Request) (*Review, error) {
	j.calls.Add(1)
	return &Review{}, nil
}

func TestIdempotentConcurrentCreationAcrossConnections(t *testing.T) {
	e := newEnv(t)
	otherDB, err := store.Open(e.svc.db.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherDB.Close() })
	j := &countingJudge{}
	e.svc.opts.Judge, e.svc.opts.JudgeEnabled = j, func() bool { return true }
	opts := e.svc.opts
	opts.DB = otherDB
	other, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Flush)
	services := []*Service{e.svc, other}
	start := make(chan struct{})
	type result struct {
		request *SubmitResult
		session *Session
		err     error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			<-start
			r, err := svc.Submit(context.Background(), "ag_1", keyedSubmission("concurrent"))
			if err != nil {
				results <- result{err: err}
				return
			}
			ss, err := svc.StartSessionWithKey(context.Background(), "ag_1", "One job", "repo", "main", "host", "concurrent")
			results <- result{request: r, session: ss, err: err}
		}(services[i%len(services)])
	}
	close(start)
	wg.Wait()
	close(results)
	var requestID, sessionID string
	created := 0
	for r := range results {
		if r.err != nil || r.request == nil || !r.request.OK || r.session == nil {
			t.Fatalf("concurrent submit: %+v", r)
		}
		if requestID == "" {
			requestID, sessionID = r.request.RequestID, r.session.ID
		}
		if requestID != r.request.RequestID || sessionID != r.session.ID {
			t.Fatalf("concurrent retry created distinct records: %+v", r)
		}
		if !r.request.Replayed {
			created++
		}
	}
	for _, svc := range services {
		svc.Flush()
	}
	if created != 1 || j.calls.Load() != 1 {
		t.Fatalf("created %d requests, ran %d background checks", created, j.calls.Load())
	}
	for _, table := range []string{"inbox_requests", "inbox_pushes", "agent_sessions"} {
		var count int
		if err := e.svc.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s contains %d rows: %v", table, count, err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) != 3 { // arrival, completed check, session creation
		t.Fatalf("retry published extra events: %v", e.events)
	}
}

func TestIdempotentSessionKeysAndUpdates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := func(agent, title, repo, branch, host, key string) *Session {
		t.Helper()
		s, err := e.svc.StartSessionWithKey(ctx, agent, title, repo, branch, host, key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := start("ag_1", "Ship billing", "repo", "main", "host", "job")
	for _, status := range []string{SessionWorking, SessionWaitingOnOwner, SessionBlockedOnOwner, SessionIdle, SessionDone} {
		e.advance(time.Minute)
		updated, err := e.svc.UpdateSession(ctx, "ag_1", first.ID, status, "The owner can see this note")
		if err != nil {
			t.Fatal(err)
		}
		e.advance(time.Minute)
		retry := start("ag_1", " Ship billing ", " repo ", " main ", " host ", "job")
		if *retry != *updated {
			t.Fatalf("retry changed session state: %+v, want %+v", retry, updated)
		}
	}
	if _, err := e.svc.UpdateSession(ctx, "ag_2", first.ID, SessionWorking, ""); !errors.Is(err, ErrSessionNotYours) {
		t.Fatalf("other agent updated session: %v", err)
	}
	for i := 0; i < 4; i++ {
		fields := []string{"Ship billing", "repo", "main", "host"}
		fields[i] += " changed"
		if _, err := e.svc.StartSessionWithKey(ctx, "ag_1", fields[0], fields[1], fields[2], fields[3], "job"); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed field %d accepted: %v", i, err)
		}
	}
	for _, key := range []string{" ", strings.Repeat("x", MaxIdempotencyKey+1)} {
		if _, err := e.svc.StartSessionWithKey(ctx, "ag_1", "Ship billing", "", "", "", key); err == nil {
			t.Fatalf("bad key accepted: %q", key)
		}
	}
	ids := map[string]bool{first.ID: true}
	for _, c := range []struct{ agent, key string }{{"ag_1", "job-2"}, {"ag_2", "job"}, {"ag_1", ""}, {"ag_1", ""}} {
		s := start(c.agent, "Ship billing", "repo", "main", "host", c.key)
		if ids[s.ID] {
			t.Fatalf("separate session deduplicated: %+v", s)
		}
		ids[s.ID] = true
	}
}

func TestIdempotencySurvivesReopen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := mustSubmitWithKey(t, e.svc, "ag_1", keyedSubmission("persisted"))
	session, err := e.svc.StartSessionWithKey(ctx, "ag_1", "Ship billing", "repo", "main", "host", "persisted")
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Flush()
	if _, err := e.svc.Decide(ctx, first.RequestID, Decision{Action: "deny"}); err != nil {
		t.Fatal(err)
	}
	path := e.svc.db.Path
	if err := e.svc.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	opts := e.svc.opts
	opts.DB = db
	reopened, err := New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Flush)
	replay := mustSubmitWithKey(t, reopened, "ag_1", keyedSubmission("persisted"))
	if replay.RequestID != first.RequestID || replay.Status != StatusDenied {
		t.Fatalf("request lost after restart: %+v", replay)
	}
	ss, err := reopened.StartSessionWithKey(ctx, "ag_1", "Ship billing", "repo", "main", "host", "persisted")
	if err != nil || *ss != *session {
		t.Fatalf("session lost after restart: %+v, %v", ss, err)
	}
}

func TestIdempotentArrivalFailureRollsBackRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Interrupt the transaction at the exact old crash boundary: the request
	// INSERT succeeded, but its notification could not be persisted.
	if _, err := e.svc.db.Exec(`CREATE TRIGGER fail_arrival BEFORE INSERT ON inbox_pushes BEGIN SELECT RAISE(ABORT, 'arrival interrupted'); END`); err != nil {
		t.Fatal(err)
	}
	sub := questionSubmission(UrgencySoon)
	sub.IdempotencyKey = "interrupted-arrival"
	if _, err := e.svc.Submit(ctx, "ag_1", sub); err == nil {
		t.Fatal("submission succeeded without its notification")
	}
	for _, table := range []string{"inbox_requests", "inbox_pushes"} {
		var count int
		if err := e.svc.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("interrupted transaction left %d %s: %v", count, table, err)
		}
	}
	if len(e.events) != 0 {
		t.Fatalf("failed request published: %v", e.events)
	}
	if _, err := e.svc.db.Exec(`DROP TRIGGER fail_arrival`); err != nil {
		t.Fatal(err)
	}
	first := mustSubmitWithKey(t, e.svc, "ag_1", sub)
	if first.Replayed {
		t.Fatal("failed request reserved the key")
	}
}

func TestIdempotentCommittedArrivalSurvivesRestart(t *testing.T) {
	e, pushes := newAttnEnv(t, AttentionConfig{})
	ctx := context.Background()
	sub := questionSubmission(UrgencySoon)
	sub.IdempotencyKey = "committed-arrival"
	r, problems, _ := Validate(ctx, nil, "ag_1", sub)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	hash, err := submissionHash(r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID, r.AgentID, r.Status = "rq_committed", "ag_1", StatusPending
	r.CreatedAt, r.UpdatedAt = e.now.UnixMilli(), e.now.UnixMilli()
	r.ExpiresAt = e.now.Add(RequestTTL).UnixMilli()
	if inserted, err := e.svc.insert(ctx, r, sub.IdempotencyKey, hash); err != nil || !inserted {
		t.Fatalf("commit: %v, %v", inserted, err)
	}
	// Restart before Submit can publish, start checks, or send notifications.
	path := e.svc.db.Path
	if err := e.svc.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	opts := e.svc.opts
	opts.DB = db
	reopened, err := New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Flush)
	if replay := mustSubmitWithKey(t, reopened, "ag_1", sub); replay.RequestID != r.ID || !replay.Replayed {
		t.Fatalf("retry created a different request: %+v", replay)
	}
	if count := reopened.RecheckUnchecked(ctx); count != 1 {
		t.Fatalf("restart did not recover background check: %d", count)
	}
	reopened.Flush()
	e.advance(GroupWindow + time.Second)
	reopened.Tick(ctx)
	if got := pushes.take(); len(got) != 1 {
		t.Fatalf("committed arrival was lost: %+v", got)
	}
	mustSubmitWithKey(t, reopened, "ag_1", sub)
	reopened.Tick(ctx)
	if got := pushes.take(); len(got) != 0 {
		t.Fatalf("retry sent another notification: %+v", got)
	}
}

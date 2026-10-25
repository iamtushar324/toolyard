package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

type eventLog struct {
	mu  sync.Mutex
	got map[string]bool
}

func (a *eventLog) JobEvent(_ context.Context, eventType string, j Job, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got[eventType+" "+j.ID] = true
}

func (a *eventLog) has(eventType, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got[eventType+" "+id]
}

// A final write whose commit landed but whose answer was lost reads as
// "not moved" on the next try: settleFinal reads the row back, sees its
// own outcome, and records the job's audit row as for any settle.
func TestFinalWriteWithALostCommitAnswerStillCounts(t *testing.T) {
	db := storetest.Open(t)
	if db.Dialect.IsPostgres() {
		t.Skip("the failure is injected with a SQLite trigger")
	}
	svc := New(db, Options{PollEvery: 20 * time.Millisecond})
	aud := &eventLog{got: map[string]bool{}}
	svc.SetAuditor(aud)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svc.Wait(ctx)
	})
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER lose_ack BEFORE UPDATE OF status ON tool_jobs
        WHEN NEW.status = 'outcome_unknown' AND OLD.status = 'running'
      BEGIN SELECT RAISE(ABORT, 'injected: the commit answer was lost'); END`); err != nil {
		t.Fatal(err)
	}
	const why = "the call may or may not have run: the upstream connection dropped"
	var once sync.Once
	var hookErr error
	hook := func(id string, err error) {
		if err == nil {
			return
		}
		once.Do(func() {
			// As if that commit had landed: the row carries the outcome.
			if _, e := db.ExecContext(ctx, `DROP TRIGGER lose_ack`); e != nil {
				hookErr = e
				return
			}
			_, hookErr = db.ExecContext(ctx, `UPDATE tool_jobs SET status = 'outcome_unknown', error = ?, finished_at = 1 WHERE id = ?`, why, id)
		})
	}
	settleAttemptHook.Store(&hook)
	t.Cleanup(func() { settleAttemptHook.Store(nil) })

	release := make(chan struct{})
	job, h, err := svc.Start(ctx, StartRequest{AgentID: "ag_1", Tool: "alpha.write", Upstream: "alpha", ParamsHash: "h",
		Reason: "test a final write whose commit answer was lost", Via: "direct"},
		func(context.Context, ProgressFunc) Outcome {
			<-release
			return Outcome{Status: StatusOutcomeUnknown, Error: why}
		})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the job did not finish")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !aud.has(EventJobOutcomeUnknown, job.ID) {
		t.Fatal("the outcome whose commit answer was lost got no audit row")
	}
}

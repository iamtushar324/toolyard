package inbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// One malformed legacy row no longer aborts the whole sync: it is skipped
// and named, and every other row is committed.
func TestSyncLegacySkipsMalformedRowAndCommitsTheRest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	deadline := time.Now().Add(time.Hour).UnixMilli()
	for _, row := range []struct{ id, args string }{
		{"ap_good", `{"path":"ok.txt"}`},
		{"ap_bad", `{not json`},
		{"ap_good2", `{"path":"ok2.txt"}`},
	} {
		if _, err := e.svc.db.Exec(`INSERT INTO approval_requests(id,agent_id,upstream_name,tool_name,arguments,reason,status,created_at,expires_at) VALUES(?,'ag_legacy','fixture','fixture.write',?,'Original explanation','pending',?,?)`, row.id, row.args, now, deadline); err != nil {
			t.Fatal(err)
		}
	}
	err := e.svc.SyncLegacy(ctx)
	var skip *LegacySkipError
	if !errors.As(err, &skip) {
		t.Fatalf("SyncLegacy = %v, want a *LegacySkipError", err)
	}
	if !reflect.DeepEqual(skip.IDs, []string{"ap_bad"}) || len(skip.Errs) != 1 {
		t.Fatalf("skipped = %v (%d errors), want [ap_bad]", skip.IDs, len(skip.Errs))
	}
	for _, id := range []string{"ap_good", "ap_good2"} {
		if r, err := e.svc.Get(ctx, id); err != nil || r.ExecutionMode != "legacy" {
			t.Fatalf("%s not committed: %v", id, err)
		}
	}
	if _, err := e.svc.Get(ctx, "ap_bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ap_bad: %v, want ErrNotFound", err)
	}
	// The pass reports the same outcome and does not stop its caller.
	if err := e.svc.LegacySyncPass(ctx); !errors.As(err, &skip) {
		t.Fatalf("LegacySyncPass = %v, want a *LegacySkipError", err)
	}
}

// ReleaseWaiters (shutdown) ends a running Wait with ErrRestarting.
func TestWaitReturnsOnRelease(t *testing.T) {
	e := newEnv(t)
	r := submitDeploy(t, e, "ag_1")
	type result struct {
		views []AgentView
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := e.svc.Wait(context.Background(), "ag_1", []string{r.ID}, "any", time.Minute)
		done <- result{v, err}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("Wait returned before the release: %v", res.err)
	default:
	}
	e.svc.ReleaseWaiters()
	e.svc.ReleaseWaiters() // a second call is harmless
	select {
	case res := <-done:
		if !errors.Is(res.err, ErrRestarting) {
			t.Fatalf("Wait = %v, want ErrRestarting", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return on release")
	}
	// A Wait that starts after the release ends at once too.
	if _, err := e.svc.Wait(context.Background(), "ag_1", []string{r.ID}, "any", time.Minute); !errors.Is(err, ErrRestarting) {
		t.Fatalf("Wait after release = %v, want ErrRestarting", err)
	}
}

// Sync requests made before the next pass coalesce into that one pass.
func TestLegacySyncLoopCoalescesRequests(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.svc.RequestLegacySync()
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		e.svc.RunLegacySync(ctx, time.Hour)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	waitPasses := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for e.svc.legacyPasses.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("passes = %d, want %d", e.svc.legacyPasses.Load(), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitPasses(1)
	time.Sleep(50 * time.Millisecond)
	if got := e.svc.legacyPasses.Load(); got != 1 {
		t.Fatalf("five requests ran %d passes, want 1", got)
	}
	e.svc.RequestLegacySync()
	waitPasses(2)
}

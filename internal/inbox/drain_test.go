package inbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
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

// The legacy sync writes only rows that changed, and the periodic pass
// skips its transaction while approval_requests and the claims are as the
// last committed pass found them. A signal always runs a pass.
func TestLegacySyncSkipsUnchangedWork(t *testing.T) {
	storetest.SkipUnlessSQLite(t) // counts writes with SQLite triggers
	e := newEnv(t)
	ctx := context.Background()
	// Count every write to inbox_requests.
	for _, q := range []string{
		`CREATE TABLE test_inbox_writes(n INTEGER)`,
		`CREATE TRIGGER test_inbox_insert AFTER INSERT ON inbox_requests BEGIN INSERT INTO test_inbox_writes VALUES(1); END`,
		`CREATE TRIGGER test_inbox_update AFTER UPDATE ON inbox_requests BEGIN INSERT INTO test_inbox_writes VALUES(1); END`,
	} {
		if _, err := e.svc.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	writes := func() int {
		t.Helper()
		var n int
		if err := e.svc.db.QueryRow(`SELECT count(*) FROM test_inbox_writes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Now().UnixMilli()
	deadline := time.Now().Add(time.Hour).UnixMilli()
	for _, id := range []string{"ap_one", "ap_two"} {
		if _, err := e.svc.db.Exec(`INSERT INTO approval_requests(id,agent_id,upstream_name,tool_name,arguments,reason,status,created_at,expires_at) VALUES(?,'ag_legacy','fixture','fixture.write','{"path":"x.txt"}','Original explanation','pending',?,?)`, id, now, deadline); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.LegacySyncPass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := writes(); got != 2 {
		t.Fatalf("first pass wrote %d rows, want 2", got)
	}
	// Nothing changed: a full pass writes nothing.
	if err := e.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	if got := writes(); got != 2 {
		t.Fatalf("an unchanged pass wrote %d rows, want 0", got-2)
	}

	// Periodic passes: no transaction while nothing changed.
	passes := e.svc.legacyPasses.Load()
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		e.svc.RunLegacySync(runCtx, 5*time.Millisecond)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	time.Sleep(60 * time.Millisecond)
	if got := e.svc.legacyPasses.Load(); got != passes {
		t.Fatalf("periodic passes ran %d syncs on unchanged tables, want 0", got-passes)
	}

	// One row changes: the next periodic pass syncs it, and only it.
	if _, err := e.svc.db.Exec(`UPDATE approval_requests SET status='denied', decided_by='u_owner', decided_at=? WHERE id='ap_two'`, now+1); err != nil {
		t.Fatal(err)
	}
	deadlineAt := time.Now().Add(3 * time.Second)
	for e.svc.legacyPasses.Load() == passes {
		if time.Now().After(deadlineAt) {
			t.Fatal("the periodic pass did not notice the change")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	if got := e.svc.legacyPasses.Load(); got != passes+1 {
		t.Fatalf("periodic syncs after one change = %d, want 1", got-passes)
	}
	if got := writes(); got != 3 {
		t.Fatalf("writes after one changed row = %d, want 3", got)
	}
	if r, err := e.svc.Get(ctx, "ap_two"); err != nil || r.Status != "denied" {
		t.Fatalf("ap_two not synced: %v", err)
	}

	// A signal always runs a pass, changed or not.
	e.svc.RequestLegacySync()
	deadlineAt = time.Now().Add(3 * time.Second)
	for e.svc.legacyPasses.Load() != passes+2 {
		if time.Now().After(deadlineAt) {
			t.Fatalf("the signalled pass did not run; passes=%d", e.svc.legacyPasses.Load()-passes)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

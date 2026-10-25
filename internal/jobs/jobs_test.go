package jobs_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// recNotifier records every finished job it is told about, inside the
// finishing transaction.
type recNotifier struct {
	mu  sync.Mutex
	got []jobs.Job
	err error
}

func (n *recNotifier) JobFinished(ctx context.Context, tx *sql.Tx, j jobs.Job) error {
	// The transaction is usable: the event would be queued in it.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.got = append(n.got, j)
	return nil
}

func (n *recNotifier) all() []jobs.Job {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]jobs.Job(nil), n.got...)
}

func (n *recNotifier) find(id string) (jobs.Job, bool) {
	for _, j := range n.all() {
		if j.ID == id {
			return j, true
		}
	}
	return jobs.Job{}, false
}

func newService(t *testing.T, db *store.DB, opts jobs.Options) *jobs.Service {
	t.Helper()
	if opts.PollEvery == 0 {
		opts.PollEvery = 20 * time.Millisecond
	}
	svc := jobs.New(db, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svc.Wait(ctx)
	})
	return svc
}

func startReq(agent, tool string) jobs.StartRequest {
	return jobs.StartRequest{AgentID: agent, OwnerUserID: "u_1", Tool: tool, Upstream: "alpha", ParamsHash: "h1",
		Reason: "test the async job service end to end", Via: "direct"}
}

func waitFinal(t *testing.T, svc *jobs.Service, id string) *jobs.Job {
	t.Helper()
	list, timedOut, err := svc.WaitFor(context.Background(), []string{id}, false, 10*time.Second)
	if err != nil || timedOut || len(list) != 1 || list[0] == nil || !list[0].Final() {
		t.Fatalf("job %s did not finish: timedOut=%v err=%v list=%v", id, timedOut, err, list)
	}
	return list[0]
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartRunsTheCallAndKeepsTheResult(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()

	req := startReq("ag_1", "alpha.get_report")
	req.CallbackRef = "cb_1"
	job, h, err := svc.Start(ctx, req, func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: `{"content":[{"type":"text","text":"done"}]}`}
	})
	if err != nil || h == nil || job.Status != jobs.StatusQueued || !strings.HasPrefix(job.ID, "job_") {
		t.Fatalf("Start = %+v %v %v", job, h, err)
	}
	<-h.Done()
	got := waitFinal(t, svc, job.ID)
	if got.Status != jobs.StatusSucceeded || got.StartedAt == 0 || got.FinishedAt == 0 || !got.HasResult() || got.ResultInBlobStore() {
		t.Fatalf("final job = %+v", got)
	}
	if got.AgentID != "ag_1" || got.OwnerUserID != "u_1" || got.Tool != "alpha.get_report" || got.CallbackRef != "cb_1" || got.OwnerInstance != svc.InstanceID() {
		t.Fatalf("stored fields = %+v", got)
	}
	env, err := svc.Result(ctx, got)
	if err != nil || env != `{"content":[{"type":"text","text":"done"}]}` {
		t.Fatalf("Result = %q %v", env, err)
	}
	notice, ok := n.find(job.ID)
	if !ok || notice.Status != jobs.StatusSucceeded || notice.CallbackRef != "cb_1" {
		t.Fatalf("notifier got %+v (%v)", notice, ok)
	}
}

func TestLargeResultsGoToTheBlobStoreAndArePurged(t *testing.T) {
	db := storetest.Open(t)
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	svc := newService(t, db, jobs.Options{Blobs: blobs, InlineLimit: 16, Now: clock})
	ctx := context.Background()
	big := `{"content":[{"type":"text","text":"` + strings.Repeat("x", 200) + `"}]}`
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_big"), func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: big}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitFinal(t, svc, job.ID)
	if !got.ResultInBlobStore() || got.ResultBytes != len(big) {
		t.Fatalf("result not in the blob store: %+v", got)
	}
	if env, err := svc.Result(ctx, got); err != nil || env != big {
		t.Fatalf("Result from blob = %d bytes, %v", len(env), err)
	}
	var inline sql.NullString
	if err := db.QueryRow(`SELECT result FROM tool_jobs WHERE id = ?`, job.ID).Scan(&inline); err != nil || inline.Valid {
		t.Fatalf("inline result kept: %v %v", inline, err)
	}

	// Not yet past retention: kept.
	if n, err := svc.Purge(ctx); err != nil || n != 0 {
		t.Fatalf("early Purge = %d %v", n, err)
	}
	clockMu.Lock()
	now = now.Add(jobs.DefaultRetention + time.Hour)
	clockMu.Unlock()
	if n, err := svc.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("Purge = %d %v", n, err)
	}
	if _, err := svc.Get(ctx, job.ID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("purged job still readable: %v", err)
	}
	if ok, _ := blobs.Exists(ctx, sha256Hex(big)); ok {
		t.Fatal("the result blob outlived its job")
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestCancelARunningJob(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()
	started := make(chan struct{})
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		close(started)
		<-ctx.Done()
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Error: "context canceled"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	j, cancelled, err := svc.Cancel(ctx, job.ID)
	if err != nil || !cancelled || j == nil {
		t.Fatalf("Cancel = %+v %v %v", j, cancelled, err)
	}
	got := waitFinal(t, svc, job.ID)
	if got.Status != jobs.StatusCancelled || !strings.Contains(got.Error, "may already have taken effect") {
		t.Fatalf("cancelled job = %+v", got)
	}
	// Cancelling a final job changes nothing.
	if _, again, err := svc.Cancel(ctx, job.ID); err != nil || again {
		t.Fatalf("second Cancel = %v %v", again, err)
	}
	if notice, ok := n.find(job.ID); !ok || notice.Status != jobs.StatusCancelled {
		t.Fatalf("notifier got %+v (%v)", notice, ok)
	}
}

func TestCancelAQueuedJobRunsNothing(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{MaxRunning: 1})
	ctx := context.Background()
	release := make(chan struct{})
	first, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "first running", func() bool { j, _ := svc.Get(ctx, first.ID); return j != nil && j.Status == jobs.StatusRunning })
	var ran sync.WaitGroup
	ran.Add(1)
	second, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_other"), func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		ran.Done()
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := svc.Get(ctx, second.ID); j.Status != jobs.StatusQueued {
		t.Fatalf("second job = %s, want queued behind the slot limit", j.Status)
	}
	if _, cancelled, err := svc.Cancel(ctx, second.ID); err != nil || !cancelled {
		t.Fatalf("Cancel queued = %v %v", cancelled, err)
	}
	got := waitFinal(t, svc, second.ID)
	if got.Status != jobs.StatusCancelled || got.StartedAt != 0 || !strings.Contains(got.Error, "nothing ran") {
		t.Fatalf("cancelled queued job = %+v", got)
	}
	close(release)
	waitFinal(t, svc, first.ID)
	done := make(chan struct{})
	go func() { ran.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("a cancelled queued job ran")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInlineWaitSkipsTheCallbackOnlyWhenTheCallerGotTheResult(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()

	fast := startReq("ag_1", "alpha.get_fast")
	fast.InlineWait = true
	job, h, err := svc.Start(ctx, fast, func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: "fast"}
	})
	if err != nil {
		t.Fatal(err)
	}
	out, inline := h.WaitInline(ctx, 5*time.Second)
	if !inline || out.Envelope != "fast" {
		t.Fatalf("WaitInline = %+v %v", out, inline)
	}
	waitFinal(t, svc, job.ID)
	if _, ok := n.find(job.ID); ok {
		t.Fatal("a result delivered inline also sent a callback")
	}

	slow := startReq("ag_1", "alpha.get_slow")
	slow.InlineWait = true
	release := make(chan struct{})
	job2, h2, err := svc.Start(ctx, slow, func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: "slow"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, inline := h2.WaitInline(ctx, 50*time.Millisecond); inline {
		t.Fatal("a slow job was delivered inline")
	}
	close(release)
	waitFinal(t, svc, job2.ID)
	if _, ok := n.find(job2.ID); !ok {
		t.Fatal("a job handed back as a job id sent no callback")
	}
}

func TestIdempotencyKeyReplaysTheSameJob(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	run := func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
	req := startReq("ag_1", "alpha.get_report")
	req.IdempotencyKey = "key-1"
	first, h, err := svc.Start(ctx, req, run)
	if err != nil || h == nil {
		t.Fatal(err)
	}
	again, h2, err := svc.Start(ctx, req, run)
	if err != nil || h2 != nil || again.ID != first.ID {
		t.Fatalf("replay = %+v %v %v", again, h2, err)
	}
	other := req
	other.ParamsHash = "h2"
	if _, _, err := svc.Start(ctx, other, run); !errors.Is(err, jobs.ErrIdempotencyKeyReused) {
		t.Fatalf("other params with the same key = %v", err)
	}
	// Another agent's key is its own.
	req.AgentID = "ag_2"
	if j, _, err := svc.Start(ctx, req, run); err != nil || j.ID == first.ID {
		t.Fatalf("another agent's key = %+v %v", j, err)
	}
	if _, _, err := svc.Start(ctx, jobs.StartRequest{AgentID: "ag_1", Tool: "x", IdempotencyKey: "bad\nkey"}, run); err == nil {
		t.Fatal("a control character in the key was accepted")
	}
}

func TestTooManyActiveJobs(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{MaxActive: 2})
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), block); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), block); !errors.Is(err, jobs.ErrTooManyActive) {
		t.Fatalf("third active job = %v", err)
	}
	if _, _, err := svc.Start(ctx, startReq("ag_2", "alpha.get_slow"), block); err != nil {
		t.Fatalf("another agent is not capped by ag_1's jobs: %v", err)
	}
}

func TestNotifierFailureStillRecordsTheStatus(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{err: errors.New("unknown receiver cb_x")}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_report"), func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusFailed, Envelope: "boom", IsError: true, Error: "boom"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitFinal(t, svc, job.ID)
	if got.Status != jobs.StatusFailed || !got.ResultIsError || !strings.Contains(got.CallbackError, "unknown receiver") {
		t.Fatalf("job after a notifier failure = %+v", got)
	}
}

func TestProgressIsRecorded(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	release := make(chan struct{})
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, progress jobs.ProgressFunc) jobs.Outcome {
		total := 4.0
		progress(1, &total, "a quarter")
		progress(2, &total, "half") // within a second: kept for the heartbeat
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "first progress", func() bool {
		j, _ := svc.Get(ctx, job.ID)
		return j != nil && j.Progress != nil && j.Progress.Message == "a quarter"
	})
	svc.Heartbeat(ctx)
	j, _ := svc.Get(ctx, job.ID)
	if j.Progress == nil || j.Progress.Progress != 2 || j.Progress.Total == nil || *j.Progress.Total != 4 || j.Progress.Message != "half" {
		t.Fatalf("progress after the heartbeat = %+v", j.Progress)
	}
	close(release)
	waitFinal(t, svc, job.ID)
}

func TestListScopesAndFilters(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	ok := func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
	a1, _, _ := svc.Start(ctx, startReq("ag_1", "alpha.one"), ok)
	a2, _, _ := svc.Start(ctx, startReq("ag_1", "alpha.two"), ok)
	b1req := startReq("ag_2", "alpha.three")
	b1req.OwnerUserID = "u_2"
	b1, _, _ := svc.Start(ctx, b1req, ok)
	for _, j := range []*jobs.Job{a1, a2, b1} {
		waitFinal(t, svc, j.ID)
	}
	mine, err := svc.List(ctx, jobs.Filter{AgentID: "ag_1"})
	if err != nil || len(mine) != 2 {
		t.Fatalf("ag_1 jobs = %d %v", len(mine), err)
	}
	for _, j := range mine {
		if j.AgentID != "ag_1" {
			t.Fatalf("another agent's job listed: %+v", j)
		}
	}
	if owned, _ := svc.List(ctx, jobs.Filter{OwnerUserID: "u_2"}); len(owned) != 1 || owned[0].ID != b1.ID {
		t.Fatalf("u_2's jobs = %+v", owned)
	}
	if none, _ := svc.List(ctx, jobs.Filter{}); len(none) != 0 {
		t.Fatalf("an unscoped filter listed %d jobs", len(none))
	}
	if all, _ := svc.List(ctx, jobs.Filter{All: true, Status: jobs.StatusSucceeded}); len(all) != 3 {
		t.Fatalf("all succeeded = %d", len(all))
	}
	if active, _ := svc.List(ctx, jobs.Filter{All: true, Status: "active"}); len(active) != 0 {
		t.Fatalf("active = %d", len(active))
	}
	page, _ := svc.List(ctx, jobs.Filter{AgentID: "ag_1", Limit: 1})
	if len(page) != 1 {
		t.Fatalf("page = %d", len(page))
	}
	rest, _ := svc.List(ctx, jobs.Filter{AgentID: "ag_1", Limit: 5, BeforeCreated: page[0].CreatedAt, BeforeID: page[0].ID})
	if len(rest) != 1 || rest[0].ID == page[0].ID {
		t.Fatalf("next page = %+v", rest)
	}
}

func TestRecordStoresAFinalJob(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()
	j, err := svc.Record(ctx, startReq("ag_1", "alpha.write"), jobs.Outcome{Status: jobs.StatusFailed, Envelope: "denied", IsError: true, Error: "denied by policy"})
	if err != nil || j.Status != jobs.StatusFailed || !j.Final() || j.Error != "denied by policy" {
		t.Fatalf("Record = %+v %v", j, err)
	}
	if _, ok := n.find(j.ID); !ok {
		t.Fatal("Record sent no notice")
	}
}

// Two replicas on one database: recovery on one never touches a job whose
// lease the other keeps renewing; once that replica stops (its heartbeat
// ends), its running job becomes outcome_unknown and its queued job
// failed, and the call is never run again.
func TestRecoveryTakesOnlyExpiredLeases(t *testing.T) {
	db := storetest.Open(t)
	const lease = 500 * time.Millisecond
	nB := &recNotifier{}
	a := newService(t, db, jobs.Options{InstanceID: "node-a", LeaseTTL: lease, MaxRunning: 1})
	b := newService(t, db, jobs.Options{InstanceID: "node-b", LeaseTTL: lease, Notifier: nB})
	ctx := context.Background()

	aCtx, stopA := context.WithCancel(ctx)
	defer stopA()
	a.Run(aCtx)

	release := make(chan struct{})
	defer close(release)
	var runs sync.Map
	block := func(name string) jobs.RunFunc {
		return func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
			n, _ := runs.LoadOrStore(name, new(int))
			*(n.(*int))++
			select {
			case <-release:
			case <-ctx.Done():
			}
			return jobs.Outcome{Status: jobs.StatusSucceeded}
		}
	}
	running, _, err := a.Start(ctx, startReq("ag_1", "alpha.write_report"), block("running"))
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running on node-a", func() bool { j, _ := a.Get(ctx, running.ID); return j != nil && j.Status == jobs.StatusRunning })
	queued, _, err := a.Start(ctx, startReq("ag_1", "alpha.write_other"), block("queued"))
	if err != nil {
		t.Fatal(err)
	}

	// Node-a is alive: several lease periods pass, node-b recovers nothing.
	deadline := time.Now().Add(3 * lease)
	for time.Now().Before(deadline) {
		if failed, unknown, err := b.Recover(ctx); err != nil || failed+unknown != 0 {
			t.Fatalf("node-b recovered a live node's jobs: %d %d %v", failed, unknown, err)
		}
		time.Sleep(lease / 5)
	}
	for _, id := range []string{running.ID, queued.ID} {
		j, _ := b.Get(ctx, id)
		if j.Final() || j.OwnerInstance != "node-a" {
			t.Fatalf("live job changed: %+v", j)
		}
	}

	// Node-a stops renewing (it crashed). After its leases end, node-b
	// settles both jobs.
	stopA()
	time.Sleep(2 * lease)
	failed, unknown, err := b.Recover(ctx)
	if err != nil || failed != 1 || unknown != 1 {
		t.Fatalf("Recover = %d failed, %d unknown, %v", failed, unknown, err)
	}
	r, _ := b.Get(ctx, running.ID)
	if r.Status != jobs.StatusOutcomeUnknown || !strings.Contains(r.Error, "may or may not have run") {
		t.Fatalf("running job after recovery = %+v", r)
	}
	q, _ := b.Get(ctx, queued.ID)
	if q.Status != jobs.StatusFailed || !strings.Contains(q.Error, "nothing ran") || q.StartedAt != 0 {
		t.Fatalf("queued job after recovery = %+v", q)
	}
	if _, ok := nB.find(running.ID); !ok {
		t.Fatal("recovery of the running job sent no notice")
	}
	// A second pass finds nothing, and nothing ran twice.
	if f, u, err := b.Recover(ctx); err != nil || f+u != 0 {
		t.Fatalf("second Recover = %d %d %v", f, u, err)
	}
	if n, ok := runs.Load("running"); !ok || *(n.(*int)) != 1 {
		t.Fatalf("the running call ran %v times", n)
	}
	if _, ok := runs.Load("queued"); ok {
		t.Fatal("the queued call ran")
	}
}

func TestDrainRefusesNewJobs(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	svc.BeginDrain()
	if _, _, err := svc.Start(context.Background(), startReq("ag_1", "alpha.get"), func(context.Context, jobs.ProgressFunc) jobs.Outcome {
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}); !errors.Is(err, jobs.ErrDraining) {
		t.Fatalf("Start while draining = %v", err)
	}
}

func TestShutdownStopsRunningJobsAsOutcomeUnknown(t *testing.T) {
	db := storetest.Open(t)
	svc := jobs.New(db, jobs.Options{PollEvery: 20 * time.Millisecond})
	ctx := context.Background()
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-ctx.Done()
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Error: "cut off"}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	stop, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	svc.Wait(stop)
	j, _ := svc.Get(ctx, job.ID)
	if j.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("job after shutdown = %+v", j)
	}
}

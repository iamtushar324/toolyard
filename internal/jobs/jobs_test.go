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
	if ok, _ := blobs.Exists(ctx, jobBlobKey(big)); !ok {
		t.Fatal("the result is not under its domain-separated key")
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
	if ok, _ := blobs.Exists(ctx, jobBlobKey(big)); ok {
		t.Fatal("the result blob outlived its job")
	}
	// The key is domain-separated: never the content's own sha256, which
	// an Inbox attachment of the same bytes would use.
	if jobBlobKey(big) == sha256Hex(big) {
		t.Fatal("job blob key equals the content hash")
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func jobBlobKey(env string) string { return sha256Hex("tool_job\x00" + env) }

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
	reported := make(chan struct{})
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, progress jobs.ProgressFunc) jobs.Outcome {
		total := 4.0
		// Neither call touches the database on this goroutine.
		progress(1, &total, "a quarter")
		progress(2, &total, "half")
		close(reported)
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-reported
	waitUntil(t, "the latest progress", func() bool {
		svc.Heartbeat(ctx)
		j, _ := svc.Get(ctx, job.ID)
		return j != nil && j.Progress != nil && j.Progress.Message == "half"
	})
	j, _ := svc.Get(ctx, job.ID)
	if j.Progress.Progress != 2 || j.Progress.Total == nil || *j.Progress.Total != 4 {
		t.Fatalf("progress = %+v", j.Progress)
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
	audB := &recAuditor{}
	b.SetAuditor(audB)
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
	if !audB.has(jobs.EventJobRecovered, running.ID) || !audB.has(jobs.EventJobRecovered, queued.ID) {
		t.Fatal("recovery wrote no job.recovered audit rows")
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

// recAuditor records the job events the service reports.
type recAuditor struct {
	mu  sync.Mutex
	got []string
}

func (a *recAuditor) JobEvent(_ context.Context, eventType string, j jobs.Job, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, eventType+" "+j.ID)
}

func (a *recAuditor) has(eventType, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, g := range a.got {
		if g == eventType+" "+id {
			return true
		}
	}
	return false
}

func blockUntil(release <-chan struct{}) jobs.RunFunc {
	return func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

func TestAdoptRecordsACallAlreadyRunning(t *testing.T) {
	db := storetest.Open(t)
	n := &recNotifier{}
	svc := newService(t, db, jobs.Options{Notifier: n})
	ctx := context.Background()
	relay := jobs.NewProgressRelay()
	total := 2.0
	relay.Report(1, &total, "before the job existed")
	done := make(chan struct{})
	var out jobs.Outcome
	job, err := svc.Adopt(ctx, startReq("ag_1", "alpha.get_slow"), jobs.Adoption{
		StartedAt: time.Now().Add(-time.Second), Cancel: func() {}, Done: done, Relay: relay,
		Outcome: func() jobs.Outcome { return out },
	})
	if err != nil || job.Status != jobs.StatusRunning || job.StartedAt == 0 {
		t.Fatalf("Adopt = %+v %v", job, err)
	}
	waitUntil(t, "the relayed progress", func() bool {
		svc.Heartbeat(ctx)
		j, _ := svc.Get(ctx, job.ID)
		return j != nil && j.Progress != nil && j.Progress.Message == "before the job existed"
	})
	out = jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: "late answer"}
	close(done)
	got := waitFinal(t, svc, job.ID)
	if env, _ := svc.Result(ctx, got); got.Status != jobs.StatusSucceeded || env != "late answer" {
		t.Fatalf("adopted job = %+v %q", got, env)
	}
	if _, ok := n.find(job.ID); !ok {
		t.Fatal("an adopted job sent no notice")
	}
}

func TestAdoptedJobCancelStopsTheCall(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	done := make(chan struct{})
	var once sync.Once
	job, err := svc.Adopt(ctx, startReq("ag_1", "alpha.get_slow"), jobs.Adoption{
		StartedAt: time.Now(), Done: done,
		Cancel:  func() { once.Do(func() { close(done) }) },
		Outcome: func() jobs.Outcome { return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Error: "context canceled"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, cancelled, err := svc.Cancel(ctx, job.ID); err != nil || !cancelled {
		t.Fatalf("Cancel = %v %v", cancelled, err)
	}
	if got := waitFinal(t, svc, job.ID); got.Status != jobs.StatusCancelled {
		t.Fatalf("adopted job after cancel = %+v", got)
	}
}

func TestOwnerAndAnonymousCaps(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{MaxActive: 10, MaxActivePerOwner: 2, MaxActiveAnonymous: 1})
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	for _, agent := range []string{"ag_1", "ag_2"} {
		if _, _, err := svc.Start(ctx, startReq(agent, "alpha.get_slow"), blockUntil(release)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.Start(ctx, startReq("ag_3", "alpha.get_slow"), blockUntil(release)); !errors.Is(err, jobs.ErrTooManyActive) {
		t.Fatalf("a third job of owner u_1 = %v", err)
	}
	other := startReq("ag_4", "alpha.get_slow")
	other.OwnerUserID = "u_2"
	if _, _, err := svc.Start(ctx, other, blockUntil(release)); err != nil {
		t.Fatalf("another owner is not capped by u_1: %v", err)
	}
	anon := startReq("", "alpha.get_slow")
	anon.OwnerUserID = ""
	if _, _, err := svc.Start(ctx, anon, blockUntil(release)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Start(ctx, anon, blockUntil(release)); !errors.Is(err, jobs.ErrTooManyActive) {
		t.Fatalf("a second anonymous job = %v", err)
	}
}

// Concurrent starts never pass the cap together: the count and the insert
// are one step (an advisory lock on Postgres).
func TestConcurrentStartsKeepTheCap(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{MaxActive: 3})
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), blockUntil(release)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("%d concurrent starts passed a cap of 3", ok)
	}
}

func TestRunSlotsAreSharedBetweenAgents(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{MaxRunning: 2, MaxRunningPerAgent: 1})
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	running := func(id string) bool { j, _ := svc.Get(ctx, id); return j != nil && j.Status == jobs.StatusRunning }
	a1, _, _ := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), blockUntil(release))
	waitUntil(t, "ag_1's first job running", func() bool { return running(a1.ID) })
	a2, _, _ := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), blockUntil(release))
	b1req := startReq("ag_2", "alpha.get_slow")
	b1req.OwnerUserID = "u_2"
	b1, _, _ := svc.Start(ctx, b1req, blockUntil(release))
	waitUntil(t, "ag_2's job running beside ag_1's", func() bool { return running(b1.ID) })
	if running(a2.ID) {
		t.Fatal("ag_1 took more than its share of the run slots")
	}
}

func TestUserCancelKeepsTheCallsOwnAnswer(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-ctx.Done()
		return jobs.Outcome{Status: jobs.StatusFailed, Envelope: "tool error", IsError: true, Error: "tool error"}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	if _, _, err := svc.Cancel(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitFinal(t, svc, job.ID); got.Status != jobs.StatusFailed || got.Error != "tool error" {
		t.Fatalf("a call that answered after the cancel = %+v", got)
	}
}

// A run whose row another replica settled (its lease lapsed) is stopped
// at the next heartbeat, and its late outcome is not kept.
func TestHeartbeatStopsARunThatLostItsRow(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	ctx := context.Background()
	stopped := make(chan struct{})
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-ctx.Done()
		close(stopped)
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: "late"}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	if _, err := db.ExecContext(ctx, `UPDATE tool_jobs SET status = 'outcome_unknown', finished_at = 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	svc.Heartbeat(ctx)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the run that lost its row was not stopped")
	}
	waitUntil(t, "the goroutine to end", func() bool { return svc.Running() == 0 })
	if j, _ := svc.Get(ctx, job.ID); j.Status != jobs.StatusOutcomeUnknown || j.HasResult() {
		t.Fatalf("row after the lost run = %+v", j)
	}
}

func TestGuardStopsAJobWhoseAccessWasRemoved(t *testing.T) {
	db := storetest.Open(t)
	svc := newService(t, db, jobs.Options{})
	aud := &recAuditor{}
	svc.SetAuditor(aud)
	var mu sync.Mutex
	revoked := map[string]bool{}
	svc.SetGuard(func(_ context.Context, agentID, _, _ string) bool {
		mu.Lock()
		defer mu.Unlock()
		return !revoked[agentID]
	})
	ctx := context.Background()
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-ctx.Done()
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Error: "context canceled"}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	svc.Heartbeat(ctx)
	if j, _ := svc.Get(ctx, job.ID); j.Final() {
		t.Fatal("the guard stopped a job that still has access")
	}
	mu.Lock()
	revoked["ag_1"] = true
	mu.Unlock()
	svc.Heartbeat(ctx)
	got := waitFinal(t, svc, job.ID)
	if got.Status != jobs.StatusCancelled || !strings.Contains(got.Error, "access to this tool was removed") {
		t.Fatalf("revoked job = %+v", got)
	}
	waitUntil(t, "the revoked audit row", func() bool { return aud.has(jobs.EventJobRevoked, job.ID) })
}

func TestOrphanedResultBlobIsDeleted(t *testing.T) {
	db := storetest.Open(t)
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(t, db, jobs.Options{Blobs: blobs, InlineLimit: 16})
	ctx := context.Background()
	release := make(chan struct{})
	big := strings.Repeat("y", 300)
	job, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_big"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: big}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "running", func() bool { j, _ := svc.Get(ctx, job.ID); return j != nil && j.Status == jobs.StatusRunning })
	// Another replica settled it meanwhile.
	if _, err := db.ExecContext(ctx, `UPDATE tool_jobs SET status = 'outcome_unknown', finished_at = 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitUntil(t, "the goroutine to end", func() bool { return svc.Running() == 0 })
	if ok, _ := blobs.Exists(ctx, jobBlobKey(big)); ok {
		t.Fatal("a result blob for a row that did not move was kept")
	}
}

func TestNeverStartedJobIsAudited(t *testing.T) {
	db := storetest.Open(t)
	svc := jobs.New(db, jobs.Options{PollEvery: 20 * time.Millisecond, MaxRunning: 1})
	aud := &recAuditor{}
	svc.SetAuditor(aud)
	ctx := context.Background()
	first, _, err := svc.Start(ctx, startReq("ag_1", "alpha.get_slow"), func(ctx context.Context, _ jobs.ProgressFunc) jobs.Outcome {
		<-ctx.Done()
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown}
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the first job running", func() bool { j, _ := svc.Get(ctx, first.ID); return j != nil && j.Status == jobs.StatusRunning })
	queued, _, err := svc.Start(ctx, startReq("ag_2", "alpha.get_other"), blockUntil(make(chan struct{})))
	if err != nil {
		t.Fatal(err)
	}
	stop, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	svc.Wait(stop)
	if j, _ := svc.Get(ctx, queued.ID); j.Status != jobs.StatusFailed || j.StartedAt != 0 {
		t.Fatalf("queued job after shutdown = %+v", j)
	}
	if !aud.has(jobs.EventJobNotStarted, queued.ID) {
		t.Fatal("no job.not_started audit row")
	}
}

package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// TestAnomalyRunDoesNotDeadlockSingleConn runs a detection pass that finds a
// reason repeat, an error spike and an args outlier against the real store,
// which has exactly one connection. The detectors used to query while their
// cursor was still open, which waited forever for that one connection and
// froze every other DB caller in the process.
func TestAnomalyRunDoesNotDeadlockSingleConn(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "anomaly.db"))
	if err != nil {
		t.Fatal(err)
	}

	hourStart := time.Now().Truncate(time.Hour).Add(-time.Hour)
	lastHour := hourStart.Add(10 * time.Minute).UnixMilli()
	var evs []Event
	// Same _reason six times in the last full hour: reason_repeat.
	for i := 0; i < 6; i++ {
		evs = append(evs, Event{TS: lastHour + int64(i), AgentID: "ag_1", ToolName: "memory.set",
			ReasonText: "same placeholder reason every time", Outcome: "ok"})
	}
	// 25 calls, all errors, in the last full hour: error_spike.
	for i := 0; i < 25; i++ {
		evs = append(evs, Event{TS: lastHour + 100 + int64(i), AgentID: "ag_2", ToolName: "flaky.tool",
			ReasonText: "calling the flaky tool for test " + itoa(int64(i)), Outcome: "error"})
	}
	// A 30-day baseline of small args, then one huge call last hour: args_outlier.
	for i := 0; i < 60; i++ {
		evs = append(evs, Event{TS: hourStart.Add(-time.Duration(i+1) * time.Hour).UnixMilli(), AgentID: "ag_3",
			ToolName: "notes.write", ArgsSizeBytes: 100, Outcome: "ok"})
	}
	evs = append(evs, Event{TS: lastHour + 500, AgentID: "ag_3", ToolName: "notes.write", ArgsSizeBytes: 100000, Outcome: "ok"})
	if err := (&Recorder{db: db}).insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	r := NewReader(db)
	done := make(chan error, 1)
	go func() { done <- NewAnomalyDetector(r, 3).Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Don't close the DB: Close would wait on the stuck connection too.
		t.Fatal("anomaly Run deadlocked on the single DB connection")
	}
	defer db.Close()

	got, err := r.Anomalies(context.Background(), 50, true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	for _, k := range []string{"reason_repeat", "error_spike", "args_outlier"} {
		if !kinds[k] {
			t.Errorf("no %s anomaly recorded; got %v", k, kinds)
		}
	}
}

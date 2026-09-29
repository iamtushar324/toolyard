package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestRecorder(t *testing.T) (*Recorder, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// No run() goroutine: tests call insertBatch directly.
	return &Recorder{db: db}, db
}

// The upstream bucket used to query a column that does not exist
// (upstream_name), so the fallback silently skipped it. It must answer
// from call_events.upstream.
func TestApprovalLatencyByUpstream(t *testing.T) {
	r, db := newTestRecorder(t)
	now := time.Now().UnixMilli()
	var evs []Event
	for i := 0; i < MinApprovalLatencySamples; i++ {
		evs = append(evs, Event{
			TS: now - int64(i+1)*1000, AgentID: "ag_1", Upstream: "github", ShortName: "merge", ToolName: "github.merge",
			IsWrite: true, Outcome: OutcomeOK, ApprovalOutcome: ApprovalApproved, ApprovalLatencyMs: 10000 * (i + 1),
			// Distinct fingerprints and tool names keep the tighter buckets
			// under the sample minimum, so only the upstream bucket can answer.
			Fingerprint: "fp" + string(rune('a'+i)), Via: "direct",
		})
		evs[i].ToolName = "github.tool" + string(rune('a'+i))
	}
	if err := r.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	est := NewReader(db).ApprovalLatency(context.Background(), "nope", "github.other", "github")
	if est == nil || est.BasedOn != "upstream" {
		t.Fatalf("want an upstream-based estimate, got %+v", est)
	}
	if est.Samples != MinApprovalLatencySamples || est.P50Seconds != 30 {
		t.Fatalf("estimate: %+v", est)
	}
}

// insertBatch writes the actor columns migration 0025 added.
func TestInsertBatchWritesActorColumns(t *testing.T) {
	r, db := newTestRecorder(t)
	err := r.insertBatch([]Event{{
		TS: time.Now().UnixMilli(), AgentID: "ag_1", AgentName: "builder", SessionID: "ses_0123456789abcdef",
		OwnerUserID: "u_owner", ClientKind: "claude_code", Upstream: "github", ShortName: "merge",
		ToolName: "github.merge", Outcome: OutcomeOK, Via: "tools.execute",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var owner, kind, name, sess, via string
	err = db.QueryRowContext(context.Background(),
		`SELECT owner_user_id, client_kind, agent_name, session_id, via FROM call_events`).Scan(&owner, &kind, &name, &sess, &via)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "u_owner" || kind != "claude_code" || name != "builder" || sess != "ses_0123456789abcdef" || via != "tools.execute" {
		t.Fatalf("stored %q %q %q %q %q", owner, kind, name, sess, via)
	}
}

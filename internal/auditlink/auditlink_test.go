package auditlink

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var raiser = actor.Raiser{
	CallerID: "ag_1", AgentName: "claude-cloud-3", AgentKind: "agent",
	OwnerUserID: "u_owner", OwnerEmail: "owner@example.test",
	AgentSessionID: "ses_1", ClientSessionID: "t3-1", ClientKind: "t3", Via: "direct",
}

type fixture struct {
	bus *approval.Bus
	log *audit.Logger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	lg := audit.New(db)
	bus.AddNotifier(Notifier(lg))
	return &fixture{bus: bus, log: lg}
}

func (f *fixture) hold(t *testing.T, tool string) *approval.Request {
	t.Helper()
	req, err := f.bus.Hold(context.Background(), approval.NewRequest{
		AgentID: "ag_1", UpstreamName: "github", ToolName: tool, Reason: "needs a ticket",
		Arguments: map[string]any{"k": tool}, RaisedBy: raiser,
	}, 0)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	return req
}

// waitRow polls until the notifier's goroutine has written the row.
func (f *fixture) waitRow(t *testing.T, approvalID, eventType string) audit.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := f.log.Query(context.Background(), audit.Filter{ApprovalID: approvalID, EventType: eventType})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(rows) == 1 {
			return rows[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s row for %s", eventType, approvalID)
	return audit.Event{}
}

func checkRaiser(t *testing.T, ev audit.Event, req *approval.Request) {
	t.Helper()
	if ev.Raiser != raiser {
		t.Errorf("%s: raiser = %+v, want %+v", ev.EventType, ev.Raiser, raiser)
	}
	if ev.AgentID != "ag_1" || ev.ToolName != req.ToolName || ev.UpstreamName != "github" || ev.ApprovalID != req.ID {
		t.Errorf("%s: tool/upstream/approval = %q/%q/%q", ev.EventType, ev.ToolName, ev.UpstreamName, ev.ApprovalID)
	}
}

func TestAuditLink_CreateAndDecideRows(t *testing.T) {
	f := newFixture(t)
	req := f.hold(t, "github.create_issue")

	created := f.waitRow(t, req.ID, audit.EventApprovalCreate)
	checkRaiser(t, created, req)
	if created.Reason != "needs a ticket" || created.DecidedVia != "" {
		t.Errorf("create row: reason=%q decided_via=%q", created.Reason, created.DecidedVia)
	}

	// The decision comes from an HTTP handler whose context is cancelled
	// as soon as it returns; the row must still land.
	ctx, cancel := context.WithCancel(context.Background())
	d := actor.Decider{UserID: "u_dec", Email: "dec@example.test", Name: "Dec", Via: actor.ViaDashboard, Ref: "sess_1"}
	if _, err := f.bus.DecideAs(ctx, req.ID, approval.StatusAllowed, d); err != nil {
		t.Fatalf("decide: %v", err)
	}
	cancel()
	decided := f.waitRow(t, req.ID, audit.EventApprovalDecide)
	checkRaiser(t, decided, req)
	if decided.Decision != approval.StatusAllowed {
		t.Errorf("decide row decision = %q, want allowed", decided.Decision)
	}
	if decided.DecidedByUserID != "u_dec" || decided.DecidedByEmail != d.Email || decided.DecidedByName != "Dec" ||
		decided.DecidedVia != actor.ViaDashboard || decided.DeciderRef != "sess_1" {
		t.Errorf("decide row decider = %q/%q/%q via %q ref %q", decided.DecidedByUserID, decided.DecidedByEmail,
			decided.DecidedByName, decided.DecidedVia, decided.DeciderRef)
	}

	if err := f.bus.SetResult(context.Background(), req.ID, `{"content":[]}`, false, ""); err != nil {
		t.Fatalf("set result: %v", err)
	}
	executed := f.waitRow(t, req.ID, audit.EventApprovalExecuted)
	checkRaiser(t, executed, req)
	if executed.ResultSummary != "executed" || executed.DecidedByUserID != "u_dec" {
		t.Errorf("executed row: summary=%q decided_by=%q", executed.ResultSummary, executed.DecidedByUserID)
	}
}

func TestAuditLink_DeniedByToken(t *testing.T) {
	f := newFixture(t)
	req := f.hold(t, "github.delete_repo")
	tok := f.bus.DecisionTokenFor(req.ID, "u_phone")
	if _, _, err := f.bus.DecideByTokenAs(context.Background(), tok, approval.StatusDenied); err != nil {
		t.Fatalf("decide by token: %v", err)
	}
	row := f.waitRow(t, req.ID, audit.EventApprovalDecide)
	if row.Decision != approval.StatusDenied || row.DecidedByUserID != "u_phone" || row.DecidedVia != actor.ViaPushToken {
		t.Errorf("token decide row: decision=%q by=%q via=%q", row.Decision, row.DecidedByUserID, row.DecidedVia)
	}
}

func TestAuditLink_CancelRow(t *testing.T) {
	f := newFixture(t)
	req := f.hold(t, "github.cancelme")
	if _, err := f.bus.CancelByAgent(context.Background(), req.ID, "ag_1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	row := f.waitRow(t, req.ID, audit.EventApprovalCancel)
	checkRaiser(t, row, req)
	if row.Decision != approval.StatusCancelled || row.DecidedVia != actor.ViaAgentCancel || row.DeciderRef != "ag_1" {
		t.Errorf("cancel row: decision=%q via=%q ref=%q", row.Decision, row.DecidedVia, row.DeciderRef)
	}
}

func TestAuditLink_ExpireRow(t *testing.T) {
	f := newFixture(t)
	f.bus.SetTTL(time.Millisecond)
	req := f.hold(t, "github.expireme")
	time.Sleep(5 * time.Millisecond)
	if n, err := f.bus.SweepExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	row := f.waitRow(t, req.ID, audit.EventApprovalExpire)
	checkRaiser(t, row, req)
	if row.Decision != approval.StatusExpired || row.DecidedVia != actor.ViaExpiry || row.DecidedByUserID != "" {
		t.Errorf("expire row: decision=%q via=%q by=%q", row.Decision, row.DecidedVia, row.DecidedByUserID)
	}
}

func TestAuditLinkEventFor_IgnoresUnknownAndNilRaiser(t *testing.T) {
	req := &approval.Request{ID: "ap_x", AgentID: "ag_x", ToolName: "t", Status: approval.StatusAllowed,
		DecidedBy: "rule:ar_1", ResultError: "upstream gone"}
	if _, ok := EventFor(req, "approval.unknown"); ok {
		t.Fatal("unknown event type produced a row")
	}
	ev, ok := EventFor(req, "approval.decide")
	if !ok || ev.Raiser != (actor.Raiser{}) || ev.AgentID != "ag_x" {
		t.Fatalf("decide without RaisedBy: ok=%v ev=%+v", ok, ev)
	}
	if ev.DecidedVia != actor.ViaAutoRule || ev.DeciderRef != "ar_1" {
		t.Errorf("legacy rule decider not mapped: via=%q ref=%q", ev.DecidedVia, ev.DeciderRef)
	}
	ex, _ := EventFor(req, "approval.executed")
	if ex.ResultSummary != "execution failed: upstream gone" {
		t.Errorf("executed summary = %q", ex.ResultSummary)
	}
}

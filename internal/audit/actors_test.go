package audit

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

// fullRaiser has every Raiser field set so a round-trip test notices a
// column that is written but not read, or read from the wrong position.
var fullRaiser = actor.Raiser{
	CallerID:             "ag_1",
	AgentName:            "claude-cloud-3",
	AgentKind:            "agent",
	OwnerUserID:          "u_owner",
	OwnerEmail:           "owner@example.test",
	OwnerName:            "Owner Person",
	MCPSessionID:         "mcp-sess-1",
	AgentSessionID:       "ses_abc",
	ClientSessionID:      "t3-thread-9",
	ClientSessionClaimed: true,
	ClientKind:           "t3",
	ClientName:           "t3code/1.2",
	ClientIP:             "10.0.0.7",
	Via:                  "direct",
}

var fullDecider = actor.Decider{
	UserID: "u_decider",
	Email:  "decider@example.test",
	Name:   "Decider Person",
	Via:    actor.ViaDashboard,
	Ref:    "sess_dash_1",
}

func TestAuditWriteRoundTripsEveryColumn(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	in := Event{
		AgentID:       "ag_1",
		UpstreamName:  "github",
		ToolName:      "github.create_issue",
		EventType:     EventApprovalDecide,
		Decision:      "allowed",
		Reason:        "needs a ticket",
		Arguments:     json.RawMessage(`{"title":"x"}`),
		ResultSummary: "ok",
		ApprovalID:    "ap_1",
		Raiser:        fullRaiser,
	}
	in.SetDecider(fullDecider)
	if err := l.Write(ctx, in); err != nil {
		t.Fatalf("write: %v", err)
	}

	check := func(name string, got []Event) {
		t.Helper()
		if len(got) != 1 {
			t.Fatalf("%s: got %d rows, want 1", name, len(got))
		}
		e := got[0]
		if e.ID == "" || e.TS == 0 {
			t.Fatalf("%s: id/ts not filled: %+v", name, e)
		}
		e.ID, e.TS = "", 0
		if !reflect.DeepEqual(e, in) {
			t.Errorf("%s: round-trip mismatch\n got %+v\nwant %+v", name, e, in)
		}
	}
	got, err := l.Query(ctx, Filter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	check("Query", got)
	got, err = l.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	check("Recent", got)
}

func TestAuditWriteFillsRaiserFromContextWithoutOverwriting(t *testing.T) {
	l := newTestAudit(t)
	ctx := actor.WithRaiser(context.Background(), fullRaiser)
	sub, cancel := l.Subscribe()
	defer cancel()

	err := l.Write(ctx, Event{
		EventType: EventCallAllowed,
		AgentID:   "ag_1",
		Raiser:    actor.Raiser{AgentName: "explicit-name", ClientKind: "cli"},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := l.Query(ctx, Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	e := got[0]
	if e.AgentName != "explicit-name" || e.ClientKind != "cli" {
		t.Errorf("explicit fields overwritten: name=%q kind=%q", e.AgentName, e.ClientKind)
	}
	if e.OwnerUserID != fullRaiser.OwnerUserID || e.AgentSessionID != fullRaiser.AgentSessionID ||
		e.ClientSessionID != fullRaiser.ClientSessionID || !e.ClientSessionClaimed || e.Via != fullRaiser.Via {
		t.Errorf("context fields not merged: %+v", e.Raiser)
	}
	// Subscribers (the SSE feed) see the enriched event too.
	select {
	case fanned := <-sub:
		if fanned.OwnerUserID != fullRaiser.OwnerUserID || fanned.AgentName != "explicit-name" {
			t.Errorf("fan-out event lacks merged raiser: %+v", fanned.Raiser)
		}
	default:
		t.Error("no event fanned out to subscriber")
	}
}

func TestAuditWriteWithoutContextRaiserLeavesFieldsEmpty(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	if err := l.Write(ctx, Event{EventType: EventCallStart, AgentID: "ag_1"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := l.Query(ctx, Filter{})
	// CallerID mirrors agent_id on read; nothing else is invented.
	if len(got) != 1 || got[0].Raiser != (actor.Raiser{CallerID: "ag_1"}) {
		t.Fatalf("unexpected raiser on bare write: %+v", got)
	}
}

func TestAuditWriteFillsAgentIDFromCallerID(t *testing.T) {
	l := newTestAudit(t)
	ctx := actor.WithRaiser(context.Background(), actor.Raiser{CallerID: "dashboard:u_1"})
	if err := l.Write(ctx, Event{EventType: EventCallStart}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := l.Query(ctx, Filter{AgentID: "dashboard:u_1"})
	if len(got) != 1 {
		t.Fatalf("agent_id not filled from the context caller: %+v", got)
	}
}

// The redactor wipes 40+ char hex blobs (and more); identity columns are
// ids that may look exactly like that, so they must bypass it.
func TestAuditWriteNeverRedactsIDColumns(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	hexID := strings.Repeat("ab", 24) // 48 hex chars: matches the sha-style pattern
	in := Event{
		EventType:  EventApprovalDecide,
		ApprovalID: hexID,
		Reason:     "sig " + hexID,
		Raiser: actor.Raiser{
			CallerID:        hexID,
			MCPSessionID:    hexID,
			AgentSessionID:  hexID,
			ClientSessionID: hexID,
			ClientName:      "ghp_" + strings.Repeat("Z", 36),
		},
	}
	in.SetDecider(actor.Decider{UserID: hexID, Ref: hexID, Via: actor.ViaPasskey})
	if err := l.Write(ctx, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := l.Query(ctx, Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	e := got[0]
	for name, v := range map[string]string{
		"approval_id":        e.ApprovalID,
		"caller_id":          e.CallerID,
		"mcp_session_id":     e.MCPSessionID,
		"agent_session_id":   e.AgentSessionID,
		"client_session_id":  e.ClientSessionID,
		"decided_by_user_id": e.DecidedByUserID,
		"decider_ref":        e.DeciderRef,
	} {
		if v != hexID {
			t.Errorf("%s = %q, want the untouched id", name, v)
		}
	}
	if e.ClientName != in.ClientName {
		t.Errorf("client_name = %q, want untouched", e.ClientName)
	}
	if strings.Contains(e.Reason, hexID) {
		t.Errorf("reason was not redacted: %q", e.Reason)
	}
}

func TestAuditWriteCleansIdentityStrings(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	long := strings.Repeat("n", actor.MaxLen+20)
	in := Event{
		EventType: EventCallStart,
		Raiser:    actor.Raiser{AgentName: "  bad\x00name\n ", ClientName: long},
	}
	in.SetDecider(actor.Decider{Name: "\tDecider\r", Via: actor.ViaDashboard})
	if err := l.Write(ctx, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := l.Query(ctx, Filter{})
	if got[0].AgentName != "badname" {
		t.Errorf("agent_name = %q, want control chars and space stripped", got[0].AgentName)
	}
	if n := len([]rune(got[0].ClientName)); n != actor.MaxLen {
		t.Errorf("client_name length = %d, want capped at %d", n, actor.MaxLen)
	}
	if got[0].DecidedByName != "Decider" {
		t.Errorf("decided_by_name = %q, want trimmed", got[0].DecidedByName)
	}
}

func TestAuditQueryActorFilters(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	write := func(e Event) {
		t.Helper()
		if err := l.Write(ctx, e); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	a := Event{EventType: EventCallAllowed, ApprovalID: "ap_a",
		Raiser: actor.Raiser{OwnerUserID: "u_1", AgentSessionID: "ses_1", ClientKind: "t3"}}
	a.SetDecider(actor.Decider{UserID: "u_dec", Via: actor.ViaDashboard})
	write(a)
	write(Event{EventType: EventCallAllowed, ApprovalID: "ap_b",
		Raiser: actor.Raiser{OwnerUserID: "u_2", AgentSessionID: "ses_2", ClientKind: "cli"}})
	write(Event{EventType: EventCallDenied,
		Raiser: actor.Raiser{OwnerUserID: "u_1", AgentSessionID: "ses_1", ClientKind: "t3"}})

	cases := []struct {
		name string
		f    Filter
		want int
	}{
		{"owner", Filter{OwnerUserID: "u_1"}, 2},
		{"decided_by", Filter{DecidedByUserID: "u_dec"}, 1},
		{"session", Filter{AgentSessionID: "ses_2"}, 1},
		{"approval", Filter{ApprovalID: "ap_a"}, 1},
		{"client_kind", Filter{ClientKind: "t3"}, 2},
		{"combined", Filter{OwnerUserID: "u_1", EventType: EventCallDenied}, 1},
		{"no match", Filter{OwnerUserID: "u_none"}, 0},
	}
	for _, c := range cases {
		got, err := l.Query(ctx, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d rows, want %d", c.name, len(got), c.want)
		}
	}
}

package inbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

// systemQuestion is the one approve/reject question the system item tests
// ask.
func systemQuestion() []QuestionSpec {
	return []QuestionSpec{{ID: "decision", Type: QuestionSingleChoice, Prompt: "Make this version live?",
		Options: []Option{{ID: "approve", Label: "Approve"}, {ID: "reject", Label: "Reject"}}}}
}

func systemItem(ref string) SystemItem {
	return SystemItem{Kind: "test.review", Ref: ref, UserID: "u_1",
		Title: "Review skills update", Summary: "2 added, 1 changed", Message: "**owner/repo** has a new commit.",
		Questions: systemQuestion()}
}

// recordingHandler registers a handler that records every call.
type recordingHandler struct {
	mu    sync.Mutex
	calls []Request
	note  string
	err   error
}

func (h *recordingHandler) handle(_ context.Context, r *Request) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, *r)
	return h.note, h.err
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// activityText (inbox_test.go) joins an item's timeline.

func answerDecision(r *Request, choice, sub string) Decision {
	return Decision{Action: "answer", RequestRevision: r.Revision, SubmissionID: sub,
		Answers: map[string]Answer{"decision": {Value: choice}},
		Decider: actor.Decider{UserID: "u_1", Name: "Owner", Via: actor.ViaDashboard}}
}

func TestSystemItemIsOwnedByItsPersonAndChecked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r, err := e.svc.CreateSystemItem(ctx, systemItem("skp_1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.AgentID != SystemAgentID("u_1") || r.System == nil || r.System.Kind != "test.review" || r.System.Ref != "skp_1" {
		t.Fatalf("stored item: agent %q system %+v", r.AgentID, r.System)
	}
	if r.Kind != KindQuestion || r.SchemaVersion != SchemaVersionRich || !r.Checked || r.Status != StatusPending || r.Urgency != UrgencySoon {
		t.Fatalf("shape: %+v", r)
	}
	if !strings.Contains(activityText(r), "Sent by toolyard") {
		t.Fatalf("activity %q", activityText(r))
	}
	if got := e.svc.agentName(ctx, r.AgentID); got != SystemSenderName {
		t.Fatalf("sender name %q", got)
	}
	if uid, ok := SystemAgentUser(r.AgentID); !ok || uid != "u_1" {
		t.Fatalf("SystemAgentUser = %q %v", uid, ok)
	}
	if _, ok := SystemAgentUser("ag_1"); ok {
		t.Fatal("an agent ID is not a system sender")
	}
	// The owner's list (bounded by their senders) has it; another person's
	// does not.
	mine, err := e.svc.List(ctx, ListFilter{Open: true, AgentIDs: []string{SystemAgentID("u_1")}})
	if err != nil || len(mine) != 1 || mine[0].ID != r.ID {
		t.Fatalf("owner list: %d %v", len(mine), err)
	}
	theirs, err := e.svc.List(ctx, ListFilter{Open: true, AgentIDs: []string{SystemAgentID("u_2"), "ag_1"}})
	if err != nil || len(theirs) != 0 {
		t.Fatalf("other list: %d %v", len(theirs), err)
	}
	stored, err := e.svc.Get(ctx, r.ID)
	if err != nil || stored.System == nil || stored.System.Ref != "skp_1" {
		t.Fatalf("Get: %+v %v", stored, err)
	}
}

func TestSystemItemAnswerRunsTheHandlerOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &recordingHandler{note: "Version abc123 is live."}
	e.svc.HandleSystem("test.review", h.handle)
	r, err := e.svc.CreateSystemItem(ctx, systemItem("skp_2"))
	if err != nil {
		t.Fatal(err)
	}
	d := answerDecision(r, "approve", "sub-1")
	got, err := e.svc.Decide(ctx, r.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	if h.count() != 1 {
		t.Fatalf("handler calls %d", h.count())
	}
	call := h.calls[0]
	if call.Status != StatusAnswered || call.SystemChoice("decision") != "approve" || call.DeciderUserID != "u_1" || call.System.Ref != "skp_2" {
		t.Fatalf("handler saw %+v", call)
	}
	if !strings.Contains(activityText(got), "Version abc123 is live.") {
		t.Fatalf("returned activity %q", activityText(got))
	}
	stored, _ := e.svc.Get(ctx, r.ID)
	if !strings.Contains(activityText(stored), "Version abc123 is live.") {
		t.Fatalf("stored activity %q", activityText(stored))
	}
	// A retry of the same submission replays the stored decision and
	// never runs the handler again.
	if _, err := e.svc.Decide(ctx, r.ID, d); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if h.count() != 1 {
		t.Fatalf("handler ran again on a replay: %d", h.count())
	}
	// A second, different decision is refused before any handler runs.
	if _, err := e.svc.Decide(ctx, r.ID, answerDecision(stored, "reject", "sub-2")); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second decision: %v", err)
	}
	if h.count() != 1 {
		t.Fatalf("handler calls after a refused decision: %d", h.count())
	}
}

func TestSystemItemDenyAndSnooze(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &recordingHandler{note: "Rejected."}
	e.svc.HandleSystem("test.review", h.handle)
	r, err := e.svc.CreateSystemItem(ctx, systemItem("skp_3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(ctx, r.ID, Decision{Action: "snooze", SnoozeMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	if h.count() != 0 {
		t.Fatal("a snooze is no decision")
	}
	if _, err := e.svc.Decide(ctx, r.ID, Decision{Action: "deny", Note: "not now", Decider: actor.Decider{UserID: "u_1"}}); err != nil {
		t.Fatal(err)
	}
	if h.count() != 1 || h.calls[0].Status != StatusDenied || h.calls[0].SystemChoice("decision") != "" || h.calls[0].OwnerNote != "not now" {
		t.Fatalf("deny: %d %+v", h.count(), h.calls)
	}
}

func TestSystemItemHandlerErrorsAndPanicsAreRecorded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.HandleSystem("test.fail", func(context.Context, *Request) (string, error) {
		return "", errors.New("the version is no longer staged")
	})
	e.svc.HandleSystem("test.panic", func(context.Context, *Request) (string, error) { panic("boom") })
	for kind, want := range map[string]string{
		"test.fail":  "toolyard could not apply this decision: the version is no longer staged",
		"test.panic": "toolyard could not apply this decision: handler panicked: boom",
	} {
		it := systemItem("ref-" + kind)
		it.Kind = kind
		r, err := e.svc.CreateSystemItem(ctx, it)
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.svc.Decide(ctx, r.ID, answerDecision(r, "approve", "s-"+kind))
		if err != nil {
			t.Fatalf("%s: the decision itself stands: %v", kind, err)
		}
		if got.Status != StatusAnswered || !strings.Contains(activityText(got), want) {
			t.Fatalf("%s: status %s activity %q", kind, got.Status, activityText(got))
		}
	}
	// No handler registered: the decision is recorded and nothing else.
	it := systemItem("ref-none")
	it.Kind = "test.unregistered"
	r, err := e.svc.CreateSystemItem(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Decide(ctx, r.ID, answerDecision(r, "approve", "s-none")); err != nil || got.Status != StatusAnswered {
		t.Fatalf("unregistered: %+v %v", got, err)
	}
}

func TestSystemItemValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := map[string]func(*SystemItem){
		"no kind":        func(it *SystemItem) { it.Kind = "" },
		"no ref":         func(it *SystemItem) { it.Ref = " " },
		"user and agent": func(it *SystemItem) { it.AgentID = "ag_1" },
		"neither":        func(it *SystemItem) { it.UserID = "" },
		"system sender":  func(it *SystemItem) { it.UserID = ""; it.AgentID = SystemAgentID("u_9") },
		"no question":    func(it *SystemItem) { it.Questions = nil },
		"long title":     func(it *SystemItem) { it.Title = strings.Repeat("x", MaxTitle+1) },
		"one option": func(it *SystemItem) {
			it.Questions = []QuestionSpec{{ID: "decision", Type: QuestionSingleChoice, Prompt: "Go?", Options: []Option{{ID: "approve", Label: "Approve"}}}}
		},
	}
	for name, mutate := range cases {
		it := systemItem("skp_v")
		mutate(&it)
		if _, err := e.svc.CreateSystemItem(ctx, it); !errors.Is(err, ErrSystemItem) {
			t.Errorf("%s: %v, want ErrSystemItem", name, err)
		}
	}
}

func TestSystemItemAboutAnAgentCountsTowardItsLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	it := systemItem("skp_a")
	it.UserID, it.AgentID, it.Activity = "", "ag_1", "Proposed by the agent"
	r, err := e.svc.CreateSystemItem(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if r.AgentID != "ag_1" || !strings.Contains(activityText(r), "Proposed by the agent") {
		t.Fatalf("agent item: %q %q", r.AgentID, activityText(r))
	}
	// The agent sees its own item.
	views, err := e.svc.Status(ctx, "ag_1", []string{r.ID})
	if err != nil || len(views) != 1 {
		t.Fatalf("agent status: %v %v", views, err)
	}
	for i := 1; i < MaxPendingPerAgent; i++ {
		it.Ref = "skp_a" + strings.Repeat("x", i)
		if _, err := e.svc.CreateSystemItem(ctx, it); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	it.Ref = "skp_over"
	if _, err := e.svc.CreateSystemItem(ctx, it); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("over the limit: %v", err)
	}
}

// TestAgentsCannotTagSystemItems: "system" in an agent's submission is
// ignored, so no agent can make Decide run a handler.
func TestAgentsCannotTagSystemItems(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &recordingHandler{}
	e.svc.HandleSystem("skillhub.review", h.handle)
	sub, err := DecodeSubmission(map[string]any{
		"kind": "question", "schema_version": 3, "title": "Pick a region", "summary": "Where to deploy",
		"message": "I can deploy to either region.", "urgency": "soon",
		"questions": []any{map[string]any{"id": "decision", "type": "single_choice", "prompt": "Which region?",
			"options": []any{map[string]any{"id": "approve", "label": "EU"}, map[string]any{"id": "reject", "label": "US"}}}},
		"system": map[string]any{"kind": "skillhub.review", "ref": "skp_forged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Submit(ctx, "ag_1", sub)
	if err != nil || !res.OK {
		t.Fatalf("submit: %v %+v", err, res)
	}
	e.svc.Flush()
	r, err := e.svc.Get(ctx, res.RequestID)
	if err != nil || r.System != nil {
		t.Fatalf("stored system tag %+v %v", r.System, err)
	}
	if _, err := e.svc.Decide(ctx, r.ID, answerDecision(r, "approve", "s-forged")); err != nil {
		t.Fatal(err)
	}
	if h.count() != 0 {
		t.Fatal("an agent's item ran a system handler")
	}
}

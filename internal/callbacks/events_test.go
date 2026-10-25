package callbacks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// everything is a receiver that opted into every event and the v3 shape.
var everything = Subscription{Events: []string{"*"}, V3: true}

// newItem stores a pending Inbox item and lets refs follow it ("" asks for
// the agent's default receiver).
func newItem(t *testing.T, s *Service, db *store.DB, id, agent string, refs ...string) *inbox.Request {
	t.Helper()
	ctx := t.Context()
	r := &inbox.Request{ID: id, AgentID: agent, Revision: 1, Status: inbox.StatusPending, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES(?,?,'access','pending','soon','{}',0,0,?)`, r.ID, r.AgentID, r.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err = s.RegisterTx(ctx, tx, r, ref); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return r
}

// pullEvents reads the pending events of a pull receiver and checks each
// one's Standard Webhooks signature.
func pullEvents(t *testing.T, s *Service, agent, ref string) []PullEvent {
	t.Helper()
	events, err := s.Pull(t.Context(), agent, ref)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secretBytes(testSecret())
	for _, e := range events {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(e.EventID + "." + e.Headers["webhook-timestamp"] + "." + e.Body))
		if e.Headers["webhook-signature"] != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) || e.Headers["webhook-id"] != e.EventID {
			t.Fatalf("bad signature on %s", e.EventID)
		}
	}
	return events
}

func envelopeOf(t *testing.T, body string) (Envelope, map[string]any) {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	data, _ := env.Data.(map[string]any)
	return env, data
}

func countOutbox(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM callback_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Two receivers follow one item: both get the inbox.decision event, once,
// after the full decision, with the verdicts and the answers.
func TestTwoReceiversFollowOneItem(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	a, err := s.RegisterPullWith(ctx, "agent", "mac", "first", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RegisterPullWith(ctx, "agent", "mac", "second", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	req := newItem(t, s, db, "rq_fan", "agent", a.ID, b.ID, a.ID)
	if n := countOutbox(t, db); n != 0 {
		t.Fatalf("events before the decision: %d", n)
	}
	req.Revision, req.Status = 2, inbox.StatusApproved
	req.Tools = []inbox.ToolRequest{{CallID: "deploy", Decision: inbox.ToolAllowed}, {CallID: "flag", Decision: inbox.ToolRefused, Reason: "Not yet"}}
	req.Answers = map[string]inbox.Answer{"env": {Value: "stage", Labels: []string{"Stage"}}, "notify": {Value: false, Labels: []string{"No"}}}
	tx, _ := db.BeginTx(ctx, nil)
	if err = s.DecisionTx(ctx, tx, req); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, ref := range []string{a.ID, b.ID} {
		events := pullEvents(t, s, "agent", ref)
		if len(events) != 1 {
			t.Fatalf("%s: %d events", ref, len(events))
		}
		var ev Event
		if err := json.Unmarshal([]byte(events[0].Body), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != EventInboxDecision || ev.EventID != events[0].EventID || len(ev.Data.Calls) != 2 || ev.Data.Calls[1].Reason != "Not yet" {
			t.Fatalf("%s: event %+v", ref, ev)
		}
		if ev.Data.Answers["env"].Value != "stage" || ev.Data.Answers["notify"].Value != false {
			t.Fatalf("%s: answers %+v", ref, ev.Data.Answers)
		}
		ids[ev.EventID] = true
	}
	if len(ids) != 2 {
		t.Fatal("each receiver needs its own event id")
	}
	tx, _ = db.BeginTx(ctx, nil)
	if err = s.DecisionTx(ctx, tx, req); err == nil {
		t.Fatal("a repeated logical event was queued again")
	}
	tx.Rollback()
	if n := countOutbox(t, db); n != 2 {
		t.Fatalf("outbox rows: %d", n)
	}
	history, err := s.History(ctx, "agent", req.ID)
	if err != nil || len(history) != 2 || history[0].Type != EventInboxDecision {
		t.Fatalf("history: %+v %v", history, err)
	}
}

func TestDefaultReceiverFollowsItemsWithoutRef(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "default-hook", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.RegisterPull(ctx, "other", "mac", "theirs", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDefault(ctx, "agent", theirs.ID); !errors.Is(err, ErrReceiver) {
		t.Fatalf("another agent's receiver became the default: %v", err)
	}
	newItem(t, s, db, "rq_none", "agent", "")
	if err = s.SetDefault(ctx, "agent", r.ID); err != nil {
		t.Fatal(err)
	}
	if def, _ := s.DefaultRef(ctx, "agent"); def != r.ID {
		t.Fatalf("default %q", def)
	}
	list, err := s.List(ctx, "agent", false)
	if err != nil || len(list) != 1 || !list[0].Default {
		t.Fatalf("list: %+v %v", list, err)
	}
	newItem(t, s, db, "rq_default", "agent", "")
	var n int
	db.QueryRow(`SELECT count(*) FROM callback_subscriptions WHERE request_id='rq_default' AND receiver_id=?`, r.ID).Scan(&n)
	if n != 1 {
		t.Fatal("the default receiver doesn't follow an item without callback_ref")
	}
	db.QueryRow(`SELECT count(*) FROM callback_subscriptions WHERE request_id='rq_none'`).Scan(&n)
	if n != 0 {
		t.Fatal("an item from before the default got a receiver")
	}
	if _, err = s.Change(ctx, "agent", r.ID, "remove", "", r.Revision); err != nil {
		t.Fatal(err)
	}
	if def, _ := s.DefaultRef(ctx, "agent"); def != "" {
		t.Fatalf("a removed receiver stayed the default: %q", def)
	}
	newItem(t, s, db, "rq_after", "agent", "")
	db.QueryRow(`SELECT count(*) FROM callback_subscriptions WHERE request_id='rq_after'`).Scan(&n)
	if n != 0 {
		t.Fatal("a removed default still follows new items")
	}
}

// EmitForAgentTx is the generic emitter: one signed event in the caller's
// transaction, for one of that agent's receivers (or its default), filed
// under the subject its data names.
func TestEmitForAgentTxQueuesSignedEvents(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "jobs", testSecret(), Subscription{Events: []string{"job.*"}})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.RegisterPullWith(ctx, "other", "mac", "their-jobs", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"job_id": "job_1", "status": "succeeded"}
	emit := func(agent, ref, eventType string) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.EmitForAgentTx(ctx, tx, agent, ref, eventType, data); err != nil {
			return err
		}
		return tx.Commit()
	}
	// No default receiver yet: an event for the default goes nowhere.
	if err = emit("agent", "", EventJobCompleted); err != nil || countOutbox(t, db) != 0 {
		t.Fatalf("no default receiver: %v, %d rows", err, countOutbox(t, db))
	}
	tx, _ := db.BeginTx(ctx, nil)
	if err = s.EmitForAgentTx(ctx, tx, "agent", r.ID, EventJobCompleted, data); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := countOutbox(t, db); n != 0 {
		t.Fatal("an event escaped a rolled-back transaction")
	}
	if err = emit("agent", r.ID, EventJobCompleted); err != nil {
		t.Fatal(err)
	}
	events := pullEvents(t, s, "agent", r.ID)
	if len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}
	env, got := envelopeOf(t, events[0].Body)
	if env.Type != EventJobCompleted || env.EventID != events[0].EventID || got["job_id"] != "job_1" {
		t.Fatalf("envelope %+v", env)
	}
	var subject, kind string
	db.QueryRow(`SELECT request_id,event_type FROM callback_outbox`).Scan(&subject, &kind)
	if subject != "job_1" || kind != EventJobCompleted {
		t.Fatalf("outbox row: %q %q", subject, kind)
	}
	if err = emit("agent", theirs.ID, EventJobCompleted); !errors.Is(err, ErrReceiver) {
		t.Fatalf("another agent's receiver: %v", err)
	}
	if err = emit("agent", "cb_unknown", EventJobFailed); !errors.Is(err, ErrReceiver) {
		t.Fatalf("unknown receiver: %v", err)
	}
	if err = emit("agent", r.ID, "Job Completed"); err == nil {
		t.Fatal("a malformed event type was accepted")
	}
	// The receiver opted into job.* only: a ping-shaped event is dropped.
	if err = emit("agent", r.ID, EventExecutionCompleted); err != nil || countOutbox(t, db) != 1 {
		t.Fatalf("an event the receiver did not opt into was queued: %v", err)
	}
	// The default receiver is read inside the caller's transaction.
	if err = s.SetDefault(ctx, "agent", r.ID); err != nil {
		t.Fatal(err)
	}
	if err = emit("agent", "", EventJobFailed); err != nil || countOutbox(t, db) != 2 {
		t.Fatalf("default receiver: %v, %d rows", err, countOutbox(t, db))
	}
	if _, err = s.Change(ctx, "agent", r.ID, "disable", "", r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = emit("agent", r.ID, EventJobFailed); !errors.Is(err, ErrReceiver) {
		t.Fatalf("a disabled receiver: %v", err)
	}
}

// A legacy receiver (registered without events, as every receiver before
// v0.1) gets only the three decision events, in the version 2 shape.
func TestLegacyReceiverGetsOnlyWhatItAccepts(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	legacy, err := s.RegisterPull(ctx, "agent", "mac", "t3-session", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(legacy.Events, ",") != "inbox.cancelled,inbox.decision,inbox.expired" || legacy.V3 {
		t.Fatalf("legacy receiver: %+v", legacy)
	}
	if _, err = s.Test(ctx, "agent", legacy.ID); !errors.Is(err, ErrEventNotAccepted) {
		t.Fatalf("a legacy receiver was pinged: %v", err)
	}
	req := newItem(t, s, db, "rq_legacy", "agent", legacy.ID)
	tx, _ := db.BeginTx(ctx, nil)
	if err = s.ExecutionTx(ctx, tx, &inbox.Execution{GrantID: "gr_l", RequestID: req.ID, CallID: "c", State: inbox.ExecutionSucceeded}, "agent", "deploy.run"); err != nil {
		t.Fatal(err)
	}
	req.Revision, req.Status = 2, inbox.StatusApproved
	req.Tools = []inbox.ToolRequest{{CallID: "c", Decision: inbox.ToolAllowed}}
	req.Answers = map[string]inbox.Answer{"env": {Value: "stage"}}
	if err = s.DecisionTx(ctx, tx, req); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events := pullEvents(t, s, "agent", legacy.ID)
	if len(events) != 1 {
		t.Fatalf("%d events for a legacy receiver", len(events))
	}
	_, legacyData := envelopeOf(t, events[0].Body)
	if _, ok := legacyData["answers"]; ok {
		t.Fatalf("a legacy receiver got answers: %s", events[0].Body)
	}
	// More than 12 calls only fit the v3 shape: refused when named,
	// skipped for a default receiver.
	big := &inbox.Request{ID: "rq_big", AgentID: "agent"}
	for i := 0; i < 13; i++ {
		big.Tools = append(big.Tools, inbox.ToolRequest{CallID: fmt.Sprintf("c%d", i)})
	}
	if err = s.CheckSubscription(ctx, "agent", legacy.ID, big); !errors.Is(err, ErrNeedsV3) {
		t.Fatalf("CheckSubscription: %v", err)
	}
	tx, _ = db.BeginTx(ctx, nil)
	defer tx.Rollback()
	if err = s.RegisterTx(ctx, tx, big, legacy.ID); !errors.Is(err, ErrNeedsV3) {
		t.Fatalf("RegisterTx: %v", err)
	}
}

func TestNormalizeEvents(t *testing.T) {
	got, err := NormalizeEvents([]string{"ping", "job.*", "ping", "inbox.decision"})
	if err != nil || strings.Join(got, ",") != "inbox.decision,job.*,ping" {
		t.Fatalf("normalize: %v %v", got, err)
	}
	for _, bad := range []string{"inbox.decisions", "Ping", "job*", "*.*", ""} {
		if _, err := NormalizeEvents([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !acceptsEvent([]string{"job.*"}, "job.failed") || acceptsEvent([]string{"job.*"}, "jobs.failed") || !acceptsEvent([]string{"*"}, "ping") {
		t.Fatal("acceptsEvent")
	}
}

// mustPing queues a ping and returns its id.
func mustPing(t *testing.T, s *Service, ref string) string {
	t.Helper()
	id, err := s.Test(t.Context(), "agent", ref)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPingAndCheckRef(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "pinged", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Test(ctx, "other", r.ID); !errors.Is(err, ErrReceiver) {
		t.Fatalf("another agent pinged the receiver: %v", err)
	}
	id, err := s.Test(ctx, "agent", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.Test(ctx, "agent", r.ID); err != nil || again != id {
		t.Fatalf("a second test must return the pending ping: %q %v", again, err)
	}
	events := pullEvents(t, s, "agent", r.ID)
	if len(events) != 1 || events[0].EventID != id {
		t.Fatalf("ping: %+v", events)
	}
	env, data := envelopeOf(t, events[0].Body)
	if env.Type != EventPing || data["callback_ref"] != r.ID {
		t.Fatalf("ping envelope: %+v", env)
	}
	if err = s.CheckRef(ctx, "agent", r.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckRef(ctx, "other", r.ID); !errors.Is(err, ErrReceiver) {
		t.Fatalf("check ref across agents: %v", err)
	}
	// The quota counts every ping of the agent's receivers in the last minute.
	for i := 0; i < PingsPerMinute; i++ {
		if err := s.Ack(ctx, "agent", r.ID, mustPing(t, s, r.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Test(ctx, "agent", r.ID); !errors.Is(err, ErrPingQuota) {
		t.Fatalf("ping quota: %v", err)
	}
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = secretBytes(secret); err != nil {
		t.Fatalf("generated secret %v", err)
	}
}

// A granted call's outcome reaches every receiver of its item once.
func TestExecutionCompletedFollowsTheItem(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "runs", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	req := newItem(t, s, db, "rq_exec", "agent", r.ID)
	e := &inbox.Execution{GrantID: "gr_1", RequestID: req.ID, CallID: "deploy", State: inbox.ExecutionUnknown, StartedAt: 10, FinishedAt: 20, Error: "upstream transport failed after dispatch"}
	for range 2 {
		tx, _ := db.BeginTx(ctx, nil)
		if err = s.ExecutionTx(ctx, tx, e, "agent", "deploy.run"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	events := pullEvents(t, s, "agent", r.ID)
	if len(events) != 1 {
		t.Fatalf("%d execution events", len(events))
	}
	env, data := envelopeOf(t, events[0].Body)
	if env.Type != EventExecutionCompleted || data["state"] != inbox.ExecutionUnknown || data["tool"] != "deploy.run" || data["call_id"] != "deploy" || data["inbox_id"] != req.ID {
		t.Fatalf("execution event: %s", events[0].Body)
	}
}

// Legacy deferred approvals: the receiver named at the call (or the
// default) hears approval.decided and approval.executed, whether the bus
// or the Inbox ledger decides.
func TestApprovalEventsFollowTheApproval(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "approvals", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	bus.SetEventSink(s)
	held, err := bus.Hold(ctx, approval.NewRequest{AgentID: "agent", UpstreamName: "github", ToolName: "github.merge_pull_request",
		Arguments: map[string]any{"number": 7}, Reason: "Merge the approved fix for the checkout bug", CallbackRef: r.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bus.Decide(ctx, held.ID, approval.StatusAllowed, "user_owner"); err != nil {
		t.Fatal(err)
	}
	if err = bus.SetResult(ctx, held.ID, `{"content":[{"type":"text","text":"merged"}]}`, false, ""); err != nil {
		t.Fatal(err)
	}
	events := pullEvents(t, s, "agent", r.ID)
	if len(events) != 2 {
		t.Fatalf("%d approval events", len(events))
	}
	types := map[string]map[string]any{}
	for _, e := range events {
		env, data := envelopeOf(t, e.Body)
		types[env.Type] = data
	}
	if d := types[EventApprovalDecided]; d == nil || d["approval_id"] != held.ID || d["status"] != approval.StatusAllowed || d["tool"] != "github.merge_pull_request" {
		t.Fatalf("approval.decided: %+v", types)
	}
	if d := types[EventApprovalExecuted]; d == nil || d["execution_state"] != "succeeded" {
		t.Fatalf("approval.executed: %+v", types)
	}
	for _, e := range events {
		if err := s.Ack(ctx, "agent", r.ID, e.EventID); err != nil {
			t.Fatal(err)
		}
	}

	// The Inbox ledger decides a legacy item (same ID as its approval):
	// its followers hear approval.decided, not inbox.decision.
	if err = s.SetDefault(ctx, "agent", r.ID); err != nil {
		t.Fatal(err)
	}
	legacy := &approval.Request{ID: "ap_legacy", AgentID: "agent", ToolName: "github.create_review", Status: approval.StatusPending}
	if err = s.SubscribeApproval(ctx, legacy, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SubscribeApproval(ctx, legacy, "cb_not_mine"); !errors.Is(err, ErrReceiver) {
		t.Fatalf("an unknown receiver followed an approval: %v", err)
	}
	item := &inbox.Request{ID: "ap_legacy", AgentID: "agent", ExecutionMode: "legacy", Revision: 2, Status: inbox.StatusDenied, OwnerNote: "Not this PR",
		Tools: []inbox.ToolRequest{{CallID: "call_1", Tool: "github.create_review", Upstream: "github", Decision: inbox.ToolRefused}}}
	tx, _ := db.BeginTx(ctx, nil)
	if err = s.DecisionTx(ctx, tx, item); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events = pullEvents(t, s, "agent", r.ID)
	if len(events) != 1 {
		t.Fatalf("%d legacy decision events", len(events))
	}
	env, data := envelopeOf(t, events[0].Body)
	if env.Type != EventApprovalDecided || data["status"] != inbox.StatusDenied || data["owner_note"] != "Not this PR" || data["approval_id"] != "ap_legacy" {
		t.Fatalf("legacy decision: %s", events[0].Body)
	}
}

// Rotation rebinds the receivers that follow approvals, like those that
// follow Inbox items.
func TestRotationRebindsSubjectSubscriptions(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "rotating", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubscribeApproval(ctx, &approval.Request{ID: "ap_rot", AgentID: "agent"}, r.ID); err != nil {
		t.Fatal(err)
	}
	next := "whsec_" + base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxyz012345"))
	if _, err = s.Change(ctx, "agent", r.ID, "rotate", next, r.Revision); err != nil {
		t.Fatal(err)
	}
	var rev int
	db.QueryRow(`SELECT receiver_revision FROM callback_subject_subscriptions WHERE subject_id='ap_rot'`).Scan(&rev)
	if rev != r.Revision+1 {
		t.Fatalf("subject subscription revision %d", rev)
	}
}

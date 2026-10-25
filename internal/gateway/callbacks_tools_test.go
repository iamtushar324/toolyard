package gateway

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

type callbackFixture struct {
	*inboxFixture
	db *store.DB
	cb *callbacks.Service
}

// newCallbackFixture is the inbox fixture with the callbacks service wired
// as in cmd/gateway: inbox subscriptions and the callbacks.* tools.
func newCallbackFixture(t *testing.T) *callbackFixture {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	f := &inboxFixture{mode: ApprovalModeExecute}
	f.pol = policy.New(db)
	f.aud = audit.New(db)
	f.gw = New(Options{Policy: f.pol, Approval: bus, Audit: f.aud, Hub: realtime.NewHub(), Memory: memory.New(db)})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.svc, err = inbox.New(context.Background(), inbox.Options{DB: db, Catalog: f.gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.svc.Flush)
	cipher, err := sealbox.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cb := callbacks.New(db, cipher)
	cb.Resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	f.svc.SetCallbacks(cb)
	bus.SetEventSink(cb)
	f.gw.SetInbox(f.svc, inbox.NewGuide(nil), func() string { return f.mode })
	f.gw.RegisterInboxTools()
	f.gw.RegisterCallbackTools(cb, nil)
	f.gw.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: "deploy.run", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "deploy", originalName: "run", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			f.calls.Add(1)
			return mcp.NewToolResultText("deployed"), nil
		},
	})
	f.agent = WithAgentID(context.Background(), "ag_1")
	f.anonym = context.Background()
	return &callbackFixture{inboxFixture: f, db: db, cb: cb}
}

func TestCallbackToolsOverMCP(t *testing.T) {
	f := newCallbackFixture(t)
	ctx := context.Background()
	for _, n := range CallbackToolNames {
		if !IsPinned(n) || !f.gw.HasTool(n) {
			t.Errorf("%s should be registered and pinned", n)
		}
	}
	if res := f.call(t, f.anonym, "callbacks.list", nil); !res.IsError || !strings.Contains(textOf(res), "enrolled agent") {
		t.Fatalf("anonymous callbacks.list: %s", textOf(res))
	}

	// Register with a generated secret: it comes back once.
	everything := map[string]any{"destination": "https://receiver.example/hooks/one", "events": []any{"*"}, "v3": true}
	first := structured(t, f.call(t, f.agent, "callbacks.register", everything))
	ref, _ := first["callback_ref"].(string)
	secret, _ := first["secret"].(string)
	if !strings.HasPrefix(ref, "cb_") || !strings.HasPrefix(secret, "whsec_") || first["status"] != "active" || first["v3"] != true {
		t.Fatalf("register: %v", first)
	}
	again := structured(t, f.call(t, f.agent, "callbacks.register", everything))
	if again["callback_ref"] != ref || again["secret"] != nil || again["secret_returned"] != false {
		t.Fatalf("a repeated registration must return the receiver without its secret: %v", again)
	}
	if res := f.call(t, f.agent, "callbacks.register", map[string]any{"destination": "https://receiver.example/hooks/one"}); !res.IsError {
		t.Fatal("a registration with other events under the same client_receiver_id was accepted")
	}
	if res := f.call(t, f.agent, "callbacks.register", map[string]any{"destination": "https://receiver.example/hooks/bad", "events": []any{"inbox.decisions"}}); !res.IsError {
		t.Fatal("an unknown event type was accepted")
	}
	// Without events, a receiver gets the three legacy decision events only.
	legacy := structured(t, f.call(t, f.agent, "callbacks.register", map[string]any{"destination": "https://receiver.example/hooks/legacy"}))
	if evs, _ := legacy["events"].([]any); len(evs) != 3 || legacy["v3"] != false {
		t.Fatalf("legacy receiver: %v", legacy)
	}
	if res := f.call(t, f.agent, "callbacks.test", map[string]any{"callback_ref": legacy["callback_ref"]}); !res.IsError {
		t.Fatal("a receiver that did not opt into ping was pinged")
	}
	if res := f.call(t, f.agent, "callbacks.register", map[string]any{"destination": "http://receiver.example/plain"}); !res.IsError {
		t.Fatal("an http destination was accepted")
	}
	// The audit record never keeps the secret.
	var leaked int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE result_summary LIKE ? OR arguments LIKE ?`, "%"+secret+"%", "%"+secret+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the generated secret reached the audit log (%d rows, %v)", leaked, err)
	}

	// A second receiver, with the agent's own secret, made the default.
	own := "whsec_" + strings.Repeat("QUJD", 8)
	second := structured(t, f.call(t, f.agent, "callbacks.register", map[string]any{"destination": "https://receiver.example/hooks/two", "secret": own, "make_default": true,
		"events": []any{"inbox.*"}, "v3": true}))
	ref2, _ := second["callback_ref"].(string)
	if ref2 == "" || second["secret"] != nil || second["default"] != true {
		t.Fatalf("second register: %v", second)
	}
	list := structured(t, f.call(t, f.agent, "callbacks.list", nil))
	if recv, _ := list["receivers"].([]any); len(recv) != 3 || list["default"] != ref2 {
		t.Fatalf("list: %v", list)
	}
	if s := structured(t, f.call(t, f.agent, "callbacks.set_default", map[string]any{"callback_ref": ref})); s["default"] != ref {
		t.Fatalf("set_default: %v", s)
	}
	if res := f.call(t, WithAgentID(ctx, "ag_2"), "callbacks.set_default", map[string]any{"callback_ref": ref}); !res.IsError {
		t.Fatal("another agent made this receiver its default")
	}

	// A ping is queued and shows in the receiver's deliveries.
	ping := structured(t, f.call(t, f.agent, "callbacks.test", map[string]any{"callback_ref": ref}))
	pingID, _ := ping["event_id"].(string)
	if !strings.HasPrefix(pingID, "evt_") {
		t.Fatalf("test: %v", ping)
	}
	withHistory := structured(t, f.call(t, f.agent, "callbacks.list", map[string]any{"callback_ref": ref}))
	deliveries, _ := withHistory["deliveries"].([]any)
	if len(deliveries) != 1 || deliveries[0].(map[string]any)["event_id"] != pingID || deliveries[0].(map[string]any)["type"] != callbacks.EventPing {
		t.Fatalf("deliveries: %v", withHistory)
	}

	// An Inbox item without callback_ref reports to the default receiver;
	// one with callback_refs reports to both.
	ask := func(extra map[string]any) string {
		args := map[string]any{"schema_version": 3, "questions": []any{
			map[string]any{"id": "go", "type": "yes_no", "prompt": "Shall I start the migration tonight?"},
		}}
		for k, v := range extra {
			args[k] = v
		}
		s := structured(t, f.call(t, f.agent, "inbox.ask", args))
		id, _ := s["request_id"].(string)
		if s["ok"] != true || id == "" {
			t.Fatalf("inbox.ask: %v", s)
		}
		return id
	}
	followers := func(id string) int {
		var n int
		if err := f.db.QueryRow(`SELECT count(*) FROM callback_subscriptions WHERE request_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	byDefault := ask(nil)
	both := ask(map[string]any{"callback_ref": ref, "callback_refs": []any{ref2}})
	if followers(byDefault) != 1 || followers(both) != 2 {
		t.Fatalf("followers: default %d, both %d", followers(byDefault), followers(both))
	}
	f.svc.Flush()
	if _, err := f.svc.Decide(ctx, both, inbox.Decision{Action: "answer", RequestRevision: 1, SubmissionID: "answer_both",
		Answers: map[string]inbox.Answer{"go": {Value: true}}}); err != nil {
		t.Fatal(err)
	}
	var decisions int
	if err := f.db.QueryRow(`SELECT count(*) FROM callback_outbox WHERE request_id=? AND event_type=?`, both, callbacks.EventInboxDecision).Scan(&decisions); err != nil || decisions != 2 {
		t.Fatalf("inbox.decision events for two receivers: %d %v", decisions, err)
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM callback_outbox WHERE request_id=? AND receiver_id=?`, both, ref2).Scan(&payload); err != nil || !strings.Contains(payload, `"answers":{"go":{"value":true`) {
		t.Fatalf("decision payload: %s %v", payload, err)
	}

	// Rotate with a generated secret, then disable.
	rotated := structured(t, f.call(t, f.agent, "callbacks.update", map[string]any{"callback_ref": ref, "action": "rotate", "expected_revision": 1}))
	if s, _ := rotated["secret"].(string); !strings.HasPrefix(s, "whsec_") || s == secret || rotated["revision"] != float64(2) {
		t.Fatalf("rotate: %v", rotated)
	}
	if res := f.call(t, f.agent, "callbacks.update", map[string]any{"callback_ref": ref, "action": "disable", "expected_revision": 1}); !res.IsError {
		t.Fatal("a stale revision was accepted")
	}
	disabled := structured(t, f.call(t, f.agent, "callbacks.update", map[string]any{"callback_ref": ref, "action": "disable", "expected_revision": 2}))
	if disabled["status"] != "disabled" {
		t.Fatalf("disable: %v", disabled)
	}
	if res := f.call(t, f.agent, "callbacks.test", map[string]any{"callback_ref": ref}); !res.IsError {
		t.Fatal("a disabled receiver was pinged")
	}
}

// _callback_ref is stripped before a tool sees it, and a legacy approval
// the call creates is followed by that receiver.
func TestCallbackRefArgumentFollowsLegacyApproval(t *testing.T) {
	_, _, clean, err := extractReason(map[string]any{ReasonField: testReason, callbackRefArg: "cb_x", "a": "b"}, ReasonField)
	if err != nil || clean[callbackRefArg] != nil || clean["a"] != "b" {
		t.Fatalf("_callback_ref must be stripped before dispatch: %v %v", clean, err)
	}
	ctx := withCallbackRef(context.Background(), map[string]any{callbackRefArg: " cb_x "})
	if callbackRefFrom(ctx) != "cb_x" || callbackRefFrom(context.Background()) != "" {
		t.Fatal("callback ref context")
	}
	if nested := withCallbackRef(ctx, map[string]any{"a": "b"}); callbackRefFrom(nested) != "" {
		t.Fatal("a nested call inherited the outer call's _callback_ref")
	}
}

// _callback_ref is advertised on every wrapped tool; a tool that declares
// its own keeps it: toolyard neither reads nor strips it.
func TestOwnCallbackRefParameterIsTheTools(t *testing.T) {
	plain, _ := wrapSchema(mcp.Tool{Name: "x", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"a": map[string]any{"type": "string"}}}})
	if _, ok := plain.InputSchema.Properties[callbackRefArg]; !ok {
		t.Fatal("_callback_ref missing from the wrapped schema")
	}
	own, _ := wrapSchema(mcp.Tool{Name: "y", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{callbackRefArg: map[string]any{"type": "string", "description": "the tool's own reference"}}}})
	ours, theirs := toolEntry{tool: plain}, toolEntry{tool: own}
	if ownCallbackRef(ours) || !ownCallbackRef(theirs) {
		t.Fatal("ownCallbackRef")
	}
	args := map[string]any{ReasonField: testReason, callbackRefArg: "cb_x"}
	_, _, clean, err := extractReason(args, ReasonField)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoreOwnCallbackRef(theirs, args, clean); got[callbackRefArg] != "cb_x" {
		t.Fatalf("the tool's own _callback_ref was stripped: %v", got)
	}
	if got := restoreOwnCallbackRef(ours, args, map[string]any{}); got[callbackRefArg] != nil {
		t.Fatalf("toolyard's _callback_ref reached the tool: %v", got)
	}
	if callbackRefFrom(withCallbackRefFor(context.Background(), theirs, args)) != "" || callbackRefFrom(withCallbackRefFor(context.Background(), ours, args)) != "cb_x" {
		t.Fatal("withCallbackRefFor")
	}
	long := map[string]any{callbackRefArg: strings.Repeat("c", maxCallbackRef+1)}
	if callbackRefFrom(withCallbackRef(context.Background(), long)) != "" {
		t.Fatal("an over-long _callback_ref was used")
	}
	res := withCallbackWarning(&mcp.CallToolResult{StructuredContent: map[string]any{"status": "pending_approval"}}, "_callback_ref was not registered")
	if res.StructuredContent.(map[string]any)["callback_warning"] != "_callback_ref was not registered" || !strings.Contains(textOf(res), "Warning: _callback_ref") {
		t.Fatalf("warning: %+v", res)
	}
}

package approval

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var testRaiser = actor.Raiser{
	CallerID:        "agent-1",
	AgentName:       "claude-cloud-3",
	AgentKind:       "agent",
	OwnerUserID:     "u_owner",
	OwnerEmail:      "owner@example.test",
	AgentSessionID:  "ses_1",
	ClientSessionID: "t3-thread-1",
	ClientKind:      "t3",
	Via:             "direct",
}

func holdRaised(t *testing.T, bus *Bus, tool string, r actor.Raiser) *Request {
	t.Helper()
	req, err := bus.Hold(context.Background(), NewRequest{
		AgentID: "agent-1", UpstreamName: "up", ToolName: tool,
		Arguments: map[string]any{"k": tool}, RaisedBy: r,
	}, 0)
	if err != nil {
		t.Fatalf("hold %s: %v", tool, err)
	}
	return req
}

func TestApprovalRaisedBy_PersistsThroughRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	bus1, err := New(ctx, db)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	req := holdRaised(t, bus1, "tool.raised", testRaiser)
	if req.RaisedBy == nil || *req.RaisedBy != testRaiser {
		t.Fatalf("Hold's request lacks RaisedBy: %+v", req.RaisedBy)
	}

	// A second Bus on the same DB stands in for a restart.
	bus2, err := New(ctx, db)
	if err != nil {
		t.Fatalf("new bus 2: %v", err)
	}
	got, err := bus2.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RaisedBy == nil || *got.RaisedBy != testRaiser {
		t.Fatalf("RaisedBy after restart = %+v, want %+v", got.RaisedBy, testRaiser)
	}
	pend, _ := bus2.ListPending(ctx)
	if len(pend) != 1 || pend[0].RaisedBy == nil || pend[0].RaisedBy.OwnerUserID != "u_owner" {
		t.Fatalf("ListPending lost RaisedBy: %+v", pend)
	}
}

func TestApprovalRaisedBy_ZeroRaiserStaysNil(t *testing.T) {
	bus := newTestBus(t)
	req := holdPending(t, bus, "tool.bare")
	got, _ := bus.Get(context.Background(), req.ID)
	if req.RaisedBy != nil || got.RaisedBy != nil {
		t.Fatalf("zero raiser should not be stored: %+v / %+v", req.RaisedBy, got.RaisedBy)
	}
}

func TestDecideAs_PersistsDecider(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	req := holdRaised(t, bus, "tool.decideas", testRaiser)
	d := actor.Decider{UserID: "u_dec", Email: "dec@example.test", Name: "Dec Ider", Via: actor.ViaPasskey, Ref: "cred_1"}
	out, err := bus.DecideAs(ctx, req.ID, StatusAllowed, d)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	for name, r := range map[string]*Request{"returned": out} {
		if r.DecidedBy != "u_dec" || r.DecidedVia != actor.ViaPasskey || r.DeciderEmail != d.Email ||
			r.DeciderName != d.Name || r.DeciderRef != "cred_1" {
			t.Errorf("%s: decider columns = by=%q via=%q email=%q name=%q ref=%q", name,
				r.DecidedBy, r.DecidedVia, r.DeciderEmail, r.DeciderName, r.DeciderRef)
		}
		if r.Decider() != d {
			t.Errorf("%s: Decider() = %+v, want %+v", name, r.Decider(), d)
		}
		if r.RaisedBy == nil || *r.RaisedBy != testRaiser {
			t.Errorf("%s: RaisedBy lost on decide: %+v", name, r.RaisedBy)
		}
	}
	got, _ := bus.Get(ctx, req.ID)
	if got.Decider() != d {
		t.Errorf("Get: Decider() = %+v, want %+v", got.Decider(), d)
	}
	if _, err := bus.DecideAs(ctx, req.ID, StatusDenied, d); !errors.Is(err, ErrNotPending) {
		t.Errorf("second decide: err = %v, want ErrNotPending", err)
	}
}

func TestDecide_LegacyStringMapping(t *testing.T) {
	cases := []struct {
		in   string
		want actor.Decider
	}{
		{"", actor.Decider{}},
		{"token", actor.Decider{Via: actor.ViaPushToken}},
		{"telegram:12345", actor.Decider{Via: actor.ViaTelegram, Ref: "12345"}},
		{"rule:ar_1", actor.Decider{Via: actor.ViaAutoRule, Ref: "ar_1"}},
		{"agent:ag_1", actor.Decider{Via: actor.ViaAgentCancel, Ref: "ag_1"}},
		{"u_abc", actor.Decider{UserID: "u_abc", Via: actor.ViaDashboard}},
		{"tester", actor.Decider{UserID: "tester", Via: actor.ViaDashboard}},
		{"agent_cancel:ag_2", actor.Decider{Via: actor.ViaAgentCancel, Ref: "ag_2"}},
		{"auto_rule:ar_2", actor.Decider{Via: actor.ViaAutoRule, Ref: "ar_2"}},
	}
	for _, c := range cases {
		if got := DeciderFromLegacy(c.in); got != c.want {
			t.Errorf("DeciderFromLegacy(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}

	bus := newTestBus(t)
	ctx := context.Background()
	req := holdPending(t, bus, "tool.legacy")
	out, err := bus.Decide(ctx, req.ID, StatusDenied, "telegram:777")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if out.DecidedBy != "telegram:777" || out.DecidedVia != actor.ViaTelegram || out.DeciderRef != "777" {
		t.Errorf("legacy telegram decide: by=%q via=%q ref=%q", out.DecidedBy, out.DecidedVia, out.DeciderRef)
	}
	req2 := holdPending(t, bus, "tool.legacy2")
	out2, err := bus.Decide(ctx, req2.ID, StatusAllowed, "u_dash")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if out2.DecidedBy != "u_dash" || out2.DecidedVia != actor.ViaDashboard {
		t.Errorf("legacy user decide: by=%q via=%q", out2.DecidedBy, out2.DecidedVia)
	}
}

// Decider() on a row written before the decider columns existed maps the
// free-text decided_by, and an expired legacy row reports expiry.
func TestRequestDecider_LegacyRows(t *testing.T) {
	r := &Request{Status: StatusAllowed, DecidedBy: "rule:ar_9"}
	if got := r.Decider(); got != (actor.Decider{Via: actor.ViaAutoRule, Ref: "ar_9"}) {
		t.Errorf("legacy rule row: %+v", got)
	}
	r = &Request{Status: StatusExpired}
	if got := r.Decider(); got != (actor.Decider{Via: actor.ViaExpiry}) {
		t.Errorf("legacy expired row: %+v", got)
	}
}

// ownedAuto matches everything and reports the rule's creator.
type ownedAuto struct{ createdBy string }

func (a ownedAuto) Match(agentID, upstream, toolName, fingerprint string, isDestructive bool) *AutoMatch {
	return &AutoMatch{ID: "rule-owned", Kind: "tool", CreatedBy: a.createdBy}
}
func (ownedAuto) MarkHit(ctx context.Context, ruleID, agentID string)                   {}
func (ownedAuto) MarkDenial(ctx context.Context, agentID, toolName, fingerprint string) {}
func (ownedAuto) IsDestructive(ctx context.Context, toolName string) bool               { return false }

func TestAutoRule_DeciderCarriesRuleAndCreator(t *testing.T) {
	ctx := context.Background()
	for _, creator := range []string{"u_creator", ""} {
		bus := newTestBus(t)
		bus.SetAutoApprover(ownedAuto{createdBy: creator})
		req := holdRaised(t, bus, "tool.auto", testRaiser)
		if req.Status != StatusAllowed || req.AutoDecidedBy != "rule-owned" {
			t.Fatalf("creator=%q: status=%q auto=%q, want allowed by rule-owned", creator, req.Status, req.AutoDecidedBy)
		}
		want := actor.Decider{UserID: creator, Via: actor.ViaAutoRule, Ref: "rule-owned"}
		if req.Decider() != want {
			t.Errorf("creator=%q: Hold Decider() = %+v, want %+v", creator, req.Decider(), want)
		}
		got, _ := bus.Get(ctx, req.ID)
		if got.Decider() != want || got.DecidedBy != want.Legacy() {
			t.Errorf("creator=%q: Get Decider() = %+v (decided_by %q), want %+v", creator, got.Decider(), got.DecidedBy, want)
		}
		if got.RaisedBy == nil || got.RaisedBy.OwnerUserID != "u_owner" {
			t.Errorf("creator=%q: RaisedBy lost on auto decide", creator)
		}
	}
}

func TestCancelByAgent_Decider(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	req := holdRaised(t, bus, "tool.cancel", testRaiser)
	out, err := bus.CancelByAgent(ctx, req.ID, "agent-1")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	want := actor.Decider{Via: actor.ViaAgentCancel, Ref: "agent-1"}
	if out.Status != StatusCancelled || out.Decider() != want {
		t.Errorf("cancel: status=%q decider=%+v, want cancelled %+v", out.Status, out.Decider(), want)
	}
	got, _ := bus.Get(ctx, req.ID)
	if got.Decider() != want || got.DecidedAt == 0 {
		t.Errorf("Get after cancel: decider=%+v decided_at=%d", got.Decider(), got.DecidedAt)
	}
}

func TestSweepExpired_Decider(t *testing.T) {
	bus := newTestBus(t)
	bus.SetTTL(time.Millisecond)
	ctx := context.Background()
	req := holdRaised(t, bus, "tool.expire", testRaiser)
	time.Sleep(5 * time.Millisecond)
	n, err := bus.SweepExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	got, _ := bus.Get(ctx, req.ID)
	if got.Status != StatusExpired || got.Decider() != (actor.Decider{Via: actor.ViaExpiry}) {
		t.Errorf("expired row: status=%q decider=%+v", got.Status, got.Decider())
	}
	if got.DecidedBy != "" {
		t.Errorf("expiry must not name a decider, decided_by=%q", got.DecidedBy)
	}
	if got.RaisedBy == nil || got.RaisedBy.AgentSessionID != "ses_1" {
		t.Errorf("RaisedBy lost on expiry: %+v", got.RaisedBy)
	}
}

func TestDecisionTokenFor_BindsRecipient(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	req := holdPending(t, bus, "tool.token")
	tok := bus.DecisionTokenFor(req.ID, "u_phone")
	if tok == req.DecisionToken {
		t.Fatal("bound token must differ from the id-only token")
	}
	out, d, err := bus.DecideByTokenAs(ctx, tok, StatusAllowed)
	if err != nil {
		t.Fatalf("decide by bound token: %v", err)
	}
	want := actor.Decider{UserID: "u_phone", Via: actor.ViaPushToken}
	if d != want || out.Decider() != want || out.DecidedBy != "u_phone" {
		t.Errorf("bound decide: d=%+v row=%+v decided_by=%q", d, out.Decider(), out.DecidedBy)
	}
}

func TestDecisionTokenFor_EmptyRecipientIsLegacy(t *testing.T) {
	bus := newTestBus(t)
	req := holdPending(t, bus, "tool.token.empty")
	if bus.DecisionTokenFor(req.ID, "") != req.DecisionToken {
		t.Fatal("empty recipient should mint the id-only token")
	}
}

func TestDecideByToken_LegacyTokenStillWorks(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	req := holdPending(t, bus, "tool.token.legacy")
	out, d, err := bus.DecideByTokenAs(ctx, req.DecisionToken, StatusDenied)
	if err != nil {
		t.Fatalf("legacy token: %v", err)
	}
	if d != (actor.Decider{Via: actor.ViaPushToken}) || out.Status != StatusDenied || out.DecidedBy != actor.ViaPushToken {
		t.Errorf("legacy token decide: d=%+v status=%q decided_by=%q", d, out.Status, out.DecidedBy)
	}
	// The pre-existing entrypoint keeps working for current callers.
	req2 := holdPending(t, bus, "tool.token.legacy2")
	out2, err := bus.DecideByToken(ctx, req2.DecisionToken, StatusAllowed)
	if err != nil || out2.Status != StatusAllowed || out2.DecidedVia != actor.ViaPushToken {
		t.Errorf("DecideByToken: err=%v status=%q via=%q", err, out2.Status, out2.DecidedVia)
	}
}

func TestDecideByToken_TamperedTokenFails(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	req := holdPending(t, bus, "tool.token.tamper")
	tok := bus.DecisionTokenFor(req.ID, "u_phone")
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	// Flip a byte inside the recipient user id: the signature covers it.
	raw[len(raw)-1] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if _, _, err := bus.DecideByTokenAs(ctx, tampered, StatusAllowed); err == nil {
		t.Fatal("tampered recipient accepted")
	}
	if _, _, err := bus.DecideByTokenAs(ctx, "not-a-token", StatusAllowed); err == nil {
		t.Fatal("garbage token accepted")
	}
	got, _ := bus.Get(ctx, req.ID)
	if got.Status != StatusPending {
		t.Fatalf("status after tampered attempts = %q, want pending", got.Status)
	}
}

func TestDecideByToken_TokenForAnotherApprovalFails(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	a := holdPending(t, bus, "tool.token.a")
	b := holdPending(t, bus, "tool.token.b")
	// Signature from A's token over B's payload: must not decide B.
	rawA, _ := base64.RawURLEncoding.DecodeString(bus.DecisionTokenFor(a.ID, "u_phone"))
	rawB, _ := base64.RawURLEncoding.DecodeString(bus.DecisionTokenFor(b.ID, "u_phone"))
	spliced := append(append([]byte{}, rawA[:64]...), rawB[64:]...)
	if _, _, err := bus.DecideByTokenAs(ctx, base64.RawURLEncoding.EncodeToString(spliced), StatusAllowed); err == nil {
		t.Fatal("spliced token accepted")
	}
	// A's own token decides A only; B stays pending.
	if _, _, err := bus.DecideByTokenAs(ctx, bus.DecisionTokenFor(a.ID, "u_phone"), StatusAllowed); err != nil {
		t.Fatalf("A's token: %v", err)
	}
	gotA, _ := bus.Get(ctx, a.ID)
	gotB, _ := bus.Get(ctx, b.ID)
	if gotA.Status != StatusAllowed || gotB.Status != StatusPending {
		t.Fatalf("A=%q B=%q, want allowed/pending", gotA.Status, gotB.Status)
	}
}

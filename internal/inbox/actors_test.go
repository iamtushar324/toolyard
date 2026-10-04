package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

func hasActivity(r *Request, text string) bool {
	for _, a := range r.Activity {
		if strings.Contains(a.Text, text) {
			return true
		}
	}
	return false
}

// A decision records who made it and how; the grants it issues carry the
// same person as issued_by, and the activity line names them.
func TestDecisionRecordsDecider(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")
	alice := actor.Decider{UserID: "u_alice", Email: "alice@example.com", Name: "Alice", Via: actor.ViaDashboard}
	got, err := e.decide(ctx, r.ID, Decision{Action: "approve", Allow: []bool{true, true, false}, Decider: alice})
	if err != nil {
		t.Fatal(err)
	}
	if got.DeciderUserID != "u_alice" || got.DeciderEmail != "alice@example.com" || got.DeciderName != "Alice" ||
		got.DeciderVia != actor.ViaDashboard || got.DecidedBy != "Alice" || got.DecidedAt == 0 {
		t.Fatalf("decider not recorded: %+v", got)
	}
	if got.Decider() != alice {
		t.Fatalf("Decider() = %+v", got.Decider())
	}
	if !hasActivity(got, "Alice allowed 2 of 3 tools") {
		t.Fatalf("activity should name the person: %+v", got.Activity)
	}
	grants, err := e.svc.ListGrants(ctx, "", "ag_1", 10)
	if err != nil || len(grants) != 2 {
		t.Fatalf("grants: %v %d", err, len(grants))
	}
	for _, g := range grants {
		if g.IssuedBy != "u_alice" {
			t.Fatalf("grant %s issued_by = %q, want u_alice", g.ID, g.IssuedBy)
		}
	}

	// Revoking one grant records the revoker; the kill switch too.
	bob := actor.Decider{UserID: "u_bob", Name: "Bob", Via: actor.ViaDashboard}
	if err := e.svc.RevokeGrantBy(ctx, got.Tools[0].GrantID, bob); err != nil {
		t.Fatal(err)
	}
	g, _, err := e.svc.getGrantWithHash(ctx, got.Tools[0].GrantID)
	if err != nil || g.Status != GrantRevoked || g.RevokedBy != "u_bob" {
		t.Fatalf("revoked grant: %v %+v", err, g)
	}
	after, _ := e.svc.Get(ctx, r.ID)
	if !hasActivity(after, "Bob revoked the permission for db.migrate") {
		t.Fatalf("revoke activity: %+v", after.Activity)
	}
	if n, err := e.svc.RevokeAllBy(ctx, "", bob); err != nil || n != 1 {
		t.Fatalf("kill switch: %d %v", n, err)
	}
	grants, _ = e.svc.ListGrants(ctx, GrantRevoked, "ag_1", 10)
	for _, g := range grants {
		if g.RevokedBy != "u_bob" {
			t.Fatalf("grant %s revoked_by = %q", g.ID, g.RevokedBy)
		}
	}
}

// A redeemed grant names its issuer by user id, email and name, read off
// the request they decided (the record the inbox.decide audit row uses),
// so the gateway's call row can say who authorised the call. When the
// request doesn't name the grant's issuer, the grant names no one else.
func TestRedeemNamesIssuer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")
	alice := actor.Decider{UserID: "u_alice", Email: "alice@example.com", Name: "Alice", Via: actor.ViaDashboard}
	if _, err := e.decide(ctx, r.ID, Decision{Action: "approve", Allow: []bool{true, true, false}, Decider: alice}); err != nil {
		t.Fatal(err)
	}
	views, err := e.svc.Status(ctx, "ag_1", []string{r.ID})
	if err != nil || len(views) != 1 || views[0].Tools[0].Grant == "" || views[0].Tools[1].Grant == "" {
		t.Fatalf("status: %v %+v", err, views)
	}
	g, err := e.svc.Redeem(ctx, views[0].Tools[0].Grant, "ag_1", "db.migrate", map[string]any{"env": "prod", "migration": "0042"})
	if err != nil {
		t.Fatal(err)
	}
	want := actor.Decider{UserID: "u_alice", Email: "alice@example.com", Name: "Alice", Via: actor.ViaInboxGrant, Ref: g.ID}
	if g.Decider() != want {
		t.Fatalf("grant decider = %+v, want %+v", g.Decider(), want)
	}

	if _, err := e.svc.db.ExecContext(ctx, `UPDATE inbox_grants SET issued_by = ? WHERE id = ?`, "u_other", views[0].Tools[1].GrantID); err != nil {
		t.Fatal(err)
	}
	g, err = e.svc.Redeem(ctx, views[0].Tools[1].Grant, "ag_1", "deploy.run", map[string]any{"service": "api", "env": "prod", "ref": "7c1d2e9"})
	if err != nil {
		t.Fatal(err)
	}
	if d := g.Decider(); d.UserID != "u_other" || d.Email != "" || d.Name != "" {
		t.Fatalf("mismatched issuer decider = %+v, want u_other with no email or name", d)
	}
}

// Without a structured decider, By (a username) still names the person
// and the instrument defaults to the dashboard; with nothing at all the
// wording stays "You".
func TestDecisionDefaultsDeciderFromBy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")
	got, err := e.decide(ctx, r.ID, Decision{Action: "deny", By: "barsha"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DecidedBy != "barsha" || got.DeciderName != "barsha" || got.DeciderVia != actor.ViaDashboard || got.DeciderUserID != "" {
		t.Fatalf("defaulted decider: %+v", got)
	}
	if !hasActivity(got, "barsha denied it") {
		t.Fatalf("activity: %+v", got.Activity)
	}

	r2 := submitDeploy(t, e, "ag_1")
	got, err = e.decide(ctx, r2.ID, Decision{Action: "return"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DecidedBy != actor.ViaDashboard || got.DeciderVia != actor.ViaDashboard || !hasActivity(got, "You sent it back to replan") {
		t.Fatalf("anonymous decision: %+v", got)
	}
}

// A notification tap is recorded as made through the push token, by the
// person the (v2) token was issued to. v1 tokens still work, unattributed,
// and a token issued to one person cannot be replayed as another.
func TestDecideByTapBindsRecipient(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{Details: true})
	ctx := context.Background()
	s := deploySubmission()
	s.Urgency = UrgencyNow
	_, r := mustSubmit(t, e, "ag_1", s)
	p := log.take()[0]
	if p.TapTokenFor == nil {
		t.Fatal("push with actions has no TapTokenFor")
	}
	bound := p.TapTokenFor("u_owner")
	if bound == "" || bound == p.TapToken {
		t.Fatalf("bound token: %q", bound)
	}
	if p.TapTokenFor("") != "" || p.TapTokenFor("a|b") != "" {
		t.Fatal("unusable user ids should not mint a token")
	}
	for _, bad := range []string{"approve", "allow", "opt0"} {
		if _, err := e.svc.DecideByTap(ctx, bound, bad); !errors.Is(err, ErrTapToken) {
			t.Fatalf("action %q accepted: %v", bad, err)
		}
	}
	// Same signature, another user in the body: refused.
	other := e.svc.tapTokenFor(r.ID, []string{TapDeny, TapSnooze}, e.now.Add(RequestTTL), "u_other")
	sig := bound[strings.LastIndex(bound, "."):]
	forged := other[:strings.LastIndex(other, ".")] + sig
	if _, err := e.svc.DecideByTap(ctx, forged, TapDeny); !errors.Is(err, ErrTapToken) {
		t.Fatalf("forged recipient accepted: %v", err)
	}
	got, err := e.svc.DecideByTap(ctx, bound, TapDeny)
	if err != nil || got.Status != StatusDenied {
		t.Fatalf("deny by bound tap: %v %+v", err, got)
	}
	if got.DecidedBy != "notification" || got.DeciderUserID != "u_owner" || got.DeciderVia != actor.ViaPushToken {
		t.Fatalf("tap decider: by=%q user=%q via=%q", got.DecidedBy, got.DeciderUserID, got.DeciderVia)
	}
	if !hasActivity(got, "You denied it from a notification") {
		t.Fatalf("activity: %+v", got.Activity)
	}

	// Legacy (unbound) token: still accepted, no person attached.
	_, q := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	p = log.take()[0]
	got, err = e.svc.DecideByTap(ctx, p.TapToken, "opt1")
	if err != nil || got.ID != q.ID || got.Status != StatusAnswered {
		t.Fatalf("answer by legacy tap: %v %+v", err, got)
	}
	if got.DeciderUserID != "" || got.DeciderVia != actor.ViaPushToken {
		t.Fatalf("legacy tap decider: user=%q via=%q", got.DeciderUserID, got.DeciderVia)
	}
}

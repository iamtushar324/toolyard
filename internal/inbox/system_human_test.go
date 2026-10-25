package inbox

import (
	"context"
	"errors"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

// A HumanOnly system kind takes an answer only from its owner in person:
// the dashboard, their own notification, or a passkey. Every other route
// is refused inside Decide, and the item stays open for the owner.
func TestHumanOnlySystemItemRefusesOtherDeciders(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &recordingHandler{note: "Opened."}
	e.svc.HandleSystem("test.github_write", h.handle, SystemOptions{HumanOnly: true})
	item := systemItem("skp_h1")
	item.Kind = "test.github_write"
	r, err := e.svc.CreateSystemItem(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]actor.Decider{
		"operator token":  {UserID: "u_1", Name: "Owner", Via: actor.ViaOperator},
		"batch click":     {UserID: "u_1", Name: "Owner", Via: actor.ViaDashboardBatch},
		"telegram":        {UserID: "u_1", Via: actor.ViaTelegram},
		"auto rule":       {Via: actor.ViaAutoRule, Ref: "rule_1"},
		"policy":          {Via: actor.ViaPolicy},
		"grant":           {UserID: "u_1", Via: actor.ViaInboxGrant},
		"unknown via":     {UserID: "u_1", Via: "something_new"},
		"no via":          {UserID: "u_1", Name: "Owner"},
		"no person":       {Name: "Owner", Via: actor.ViaDashboard},
		"another person":  {UserID: "u_2", Name: "Other", Via: actor.ViaDashboard},
		"unbound tap":     {Via: actor.ViaPushToken},
		"another's tap":   {UserID: "u_2", Via: actor.ViaPushToken},
		"another passkey": {UserID: "u_2", Via: actor.ViaPasskey},
	}
	for name, d := range refused {
		dec := answerDecision(r, "approve", "sub-"+name)
		dec.Decider = d
		if _, err := e.svc.Decide(ctx, r.ID, dec); !errors.Is(err, ErrHumanOnly) {
			t.Errorf("%s: %v, want ErrHumanOnly", name, err)
		}
	}
	if h.count() != 0 {
		t.Fatalf("a refused decision ran the handler %d times", h.count())
	}
	if got, _ := e.svc.Get(ctx, r.ID); got.Status != StatusPending {
		t.Fatalf("a refused decision closed the item: %s", got.Status)
	}
	// A deny is not an answer: anyone who may decide may refuse.
	r2, err := e.svc.CreateSystemItem(ctx, func() SystemItem { it := item; it.Ref = "skp_h2"; return it }())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(ctx, r2.ID, Decision{Action: "deny", Decider: actor.Decider{UserID: "u_1", Via: actor.ViaOperator}}); err != nil {
		t.Fatalf("deny with an operator token: %v", err)
	}
	// The owner in person: the dashboard, their notification, a passkey.
	for i, via := range []string{actor.ViaDashboard, actor.ViaPushToken, actor.ViaPasskey} {
		it := item
		it.Ref = "skp_ok" + via
		ri, err := e.svc.CreateSystemItem(ctx, it)
		if err != nil {
			t.Fatal(err)
		}
		dec := answerDecision(ri, "approve", "sub-ok-"+via)
		dec.Decider = actor.Decider{UserID: "u_1", Name: "Owner", Via: via}
		if _, err := e.svc.Decide(ctx, ri.ID, dec); err != nil {
			t.Fatalf("%s: %v", via, err)
		}
		if h.count() != i+2 { // the deny above ran the handler once
			t.Fatalf("%s: handler calls %d", via, h.count())
		}
	}
}

// Kinds without the option keep every route (the reviews and source
// proposals register none).
func TestSystemItemWithoutHumanOnlyTakesOperatorAnswers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &recordingHandler{}
	e.svc.HandleSystem("test.review", h.handle)
	r, err := e.svc.CreateSystemItem(ctx, systemItem("skp_o1"))
	if err != nil {
		t.Fatal(err)
	}
	dec := answerDecision(r, "approve", "sub-op")
	dec.Decider = actor.Decider{UserID: "u_1", Via: actor.ViaOperator}
	got, err := e.svc.Decide(ctx, r.ID, dec)
	if err != nil || h.count() != 1 || got.DeciderVia != actor.ViaOperator {
		t.Fatalf("decide: %v calls %d via %q", err, h.count(), got.DeciderVia)
	}
}

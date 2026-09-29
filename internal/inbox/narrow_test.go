package inbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustC(t *testing.T, v any) Constraint {
	t.Helper()
	c, err := ParseConstraint(v)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWithin(t *testing.T) {
	cases := []struct {
		narrow, req any
		want        bool
	}{
		{"prod", "prod", true},
		{"staging", "prod", false},
		{"api", map[string]any{"in": []any{"api", "worker"}}, true},
		{map[string]any{"in": []any{"api"}}, map[string]any{"in": []any{"api", "worker"}}, true},
		{map[string]any{"in": []any{"api", "web"}}, map[string]any{"in": []any{"api", "worker"}}, false},
		{"release/1.2", map[string]any{"prefix": "release/"}, true},
		{map[string]any{"prefix": "release/1."}, map[string]any{"prefix": "release/"}, true},
		{map[string]any{"prefix": "rel"}, map[string]any{"prefix": "release/"}, false},
		{3, map[string]any{"gte": 1, "lte": 6}, true},
		{map[string]any{"gte": 2, "lte": 4}, map[string]any{"gte": 1, "lte": 6}, true},
		{map[string]any{"lte": 4}, map[string]any{"gte": 1, "lte": 6}, false},
		{map[string]any{"gte": 0, "lte": 4}, map[string]any{"gte": 1, "lte": 6}, false},
		{"7c1d2e9", map[string]any{"limit": "merge commit", "pattern": "^[0-9a-f]{7,40}$"}, true},
		{"main", map[string]any{"limit": "merge commit", "pattern": "^[0-9a-f]{7,40}$"}, false},
		{"anything", map[string]any{"any": true}, true},
		{map[string]any{"any": true}, "prod", false},
		{map[string]any{"prefix": "p"}, "prod", false},
	}
	for i, c := range cases {
		if got := mustC(t, c.narrow).Within(mustC(t, c.req)); got != c.want {
			t.Errorf("case %d: %v within %v = %v, want %v", i, c.narrow, c.req, got, c.want)
		}
	}
}

func TestOwnerNarrowsScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")

	widen := Decision{Action: "approve", Allow: []bool{true, true, false},
		Params: map[int]map[string]Constraint{0: {"env": mustC(t, "staging")}}}
	if _, err := e.svc.Decide(ctx, r.ID, widen); !errors.Is(err, ErrWiden) {
		t.Fatalf("changing a value must fail: %v", err)
	}
	extra := Decision{Action: "approve", Allow: []bool{true, true, false},
		Params: map[int]map[string]Constraint{0: {"region": mustC(t, "eu")}}}
	if _, err := e.svc.Decide(ctx, r.ID, extra); !errors.Is(err, ErrWiden) {
		t.Fatalf("adding a parameter must fail: %v", err)
	}
	if _, err := e.svc.Decide(ctx, r.ID, Decision{Action: "approve", Allow: []bool{true, true, false}, TTLSeconds: 3600}); !errors.Is(err, ErrWiden) {
		t.Fatalf("a longer TTL must fail: %v", err)
	}

	got, err := e.svc.Decide(ctx, r.ID, Decision{Action: "approve", Allow: []bool{true, true, false}, TTLSeconds: 600,
		Params: map[int]map[string]Constraint{1: {"ref": mustC(t, "7c1d2e9")}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools[1].Requested == nil || got.Tools[1].Params["ref"].Op != "eq" || got.TTLSeconds != 600 {
		t.Fatalf("narrowing not stored: %+v", got.Tools[1])
	}
	views, _ := e.svc.Status(ctx, "ag_1", []string{r.ID})
	tv := views[0].Tools[1]
	if !tv.Narrowed || tv.Params["ref"].Describe() != `"7c1d2e9"` || views[0].Tools[0].Narrowed {
		t.Fatalf("agent view: %+v", views[0].Tools)
	}
	// The pattern would have allowed another commit; the narrowed grant doesn't.
	if _, err := e.svc.Redeem(ctx, tv.Grant, "ag_1", "deploy.run", map[string]any{"service": "api", "env": "prod", "ref": "abcdef1"}); !errors.Is(err, ErrGrantScope) {
		t.Fatalf("narrowed grant allowed another value: %v", err)
	}
	e.advance(9 * time.Minute)
	if _, err := e.svc.Redeem(ctx, tv.Grant, "ag_1", "deploy.run", map[string]any{"service": "api", "env": "prod", "ref": "7c1d2e9"}); err != nil {
		t.Fatalf("narrowed grant refused its own value: %v", err)
	}
	e.advance(2 * time.Minute)
	if _, err := e.svc.Redeem(ctx, views[0].Tools[0].Grant, "ag_1", "db.migrate", map[string]any{"env": "prod", "migration": "0042"}); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("shortened TTL not applied: %v", err)
	}
}

package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestRequestedSecretResolvesOnlyAfterOwnerSetsIt(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)

	m, err := s.Request(ctx, "GITHUB_TOKEN", "PAT for github MCP", "operator: claude")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !m.Pending || m.RequestedBy != "operator: claude" {
		t.Fatalf("meta = %+v", m)
	}
	if ok, _ := s.Exists(ctx, "GITHUB_TOKEN"); !ok {
		t.Fatal("a requested secret must exist so servers can reference it")
	}
	if _, err := s.Resolve(ctx, "GITHUB_TOKEN"); !errors.Is(err, ErrPending) {
		t.Fatalf("resolve pending: err = %v, want ErrPending", err)
	}
	if _, err := s.Request(ctx, "GITHUB_TOKEN", "", ""); err != ErrExists {
		t.Fatalf("duplicate request: %v", err)
	}

	if _, err := s.Update(ctx, "GITHUB_TOKEN", "ghp_value", nil); err != nil {
		t.Fatalf("owner sets value: %v", err)
	}
	got, err := s.Get(ctx, "GITHUB_TOKEN")
	if err != nil || got.Pending {
		t.Fatalf("after set: %+v %v", got, err)
	}
	if v, err := s.Resolve(ctx, "GITHUB_TOKEN"); err != nil || v != "ghp_value" {
		t.Fatalf("resolve = %q %v", v, err)
	}
}

func TestEmbeddedRefs(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if _, err := s.Create(ctx, "API_KEY", "k1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "ORG", "o2", ""); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		in    string
		names []string
		ok    bool
	}{
		{"secret://API_KEY", []string{"API_KEY"}, true},
		{"Bearer ${secret://API_KEY}", []string{"API_KEY"}, true},
		{"${secret://API_KEY}:${secret://ORG}", []string{"API_KEY", "ORG"}, true},
		{"plain", nil, true},
		{"Bearer ${secret://api_key}", nil, false},
		{"Bearer ${secret://API_KEY", nil, false},
		{"secret://bad name", nil, false},
	}
	for _, c := range cases {
		names, ok := Refs(c.in)
		if ok != c.ok || len(names) != len(c.names) {
			t.Errorf("Refs(%q) = %v %v, want %v %v", c.in, names, ok, c.names, c.ok)
			continue
		}
		for i := range names {
			if names[i] != c.names[i] {
				t.Errorf("Refs(%q)[%d] = %q", c.in, i, names[i])
			}
		}
	}
	if !IsRef("Bearer ${secret://API_KEY}") || IsRef("Bearer x") {
		t.Error("IsRef wrong for embedded refs")
	}

	out, err := s.ResolveMap(ctx, map[string]string{
		"Authorization": "Bearer ${secret://API_KEY}",
		"X-Pair":        "${secret://API_KEY}:${secret://ORG}",
		"Whole":         "secret://ORG",
		"Plain":         "literal",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Authorization": "Bearer k1", "X-Pair": "k1:o2", "Whole": "o2", "Plain": "literal"}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %q, want %q", k, out[k], v)
		}
	}
	if _, err := s.ResolveMap(ctx, map[string]string{"A": "x ${secret://MISSING}"}); err == nil {
		t.Error("unknown embedded secret should fail")
	}
}

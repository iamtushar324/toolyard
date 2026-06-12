package upstreams

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeResolver implements SecretResolver over a static map.
type fakeResolver struct {
	store map[string]string
}

func (f fakeResolver) ResolveMap(ctx context.Context, in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if len(v) > len(RefPrefixTest) && v[:len(RefPrefixTest)] == RefPrefixTest {
			name := v[len(RefPrefixTest):]
			rv, ok := f.store[name]
			if !ok {
				return nil, errors.New("unknown secret " + name)
			}
			out[k] = rv
			continue
		}
		out[k] = v
	}
	return out, nil
}

func (f fakeResolver) Exists(ctx context.Context, name string) (bool, error) {
	_, ok := f.store[name]
	return ok, nil
}

const RefPrefixTest = "secret://"

func TestMasked(t *testing.T) {
	srv := Server{
		Name: "github",
		Env: map[string]string{
			"TOKEN": "secret://GH_TOKEN", // ref → passthrough
			"PLAIN": "ghp_rawvalue",      // plaintext → masked
			"EMPTY": "",                  // empty → empty
		},
		Headers: map[string]string{
			"X-Api-Key": "raw-header-value",
		},
	}
	m := Masked(srv)
	if m.Env["TOKEN"] != "secret://GH_TOKEN" {
		t.Errorf("ref should pass through, got %q", m.Env["TOKEN"])
	}
	if m.Env["PLAIN"] != "•••" {
		t.Errorf("plaintext should be masked, got %q", m.Env["PLAIN"])
	}
	if m.Env["EMPTY"] != "" {
		t.Errorf("empty should stay empty, got %q", m.Env["EMPTY"])
	}
	if m.Headers["X-Api-Key"] != "•••" {
		t.Errorf("header should be masked, got %q", m.Headers["X-Api-Key"])
	}
	want := []string{"PLAIN"}
	if !reflect.DeepEqual(m.EnvPlaintextKeys, want) {
		t.Errorf("EnvPlaintextKeys = %v, want %v", m.EnvPlaintextKeys, want)
	}
}

func TestToCfgEnvFuncSubstitutes(t *testing.T) {
	s := &Service{secrets: fakeResolver{store: map[string]string{"GH_TOKEN": "ghp_live"}}}
	cfg := s.toCfg(Server{
		Name:      "github",
		Transport: "stdio",
		Command:   "echo",
		Env:       map[string]string{"TOKEN": "secret://GH_TOKEN", "RAW": "literal"},
	})
	if cfg.EnvFunc == nil {
		t.Fatal("expected EnvFunc to be wired for stdio + secrets")
	}
	env, err := cfg.EnvFunc(context.Background())
	if err != nil {
		t.Fatalf("EnvFunc: %v", err)
	}
	if env["TOKEN"] != "ghp_live" || env["RAW"] != "literal" {
		t.Fatalf("EnvFunc resolved = %+v", env)
	}
}

func TestToCfgHTTPHeaderComposition(t *testing.T) {
	s := &Service{secrets: fakeResolver{store: map[string]string{"KEY": "resolved-key"}}}
	cfg := s.toCfg(Server{
		Name:      "remote",
		Transport: "http",
		URL:       "https://example.com/mcp",
		Headers:   map[string]string{"X-Api-Key": "secret://KEY"},
	})
	if cfg.HeaderFunc == nil {
		t.Fatal("expected HeaderFunc for http + static headers")
	}
	h := cfg.HeaderFunc(context.Background())
	if h["X-Api-Key"] != "resolved-key" {
		t.Fatalf("header resolved = %+v", h)
	}
}

func TestValidateSecretRefs(t *testing.T) {
	s := &Service{secrets: fakeResolver{store: map[string]string{"KNOWN": "x"}}}
	ctx := context.Background()

	if err := s.validateSecretRefs(ctx, Server{Env: map[string]string{"A": "secret://KNOWN"}}); err != nil {
		t.Fatalf("known ref should validate: %v", err)
	}
	if err := s.validateSecretRefs(ctx, Server{Env: map[string]string{"A": "secret://MISSING"}}); err == nil {
		t.Fatal("unknown ref should fail validation")
	}
	if err := s.validateSecretRefs(ctx, Server{Headers: map[string]string{"H": "secret://lower"}}); err == nil {
		t.Fatal("malformed ref should fail validation")
	}
	// plaintext passes (not a ref).
	if err := s.validateSecretRefs(ctx, Server{Env: map[string]string{"A": "plain"}}); err != nil {
		t.Fatalf("plaintext should validate: %v", err)
	}
}

package upstreams

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
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

func TestAssertionProfileHasNoReservedServiceName(t *testing.T) {
	if err := validate(Server{Name: "bks_preview", Transport: "http", URL: "http://127.0.0.1:18791/mcp", AuthMode: AuthPerUser, AssertionProfile: "preview"}); err != nil {
		t.Fatalf("normal configured upstream name is not allowed: %v", err)
	}
}

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

// TestValidateIdentity covers the identity-forwarding rules: http only, a
// real header token, none of the transport/credential headers, and no
// static header under the same name (an admin must not pin a fixed key).
func TestValidateIdentity(t *testing.T) {
	http := func(id *IdentityForwarding, headers map[string]string) Server {
		return Server{Name: "bk", Transport: "http", URL: "https://example.com/mcp", Identity: id, Headers: headers}
	}
	ok := []Server{
		http(nil, nil),
		http(&IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true}, nil),
		http(&IdentityForwarding{Header: "X-Custom_Id.1"}, map[string]string{"X-Api-Key": "k"}),
		{Name: "bk", Transport: "", URL: "https://example.com/mcp", Identity: &IdentityForwarding{Header: "x-id"}},
	}
	for i, srv := range ok {
		if err := validate(srv); err != nil {
			t.Errorf("ok[%d]: validate = %v, want nil", i, err)
		}
	}
	bad := map[string]Server{
		"stdio":         {Name: "fs", Transport: "stdio", Command: "echo", Identity: &IdentityForwarding{Header: "x-id"}},
		"empty header":  http(&IdentityForwarding{Header: ""}, nil),
		"space":         http(&IdentityForwarding{Header: "x id"}, nil),
		"colon":         http(&IdentityForwarding{Header: "x-id:"}, nil),
		"non-ascii":     http(&IdentityForwarding{Header: "x-idé"}, nil),
		"authorization": http(&IdentityForwarding{Header: "Authorization"}, nil),
		"cookie":        http(&IdentityForwarding{Header: "cookie"}, nil),
		"host":          http(&IdentityForwarding{Header: "HOST"}, nil),
		"content-type":  http(&IdentityForwarding{Header: "content-type"}, nil),
		"session":       http(&IdentityForwarding{Header: "Mcp-Session-Id"}, nil),
		"protocol":      http(&IdentityForwarding{Header: "mcp-protocol-version"}, nil),
		"last-event-id": http(&IdentityForwarding{Header: "Last-Event-ID"}, nil),
		"accept":        http(&IdentityForwarding{Header: "accept"}, nil),
		"length":        http(&IdentityForwarding{Header: "Content-Length"}, nil),
		"static clash":  http(&IdentityForwarding{Header: "x-bk-bifrost-vk"}, map[string]string{"X-BK-Bifrost-VK": "pinned"}),
	}
	for name, srv := range bad {
		if err := validate(srv); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: validate = %v, want ErrInvalid", name, err)
		}
	}
}

// fakeAuth is a HeaderProvider that always offers the same headers.
type fakeAuth struct{ headers map[string]string }

func (f fakeAuth) HeaderFunc(string) func(ctx context.Context) map[string]string {
	return func(context.Context) map[string]string { return f.headers }
}
func (f fakeAuth) HasClient(context.Context, string) (bool, error) { return true, nil }
func (f fakeAuth) Disconnect(context.Context, string) error        { return nil }

// TestValidateRejectsMaskedValues: the "•••" placeholder that Masked()
// returns is never accepted as a real value, so a client that round-trips
// the masked view back into a save can't overwrite a credential with dots.
func TestValidateRejectsMaskedValues(t *testing.T) {
	base := Server{Name: "bk", Transport: "http", URL: "https://example.com/mcp"}
	h := base
	h.Headers = map[string]string{"X-Api-Key": MaskedValue}
	err := validate(h)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "X-Api-Key") || !strings.Contains(err.Error(), "masked value") {
		t.Fatalf("masked header: err = %v", err)
	}
	e := base
	e.Env = map[string]string{"TOKEN": MaskedValue}
	err = validate(e)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "TOKEN") || !strings.Contains(err.Error(), "masked value") {
		t.Fatalf("masked env: err = %v", err)
	}
	// Refs and plaintext still pass.
	ok := base
	ok.Headers = map[string]string{"X-Api-Key": "secret://KEY", "X-Plain": "value"}
	if err := validate(ok); err != nil {
		t.Fatalf("ref/plaintext headers: %v", err)
	}
}

// TestToCfgIdentityHeader: the identity header carries the key from the
// call context or nothing at all. Whatever static headers or OAuth put
// under that name is discarded, whichever letter case they used.
func TestToCfgIdentityHeader(t *testing.T) {
	s := &Service{auth: fakeAuth{headers: map[string]string{"Authorization": "Bearer live", "X-BK-BIFROST-VK": "from-oauth"}}}
	cfg := s.toCfg(Server{
		Name:      "bk",
		Transport: "http",
		URL:       "https://example.com/mcp",
		Headers:   map[string]string{"X-Bk-Bifrost-Vk": "pinned", "X-Api-Key": "shared"},
		Identity:  &IdentityForwarding{Header: "x-bk-bifrost-vk"},
	})
	if cfg.IdentityHeader != "x-bk-bifrost-vk" {
		t.Fatalf("IdentityHeader = %q", cfg.IdentityHeader)
	}
	if cfg.HeaderFunc == nil {
		t.Fatal("expected HeaderFunc when identity forwarding is on")
	}
	// No key on the context: the header is absent under every spelling.
	h := cfg.HeaderFunc(context.Background())
	for k := range h {
		if strings.EqualFold(k, "x-bk-bifrost-vk") {
			t.Fatalf("identity header leaked without a key: %q=%q", k, h[k])
		}
	}
	if h["Authorization"] != "Bearer live" || h["X-Api-Key"] != "shared" {
		t.Fatalf("other headers lost: %+v", h)
	}
	// With a key: exactly one identity header, holding the key.
	h = cfg.HeaderFunc(gateway.WithForwardedKey(context.Background(), "vk-alice"))
	n := 0
	for k, v := range h {
		if strings.EqualFold(k, "x-bk-bifrost-vk") {
			n++
			if v != "vk-alice" {
				t.Fatalf("identity header = %q, want the caller's key", v)
			}
		}
	}
	if n != 1 {
		t.Fatalf("identity header set %d times: %+v", n, h)
	}
	if h["Authorization"] != "Bearer live" {
		t.Fatalf("OAuth header lost: %+v", h)
	}

	// Without Identity nothing changes: a static header of that name is
	// sent verbatim and a forwarded key is ignored.
	plain := s.toCfg(Server{Name: "plain", Transport: "http", URL: "https://example.com/mcp",
		Headers: map[string]string{"x-bk-bifrost-vk": "pinned"}})
	if plain.IdentityHeader != "" {
		t.Fatalf("IdentityHeader = %q on a plain server", plain.IdentityHeader)
	}
	h = plain.HeaderFunc(gateway.WithForwardedKey(context.Background(), "vk-alice"))
	if h["x-bk-bifrost-vk"] != "pinned" {
		t.Fatalf("plain server header = %+v", h)
	}

	// An identity-only server (no static headers, no OAuth) still gets a
	// HeaderFunc, otherwise the key could never be sent.
	bare := (&Service{}).toCfg(Server{Name: "bare", Transport: "http", URL: "https://example.com/mcp",
		Identity: &IdentityForwarding{Header: "x-bk-bifrost-vk"}})
	if bare.HeaderFunc == nil {
		t.Fatal("identity-only server has no HeaderFunc")
	}
	if h := bare.HeaderFunc(gateway.WithForwardedKey(context.Background(), "vk-bob")); h["x-bk-bifrost-vk"] != "vk-bob" {
		t.Fatalf("bare header = %+v", h)
	}
}

// TestToCfgUnresolvedSecretRefDropped: a header whose secret:// ref can't
// be resolved is left out of the request. The literal ref must never go
// upstream, and the other headers are unaffected.
func TestToCfgUnresolvedSecretRefDropped(t *testing.T) {
	s := &Service{secrets: fakeResolver{store: map[string]string{"KNOWN": "resolved-known"}}}
	cfg := s.toCfg(Server{
		Name:      "remote",
		Transport: "http",
		URL:       "https://example.com/mcp",
		Headers: map[string]string{
			"X-Known":   "secret://KNOWN",
			"X-Missing": "secret://MISSING",
			"X-Plain":   "literal",
		},
	})
	h := cfg.HeaderFunc(context.Background())
	if _, ok := h["X-Missing"]; ok {
		t.Fatalf("unresolved ref was sent: %q", h["X-Missing"])
	}
	if h["X-Known"] != "resolved-known" || h["X-Plain"] != "literal" {
		t.Fatalf("headers = %+v", h)
	}
	for _, v := range h {
		if strings.HasPrefix(v, "secret://") {
			t.Fatalf("literal secret ref leaked into headers: %+v", h)
		}
	}

	// No broker wired at all: refs are dropped too, plaintext still flows.
	none := (&Service{}).toCfg(Server{Name: "remote", Transport: "http", URL: "https://example.com/mcp",
		Headers: map[string]string{"X-Missing": "secret://MISSING", "X-Plain": "literal"}})
	h = none.HeaderFunc(context.Background())
	if _, ok := h["X-Missing"]; ok || h["X-Plain"] != "literal" {
		t.Fatalf("no-broker headers = %+v", h)
	}
}

// TestMaskedKeepsIdentity: Masked hides values, not the identity setting.
func TestMaskedKeepsIdentity(t *testing.T) {
	id := &IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true}
	m := Masked(Server{Name: "bk", Identity: id, Headers: map[string]string{"X-Api-Key": "raw"}})
	if m.Identity == nil || *m.Identity != *id {
		t.Fatalf("Masked identity = %+v, want %+v", m.Identity, id)
	}
	if m.Headers["X-Api-Key"] != "•••" {
		t.Fatalf("header not masked: %+v", m.Headers)
	}
}

// TestPatchIdentityTriState: absent leaves the setting alone, an explicit
// null clears it, an object replaces it.
func TestPatchIdentityTriState(t *testing.T) {
	var absent Patch
	if err := json.Unmarshal([]byte(`{"url":"https://x/mcp"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Identity.Set {
		t.Fatal("absent identity reported as set")
	}
	var cleared Patch
	if err := json.Unmarshal([]byte(`{"identity":null}`), &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.Identity.Set || cleared.Identity.Value != nil {
		t.Fatalf("null identity = %+v, want Set with nil Value", cleared.Identity)
	}
	var set Patch
	if err := json.Unmarshal([]byte(`{"identity":{"header":"x-bk-bifrost-vk","register":true}}`), &set); err != nil {
		t.Fatal(err)
	}
	if !set.Identity.Set || set.Identity.Value == nil || set.Identity.Value.Header != "x-bk-bifrost-vk" || !set.Identity.Value.Register {
		t.Fatalf("identity = %+v", set.Identity)
	}
	var unknown Patch
	if err := json.Unmarshal([]byte(`{"identity":{"header":"x","bogus":1}}`), &unknown); err == nil {
		t.Fatal("unknown identity field accepted")
	}
}

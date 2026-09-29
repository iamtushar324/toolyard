package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

var testAgent = &identity.Agent{
	ID: "ag_1", Name: "claude-cloud-3", Kind: identity.AgentKindAgent, Owner: "u_1",
	OwnerEmail: "ada@beknown.work", OwnerDisplayName: "Ada Lovelace", OwnerUsername: "ada",
}

// mcpAuthProbe runs a request through guard → (a handler standing in for
// mcp-go, which calls contextFunc on the request's context) and reports
// how many times the token was verified and what the call context held.
type mcpAuthProbe struct {
	auth     mcpAuth
	verified int32
	served   bool
	raiser   actor.Raiser
	hasRaise bool
	agentID  string
}

func newMCPAuthProbe(requireAuth bool, proxies ...string) *mcpAuthProbe {
	p := &mcpAuthProbe{}
	p.auth = mcpAuth{
		verify: func(ctx context.Context, tok string) (*identity.Agent, error) {
			atomic.AddInt32(&p.verified, 1)
			if tok != "ag_1.good" {
				return nil, identity.ErrAgentTokenInvalid
			}
			ag := *testAgent
			return &ag, nil
		},
		requireAuth: requireAuth,
	}
	for _, c := range proxies {
		_, n, _ := net.ParseCIDR(c)
		p.auth.sec.TrustedProxies = append(p.auth.sec.TrustedProxies, n)
	}
	return p
}

func (p *mcpAuthProbe) do(r *http.Request) *httptest.ResponseRecorder {
	p.verified, p.served, p.hasRaise, p.raiser, p.agentID = 0, false, false, actor.Raiser{}, ""
	rec := httptest.NewRecorder()
	p.auth.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.served = true
		ctx := p.auth.contextFunc(r.Context(), r)
		p.raiser, p.hasRaise = actor.RaiserFrom(ctx)
		p.agentID = gateway.AgentIDFromContext(ctx)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec
}

func mcpReq(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestMCPAuthVerifiesOnceAndBuildsRaiser(t *testing.T) {
	p := newMCPAuthProbe(false, "10.0.0.0/8")

	// Bearer through the trusted proxy: the forwarded IP is the client,
	// but the T3 session header is still the caller's claim (Traefik and
	// socat pass client headers through unchanged, so any public caller
	// could set it).
	rec := p.do(mcpReq("10.0.0.7:5000", map[string]string{
		"Authorization":     "Bearer ag_1.good",
		"X-Forwarded-For":   "203.0.113.9, 10.0.0.7",
		"Mcp-Session-Id":    "mcp_abc",
		"x-t3-session-id":   "thr_42",
		"x-toolyard-client": "t3",
	}))
	if rec.Code != http.StatusOK || !p.served || p.verified != 1 {
		t.Fatalf("bearer: code %d served %v verified %d", rec.Code, p.served, p.verified)
	}
	if !p.hasRaise || p.agentID != "ag_1" {
		t.Fatalf("call context: raiser %v agent %q", p.hasRaise, p.agentID)
	}
	want := actor.Raiser{
		CallerID: "ag_1", AgentName: "claude-cloud-3", AgentKind: "agent",
		OwnerUserID: "u_1", OwnerEmail: "ada@beknown.work", OwnerName: "Ada Lovelace",
		MCPSessionID: "mcp_abc", ClientSessionID: "thr_42", ClientSessionClaimed: true,
		ClientKind: "t3", ClientIP: "203.0.113.9",
	}
	if p.raiser != want {
		t.Fatalf("raiser = %+v\nwant     %+v", p.raiser, want)
	}

	// x-bf-vk from an untrusted peer: same agent, the session id is the
	// caller's claim, the client kind is sniffed from the User-Agent and
	// X-Forwarded-For is ignored.
	rec = p.do(mcpReq("198.51.100.4:6000", map[string]string{
		"x-bf-vk":            "ag_1.good",
		"X-Forwarded-For":    "203.0.113.9",
		"x-bk-agent-session": "ses_claimed",
		"User-Agent":         "claude-code/2.0.1",
	}))
	if rec.Code != http.StatusOK || p.verified != 1 {
		t.Fatalf("x-bf-vk: code %d verified %d", rec.Code, p.verified)
	}
	if p.raiser.CallerID != "ag_1" || p.raiser.OwnerEmail != "ada@beknown.work" {
		t.Fatalf("x-bf-vk raiser: %+v", p.raiser)
	}
	if p.raiser.ClientSessionID != "ses_claimed" || !p.raiser.ClientSessionClaimed {
		t.Fatalf("untrusted peer's session must be claimed: %+v", p.raiser)
	}
	if p.raiser.ClientIP != "198.51.100.4" || p.raiser.ClientKind != "claude_code" || p.raiser.MCPSessionID != "" {
		t.Fatalf("x-bf-vk raiser: %+v", p.raiser)
	}
	if p.raiser.Via != "" {
		t.Fatalf("Via is the gateway's to set, got %q", p.raiser.Via)
	}

	// Header values are cleaned: control characters dropped, long values
	// capped.
	rec = p.do(mcpReq("198.51.100.4:6000", map[string]string{
		"Authorization":   "Bearer ag_1.good",
		"x-t3-session-id": " thr\x01_9 ",
	}))
	if rec.Code != http.StatusOK || p.raiser.ClientSessionID != "thr_9" {
		t.Fatalf("cleaned session: %+v", p.raiser)
	}

	// A bad token is a 401 before the handler; still one verification.
	rec = p.do(mcpReq("198.51.100.4:6000", map[string]string{"Authorization": "Bearer ag_1.stale"}))
	if rec.Code != http.StatusUnauthorized || p.served || p.verified != 1 {
		t.Fatalf("bad token: code %d served %v verified %d", rec.Code, p.served, p.verified)
	}

	// No credential: through untouched (anonymous local caller), nothing
	// verified, no raiser.
	rec = p.do(mcpReq("127.0.0.1:6000", nil))
	if rec.Code != http.StatusOK || !p.served || p.verified != 0 || p.hasRaise || p.agentID != "" {
		t.Fatalf("anonymous: code %d served %v verified %d raiser %v agent %q", rec.Code, p.served, p.verified, p.hasRaise, p.agentID)
	}

	// With -require-auth-on-mcp anonymous traffic is refused.
	strict := newMCPAuthProbe(true)
	if rec := strict.do(mcpReq("127.0.0.1:6000", nil)); rec.Code != http.StatusUnauthorized || strict.served {
		t.Fatalf("require auth, anonymous: code %d served %v", rec.Code, strict.served)
	}
}

func TestClientKindFromRequest(t *testing.T) {
	cases := []struct {
		name, header, ua, want string
	}{
		{"explicit t3", "t3", "node", "t3"},
		{"explicit cli", "cli", "Go-http-client/1.1", "cli"},
		{"explicit mixed case", "Claude_Code", "", "claude_code"},
		{"explicit clientInfo-style name", "claude-code/1.2", "", "claude_code"},
		{"explicit unknown", "weird-thing", "claude-code/1.0", "unknown"},
		{"ua claude code", "", "claude-code/2.0.1", "claude_code"},
		{"ua t3", "", "t3-code/0.9 (darwin)", "t3"},
		{"ua codex", "", "codex-cli/0.4.0", "codex"},
		{"ua cursor", "", "Cursor/0.48.7", "cursor"},
		{"ua opencode", "", "opencode/1.1", "opencode"},
		{"ua toolyard cli", "", "toolyard-cli/dev", "cli"},
		{"ua browser", "", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120", "browser"},
		{"ua unknown leaves it to clientInfo", "", "Go-http-client/1.1", ""},
		{"no ua", "", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if c.header != "" {
			r.Header.Set("x-toolyard-client", c.header)
		}
		if c.ua != "" {
			r.Header.Set("User-Agent", c.ua)
		}
		if got := clientKindFromRequest(r); got != c.want {
			t.Errorf("%s: clientKindFromRequest = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInboxTapToken(t *testing.T) {
	bound := inbox.Push{TapToken: "legacy", TapTokenFor: func(uid string) string { return "bound:" + uid }}
	if got := inboxTapToken(bound, "u_1"); got != "bound:u_1" {
		t.Errorf("bound = %q", got)
	}
	if got := inboxTapToken(inbox.Push{TapToken: "legacy"}, "u_1"); got != "legacy" {
		t.Errorf("legacy = %q", got)
	}
	if got := inboxTapToken(inbox.Push{}, "u_1"); got != "" {
		t.Errorf("no actions = %q", got)
	}
}

// A Telegram button records the paired Telegram user as the instrument
// and names it as such; no toolyard user is attributed, because the
// pairing stores no link from a Telegram id to a person.
func TestTelegramDecider(t *testing.T) {
	got := telegramDecider("telegram:12345")
	want := actor.Decider{Name: "Telegram user 12345", Via: actor.ViaTelegram, Ref: "12345"}
	if got != want {
		t.Fatalf("telegramDecider = %+v, want %+v", got, want)
	}
	if got.UserID != "" || got.Email != "" {
		t.Fatalf("a Telegram tap must not be attributed to a toolyard user: %+v", got)
	}
	if got.Legacy() != "telegram:12345" {
		t.Fatalf("legacy decided_by = %q", got.Legacy())
	}
	// A malformed value still records the route without inventing a
	// person or a name.
	if odd := telegramDecider("telegram:"); odd.UserID != "" || odd.Name != "" || odd.Via != actor.ViaTelegram {
		t.Fatalf("malformed = %+v", odd)
	}
}

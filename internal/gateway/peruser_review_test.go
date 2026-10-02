package gateway_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// TestPerUser401RefreshesThenRetries: an upstream 401 is first taken for
// an expired access token: the token is refreshed once and the request
// retried once on a fresh connection, with no re-auth mark. A 401 at dial
// time gets the same treatment. Only when there is nothing to refresh
// with is the person marked and the call refused.
func TestPerUser401RefreshesThenRetries(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob-1")
	f.add(t, "linear", up)
	if res := f.call(t, agBob, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-bob-1" {
		t.Fatalf("warm call: %q", text(res))
	}

	// The token expires under a live session: 401 on the call.
	up.denyBearer("Bearer tok-bob-1")
	f.tokens.canRefreshTo("linear", uBob, "tok-bob-2")
	res := f.call(t, agBob, "linear.get_whoami")
	if res.IsError || text(res) != "Bearer tok-bob-2" {
		t.Fatalf("after refresh ran as %q (isError=%v %s)", text(res), res.IsError, text(res))
	}
	if got := f.tokens.refreshes(); len(got) != 1 || got[0] != "linear/"+uBob {
		t.Fatalf("refreshes = %v", got)
	}
	if got := f.tokens.marks(); len(got) != 0 {
		t.Fatalf("marked after a refreshable 401: %v", got)
	}
	// The retried call ran on a new session: one bearer per session still.
	for sid, bearers := range up.sessionsByBearer() {
		if len(bearers) != 1 {
			t.Fatalf("session %s carried %v", sid, bearers)
		}
	}

	// 401 at dial time (the connection was dropped; the stored token is
	// dead by the time the next call dials): refresh, re-dial, succeed.
	f.svc.DropUserConnection("linear", uBob)
	up.denyBearer("Bearer tok-bob-2")
	f.tokens.canRefreshTo("linear", uBob, "tok-bob-3")
	if res := f.call(t, agBob, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-bob-3" {
		t.Fatalf("after dial 401 ran as %q (isError=%v)", text(res), res.IsError)
	}
	if got := f.tokens.refreshes(); len(got) != 2 {
		t.Fatalf("refreshes = %v", got)
	}

	// Nothing to refresh with: the store marks the row, the call is
	// refused, and the next call is refused before the upstream.
	up.denyBearer("Bearer tok-bob-3")
	res = f.call(t, agBob, "linear.get_whoami")
	if !res.IsError || !strings.Contains(text(res), "could not be refreshed") {
		t.Fatalf("unrefreshable 401: isError=%v %q", res.IsError, text(res))
	}
	if got := f.tokens.marks(); len(got) != 1 || got[0] != "linear/"+uBob {
		t.Fatalf("marks = %v", got)
	}
	before := len(up.requests())
	if res := f.call(t, agBob, "linear.get_whoami"); !res.IsError || !strings.Contains(text(res), "#connections") {
		t.Fatalf("bob after the mark: isError=%v %q", res.IsError, text(res))
	}
	if len(up.requests()) != before {
		t.Fatal("refused call reached the upstream")
	}
	// Alice was never touched.
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice: %q", text(res))
	}
	if got := f.tokens.refreshes(); len(got) != 3 {
		t.Fatalf("refreshes after everything = %v", got)
	}
}

// TestPerUserExpiredTokenRefreshedBeforeDial: a token whose access expiry
// has passed (toolyard was down longer than its lifetime) is refreshed
// before the first dial, so the upstream never sees the dead bearer and
// nobody is flipped to needs_reauth. Expired with nothing to refresh is
// refused before the upstream, with the reason.
func TestPerUserExpiredTokenRefreshedBeforeDial(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice-old")
	f.tokens.connect("linear", uBob, "tok-bob-old")
	f.tokens.expired["linear/"+uAlice] = true
	f.tokens.expired["linear/"+uBob] = true
	f.tokens.canRefreshTo("linear", uAlice, "tok-alice-new")
	// Bob cannot be refreshed.

	// Adding the server loads tools over the first user whose token can
	// be made ready: alice, with her new token; bob is marked, not dialled.
	srv := f.add(t, "linear", up)
	if srv.LastStatus != "ok" || srv.ToolCount != 1 {
		t.Fatalf("server after add = %q/%d", srv.LastStatus, srv.ToolCount)
	}
	if got := up.bearersOn("initialize"); len(got) != 1 || got[0] != "Bearer tok-alice-new" {
		t.Fatalf("initialize bearers = %v", got)
	}
	if got := f.tokens.freshens(); len(got) == 0 || got[0] != "linear/"+uAlice {
		t.Fatalf("freshen calls = %v", got)
	}
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice-new" {
		t.Fatalf("alice ran as %q", text(res))
	}
	for _, r := range up.requests() {
		if strings.Contains(r.bearer, "-old") {
			t.Fatalf("an expired bearer reached the upstream: %+v", r)
		}
	}

	// Bob: expired, no refresh token: refused with the reason, upstream untouched.
	before := len(up.requests())
	f.tokens.connect("linear", uBob, "tok-bob-old")
	f.tokens.expired["linear/"+uBob] = true
	res := f.call(t, agBob, "linear.get_whoami")
	if !res.IsError || !strings.Contains(text(res), "could not be used right now") || !strings.Contains(text(res), "#connections") {
		t.Fatalf("bob expired without refresh: isError=%v %q", res.IsError, text(res))
	}
	if len(up.requests()) != before {
		t.Fatal("refused call reached the upstream")
	}
}

// TestPerUserNoBearerFailsClosed: when the store says the person is
// connected but no bearer comes back for them (a read or decrypt failure,
// a flip between the check and the request), the call is refused before
// the upstream; no request goes out without the person's Authorization.
func TestPerUserNoBearerFailsClosed(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	f.tokens.empty["linear/"+uBob] = true

	before := len(up.requests())
	res := f.call(t, agBob, "linear.get_whoami")
	if !res.IsError || !strings.Contains(text(res), "#connections") {
		t.Fatalf("no bearer on file: isError=%v %q", res.IsError, text(res))
	}
	if len(up.requests()) != before {
		t.Fatalf("a request went out without bob's bearer: %+v", up.requests()[before:])
	}
	for _, r := range up.requests() {
		if r.bearer == "" && r.method != "" {
			t.Fatalf("a request without Authorization reached the upstream: %+v", r)
		}
	}
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice: %q", text(res))
	}
}

// TestPerUserConcurrentFirstCalls: two people's first calls land at the
// same time, under a tight per-user cap, and both complete (no two
// connection locks are ever nested during admission).
func TestPerUserConcurrentFirstCalls(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	// The tool list came over alice's session; drop it so both first
	// calls dial, and cap the per-user pool at one so admission evicts.
	f.svc.DropUserConnection("linear", uAlice)
	f.gw.SetMaxLivePerUser(1)

	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		errs := make(chan string, 2)
		for _, c := range []struct{ ag, want string }{{agAlice, "Bearer tok-alice"}, {agBob, "Bearer tok-bob"}} {
			wg.Add(1)
			go func(ag, want string) {
				defer wg.Done()
				res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), ag), "test", "linear.get_whoami",
					map[string]any{gateway.ReasonField: puReason})
				if err != nil || res.IsError || text(res) != want {
					errs <- ag + ": " + text(res)
				}
			}(c.ag, c.want)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		f.gw.SweepIdleStdioUpstreams(0)
	}
	if n := f.gw.PerUserLiveCount(); n > 1 {
		t.Fatalf("per-user live after cap 1 = %d", n)
	}
}

// TestPerUserPoolSeparateFromShared: per-user connections are admitted
// against their own cap and never evict a shared upstream, and a shared
// upstream never evicts a person's session.
func TestPerUserPoolSeparateFromShared(t *testing.T) {
	up := newBearerUpstream(t)
	shared := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.gw.SetMaxLiveUpstreams(1)
	f.gw.SetMaxLivePerUser(1)
	if _, err := f.svc.Add(context.Background(), upstreams.Server{Name: "bk", Transport: "http", URL: shared.srv.URL}); err != nil {
		t.Fatalf("add shared: %v", err)
	}
	f.add(t, "linear", up)

	f.call(t, agAlice, "linear.get_whoami")
	f.call(t, agBob, "linear.get_whoami")
	if live, pu := f.gw.LiveUpstreamCount(), f.gw.PerUserLiveCount(); live != 2 || pu != 1 {
		t.Fatalf("live=%d per-user live=%d; the shared upstream must stay live and only one person's session", live, pu)
	}
	if shared.count("initialize") != 1 {
		t.Fatalf("shared upstream was re-dialled (evicted): %d initializes", shared.count("initialize"))
	}
	// A shared call now does not evict the person's session either.
	if res := f.call(t, agAlice, "bk.get_whoami"); res.IsError {
		t.Fatalf("shared call: %q", text(res))
	}
	if pu := f.gw.PerUserLiveCount(); pu != 1 {
		t.Fatalf("per-user live after a shared call = %d", pu)
	}
	if f.gw.MaxLivePerUser() != 1 || f.gw.MaxLiveUpstreams() != 1 {
		t.Fatal("caps not reported")
	}
}

// TestPerUserDropReplacesConnectionUnderCall: dropping a person's
// connection while a call still holds it makes that call finish on the
// tracked replacement, and nothing lives on outside the pool.
func TestPerUserDropReplacesConnectionUnderCall(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	f.call(t, agBob, "linear.get_whoami")
	// Idle-suspend bob's connection, then drop it: the next call's first
	// pick may be the retired one (a race in production); it must still
	// complete, on a connection the pool tracks.
	f.gw.SweepIdleStdioUpstreams(0)
	f.svc.DropUserConnection("linear", uBob)
	if res := f.call(t, agBob, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-bob" {
		t.Fatalf("after drop: %q", text(res))
	}
	if live, pu := f.gw.LiveUpstreamCount(), f.gw.PerUserLiveCount(); live != 1 || pu != 1 {
		t.Fatalf("live=%d per-user live=%d after drop and call", live, pu)
	}
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 1 {
		t.Fatalf("sweep found %d live connections, want the one replacement", n)
	}
}

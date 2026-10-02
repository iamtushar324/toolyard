package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// TestPickEvictionCandidate_PerUserPool: per-user connections and shared
// upstreams are admitted against separate caps and never evict each other.
func TestPickEvictionCandidate_PerUserPool(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, perUser: map[string]*perUserGroup{}, maxLiveUpstreams: 1, maxLivePerUser: 1}
	now := time.Now().UnixNano()
	sharedLive := mkUpstream("bk", now-int64(time.Hour), true)
	g.upstreams["bk"] = sharedLive
	pu := &perUserGroup{cfg: UpstreamConfig{Name: "linear"}, pool: g, conns: map[string]*upstream{}}
	g.perUser["linear"] = pu
	alice := mkUpstream("linear", now-int64(time.Minute), true)
	alice.userID = "u_alice"
	pu.conns["u_alice"] = alice

	// A person's connection arriving: evicts the other person's (its
	// pool is full), never the much older shared upstream.
	bob := &upstream{cfg: UpstreamConfig{Name: "linear"}, userID: "u_bob", pool: g}
	victim, live, limit := g.pickEvictionCandidate(bob)
	if victim != alice || live != 1 || limit != 1 {
		t.Fatalf("per-user admission: victim=%v live=%d limit=%d", victim, live, limit)
	}
	// A shared upstream arriving: evicts the shared one, not a person.
	other := &upstream{cfg: UpstreamConfig{Name: "other"}, pool: g}
	victim, live, limit = g.pickEvictionCandidate(other)
	if victim != sharedLive || live != 1 || limit != 1 {
		t.Fatalf("shared admission: victim=%v live=%d limit=%d", victim, live, limit)
	}
	// An unbounded per-user pool never evicts.
	g.maxLivePerUser = 0
	if victim, _, _ := g.pickEvictionCandidate(bob); victim != nil {
		t.Fatalf("unbounded per-user pool evicted %v", victim.label())
	}
}

// TestRetiredConnectionNeverDials: a connection dropped from its group
// answers errRetired instead of dialling, and the group hands out a new,
// tracked one.
func TestRetiredConnectionNeverDials(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	pu := &perUserGroup{cfg: UpstreamConfig{Name: "bk", Transport: "http", URL: f.srv.URL}, conns: map[string]*upstream{}}
	c1 := pu.conn("u")
	pu.drop("u")
	if _, err := c1.callTool(context.Background(), "get_ok", nil); !errors.Is(err, errRetired) {
		t.Fatalf("retired connection dialled: err=%v", err)
	}
	if f.count("initialize") != 0 {
		t.Fatalf("retired connection sent %d initializes", f.count("initialize"))
	}
	c2 := pu.conn("u")
	if c2 == c1 {
		t.Fatal("drop did not replace the connection")
	}
	res, err := pu.callAs(context.Background(), "u", "get_ok", nil)
	if err != nil || resultText(res) != "ok" {
		t.Fatalf("callAs on the replacement: %v %v", err, res)
	}
	if all := pu.all(); len(all) != 1 || all[0] != c2 {
		t.Fatalf("group tracks %d connections, want the one replacement", len(all))
	}
	// A dial that completes after the connection was retired is closed,
	// not kept.
	c3 := pu.conn("v")
	c3.retire()
	if err := c3.resume(context.Background()); !errors.Is(err, errRetired) {
		t.Fatalf("resume on retired = %v", err)
	}
	if !c3.suspended() {
		t.Fatal("retired connection holds a client")
	}
}

// TestRemoveUpstreamDuringResume: a shared upstream removed while it is
// re-dialling (an idle server waking up) must not end up holding a client
// that nothing tracks: the dial finishes, sees the upstream retired and
// closes what it opened.
func TestRemoveUpstreamDuringResume(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	target, _ := url.Parse(f.srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	seen := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "initialize" {
			select {
			case seen <- struct{}{}:
			default:
			}
			<-release
		}
		proxy.ServeHTTP(w, r)
	}))
	defer gate.Close()

	g := &Gateway{upstreams: map[string]*upstream{}, perUser: map[string]*perUserGroup{},
		tools: map[string]toolEntry{}, mcp: server.NewMCPServer("t", "1")}
	u := &upstream{cfg: UpstreamConfig{Name: "bk", Transport: "http", URL: gate.URL}}
	g.upstreams["bk"] = u

	errc := make(chan error, 1)
	go func() { errc <- u.resume(context.Background()) }()
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("resume never reached the upstream")
	}
	if err := g.RemoveUpstream("bk"); err != nil {
		t.Fatalf("RemoveUpstream: %v", err)
	}
	close(release)
	select {
	case err := <-errc:
		if !errors.Is(err, errRetired) {
			t.Fatalf("resume after removal = %v, want errRetired", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("resume did not return")
	}
	if !u.suspended() {
		t.Fatal("removed upstream kept the client its in-flight dial opened")
	}
}

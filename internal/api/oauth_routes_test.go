package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// tokenCallCounter counts the requests that reach the provider's token
// endpoint, passing every request through.
type tokenCallCounter struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (c *tokenCallCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/token") {
		c.calls.Add(1)
	}
	return c.next.RoundTrip(r)
}

// A callback the browser delivers twice (a double redirect, a reload, a
// retry) exchanges its code with the provider once: the pending flow is
// claimed before the exchange, so the second request finds nothing.
func TestOAuthCallbackDeliveredTwiceExchangesOnce(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	e.seedServer(t, "shared", "ok", true)
	oe.client(t, "shared")
	counter := &tokenCallCounter{next: http.DefaultTransport}
	oe.oa.SetHTTPClient(&http.Client{Transport: counter})

	// Not browser-bound (as an operator-token start), so a callback with no
	// cookie may finish it.
	_, state, err := oe.oa.BeginCallback(context.Background(), "shared", e.admin.ID, oauth.ModeCallback, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	const n = 2
	recs := make([]*httptest.ResponseRecorder, n)
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			recs[i] = e.callbackCookies(t, state, "bot")
		}(i)
	}
	close(barrier)
	wg.Wait()

	ok, gone := 0, 0
	for i, rec := range recs {
		switch {
		case rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Authorized"):
			ok++
		case rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "expired or was already used"):
			gone++
		default:
			t.Fatalf("callback %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if ok != 1 || gone != 1 {
		t.Fatalf("callbacks: %d authorized, %d refused; want 1 and 1", ok, gone)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}
	var tokens int
	if err := e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE upstream_name = 'shared'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Fatalf("shared token rows = %d, want 1", tokens)
	}
}

// A paste-mode flow reaching the callback is refused as the wrong mode and
// stays pending for the paste route.
func TestOAuthCallbackWrongModeKeepsFlow(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	e.seedServer(t, "shared", "ok", true)
	oe.client(t, "shared")
	_, state, err := oe.oa.BeginCallback(context.Background(), "shared", e.admin.ID, oauth.ModePaste, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.callbackCookies(t, state, "bot")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Wrong mode") {
		t.Fatalf("paste flow on the callback: %d %s", rec.Code, rec.Body.String())
	}
	if p, err := oe.oa.LoadPending(context.Background(), state); err != nil || p.Mode != oauth.ModePaste {
		t.Fatalf("paste flow after the callback: %+v, %v", p, err)
	}
}

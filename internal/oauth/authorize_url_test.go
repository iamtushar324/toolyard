package oauth

import (
	"net/url"
	"testing"
)

// TestJoinAuthorizeURLNoDuplicates: parameters already in the stored
// endpoint (Bifrost keeps Google's with access_type and prompt) are
// replaced by ours, never sent twice; the rest of the endpoint is kept.
func TestJoinAuthorizeURLNoDuplicates(t *testing.T) {
	q := url.Values{}
	q.Set("client_id", "cid")
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	got, err := joinAuthorizeURL("https://accounts.google.com/o/oauth2/v2/auth?access_type=offline&prompt=consent&hd=beknown.work", q)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query()
	for _, k := range []string{"access_type", "prompt", "client_id"} {
		if len(v[k]) != 1 {
			t.Errorf("%s appears %d times in %s", k, len(v[k]), got)
		}
	}
	if v.Get("hd") != "beknown.work" || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Errorf("endpoint not kept: %s", got)
	}

	plain, err := joinAuthorizeURL("https://mcp.linear.app/authorize", q)
	if err != nil {
		t.Fatal(err)
	}
	if pu, _ := url.Parse(plain); pu.Query().Get("client_id") != "cid" || pu.Path != "/authorize" {
		t.Errorf("plain endpoint: %s", plain)
	}
	if _, err := joinAuthorizeURL("://bad", q); err == nil {
		t.Error("a bad endpoint must error")
	}
}

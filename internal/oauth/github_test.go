package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type githubRoundTrip func(*http.Request) (*http.Response, error)

func (f githubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubOAuthVerifiesAccountAndRefreshExpiry(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	if err := f.svc.PutClient(ctx, ClientRecord{UpstreamName: "linear", Issuer: "https://github.com", AuthorizationEndpoint: "https://github.com/login/oauth/authorize", TokenEndpoint: "https://github.com/login/oauth/access_token", ClientID: "Iv1.test", ClientSecret: "secret", RedirectURI: "https://toolyard.test/v1/mcp-oauth/callback", TokenEndpointAuthMethod: "client_secret_post", ExtraAuthorizeParams: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	reject := false
	f.svc.SetHTTPClient(&http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"access_token":"alice-token","refresh_token":"alice-refresh","token_type":"bearer","expires_in":28800,"refresh_token_expires_in":15811200}`
		status := 200
		if r.URL.Host == "api.github.com" {
			if r.Header.Get("Authorization") != "Bearer alice-token" {
				t.Fatal("wrong identity bearer")
			}
			body = `{"id":41,"login":"alice-gh"}`
			if reject {
				status = 401
				body = `{"message":"revoked"}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	rec, err := f.svc.ExchangeCodeForUser(ctx, "linear", "u_ada", "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AccountLabel != "alice-gh (#41)" || rec.RefreshExpiresAt.Before(time.Now().Add(180*24*time.Hour)) || rec.AccountVerified {
		t.Fatalf("incorrect GitHub metadata: label %s", rec.AccountLabel)
	}
	reject = true
	if _, err := f.svc.ExchangeCodeForUser(ctx, "linear", "u_bob", "code", "verifier"); err == nil {
		t.Fatal("unverified GitHub token stored")
	}
	if rec, err := f.svc.GetUserToken(ctx, "linear", "u_bob"); err == nil && rec != nil {
		t.Fatal("failed exchange created a token row")
	}
	var tr tokenResponse
	if err := json.Unmarshal([]byte(`{"refresh_token_expires_in":123}`), &tr); err != nil || tr.RefreshTokenExpiresIn != 123 {
		t.Fatal("GitHub refresh expiry not decoded")
	}
}

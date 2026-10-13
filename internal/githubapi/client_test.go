package githubapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubTokenCannotFollowRedirect(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" {
			t.Fatal("credential leaked across origin")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.test"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}, "secret")
	if err := c.Do(context.Background(), "GET", "/user", nil, &User{}); err == nil || calls != 1 {
		t.Fatalf("redirect followed: calls %d error %v", calls, err)
	}
}

func TestGitHubRepositoryPathRejectsTraversal(t *testing.T) {
	for _, part := range []string{"..", "o/r", "o?token=x", "o#secret", "https://evil.test"} {
		if _, err := RepoPath(part, "repo", 1); err == nil {
			t.Fatalf("unsafe owner accepted: %s", part)
		}
	}
}

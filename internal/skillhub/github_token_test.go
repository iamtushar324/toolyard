package skillhub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestGitHubConnectionsTokenSource(t *testing.T) {
	ctx := context.Background()
	tokens := map[string]string{
		"GitHubA/u_1": "tok-a1", "GitHubB/u_2": "tok-b2",
		"GitHubA/u_expired": "stale", "GitHubB/u_expired": "tok-b-expired",
		"GitHubA/u_blocked": "tok-blocked",
	}
	g := &GitHubConnections{
		Servers: func(context.Context) ([]string, error) { return []string{"GitHubA", "GitHubB"}, nil },
		Freshen: func(_ context.Context, server, uid string) error {
			if server == "GitHubA" && uid == "u_expired" {
				return errors.New("needs a new sign-in")
			}
			return nil
		},
		Token:  func(_ context.Context, server, uid string) (string, error) { return tokens[server+"/"+uid], nil },
		Active: func(_ context.Context, uid string) bool { return uid != "u_blocked" },
	}
	ts := g.TokenSource()
	for uid, want := range map[string]string{"u_1": "tok-a1", "u_2": "tok-b2", "u_expired": "tok-b-expired"} {
		if got, err := ts(ctx, uid); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", uid, got, err, want)
		}
	}
	for _, uid := range []string{"u_none", "u_blocked", ""} {
		if got, err := ts(ctx, uid); !errors.Is(err, ErrNoGitHubConnection) || got != "" {
			t.Errorf("%q: %q %v", uid, got, err)
		}
	}
	g.Servers = func(context.Context) ([]string, error) { return nil, errors.New("database is down") }
	if _, err := g.TokenSource()(ctx, "u_1"); err == nil {
		t.Error("a server list error was not reported")
	}
	var nilConns *GitHubConnections
	if _, err := nilConns.TokenSource()(ctx, "u_1"); !errors.Is(err, ErrNoGitHubConnection) {
		t.Errorf("nil connections: %v", err)
	}
}

func TestAuthGitHubSourceReadsWithTheCreatorsConnection(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "private-skills")
	gh.private = true
	gh.token = "conn-token-value"
	gh.commit("main", changeTestTree())
	svc := newTestService(t, gh)
	ctx := context.Background()

	// Without a token source, auth github is not available.
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/private-skills", Auth: AuthGitHub}); err == nil {
		t.Fatal("auth github worked without a token source")
	}
	if _, err := svc.InspectAs(ctx, "usr_admin", "acme/private-skills"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inspect without a token source: %v", err)
	}

	var mu sync.Mutex
	conn := map[string]string{"usr_admin": gh.token}
	svc.SetTokenSource((&GitHubConnections{
		Servers: func(context.Context) ([]string, error) { return []string{"GitHubForUsers"}, nil },
		Token: func(_ context.Context, _, uid string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return conn[uid], nil
		},
	}).TokenSource())

	info, err := svc.InspectAs(ctx, "usr_admin", "https://github.com/acme/private-skills/tree/main/skills")
	if err != nil || !info.Private || info.Branch != "main" || info.Path != "skills" {
		t.Fatalf("inspect with the connection: %+v %v", info, err)
	}
	if _, err := svc.InspectAs(ctx, "usr_member", "acme/private-skills"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no usable GitHub connection") {
		t.Fatalf("inspect without a connection: %v", err)
	}

	src, res := mustCreate(t, svc, SourceInput{URL: "acme/private-skills", Path: "skills", Auth: AuthGitHub, Mode: ModeAuto})
	if src.Auth != AuthGitHub || src.HasToken || !res.Activated || src.CreatedBy != "usr_admin" {
		t.Fatalf("source %+v result %+v", src, res)
	}
	if skills, err := svc.Search(ctx, adminViewer, SearchQuery{Text: "deploy"}); err != nil || len(skills) == 0 {
		t.Fatalf("search the synced source: %v %v", skills, err)
	}

	// Later syncs read with the creator's connection too; once it is gone
	// the sync fails without showing any token.
	gh.commit("main", map[string]string{"skills/deploy/old.md": "newer\n"})
	mu.Lock()
	delete(conn, "usr_admin")
	mu.Unlock()
	res2, err := svc.SyncNow(ctx, src.ID)
	if err == nil && (res2 == nil || res2.Error == "") {
		t.Fatalf("sync without the connection: %+v", res2)
	}
	if err != nil && strings.Contains(err.Error(), gh.token) {
		t.Fatal("the token is in the sync error")
	}
	if s, _ := svc.GetSource(ctx, src.ID); strings.Contains(s.LastError, gh.token) {
		t.Fatal("the token is in last_error")
	}
}

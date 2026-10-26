package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// accessSettings is a settings store the default token can be written to.
type accessSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (a *accessSettings) GetString(key, fallback string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.m[key]; ok {
		return v
	}
	return fallback
}

func (a *accessSettings) Set(_ context.Context, key string, value any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, _ := value.(string)
	a.m[key] = s
	return nil
}

const accessDefaultToken = "github_pat_DEFAULT_SENTINEL_0123456789"

func newAccessTest(t *testing.T, gh *fakeGitHub) (*Service, *accessSettings) {
	t.Helper()
	st := &accessSettings{m: map[string]string{}}
	gh.mu.Lock()
	gh.rateLimitTokens = map[string]bool{accessDefaultToken: true, "ghp_source": true}
	gh.mu.Unlock()
	return newTestService(t, gh, func(o *Options) { o.Settings = st }), st
}

// TestDefaultTokenReadsSourcesWithoutOne: with a default token set, a
// source without its own reads with it; the token is stored sealed and
// never in an answer; clearing it goes back to anonymous reads.
func TestDefaultTokenReadsSourcesWithoutOne(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc, st := newAccessTest(t, gh)
	ctx := context.Background()

	acc, err := svc.GitHubAccess(ctx)
	if err != nil || acc.Set || acc.Rate == nil || acc.Rate.Limit != 60 {
		t.Fatalf("before: %+v %v", acc, err)
	}
	acc, err = svc.SetGitHubToken(ctx, "usr_admin", "  "+accessDefaultToken+"  ")
	if err != nil || !acc.Set || acc.Rate == nil || acc.Rate.Limit != 5000 {
		t.Fatalf("set: %+v %v", acc, err)
	}
	stored := st.GetString(settings.SkillsGitHubToken, "")
	if stored == "" || strings.Contains(stored, "SENTINEL") {
		t.Fatalf("the default token is not stored sealed: %q", stored)
	}
	for _, v := range []any{acc} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "SENTINEL") {
			t.Fatalf("an answer carries the token: %s", b)
		}
	}
	if acc, _ := svc.GitHubAccess(ctx); !acc.Set || acc.Rate.Limit != 5000 {
		t.Fatalf("status after set: %+v", acc)
	}

	mark := gh.authMark()
	src, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if src.Auth != AuthNone || src.HasToken {
		t.Fatalf("the source got the default token as its own: %+v", src)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if info, err := svc.Inspect(ctx, "acme/skills", ""); err != nil || info.DefaultBranch != "main" {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	calls := gh.auths(mark)
	if len(calls) == 0 {
		t.Fatal("no API calls")
	}
	for _, a := range calls {
		if a != "Bearer "+accessDefaultToken {
			t.Fatalf("a call without the default token: %q in %q", a, calls)
		}
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if b, _ := json.Marshal(s); strings.Contains(string(b), "SENTINEL") {
		t.Fatalf("the source carries the token: %s", b)
	}

	if acc, err := svc.SetGitHubToken(ctx, "usr_admin", ""); err != nil || acc.Set {
		t.Fatalf("clear: %+v %v", acc, err)
	}
	mark = gh.authMark()
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range gh.auths(mark) {
		if a != "" {
			t.Fatalf("after clearing, a call still carries a token: %q", a)
		}
	}
}

// TestSourceTokenWinsOverDefault: a source's own token is used, never the
// default; a private repository still asks for a token on inspect.
func TestSourceTokenWinsOverDefault(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	gh.mu.Lock()
	gh.private, gh.token = true, "ghp_source"
	gh.mu.Unlock()
	svc, _ := newAccessTest(t, gh)
	ctx := context.Background()
	if _, err := svc.SetGitHubToken(ctx, "usr_admin", accessDefaultToken); err != nil {
		t.Fatal(err)
	}
	// The default token cannot read the private repository.
	if info, err := svc.Inspect(ctx, "acme/skills", ""); err != nil || !info.NeedsToken {
		t.Fatalf("inspect a private repository with the default token: %+v %v", info, err)
	}
	mark := gh.authMark()
	src, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, Token: "ghp_source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range gh.auths(mark) {
		if a != "Bearer ghp_source" {
			t.Fatalf("a call of a source with its own token used %q", a)
		}
	}
}

// TestSetGitHubTokenRefusals: a token GitHub rejects, or that is no token,
// is not stored; a settings store that cannot write refuses.
func TestSetGitHubTokenRefusals(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	svc, st := newAccessTest(t, gh)
	ctx := context.Background()
	for _, bad := range []string{"github_pat_rejected", "two words", strings.Repeat("x", 2000)} {
		if _, err := svc.SetGitHubToken(ctx, "usr_admin", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%.20q: %v, want ErrInvalid", bad, err)
		}
	}
	if v := st.GetString(settings.SkillsGitHubToken, ""); v != "" {
		t.Fatalf("a refused token was stored: %q", v)
	}
	ro := newTestService(t, gh) // staticSettings: read-only
	if _, err := ro.SetGitHubToken(ctx, "usr_admin", accessDefaultToken); err == nil {
		t.Fatal("a read-only settings store accepted a token")
	}
	if acc, err := ro.GitHubAccess(ctx); err != nil || acc.Set {
		t.Fatalf("read-only status: %+v %v", acc, err)
	}
}

// TestRateLimitPausesOnlySourcesWithoutToken: a rate limit on the
// anonymous credential pauses every source without its own token together
// (and is no failure of the source); a source with its own token goes on.
func TestRateLimitPausesOnlySourcesWithoutToken(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	anon, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	withToken, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Slug: "own-token", Mode: ModeAuto, Token: "ghp_b"})
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.retryAfter = 1200
	gh.mu.Unlock()
	_, err = svc.SyncNow(ctx, anon.ID)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limited sync: %v", err)
	}
	if until, ok := RateLimitedUntil(err); !ok || until.Before(time.Now().Add(19*time.Minute)) {
		t.Fatalf("RateLimitedUntil = %v %v", until, ok)
	}
	var failures int
	if err := svc.db.QueryRow(`SELECT failures FROM skill_sources WHERE id = ?`, anon.ID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	s, _ := svc.GetSource(ctx, anon.ID)
	if failures != 0 || s.NextCheckAt == nil || s.NextCheckAt.Before(time.Now().Add(19*time.Minute)) || !strings.Contains(s.LastError, "rate limited") {
		t.Fatalf("after the rate limit: failures %d next %v error %q", failures, s.NextCheckAt, s.LastError)
	}

	// Both due now: the poller checks the source with its own token only.
	if _, err := svc.db.Exec(`UPDATE skill_sources SET next_check_at = NULL`); err != nil {
		t.Fatal(err)
	}
	mark := gh.authMark()
	svc.pollDue(ctx)
	calls := gh.auths(mark)
	own := 0
	for _, a := range calls {
		switch a {
		case "":
			t.Fatalf("an anonymous call during the rate limit: %q", calls)
		case "Bearer ghp_b":
			own++
		}
	}
	if own == 0 {
		t.Fatalf("the source with its own token was not checked: %q", calls)
	}
	if s, _ := svc.GetSource(ctx, withToken.ID); s.LastCheckedAt == nil || s.LastError != "" {
		t.Fatalf("source with its own token: %+v", s)
	}
	// The panel shows the pause; reading the limit itself does not count.
	acc, err := svc.GitHubAccess(ctx)
	if err != nil || acc.PausedUntil == nil || acc.Rate == nil {
		t.Fatalf("status during the pause: %+v %v", acc, err)
	}
	// A rate limit error built for fakes matches too.
	fake := RateLimitError(time.Now().Add(time.Minute))
	if !errors.Is(fake, ErrRateLimited) {
		t.Fatal("RateLimitError does not match ErrRateLimited")
	}
	if errors.Is(errors.New("github: HTTP 500"), ErrRateLimited) {
		t.Fatal("any error matches ErrRateLimited")
	}
}

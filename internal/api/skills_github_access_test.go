package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

const skillsDefaultTokenSentinel = "github_pat_DEFAULT_SENTINEL_api_0123"

// GitHubAccess and SetGitHubToken make skillsAdminFake the hub's default
// token holder (skillsGitHubAccess). A token "bad…" is refused with an
// error that quotes it, as a careless implementation might.
func (f *skillsAdminFake) GitHubAccess(context.Context) (*skillhub.GitHubAccess, error) {
	if err := f.record("GitHubAccess"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &skillhub.GitHubAccess{Set: f.defaultToken != "", Rate: &skillhub.RateLimit{Limit: 60, Remaining: 3, Reset: time.Unix(1_900_000_000, 0).UTC()}}, nil
}

func (f *skillsAdminFake) SetGitHubToken(_ context.Context, by, token string) (*skillhub.GitHubAccess, error) {
	if err := f.record("SetGitHubToken"); err != nil {
		return nil, err
	}
	if strings.HasPrefix(token, "bad") {
		return nil, fmt.Errorf("%w: GitHub rejected %s", skillhub.ErrInvalid, token)
	}
	f.mu.Lock()
	f.defaultToken, f.lastBy = token, by
	f.mu.Unlock()
	return &skillhub.GitHubAccess{Set: token != "", Rate: &skillhub.RateLimit{Limit: 5000, Remaining: 4999}}, nil
}

// skillsAdminOnly hides the default-token methods: an admin service that
// has none.
type skillsAdminOnly struct{ skillhub.Admin }

func TestSkillsGitHubTokenRoute(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	rec := e.do(t, cookie, http.MethodGet, "/v1/skills/admin/github-token", "")
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["set"] != false || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var bodies []string
	rec = e.do(t, cookie, http.MethodPut, "/v1/skills/admin/github-token", `{"token":"`+skillsDefaultTokenSentinel+`"}`)
	bodies = append(bodies, rec.Body.String())
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["set"] != true || fake.defaultToken != skillsDefaultTokenSentinel || fake.lastBy != e.admin.ID {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, cookie, http.MethodGet, "/v1/skills/admin/github-token", "")
	bodies = append(bodies, rec.Body.String())
	if skillsJSON(t, rec)["set"] != true {
		t.Fatalf("get after put: %s", rec.Body.String())
	}
	// A refusal that quotes the token is redacted.
	rec = e.do(t, cookie, http.MethodPut, "/v1/skills/admin/github-token", `{"token":"bad_`+skillsDefaultTokenSentinel+`"}`)
	bodies = append(bodies, rec.Body.String())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad token: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, cookie, http.MethodPut, "/v1/skills/admin/github-token", `{"token":""}`)
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["set"] != false {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	for i, b := range bodies {
		if strings.Contains(b, "SENTINEL") {
			t.Errorf("response %d carries the token: %s", i, b)
		}
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE COALESCE(reason,'') LIKE ? OR COALESCE(result_summary,'') LIKE ?`, "%SENTINEL%", "%SENTINEL%").Scan(&n)
	if n != 0 {
		t.Fatalf("the token reached %d audit rows", n)
	}
	var set, cleared int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'skills.github_token_set'`).Scan(&set)
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'skills.github_token_clear'`).Scan(&cleared)
	if set != 1 || cleared != 1 {
		t.Fatalf("audit: %d set, %d cleared", set, cleared)
	}
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/admin/github-token", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
	// Members are refused; an admin service without a default token
	// answers 404; with the hub off, 404 too.
	m := e.member(t, "user_m", "m@beknown.work")
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodGet, "/v1/skills/admin/github-token", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member: %d", rec.Code)
	}
	e.srv.skillsAdmin = skillsAdminOnly{fake}
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/admin/github-token", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no default token support: %d", rec.Code)
	}
	e.srv.skillsAdmin = nil
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/admin/github-token", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("hub off: %d", rec.Code)
	}
	if scope, ok := operatorScopeFor(http.MethodPut, "/v1/skills/admin/github-token"); !ok || scope != "owner" {
		t.Fatalf("setting the default token needs the owner scope: %q %v", scope, ok)
	}
}

// TestSkillsRateLimitAnswers429: a GitHub rate limit is 429 with
// retry_after and reset_at on every admin route, never 500.
func TestSkillsRateLimitAnswers429(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	until := time.Now().Add(10 * time.Minute)
	fake.err = fmt.Errorf("skillhub: read acme/skills: %w", skillhub.RateLimitError(until))
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/skills"}`},
		{http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills"}`},
		{http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills"}`},
		{http.MethodPost, "/v1/skills/sources/src_1/sync", ""},
	} {
		rec := e.do(t, cookie, rt.method, rt.path, rt.body)
		got := skillsJSON(t, rec)
		secs, _ := got["retry_after"].(float64)
		if rec.Code != http.StatusTooManyRequests || got["code"] != "rate_limited" || secs < 590 || secs > 601 {
			t.Errorf("%s %s: %d %v", rt.method, rt.path, rec.Code, got)
			continue
		}
		if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra != int(secs) {
			t.Errorf("%s: Retry-After %q", rt.path, rec.Header().Get("Retry-After"))
		}
		if reset, err := time.Parse(time.RFC3339, got["reset_at"].(string)); err != nil || reset.Unix() != until.Unix() {
			t.Errorf("%s: reset_at %v (%v)", rt.path, got["reset_at"], err)
		}
		if msg, _ := got["error"].(string); !strings.Contains(msg, "GitHub access") {
			t.Errorf("%s: message %q", rt.path, msg)
		}
	}
}

// TestSkillsAgentRateLimitIs429: the agent REST mapping turns the tool's
// rate_limited refusal into 429 with its retry fields.
func TestSkillsAgentRateLimitIs429(t *testing.T) {
	res := mcp.NewToolResultError("GitHub's rate limit is reached")
	res.StructuredContent = map[string]any{"error": gateway.SkillsErrRateLimited, "message": "GitHub's rate limit is reached",
		"retry_after": 42, "reset_at": "2026-10-25T21:27:50Z"}
	rec := httptest.NewRecorder()
	writeSkillsResult(rec, res)
	got := skillsJSON(t, rec)
	if rec.Code != http.StatusTooManyRequests || got["code"] != "rate_limited" || got["retry_after"] != float64(42) ||
		got["reset_at"] != "2026-10-25T21:27:50Z" || rec.Header().Get("Retry-After") != "42" {
		t.Fatalf("%d %v %v", rec.Code, got, rec.Header())
	}
}

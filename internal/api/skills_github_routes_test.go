package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillsInspectAsFake adds InspectAs (the skills hub's Inspect with a
// person's GitHub connection) to the admin fake.
type skillsInspectAsFake struct {
	*skillsAdminFake
	by, url string
}

func (f *skillsInspectAsFake) InspectAs(_ context.Context, userID, rawURL string) (*skillhub.RepoInfo, error) {
	f.by, f.url = userID, rawURL
	return &skillhub.RepoInfo{RepoRef: skillhub.RepoRef{Owner: "acme", Repo: "private"}, Private: true,
		DefaultBranch: "main", Branches: []string{"main"}}, nil
}

func TestSkillsInspectWithGitHubConnection(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	body := `{"url":"acme/private","auth":"github"}`

	// A service without InspectAs refuses plainly.
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/inspect", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("without InspectAs: %d %s", rec.Code, rec.Body.String())
	}

	as := &skillsInspectAsFake{skillsAdminFake: fake}
	e.srv.skillsAdmin = as
	rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/inspect", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body.String())
	}
	got := skillsJSON(t, rec)
	if got["private"] != true || got["default_branch"] != "main" || as.by != e.admin.ID || as.url != "acme/private" {
		t.Fatalf("inspect body %v by %q url %q", got, as.by, as.url)
	}

	// A member never reaches it.
	m := e.member(t, "user_gh", "gh@beknown.work")
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodPost, "/v1/skills/sources/inspect", body); rec.Code != http.StatusForbidden {
		t.Fatalf("member: %d %s", rec.Code, rec.Body.String())
	}
}

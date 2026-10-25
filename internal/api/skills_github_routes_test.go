package api

import (
	"context"
	"net/http"
	"strings"
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

// skillsCreatorFake is the admin fake whose sources were added by creator.
type skillsCreatorFake struct {
	*skillsAdminFake
	creator string
}

func (f *skillsCreatorFake) GetSource(ctx context.Context, id string) (*skillhub.Source, error) {
	src, err := f.skillsAdminFake.GetSource(ctx, id)
	if src != nil {
		src.CreatedBy = f.creator
	}
	return src, err
}

// A person's GitHub connection is lent only by that person, in the
// dashboard: an operator token never creates, previews or switches a
// source to auth "github", and only the source's creator may switch it.
func TestSkillsGitHubConnectionNeedsItsOwner(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	ctx := context.Background()
	tok, _, err := e.id.CreateOperatorToken(ctx, e.admin.ID, "automation", []string{"owner"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/skills/sources", `{"url":"acme/private","auth":"github"}`},
		{http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/private","auth":"github"}`},
		{http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/private","auth":"github"}`},
		{http.MethodPatch, "/v1/skills/sources/src_1", `{"auth":"github"}`},
	} {
		rec := e.bearer(t, tok, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
			t.Errorf("operator %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	if fake.called("CreateSource") || fake.called("Preview") || fake.called("UpdateSource src_1") {
		t.Fatalf("an operator token reached the service: %v", fake.calls)
	}
	// Without auth github, the same token still creates a source.
	if rec := e.bearer(t, tok, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills"}`); rec.Code != http.StatusCreated {
		t.Fatalf("operator create without github: %d %s", rec.Code, rec.Body.String())
	}

	// In the dashboard: another admin cannot switch someone else's source
	// to their connection; the creator can.
	rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", `{"auth":"github"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "github_connection_creator_only") {
		t.Fatalf("non-creator switch: %d %s", rec.Code, rec.Body.String())
	}
	if fake.called("UpdateSource src_1") {
		t.Fatal("the refused switch reached the service")
	}
	e.srv.skillsAdmin = &skillsCreatorFake{skillsAdminFake: fake, creator: e.admin.ID}
	if rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", `{"auth":"github"}`); rec.Code != http.StatusOK {
		t.Fatalf("creator switch: %d %s", rec.Code, rec.Body.String())
	}
	// Other changes by any admin stay allowed.
	e.srv.skillsAdmin = fake
	if rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("other update: %d %s", rec.Code, rec.Body.String())
	}
}

package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillInspectorAs is the skills hub's Inspect with a person's toolyard
// GitHub connection (*skillhub.Service implements it; slice D).
type skillInspectorAs interface {
	InspectAs(ctx context.Context, userID, rawURL string) (*skillhub.RepoInfo, error)
}

// skillInspectWithConnection is POST /v1/skills/sources/inspect with
// {"auth": "github"}: the Add source wizard's "Use my GitHub connection"
// reads a private repository with the signed-in admin's (or skill
// contributor's) own toolyard GitHub connection. The token is used for
// that read only. An operator token never reaches a person's GitHub
// connection.
func (s *Server) skillInspectWithConnection(r *http.Request, u *identity.User, rawURL string) (*skillhub.RepoInfo, error) {
	if isOperatorRequest(r) {
		return nil, fmt.Errorf("%w: an operator token cannot read with a person's GitHub connection; use the Skills page", skillhub.ErrInvalid)
	}
	if u == nil {
		return nil, fmt.Errorf("%w: %v", skillhub.ErrInvalid, errSkillManager)
	}
	insp, ok := s.skillsAdmin.(skillInspectorAs)
	if !ok {
		return nil, fmt.Errorf("%w: reading with a GitHub connection is not available on this server", skillhub.ErrInvalid)
	}
	return insp.InspectAs(r.Context(), u.ID, rawURL)
}

// refuseGitHubConnectionUse answers 403 when an operator token asks for a
// source that reads with a person's GitHub connection (auth "github" on
// create or preview): only the person in the dashboard lends theirs.
func (s *Server) refuseGitHubConnectionUse(w http.ResponseWriter, r *http.Request, auth skillhub.AuthMode) bool {
	if auth != skillhub.AuthGitHub || !isOperatorRequest(r) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "an operator token cannot use a person's GitHub connection; choose \"Use my GitHub connection\" on the Skills page", "code": "github_connection_human_only"})
	return true
}

// refuseGitHubSwitch guards a source update that sets auth "github": the
// source then reads with its creator's GitHub connection, so only the
// creator, in the dashboard, may choose that. Another admin, or an
// operator token, is refused (403); they can give the source a token
// instead.
func (s *Server) refuseGitHubSwitch(w http.ResponseWriter, r *http.Request, u *identity.User, before *skillhub.Source, auth skillhub.AuthMode) bool {
	if auth != skillhub.AuthGitHub || before == nil {
		return false
	}
	if s.refuseGitHubConnectionUse(w, r, auth) {
		return true
	}
	if before.Auth == skillhub.AuthGitHub || before.CreatedBy == u.ID {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "only the person who added this source can make it read with their GitHub connection; give it a read-only token instead", "code": "github_connection_creator_only"})
	return true
}

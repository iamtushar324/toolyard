package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillInspectorAs is the skills hub's Inspect with a person's toolyard
// GitHub connection (*skillhub.Service implements it; slice D).
type skillInspectorAs interface {
	InspectAs(ctx context.Context, userID, rawURL string) (*skillhub.RepoInfo, error)
}

// skillInspectWithConnection is POST /v1/skills/sources/inspect with
// {"auth": "github"}: the Add source wizard's "Use my GitHub connection"
// reads a private repository with the signed-in admin's own toolyard
// GitHub connection. The token is used for that read only. An operator
// token never reaches a person's GitHub connection.
func (s *Server) skillInspectWithConnection(r *http.Request, rawURL string) (*skillhub.RepoInfo, error) {
	if isOperatorRequest(r) {
		return nil, fmt.Errorf("%w: an operator token cannot read with a person's GitHub connection; use the Skills page", skillhub.ErrInvalid)
	}
	u, err := s.requireAdmin(r)
	if err != nil {
		return nil, fmt.Errorf("%w: only a signed-in admin can read with their GitHub connection", skillhub.ErrInvalid)
	}
	insp, ok := s.skillsAdmin.(skillInspectorAs)
	if !ok {
		return nil, fmt.Errorf("%w: reading with a GitHub connection is not available on this server", skillhub.ErrInvalid)
	}
	return insp.InspectAs(r.Context(), u.ID, rawURL)
}

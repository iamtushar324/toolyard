package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// restProposals knows one proposal, skp_1, of the agent agentID.
type restProposals struct {
	mu      sync.Mutex
	agentID string
	viewers []skillhub.Viewer
}

func (p *restProposals) ProposeSource(context.Context, skillhub.Viewer, skillhub.SourceInput, string) (*skillhub.Proposal, error) {
	return nil, skillhub.ErrDisabled
}

func (p *restProposals) ProposeChange(context.Context, skillhub.Viewer, skillhub.ChangeInput) (*skillhub.Proposal, error) {
	return nil, skillhub.ErrDisabled
}

func (p *restProposals) GetProposal(_ context.Context, v skillhub.Viewer, id string) (*skillhub.Proposal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.viewers = append(p.viewers, v)
	if id != "skp_1" || v.AgentID != p.agentID {
		return nil, skillhub.ErrNotFound
	}
	return &skillhub.Proposal{ID: "skp_1", Kind: "source", Status: "pending", InboxID: "rq_1", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}, nil
}

// TestSkillsProposalRoute: GET /v1/skills/proposal?id= is skills.proposal
// for the calling agent (bearer only), like the other agent skills routes.
func TestSkillsProposalRoute(t *testing.T) {
	props := &restProposals{}
	e := newSkillsEnv(t, true, props)
	props.agentID = e.agentID
	rec := e.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1", e.member, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("own proposal: %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := decodeBody(t, rec)["proposal"].(map[string]any); p["id"] != "skp_1" || p["status"] != "pending" {
		t.Fatalf("body %s", rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1", e.admin, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another agent's proposal: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/proposal", e.member, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no id: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1&x=1", e.member, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown parameter: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1", e.opTok, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator token: %d %s", rec.Code, rec.Body.String())
	}
	if !skillsAgentPath("/v1/skills/proposal") {
		t.Fatal("/v1/skills/proposal must be an agent (bearer-only) route")
	}
	// Without proposals on the server the tool answers 503.
	off := newSkillsEnv(t, true)
	if rec := off.do(t, http.MethodGet, "/v1/skills/proposal?id=skp_1", off.member, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no proposals: %d %s", rec.Code, rec.Body.String())
	}
}

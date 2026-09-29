package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
)

// The agent polling its own approval must not receive the one-tap decision
// token: /v1/approvals/decide-by-token is unauthenticated, so an agent
// holding the token could approve its own call.
func TestCLIApprovalStatusOmitsDecisionToken(t *testing.T) {
	f := newInboxAPIFixture(t)
	req, err := f.srv.approval.Hold(context.Background(), approval.NewRequest{
		AgentID: f.agent, UpstreamName: "builtin", ToolName: "memory.set",
		Arguments: map[string]any{"key": "k"}, Reason: "store a value for the self-approval test",
		RequireHuman: true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := f.srv.approval.Get(context.Background(), req.ID)
	if err != nil || full.DecisionToken == "" {
		t.Fatalf("setup: approval has no decision token (err %v)", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/agents/approvals/"+req.ID, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), full.DecisionToken) {
		t.Fatal("agent-facing approval status leaks the decision token")
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["decision_token"]; ok {
		t.Fatalf("decision_token field present: %v", out["decision_token"])
	}
	if out["status"] != "pending" || out["id"] != req.ID {
		t.Fatalf("status payload lost fields: %v", out)
	}
}

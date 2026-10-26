package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// An owner-scope operator token reaches POST /v1/inbox/{id}/decide, but it
// decides as itself (Via operator), and a HumanOnly system item (a GitHub
// write with the owner's own account) refuses it; the owner in the
// dashboard can still decide.
func TestInboxDecideOperatorTokenAndHumanOnlyItems(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	uid := f.ownerID(t)
	tok, _, err := identity.New(f.db).CreateOperatorToken(ctx, uid, "automation", []string{"owner"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := f.srv.RoleGuard(f.mux)
	var mu sync.Mutex
	calls := map[string]int{}
	handler := func(kind string) inbox.SystemHandler {
		return func(_ context.Context, r *inbox.Request) (string, error) {
			mu.Lock()
			calls[kind]++
			mu.Unlock()
			return "applied", nil
		}
	}
	f.svc.HandleSystem("test.github_write", handler("write"), inbox.SystemOptions{HumanOnly: true})
	f.svc.HandleSystem("test.notice", handler("notice"))
	item := func(kind, ref string) *inbox.Request {
		r, err := f.svc.CreateSystemItem(ctx, inbox.SystemItem{Kind: kind, Ref: ref, UserID: uid,
			Title: "Open a pull request", Summary: "An agent proposes a skill change", Message: "The change, with its diff.",
			Questions: []inbox.QuestionSpec{{ID: "decision", Type: inbox.QuestionSingleChoice, Prompt: "Open a pull request?",
				Options: []inbox.Option{{ID: "approve", Label: "Approve"}, {ID: "reject", Label: "Reject"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	answer := func(r *inbox.Request, sub string) map[string]any {
		return map[string]any{"action": "answer", "request_revision": r.Revision, "submission_id": sub,
			"answers": map[string]any{"decision": map[string]any{"value": "approve"}}}
	}
	operator := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	write := item("test.github_write", "g1")
	rec := operator("/v1/inbox/"+write.ID+"/decide", answer(write, "op-1"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_only") {
		t.Fatalf("operator answer on a HumanOnly item: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := f.svc.Get(ctx, write.ID); got.Status != inbox.StatusPending || calls["write"] != 0 {
		t.Fatalf("the refused answer was applied: %s, calls %v", got.Status, calls)
	}
	// The owner in the dashboard decides it.
	if code, out := f.owner(t, http.MethodPost, "/v1/inbox/"+write.ID+"/decide", answer(write, "dash-1")); code != http.StatusOK {
		t.Fatalf("dashboard answer: %d %v", code, out)
	}
	got, _ := f.svc.Get(ctx, write.ID)
	if got.Status != inbox.StatusAnswered || got.DeciderVia != actor.ViaDashboard || calls["write"] != 1 {
		t.Fatalf("after the dashboard answer: %s via %q calls %v", got.Status, got.DeciderVia, calls)
	}

	// Other items still take an operator answer, recorded as Via operator.
	notice := item("test.notice", "n1")
	if rec := operator("/v1/inbox/"+notice.ID+"/decide", answer(notice, "op-2")); rec.Code != http.StatusOK {
		t.Fatalf("operator answer on another item: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := f.svc.Get(ctx, notice.ID); got.DeciderVia != actor.ViaOperator || calls["notice"] != 1 {
		t.Fatalf("operator decision recorded via %q, calls %v", got.DeciderVia, calls)
	}
}

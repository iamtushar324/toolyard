package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

type githubAuth struct {
	connected map[string]bool
	tokens    map[string]string
}

func (a *githubAuth) ConnectedUsers(context.Context, string) ([]string, error) {
	return []string{"alice", "bob"}, nil
}
func (a *githubAuth) UserConnected(_ context.Context, _, uid string) (bool, error) {
	return a.connected[uid], nil
}
func (*githubAuth) FreshenUserToken(context.Context, string, string) error { return nil }
func (*githubAuth) RefreshUserToken(context.Context, string, string) error { return nil }
func (a *githubAuth) MarkUserUnauthorized(_ context.Context, _, uid, _ string) {
	a.connected[uid] = false
}

type githubTransport struct {
	mu      sync.Mutex
	head    string
	posts   int
	tokens  []string
	rows    []map[string]any
	revoked bool
}

func (h *githubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.URL.Host != "api.github.com" {
		panic("token sent outside GitHub")
	}
	token := r.Header.Get("Authorization")
	h.tokens = append(h.tokens, token)
	code := 200
	var data any = map[string]any{"state": "open", "head": map[string]string{"sha": h.head}}
	id := int64(41)
	login := "alice-gh"
	if token == "Bearer bob-token" {
		id = 42
		login = "bob-gh"
	}
	switch {
	case h.revoked:
		code = 401
		data = map[string]string{"message": "revoked"}
	case r.URL.Path == "/user":
		data = map[string]any{"id": id, "login": login}
	case r.Method == http.MethodPost:
		h.posts++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payload["id"], payload["user"], payload["html_url"] = h.posts, map[string]any{"id": id}, "https://github.com/o/r/pull/7#comment"
		h.rows = append(h.rows, payload)
		data = payload
		code = 201
	case strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/reviews"):
		data = h.rows
	}
	b, _ := json.Marshal(data)
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}

func githubFixture(t *testing.T) (*accessFixture, *githubAuth, *githubTransport, context.Context, context.Context) {
	t.Helper()
	f := newAccessFixture(t, nil)
	a := &githubAuth{connected: map[string]bool{"alice": true, "bob": true}, tokens: map[string]string{"alice": "alice-token", "bob": "bob-token"}}
	h := &githubTransport{head: "aaa"}
	f.gw.SetGitHubHTTPClient(&http.Client{Transport: h})
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "github", Transport: "github", URL: "https://api.github.com", PerUser: true, PerUserAuth: a, HeaderFunc: func(ctx context.Context) map[string]string {
		uid, _ := UpstreamUser(ctx)
		return map[string]string{"Authorization": "Bearer " + a.tokens[uid]}
	}}); err != nil {
		t.Fatal(err)
	}
	f.bus.SetExecutor(f.gw)
	alice := actor.WithRaiser(f.admin, actor.Raiser{CallerID: adminID, OwnerUserID: "alice"})
	bob := actor.WithRaiser(f.admin, actor.Raiser{CallerID: adminID, OwnerUserID: "bob"})
	return f, a, h, alice, bob
}

func githubCallArgs() map[string]any {
	return map[string]any{ReasonField: "Explain the proposed pull request feedback to its author", "owner": "o", "repo": "r", "pull_number": float64(7), "body": "Please add a regression test."}
}

func githubPending(t *testing.T, f *accessFixture, ctx context.Context, tool string) *approval.Request {
	t.Helper()
	args := githubCallArgs()
	if tool == "submit_pull_request_review" {
		args["event"] = "REQUEST_CHANGES"
	}
	return githubPendingArgs(t, f, ctx, tool, args)
}

func githubPendingArgs(t *testing.T, f *accessFixture, ctx context.Context, tool string, args map[string]any) *approval.Request {
	t.Helper()
	res := f.call(t, ctx, "github."+tool, args)
	if res.IsError {
		t.Fatalf("call failed: %+v", res)
	}
	rows, err := f.bus.ListPending(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending=%+v err=%v", rows, err)
	}
	r, err := f.bus.Get(context.Background(), rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func githubDecide(t *testing.T, f *accessFixture, r *approval.Request) *approval.Request {
	t.Helper()
	if _, err := f.bus.DecideAs(context.Background(), r.ID, approval.StatusAllowed, actor.Decider{UserID: r.PersonalOwner(), Via: actor.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := f.bus.Get(context.Background(), r.ID)
		if err == nil && out.ResultExecutedAt > 0 {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("executor did not finish")
	return nil
}

func TestGitHubAlwaysAsksOwnerAndExecutesOnce(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	if _, err := f.gw.policy.Set(context.Background(), policy.ScopeUpstream, "github", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	f.gw.approvalMode = func() string { return ApprovalModeInbox }
	args := githubCallArgs()
	args[IntentField] = "read"
	args[GrantField] = "fake-grant"
	res := f.call(t, alice, "github.create_pull_request_comment", args)
	if res.IsError {
		t.Fatal(res)
	}
	rows, _ := f.bus.ListPending(context.Background())
	if len(rows) != 1 || h.posts != 0 {
		t.Fatalf("pending %d posts %d", len(rows), h.posts)
	}
	req, _ := f.bus.Get(context.Background(), rows[0].ID)
	if _, err := f.bus.DecideAs(context.Background(), req.ID, approval.StatusAllowed, actor.Decider{UserID: "bob", Via: actor.ViaDashboard}); err != approval.ErrWrongOwner {
		t.Fatalf("wrong owner: %v", err)
	}
	out := githubDecide(t, f, req)
	if out.ResultIsError || h.posts != 1 {
		t.Fatalf("result=%+v posts=%d", out, h.posts)
	}
	// A deferred result fetch cannot rerun the write.
	f.call(t, alice, "github.create_pull_request_comment", map[string]any{ApprovalIDField: req.ID})
	if h.posts != 1 {
		t.Fatal("resume duplicated write")
	}
	// Crash recovery reconciles the same persisted operation on GitHub.
	f.gw.Execute(context.Background(), out)
	if h.posts != 1 {
		t.Fatal("recovery duplicated write")
	}
}

func TestGitHubWritesFailAfterConnectionOrCommitChanges(t *testing.T) {
	for _, change := range []string{"disconnect", "token", "head", "revoked"} {
		t.Run(change, func(t *testing.T) {
			f, a, h, alice, _ := githubFixture(t)
			r := githubPending(t, f, alice, "create_pull_request_comment")
			switch change {
			case "disconnect":
				a.connected["alice"] = false
			case "token":
				a.tokens["alice"] = "another-token"
			case "head":
				h.head = "bbb"
			case "revoked":
				h.revoked = true
			}
			out := githubDecide(t, f, r)
			if !out.ResultIsError || h.posts != 0 {
				t.Fatalf("unsafe write: %+v posts=%d", out, h.posts)
			}
		})
	}
}

func TestGitHubReadsUseEachUsersToken(t *testing.T) {
	f, _, h, alice, bob := githubFixture(t)
	args := githubCallArgs()
	delete(args, "body")
	for _, ctx := range []context.Context{alice, bob} {
		res := f.call(t, ctx, "github.get_pull_request", args)
		if res.IsError {
			t.Fatal(res)
		}
	}
	if len(h.tokens) != 2 || h.tokens[0] != "Bearer alice-token" || h.tokens[1] != "Bearer bob-token" {
		t.Fatalf("tokens=%v", h.tokens)
	}
}

func TestGitHubReviewsRequireOwnerAndBindCommit(t *testing.T) {
	for _, event := range []string{"COMMENT", "REQUEST_CHANGES", "APPROVE"} {
		t.Run(event, func(t *testing.T) {
			f, _, h, alice, _ := githubFixture(t)
			if _, err := f.gw.policy.Set(context.Background(), policy.ScopeUpstream, "github", "allow", "", false); err != nil {
				t.Fatal(err)
			}
			f.gw.approvalMode = func() string { return ApprovalModeInbox }
			args := githubCallArgs()
			args["event"], args[IntentField], args[GrantField] = event, "read", "fake-grant"
			r := githubPendingArgs(t, f, alice, "submit_pull_request_review", args)
			if r.Status != approval.StatusPending || r.PersonalOwner() != "alice" || h.posts != 0 || r.Arguments["event"] != event || r.Arguments["body"] != args["body"] {
				t.Fatalf("review did not wait for its owner: %+v posts=%d", r, h.posts)
			}
			snapshot, _ := r.Arguments[approval.PersonalGitHubField].(map[string]any)
			if snapshot["head_sha"] != "aaa" || snapshot["github_login"] != "alice-gh" {
				t.Fatalf("approval lost its account or commit: %v", snapshot)
			}
			for _, d := range []actor.Decider{{UserID: "bob", Via: actor.ViaDashboard}, {UserID: "alice", Via: actor.ViaAutoRule}, {UserID: "alice", Via: actor.ViaInboxGrant}} {
				if _, err := f.bus.DecideAs(context.Background(), r.ID, approval.StatusAllowed, d); err != approval.ErrWrongOwner {
					t.Fatalf("unsafe review decider: %v", err)
				}
			}
			out := githubDecide(t, f, r)
			if out.ResultIsError || h.posts != 1 || h.rows[0]["commit_id"] != "aaa" || h.rows[0]["event"] != event || !strings.HasPrefix(h.rows[0]["body"].(string), args["body"].(string)) {
				t.Fatalf("bad review: %+v posts=%d", out, h.posts)
			}
			f.call(t, alice, "github.submit_pull_request_review", map[string]any{ApprovalIDField: r.ID})
			f.gw.Execute(context.Background(), out)
			if h.posts != 1 {
				t.Fatal("review result fetch or recovery duplicated the write")
			}
		})
	}
}

func TestGitHubReviewsRejectUnknownEventsAndForgedSnapshots(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	for _, event := range []any{"", "APPROVED", "DISMISS", nil} {
		args := githubCallArgs()
		args["event"] = event
		if res := f.call(t, alice, "github.submit_pull_request_review", args); !res.IsError {
			t.Fatalf("unsupported review event accepted: %v", event)
		}
	}
	args := githubCallArgs()
	args[approval.PersonalGitHubField] = map[string]any{"owner_user_id": "bob"}
	if res := f.call(t, alice, "github.create_pull_request_comment", args); !res.IsError {
		t.Fatal("forged snapshot accepted")
	}
	rows, _ := f.bus.ListPending(context.Background())
	if len(rows) != 0 || h.posts != 0 {
		t.Fatal("invalid review created a request or a write")
	}
}

func TestGitHubApproveStopsAfterConnectionOrCommitChanges(t *testing.T) {
	for _, change := range []string{"disconnect", "token", "head", "revoked"} {
		t.Run(change, func(t *testing.T) {
			f, a, h, alice, _ := githubFixture(t)
			args := githubCallArgs()
			args["event"] = "APPROVE"
			r := githubPendingArgs(t, f, alice, "submit_pull_request_review", args)
			switch change {
			case "disconnect":
				a.connected["alice"] = false
			case "token":
				a.tokens["alice"] = "another-token"
			case "head":
				h.head = "bbb"
			case "revoked":
				h.revoked = true
			}
			out := githubDecide(t, f, r)
			if !out.ResultIsError || h.posts != 0 {
				t.Fatalf("stale APPROVE review submitted: %+v posts=%d", out, h.posts)
			}
		})
	}
}

func TestGitHubApproveDeniedSendsNoReview(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	args := githubCallArgs()
	args["event"] = "APPROVE"
	r := githubPendingArgs(t, f, alice, "submit_pull_request_review", args)
	if _, err := f.bus.DecideAs(context.Background(), r.ID, approval.StatusDenied, actor.Decider{UserID: "alice", Via: actor.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	f.call(t, alice, "github.submit_pull_request_review", map[string]any{ApprovalIDField: r.ID})
	if h.posts != 0 {
		t.Fatal("denied APPROVE review submitted")
	}
}

func TestGitHubReviewSchemaAllowsAllEvents(t *testing.T) {
	f, _, _, _, _ := githubFixture(t)
	e, ok := f.gw.tools["github.submit_pull_request_review"]
	if !ok {
		t.Fatal("review tool is missing")
	}
	b, err := json.Marshal(e.tool.InputSchema.Properties["event"])
	if err != nil {
		t.Fatal(err)
	}
	var property struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(b, &property); err != nil {
		t.Fatal(err)
	}
	if strings.Join(property.Enum, ",") != "COMMENT,REQUEST_CHANGES,APPROVE" {
		t.Fatalf("review event schema: %s", b)
	}
	if !e.requireHuman || !e.personalGitHub {
		t.Fatal("review schema lost mandatory owner approval")
	}
}

func TestGitHubDeniedRequestSendsNoWrite(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	r := githubPending(t, f, alice, "create_pull_request_comment")
	if _, err := f.bus.DecideAs(context.Background(), r.ID, approval.StatusDenied, actor.Decider{UserID: "alice", Via: actor.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	f.call(t, alice, "github.create_pull_request_comment", map[string]any{ApprovalIDField: r.ID})
	if h.posts != 0 {
		t.Fatal("denied request executed")
	}
}

func TestGitHubResultsStayWithOriginalOwner(t *testing.T) {
	f, _, _, alice, bob := githubFixture(t)
	r := githubPending(t, f, alice, "create_pull_request_comment")
	// The same caller ID can acquire a different owner after account changes.
	// The approval's original owner still controls its contents and results.
	if f.gw.approvalVisible(bob, r) {
		t.Fatal("new owner can inspect former owner's request")
	}
	if res, _ := f.gw.handlePollApproval()(bob, map[string]any{"approval_id": r.ID}); !res.IsError {
		t.Fatal("poll leaked former owner's request")
	}
}

func TestGitHubInlineCommentBindsCommitAndLine(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	args := githubCallArgs()
	args["path"], args["line"], args["side"] = "src/main.go", float64(12), "RIGHT"
	if res := f.call(t, alice, "github.create_review_comment", args); res.IsError {
		t.Fatal(res)
	}
	rows, _ := f.bus.ListPending(context.Background())
	if len(rows) != 1 || h.posts != 0 {
		t.Fatal("inline comment did not wait")
	}
	r, _ := f.bus.Get(context.Background(), rows[0].ID)
	out := githubDecide(t, f, r)
	if out.ResultIsError || h.posts != 1 || h.rows[0]["commit_id"] != "aaa" || h.rows[0]["path"] != "src/main.go" || h.rows[0]["line"] != float64(12) {
		t.Fatal("inline comment lost its approved location")
	}
}

func TestGitHubApprovalCannotExpireWhileWaitingToWrite(t *testing.T) {
	f, _, h, alice, _ := githubFixture(t)
	f.bus.SetTTL(500 * time.Millisecond)
	r := githubPending(t, f, alice, "create_pull_request_comment")
	// Simulate another GitHub write occupying the serialized write path.
	f.gw.githubWriteMu.Lock()
	if _, err := f.bus.DecideAs(context.Background(), r.ID, approval.StatusAllowed, actor.Decider{UserID: "alice", Via: actor.ViaDashboard}); err != nil {
		f.gw.githubWriteMu.Unlock()
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	f.gw.githubWriteMu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := f.bus.Get(context.Background(), r.ID)
		if out.ResultExecutedAt > 0 {
			if !out.ResultIsError || h.posts != 0 {
				t.Fatal("expired queued write executed")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expired request did not finish")
}

func TestGitHubRestrictedReadsKeepResultsPrivate(t *testing.T) {
	f, _, _, alice, bob := githubFixture(t)
	if _, err := f.gw.policy.Set(context.Background(), policy.ScopeTool, "github.get_pull_request", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	f.gw.approvalMode = func() string { return ApprovalModeInbox }
	args := githubCallArgs()
	delete(args, "body")
	res := f.call(t, alice, "github.get_pull_request", args)
	if res.IsError {
		t.Fatal(res)
	}
	rows, _ := f.bus.ListPending(context.Background())
	if len(rows) != 1 || rows[0].PersonalOwner() != "alice" {
		t.Fatal("restricted read lost its owner")
	}
	r, _ := f.bus.Get(context.Background(), rows[0].ID)
	out := githubDecide(t, f, r)
	if out.ResultIsError || f.gw.approvalVisible(bob, out) {
		t.Fatal("restricted read exposed its result")
	}
}

package approval

import (
	"context"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"testing"
	"time"
)

func TestGitHubApprovalRejectsExpiryAndAutomaticDeciders(t *testing.T) {
	bus := newTestBus(t)
	bus.SetAutoApprover(alwaysAuto{})
	ctx := context.Background()
	req, err := bus.Hold(ctx, NewRequest{AgentID: "a", ToolName: "github.create_pull_request_comment", RequireHuman: true, Arguments: map[string]any{PersonalGitHubField: map[string]any{"owner_user_id": "alice"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != StatusPending {
		t.Fatal("automatic rule permitted a GitHub write")
	}
	for _, d := range []actor.Decider{{UserID: "bob", Via: actor.ViaDashboard}, {UserID: "alice", Via: actor.ViaAutoRule}, {UserID: "alice", Via: actor.ViaInboxGrant}, {Via: actor.ViaPushToken}} {
		if _, err := bus.DecideAs(ctx, req.ID, StatusAllowed, d); err != ErrWrongOwner {
			t.Fatalf("unsafe decider: %v", err)
		}
	}
	if _, err := bus.db.Exec(`UPDATE approval_requests SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.DecideAs(ctx, req.ID, StatusAllowed, actor.Decider{UserID: "alice", Via: actor.ViaDashboard}); err != ErrNotPending {
		t.Fatalf("expired request accepted: %v", err)
	}
}

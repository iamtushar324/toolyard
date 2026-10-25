package approval

import (
	"context"
	"testing"
)

// TestCancelBySystemCancelsPersonalApprovals: when toolyard revokes an
// agent (an OAuth grant), its pending approvals are cancelled even when
// they are personal (only their owner may decide them, so CancelByAgent
// refuses), and the row says the system cancelled it, not the agent.
func TestCancelBySystemCancelsPersonalApprovals(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	personal, err := bus.Hold(ctx, NewRequest{AgentID: "ag_app", ToolName: "github.create_pull_request_comment", RequireHuman: true,
		Arguments: map[string]any{PersonalGitHubField: map[string]any{"owner_user_id": "alice"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.CancelByAgent(ctx, personal.ID, "ag_app"); err != ErrWrongOwner {
		t.Fatalf("CancelByAgent on a personal approval: %v, want ErrWrongOwner", err)
	}
	if _, err := bus.CancelBySystem(ctx, personal.ID, "ag_other", "grant revoked"); err != ErrNotFound {
		t.Fatalf("another agent's approval: %v, want ErrNotFound", err)
	}
	got, err := bus.CancelBySystem(ctx, personal.ID, "ag_app", "grant revoked")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("status %q, want cancelled", got.Status)
	}
	var via, ref, by string
	if err := bus.db.QueryRow(`SELECT COALESCE(decided_via,''), COALESCE(decider_ref,''), COALESCE(decided_by,'') FROM approval_requests WHERE id = ?`, personal.ID).
		Scan(&via, &ref, &by); err != nil {
		t.Fatal(err)
	}
	if via != ViaSystem || ref != "grant revoked" || by != "system:grant revoked" {
		t.Fatalf("recorded as %q / %q / %q, want the system and the reason", via, ref, by)
	}
	if _, err := bus.CancelBySystem(ctx, personal.ID, "ag_app", "grant revoked"); err != ErrNotPending {
		t.Fatalf("second cancel: %v, want ErrNotPending", err)
	}
}

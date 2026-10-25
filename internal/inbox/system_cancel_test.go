package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCancelBySystem: toolyard cancels a revoked agent's pending request
// and says so; it is not recorded as withdrawn by the agent.
func TestCancelBySystem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")
	if _, err := e.svc.CancelBySystem(ctx, "ag_2", r.ID, "grant revoked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another agent's request: %v", err)
	}
	got, err := e.svc.CancelBySystem(ctx, "ag_1", r.ID, "grant revoked")
	if err != nil || got.Status != StatusCancelled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	var texts []string
	for _, a := range got.Activity {
		texts = append(texts, a.Text)
	}
	joined := strings.Join(texts, " | ")
	if !strings.Contains(joined, "Cancelled by toolyard: grant revoked") || strings.Contains(joined, "Withdrawn by the agent") {
		t.Fatalf("activity %q", joined)
	}
	if _, err := e.svc.CancelBySystem(ctx, "ag_1", r.ID, "grant revoked"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second cancel: %v", err)
	}
}

package approval

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

type fakeSink struct {
	mu     sync.Mutex
	subs   []string
	events []string
	fail   bool
}

func (f *fakeSink) SubscribeApproval(_ context.Context, req *Request, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, req.ID+"|"+ref)
	return nil
}

func (f *fakeSink) ApprovalEventTx(ctx context.Context, tx *sql.Tx, req *Request, eventType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("outbox unavailable")
	}
	// The event sees the row as the transaction has it.
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM approval_requests WHERE id=?`, req.ID).Scan(&status); err != nil {
		return err
	}
	f.events = append(f.events, eventType+"|"+req.Status+"|"+status+"|"+req.ExecutionState)
	return nil
}

// The bus subscribes the call's receiver when the approval is created and
// emits approval.decided and approval.executed inside the transactions that
// record them: a failed event write rolls the decision back.
func TestBusEmitsApprovalEventsInItsTransactions(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	sink := &fakeSink{fail: true}
	bus.SetEventSink(sink)
	req, err := bus.Hold(ctx, NewRequest{AgentID: "ag_1", UpstreamName: "github", ToolName: "github.merge_pull_request",
		Arguments: map[string]any{"number": 9}, Reason: "Merge the reviewed fix for the login bug", CallbackRef: "cb_hook"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.subs) != 1 || sink.subs[0] != req.ID+"|cb_hook" {
		t.Fatalf("subscriptions: %v", sink.subs)
	}
	if _, err = bus.Decide(ctx, req.ID, StatusAllowed, "user_owner"); err == nil {
		t.Fatal("a decision committed without its event")
	}
	if cur, _ := bus.Get(ctx, req.ID); cur.Status != StatusPending {
		t.Fatalf("a failed event left the approval %s", cur.Status)
	}
	sink.fail = false
	if _, err = bus.Decide(ctx, req.ID, StatusAllowed, "user_owner"); err != nil {
		t.Fatal(err)
	}
	if err = bus.SetResult(ctx, req.ID, `{"content":[]}`, true, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{EventApprovalDecided + "|allowed|allowed|", EventApprovalExecuted + "|allowed|allowed|failed"}
	if len(sink.events) != 2 || sink.events[0] != want[0] || sink.events[1] != want[1] {
		t.Fatalf("events %v, want %v", sink.events, want)
	}
	// A result recorded again is a no-op and emits nothing.
	if err = bus.SetResult(ctx, req.ID, `{"content":[]}`, false, ""); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("a repeated result emitted again: %v", sink.events)
	}
}

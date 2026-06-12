package chatnotify

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// fakeChannel records SendApproval / UpdateApproval calls.
type fakeChannel struct {
	mu      sync.Mutex
	ready   bool
	sends   map[string]MessageRef // approvalID -> ref
	updates map[string][]string   // approvalID -> texts
	nextID  int
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{ready: true, sends: map[string]MessageRef{}, updates: map[string][]string{}}
}

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Ready() bool  { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }

func (f *fakeChannel) SendApproval(ctx context.Context, approvalID, text string) (MessageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ref := MessageRef{ChatID: "100", MessageID: string(rune('0' + f.nextID))}
	f.sends[approvalID] = ref
	return ref, nil
}

func (f *fakeChannel) UpdateApproval(ctx context.Context, ref MessageRef, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// key by chat+message via reverse lookup of approval not available; store by ref message id
	f.updates[ref.MessageID] = append(f.updates[ref.MessageID], text)
	return nil
}

func (f *fakeChannel) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeChannel) updateCountFor(approvalID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := f.sends[approvalID]
	return len(f.updates[ref.MessageID])
}

func newTestRegistry(t *testing.T) (*Registry, *approval.Bus, *fakeChannel) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg := NewRegistry(db, bus, nil)
	fc := newFakeChannel()
	reg.Register(fc)
	bus.AddNotifier(reg)
	return reg, bus, fc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func holdAsync(bus *approval.Bus, in approval.NewRequest) {
	go func() { _, _ = bus.Hold(context.Background(), in, 0) }()
}

func TestRegistrySendsOnHold(t *testing.T) {
	_, bus, fc := newTestRegistry(t)
	holdAsync(bus, approval.NewRequest{
		AgentID: "ag1", UpstreamName: "github", ToolName: "create_issue", Reason: "file a bug",
	})
	waitFor(t, "send on hold", func() bool { return fc.sendCount() == 1 })
}

func TestRegistryDecideEditsMessage(t *testing.T) {
	_, bus, fc := newTestRegistry(t)
	holdAsync(bus, approval.NewRequest{
		AgentID: "ag1", UpstreamName: "github", ToolName: "create_issue", Reason: "x",
	})
	var id string
	waitFor(t, "pending visible", func() bool {
		pend, _ := bus.ListPending(context.Background())
		if len(pend) == 1 {
			id = pend[0].ID
			return true
		}
		return false
	})
	if _, err := bus.Decide(context.Background(), id, approval.StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	waitFor(t, "edit on decide", func() bool { return fc.updateCountFor(id) >= 1 })
}

func TestRegistryExpiryEditsMessage(t *testing.T) {
	reg, bus, fc := newTestRegistry(t)
	_ = reg
	bus.SetTTL(1 * time.Millisecond)
	holdAsync(bus, approval.NewRequest{
		AgentID: "ag1", UpstreamName: "github", ToolName: "create_issue", Reason: "x",
	})
	var id string
	waitFor(t, "send", func() bool {
		if fc.sendCount() == 1 {
			for k := range fc.sends {
				id = k
			}
			return true
		}
		return false
	})
	// Trigger expiry sweep.
	go bus.RunSweeper(context.Background(), 5*time.Millisecond)
	waitFor(t, "edit on expiry", func() bool { return fc.updateCountFor(id) >= 1 })
}

func TestReconcilerSendsForOrphanPending(t *testing.T) {
	reg, bus, fc := newTestRegistry(t)
	// Make the channel unready so the create event does NOT send.
	fc.mu.Lock()
	fc.ready = false
	fc.mu.Unlock()
	holdAsync(bus, approval.NewRequest{
		AgentID: "ag1", UpstreamName: "github", ToolName: "create_issue", Reason: "x",
	})
	waitFor(t, "pending exists", func() bool {
		pend, _ := bus.ListPending(context.Background())
		return len(pend) == 1
	})
	if fc.sendCount() != 0 {
		t.Fatalf("expected no send while unready, got %d", fc.sendCount())
	}
	// Re-enable and reconcile → should send for the orphan pending.
	fc.mu.Lock()
	fc.ready = true
	fc.mu.Unlock()
	reg.reconcile(context.Background())
	if fc.sendCount() != 1 {
		t.Fatalf("reconcile should send for orphan pending, got %d", fc.sendCount())
	}
}

package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/events"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubEventsProvider records calls for argument-plumbing assertions.
type stubEventsProvider struct {
	briefSince  int64
	lastFilter  events.Filter
	ackedIDs    []string
	publishType string
	publishSumm string
	ensuredName string
}

func (s *stubEventsProvider) Brief(ctx context.Context, since int64) (string, error) {
	s.briefSince = since
	return "# brief", nil
}
func (s *stubEventsProvider) Query(ctx context.Context, f events.Filter) ([]events.Event, error) {
	s.lastFilter = f
	return []events.Event{{ID: "ev_1", SourceName: "x", Type: "t", Summary: "hi"}}, nil
}
func (s *stubEventsProvider) Get(ctx context.Context, id string) (*events.Event, error) {
	return &events.Event{ID: id, Summary: "got"}, nil
}
func (s *stubEventsProvider) Ack(ctx context.Context, ids []string, actor string) (int, error) {
	s.ackedIDs = ids
	return len(ids), nil
}
func (s *stubEventsProvider) EnsureAgentSource(ctx context.Context, agentName string) (*events.Source, error) {
	s.ensuredName = agentName
	return &events.Source{ID: "evs_1", Name: "agent:" + agentName, Kind: events.KindAgent}, nil
}
func (s *stubEventsProvider) Ingest(ctx context.Context, src *events.Source, in events.IngestInput) (*events.Event, bool, error) {
	s.publishType = in.Type
	s.publishSumm = in.Summary
	return &events.Event{ID: "ev_pub", Type: in.Type, Summary: in.Summary}, false, nil
}

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	gw := New(Options{
		Policy:   policy.New(db),
		Approval: bus,
		Audit:    audit.New(db),
		Hub:      realtime.NewHub(),
		Memory:   memory.New(db),
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	return gw
}

func TestRegisterEventsToolsForcedAllow(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubEventsProvider{}
	gw.RegisterEventsTools(stub)

	for _, name := range []string{"events.brief", "events.query", "events.get", "events.ack", "events.publish"} {
		gw.mu.RLock()
		entry, ok := gw.tools[name]
		gw.mu.RUnlock()
		if !ok {
			t.Errorf("%s not registered", name)
			continue
		}
		if entry.forcedAction == nil || *entry.forcedAction != policy.ActionAllow {
			t.Errorf("%s: want forcedAction=Allow, got %v", name, entry.forcedAction)
		}
		if entry.upstream != eventsUpstream {
			t.Errorf("%s: upstream = %q", name, entry.upstream)
		}
	}
}

func TestEventsToolArgPlumbing(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubEventsProvider{}
	gw.RegisterEventsTools(stub)
	ctx := WithAgentID(context.Background(), "codex-1")

	// brief with since_minutes → non-zero since.
	entry := gw.tools["events.brief"]
	if _, err := entry.handle(ctx, map[string]any{"since_minutes": 60}); err != nil {
		t.Fatalf("brief: %v", err)
	}
	if stub.briefSince == 0 {
		t.Error("since_minutes did not translate to a since timestamp")
	}

	// query plumbing.
	entry = gw.tools["events.query"]
	if _, err := entry.handle(ctx, map[string]any{"source": "ci", "type": "deploy", "unacked_only": true, "limit": 10}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if stub.lastFilter.SourceName != "ci" || stub.lastFilter.Type != "deploy" || !stub.lastFilter.UnackedOnly || stub.lastFilter.Limit != 10 {
		t.Fatalf("query filter not plumbed: %+v", stub.lastFilter)
	}

	// ack with ids list.
	entry = gw.tools["events.ack"]
	if _, err := entry.handle(ctx, map[string]any{"ids": []any{"ev_a", "ev_b"}}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if len(stub.ackedIDs) != 2 {
		t.Fatalf("ack ids = %v", stub.ackedIDs)
	}

	// publish uses agent id from context.
	entry = gw.tools["events.publish"]
	if _, err := entry.handle(ctx, map[string]any{"type": "handoff", "summary": "done with task"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if stub.ensuredName != "codex-1" || stub.publishType != "handoff" || stub.publishSumm != "done with task" {
		t.Fatalf("publish not plumbed: name=%q type=%q summ=%q", stub.ensuredName, stub.publishType, stub.publishSumm)
	}
}

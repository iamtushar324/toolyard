package realtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

type approvalLike struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Args  string `json:"args"`
}

type cardLike struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
}

func hubOn(t *testing.T, db *store.DB, id string) *Hub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
	})
	h := NewHub()
	h.RegisterType("approval", func(raw json.RawMessage) (any, error) {
		var a approvalLike
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return &a, nil
	})
	h.SetCluster(ctx, c)
	return h
}

func next(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(10 * time.Second):
		t.Fatal("no event from the other node")
		return Event{}
	}
}

// An event published on one node reaches the other node's streams, with
// its data rebuilt as the type a stream filter checks.
func TestHubFansOutAcrossNodes(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	hub1 := hubOn(t, db, "node-1")
	hub2 := hubOn(t, db, "node-2")
	stream := hub2.subscribe()
	defer hub2.unsubscribe(stream)
	local := hub1.subscribe()
	defer hub1.unsubscribe(local)

	// A typed event, larger than a NOTIFY payload.
	big := &approvalLike{ID: "ap_1", Owner: "u_1", Args: strings.Repeat("a", 9000)}
	hub1.Publish(Event{Type: "approval", Data: big})
	evt := next(t, stream)
	got, ok := evt.Data.(*approvalLike)
	if evt.Type != "approval" || !ok || got.ID != "ap_1" || got.Owner != "u_1" || len(got.Args) != 9000 {
		t.Fatalf("typed event arrived as %T %+v", evt.Data, evt)
	}
	if evt := next(t, local); evt.Data != any(big) {
		t.Fatal("the local stream did not get the original event")
	}

	// A map stays a map.
	hub1.Publish(Event{Type: "mcp_oauth_user_done", Data: map[string]any{"user_id": "u_2", "upstream": "github"}})
	evt = next(t, stream)
	m, ok := evt.Data.(map[string]any)
	if !ok || m["user_id"] != "u_2" {
		t.Fatalf("map event arrived as %T %+v", evt.Data, evt.Data)
	}

	// Any other struct arrives as raw JSON, so a filter treats it as it
	// treats an unknown struct (not as a map with a user_id).
	hub1.Publish(Event{Type: "inbox", Data: cardLike{ID: "rq_1", UserID: "u_3"}})
	evt = next(t, stream)
	raw, ok := evt.Data.(json.RawMessage)
	if !ok || !strings.Contains(string(raw), `"rq_1"`) {
		t.Fatalf("struct event arrived as %T %v", evt.Data, evt.Data)
	}

	// Nothing comes back to node 1 a second time.
	select {
	case evt := <-local:
		if evt.Type == "approval" {
			t.Fatalf("node 1 got its own event back: %+v", evt)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

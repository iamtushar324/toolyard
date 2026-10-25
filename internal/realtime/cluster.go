package realtime

// High availability (TEC-1630): a dashboard holds its event stream open on
// one replica, while the approval, audit or Inbox change it must see may
// happen on another. With a multi-node cluster every event published on
// one replica is sent to the others (cluster.PublishLarge, so an event
// that carries a large approval or audit row still fits) and each
// delivers it to its own streams.
//
// A stream's filter (ServeSSEFiltered) looks at the Go type of an event's
// data, so the data is rebuilt as the type it had: a map[string]any as a
// map, a type registered with RegisterType as that type, and anything
// else as raw JSON, which every filter treats as it treats an unknown
// struct.

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

const (
	// clusterTopic carries remoteEvent.
	clusterTopic = "realtime"
	// remoteQueue bounds the events waiting to go to other replicas; a
	// full queue drops (live events are best-effort, as for a slow
	// stream).
	remoteQueue = 256
)

// Kinds of remoteEvent data.
const (
	kindMap   = "map"
	kindTyped = "typed"
	kindRaw   = "raw"
)

// remoteEvent is an event as it travels between replicas.
type remoteEvent struct {
	Instance string          `json:"n"`
	Type     string          `json:"type"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
}

// Decoder rebuilds the data of an event type from its JSON.
type Decoder func(raw json.RawMessage) (any, error)

// remote is the hub's link to the other replicas.
type remote struct {
	c        *cluster.Cluster
	queue    chan Event
	mu       sync.RWMutex
	decoders map[string]Decoder
}

// SetCluster sends this hub's events to the other replicas and delivers
// theirs to this hub's streams until ctx ends. Register the typed events
// (RegisterType) before events flow. Without a multi-node cluster it does
// nothing.
func (h *Hub) SetCluster(ctx context.Context, c *cluster.Cluster) {
	if !c.MultiNode() {
		return
	}
	r := &remote{c: c, queue: make(chan Event, remoteQueue), decoders: map[string]Decoder{}}
	h.mu.Lock()
	if h.remote != nil {
		for t, d := range h.remote.decoders {
			r.decoders[t] = d
		}
	}
	h.remote = r
	h.mu.Unlock()
	c.Subscribe(clusterTopic, func(payload string) {
		if payload == cluster.Resync {
			return
		}
		if evt, ok := r.decode(payload); ok {
			h.publishLocal(evt)
		}
	})
	go r.run(ctx)
}

// RegisterType makes events of eventType arrive from other replicas with
// their data rebuilt by decode (for example as *approval.Request), so a
// stream filter that switches on the data's type sees what it would see
// for a local event. Safe to call before or after SetCluster.
func (h *Hub) RegisterType(eventType string, decode Decoder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.remote == nil {
		// Kept for SetCluster, which copies it.
		h.remote = &remote{decoders: map[string]Decoder{}}
	}
	h.remote.mu.Lock()
	h.remote.decoders[eventType] = decode
	h.remote.mu.Unlock()
}

// send queues one local event for the other replicas, without blocking.
func (r *remote) send(evt Event) {
	if r.c == nil {
		return // RegisterType ran, SetCluster did not
	}
	select {
	case r.queue <- evt:
	default:
	}
}

// run encodes and publishes queued events until ctx ends.
func (r *remote) run(ctx context.Context) {
	lg := logx.For("realtime")
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-r.queue:
			body, err := r.encode(evt)
			if err != nil {
				continue
			}
			if err := r.c.PublishLarge(ctx, clusterTopic, string(body)); err != nil && ctx.Err() == nil {
				lg.Warn("realtime: sending a live event to other nodes failed", "type", evt.Type, "err", err.Error())
			}
		}
	}
}

// encode wraps one event for the other replicas.
func (r *remote) encode(evt Event) ([]byte, error) {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		return nil, err
	}
	kind := kindRaw
	if _, ok := evt.Data.(map[string]any); ok {
		kind = kindMap
	} else {
		r.mu.RLock()
		_, typed := r.decoders[evt.Type]
		r.mu.RUnlock()
		if typed {
			kind = kindTyped
		}
	}
	return json.Marshal(remoteEvent{Instance: r.c.InstanceID(), Type: evt.Type, Kind: kind, Data: data})
}

// decode rebuilds an event another replica published. Our own come back
// on the channel too; they were delivered when they were published.
func (r *remote) decode(payload string) (Event, bool) {
	var m remoteEvent
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m.Instance == r.c.InstanceID() || m.Type == "" {
		return Event{}, false
	}
	evt := Event{Type: m.Type}
	switch m.Kind {
	case kindMap:
		var v map[string]any
		if err := json.Unmarshal(m.Data, &v); err != nil {
			return Event{}, false
		}
		evt.Data = v
	case kindTyped:
		r.mu.RLock()
		dec := r.decoders[m.Type]
		r.mu.RUnlock()
		if dec == nil {
			return Event{}, false
		}
		v, err := dec(m.Data)
		if err != nil {
			return Event{}, false
		}
		evt.Data = v
	default:
		evt.Data = m.Data
	}
	return evt, true
}

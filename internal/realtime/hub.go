// Package realtime is a tiny SSE fan-out hub. It is the dashboard's live feed
// of approval and audit events.
//
// We chose SSE only (no WebSocket) for v0.1: it streams text/event-stream over
// plain HTTP, so it works through every reverse proxy with no upgrade dance,
// and the dashboard only consumes events one-way.
package realtime

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event is what dashboard clients see.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type Hub struct {
	mu      sync.RWMutex
	clients map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: map[chan Event]struct{}{}}
}

// Publish fans an event out to all current subscribers (best-effort, drop on
// slow consumer).
func (h *Hub) Publish(evt Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (h *Hub) subscribe() chan Event {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// ServeSSE writes the SSE stream until the client closes the connection.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := h.subscribe()
	defer h.unsubscribe(ch)

	// initial open marker so the dashboard knows the stream is live
	_, _ = w.Write([]byte("event: open\ndata: {}\n\n"))
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// A real `event: ping` (not an SSE comment): comments are
			// invisible to EventSource, so the client can't use them for
			// staleness detection. A named event fires an addEventListener
			// handler, letting the dashboard's watchdog tell "alive but
			// quiet" from "dead socket".
			_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			body, err := json.Marshal(evt.Data)
			if err != nil {
				continue
			}
			_, _ = w.Write([]byte("event: " + evt.Type + "\ndata: "))
			_, _ = w.Write(body)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

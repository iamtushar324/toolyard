package voice

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// wsSink receives Gemini's 24 kHz mono int16 PCM and ships binary WebSocket
// frames to the browser unchanged. The browser AudioContext handles
// up-resampling to whatever the output device wants (LDAC over BTR11 -> 96 kHz;
// built-in speakers -> 48 kHz; etc.). Send must not block for long (it runs on
// the model receive loop), so we queue chunks to a writer goroutine and drop
// silently when that goroutine can't keep up.
type wsSink struct {
	conn   *websocket.Conn
	ctx    context.Context
	queue  chan []byte
	closed atomic.Bool
}

func newWSSink(ctx context.Context, conn *websocket.Conn) *wsSink {
	s := &wsSink{
		conn:  conn,
		ctx:   ctx,
		queue: make(chan []byte, 32),
	}
	go s.writeLoop()
	return s
}

// Send queues a chunk of 24 kHz mono int16 little-endian PCM for delivery
// to the browser as-is. Returns false only after Close.
func (s *wsSink) Send(chunk []byte) bool {
	if s.closed.Load() {
		return false
	}
	if len(chunk) == 0 {
		return true
	}
	// Copy so the caller's buffer can be reused while we hold it.
	buf := make([]byte, len(chunk))
	copy(buf, chunk)
	select {
	case s.queue <- buf:
		return true
	default:
		// Writer is behind. Dropping a frame is better than wedging the
		// model's receive loop. The client will hear a glitch; that's
		// preferable to the entire call freezing.
		return true
	}
}

// Interrupt signals barge-in: drop everything that's queued so the prior
// model turn doesn't bleed into the next one.
func (s *wsSink) Interrupt() {
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}

// Close drains in-flight writes and marks the sink dead. Idempotent.
func (s *wsSink) Close() {
	if s.closed.Swap(true) {
		return
	}
	close(s.queue)
}

func (s *wsSink) writeLoop() {
	for chunk := range s.queue {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		err := s.conn.Write(ctx, websocket.MessageBinary, chunk)
		cancel()
		if err != nil {
			// The connection is gone or stalled. Mark closed and drain.
			s.closed.Store(true)
			for range s.queue {
			}
			return
		}
	}
}

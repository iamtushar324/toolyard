package voice

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// errReadRateExceeded is surfaced on errCh when the inbound byte rate
// trips the read-rate cap in readLoop.
var errReadRateExceeded = errors.New("voice: inbound read rate exceeded")

// wsSource turns the binary side of a WebSocket into the mic channel consumed
// by the live Gemini client. Text frames are JSON control messages that
// surface on the control channel.
//
// The reader runs in a goroutine started by start(); it returns when the
// peer closes, ctx is cancelled, or a transport error fires. The mic
// channel is closed when the reader exits so the live session unwinds cleanly.
type wsSource struct {
	conn    *websocket.Conn
	mic     chan []byte
	control chan clientMsg
	errCh   chan error

	// muted, when set, makes readLoop drain inbound audio frames instead
	// of forwarding them to the mic channel — a true server-side mute, so
	// audio stops reaching Gemini even if the client keeps streaming.
	muted atomic.Bool
}

// SetMuted toggles server-side mic muting. The client-side mute alone
// can't be trusted to actually stop the upstream audio bill.
func (s *wsSource) SetMuted(m bool) { s.muted.Store(m) }

func newWSSource(conn *websocket.Conn) *wsSource {
	return &wsSource{
		conn:    conn,
		mic:     make(chan []byte, 64),
		control: make(chan clientMsg, 8),
		errCh:   make(chan error, 1),
	}
}

// start launches the read loop. Cancel ctx (or close the conn) to stop it.
func (s *wsSource) start(ctx context.Context) {
	go s.readLoop(ctx)
}

// Mic returns the read-only channel of 16 kHz mono int16 LE PCM chunks.
// Closed when the reader exits.
func (s *wsSource) Mic() <-chan []byte { return s.mic }

// Control returns inbound control messages from the browser. Closed when
// the reader exits.
func (s *wsSource) Control() <-chan clientMsg { return s.control }

// Err returns the first read-loop error (or nil) once Done is closed.
func (s *wsSource) Err() error {
	select {
	case e := <-s.errCh:
		return e
	default:
		return nil
	}
}

func (s *wsSource) readLoop(ctx context.Context) {
	defer close(s.mic)
	defer close(s.control)

	// Read-rate cap: a token bucket refilled at ~128 KiB/s with a 1 MiB
	// burst. Real 16 kHz mono int16 mic audio is ~32 KiB/s, so this is
	// ~4× headroom; a client (or a hijacked socket) that floods us far
	// past mic rate trips StatusPolicyViolation instead of pinning a CPU
	// core and ballooning memory.
	const (
		rateBytesPerSec = 128 * 1024
		burstBytes      = 1024 * 1024
	)
	tokens := float64(burstBytes)
	last := time.Now()

	for {
		mt, data, err := s.conn.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				select {
				case s.errCh <- err:
				default:
				}
			}
			return
		}

		// Refill then charge the bucket for this frame. Both binary and
		// text frames cost us to read, so both are metered.
		now := time.Now()
		tokens += now.Sub(last).Seconds() * rateBytesPerSec
		if tokens > burstBytes {
			tokens = burstBytes
		}
		last = now
		tokens -= float64(len(data))
		if tokens < 0 {
			select {
			case s.errCh <- errReadRateExceeded:
			default:
			}
			s.conn.Close(websocket.StatusPolicyViolation, "read rate exceeded")
			return
		}

		switch mt {
		case websocket.MessageBinary:
			if s.muted.Load() {
				// Muted: drain the frame without forwarding so audio
				// genuinely stops reaching Gemini.
				continue
			}
			// Copy is implicit — coder/websocket returns a fresh slice
			// per Read, so we can hand it to the channel directly.
			select {
			case s.mic <- data:
			case <-ctx.Done():
				return
			}
		case websocket.MessageText:
			var msg clientMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				// Ignore malformed control frames rather than killing
				// the call — the JS side may send debug payloads.
				continue
			}
			select {
			case s.control <- msg:
			case <-ctx.Done():
				return
			default:
				// Drop on overflow; control isn't critical.
			}
		}
	}
}

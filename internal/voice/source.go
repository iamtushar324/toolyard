package voice

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coder/websocket"
)

// wsSource turns the binary side of a WebSocket into the <-chan []byte
// that maestro.New expects on the mic side. Text frames are JSON control
// messages that surface on the control channel.
//
// The reader runs in a goroutine started by start(); it returns when the
// peer closes, ctx is cancelled, or a transport error fires. The mic
// channel is closed when the reader exits so the maestro session unwinds
// cleanly.
type wsSource struct {
	conn    *websocket.Conn
	mic     chan []byte
	control chan clientMsg
	errCh   chan error
}

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
		switch mt {
		case websocket.MessageBinary:
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

package voice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

func discardLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLiveSession scripts Receive() and records what the client sends.
type fakeLiveSession struct {
	mu        sync.Mutex
	recvQueue chan *genai.LiveServerMessage

	sentAudio     [][]byte
	toolResponses []genai.LiveToolResponseInput
	closed        bool
}

func newFakeLiveSession() *fakeLiveSession {
	return &fakeLiveSession{recvQueue: make(chan *genai.LiveServerMessage, 16)}
}

func (f *fakeLiveSession) SendRealtimeInput(in genai.LiveRealtimeInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Audio != nil {
		f.sentAudio = append(f.sentAudio, in.Audio.Data)
	}
	return nil
}

func (f *fakeLiveSession) SendToolResponse(in genai.LiveToolResponseInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolResponses = append(f.toolResponses, in)
	return nil
}

func (f *fakeLiveSession) Receive() (*genai.LiveServerMessage, error) {
	msg, ok := <-f.recvQueue
	if !ok {
		return nil, errors.New("session closed")
	}
	return msg, nil
}

func (f *fakeLiveSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.recvQueue)
	}
	return nil
}

// captureSink records played chunks and interrupts.
type captureSink struct {
	mu         sync.Mutex
	chunks     [][]byte
	interrupts int
}

func (c *captureSink) Send(chunk []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunks = append(c.chunks, chunk)
	return true
}

func (c *captureSink) Interrupt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interrupts++
}

// TestLiveClientToolDispatch drives one scripted session end-to-end: a mic
// chunk goes up, a tool call comes down and is dispatched, audio comes
// down and reaches the sink, then the mic closes and run() returns.
func TestLiveClientToolDispatch(t *testing.T) {
	sess := newFakeLiveSession()
	sink := &captureSink{}
	mic := make(chan []byte, 4)

	var dispatched []string
	var mu sync.Mutex
	var msgs []serverMsg

	lc := &liveClient{
		apiKey: "test",
		audio:  sink,
		mic:    mic,
		connector: func(ctx context.Context, cfg liveConnectorConfig) (liveSession, error) {
			if cfg.SystemInstruction == "" {
				t.Error("system instruction must be passed to the connector")
			}
			return sess, nil
		},
		dispatch: func(_ context.Context, name string, args map[string]any) (string, error) {
			dispatched = append(dispatched, name)
			return "tool output", nil
		},
		persona: "test persona",
		notify: func(m serverMsg) {
			mu.Lock()
			msgs = append(msgs, m)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mic <- []byte{1, 2, 3, 4}

	sess.recvQueue <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "fc1", Name: "memory.get", Args: map[string]any{"key": "k", "_reason": "test reason long enough"}},
			},
		},
	}
	sess.recvQueue <- &genai.LiveServerMessage{
		ServerContent: &genai.LiveServerContent{
			ModelTurn: &genai.Content{Parts: []*genai.Part{
				{InlineData: &genai.Blob{Data: []byte{9, 9}}},
			}},
			TurnComplete: true,
		},
	}

	done := make(chan error, 1)
	go func() { done <- lc.run(ctx) }()

	// Give the loops a beat to chew through the scripted messages, then
	// hang up by closing the mic.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		n := len(sess.toolResponses)
		sess.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(mic)

	select {
	case err := <-done:
		if !errors.Is(err, errMicClosed) {
			t.Fatalf("run returned %v, want errMicClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after mic close")
	}

	if len(dispatched) != 1 || dispatched[0] != "memory.get" {
		t.Errorf("dispatched = %v", dispatched)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.sentAudio) != 1 {
		t.Errorf("mic chunks sent = %d, want 1", len(sess.sentAudio))
	}
	if len(sess.toolResponses) != 1 {
		t.Fatalf("tool responses = %d, want 1", len(sess.toolResponses))
	}
	fr := sess.toolResponses[0].FunctionResponses
	if len(fr) != 1 || fr[0].Name != "memory.get" || fr[0].Response["output"] != "tool output" {
		t.Errorf("function response = %+v", fr)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.chunks) != 1 {
		t.Errorf("sink chunks = %d, want 1", len(sink.chunks))
	}

	// The browser should have seen a tool_call message and state changes.
	mu.Lock()
	defer mu.Unlock()
	var sawToolCall, sawSpeaking bool
	for _, m := range msgs {
		if m.Type == "tool_call" && m.Name == "memory.get" {
			sawToolCall = true
		}
		if m.Type == "state" && m.Value == stateSpeaking {
			sawSpeaking = true
		}
	}
	if !sawToolCall {
		t.Error("browser never saw a tool_call message")
	}
	if !sawSpeaking {
		t.Error("browser never saw the speaking state")
	}
}

// TestLiveClientDispatchError verifies a failing tool surfaces as an error
// response to Gemini rather than killing the session.
func TestLiveClientDispatchError(t *testing.T) {
	sess := newFakeLiveSession()
	lc := &liveClient{
		log:   discardLogger(t),
		audio: &captureSink{},
		dispatch: func(context.Context, string, map[string]any) (string, error) {
			return "", errors.New("denied by policy: nope")
		},
	}
	lc.handleToolCall(context.Background(), sess, &genai.LiveServerToolCall{
		FunctionCalls: []*genai.FunctionCall{{ID: "x", Name: "github.create_issue"}},
	})
	if len(sess.toolResponses) != 1 {
		t.Fatalf("tool responses = %d", len(sess.toolResponses))
	}
	resp := sess.toolResponses[0].FunctionResponses[0]
	if resp.Response["error"] != "denied by policy: nope" {
		t.Errorf("response = %+v", resp.Response)
	}
}

// TestLiveClientInterrupt verifies barge-in flushes the sink.
func TestLiveClientInterrupt(t *testing.T) {
	sink := &captureSink{}
	lc := &liveClient{log: discardLogger(t), audio: sink}
	lc.handleServerContent(&genai.LiveServerContent{Interrupted: true})
	if sink.interrupts != 1 {
		t.Errorf("interrupts = %d, want 1", sink.interrupts)
	}
}

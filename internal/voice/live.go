// Gemini Live client for voice calls: streams browser mic audio up,
// synthesized speech down, and dispatches the model's tool calls through
// the gateway's normal routing (policy → approval → audit) — the same
// pipeline every enrolled agent goes through.
//
// The tool surface is toolyard's own catalog; there is no external voice
// worker or tmux-backed agent session in this path.
package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"
)

// DefaultModel is the Gemini Live model used when the operator doesn't
// override it via -voice-model / $TOOLYARD_VOICE_MODEL.
const DefaultModel = "gemini-2.5-flash-preview-native-audio-dialog"

// liveSession is the subset of *genai.Session the client depends on.
// An interface so the recv/dispatch logic is unit-testable without a real
// WebSocket to Google.
type liveSession interface {
	SendRealtimeInput(genai.LiveRealtimeInput) error
	SendToolResponse(genai.LiveToolResponseInput) error
	Receive() (*genai.LiveServerMessage, error)
	Close() error
}

// liveConnector opens a session. Swappable so tests can return a fake.
type liveConnector func(ctx context.Context, cfg liveConnectorConfig) (liveSession, error)

type liveConnectorConfig struct {
	APIKey            string
	Model             string
	SystemInstruction string
	Tools             []*genai.Tool
}

// defaultConnector opens a real genai Live session.
func defaultConnector(ctx context.Context, cfg liveConnectorConfig) (liveSession, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("genai NewClient: %w", err)
	}
	lcc := &genai.LiveConnectConfig{
		ResponseModalities:       []genai.Modality{genai.ModalityAudio},
		Tools:                    cfg.Tools,
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
	}
	if cfg.SystemInstruction != "" {
		lcc.SystemInstruction = &genai.Content{
			Role:  "user",
			Parts: []*genai.Part{{Text: cfg.SystemInstruction}},
		}
	}
	sess, err := client.Live.Connect(ctx, cfg.Model, lcc)
	if err != nil {
		return nil, fmt.Errorf("live connect: %w", err)
	}
	return sess, nil
}

// audioSink is the playback side (implemented by wsSink).
type audioSink interface {
	Send(chunk []byte) bool
	Interrupt()
}

// toolDispatcher executes one named tool call and returns the textual
// result handed back to Gemini. Implemented by Service.dispatchTool, which
// routes through the gateway.
type toolDispatcher func(ctx context.Context, name string, args map[string]any) (string, error)

// liveClient orchestrates one call's Live session. On transient errors it
// reconnects with exponential backoff until ctx is cancelled.
type liveClient struct {
	apiKey     string
	model      string
	persona    string
	tools      []*genai.Tool
	dispatch   toolDispatcher
	audio      audioSink
	mic        <-chan []byte
	connector  liveConnector
	log        *slog.Logger
	maxBackoff time.Duration

	// notify pushes a control message to the browser (state changes,
	// transcripts, tool-call activity). Best-effort; may be nil.
	notify func(serverMsg)

	lastState string // last state pushed to the browser, to avoid spam
}

func (c *liveClient) run(ctx context.Context) error {
	if c.apiKey == "" {
		return errors.New("voice live: api key required")
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.connector == nil {
		c.connector = defaultConnector
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = 30 * time.Second
	}
	if c.log == nil {
		c.log = slog.Default()
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Mic channel gone means the browser hung up — nothing to
		// reconnect for.
		if errors.Is(err, errMicClosed) {
			return err
		}
		c.log.Warn("voice live session ended; reconnecting", "err", err, "backoff", backoff)
		c.setState(stateConnecting)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < c.maxBackoff {
			backoff *= 2
			if backoff > c.maxBackoff {
				backoff = c.maxBackoff
			}
		}
	}
}

var errMicClosed = errors.New("mic channel closed")

func (c *liveClient) runOnce(ctx context.Context) error {
	sess, err := c.connector(ctx, liveConnectorConfig{
		APIKey:            c.apiKey,
		Model:             c.model,
		SystemInstruction: c.persona,
		Tools:             c.tools,
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer sess.Close()
	c.log.Info("voice live session open", "model", c.model)
	c.setState(stateListening)

	errCh := make(chan error, 2)
	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()

	go func() { errCh <- c.sendLoop(sendCtx, sess) }()
	go func() { errCh <- c.recvLoop(ctx, sess) }()

	err = <-errCh
	cancelSend()
	// Close the session before draining: recvLoop may be parked inside
	// Receive(), and only a close unblocks it.
	sess.Close()
	<-errCh // drain the other loop so we don't leak it
	return err
}

func (c *liveClient) sendLoop(ctx context.Context, sess liveSession) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-c.mic:
			if !ok {
				return errMicClosed
			}
			input := genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					Data:     chunk,
					MIMEType: "audio/pcm;rate=16000",
				},
			}
			if err := sess.SendRealtimeInput(input); err != nil {
				return fmt.Errorf("SendRealtimeInput: %w", err)
			}
		}
	}
}

func (c *liveClient) recvLoop(ctx context.Context, sess liveSession) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, err := sess.Receive()
		if err != nil {
			return fmt.Errorf("Receive: %w", err)
		}
		if msg == nil {
			continue
		}
		c.handleMessage(ctx, sess, msg)
	}
}

func (c *liveClient) handleMessage(ctx context.Context, sess liveSession, msg *genai.LiveServerMessage) {
	if msg.SetupComplete != nil {
		c.log.Debug("voice live setup complete")
	}
	if sc := msg.ServerContent; sc != nil {
		c.handleServerContent(sc)
	}
	if tc := msg.ToolCall; tc != nil {
		c.handleToolCall(ctx, sess, tc)
	}
	if msg.GoAway != nil {
		c.log.Warn("voice live server requested goaway; will reconnect")
	}
}

func (c *liveClient) handleServerContent(sc *genai.LiveServerContent) {
	if sc.Interrupted {
		// Barge-in: flush queued playback so the previous turn doesn't
		// bleed into the next one.
		c.audio.Interrupt()
		c.setState(stateListening)
	}
	if sc.InputTranscription != nil && sc.InputTranscription.Text != "" {
		c.sendMsg(serverMsg{Type: "transcript", Role: "user", Text: strings.TrimSpace(sc.InputTranscription.Text)})
	}
	if sc.OutputTranscription != nil && sc.OutputTranscription.Text != "" {
		c.sendMsg(serverMsg{Type: "transcript", Role: "assistant", Text: strings.TrimSpace(sc.OutputTranscription.Text)})
	}
	if sc.ModelTurn != nil {
		for _, p := range sc.ModelTurn.Parts {
			if p.InlineData != nil && len(p.InlineData.Data) > 0 {
				c.setState(stateSpeaking)
				if !c.audio.Send(p.InlineData.Data) {
					c.log.Warn("voice audio sink closed; dropping chunk", "bytes", len(p.InlineData.Data))
				}
			}
		}
	}
	if sc.TurnComplete {
		c.setState(stateListening)
	}
}

func (c *liveClient) handleToolCall(ctx context.Context, sess liveSession, tc *genai.LiveServerToolCall) {
	c.setState(stateThinking)
	responses := make([]*genai.FunctionResponse, 0, len(tc.FunctionCalls))
	for _, fc := range tc.FunctionCalls {
		responses = append(responses, c.dispatchOne(ctx, fc))
	}
	if len(responses) == 0 {
		return
	}
	if err := sess.SendToolResponse(genai.LiveToolResponseInput{FunctionResponses: responses}); err != nil {
		c.log.Error("voice SendToolResponse", "err", err)
	}
}

func (c *liveClient) dispatchOne(ctx context.Context, fc *genai.FunctionCall) *genai.FunctionResponse {
	raw, _ := json.Marshal(fc.Args)
	c.log.Info("voice tool call", "tool", fc.Name, "args_bytes", len(raw))
	c.sendMsg(serverMsg{Type: "tool_call", Name: fc.Name, Args: string(raw)})

	out, err := c.dispatch(ctx, fc.Name, fc.Args)
	if err != nil {
		c.log.Warn("voice tool call failed", "tool", fc.Name, "err", err)
		return &genai.FunctionResponse{
			ID:       fc.ID,
			Name:     fc.Name,
			Response: map[string]any{"error": err.Error()},
		}
	}
	return &genai.FunctionResponse{
		ID:       fc.ID,
		Name:     fc.Name,
		Response: map[string]any{"output": out},
	}
}

// setState pushes a state transition to the browser, deduplicated.
func (c *liveClient) setState(state string) {
	if state == c.lastState {
		return
	}
	c.lastState = state
	c.sendMsg(serverMsg{Type: "state", Value: state})
}

func (c *liveClient) sendMsg(m serverMsg) {
	if c.notify != nil {
		c.notify(m)
	}
}

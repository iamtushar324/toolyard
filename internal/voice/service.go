// Package voice exposes the dashboard's "live call" feature: a WebSocket
// from the browser carrying mic audio in / synthesized audio out, with a
// Gemini Live session wired up server-side. The model gets toolyard's own
// system prompt plus the full gateway tool catalog — memory, approvals,
// lake, upstreams — dispatched through the same policy → approval → audit
// pipeline as any enrolled agent.
//
// One Service per gateway. One active call per user (extra attempts get a
// 409 from HandleWS so the dashboard can offer "hang up the other one").
// The Service is goroutine-safe.
package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

// ErrNoAPIKey is returned by HandleWS when the gateway hasn't been
// configured with a GEMINI_API_KEY. The api layer surfaces this as a 503
// so the dashboard can render a "set an API key" hint instead of crashing
// the open call.
var ErrNoAPIKey = errors.New("voice: GEMINI_API_KEY not configured")

// ErrAlreadyActive is returned by HandleWS when the user already has an
// active call. The api layer turns this into a 409 with the active call's
// id so the dashboard can show "hang up the other tab?".
type ErrAlreadyActive struct {
	CallID string
}

func (e *ErrAlreadyActive) Error() string {
	return fmt.Sprintf("voice: user already has active call %s", e.CallID)
}

// Config wires the Service. APIKey is required; everything else has a
// sensible default.
type Config struct {
	APIKey string
	Model  string // optional; falls back to DefaultModel
	// PersonaPath, if set, is read once per call and used as the system
	// instruction. Missing file → embedded default. Toolyard's wiring
	// layer seeds ~/.toolyard/voice/soul.md on first run.
	PersonaPath string
	// Tools, when set, exposes the gateway's catalog to the model. Calls
	// are routed through the gateway's normal policy → approval → audit
	// path under a per-call "voice:<user>" identity. Nil = no tools.
	Tools ToolBackend
	// Hub, when set, receives "voice.call.started" and "voice.call.ended"
	// events so other dashboard tabs see the call indicator.
	Hub *realtime.Hub
	// MaxCallDuration caps how long a single call may run before the
	// server hangs it up. Bounds the worst-case Gemini Live bill from a
	// forgotten/abandoned tab. <=0 falls back to defaultMaxCallDuration.
	// (No inactivity timer: the mic streams even during silence, so it
	// would never fire — a hard duration cap is the real cost bound.)
	MaxCallDuration time.Duration
	// Logger receives session events. Optional.
	Logger *slog.Logger
}

// defaultMaxCallDuration is the fallback hard cap on a single voice call.
const defaultMaxCallDuration = 60 * time.Minute

// Service owns the per-user active-call map and constructs Gemini Live
// sessions on demand.
type Service struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	calls map[string]*Call // userID → active call
}

// Call is the metadata for one in-flight session. The dashboard's
// /v1/voice/sessions endpoint serialises these via Snapshot.
type Call struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	StartedAt time.Time `json:"started_at"`

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a Service. Returns nil only on a nil-cfg call.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, log: log, calls: map[string]*Call{}}
}

// ActiveCall returns the user's current call, or nil. Safe to call
// concurrently.
func (s *Service) ActiveCall(userID string) *Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[userID]
}

// Snapshot returns a copy of every active call for the dashboard. Doesn't
// expose the cancel func or done channel — just the JSON-safe metadata.
func (s *Service) Snapshot() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, Call{ID: c.ID, UserID: c.UserID, StartedAt: c.StartedAt})
	}
	return out
}

// Hangup terminates the user's active call, if any. Returns true when a
// call was found and cancelled. The HandleWS goroutine will tear down the
// WebSocket and unregister the call on its way out.
func (s *Service) Hangup(userID string) bool {
	s.mu.Lock()
	c := s.calls[userID]
	s.mu.Unlock()
	if c == nil {
		return false
	}
	c.cancel()
	return true
}

// registerCall claims the user's call slot. Returns ErrAlreadyActive when
// another call is in flight.
func (s *Service) registerCall(userID string, c *Call) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.calls[userID]; existing != nil {
		return &ErrAlreadyActive{CallID: existing.ID}
	}
	s.calls[userID] = c
	return nil
}

func (s *Service) unregisterCall(userID, callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.calls[userID]; existing != nil && existing.ID == callID {
		delete(s.calls, userID)
	}
}

// HandleWS upgrades the request, starts a Gemini Live session, and blocks
// until either side closes. Caller has already authenticated userID. The
// caller MUST NOT write to w after this returns: the upgrade has taken
// it over (or, on the 409 path, the function returned an error and the
// caller writes the HTTP response itself).
func (s *Service) HandleWS(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) error {
	if s.cfg.APIKey == "" {
		return ErrNoAPIKey
	}

	// 409 if the user already has a call. Don't even start the upgrade —
	// the api layer needs to return a JSON response with the active call
	// ID so the dashboard can offer "hang up the other one".
	s.mu.Lock()
	if existing := s.calls[userID]; existing != nil {
		s.mu.Unlock()
		return &ErrAlreadyActive{CallID: existing.ID}
	}
	s.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// InsecureSkipVerify disables coder/websocket's built-in Origin
		// check, which compares the Origin header to the request's Host.
		// Reverse proxies (nginx-proxy-manager, Caddy, etc.) frequently
		// rewrite Host to the backend address while leaving Origin as
		// the public hostname — the mismatch makes the library reject
		// with 403 before we even see the upgrade. CSRF protection
		// for this endpoint comes from the same JWT session cookie
		// the rest of /v1/* relies on, with SameSite enforced on the
		// cookie itself; the Origin check would be redundant.
		InsecureSkipVerify: true,
		// Skip permessage-deflate; the payload is mostly already-compressed
		// PCM-encoded audio, so deflate just burns CPU.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return fmt.Errorf("websocket accept: %w", err)
	}
	// Bound the per-message size: an attacker shouldn't be able to push
	// a multi-megabyte text frame through us.
	conn.SetReadLimit(1 << 20) // 1 MiB

	call := &Call{
		ID:        uuid.NewString(),
		UserID:    userID,
		StartedAt: time.Now(),
		done:      make(chan struct{}),
	}
	callCtx, cancel := context.WithCancel(ctx)
	call.cancel = cancel
	defer cancel()
	defer close(call.done)

	if err := s.registerCall(userID, call); err != nil {
		// Race: a second tab opened a call between our pre-check and
		// the WS upgrade. Tell the browser and shut down.
		_ = conn.Write(callCtx, websocket.MessageText, mustJSON(serverMsg{
			Type: "hangup", Reason: "another call started",
		}))
		conn.Close(websocket.StatusPolicyViolation, "duplicate")
		return err
	}
	defer s.unregisterCall(userID, call.ID)

	if s.cfg.Hub != nil {
		s.cfg.Hub.Publish(realtime.Event{Type: "voice.call.started", Data: map[string]string{
			"call_id": call.ID, "user_id": userID,
		}})
		defer s.cfg.Hub.Publish(realtime.Event{Type: "voice.call.ended", Data: map[string]string{
			"call_id": call.ID, "user_id": userID,
		}})
	}

	s.log.Info("voice call started",
		"call_id", call.ID, "user_id", userID,
		"remote", r.RemoteAddr,
	)

	err = s.runCall(callCtx, conn, call)

	s.log.Info("voice call ended",
		"call_id", call.ID, "user_id", userID,
		"duration", time.Since(call.StartedAt).Round(time.Millisecond),
		"err", err,
	)

	// Best-effort close — already-closed sockets are a no-op.
	conn.Close(websocket.StatusNormalClosure, "bye")
	return err
}

// runCall sets up the source/sink, constructs the Gemini Live client, and
// blocks until either side exits.
func (s *Service) runCall(ctx context.Context, conn *websocket.Conn, call *Call) error {
	src := newWSSource(conn)
	src.start(ctx)

	sink := newWSSink(ctx, conn)
	defer sink.Close()

	// Tell the browser we're connecting before the (potentially slow) live
	// connect handshake fires.
	_ = conn.Write(ctx, websocket.MessageText, mustJSON(serverMsg{
		Type: "state", Value: stateConnecting, CallID: call.ID,
	}))

	// notify pushes control messages (state/transcript/tool_call) to the
	// browser. coder/websocket serialises concurrent writers internally,
	// so this is safe alongside the sink's binary writeLoop.
	notify := func(m serverMsg) {
		m.CallID = call.ID
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = conn.Write(wctx, websocket.MessageText, mustJSON(m))
	}

	lc := &liveClient{
		apiKey:  s.cfg.APIKey,
		model:   s.cfg.Model,
		persona: s.loadPersona() + toolProtocolAppendix,
		audio:   sink,
		mic:     src.Mic(),
		log:     s.log.With("call_id", call.ID),
		notify:  notify,
	}
	if s.cfg.Tools != nil {
		toolCtx := gateway.WithAgentID(ctx, "voice:"+call.UserID)
		catalog := s.cfg.Tools.Catalog()
		lc.tools = buildGenaiTools(catalog, s.log)
		lc.dispatch = func(_ context.Context, name string, args map[string]any) (string, error) {
			// Deliberately use toolCtx (the call's lifetime + identity),
			// not the recv-loop's ctx: both share the same cancellation
			// root, and toolCtx carries the agent identity.
			return dispatchTool(toolCtx, s.cfg.Tools, name, args)
		}
		s.log.Info("voice tools declared", "call_id", call.ID, "count", len(catalog))
	}

	// The live client runs on its own goroutine; we watch the control
	// channel on the main one so we can react to hangup / mute promptly.
	liveDone := make(chan error, 1)
	go func() { liveDone <- lc.run(ctx) }()

	// Hard cap on call duration — bounds the worst-case Gemini Live bill
	// from an abandoned tab.
	maxDur := s.cfg.MaxCallDuration
	if maxDur <= 0 {
		maxDur = defaultMaxCallDuration
	}
	maxTimer := time.NewTimer(maxDur)
	defer maxTimer.Stop()

	// State transitions (listening/thinking/speaking) and transcripts are
	// pushed by the live client itself via notify.

	// Music ducking is handled entirely on the client (audible pink-
	// noise anchor → OS audio session activation → cooperating apps
	// like Spotify/Music voluntarily pause). The server doesn't reach
	// across to another machine's media apps; this used to live here
	// via nowplaying-cli and was deleted in favor of the web-client
	// approach because the gateway might run on a different host than
	// where the music is playing.

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-liveDone:
			return err
		case <-maxTimer.C:
			s.log.Info("voice call hit max duration; hanging up",
				"call_id", call.ID, "max", maxDur)
			_ = conn.Write(ctx, websocket.MessageText, mustJSON(serverMsg{
				Type: "hangup", Reason: "max call duration reached", CallID: call.ID,
			}))
			return nil
		case msg, ok := <-src.Control():
			if !ok {
				// Reader exited → peer hung up.
				return src.Err()
			}
			switch msg.Type {
			case "hangup":
				return nil
			case "mute":
				// Server-side mute: stop forwarding mic frames to Gemini
				// (the client-side mute alone can't be trusted to halt
				// the upstream audio bill).
				src.SetMuted(msg.Mute)
				s.log.Debug("voice mute toggle", "call_id", call.ID, "mute", msg.Mute)
			}
		}
	}
}

// loadPersona reads the operator's soul.md, falling back to the embedded
// default. The tool-protocol appendix is composed in by the caller so an
// operator edit can't strip the call mechanics.
func (s *Service) loadPersona() string {
	if s.cfg.PersonaPath != "" {
		if data, err := os.ReadFile(s.cfg.PersonaPath); err == nil {
			return string(data)
		}
	}
	return DefaultPersona()
}

// SeedPersona writes the embedded default persona to PersonaPath if no
// file is there yet. Toolyard's wiring calls this once at startup so the
// operator has a file to edit later.
func (s *Service) SeedPersona() error {
	if s.cfg.PersonaPath == "" {
		return nil
	}
	if _, err := os.Stat(s.cfg.PersonaPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.PersonaPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.cfg.PersonaPath, []byte(DefaultPersona()), 0o644)
}

func mustJSON(v serverMsg) []byte {
	b, _ := json.Marshal(v) // small struct, can't fail
	return b
}

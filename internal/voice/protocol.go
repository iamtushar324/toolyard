// Package voice bridges browser-mic WebSocket connections to Gemini Live so the
// dashboard can run a live voice call without any local audio device on the
// server.
//
// Wire protocol over the single /v1/voice/ws WebSocket:
//
//   - Binary frames: raw little-endian int16 PCM, mono. The client sends
//     16 kHz frames (mic). The server sends 16 kHz frames (downsampled
//     from Gemini's 24 kHz output by the sink). Both directions move in
//     small chunks; we don't impose a chunk size, but ~20 ms (640 bytes)
//     is what the browser worklet emits.
//
//   - Text frames: JSON control messages. Direction is identified by the
//     "type" tag. Client → server: "start", "hangup", "mute". Server →
//     client: "state", "transcript", "tool_call", "error", "hangup".
//
// This file defines the JSON shapes only. Behavior lives in service.go /
// ws.go / source.go / sink.go.
package voice

// Inbound control messages from the browser.
type clientMsg struct {
	Type string `json:"type"`
	// Start payload.
	CWD     string `json:"cwd,omitempty"`
	Persona string `json:"persona,omitempty"`
	// Mute payload (true to mute mic, false to unmute).
	Mute bool `json:"mute,omitempty"`
}

// Outbound control messages to the browser.
type serverMsg struct {
	Type string `json:"type"`
	// "state"  → value: "connecting" | "listening" | "thinking" | "speaking"
	Value string `json:"value,omitempty"`
	// "transcript" → role + text
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
	// "tool_call" → name + args (raw JSON object, surfaced as a string so
	// the dashboard can pretty-print without us re-parsing here)
	Name string `json:"name,omitempty"`
	Args string `json:"args,omitempty"`
	// "error" / "hangup" → message / reason
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Optional call ID echoed back so the UI can tag the session.
	CallID string `json:"call_id,omitempty"`
}

// State strings used in serverMsg.Value. Centralised so the dashboard and
// server can't drift.
const (
	stateConnecting = "connecting"
	stateListening  = "listening"
	stateThinking   = "thinking"
	stateSpeaking   = "speaking"
)

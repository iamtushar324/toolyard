package codemode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Script languages. Starlark is the default everywhere: Bifrost clients
// and the bk-docs skills send Starlark and parse its output.
const (
	LanguageStarlark   = "starlark"
	LanguageJavaScript = "javascript"
)

// ParseLanguage reads a language name: "" or "starlark" (also "python"),
// "javascript" (also "js").
func ParseLanguage(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", LanguageStarlark, "python":
		return LanguageStarlark, nil
	case LanguageJavaScript, "js":
		return LanguageJavaScript, nil
	}
	return "", fmt.Errorf("unknown language %q: use \"starlark\" (the default) or \"javascript\"", s)
}

// Echo modes: how much of each tool's answer executeToolCode's text
// repeats as a "[TOOL] ..." line. Full is Bifrost's behaviour and the
// default; the call ledger is in every result either way.
const (
	EchoFull    = "full"
	EchoPreview = "preview"
	EchoNone    = "none"
)

// ParseEcho reads an echo mode; "" is full.
func ParseEcho(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", EchoFull:
		return EchoFull, nil
	case EchoPreview:
		return EchoPreview, nil
	case EchoNone:
		return EchoNone, nil
	}
	return "", fmt.Errorf("unknown echo %q: use \"full\" (the default), \"preview\" or \"none\"", s)
}

const (
	// echoPreviewBytes is how much of a tool's answer echo "preview"
	// shows.
	echoPreviewBytes = 500
	// echoErrorBytes caps a failed call's echo line under echo "preview"
	// and "none", and any echo line once tool echoes used their share.
	echoErrorBytes = 2 << 10
)

// NewRunID returns a code-mode run id for executeToolCode: "cm_" and 32
// hex characters. (A REST run is named by its own cr_ id.)
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("codemode: no randomness: %v", err))
	}
	return "cm_" + hex.EncodeToString(b[:])
}

type agentKeyCtx struct{}

// WithAgentKey names the caller a script runs for, for the per-caller
// script cap. Without it the runtime asks its Caller (an AgentKey
// method), and treats a caller it cannot name as one anonymous caller.
func WithAgentKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, agentKeyCtx{}, key)
}

// agentKeyer is implemented by a Caller that can name the ctx caller.
type agentKeyer interface {
	AgentKey(ctx context.Context) string
}

// agentKey is the caller ctx's script counts against.
func (r *Runtime) agentKey(ctx context.Context) string {
	if k, ok := ctx.Value(agentKeyCtx{}).(string); ok {
		return k
	}
	if ak, ok := r.caller.(agentKeyer); ok {
		return ak.AgentKey(ctx)
	}
	return ""
}

// Package actor carries who raised a tool call and who decided it, from the
// point toolyard learns each fact (token check, dashboard session, approval
// click) to every audit row, approval and metric that records it.
//
// It is a leaf package: no toolyard imports, so audit, approval, gateway,
// inbox and api can all depend on it.
package actor

import (
	"context"
	"strings"
	"unicode"
)

// Raiser is who raised a tool call.
type Raiser struct {
	// CallerID is the gateway caller: "ag_<uuid>", "dashboard:<uid>",
	// "voice:<uid>", or "" for a local unauthenticated MCP caller.
	CallerID  string `json:"caller_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	// AgentKind: agent | identity | dashboard | voice | cli | system.
	AgentKind   string `json:"agent_kind,omitempty"`
	OwnerUserID string `json:"owner_user_id,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	// MCPSessionID is the Mcp-Session-Id (empty under -stateless-mcp).
	MCPSessionID string `json:"mcp_session_id,omitempty"`
	// AgentSessionID is a toolyard agent session ("ses_…", session.start)
	// the call was tagged with and that belongs to the calling agent.
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// ClientSessionID is the client's own session id, e.g. a T3 thread id
	// from x-t3-session-id. Claimed is set when it did not come from a
	// trusted proxy, so it is the caller's unverified word.
	ClientSessionID      string `json:"client_session_id,omitempty"`
	ClientSessionClaimed bool   `json:"client_session_claimed,omitempty"`
	// ClientKind: t3 | claude_code | codex | cursor | opencode | cli | browser | unknown.
	ClientKind string `json:"client_kind,omitempty"`
	// ClientName is the MCP clientInfo "name/version".
	ClientName string `json:"client_name,omitempty"`
	ClientIP   string `json:"client_ip,omitempty"`
	// Via is the path into the gateway: direct | tools.execute | code_mode
	// | dashboard | cli | voice | auto-execute | internal.
	Via string `json:"via,omitempty"`
}

// Decider is who approved or denied a request, and how.
type Decider struct {
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
	Name   string `json:"name,omitempty"`
	Via    string `json:"via"`
	// Ref identifies the instrument: rule id, grant id, Telegram user id,
	// batch id, dashboard session id, passkey credential id.
	Ref string `json:"ref,omitempty"`
}

// Decider.Via values.
const (
	ViaDashboard      = "dashboard"
	ViaDashboardBatch = "dashboard_batch"
	ViaPushToken      = "push_token"
	ViaPasskey        = "passkey"
	ViaTelegram       = "telegram"
	ViaAutoRule       = "auto_rule"
	ViaPolicy         = "policy"
	ViaInboxGrant     = "inbox_grant"
	ViaAgentCancel    = "agent_cancel"
	ViaExpiry         = "expiry"
)

// LegacyAutoRulePrefix is the historical decided_by form of an
// auto-approval rule decision, "rule:<rule id>".
const LegacyAutoRulePrefix = "rule:"

// Legacy is the value for the historical free-text decided_by column. A
// person instrument (dashboard, dashboard_batch, push_token, passkey,
// telegram, inbox_grant) yields the user id when one is known, else
// "<via>:<ref>" or "<via>". A machine instrument (auto_rule, policy,
// agent_cancel, expiry) never yields a user id, so an auto-approval is
// not mistaken for a person's click: auto_rule keeps the historical
// "rule:<id>", the others are "<via>:<ref>" or "<via>".
func (d Decider) Legacy() string {
	switch d.Via {
	case ViaAutoRule:
		if d.Ref != "" {
			return LegacyAutoRulePrefix + d.Ref
		}
		return d.Via
	case ViaPolicy, ViaAgentCancel, ViaExpiry:
		return d.viaRef()
	}
	if d.UserID != "" {
		return d.UserID
	}
	return d.viaRef()
}

func (d Decider) viaRef() string {
	if d.Ref != "" {
		return d.Via + ":" + d.Ref
	}
	return d.Via
}

// IsZero reports whether no decision source is recorded.
func (d Decider) IsZero() bool { return d == Decider{} }

// Merge returns r with its empty fields filled from o.
func (r Raiser) Merge(o Raiser) Raiser {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&r.CallerID, o.CallerID)
	fill(&r.AgentName, o.AgentName)
	fill(&r.AgentKind, o.AgentKind)
	fill(&r.OwnerUserID, o.OwnerUserID)
	fill(&r.OwnerEmail, o.OwnerEmail)
	fill(&r.OwnerName, o.OwnerName)
	fill(&r.MCPSessionID, o.MCPSessionID)
	fill(&r.AgentSessionID, o.AgentSessionID)
	if r.ClientSessionID == "" {
		r.ClientSessionID, r.ClientSessionClaimed = o.ClientSessionID, o.ClientSessionClaimed
	}
	fill(&r.ClientKind, o.ClientKind)
	fill(&r.ClientName, o.ClientName)
	fill(&r.ClientIP, o.ClientIP)
	fill(&r.Via, o.Via)
	return r
}

type raiserKey struct{}

// WithRaiser returns ctx carrying r.
func WithRaiser(ctx context.Context, r Raiser) context.Context {
	return context.WithValue(ctx, raiserKey{}, r)
}

// RaiserFrom returns the Raiser set by WithRaiser.
func RaiserFrom(ctx context.Context) (Raiser, bool) {
	r, ok := ctx.Value(raiserKey{}).(Raiser)
	return r, ok
}

// MaxLen caps any single identity field.
const MaxLen = 128

// Clean makes a caller-supplied value safe to store and display: control
// characters dropped, surrounding space trimmed, capped at MaxLen runes.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if rs := []rune(s); len(rs) > MaxLen {
		s = string(rs[:MaxLen])
	}
	return s
}

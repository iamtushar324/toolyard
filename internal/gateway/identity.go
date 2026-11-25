package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// IdentityResolver returns the raw per-person identity key to forward to
// upstreams whose config sets IdentityHeader. callerID is the gateway caller
// (an agent id, "dashboard:<uid>" or "voice:<uid>"); the key belongs to the
// caller's dashboard user. *identitykeys.Service satisfies it.
type IdentityResolver interface {
	ForwardKey(ctx context.Context, callerID string) (string, error)
}

// ErrNoIdentityKey means the caller's user has no usable identity key, so a
// call to an identity-forwarding upstream is refused rather than made under
// the gateway's service identity.
var ErrNoIdentityKey = errors.New("no identity key for this user")

type forwardedKeyKey struct{}

// WithForwardedKey carries the caller's raw identity key to the upstream
// transport's per-request header function for one tool call.
func WithForwardedKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, forwardedKeyKey{}, key)
}

// ForwardedKey returns the key set by WithForwardedKey. Only tool calls to
// identity-forwarding upstreams carry one; initialize, tools/list, ping and
// reconnects never do.
func ForwardedKey(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(forwardedKeyKey{}).(string)
	return k, ok && k != ""
}

// GuestSafeMeta is the _meta key an upstream sets to true on a tool that a
// guest (someone outside the organisation, identity.RoleGuest) may call on
// an identity-forwarding upstream without a forwarded key (TEC-2060). The
// call then goes out under the upstream's shared credentials only.
const GuestSafeMeta = "beknown/guestSafe"

// GuestSafe reports whether the upstream marked t guest-safe: its _meta
// carries GuestSafeMeta with the JSON value true (nothing else counts).
// The marker stays on the catalog entry because wrapSchema copies the
// upstream's tool, _meta included.
func GuestSafe(t mcp.Tool) bool {
	if t.Meta == nil || t.Meta.AdditionalFields == nil {
		return false
	}
	v, ok := t.Meta.AdditionalFields[GuestSafeMeta].(bool)
	return ok && v
}

// refuseGuestNotOpen is the outcome of a guest's call to a tool of an
// identity-forwarding upstream that is not marked guest-safe: the upstream
// is never contacted, and the audit row and the metric say why.
func (g *Gateway) refuseGuestNotOpen(ctx context.Context, entry toolEntry, agentID, reason, approvalID string, ev *metrics.Event) *mcp.CallToolResult {
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallFailed,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Decision:      "refused",
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: "identity: tool not open to guests",
	})
	ev.Outcome = metrics.OutcomeError
	ev.ErrorClass = "identity"
	return mcp.NewToolResultError(fmt.Sprintf("%s is not open to guests: %s records who made each change, and guest accounts have no Beknown key. "+
		"Ask the person who invited you, or use a tool of %s that is open to guests.", entry.tool.Name, entry.upstream, entry.upstream))
}

package gateway

import (
	"context"
	"errors"
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

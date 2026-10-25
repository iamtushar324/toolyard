package gateway

import (
	"context"
	"strings"
)

// MERGE NOTE (slice E): this file is a byte-for-byte copy of the
// _callback_ref helpers that slice D adds in callbacks_tools.go. Delete
// this file when D's change is in the branch; nothing else changes.

// callbackRefArg is the reserved argument with which one call names the
// receiver (callbacks.register) that hears about it: approval.decided and
// approval.executed for a legacy deferred approval. Like _reason it is
// stripped before the tool sees it.
const callbackRefArg = "_callback_ref"

type callbackRefKey struct{}

// withCallbackRef carries a call's _callback_ref to holdAndWait. Each call
// names its own: a nested call (code mode, tools.execute) without one does
// not inherit the outer call's.
func withCallbackRef(ctx context.Context, args map[string]any) context.Context {
	ref := strings.TrimSpace(stringArg(args, callbackRefArg))
	if ref == "" && callbackRefFrom(ctx) == "" {
		return ctx
	}
	return context.WithValue(ctx, callbackRefKey{}, ref)
}

// callbackRefFrom is the call's _callback_ref, or "".
func callbackRefFrom(ctx context.Context) string {
	ref, _ := ctx.Value(callbackRefKey{}).(string)
	return ref
}

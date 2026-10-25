package gateway

import (
	"context"
	"strings"
)

// MERGE NOTE (slice E): every declaration below is a byte-for-byte copy
// of slice D's callbacks_tools.go (PR #49 at c145e86). When D is in the
// branch, delete this file; the schema_wrap.go, server.go and
// codemode_tools.go lines that use these helpers are already identical to
// D's.

// callbackRefArg is the reserved argument with which one call names the
// receiver (callbacks.register) that hears about it: approval.decided and
// approval.executed for a legacy deferred approval. Like _reason it is
// stripped before the tool sees it, unless the tool declares a
// _callback_ref parameter of its own (ownCallbackRef): then the argument
// is the tool's, and toolyard neither reads nor strips it.
const callbackRefArg = "_callback_ref"

// maxCallbackRef bounds a _callback_ref (the same bound as callback_refs).
const maxCallbackRef = 512

const callbackRefPropDescription = "Optional: a callback_ref from callbacks.register. If this call waits for your owner's approval, that receiver gets approval.decided and approval.executed webhooks."

type callbackRefKey struct{}

// withCallbackRef carries a call's _callback_ref to holdAndWait. Each call
// names its own: a nested call (code mode, tools.execute) without one does
// not inherit the outer call's. A reference longer than maxCallbackRef is
// ignored (the deferred answer then has no callback).
func withCallbackRef(ctx context.Context, args map[string]any) context.Context {
	ref := strings.TrimSpace(stringArg(args, callbackRefArg))
	if len(ref) > maxCallbackRef {
		ref = ""
	}
	if ref == "" && callbackRefFrom(ctx) == "" {
		return ctx
	}
	return context.WithValue(ctx, callbackRefKey{}, ref)
}

// withCallbackRefFor is withCallbackRef for a call to e: a tool with its
// own _callback_ref parameter names no receiver.
func withCallbackRefFor(ctx context.Context, e toolEntry, args map[string]any) context.Context {
	if ownCallbackRef(e) {
		return withCallbackRef(ctx, nil)
	}
	return withCallbackRef(ctx, args)
}

// ownCallbackRef reports whether e declares a _callback_ref parameter of
// its own: one whose schema is not the one toolyard injects.
func ownCallbackRef(e toolEntry) bool {
	p, ok := e.tool.InputSchema.Properties[callbackRefArg].(map[string]any)
	if !ok {
		return false
	}
	d, _ := p["description"].(string)
	return d != callbackRefPropDescription
}

// restoreOwnCallbackRef puts back the _callback_ref argument that
// extractReasonOpt stripped when it belongs to the tool.
func restoreOwnCallbackRef(e toolEntry, args, clean map[string]any) map[string]any {
	if v, ok := args[callbackRefArg]; ok && clean != nil && ownCallbackRef(e) {
		clean[callbackRefArg] = v
	}
	return clean
}

// callbackRefFrom is the call's _callback_ref, or "".
func callbackRefFrom(ctx context.Context) string {
	ref, _ := ctx.Value(callbackRefKey{}).(string)
	return ref
}

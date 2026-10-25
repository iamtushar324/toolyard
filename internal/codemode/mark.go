package codemode

import "context"

// The script mark: every nested call a code-mode script makes carries it
// in its context, at any depth (tools.execute hops included), so the
// gateway can refuse to start code mode from inside a script and can tell
// a script's calls from an agent's own. Work that leaves the call's
// context, such as a job that runs a nested call later on a goroutine of
// its own, carries the mark over with CarryScriptMark; a context built
// with context.WithoutCancel keeps it already.

type scriptMarkKey struct{}

// WithinScript marks ctx as the context of a code-mode script's nested
// call.
func WithinScript(ctx context.Context) context.Context {
	return context.WithValue(ctx, scriptMarkKey{}, true)
}

// IsWithinScript reports a context WithinScript marked, or one that
// CarryScriptMark copied the mark to.
func IsWithinScript(ctx context.Context) bool {
	v, _ := ctx.Value(scriptMarkKey{}).(bool)
	return v
}

// CarryScriptMark returns dst marked as src is: dst itself when src has no
// mark. Use it when a nested call's work continues on a context that does
// not derive from the call's (a background job), so a script still cannot
// reach code mode through it.
func CarryScriptMark(dst, src context.Context) context.Context {
	if IsWithinScript(src) && !IsWithinScript(dst) {
		return WithinScript(dst)
	}
	return dst
}

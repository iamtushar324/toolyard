package audit

import "context"

type runIDKey struct{}

// WithRunID tags every audit row written with ctx (Write fills
// Event.RunID from it) with a code-mode run: executeToolCode's "cm_" id or
// a REST run's "cr_" id, so the outer call and every nested call of one
// script share it.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

// RunIDFrom returns the run id WithRunID put on ctx, or "".
func RunIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
}

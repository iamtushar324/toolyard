//go:build !(linux || darwin)

package codemode

import "context"

// withJSMemory leaves wazero's own memory allocation in place.
func withJSMemory(ctx context.Context) context.Context { return ctx }

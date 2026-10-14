// Package ops holds the process lifecycle state that liveness, readiness
// and metrics report, the probe handlers, and the private ops listener.
//
// The state is three flags, each set once and read on every probe:
//
//   - keys verified: every master key loaded and matched its canary in
//     the database (cmd/gateway, internal/keyguard);
//   - started: startup recovery and upstream loading are done;
//   - draining: shutdown began; new tool calls are refused while
//     in-flight work finishes.
//
// A process is ready only when its keys are verified, it has started and
// it is not draining (and its database answers; see Prober).
package ops

import "sync/atomic"

// State is the lifecycle state of one toolyard process. The zero value is
// "not started, keys not verified, not draining". Safe for concurrent use.
type State struct {
	draining     atomic.Bool
	started      atomic.Bool
	keysVerified atomic.Bool
}

// Default is the process-wide state the gateway, the probes and the
// metrics share.
var Default = &State{}

// SetDraining marks the start of the drain. It never clears.
func (s *State) SetDraining() { s.draining.Store(true) }

// Draining reports whether the drain has started.
func (s *State) Draining() bool { return s.draining.Load() }

// MarkStarted records that startup recovery and upstream loading are done.
func (s *State) MarkStarted() { s.started.Store(true) }

// Started reports whether MarkStarted was called.
func (s *State) Started() bool { return s.started.Load() }

// MarkKeysVerified records that every master key loaded and matched its
// canary.
func (s *State) MarkKeysVerified() { s.keysVerified.Store(true) }

// KeysVerified reports whether MarkKeysVerified was called.
func (s *State) KeysVerified() bool { return s.keysVerified.Load() }

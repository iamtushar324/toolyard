package jobs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Async modes: when a call to an upstream server's tool runs as a job.
const (
	// AsyncOff runs the call inline, as before async tools existed. On a
	// tool rule it switches the tool off when its server is async.
	AsyncOff = "off"
	// AsyncAlways hands the caller a job at once.
	AsyncAlways = "always"
	// AsyncAfter runs the call inline and hands back a job only when it is
	// still running after AfterMs milliseconds.
	AsyncAfter = "after"
)

// Bounds of an async setting.
const (
	MinAsyncAfterMs = 10
	MaxAsyncAfterMs = 10 * 60 * 1000
	// MaxAsyncTools caps the per-tool rules of one server.
	MaxAsyncTools = 500
	// maxAsyncToolName caps a tool name in a rule.
	maxAsyncToolName = 200
)

// ErrInvalidAsync wraps every validation error of an async setting.
var ErrInvalidAsync = errors.New("invalid async setting")

// AsyncRule is one effective rule: the mode, and the inline wait for
// AsyncAfter.
type AsyncRule struct {
	Mode    string `json:"mode"`
	AfterMs int    `json:"after_ms,omitempty"`
}

// Enabled reports whether the rule makes calls run as jobs.
func (r AsyncRule) Enabled() bool { return r.Mode == AsyncAlways || r.Mode == AsyncAfter }

// normalized spells an empty mode "off" and drops AfterMs unless the mode
// uses it.
func (r AsyncRule) normalized() AsyncRule {
	if r.Mode == "" {
		r.Mode = AsyncOff
	}
	if r.Mode != AsyncAfter {
		r.AfterMs = 0
	}
	return r
}

func (r AsyncRule) validate(where string) error {
	switch r.Mode {
	case "", AsyncOff, AsyncAlways:
		return nil
	case AsyncAfter:
		if r.AfterMs < MinAsyncAfterMs || r.AfterMs > MaxAsyncAfterMs {
			return fmt.Errorf("%w: %safter_ms must be between %d and %d for mode %q", ErrInvalidAsync, where, MinAsyncAfterMs, MaxAsyncAfterMs, AsyncAfter)
		}
		return nil
	}
	return fmt.Errorf("%w: %smode must be %q, %q or %q", ErrInvalidAsync, where, AsyncOff, AsyncAlways, AsyncAfter)
}

// AsyncSetting is an upstream server's async setting: a rule for the
// whole server and per-tool rules (keyed by the tool's own name on the
// server, without the "<server>." prefix) that override it.
type AsyncSetting struct {
	Mode    string               `json:"mode,omitempty"`
	AfterMs int                  `json:"after_ms,omitempty"`
	Tools   map[string]AsyncRule `json:"tools,omitempty"`
}

// ServerRule is the server-wide rule.
func (s AsyncSetting) ServerRule() AsyncRule {
	return AsyncRule{Mode: s.Mode, AfterMs: s.AfterMs}.normalized()
}

// RuleFor is the effective rule for one tool of the server: its own rule
// when it has one, else the server's.
func (s AsyncSetting) RuleFor(tool string) AsyncRule {
	if r, ok := s.Tools[tool]; ok {
		return r.normalized()
	}
	return s.ServerRule()
}

// IsZero reports a setting that makes nothing async and names no tool.
func (s AsyncSetting) IsZero() bool {
	return !s.ServerRule().Enabled() && len(s.Tools) == 0
}

// AnyEnabled reports whether some tool of the server may run as a job.
func (s AsyncSetting) AnyEnabled() bool {
	if s.ServerRule().Enabled() {
		return true
	}
	for _, r := range s.Tools {
		if r.normalized().Enabled() {
			return true
		}
	}
	return false
}

// Validate checks the modes, the inline waits and the tool names.
func (s AsyncSetting) Validate() error {
	if err := (AsyncRule{Mode: s.Mode, AfterMs: s.AfterMs}).validate(""); err != nil {
		return err
	}
	if len(s.Tools) > MaxAsyncTools {
		return fmt.Errorf("%w: at most %d tool rules", ErrInvalidAsync, MaxAsyncTools)
	}
	names := make([]string, 0, len(s.Tools))
	for name := range s.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" || len(name) > maxAsyncToolName || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\n\r\t") {
			return fmt.Errorf("%w: tool name %q is not valid", ErrInvalidAsync, name)
		}
		if err := s.Tools[name].validate("tools." + name + ": "); err != nil {
			return err
		}
	}
	return nil
}

// Normalized is the setting as it is stored and shown: every mode spelt
// out, after_ms only where it applies.
func (s AsyncSetting) Normalized() AsyncSetting {
	r := s.ServerRule()
	out := AsyncSetting{Mode: r.Mode, AfterMs: r.AfterMs}
	if len(s.Tools) > 0 {
		out.Tools = make(map[string]AsyncRule, len(s.Tools))
		for name, tr := range s.Tools {
			out.Tools[name] = tr.normalized()
		}
	}
	return out
}

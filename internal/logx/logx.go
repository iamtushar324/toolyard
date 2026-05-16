// Package logx is the toolyard structured-logging facade over log/slog.
//
// Goals:
//
//   - One JSON line per event in prod (parseable by `jq` from journalctl),
//     human-readable text in dev.
//   - Existing stdlib `log.Printf(...)` calls keep working — Bridge() routes
//     them through slog so we get a unified stream during the gradual
//     migration to structured calls.
//   - Component pattern: every subsystem gets its own logger via For("name")
//     so logs filter cleanly (`component=gateway`, `component=upstreams`).
//   - In-memory ring buffer (last N lines) so crash dumps can attach
//     recent context to a panic.
package logx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

var (
	defaultLogger atomic[*slog.Logger]
	ring          = newRing(200)
)

// Init configures the global logger. Safe to call once at process startup;
// later calls replace the default.
func Init(format Format, level slog.Level) *slog.Logger {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	out := io.MultiWriter(os.Stderr, ring) // ring buffer mirror for crash dumps
	switch format {
	case FormatText:
		handler = slog.NewTextHandler(out, opts)
	default:
		handler = slog.NewJSONHandler(out, opts)
	}
	lg := slog.New(handler)
	slog.SetDefault(lg)
	defaultLogger.Store(lg)
	return lg
}

// Default returns the configured logger, falling back to a JSON-stderr logger
// if Init() hasn't been called (handy in tests).
func Default() *slog.Logger {
	if lg := defaultLogger.Load(); lg != nil {
		return lg
	}
	return slog.Default()
}

// For returns a logger tagged with the given component name.
//
//	lg := logx.For("upstreams")
//	lg.Info("idle-kill suspend", "upstream", name, "idle", idle)
func For(component string) *slog.Logger {
	return Default().With("component", component)
}

// Bridge routes stdlib `log` package output through slog so legacy
// `log.Printf("...")` callers end up in the same stream during the
// migration. Each stdlib log line becomes a single slog Info event with
// component=legacy and the raw message in the "msg" field.
//
// Returns an io.Writer; callers wire it via log.SetOutput(bridgeWriter).
// We do NOT clear stdlib log flags — leave that to the caller so they can
// pick a sensible policy.
func Bridge() io.Writer {
	return &bridgeWriter{lg: Default().With("component", "legacy")}
}

type bridgeWriter struct {
	lg  *slog.Logger
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bridgeWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	for {
		i := bytes.IndexByte(b.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(b.buf.Next(i+1)), "\n")
		if line != "" {
			b.lg.LogAttrs(context.Background(), slog.LevelInfo, line)
		}
	}
	return len(p), nil
}

// RingSnapshot returns up to the last N log lines as a single string. Used
// by the crash dump writer to attach recent context to a panic.
func RingSnapshot() string { return ring.snapshot() }

// ---- ring buffer ------------------------------------------------------------

type ringBuf struct {
	mu    sync.Mutex
	lines []string
	max   int
	head  int
	full  bool
}

func newRing(max int) *ringBuf { return &ringBuf{lines: make([]string, max), max: max} }

func (r *ringBuf) Write(p []byte) (int, error) {
	// Each Write from a slog handler is one full JSON object or one text line.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines[r.head] = strings.TrimRight(string(p), "\n")
	r.head = (r.head + 1) % r.max
	if r.head == 0 {
		r.full = true
	}
	return len(p), nil
}

func (r *ringBuf) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if r.full {
		for i := r.head; i < r.max; i++ {
			b.WriteString(r.lines[i])
			b.WriteByte('\n')
		}
	}
	for i := 0; i < r.head; i++ {
		b.WriteString(r.lines[i])
		b.WriteByte('\n')
	}
	return b.String()
}

// ---- tiny atomic[T] (avoids pulling in sync/atomic.Pointer for old Go) ------

type atomic[T any] struct {
	mu sync.RWMutex
	v  T
}

func (a *atomic[T]) Store(v T) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomic[T]) Load() T   { a.mu.RLock(); defer a.mu.RUnlock(); return a.v }

// ParseLevel maps "debug|info|warn|error" to slog levels. Unknown → info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

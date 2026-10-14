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

	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
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
	// Every record that passes the level filter is counted by level.
	handler = countingHandler{next: handler, count: opsmetrics.IncLogRecord}
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
// migration. Each stdlib log line becomes a single slog event with
// component=legacy and the message in the "msg" field. Its level comes
// from the line (bridgeLevel): a leading "ERROR:", "WARN:" or "INFO:" is
// stripped and sets the level; otherwise a line that names an error,
// failure or panic is ERROR, one that names a warning or deprecation is
// WARN, and anything else is INFO. The configured level applies as usual.
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
		if line == "" {
			continue
		}
		level, msg := bridgeLevel(line)
		if msg != "" {
			b.lg.LogAttrs(context.Background(), level, msg)
		}
	}
	return len(p), nil
}

// bridgePrefixes are the explicit level prefixes a legacy line may start
// with (case-insensitive). Longer prefixes come first.
var bridgePrefixes = []struct {
	prefix string
	level  slog.Level
}{
	{"WARNING:", slog.LevelWarn},
	{"ERROR:", slog.LevelError},
	{"WARN:", slog.LevelWarn},
	{"INFO:", slog.LevelInfo},
}

// Words that classify a legacy line without an explicit prefix. They match
// as whole words only (see hasWord), so "error_rate" or "terrorist" do not.
var (
	bridgeErrorWords = []string{"error", "errors", "failed", "panic", "panics", "panicked"}
	bridgeWarnWords  = []string{"warn", "warning", "warnings", "deprecated"}
)

// bridgeLevel returns the level of one legacy log line and the message to
// log, with any leading level prefix removed.
func bridgeLevel(line string) (slog.Level, string) {
	trimmed := strings.TrimLeft(line, " \t")
	for _, p := range bridgePrefixes {
		if len(trimmed) >= len(p.prefix) && strings.EqualFold(trimmed[:len(p.prefix)], p.prefix) {
			return p.level, strings.TrimLeft(trimmed[len(p.prefix):], " \t")
		}
	}
	lower := strings.ToLower(line)
	switch {
	case hasWord(lower, bridgeErrorWords):
		return slog.LevelError, line
	case hasWord(lower, bridgeWarnWords):
		return slog.LevelWarn, line
	}
	return slog.LevelInfo, line
}

// hasWord reports whether s (lower case) contains one of words as a whole
// word: the bytes on both sides are not letters, digits or '_'.
func hasWord(s string, words []string) bool {
	for _, w := range words {
		for i := 0; i < len(s); {
			j := strings.Index(s[i:], w)
			if j < 0 {
				break
			}
			start := i + j
			end := start + len(w)
			if (start == 0 || !isWordByte(s[start-1])) && (end == len(s) || !isWordByte(s[end])) {
				return true
			}
			i = start + 1
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ---- record counter ---------------------------------------------------------

// countingHandler counts each record its inner handler is asked to write,
// by level, before passing it on. slog only calls Handle for records that
// pass Enabled, so the count respects the configured level.
type countingHandler struct {
	next  slog.Handler
	count func(level string)
}

func (h countingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.count != nil {
		h.count(levelLabel(r.Level))
	}
	return h.next.Handle(ctx, r)
}

func (h countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return countingHandler{next: h.next.WithAttrs(attrs), count: h.count}
}

func (h countingHandler) WithGroup(name string) slog.Handler {
	return countingHandler{next: h.next.WithGroup(name), count: h.count}
}

// levelLabel maps a slog level to one of four fixed metric labels.
func levelLabel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
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

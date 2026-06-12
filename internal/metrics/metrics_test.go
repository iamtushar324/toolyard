package metrics

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRecordDropsCountedAndLogged builds a Recorder with a tiny buffer and no
// background drainer, then floods Record so the channel overflows. The drop
// counter must climb and the first drop must emit a WARN.
func TestRecordDropsCountedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Capacity 1, no run() goroutine, so events pile up and overflow fast.
	r := &Recorder{
		log: lg,
		in:  make(chan Event, 1),
	}

	const n = 50
	for i := 0; i < n; i++ {
		r.Record(Event{ToolName: "t"})
	}

	// One event fits in the buffer; the rest are dropped.
	if got := r.DroppedCount(); got != n-1 {
		t.Fatalf("DroppedCount = %d, want %d", got, n-1)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN log line, got: %q", out)
	}
	if !strings.Contains(out, "dropped_total=1") {
		t.Fatalf("expected first-drop log with dropped_total=1, got: %q", out)
	}
}

// TestRecordDropLogCadence checks that drops are logged on the first drop and
// then every dropLogEvery'th drop, not on every single drop.
func TestRecordDropLogCadence(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	r := &Recorder{
		log: lg,
		in:  make(chan Event), // unbuffered, no reader: every Record drops
	}

	// Drive exactly dropLogEvery drops: drop #1 and drop #dropLogEvery log.
	for i := 0; i < dropLogEvery; i++ {
		r.Record(Event{})
	}

	if got := r.DroppedCount(); got != dropLogEvery {
		t.Fatalf("DroppedCount = %d, want %d", got, dropLogEvery)
	}

	lines := strings.Count(buf.String(), "level=WARN")
	if lines != 2 {
		t.Fatalf("expected 2 WARN lines (first drop + %dth drop), got %d: %q",
			dropLogEvery, lines, buf.String())
	}
}

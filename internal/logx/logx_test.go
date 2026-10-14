package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestBridgeLevelClassification(t *testing.T) {
	cases := []struct {
		line      string
		wantLevel slog.Level
		wantMsg   string
	}{
		{"metrics: batch insert failed: disk full", slog.LevelError, "metrics: batch insert failed: disk full"},
		{"lake: open clickhouse: Error dialing", slog.LevelError, "lake: open clickhouse: Error dialing"},
		{"approval sweep: 3 errors", slog.LevelError, "approval sweep: 3 errors"},
		{"executor panic on ap_1", slog.LevelError, "executor panic on ap_1"},
		{"warning: could not write the env file", slog.LevelWarn, "could not write the env file"},
		{"could not write the env file: warning only", slog.LevelWarn, "could not write the env file: warning only"},
		{"flag -x is Deprecated; use -y", slog.LevelWarn, "flag -x is Deprecated; use -y"},
		{"ERROR: explicit prefix", slog.LevelError, "explicit prefix"},
		{"  warn:   spaced prefix", slog.LevelWarn, "spaced prefix"},
		{"WARNING: long prefix", slog.LevelWarn, "long prefix"},
		{"INFO: an error word does not win over an explicit prefix", slog.LevelInfo, "an error word does not win over an explicit prefix"},
		{"toolyard: db at /var/lib/toolyard/toolyard.db", slog.LevelInfo, "toolyard: db at /var/lib/toolyard/toolyard.db"},
		{"error_rate=0 within bounds", slog.LevelInfo, "error_rate=0 within bounds"},
		{"terrorists and failedness are not words we match", slog.LevelInfo, "terrorists and failedness are not words we match"},
		{"warned_at=12", slog.LevelInfo, "warned_at=12"},
		// An explicit uppercase level word anywhere wins over the keywords.
		{"mcp-auth: WARN token check failed: bad signature", slog.LevelWarn, "mcp-auth: WARN token check failed: bad signature"},
		{"retry: INFO attempt 1 failed, retrying", slog.LevelInfo, "retry: INFO attempt 1 failed, retrying"},
		{"sync: WARNING 3 rows had errors", slog.LevelWarn, "sync: WARNING 3 rows had errors"},
		{"job: ERROR then WARN later", slog.LevelError, "job: ERROR then WARN later"},
		{"Warn and Error in mixed case are keywords, not tokens", slog.LevelError, "Warn and Error in mixed case are keywords, not tokens"},
		{"WARNINGS is not a level word; nothing failed", slog.LevelError, "WARNINGS is not a level word; nothing failed"},
	}
	for _, c := range cases {
		level, msg := bridgeLevel(c.line)
		if level != c.wantLevel || msg != c.wantMsg {
			t.Errorf("bridgeLevel(%q) = %v %q, want %v %q", c.line, level, msg, c.wantLevel, c.wantMsg)
		}
	}
}

// countRecorder collects the counting handler's labels.
type countRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *countRecorder) count(level string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[level]++
}

func (c *countRecorder) get(level string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[level]
}

func TestBridgeWriterLevelsAndCounter(t *testing.T) {
	var out bytes.Buffer
	rec := &countRecorder{}
	h := countingHandler{next: slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}), count: rec.count}
	bw := &bridgeWriter{lg: slog.New(h).With("component", "legacy")}

	// Two lines in one write, one split across writes.
	_, _ = bw.Write([]byte("routine startup line\nupstream x: dial failed\nWARN: low disk"))
	_, _ = bw.Write([]byte(" space\n"))

	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSON line %q: %v", line, err)
		}
		got = append(got, m)
	}
	// The INFO line is below the configured level: neither written nor counted.
	if len(got) != 2 {
		t.Fatalf("wrote %d records, want 2: %s", len(got), out.String())
	}
	if got[0]["level"] != "ERROR" || got[0]["msg"] != "upstream x: dial failed" || got[0]["component"] != "legacy" {
		t.Fatalf("first record = %v", got[0])
	}
	if got[1]["level"] != "WARN" || got[1]["msg"] != "low disk space" {
		t.Fatalf("second record = %v", got[1])
	}
	if rec.get("error") != 1 || rec.get("warn") != 1 || rec.get("info") != 0 {
		t.Fatalf("counts = %v, want error=1 warn=1 info=0", rec.counts)
	}
}

func TestLevelLabel(t *testing.T) {
	for level, want := range map[slog.Level]string{
		slog.LevelDebug - 4: "debug",
		slog.LevelDebug:     "debug",
		slog.LevelInfo:      "info",
		slog.LevelInfo + 2:  "info",
		slog.LevelWarn:      "warn",
		slog.LevelError:     "error",
		slog.LevelError + 4: "error",
	} {
		if got := levelLabel(level); got != want {
			t.Errorf("levelLabel(%v) = %q, want %q", level, got, want)
		}
	}
}

func TestBridgeLevelTokenFromActors(t *testing.T) {
	// The line cmd/gateway/actors.go writes when a token check fails.
	level, msg := bridgeLevel("mcp-auth: WARN token check failed: token expired")
	if level != slog.LevelWarn || msg != "mcp-auth: WARN token check failed: token expired" {
		t.Fatalf("got %v %q, want WARN and the line unchanged", level, msg)
	}
}

package opsmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/ops"
)

func testRegistry(state *ops.State) *registry {
	return newRegistry(state, time.Unix(1700000000, 0),
		func() time.Time { return time.Unix(1700000100, 0) },
		func() int { return 42 })
}

func render(r *registry) string {
	var b bytes.Buffer
	r.write(&b)
	return b.String()
}

// The whole exposition, byte for byte: names, HELP and TYPE lines, label
// quoting, sample order and number formatting.
func TestExpositionGolden(t *testing.T) {
	state := &ops.State{}
	state.MarkKeysVerified()
	r := testRegistry(state)
	r.version.Store("v1.2.3")
	r.pinger.Store(pingerBox{fn: func(context.Context) error { return nil }})

	r.toolCalls.with("ok").inc()
	r.toolCalls.with("ok").inc()
	r.toolCalls.with("denied").inc()
	r.inflight.with().set(3)
	r.auditFailures.with("intent").inc()
	r.outcomeUnknown.with("legacy", "shutdown_deadline").inc()
	r.goroutinePanics.with("telegram-poller").inc()
	r.loopLastSuccess.with("inbox-sweeper").set(unixSeconds(time.Unix(1700000050, 500000000)))
	r.legacySyncLast.with().set(unixSeconds(time.Unix(1700000060, 0)))
	r.legacySyncFailures.with().inc()
	r.logRecords.with("warn").inc()
	r.httpPanics.with().inc()

	const want = `# HELP toolyard_build_info Build information; the value is always 1.
# TYPE toolyard_build_info gauge
toolyard_build_info{version="v1.2.3"} 1
# HELP toolyard_process_start_time_seconds Start time of the process since the Unix epoch, in seconds.
# TYPE toolyard_process_start_time_seconds gauge
toolyard_process_start_time_seconds 1700000000
# HELP toolyard_started 1 once startup recovery and upstream loading are done.
# TYPE toolyard_started gauge
toolyard_started 0
# HELP toolyard_keys_verified 1 once every master key loaded and matched its database canary.
# TYPE toolyard_keys_verified gauge
toolyard_keys_verified 1
# HELP toolyard_draining 1 while the process drains before it exits.
# TYPE toolyard_draining gauge
toolyard_draining 0
# HELP toolyard_db_up 1 when the database answered a ping within 1s at scrape time.
# TYPE toolyard_db_up gauge
toolyard_db_up 1
# HELP toolyard_db_ping_seconds Duration of the scrape-time database ping, in seconds.
# TYPE toolyard_db_ping_seconds gauge
toolyard_db_ping_seconds 0
# HELP toolyard_goroutines Number of goroutines that currently exist.
# TYPE toolyard_goroutines gauge
toolyard_goroutines 42
# HELP toolyard_tool_calls_total Tool calls by outcome.
# TYPE toolyard_tool_calls_total counter
toolyard_tool_calls_total{outcome="approval"} 0
toolyard_tool_calls_total{outcome="denied"} 1
toolyard_tool_calls_total{outcome="error"} 0
toolyard_tool_calls_total{outcome="ok"} 2
toolyard_tool_calls_total{outcome="refused_draining"} 0
# HELP toolyard_inflight_executions Tool executions in flight.
# TYPE toolyard_inflight_executions gauge
toolyard_inflight_executions 3
# HELP toolyard_audit_write_failures_total Audit records that could not be written, by stage.
# TYPE toolyard_audit_write_failures_total counter
toolyard_audit_write_failures_total{stage="approval"} 0
toolyard_audit_write_failures_total{stage="intent"} 1
toolyard_audit_write_failures_total{stage="outcome"} 0
# HELP toolyard_executions_outcome_unknown_total Executions whose outcome is unknown (they may have run), by path and cause.
# TYPE toolyard_executions_outcome_unknown_total counter
toolyard_executions_outcome_unknown_total{path="legacy",cause="shutdown_deadline"} 1
# HELP toolyard_goroutine_panics_total Panics recovered in background loops and goroutines, by loop.
# TYPE toolyard_goroutine_panics_total counter
toolyard_goroutine_panics_total{loop="telegram-poller"} 1
# HELP toolyard_loop_last_success_timestamp_seconds Last successful pass of each supervised background loop, in Unix seconds.
# TYPE toolyard_loop_last_success_timestamp_seconds gauge
toolyard_loop_last_success_timestamp_seconds{loop="inbox-sweeper"} 1700000050.5
# HELP toolyard_legacy_sync_last_success_timestamp_seconds Last successful legacy approval sync into the Inbox, in Unix seconds (0 means never).
# TYPE toolyard_legacy_sync_last_success_timestamp_seconds gauge
toolyard_legacy_sync_last_success_timestamp_seconds 1700000060
# HELP toolyard_legacy_sync_failures_total Failed legacy approval syncs into the Inbox.
# TYPE toolyard_legacy_sync_failures_total counter
toolyard_legacy_sync_failures_total 1
# HELP toolyard_log_records_total Log records written, by level.
# TYPE toolyard_log_records_total counter
toolyard_log_records_total{level="debug"} 0
toolyard_log_records_total{level="error"} 0
toolyard_log_records_total{level="info"} 0
toolyard_log_records_total{level="warn"} 1
# HELP toolyard_http_panics_total Panics recovered in HTTP handlers.
# TYPE toolyard_http_panics_total counter
toolyard_http_panics_total 1
`
	if got := render(r); got != want {
		t.Fatalf("exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestDatabaseMetricsFollowThePinger(t *testing.T) {
	r := testRegistry(&ops.State{})
	if out := render(r); strings.Contains(out, "toolyard_db_up") || strings.Contains(out, "toolyard_db_ping_seconds") {
		t.Fatal("database metrics exposed without a pinger")
	}
	calls := 0
	r.pinger.Store(pingerBox{fn: func(context.Context) error {
		calls++
		return errors.New("database is closed")
	}})
	out := render(r)
	if !strings.Contains(out, "\ntoolyard_db_up 0\n") {
		t.Fatalf("failed ping not reported:\n%s", out)
	}
	render(r)
	if calls != 1 {
		t.Fatalf("%d pings for two scrapes within the cache window, want 1", calls)
	}
}

func TestStateGaugesReadTheState(t *testing.T) {
	state := &ops.State{}
	r := testRegistry(state)
	state.MarkStarted()
	state.SetDraining()
	out := render(r)
	for _, line := range []string{"toolyard_started 1", "toolyard_draining 1", "toolyard_keys_verified 0", `toolyard_build_info{version="unknown"} 1`} {
		if !strings.Contains(out, "\n"+line+"\n") {
			t.Errorf("missing %q", line)
		}
	}
}

func TestLabelValuesAreCapped(t *testing.T) {
	r := testRegistry(&ops.State{})
	for i := 0; i < 100; i++ {
		r.goroutinePanics.with(fmt.Sprintf("loop-%03d", i)).inc()
	}
	if n := len(r.goroutinePanics.series); n != maxSeries+1 {
		t.Fatalf("%d series, want %d plus other", n, maxSeries)
	}
	out := render(r)
	if !strings.Contains(out, `toolyard_goroutine_panics_total{loop="other"} 36`+"\n") {
		t.Fatalf("overflow not counted under other:\n%s", out)
	}
	if !strings.Contains(out, `toolyard_goroutine_panics_total{loop="loop-063"} 1`+"\n") || strings.Contains(out, `loop="loop-064"`) {
		t.Fatal("the first maxSeries values should keep their own series")
	}
	// Pre-seeded values count toward the cap; a known value still works.
	for i := 0; i < 100; i++ {
		r.toolCalls.with(fmt.Sprintf("x%d", i)).inc()
	}
	if n := len(r.toolCalls.series); n != maxSeries+1 {
		t.Fatalf("tool calls: %d series", n)
	}
	r.toolCalls.with("ok").inc()
	if !strings.Contains(render(r), `toolyard_tool_calls_total{outcome="ok"} 1`+"\n") {
		t.Fatal("known value lost after the cap")
	}
	// Two-label metrics overflow to an all-"other" label set.
	for i := 0; i < 70; i++ {
		r.outcomeUnknown.with("inbox", fmt.Sprintf("c%d", i)).inc()
	}
	if !strings.Contains(render(r), `toolyard_executions_outcome_unknown_total{path="other",cause="other"} 6`+"\n") {
		t.Fatal("two-label overflow not counted under other/other")
	}
}

func TestLabelValuesAreEscapedAndBounded(t *testing.T) {
	r := testRegistry(&ops.State{})
	r.goroutinePanics.with("a\"b\\c\nd").inc()
	r.goroutinePanics.with("bad\xffutf8").inc()
	r.goroutinePanics.with("").inc()
	r.goroutinePanics.with(strings.Repeat("é", 100)).inc() // 200 bytes
	out := render(r)
	for _, want := range []string{
		`{loop="a\"b\\c\nd"} 1`,
		`{loop="bad` + "�" + `utf8"} 1`,
		`{loop="unknown"} 1`,
		`{loop="` + strings.Repeat("é", 64) + `"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if c := cleanLabel(strings.Repeat("a", 127) + "é"); len(c) != 127 {
		t.Errorf("a label is cut inside a rune: %d bytes", len(c))
	}
}

func TestFormatValue(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1: "1", 0.25: "0.25", 1700000000: "1700000000", -2: "-2"} {
		if got := formatValue(v); got != want {
			t.Errorf("formatValue(%v) = %q, want %q", v, got, want)
		}
	}
	if unixSeconds(time.Time{}) != 0 {
		t.Error("zero time is not 0")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	r := testRegistry(&ops.State{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.toolCalls.with("ok").inc()
				r.goroutinePanics.with(fmt.Sprintf("loop-%d", i%10)).inc()
				r.inflight.with().set(float64(i))
				if i%100 == 0 {
					render(r)
				}
			}
		}(g)
	}
	wg.Wait()
	if !strings.Contains(render(r), `toolyard_tool_calls_total{outcome="ok"} 8000`+"\n") {
		t.Fatal("lost increments")
	}
}

// The exported functions update the process-wide registry, and Handler
// serves it with the 0.0.4 content type.
func TestExportedFunctionsAndHandler(t *testing.T) {
	SetBuildInfo("test-build")
	IncAuditWriteFailure("outcome")
	IncGoroutinePanic("exported-loop")
	SetLoopLastSuccess("exported-loop", time.Unix(1700000000, 0))
	SetLegacySyncSuccess(time.Unix(1700000001, 0))
	IncLegacySyncFailure()
	IncOutcomeUnknown("inbox", "startup_recovery")
	IncToolCall("refused_draining")
	SetInflightExecutions(5)
	IncLogRecord("error")
	IncHTTPPanic()
	SetDBPinger(func(context.Context) error { return nil })
	t.Cleanup(func() { SetDBPinger(nil) })

	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /metrics: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		`toolyard_build_info{version="test-build"} 1`,
		`toolyard_audit_write_failures_total{stage="outcome"} 1`,
		`toolyard_goroutine_panics_total{loop="exported-loop"} 1`,
		`toolyard_loop_last_success_timestamp_seconds{loop="exported-loop"} 1700000000`,
		`toolyard_legacy_sync_last_success_timestamp_seconds 1700000001`,
		`toolyard_legacy_sync_failures_total 1`,
		`toolyard_executions_outcome_unknown_total{path="inbox",cause="startup_recovery"} 1`,
		`toolyard_tool_calls_total{outcome="refused_draining"} 1`,
		`toolyard_inflight_executions 5`,
		`toolyard_log_records_total{level="error"} 1`,
		`toolyard_http_panics_total 1`,
		`toolyard_db_up 1`,
		`toolyard_process_start_time_seconds `,
		`toolyard_goroutines `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD /metrics: %d with %d bytes", head.Code, head.Body.Len())
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics: %d", post.Code)
	}
}

// Package opsmetrics is toolyard's Prometheus endpoint: a small set of
// process-wide counters and gauges, rendered by hand in the Prometheus
// text exposition format 0.0.4 (standard library only).
//
// Every function is safe for concurrent use and cheap: an atomic add or
// store after a read-locked map lookup. Label values are bounded: each
// metric keeps at most maxSeries distinct label sets, and later new ones
// are counted under "other". Labels name a stage, loop, path, cause,
// outcome or log level, never an agent, a user, a tool or an argument.
//
// The handler is served on the private ops listener (-ops-addr), never on
// the public HTTP listener.
package opsmetrics

import (
	"bufio"
	"context"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/ops"
)

const (
	// maxSeries caps the distinct label sets of one metric.
	maxSeries = 64
	// maxLabelLen caps one label value, in bytes.
	maxLabelLen = 128
	// otherValue replaces every label of a label set past maxSeries.
	otherValue = "other"
	// contentType is the text exposition format, version 0.0.4.
	contentType = "text/plain; version=0.0.4; charset=utf-8"
	// dbPingTimeout bounds the scrape-time database ping.
	dbPingTimeout = time.Second
	// dbPingCacheTTL is how long one ping result answers every scrape.
	dbPingCacheTTL = time.Second
)

const (
	typeCounter = "counter"
	typeGauge   = "gauge"
)

// series is one label set of a family. Counters keep a count in n;
// gauges keep the bits of a float64.
type series struct {
	values []string
	n      atomic.Uint64
}

func (s *series) inc() { s.n.Add(1) }

func (s *series) set(v float64) { s.n.Store(math.Float64bits(v)) }

type family struct {
	name   string
	help   string
	typ    string
	labels []string

	mu     sync.RWMutex
	series map[string]*series
}

func newFamily(name, typ, help string, labels ...string) *family {
	f := &family{name: name, help: help, typ: typ, labels: labels, series: map[string]*series{}}
	if len(labels) == 0 {
		f.with() // an unlabelled metric always has its one sample
	}
	return f
}

// with returns the series for the label values (one per label), creating
// it unless the family already holds maxSeries label sets; then the
// all-"other" series is returned.
func (f *family) with(values ...string) *series {
	vals := make([]string, len(f.labels))
	for i := range vals {
		if i < len(values) {
			vals[i] = cleanLabel(values[i])
		} else {
			vals[i] = "unknown"
		}
	}
	key := strings.Join(vals, "\xff")
	f.mu.RLock()
	s := f.series[key]
	f.mu.RUnlock()
	if s != nil {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s = f.series[key]; s != nil {
		return s
	}
	if len(f.series) >= maxSeries {
		for i := range vals {
			vals[i] = otherValue
		}
		key = strings.Join(vals, "\xff")
		if s = f.series[key]; s != nil {
			return s
		}
	}
	s = &series{values: vals}
	f.series[key] = s
	return s
}

func (f *family) value(s *series) float64 {
	if f.typ == typeCounter {
		return float64(s.n.Load())
	}
	return math.Float64frombits(s.n.Load())
}

func (f *family) write(w *bufio.Writer) {
	writeHeader(w, f.name, f.typ, f.help)
	f.mu.RLock()
	keys := make([]string, 0, len(f.series))
	for k := range f.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]*series, len(keys))
	for i, k := range keys {
		list[i] = f.series[k]
	}
	f.mu.RUnlock()
	for _, s := range list {
		writeSample(w, f.name, f.labels, s.values, f.value(s))
	}
}

// cleanLabel makes v a bounded, valid UTF-8 label value.
func cleanLabel(v string) string {
	v = strings.ToValidUTF8(v, "�")
	if len(v) > maxLabelLen {
		cut := maxLabelLen
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		v = v[:cut]
	}
	if v == "" {
		return "unknown"
	}
	return v
}

type pingFunc func(context.Context) error

type pingerBox struct{ fn pingFunc }

type registry struct {
	state      *ops.State
	start      time.Time
	now        func() time.Time
	goroutines func() int

	version atomic.Value // string
	pinger  atomic.Value // pingerBox

	pingMu sync.Mutex
	pingAt time.Time
	pingOK bool
	pingS  float64
	pinged bool

	toolCalls          *family
	inflight           *family
	auditFailures      *family
	outcomeUnknown     *family
	goroutinePanics    *family
	loopLastSuccess    *family
	legacySyncLast     *family
	legacySyncFailures *family
	logRecords         *family
	httpPanics         *family
	searchRanker       *family
	searchRankerCost   *family
	families           []*family
}

func newRegistry(state *ops.State, start time.Time, now func() time.Time, goroutines func() int) *registry {
	r := &registry{state: state, start: start, now: now, goroutines: goroutines}
	r.toolCalls = newFamily("toolyard_tool_calls_total", typeCounter,
		"Tool calls by outcome.", "outcome")
	r.inflight = newFamily("toolyard_inflight_executions", typeGauge,
		"Tool executions in flight.")
	r.auditFailures = newFamily("toolyard_audit_write_failures_total", typeCounter,
		"Audit records that could not be written, by stage.", "stage")
	r.outcomeUnknown = newFamily("toolyard_executions_outcome_unknown_total", typeCounter,
		"Executions whose outcome is unknown (they may have run), by path and cause.", "path", "cause")
	r.goroutinePanics = newFamily("toolyard_goroutine_panics_total", typeCounter,
		"Panics recovered in background loops and goroutines, by loop.", "loop")
	r.loopLastSuccess = newFamily("toolyard_loop_last_success_timestamp_seconds", typeGauge,
		"Last successful pass of each supervised background loop, in Unix seconds.", "loop")
	r.legacySyncLast = newFamily("toolyard_legacy_sync_last_success_timestamp_seconds", typeGauge,
		"Last successful legacy approval sync into the Inbox, in Unix seconds (0 means never).")
	r.legacySyncFailures = newFamily("toolyard_legacy_sync_failures_total", typeCounter,
		"Failed legacy approval syncs into the Inbox.")
	r.logRecords = newFamily("toolyard_log_records_total", typeCounter,
		"Log records written, by level.", "level")
	r.httpPanics = newFamily("toolyard_http_panics_total", typeCounter,
		"Panics recovered in HTTP handlers.")
	r.searchRanker = newFamily("toolyard_tool_search_ranker_requests_total", typeCounter,
		"Requests of the tools.search ranker (-tool-search-ranker) by outcome: ok, or why the search fell back to keyword ranking.", "outcome")
	r.searchRankerCost = newFamily("toolyard_tool_search_ranker_cost_microdollars_total", typeCounter,
		"What the ranker's provider reported its requests cost, in millionths of a US dollar.")
	r.families = []*family{
		r.toolCalls, r.inflight, r.auditFailures, r.outcomeUnknown,
		r.goroutinePanics, r.loopLastSuccess, r.legacySyncLast,
		r.legacySyncFailures, r.logRecords, r.httpPanics,
		r.searchRanker, r.searchRankerCost,
	}
	// Known label values start at zero, so increase() and alerts work
	// from the first scrape.
	for _, o := range []string{"ok", "error", "denied", "approval", "refused_draining"} {
		r.toolCalls.with(o)
	}
	for _, s := range []string{"intent", "outcome", "approval"} {
		r.auditFailures.with(s)
	}
	for _, l := range []string{"debug", "info", "warn", "error"} {
		r.logRecords.with(l)
	}
	for _, o := range []string{"ok", "no_key", "timeout", "error", "bad_response"} {
		r.searchRanker.with(o)
	}
	r.searchRankerCost.with()
	return r
}

var std = newRegistry(ops.Default, time.Now(), time.Now, runtime.NumGoroutine)

// Handler serves /metrics: GET and HEAD only.
func Handler() http.Handler { return std.handler() }

// SetBuildInfo sets the version label of toolyard_build_info.
func SetBuildInfo(version string) { std.version.Store(version) }

// SetDBPinger registers the database ping behind toolyard_db_up and
// toolyard_db_ping_seconds. Without one, neither metric is exposed.
func SetDBPinger(ping func(context.Context) error) { std.pinger.Store(pingerBox{fn: ping}) }

// IncAuditWriteFailure counts an audit record that could not be written.
// stage is "intent", "outcome" or "approval".
func IncAuditWriteFailure(stage string) { std.auditFailures.with(stage).inc() }

// IncGoroutinePanic counts a panic recovered in a background loop or
// goroutine (codemode-nested-call: a tool called from a code-mode script).
func IncGoroutinePanic(loop string) { std.goroutinePanics.with(loop).inc() }

// SetLoopLastSuccess records the last successful pass of a supervised loop.
func SetLoopLastSuccess(loop string, t time.Time) { std.loopLastSuccess.with(loop).set(unixSeconds(t)) }

// SetLegacySyncSuccess records the last successful legacy approval sync.
func SetLegacySyncSuccess(t time.Time) { std.legacySyncLast.with().set(unixSeconds(t)) }

// IncLegacySyncFailure counts a failed legacy approval sync.
func IncLegacySyncFailure() { std.legacySyncFailures.with().inc() }

// IncOutcomeUnknown counts an execution whose outcome is unknown. path is
// "inbox" or "legacy"; cause is a short fixed word chosen by the caller.
func IncOutcomeUnknown(path, cause string) { std.outcomeUnknown.with(path, cause).inc() }

// IncToolCall counts a tool call. outcome is "ok", "error", "denied",
// "approval" or "refused_draining".
func IncToolCall(outcome string) { std.toolCalls.with(outcome).inc() }

// ObserveToolSearchRanker counts one tools.search ranker request by outcome
// and adds the cost its provider reported (US dollars; 0 when none).
func ObserveToolSearchRanker(outcome string, cost float64) {
	std.observeToolSearchRanker(outcome, cost)
}

func (r *registry) observeToolSearchRanker(outcome string, cost float64) {
	r.searchRanker.with(outcome).inc()
	if cost > 0 && !math.IsInf(cost, 0) {
		r.searchRankerCost.with().n.Add(uint64(math.Round(cost * 1e6)))
	}
}

// SetInflightExecutions sets the number of tool executions in flight.
func SetInflightExecutions(n int) { std.inflight.with().set(float64(n)) }

// IncLogRecord counts a log record. level is "debug", "info", "warn" or
// "error".
func IncLogRecord(level string) { std.logRecords.with(level).inc() }

// IncHTTPPanic counts a panic recovered in an HTTP handler.
func IncHTTPPanic() { std.httpPanics.with().inc() }

func (r *registry) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		if req.Method == http.MethodHead {
			return
		}
		r.write(w)
	})
}

// pingDB pings the registered database at most once per dbPingCacheTTL.
// ok is false when no pinger is registered.
func (r *registry) pingDB() (up bool, seconds float64, ok bool) {
	box, _ := r.pinger.Load().(pingerBox)
	if box.fn == nil {
		return false, 0, false
	}
	r.pingMu.Lock()
	defer r.pingMu.Unlock()
	if r.pinged {
		if age := r.now().Sub(r.pingAt); age >= 0 && age < dbPingCacheTTL {
			return r.pingOK, r.pingS, true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
	t0 := r.now()
	err := box.fn(ctx)
	cancel()
	t1 := r.now()
	r.pinged, r.pingAt, r.pingOK, r.pingS = true, t1, err == nil, t1.Sub(t0).Seconds()
	return r.pingOK, r.pingS, true
}

// write renders every metric: the process gauges first, then the families
// in registration order, each family's samples sorted by label values.
func (r *registry) write(out io.Writer) {
	w := bufio.NewWriter(out)
	defer w.Flush()

	version, _ := r.version.Load().(string)
	if version == "" {
		version = "unknown"
	}
	writeHeader(w, "toolyard_build_info", typeGauge, "Build information; the value is always 1.")
	writeSample(w, "toolyard_build_info", []string{"version"}, []string{cleanLabel(version)}, 1)

	writeGauge(w, "toolyard_process_start_time_seconds",
		"Start time of the process since the Unix epoch, in seconds.", unixSeconds(r.start))
	writeGauge(w, "toolyard_started",
		"1 once startup recovery and upstream loading are done.", boolValue(r.state.Started()))
	writeGauge(w, "toolyard_keys_verified",
		"1 once every master key loaded and matched its database canary.", boolValue(r.state.KeysVerified()))
	writeGauge(w, "toolyard_draining",
		"1 while the process drains before it exits.", boolValue(r.state.Draining()))
	if up, secs, ok := r.pingDB(); ok {
		writeGauge(w, "toolyard_db_up",
			"1 when the database answered a ping within 1s at scrape time.", boolValue(up))
		writeGauge(w, "toolyard_db_ping_seconds",
			"Duration of the scrape-time database ping, in seconds.", secs)
	}
	writeGauge(w, "toolyard_goroutines",
		"Number of goroutines that currently exist.", float64(r.goroutines()))

	for _, f := range r.families {
		f.write(w)
	}
}

func writeGauge(w *bufio.Writer, name, help string, v float64) {
	writeHeader(w, name, typeGauge, help)
	writeSample(w, name, nil, nil, v)
}

func writeHeader(w *bufio.Writer, name, typ, help string) {
	w.WriteString("# HELP ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(escapeHelp(help))
	w.WriteString("\n# TYPE ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(typ)
	w.WriteByte('\n')
}

func writeSample(w *bufio.Writer, name string, labels, values []string, v float64) {
	w.WriteString(name)
	if len(labels) > 0 {
		w.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				w.WriteByte(',')
			}
			w.WriteString(l)
			w.WriteString(`="`)
			w.WriteString(escapeLabel(values[i]))
			w.WriteByte('"')
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(formatValue(v))
	w.WriteByte('\n')
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string { return helpEscaper.Replace(s) }

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

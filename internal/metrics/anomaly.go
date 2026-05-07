package metrics

import (
	"context"
	"log"
	"math"
	"strings"
	"time"
)

// AnomalyDetector scans recent call_events and writes anomaly_events rows
// when something looks off. It runs once per Tick interval and is safe to
// call concurrently; each Run takes a single pass over the window.
//
// Detectors implemented:
//
//   - rate_spike      : per-agent calls/hour > zScore × stdev over the 7d
//                       baseline.
//   - error_spike     : per-tool 1h error rate > 3× the trailing-7d rate
//                       (and at least 20 calls in the spike window).
//   - args_outlier    : args_size_bytes for a single call exceeds the
//                       p99 of the trailing 30d sample for that tool by
//                       at least 3×.
//   - reason_repeat   : the same reason_text appears ≥5 times in 1h
//                       across one agent (indicates "phoning it in").
type AnomalyDetector struct {
	r        *Reader
	zScore   float64
	interval time.Duration
}

// NewAnomalyDetector returns a detector. zScore defaults to 3 if zero.
func NewAnomalyDetector(r *Reader, zScore float64) *AnomalyDetector {
	if zScore <= 0 {
		zScore = 3
	}
	return &AnomalyDetector{r: r, zScore: zScore, interval: 5 * time.Minute}
}

// Run executes one detection pass. Idempotent: dedup is best-effort by the
// (kind, agent, tool, hour-bucket) shape so a cron rerunning within the same
// hour won't multiply anomalies.
func (d *AnomalyDetector) Run(ctx context.Context) error {
	if err := d.detectRateSpikes(ctx); err != nil {
		return err
	}
	if err := d.detectErrorSpikes(ctx); err != nil {
		return err
	}
	if err := d.detectArgsOutliers(ctx); err != nil {
		return err
	}
	if err := d.detectReasonRepeats(ctx); err != nil {
		return err
	}
	return nil
}

// RunForever ticks every interval until ctx is cancelled.
func (d *AnomalyDetector) RunForever(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.Run(ctx); err != nil {
				log.Printf("anomaly: %v", err)
			}
		}
	}
}

// detectRateSpikes computes per-agent calls in the last full hour vs the
// trailing 7d hourly baseline. Anomalies fire when the recent count is more
// than zScore standard deviations above the mean.
func (d *AnomalyDetector) detectRateSpikes(ctx context.Context) error {
	now := time.Now()
	hourEnd := now.Truncate(time.Hour)
	hourStart := hourEnd.Add(-time.Hour)
	baselineFrom := hourStart.Add(-7 * 24 * time.Hour)

	rows, err := d.r.db.QueryContext(ctx, `SELECT
        COALESCE(agent_id,''), (ts/3600000)*3600000 AS bucket, COUNT(*)
        FROM call_events
        WHERE agent_id IS NOT NULL AND ts >= ? AND ts < ?
        GROUP BY agent_id, bucket`, baselineFrom.UnixMilli(), hourEnd.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	type bucket struct {
		ts    int64
		count int64
	}
	per := map[string][]bucket{}
	for rows.Next() {
		var agent string
		var b bucket
		if err := rows.Scan(&agent, &b.ts, &b.count); err != nil {
			return err
		}
		per[agent] = append(per[agent], b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for agent, hours := range per {
		var lastHour int64
		var hist []float64
		for _, h := range hours {
			if h.ts == hourStart.UnixMilli() {
				lastHour = h.count
				continue
			}
			hist = append(hist, float64(h.count))
		}
		if lastHour < 5 || len(hist) < 24 {
			// Not enough signal to declare an anomaly.
			continue
		}
		mean, stdev := meanStdev(hist)
		if stdev <= 0 {
			continue
		}
		z := (float64(lastHour) - mean) / stdev
		if z < d.zScore {
			continue
		}
		if d.recentlyRecorded(ctx, "rate_spike", agent, "", hourStart) {
			continue
		}
		_ = d.r.RecordAnomaly(ctx, "rate_spike", "warn", agent, "",
			formatRateSummary(agent, lastHour, mean, z),
			map[string]any{"last_hour": lastHour, "mean": mean, "stdev": stdev, "z": z})
	}
	return nil
}

// detectErrorSpikes runs per-tool and asks: is the last hour's error rate
// dramatically worse than the prior week's?
func (d *AnomalyDetector) detectErrorSpikes(ctx context.Context) error {
	hourEnd := time.Now().Truncate(time.Hour)
	hourStart := hourEnd.Add(-time.Hour)
	baselineFrom := hourStart.Add(-7 * 24 * time.Hour)

	rows, err := d.r.db.QueryContext(ctx, `SELECT
        tool_name,
        SUM(CASE WHEN ts >= ? THEN 1 ELSE 0 END) AS recent_calls,
        SUM(CASE WHEN ts >= ? AND outcome='error' THEN 1 ELSE 0 END) AS recent_errors,
        SUM(CASE WHEN ts < ? THEN 1 ELSE 0 END) AS hist_calls,
        SUM(CASE WHEN ts < ? AND outcome='error' THEN 1 ELSE 0 END) AS hist_errors
        FROM call_events WHERE ts >= ? AND ts < ?
        GROUP BY tool_name`,
		hourStart.UnixMilli(), hourStart.UnixMilli(),
		hourStart.UnixMilli(), hourStart.UnixMilli(),
		baselineFrom.UnixMilli(), hourEnd.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tool string
		var recentCalls, recentErrors, histCalls, histErrors int64
		if err := rows.Scan(&tool, &recentCalls, &recentErrors, &histCalls, &histErrors); err != nil {
			return err
		}
		if recentCalls < 20 {
			continue
		}
		recentRate := float64(recentErrors) / float64(recentCalls)
		var histRate float64
		if histCalls > 0 {
			histRate = float64(histErrors) / float64(histCalls)
		}
		if recentRate < 0.05 {
			continue
		}
		if histRate > 0 && recentRate < histRate*3 {
			continue
		}
		if d.recentlyRecorded(ctx, "error_spike", "", tool, hourStart) {
			continue
		}
		_ = d.r.RecordAnomaly(ctx, "error_spike", "warn", "", tool,
			formatErrorSummary(tool, recentRate, histRate),
			map[string]any{
				"recent_rate": recentRate, "hist_rate": histRate,
				"recent_calls": recentCalls, "recent_errors": recentErrors,
			})
	}
	return rows.Err()
}

// detectArgsOutliers flags a single call whose args size is way over the
// tool's recent p99. Useful to catch agents accidentally dumping a whole
// repo into a tool argument.
func (d *AnomalyDetector) detectArgsOutliers(ctx context.Context) error {
	hourEnd := time.Now().Truncate(time.Hour)
	hourStart := hourEnd.Add(-time.Hour)
	baselineFrom := hourStart.Add(-30 * 24 * time.Hour)

	rows, err := d.r.db.QueryContext(ctx, `SELECT tool_name, args_size_bytes
        FROM call_events WHERE args_size_bytes IS NOT NULL
          AND ts >= ? AND ts < ?
        ORDER BY tool_name, args_size_bytes ASC`,
		baselineFrom.UnixMilli(), hourStart.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	p99 := map[string]int{}
	tool := ""
	var batch []int
	flush := func() {
		if len(batch) < 50 {
			batch = nil
			return
		}
		idx := (len(batch) * 99) / 100
		p99[tool] = batch[idx]
		batch = nil
	}
	for rows.Next() {
		var t string
		var size int
		if err := rows.Scan(&t, &size); err != nil {
			return err
		}
		if t != tool {
			flush()
			tool = t
		}
		batch = append(batch, size)
	}
	flush()
	if err := rows.Err(); err != nil {
		return err
	}

	// Now look at the last hour for outliers vs each tool's p99.
	rows2, err := d.r.db.QueryContext(ctx, `SELECT
        tool_name, COALESCE(agent_id,''), args_size_bytes, ts
        FROM call_events WHERE args_size_bytes IS NOT NULL
          AND ts >= ? AND ts < ?`, hourStart.UnixMilli(), hourEnd.UnixMilli())
	if err != nil {
		return err
	}
	defer rows2.Close()
	for rows2.Next() {
		var tname, agent string
		var size int
		var ts int64
		if err := rows2.Scan(&tname, &agent, &size, &ts); err != nil {
			return err
		}
		threshold, ok := p99[tname]
		if !ok || threshold <= 0 {
			continue
		}
		if size < threshold*3 {
			continue
		}
		if d.recentlyRecorded(ctx, "args_outlier", agent, tname, hourStart) {
			continue
		}
		_ = d.r.RecordAnomaly(ctx, "args_outlier", "info", agent, tname,
			formatArgsSummary(tname, size, threshold),
			map[string]any{"size": size, "p99_baseline": threshold, "ts": ts})
	}
	return rows2.Err()
}

// detectReasonRepeats flags an agent re-using the same reason text many
// times in an hour — usually a sign the agent is stuffing a placeholder
// to satisfy the schema rather than thinking about why it's calling.
func (d *AnomalyDetector) detectReasonRepeats(ctx context.Context) error {
	hourEnd := time.Now().Truncate(time.Hour)
	hourStart := hourEnd.Add(-time.Hour)
	rows, err := d.r.db.QueryContext(ctx, `SELECT
        COALESCE(agent_id,''), reason_text, COUNT(*)
        FROM call_events
        WHERE reason_text IS NOT NULL AND reason_text != ''
          AND ts >= ? AND ts < ?
        GROUP BY agent_id, reason_text
        HAVING COUNT(*) >= 5`, hourStart.UnixMilli(), hourEnd.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, reason string
		var n int64
		if err := rows.Scan(&agent, &reason, &n); err != nil {
			return err
		}
		if d.recentlyRecorded(ctx, "reason_repeat", agent, "", hourStart) {
			continue
		}
		snippet := reason
		if len(snippet) > 80 {
			snippet = snippet[:80] + "…"
		}
		_ = d.r.RecordAnomaly(ctx, "reason_repeat", "info", agent, "",
			"agent reused the same _reason "+itoa(n)+" times in 1h: "+snippet,
			map[string]any{"reason": reason, "count": n})
	}
	return rows.Err()
}

func (d *AnomalyDetector) recentlyRecorded(ctx context.Context, kind, agent, tool string, hourStart time.Time) bool {
	row := d.r.db.QueryRowContext(ctx, `SELECT 1 FROM anomaly_events
        WHERE kind = ? AND COALESCE(agent_id,'') = ? AND COALESCE(tool_name,'') = ?
          AND ts >= ? LIMIT 1`,
		kind, agent, tool, hourStart.UnixMilli())
	var x int
	return row.Scan(&x) == nil
}

func meanStdev(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return mean, math.Sqrt(ss / float64(len(xs)))
}

func formatRateSummary(agent string, recent int64, mean, z float64) string {
	return "agent " + agent + " made " + itoa(recent) +
		" calls last hour (z=" + ftoa(z, 1) + ", baseline avg " + ftoa(mean, 1) + ")"
}
func formatErrorSummary(tool string, recent, hist float64) string {
	return tool + " error rate spiked to " + ftoa(recent*100, 1) + "% (was " + ftoa(hist*100, 1) + "%)"
}
func formatArgsSummary(tool string, size, p99 int) string {
	return tool + " was called with args " + itoa(int64(size)) + "B (>3× tool's p99 of " + itoa(int64(p99)) + "B)"
}

// itoa / ftoa avoid pulling fmt into a hot path; the strings are tiny.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
func ftoa(f float64, decimals int) string {
	mult := 1
	for i := 0; i < decimals; i++ {
		mult *= 10
	}
	whole := int64(f)
	frac := int64(math.Abs(f-float64(whole))*float64(mult) + 0.5)
	out := itoa(whole)
	if decimals > 0 {
		fracStr := itoa(frac)
		// pad with leading zeros
		for len(fracStr) < decimals {
			fracStr = "0" + fracStr
		}
		out += "." + fracStr
	}
	return out
}

// ScoreReasons walks recent call_events and updates each row's reason_quality
// based on length + duplicate rate. Cheap and intended to be run periodically.
//
// The score is a heuristic in [0,1]:
//   +0.4 if length >= 40
//   +0.4 if length <= 600     (we want substance, not novels)
//   +0.2 if first word is an action verb-ish token
//   -0.6 if the same reason was used >= 5 times by this agent in the last 24h
func (r *Reader) ScoreReasons(ctx context.Context, since time.Time) error {
	rows, err := r.db.QueryContext(ctx, `SELECT event_id, COALESCE(agent_id,''), reason_text
        FROM call_events
        WHERE reason_text IS NOT NULL AND reason_quality IS NULL
          AND ts >= ?`, since.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		id     int64
		agent  string
		reason string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.agent, &p.reason); err != nil {
			return err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}

	// Pull duplicate counts (agent, reason) over last 24h to penalise repeats.
	dayAgo := time.Now().Add(-24 * time.Hour).UnixMilli()
	dupRows, err := r.db.QueryContext(ctx, `SELECT
        COALESCE(agent_id,''), reason_text, COUNT(*)
        FROM call_events
        WHERE reason_text IS NOT NULL AND ts >= ?
        GROUP BY agent_id, reason_text`, dayAgo)
	if err != nil {
		return err
	}
	defer dupRows.Close()
	dup := map[string]int{}
	for dupRows.Next() {
		var agent, reason string
		var n int
		if err := dupRows.Scan(&agent, &reason, &n); err != nil {
			return err
		}
		dup[agent+"|"+reason] = n
	}
	if err := dupRows.Err(); err != nil {
		return err
	}

	for _, p := range todo {
		score := scoreOne(p.reason, dup[p.agent+"|"+p.reason])
		if _, err := r.db.ExecContext(ctx,
			`UPDATE call_events SET reason_quality = ? WHERE event_id = ?`, score, p.id); err != nil {
			return err
		}
	}
	return nil
}

func scoreOne(reason string, dupCount int) float64 {
	score := 0.0
	n := len(reason)
	if n >= 40 {
		score += 0.4
	}
	if n <= 600 {
		score += 0.4
	}
	first := firstWord(reason)
	if isActionVerb(first) {
		score += 0.2
	}
	if dupCount >= 5 {
		score -= 0.6
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == ':' {
			return strings.ToLower(s[:i])
		}
	}
	return strings.ToLower(s)
}

var actionVerbs = map[string]bool{
	"create": true, "update": true, "delete": true, "fetch": true, "read": true,
	"write": true, "send": true, "post": true, "publish": true, "deploy": true,
	"add": true, "remove": true, "rename": true, "move": true, "list": true,
	"search": true, "query": true, "find": true, "compute": true, "compare": true,
	"verify": true, "check": true, "trigger": true, "run": true, "execute": true,
	"build": true, "test": true, "validate": true, "merge": true, "branch": true,
	"summarise": true, "summarize": true, "explain": true, "analyse": true,
	"analyze": true, "investigate": true, "diagnose": true, "fix": true,
}

func isActionVerb(w string) bool { return actionVerbs[w] }

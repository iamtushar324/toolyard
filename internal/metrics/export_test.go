package metrics

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// legacyExportCSV is ExportCSV as it was before keyset paging: one query, one
// cursor, a write per row while the cursor is open. The paged export must
// produce the same bytes when no two rows share a ts.
func legacyExportCSV(ctx context.Context, db *store.DB, w interface{ Write(p []byte) (int, error) }, rg Range, rowCap int) error {
	from, to := rg.bounds()
	rows, err := db.QueryContext(ctx, `SELECT
        event_id, ts, COALESCE(agent_id,''), COALESCE(agent_name,''),
        upstream, short_name, tool_name,
        is_write, is_destructive, pinned_tool,
        COALESCE(via,''), COALESCE(surface_mode,''),
        COALESCE(fingerprint,''), COALESCE(args_size_bytes,0),
        COALESCE(reason_text,''), COALESCE(reason_len,0), COALESCE(reason_quality,0),
        COALESCE(approval_id,''), COALESCE(approval_outcome,''),
        COALESCE(approval_latency_ms,0), COALESCE(approval_via,''),
        outcome, COALESCE(error_class,''), COALESCE(total_latency_ms,0),
        COALESCE(result_size_bytes,0)
        FROM call_events WHERE ts >= ? AND ts < ? ORDER BY ts ASC LIMIT ?`,
		from, to, rowCap)
	if err != nil {
		return err
	}
	defer rows.Close()
	header := "event_id,ts,agent_id,agent_name,upstream,short_name,tool_name," +
		"is_write,is_destructive,pinned_tool,via,surface_mode,fingerprint," +
		"args_size_bytes,reason_text,reason_len,reason_quality,approval_id," +
		"approval_outcome,approval_latency_ms,approval_via,outcome,error_class," +
		"total_latency_ms,result_size_bytes\n"
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	for rows.Next() {
		var (
			eventID, ts                                       int64
			agentID, agentName, upstream, shortName, toolName string
			isWrite, isDestructive, pinned                    int
			via, surfaceMode, fp                              string
			argsSize                                          int
			reason                                            string
			reasonLen                                         int
			reasonQuality                                     float64
			approvalID, approvalOutcome                       string
			approvalLatency                                   int
			approvalVia                                       string
			outcome, errClass                                 string
			totalLatency, resultSize                          int
		)
		if err := rows.Scan(&eventID, &ts, &agentID, &agentName, &upstream, &shortName, &toolName,
			&isWrite, &isDestructive, &pinned, &via, &surfaceMode, &fp, &argsSize,
			&reason, &reasonLen, &reasonQuality, &approvalID, &approvalOutcome,
			&approvalLatency, &approvalVia, &outcome, &errClass, &totalLatency, &resultSize); err != nil {
			return err
		}
		fields := []string{
			fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", ts), agentID, agentName,
			upstream, shortName, toolName,
			fmt.Sprintf("%d", isWrite), fmt.Sprintf("%d", isDestructive), fmt.Sprintf("%d", pinned),
			via, surfaceMode, fp, fmt.Sprintf("%d", argsSize),
			reason, fmt.Sprintf("%d", reasonLen), fmt.Sprintf("%.3f", reasonQuality),
			approvalID, approvalOutcome, fmt.Sprintf("%d", approvalLatency), approvalVia,
			outcome, errClass, fmt.Sprintf("%d", totalLatency), fmt.Sprintf("%d", resultSize),
		}
		if _, err := w.Write([]byte(csvEscape(fields))); err != nil {
			return err
		}
	}
	return rows.Err()
}

// insertExportRow writes one call_events row. nil values are stored as NULL.
func insertExportRow(t *testing.T, db *store.DB, ts int64, agentID, reason, errClass any, quality any, isWrite int) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO call_events(
        ts, agent_id, agent_name, upstream, short_name, tool_name,
        is_write, is_destructive, pinned_tool, via, surface_mode, fingerprint,
        args_size_bytes, reason_text, reason_len, reason_quality,
        approval_id, approval_outcome, approval_latency_ms, approval_via,
        outcome, error_class, total_latency_ms, result_size_bytes
    ) VALUES(?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?,?,?)`,
		ts, agentID, "agent, \"quoted\"", "github", "merge", "github.merge",
		isWrite, 0, 1, "direct", nil, "fp_"+strconv.FormatInt(ts, 10),
		120, reason, 42, quality,
		"ap_1", "approved", 1500, "dashboard",
		"ok", errClass, 77, 2048)
	if err != nil {
		t.Fatalf("insert call_events ts=%d: %v", ts, err)
	}
}

// TestExportCSVPagesMatchSingleQuery checks that the keyset-paged export
// writes exactly the bytes the old single-cursor export wrote: same header,
// same row order, same escaping, same range filter and the same row cap,
// whatever the page size.
func TestExportCSVPagesMatchSingleQuery(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

	// Inserted out of ts order so event_id order differs from ts order.
	reasons := []any{
		"plain reason",
		"has, a comma",
		"say \"hi\", then\nleave",
		nil,
		"carriage\rreturn",
		"",
		"last one",
	}
	for i, off := range []int64{5, 1, 7, 3, 2, 6, 4} {
		var agent any = "ag_" + strconv.Itoa(i)
		if i%3 == 0 {
			agent = nil
		}
		var quality any = 0.25 * float64(i)
		if i%2 == 1 {
			quality = nil
		}
		var errClass any
		if i == 2 {
			errClass = "timeout,retry"
		}
		insertExportRow(t, db, base+off*1000, agent, reasons[i], errClass, quality, i%2)
	}
	// Outside the range on both sides: never exported.
	insertExportRow(t, db, base-1, "ag_before", "before", nil, nil, 0)
	insertExportRow(t, db, base+100_000, "ag_after", "after", nil, nil, 0)

	rg := Range{From: time.UnixMilli(base), To: time.UnixMilli(base + 100_000)}
	r := NewReader(db)

	var want bytes.Buffer
	if err := legacyExportCSV(ctx, db, &want, rg, exportRowCap); err != nil {
		t.Fatalf("legacy export: %v", err)
	}
	if got := strings.Count(want.String(), "\n"); got < 8 {
		t.Fatalf("legacy export has %d newlines, want at least 8 (header + 7 rows): %q", got, want.String())
	}
	if !strings.HasPrefix(want.String(), exportCSVHeader) {
		t.Fatalf("header changed: %q", want.String())
	}
	if !strings.Contains(want.String(), `"say ""hi"", then`+"\n"+`leave"`) {
		t.Fatalf("escaped reason missing: %q", want.String())
	}
	if strings.Contains(want.String(), "ag_before") || strings.Contains(want.String(), "ag_after") {
		t.Fatalf("rows outside the range exported: %q", want.String())
	}

	for _, pageSize := range []int{1, 2, 3, 7, 8, 1000} {
		var got bytes.Buffer
		if err := r.exportCSV(ctx, &got, rg, pageSize, exportRowCap); err != nil {
			t.Fatalf("pageSize=%d: %v", pageSize, err)
		}
		if got.String() != want.String() {
			t.Fatalf("pageSize=%d: paged export differs\n got: %q\nwant: %q", pageSize, got.String(), want.String())
		}
	}

	var pub bytes.Buffer
	if err := r.ExportCSV(ctx, &pub, rg); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if pub.String() != want.String() {
		t.Fatalf("ExportCSV differs\n got: %q\nwant: %q", pub.String(), want.String())
	}

	// The row cap cuts the dump at the same row, including mid-page.
	for _, rowCap := range []int{1, 2, 5} {
		var capWant, capGot bytes.Buffer
		if err := legacyExportCSV(ctx, db, &capWant, rg, rowCap); err != nil {
			t.Fatalf("legacy cap=%d: %v", rowCap, err)
		}
		if err := r.exportCSV(ctx, &capGot, rg, 2, rowCap); err != nil {
			t.Fatalf("cap=%d: %v", rowCap, err)
		}
		if capGot.String() != capWant.String() {
			t.Fatalf("cap=%d: paged export differs\n got: %q\nwant: %q", rowCap, capGot.String(), capWant.String())
		}
	}

	// An empty range still writes the header.
	var empty bytes.Buffer
	emptyRange := Range{From: time.UnixMilli(base + 200_000), To: time.UnixMilli(base + 300_000)}
	if err := r.exportCSV(ctx, &empty, emptyRange, 2, exportRowCap); err != nil {
		t.Fatalf("empty range: %v", err)
	}
	if empty.String() != exportCSVHeader {
		t.Fatalf("empty range output = %q, want only the header", empty.String())
	}
}

// TestExportCSVKeysetTies puts many rows on one ts across page boundaries:
// each row comes out exactly once, in (ts, event_id) order.
func TestExportCSVKeysetTies(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	base := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < 5; i++ {
		insertExportRow(t, db, base+10, "ag_tie", "tie "+strconv.Itoa(i), nil, nil, 0)
	}
	insertExportRow(t, db, base+20, "ag_late", "late", nil, nil, 0)
	insertExportRow(t, db, base, "ag_early", "early", nil, nil, 0)

	rows, err := db.QueryContext(ctx, `SELECT event_id FROM call_events ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	var wantIDs []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		wantIDs = append(wantIDs, strconv.FormatInt(id, 10))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(wantIDs) != 7 {
		t.Fatalf("seeded %d rows, want 7", len(wantIDs))
	}

	rg := Range{From: time.UnixMilli(base), To: time.UnixMilli(base + 1000)}
	for _, pageSize := range []int{1, 2, 3, 5} {
		var out bytes.Buffer
		if err := NewReader(db).exportCSV(ctx, &out, rg, pageSize, exportRowCap); err != nil {
			t.Fatalf("pageSize=%d: %v", pageSize, err)
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if lines[0]+"\n" != exportCSVHeader {
			t.Fatalf("pageSize=%d: header = %q", pageSize, lines[0])
		}
		var gotIDs []string
		for _, line := range lines[1:] {
			gotIDs = append(gotIDs, strings.SplitN(line, ",", 2)[0])
		}
		if strings.Join(gotIDs, " ") != strings.Join(wantIDs, " ") {
			t.Fatalf("pageSize=%d: event ids %v, want %v", pageSize, gotIDs, wantIDs)
		}
	}
}

// TestCostOrdersByTotalTokens pins the Cost sort: largest tokens in + out
// first. The old "ORDER BY 3 + 4" sorted by a constant.
func TestCostOrdersByTotalTokens(t *testing.T) {
	r, db := newTestRecorder(t)
	now := time.Now().UnixMilli()
	evs := []Event{
		// a.tool: 100 tokens in, none out = 100.
		{TS: now - 3000, Upstream: "u", ShortName: "a", ToolName: "a.tool", Outcome: OutcomeOK, ArgsSizeBytes: 400},
		// b.tool: 10 in + 500 out = 510.
		{TS: now - 2000, Upstream: "u", ShortName: "b", ToolName: "b.tool", Outcome: OutcomeOK, ArgsSizeBytes: 40, ResultSizeBytes: 2000},
		// c.tool: 50 in + 60 out = 110.
		{TS: now - 1000, Upstream: "u", ShortName: "c", ToolName: "c.tool", Outcome: OutcomeOK, ArgsSizeBytes: 200, ResultSizeBytes: 240},
	}
	if err := r.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	got, err := NewReader(db).Cost(context.Background(), Range{From: time.UnixMilli(now - time.Hour.Milliseconds())})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range got {
		order = append(order, fmt.Sprintf("%s:%d+%d", c.ToolName, c.TokensIn, c.TokensOut))
	}
	want := "b.tool:10+500 c.tool:50+60 a.tool:100+0"
	if strings.Join(order, " ") != want {
		t.Fatalf("Cost order = %v, want %s", order, want)
	}
}

package approval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// backfillSQL returns the decided_by_user_id backfill statement from the
// 0025 migration file, so the test runs the SQL that ships.
func backfillSQL(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "store", "migrations", "0025_audit_actors.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	src := string(body)
	at := strings.Index(src, "SET decided_by_user_id")
	if at < 0 {
		t.Fatal("migration no longer backfills decided_by_user_id")
	}
	start := strings.LastIndex(src[:at], "UPDATE audit_events")
	end := strings.Index(src[at:], ";")
	if start < 0 || end < 0 {
		t.Fatal("cannot isolate the backfill statement")
	}
	return src[start : at+end+1]
}

// The backfill names the decider only on an approval's decision rows
// (call.allowed / call.denied), and only when a person decided.
func TestMigration0025Backfill_OnlyDecisionRowsOfHumanDecisions(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	for _, a := range []struct{ id, decidedBy string }{
		{"ap_human", "u_person"},
		{"ap_rule", "rule:ar_1"},
		{"ap_token", "token"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO approval_requests(id, agent_id, upstream_name, tool_name,
            arguments, reason, status, decided_by, created_at, expires_at)
            VALUES(?, 'ag_1', 'up', 't', '{}', 'r', 'allowed', ?, 1, 2)`, a.id, a.decidedBy); err != nil {
			t.Fatalf("seed approval %s: %v", a.id, err)
		}
	}
	rows := []struct{ id, eventType, approvalID string }{
		{"ev_start", "call.start", "ap_human"},
		{"ev_allowed", "call.allowed", "ap_human"},
		{"ev_denied", "call.denied", "ap_human"},
		{"ev_succeeded", "call.succeeded", "ap_human"},
		{"ev_rule_allowed", "call.allowed", "ap_rule"},
		{"ev_token_allowed", "call.allowed", "ap_token"},
		{"ev_no_approval", "call.allowed", ""},
	}
	for i, r := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO audit_events(id, ts, event_type, approval_id)
            VALUES(?, ?, ?, ?)`, r.id, 1000+i, r.eventType, nullStr(r.approvalID)); err != nil {
			t.Fatalf("seed event %s: %v", r.id, err)
		}
	}

	if _, err := db.ExecContext(ctx, backfillSQL(t)); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	want := map[string]string{
		"ev_start": "", "ev_allowed": "u_person", "ev_denied": "u_person", "ev_succeeded": "",
		"ev_rule_allowed": "", "ev_token_allowed": "", "ev_no_approval": "",
	}
	for id, w := range want {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT COALESCE(decided_by_user_id, '') FROM audit_events WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != w {
			t.Errorf("%s: decided_by_user_id = %q, want %q", id, got, w)
		}
	}
}

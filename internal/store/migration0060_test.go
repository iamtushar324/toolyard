package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// migration0060 is the callback fan-out migration (v0.1 slice D).
const migration0060 = "0060_callback_fanout.sql"

// seed0060 is a database with callbacks in every state before 0060: two
// receivers, two subscriptions, a pending, a delivering (claimed) and a
// delivered event, and their attempts.
var seed0060 = []string{
	`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES('rq_1','ag_1','access','approved','soon','{}',1,1,9999999999999)`,
	`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES('rq_2','ag_1','question','pending','soon','{}',1,1,9999999999999)`,
	`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES('cb_push','ag_1','','push','https://receiver.example/hook','enc',1,'push')`,
	`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES('cb_pull','ag_1','mac','pull','','enc',1,'pull')`,
	`INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES('rq_1','cb_push',1)`,
	`INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES('rq_2','cb_pull',1)`,
	`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,attempts,next_attempt_at) VALUES('evt_pending','rq_2',2,'cb_pull',1,'{"type":"inbox.decision"}','pending',0,1000)`,
	`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,attempts,next_attempt_at,claim_until) VALUES('evt_delivering','rq_1',2,'cb_push',1,'{"type":"inbox.decision"}','delivering',2,2000,99999)`,
	`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,attempts,next_attempt_at,delivered_at) VALUES('evt_delivered','rq_1',3,'cb_push',1,'{"type":"inbox.expired"}','delivered',1,3000,3500)`,
	`INSERT INTO callback_attempts(event_id,attempt,attempted_at,http_status,outcome) VALUES('evt_delivering',1,2001,500,'http_500')`,
	`INSERT INTO callback_attempts(event_id,attempt,attempted_at,http_status,outcome) VALUES('evt_delivered',1,3500,200,'delivered')`,
}

// check0060 checks what 0060 must keep and add, on either engine.
func check0060(t *testing.T, db *sql.DB) {
	t.Helper()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM callback_subscriptions`); n != 2 {
		t.Fatalf("subscriptions: %d", n)
	}
	if n := count(`SELECT count(*) FROM callback_outbox`); n != 3 {
		t.Fatalf("outbox: %d", n)
	}
	if n := count(`SELECT count(*) FROM callback_attempts`); n != 2 {
		t.Fatalf("attempts: %d", n)
	}
	rows, err := db.Query(`SELECT id,status,attempts,claim_until,dedupe_key,created_at,event_type FROM callback_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		status   string
		attempts int
		claim    int64
		dedupe   string
		created  int64
		kind     string
	}
	got := map[string]row{}
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.status, &r.attempts, &r.claim, &r.dedupe, &r.created, &r.kind); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	rows.Close()
	want := map[string]row{
		"evt_pending":    {"pending", 0, 0, "inbox:rq_2:2", 1000, ""},
		"evt_delivering": {"delivering", 2, 99999, "inbox:rq_1:2", 2000, ""},
		"evt_delivered":  {"delivered", 1, 0, "inbox:rq_1:3", 3500, ""},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %+v, want %+v", id, got[id], w)
		}
	}
	var events string
	var v3 int
	if err := db.QueryRow(`SELECT events,v3 FROM callback_receivers WHERE id='cb_push'`).Scan(&events, &v3); err != nil || events != "inbox.decision,inbox.expired,inbox.cancelled" || v3 != 0 {
		t.Fatalf("existing receiver: %q %d %v", events, v3, err)
	}
	// The new keys: a second receiver may follow rq_1, and one logical
	// event per receiver stays unique.
	if _, err := db.Exec(`INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES('rq_1','cb_pull',1)`); err != nil {
		t.Fatalf("second receiver for one item: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at,dedupe_key) VALUES('evt_dup','rq_1',2,'cb_push',1,'{}',0,'inbox:rq_1:2')`); err == nil {
		t.Fatal("a repeated logical event for one receiver was stored")
	}
	if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at,dedupe_key) VALUES('evt_other','rq_1',2,'cb_pull',1,'{}',0,'inbox:rq_1:2')`); err != nil {
		t.Fatalf("the same logical event for another receiver: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO callback_attempts(event_id,attempt,attempted_at,outcome) VALUES('evt_missing',1,1,'x')`); err == nil {
		t.Fatal("an attempt without its event was stored: the foreign key was lost")
	}
}

// TestMigration0060KeepsCallbackData (SQLite): a database with callbacks
// in every state upgrades in place; no row, key or index is lost.
func TestMigration0060KeepsCallbackData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	sdb, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := store.MigrationFilesIn("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= migration0060 {
			break
		}
		body, err := store.MigrationFile("sqlite", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sdb.Exec(body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := sdb.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, 1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range seed0060 {
		if _, err := sdb.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open with 0060 pending: %v", err)
	}
	defer db.Close()
	var problems int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&problems); err != nil || problems != 0 {
		t.Fatalf("foreign_key_check: %d %v", problems, err)
	}
	for _, ix := range []string{"callback_outbox_due", "callback_outbox_request", "callback_outbox_created"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, ix).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s: %d %v", ix, n, err)
		}
	}
	check0060(t, db.DB)
}

// TestMigration0060KeepsCallbackDataPostgres: the same upgrade on Postgres,
// from the baseline and every migration before 0060.
func TestMigration0060KeepsCallbackDataPostgres(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 2, NoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := store.MigrationFilesIn("postgres")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(name string) {
		t.Helper()
		body, err := store.MigrationFile("postgres", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	apply(store.PostgresBaseline)
	for _, name := range names {
		if name != store.PostgresBaseline && name < migration0060 {
			apply(name)
		}
	}
	for _, q := range seed0060 {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	apply(migration0060)
	for _, ix := range []string{"callback_outbox_due", "callback_outbox_request", "callback_outbox_created"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname=?`, ix).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s: %d %v", ix, n, err)
		}
	}
	var pk string
	if err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='callback_subscriptions'::regclass AND contype='p'`).Scan(&pk); err != nil || pk != "PRIMARY KEY (request_id, receiver_id)" {
		t.Fatalf("subscriptions primary key: %q %v", pk, err)
	}
	var old int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid='callback_outbox'::regclass AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (request_id, decision_revision)'`).Scan(&old); err != nil || old != 0 {
		t.Fatalf("the old unique key survived: %d %v", old, err)
	}
	check0060(t, db.DB)
}

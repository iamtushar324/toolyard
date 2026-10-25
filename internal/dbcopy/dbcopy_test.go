package dbcopy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// These tests copy a seeded SQLite source into the fresh, migrated Postgres
// database storetest gives each test (TOOLYARD_TEST_DB=postgres).

// plaintexts sealed or stored in the source: no report may show one.
var plaintexts = []string{"ag_identity.secret-token", "gho_access", "ghr_refresh", "value-one", "value-two", "123:bot", "lake-password"}

type source struct {
	dir     string
	dbPath  string
	blobDir string
	keys    map[string][]byte
	blobs   []string // sha256 names with a row and a file
	orphan  string   // a blob file no row names
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// seedSource builds a stopped SQLite installation: master keys and their
// canaries, sealed values for every key, signing keys, an identity-column
// table, a REAL column, a dropped lake column, rows the copy skips, and
// two blobs plus an orphan file.
func seedSource(t *testing.T) *source {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s := &source{dir: dir, dbPath: filepath.Join(dir, "toolyard.db"), blobDir: filepath.Join(dir, "inbox", "blobs")}
	db, err := store.Open(s.dbPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	keys, err := keyguard.LoadAll(ctx, keyguard.Config{
		DB: db.DB, DataDir: t.TempDir(), Mode: keyguard.CreateIfFresh,
		Getenv: func(string) string { return "" }, Logf: t.Logf,
	}, keyguard.ServeKeys())
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	s.keys = keys
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	seal := func(key, plaintext string, aad []byte) string {
		t.Helper()
		c, err := sealbox.NewCipher(keys[key])
		if err != nil {
			t.Fatal(err)
		}
		v, err := c.Seal([]byte(plaintext), aad)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const now = int64(1_700_000_000_000)

	u, err := identity.New(db).CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO agents(id, name, owner_user, token_hash, kind, created_at) VALUES('ag_identity', 'identity', ?, 'hash-1', 'identity', ?)`, u.ID, now)
	// internal/identitykeys seals with AAD = the user id.
	exec(`INSERT INTO user_identity_keys(user_id, agent_id, key_hash, key_sealed, created_at, updated_at) VALUES(?, 'ag_identity', 'hash-1', ?, ?, ?)`,
		u.ID, seal(keyguard.IdentityKey, "ag_identity.secret-token", []byte(u.ID)), now, now)
	exec(`INSERT INTO upstream_servers(name, transport, url, enabled, last_status, created_at, updated_at) VALUES('github', 'http', 'http://127.0.0.1:1/mcp', 1, 'ok', ?, ?)`, now, now)
	exec(`INSERT INTO oauth_tokens(upstream_name, access_token_enc, refresh_token_enc, obtained_at) VALUES('github', ?, ?, ?)`,
		seal(keyguard.OAuthKey, "gho_access", oauth.AAD("github", "access")),
		seal(keyguard.OAuthKey, "ghr_refresh", oauth.AAD("github", "refresh")), now)
	sc, err := sealbox.NewCipher(keys[keyguard.SecretsKey])
	if err != nil {
		t.Fatal(err)
	}
	svc := secrets.New(db, sc, nil)
	if _, err := svc.Create(ctx, "API_KEY", "value-one", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "OTHER_KEY", "value-two", ""); err != nil {
		t.Fatal(err)
	}
	// internal/chatnotify/telegram: a sealed bot token kept as a JSON string.
	tok, _ := json.Marshal(seal(keyguard.SecretsKey, "123:bot", []byte("toolyard.chat.v1|telegram|bot_token")))
	exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('telegram_bot_token', ?, ?)`, string(tok), now)
	exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('theme', '"dark"', ?)`, now)
	exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('clickhouse_password', '"lake-password"', ?)`, now) // skipped
	exec(`INSERT INTO user_server_access(user_id, server, created_at) VALUES(?, 'github', ?)`, u.ID, now)
	exec(`INSERT INTO user_server_access(user_id, server, created_at) VALUES(?, 'lake', ?)`, u.ID, now) // skipped
	for _, purpose := range []string{"grant_sign", "approval_sign", "vapid"} {
		exec(`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?, ?, ?, ?, ?)`,
			"key_"+purpose, purpose, randomBytes(t, 64), randomBytes(t, 32), now)
	}
	exec(`INSERT INTO federation_instance(id) VALUES('tyi_test')`)
	for _, id := range []int64{1, 2, 7, 9} { // an identity column with a gap
		exec(`INSERT INTO inbox_dry_runs(id, agent_id, title, flags, created_at) VALUES(?, 'ag_identity', ?, '[]', ?)`, id, fmt.Sprintf("draft %d", id), now+id)
	}
	// AUTOINCREMENT: sqlite_sequence keeps 9 after the delete, so SQLite
	// would hand out 10 next, never 8 or 9 again.
	exec(`DELETE FROM inbox_dry_runs WHERE id = 9`)
	exec(`INSERT INTO call_events(ts, upstream, short_name, tool_name, outcome, reason_quality) VALUES(?, 'github', 'x', 'github.x', 'ok', 0.75)`, now)
	exec(`INSERT INTO event_sources(id, name, kind, created_at, updated_at) VALUES('evs_1', 'hooks', 'webhook', ?, ?)`, now, now)
	exec(`INSERT INTO events(id, source_id, type, summary, created_at, received_at, lake_synced) VALUES('ev_1', 'evs_1', 'note', 'A note.', ?, ?, 1)`, now, now)

	if err := os.MkdirAll(s.blobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The second blob spans three 1 MiB chunks in the Postgres store.
	for _, body := range [][]byte{[]byte("first blob"), append(randomBytes(t, 2<<20), randomBytes(t, 100)...)} {
		sha := sha256Hex(body)
		if err := os.WriteFile(filepath.Join(s.blobDir, sha), body, 0o600); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES(?, 'application/octet-stream', ?, ?, ?)`, sha, len(body), "upload:"+sha, now)
		s.blobs = append(s.blobs, sha)
	}
	orphan := []byte("no row names me")
	s.orphan = sha256Hex(orphan)
	if err := os.WriteFile(filepath.Join(s.blobDir, s.orphan), orphan, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

// open opens the source as the command does: read-only.
func (s *source) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenReadOnly(s.dbPath)
	if err != nil {
		t.Fatalf("open source read-only: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableReport(t *testing.T, r *Report, name string) TableReport {
	t.Helper()
	for _, tr := range r.Tables {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("no report for table %s", name)
	return TableReport{}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func mustOK(t *testing.T, what string, r *Report, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v (problems: %v)", what, err, r.Problems)
	}
	if !r.OK() {
		t.Fatalf("%s: problems: %v", what, r.Problems)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plaintexts {
		if strings.Contains(string(b), p) {
			t.Fatalf("%s: the report shows a value (%q)", what, p)
		}
	}
}

func count(t *testing.T, db *store.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestCopyThenVerify(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := seedSource(t)
	src := s.open(t)
	dst := storetest.Open(t)

	rep, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{})
	mustOK(t, "copy", rep, err)
	// telegram_bot_token and theme; clickhouse_password is skipped.
	if tr := tableReport(t, rep, "system_settings"); tr.SkippedRows != 1 || tr.CopiedRows != 2 || !tr.Match {
		t.Fatalf("system_settings: %+v", tr)
	}
	if tr := tableReport(t, rep, "user_server_access"); tr.SkippedRows != 1 || tr.CopiedRows != 1 {
		t.Fatalf("user_server_access: %+v", tr)
	}
	if !contains(rep.DroppedColumns, "events.lake_synced") {
		t.Fatalf("dropped columns: %v", rep.DroppedColumns)
	}
	for _, id := range []string{"inbox_dry_runs.id", "call_events.event_id"} {
		if !contains(rep.IdentityReset, id) {
			t.Fatalf("identity %s not reset: %v", id, rep.IdentityReset)
		}
	}
	excluded := map[string]bool{}
	for _, e := range rep.ExcludedTables {
		excluded[e.Name] = true
	}
	for _, n := range []string{"schema_migrations", "lake_ingest_runs", "lake_saved_queries", "inbox_blob_objects", "inbox_blob_chunks"} {
		if !excluded[n] {
			t.Fatalf("%s not reported as excluded: %+v", n, rep.ExcludedTables)
		}
	}
	if rep.Blobs.Rows != 2 || rep.Blobs.Copied != 2 || len(rep.Blobs.OrphanFiles) != 1 || rep.Blobs.OrphanFiles[0] != s.orphan {
		t.Fatalf("blobs: %+v", rep.Blobs)
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM system_settings WHERE key = 'clickhouse_password'`); n != 0 {
		t.Fatal("a skipped row reached the target")
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM user_server_access WHERE server = 'lake'`); n != 0 {
		t.Fatal("a skipped lake access row reached the target")
	}
	var quality float64
	if err := dst.QueryRowContext(ctx, `SELECT reason_quality FROM call_events`).Scan(&quality); err != nil || quality != 0.75 {
		t.Fatalf("REAL column: %v %v", quality, err)
	}

	vrep, err := Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{})
	mustOK(t, "verify", vrep, err)
	if len(vrep.Tables) != len(rep.Tables) {
		t.Fatalf("verify compared %d tables, copy wrote %d", len(vrep.Tables), len(rep.Tables))
	}
	values := map[string]int{}
	for _, sr := range vrep.Sealed {
		if !sr.Match || !sr.KeyLoaded || sr.SourceValues != sr.TargetValues {
			t.Fatalf("sealed %s.%s: %+v", sr.Table, sr.Column, sr)
		}
		values[sr.Table+"."+sr.Column] = sr.SourceValues
	}
	for col, want := range map[string]int{
		"user_identity_keys.key_sealed": 1, "oauth_tokens.access_token_enc": 1, "oauth_tokens.refresh_token_enc": 1,
		"secrets.value_enc": 2, "system_settings.value": 1,
	} {
		if values[col] != want {
			t.Fatalf("%s: %d sealed values opened, want %d (%v)", col, values[col], want, values)
		}
	}
	named := map[string]NamedRowReport{}
	for _, n := range vrep.NamedRows {
		named[n.Name] = n
	}
	for _, n := range []string{"server_keys grant_sign", "server_keys approval_sign", "server_keys vapid", "federation_instance",
		"key_canaries identity.key", "key_canaries oauth.key", "key_canaries secrets.key", "key_canaries session.key"} {
		r, ok := named[n]
		if !ok || !r.SourcePresent || !r.TargetPresent || !r.Match {
			t.Fatalf("%s: %+v", n, r)
		}
		if strings.HasPrefix(n, "key_canaries") && (r.KeyMatches == nil || !*r.KeyMatches) {
			t.Fatalf("%s: the key does not open the target's canary", n)
		}
	}
	if vrep.Blobs.Verified != 2 {
		t.Fatalf("blobs verified: %+v", vrep.Blobs)
	}

	// A wrong secrets key fails: its values and its canary do not open.
	wrong := map[string][]byte{}
	for k, v := range s.keys {
		wrong[k] = v
	}
	wrong[keyguard.SecretsKey] = randomBytes(t, sealbox.MasterKeySize)
	bad, err := Verify(ctx, src, s.blobDir, dst, wrong, VerifyOptions{})
	if err != nil {
		t.Fatalf("verify with a wrong key: %v", err)
	}
	if bad.OK() {
		t.Fatal("verify passed with a wrong secrets key")
	}
	for _, sr := range bad.Sealed {
		// A column with no sealed values matches trivially; every column
		// that holds values must fail under the wrong key.
		if sr.Key == keyguard.SecretsKey && sr.SourceValues > 0 && sr.Match {
			t.Fatalf("%s.%s matched under a wrong key", sr.Table, sr.Column)
		}
		if sr.Table == "secrets" && (sr.SourceUnopened != 2 || sr.TargetUnopened != 2) {
			t.Fatalf("secrets under a wrong key: %+v", sr)
		}
	}
	for _, n := range bad.NamedRows {
		if n.Name == "key_canaries secrets.key" && (n.KeyMatches == nil || *n.KeyMatches) {
			t.Fatalf("the secrets canary opened under a wrong key: %+v", n)
		}
	}

	// A second copy into the now-full target is refused and writes nothing.
	again, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{})
	if err == nil || !strings.Contains(err.Error(), "not empty") || again.OK() || len(again.NonEmptyTargetTables) == 0 {
		t.Fatalf("second copy: %v %+v", err, again.NonEmptyTargetTables)
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM inbox_dry_runs`); n != 3 {
		t.Fatalf("the refused copy changed the target: %d dry runs", n)
	}

	// -truncate-target repeats the rehearsal: the target is emptied and
	// filled again in one transaction.
	rerun, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{TruncateTarget: true})
	mustOK(t, "copy -truncate-target", rerun, err)
	if !rerun.TruncatedTarget || rerun.Blobs.Copied != 2 {
		t.Fatalf("rerun: truncated %v, blobs %+v", rerun.TruncatedTarget, rerun.Blobs)
	}
	vrep, err = Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{})
	mustOK(t, "verify after the rerun", vrep, err)

	// New rows continue after the largest id SQLite handed out
	// (sqlite_sequence: 9, a deleted row), not after the largest copied
	// id (7).
	var id int64
	if err := dst.QueryRowContext(ctx, `INSERT INTO inbox_dry_runs(agent_id, title, flags, created_at) VALUES('ag_identity', 'new', '[]', 1) RETURNING id`).Scan(&id); err != nil || id != 10 {
		t.Fatalf("next dry run id = %d (%v), want 10", id, err)
	}
	if _, err := dst.ExecContext(ctx, `DELETE FROM inbox_dry_runs WHERE id = 10`); err != nil {
		t.Fatal(err)
	}

	// Tampering with one target row fails verify, on that table only.
	if _, err := dst.ExecContext(ctx, `UPDATE system_settings SET value = '"light"' WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}
	tampered, err := Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tampered.OK() {
		t.Fatal("verify passed after a target row changed")
	}
	for _, tr := range tampered.Tables {
		if tr.Match == (tr.Name == "system_settings") {
			t.Fatalf("table %s: match = %v", tr.Name, tr.Match)
		}
	}

	// So does a changed blob chunk (same length, other bytes).
	if _, err := dst.ExecContext(ctx, `UPDATE inbox_blob_chunks SET data = ? WHERE sha256 = ? AND seq = 0`, []byte("FIRST BLOB"), s.blobs[0]); err != nil {
		t.Fatal(err)
	}
	tampered, err = Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tampered.Blobs.TargetMismatched) != 1 || tampered.Blobs.TargetMismatched[0] != s.blobs[0] {
		t.Fatalf("a changed blob was not caught: %+v", tampered.Blobs)
	}
}

// A value that does not fit its target column stops the copy with an
// error naming the table, the column and the primary key, never the value,
// and nothing is written.
func TestCopyTypeErrorNamesTheRowNotTheValue(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "toolyard.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES('theme', '"dark"', 1)`); err != nil {
		t.Fatal(err)
	}
	// SQLite keeps text that is not a number as text in an INTEGER column.
	if _, err := db.ExecContext(ctx, `INSERT INTO inbox_dry_runs(id, agent_id, title, flags, created_at) VALUES(41, 'ag', 't', '[]', ?)`, notShown); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	src, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := storetest.Open(t)

	rep, err := Copy(ctx, src, filepath.Join(dir, "inbox", "blobs"), dst, CopyOptions{})
	var ce *CellError
	if !errors.As(err, &ce) || ce.Table != "inbox_dry_runs" || ce.Column != "created_at" {
		t.Fatalf("copy error: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "id=41") || strings.Contains(msg, notShown) {
		t.Fatalf("the error must name the key and not the value: %s", msg)
	}
	if rep.OK() {
		t.Fatal("the report is OK after a failed copy")
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM system_settings`); n != 0 {
		t.Fatalf("a failed copy left %d rows behind", n)
	}
}

// Copy refuses, before writing, a source at other migrations and a blob
// row without its file.
func TestCopyRefusesBeforeWriting(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, want string
		break_     func(t *testing.T, db *store.DB)
	}{
		{"older migrations", "different migrations", func(t *testing.T, db *store.DB) {
			if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = '0039_key_canaries.sql'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing blob file", "no file", func(t *testing.T, db *store.DB) {
			if _, err := db.ExecContext(ctx, `INSERT INTO inbox_blobs(sha256, content_type, size, source_url, created_at) VALUES(?, 'text/plain', 1, 'x', 1)`, sha256Hex([]byte("gone"))); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "toolyard.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES('theme', '"dark"', 1)`); err != nil {
				t.Fatal(err)
			}
			c.break_(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			src, err := store.OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			dst := storetest.Open(t)
			rep, err := Copy(ctx, src, filepath.Join(dir, "inbox", "blobs"), dst, CopyOptions{})
			if err == nil || !strings.Contains(err.Error(), c.want) || rep.OK() {
				t.Fatalf("copy: %v", err)
			}
			if n := count(t, dst, `SELECT COUNT(*) FROM system_settings`); n != 0 {
				t.Fatalf("a refused copy wrote %d rows", n)
			}
		})
	}
}

// openEmptyTarget creates an empty Postgres database (C collation, no
// toolyard table) and opens it without migrations, as db copy does.
func openEmptyTarget(t *testing.T) (string, *store.DB) {
	t.Helper()
	dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
	if err != nil {
		t.Fatalf("create an empty database: %v", err)
	}
	dst, err := store.OpenPostgresWith(context.Background(), dbURL, store.PostgresOptions{MaxConns: 4, NoMigrate: true})
	if err != nil {
		t.Fatalf("open the empty database: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	return dbURL, dst
}

// Copy into an empty database (no toolyard table, no migration) checks
// everything, then creates the schema and copies; verify then passes.
func TestCopyIntoAnEmptyDatabase(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := seedSource(t)
	src := s.open(t)
	_, dst := openEmptyTarget(t)

	rep, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{})
	mustOK(t, "copy into an empty database", rep, err)
	if rep.Blobs.Copied != 2 || !rep.MigrationsMatch {
		t.Fatalf("copy: blobs %+v, migrations match %v", rep.Blobs, rep.MigrationsMatch)
	}
	vrep, err := Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{})
	mustOK(t, "verify", vrep, err)
}

// A database that records no toolyard migration but holds tables (another
// application's, say) is refused before anything is written: no toolyard
// table is created in it.
func TestCopyRefusesADatabaseWithOtherTables(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := seedSource(t)
	src := s.open(t)
	_, dst := openEmptyTarget(t)
	if _, err := dst.ExecContext(ctx, `CREATE TABLE other_app_users (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	rep, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{TruncateTarget: true})
	if err == nil || !strings.Contains(err.Error(), "not an empty database") || rep.OK() {
		t.Fatalf("copy into a database with another application's table: %v", err)
	}
	for _, table := range []string{"schema_migrations", "system_settings", "inbox_blob_objects"} {
		if has, err := store.TableExists(ctx, dst, store.Postgres, table); err != nil || has {
			t.Fatalf("the refused copy created %s (%v)", table, err)
		}
	}
	if has, err := store.TableExists(ctx, dst, store.Postgres, "other_app_users"); err != nil || !has {
		t.Fatalf("the other table is gone (%v)", err)
	}
}

// A source at other migrations than this toolyard version is refused
// before an empty target is migrated.
func TestCopyRefusesAnOlderSourceBeforeMigratingAnEmptyTarget(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "toolyard.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = '0039_key_canaries.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	src, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_, dst := openEmptyTarget(t)
	rep, err := Copy(ctx, src, filepath.Join(dir, "inbox", "blobs"), dst, CopyOptions{})
	if err == nil || !strings.Contains(err.Error(), "different migrations") || rep.OK() {
		t.Fatalf("copy from an older source: %v", err)
	}
	if has, err := store.TableExists(ctx, dst, store.Postgres, "schema_migrations"); err != nil || has {
		t.Fatalf("the refused copy migrated the target (%v)", err)
	}
}

// The tables and the blobs commit together: a copy stopped at the last
// step (here the source guard) leaves no row and no blob, and a rerun then
// copies everything.
func TestCopyIsAllOrNothing(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := seedSource(t)
	src := s.open(t)
	dst := storetest.Open(t)

	guardErr := errors.New("the source changed: toolyard.db-wal appeared")
	rep, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{SourceGuard: func() error { return guardErr }})
	if !errors.Is(err, guardErr) || rep.OK() {
		t.Fatalf("copy with a failing guard: %v", err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM inbox_dry_runs`, `SELECT COUNT(*) FROM inbox_blobs`,
		`SELECT COUNT(*) FROM inbox_blob_objects`, `SELECT COUNT(*) FROM inbox_blob_chunks`} {
		if n := count(t, dst, q); n != 0 {
			t.Fatalf("a rolled-back copy left rows: %s = %d", q, n)
		}
	}
	rep, err = Copy(ctx, src, s.blobDir, dst, CopyOptions{SourceGuard: func() error { return nil }})
	mustOK(t, "rerun", rep, err)
	if rep.Blobs.Copied != 2 {
		t.Fatalf("rerun blobs: %+v", rep.Blobs)
	}
	vrep, err := Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{SourceGuard: func() error { return guardErr }})
	if !errors.Is(err, guardErr) || vrep.OK() {
		t.Fatalf("verify with a failing guard: %v", err)
	}
}

// sqlExecer runs blob store statements on a database/sql transaction.
func sqlExecer(tx *sql.Tx) blobstore.Execer {
	return func(ctx context.Context, query string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
}

// A blob file whose bytes changed after the check (same size, other bytes)
// fails copyBlobs before the transaction can commit; an unchanged one is
// stored and reads back. The blob tables exist on both engines.
func TestCopyBlobsRehashes(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	dir := t.TempDir()
	good := append(randomBytes(t, blobstore.ChunkSize), randomBytes(t, 10)...)
	goodPath := filepath.Join(dir, "good")
	if err := os.WriteFile(goodPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	orig := []byte("the bytes that were checked")
	changedPath := filepath.Join(dir, "changed")
	if err := os.WriteFile(changedPath, []byte("THE BYTES THAT WERE CHECKED"), 0o600); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = copyBlobs(ctx, []sourceBlob{
		{sha: sha256Hex(good), path: goodPath, size: int64(len(good))},
		{sha: sha256Hex(orig), path: changedPath, size: int64(len(orig))},
	}, sqlExecer(tx))
	if err == nil || !strings.Contains(err.Error(), "changed during the copy") {
		t.Fatalf("copy of a changed blob file: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM inbox_blob_objects`); n != 0 {
		t.Fatalf("%d blobs left after the rollback", n)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	copied, present, err := copyBlobs(ctx, []sourceBlob{{sha: sha256Hex(good), path: goodPath, size: int64(len(good))}}, sqlExecer(tx))
	if err != nil || copied != 1 || present != 0 {
		t.Fatalf("copy of a good blob: %d %d %v", copied, present, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := blobstore.NewPostgres(db.DB).Open(ctx, sha256Hex(good))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := io.ReadAll(b)
	if err != nil || sha256Hex(got) != sha256Hex(good) {
		t.Fatalf("read back: %d bytes, %v", len(got), err)
	}
}

// Large async-job results (files under <data>/job-results, named by a key
// that hashes the job id before the result) are copied into the target's
// blob store and read back by verify. A row whose file is missing is
// listed, not copied, and stops nothing.
func TestCopyAndVerifyJobResults(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := seedSource(t)
	resultDir := filepath.Join(s.dir, "job-results")
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte(`{"content":[{"type":"text","text":"`), randomBytes(t, 300<<10)...)
	good := sha256Hex(append([]byte(jobs.BlobKeyPrefix("job_good")), body...))
	if err := os.WriteFile(filepath.Join(resultDir, good), body, 0o600); err != nil {
		t.Fatal(err)
	}
	gone := sha256Hex(append([]byte(jobs.BlobKeyPrefix("job_gone")), []byte("lost")...))
	// A row from before the key named the job (sha256 of "tool_job", a NUL
	// byte and the result).
	oldBody := []byte(`{"content":[{"type":"text","text":"from before the job id was in the key"}]}`)
	legacy := sha256Hex(append([]byte("tool_job\x00"), oldBody...))
	if err := os.WriteFile(filepath.Join(resultDir, legacy), oldBody, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, key string }{{"job_good", good}, {"job_gone", gone}, {"job_legacy", legacy}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO tool_jobs(id, agent_id, tool, upstream, params_hash, reason, via, status,
            result_blob, result_bytes, owner_instance, lease_until, created_at, finished_at, updated_at)
            VALUES(?, 'ag_1', 'alpha.get_big', 'alpha', 'h', 'test the job result copy', 'direct', 'succeeded', ?, ?, 'node', 1, 1, 2, 2)`,
			r.id, r.key, len(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	src := s.open(t)
	dst := storetest.Open(t)

	rep, err := Copy(ctx, src, s.blobDir, dst, CopyOptions{JobResultDir: resultDir})
	mustOK(t, "copy", rep, err)
	if jr := rep.JobResults; jr.Rows != 3 || jr.Copied != 2 || !contains(jr.MissingFiles, gone) || len(jr.MismatchedFiles) != 0 {
		t.Fatalf("job results copied: %+v", jr)
	}
	for _, key := range []string{good, legacy} {
		if ok, err := blobstore.NewPostgres(dst.DB).Exists(ctx, key); err != nil || !ok {
			t.Fatalf("job result %s is not in the target blob store: %v %v", key, ok, err)
		}
	}
	vrep, err := Verify(ctx, src, s.blobDir, dst, s.keys, VerifyOptions{JobResultDir: resultDir})
	mustOK(t, "verify", vrep, err)
	if jr := vrep.JobResults; jr.Verified != 2 || !contains(jr.MissingFiles, gone) {
		t.Fatalf("job results verified: %+v", jr)
	}
}

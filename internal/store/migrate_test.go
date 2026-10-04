package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func migrationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestMigrationNumbersUnique guards against two files sharing a number,
// which makes apply order depend on the slug instead of the number.
func TestMigrationNumbersUnique(t *testing.T) {
	seen := map[string]string{}
	for _, name := range migrationFiles(t) {
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %s has no NNNN_ prefix", name)
		}
		if prev, dup := seen[num]; dup {
			t.Errorf("migration number %s used by both %s and %s", num, prev, name)
		}
		seen[num] = name
	}
}

// TestLegacyMigrationNamesPointAtFiles keeps the rename map in step with the
// files on disk.
func TestLegacyMigrationNamesPointAtFiles(t *testing.T) {
	files := map[string]bool{}
	for _, name := range migrationFiles(t) {
		files[name] = true
	}
	for oldName, newName := range legacyMigrationNames {
		if files[oldName] {
			t.Errorf("legacy name %s still exists as a file", oldName)
		}
		if !files[newName] {
			t.Errorf("legacy name %s maps to missing file %s", oldName, newName)
		}
	}
}

// TestMigrateRenamesLegacyNames simulates a database migrated before the
// renumbering: its schema_migrations rows carry the old names. Reopening must
// rename them rather than re-apply the SQL (0014_oauth_authorize_params is an
// ALTER TABLE ADD COLUMN, which fails if run twice).
func TestMigrateRenamesLegacyNames(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "toolyard.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for oldName, newName := range legacyMigrationNames {
		if _, err := db.Exec(`UPDATE schema_migrations SET name = ? WHERE name = ?`, oldName, newName); err != nil {
			t.Fatalf("set legacy name %s: %v", oldName, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db with legacy migration names: %v", err)
	}
	defer db2.Close()

	rows, err := db2.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	files := migrationFiles(t)
	if len(got) != len(files) {
		t.Errorf("schema_migrations has %d rows; want %d", len(got), len(files))
	}
	for _, name := range files {
		if !got[name] {
			t.Errorf("schema_migrations missing %s", name)
		}
	}
}

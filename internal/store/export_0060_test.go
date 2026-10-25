package store

// Test hooks for migration0060_test.go (package store_test).

// MigrationFile returns one embedded migration: dir is "sqlite" or
// "postgres".
func MigrationFile(dir, name string) (string, error) {
	b, err := migrations.ReadFile("migrations/" + dir + "/" + name)
	return string(b), err
}

// MigrationFilesIn lists one embedded migrations directory in order.
func MigrationFilesIn(dir string) ([]string, error) { return migrationNames("migrations/" + dir) }

// PostgresBaseline is the Postgres baseline migration's file name.
const PostgresBaseline = pgBaseline

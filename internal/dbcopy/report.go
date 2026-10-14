// Package dbcopy moves a toolyard installation from SQLite to Postgres
// (toolyard db copy) and proves that the two databases hold the same data
// (toolyard db verify).
//
// Copy reads a stopped SQLite database and its blob folder, and loads them
// into an empty Postgres database (migrating it only when it records no
// migration yet): every table (COPY, parents before children) and every
// blob into the Postgres blob store tables, all in one transaction. Verify
// compares the two, without writing: row counts and a SHA-256
// of every table in primary-key order, every sealed value opened with its
// master key on both sides, the signing keys and key canaries by name, and
// every blob by its sha256.
//
// Both only read the source. Neither ever prints, logs or stores a cell
// value, a sealed value or a plaintext: reports and errors hold names,
// counts, hashes and booleans, and an error about a row names its table,
// column and primary key.
package dbcopy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Report is what Copy or Verify found. It holds names, counts, hashes and
// booleans only. OK reports whether nothing is wrong.
type Report struct {
	// Command is "copy" or "verify".
	Command string `json:"command"`

	// Migrations is the number of migration names the source records;
	// MigrationsMatch is true when the target records the same names.
	Migrations           int      `json:"migrations"`
	MigrationsMatch      bool     `json:"migrations_match"`
	SourceOnlyMigrations []string `json:"source_only_migrations,omitempty"`
	TargetOnlyMigrations []string `json:"target_only_migrations,omitempty"`

	// NonEmptyTargetTables are the target tables that held rows before a
	// copy; TruncatedTarget is true when -truncate-target emptied them.
	NonEmptyTargetTables []string `json:"non_empty_target_tables,omitempty"`
	TruncatedTarget      bool     `json:"truncated_target,omitempty"`

	// ExcludedTables are source tables never copied or compared.
	ExcludedTables []ExcludedTable `json:"excluded_tables"`
	// MissingTables are source tables the target does not have.
	MissingTables []string `json:"missing_tables,omitempty"`
	// TargetOnlyTables are target tables the source does not have.
	TargetOnlyTables []string `json:"target_only_tables,omitempty"`
	// DroppedColumns are source columns the target does not have
	// ("table.column"): their values are not copied.
	DroppedColumns []string `json:"dropped_columns"`
	// TargetOnlyColumns are target columns the source does not have.
	TargetOnlyColumns []string `json:"target_only_columns,omitempty"`
	// SkipRules describe the source rows never copied or compared.
	SkipRules []string `json:"skip_rules"`

	Tables []TableReport `json:"tables"`
	// IdentityReset are the identity columns whose sequence a copy moved
	// past the copied ids ("table.column").
	IdentityReset []string `json:"identity_reset,omitempty"`

	Sealed    []SealedReport   `json:"sealed,omitempty"`
	NamedRows []NamedRowReport `json:"named_rows,omitempty"`
	Blobs     BlobReport       `json:"blobs"`

	// Problems lists everything wrong, one line each (names and counts
	// only). The report is OK when it is empty.
	Problems []string `json:"problems"`
}

// ExcludedTable is a source table that is never copied or compared.
type ExcludedTable struct {
	Name       string `json:"name"`
	SourceRows int64  `json:"source_rows"`
	Why        string `json:"why"`
}

// TableReport is one copied or compared table.
type TableReport struct {
	Name string `json:"name"`
	// SourceRows counts the source rows copied or compared; SkippedRows
	// the source rows a skip rule left out.
	SourceRows  int64 `json:"source_rows"`
	SkippedRows int64 `json:"skipped_rows,omitempty"`
	// CopiedRows is what Postgres accepted (copy only).
	CopiedRows int64 `json:"copied_rows,omitempty"`
	// TargetRows and TargetSkippedRows are the target's counts, and the
	// SHA-256 values hash every row in primary-key order (verify only).
	TargetRows        int64  `json:"target_rows,omitempty"`
	TargetSkippedRows int64  `json:"target_skipped_rows,omitempty"`
	SourceSHA256      string `json:"source_sha256,omitempty"`
	TargetSHA256      string `json:"target_sha256,omitempty"`
	// Match: copy, every source row was written; verify, the two hashes
	// are equal.
	Match bool `json:"match"`
}

// SealedReport is one column of sealed values, opened on both sides.
type SealedReport struct {
	Key       string `json:"key"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	KeyLoaded bool   `json:"key_loaded"`
	// Values counts the sealed values; Unopened those the key did not
	// open (each one is a failure).
	SourceValues   int `json:"source_values"`
	TargetValues   int `json:"target_values"`
	SourceUnopened int `json:"source_unopened"`
	TargetUnopened int `json:"target_unopened"`
	// Match: every value opened on both sides, and the two sides hold the
	// same (identifying columns, plaintext) pairs.
	Match bool `json:"match"`
}

// NamedRowReport is one row checked by name: a signing key, the federation
// instance id or a key canary.
type NamedRowReport struct {
	Name          string `json:"name"`
	SourcePresent bool   `json:"source_present"`
	TargetPresent bool   `json:"target_present"`
	// Match: the same rows, byte for byte, on both sides.
	Match bool `json:"match"`
	// KeyMatches is set for a key canary when its key was loaded: the
	// target's canary opens with it.
	KeyMatches *bool `json:"key_matches,omitempty"`
}

// BlobReport covers the blob files named by inbox_blobs.
type BlobReport struct {
	// Rows is the number of inbox_blobs rows in the source.
	Rows int `json:"rows"`
	// Bytes is the size of the blobs read.
	Bytes int64 `json:"bytes"`
	// Copied and AlreadyPresent: blobs a copy stored, and blobs the target
	// already had.
	Copied         int `json:"copied,omitempty"`
	AlreadyPresent int `json:"already_present,omitempty"`
	// Verified: target blobs whose sha256 equals their name (verify).
	Verified int `json:"verified,omitempty"`
	// MissingFiles are rows with no file in the source folder;
	// MismatchedFiles are files whose sha256 is not their name.
	MissingFiles    []string `json:"missing_files,omitempty"`
	MismatchedFiles []string `json:"mismatched_files,omitempty"`
	// OrphanFiles are blob files no row names (reported, not copied).
	OrphanFiles []string `json:"orphan_files,omitempty"`
	// TargetMissing and TargetMismatched are source blobs the target does
	// not have, or holds with other bytes (verify).
	TargetMissing    []string `json:"target_missing,omitempty"`
	TargetMismatched []string `json:"target_mismatched,omitempty"`
}

func newReport(command string) *Report {
	return &Report{
		Command:        command,
		ExcludedTables: []ExcludedTable{},
		DroppedColumns: []string{},
		SkipRules:      []string{},
		Tables:         []TableReport{},
		Problems:       []string{},
	}
}

// OK reports whether the copy or the comparison found nothing wrong.
func (r *Report) OK() bool { return r != nil && len(r.Problems) == 0 }

func (r *Report) problemf(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// MarshalJSON adds "ok" (Report.OK) to the report's fields.
func (r *Report) MarshalJSON() ([]byte, error) {
	type plain Report
	return json.Marshal(struct {
		OK bool `json:"ok"`
		*plain
	}{OK: r.OK(), plain: (*plain)(r)})
}

// listNames shows at most five names, then how many more there are.
func listNames(names []string) string {
	const show = 5
	if len(names) <= show {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:show], ", ") + fmt.Sprintf(" and %d more", len(names)-show)
}

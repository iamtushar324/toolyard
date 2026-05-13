package lake

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Statement-type validators for the lake.* tools. The point is defense in
// depth: agents call lake.insert with raw SQL, but we don't want them to
// smuggle a DELETE through that surface. None of these are full SQL parsers —
// they strip comments, then check the leading keyword(s). A malformed but
// valid-looking statement is left to ClickHouse to reject; a different
// statement type is caught here.

var (
	// commentBlock matches /* ... */ across lines.
	commentBlock = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// commentLine matches -- to end of line.
	commentLine = regexp.MustCompile(`(?m)--[^\n]*`)
	// stmtSeparator is a naive split — DuckDB allows ; in string literals,
	// but we explicitly want to reject multi-statement input on the lake.*
	// tools. False positives (one valid statement that contains ';' inside
	// a string) get flagged; the agent can quote-escape or use the
	// approval-gated lake.alter path instead.
	stmtSeparator = ";"
)

// stripCommentsAndTrim returns sqlStr with comments removed and surrounding
// whitespace + trailing semicolons trimmed.
func stripCommentsAndTrim(sqlStr string) string {
	s := commentBlock.ReplaceAllString(sqlStr, " ")
	s = commentLine.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "; \t\n\r")
	return s
}

// firstKeyword returns the first ASCII-uppercase word, or "" if empty.
func firstKeyword(sqlStr string) string {
	cleaned := stripCommentsAndTrim(sqlStr)
	if cleaned == "" {
		return ""
	}
	// Take up to the first whitespace.
	end := len(cleaned)
	for i, r := range cleaned {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			end = i
			break
		}
	}
	return strings.ToUpper(cleaned[:end])
}

// hasMultipleStatements is true when sqlStr contains any non-trailing ';'.
// We can't perfectly tell a ';' inside a literal apart from a separator
// without a real parser, so we err on strict and reject anything looking
// like multi-statement input.
func hasMultipleStatements(sqlStr string) bool {
	cleaned := stripCommentsAndTrim(sqlStr)
	return strings.Contains(cleaned, stmtSeparator)
}

// ValidateSelect rejects sqlStr unless it's a single SELECT/WITH/SHOW/
// DESCRIBE/EXPLAIN/VALUES/TABLE statement. ClickHouse has no useful PRAGMAs
// (its server-state surface is gated behind SYSTEM, which is write-shaped
// and deliberately not allowed here). SHOW + DESCRIBE cover catalog queries.
func ValidateSelect(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed for read queries")
	}
	kw := firstKeyword(sqlStr)
	switch kw {
	case "SELECT", "WITH", "SHOW", "DESCRIBE", "EXPLAIN", "VALUES", "TABLE":
		return nil
	case "":
		return errors.New("empty SQL")
	default:
		return fmt.Errorf("only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN/VALUES/TABLE allowed; got %s", kw)
	}
}

// ValidateInsert rejects sqlStr unless the leading statement is INSERT.
// INSERT ... SELECT and INSERT ... VALUES are both fine.
func ValidateInsert(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed")
	}
	kw := firstKeyword(sqlStr)
	if kw != "INSERT" {
		return fmt.Errorf("expected INSERT; got %s", kw)
	}
	return nil
}

// ValidateCreate rejects sqlStr unless the leading statement is one of the
// non-destructive DDL forms — CREATE TABLE, CREATE VIEW, CREATE INDEX,
// CREATE SCHEMA, CREATE OR REPLACE *. Bare DROP and ALTER paths are deliberately
// excluded; those have their own approval-gated tools.
func ValidateCreate(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed")
	}
	kw := firstKeyword(sqlStr)
	if kw != "CREATE" {
		return fmt.Errorf("expected CREATE; got %s", kw)
	}
	// Reject CREATE ... DROP weirdness. The leading word is CREATE; we still
	// want to make sure the rest contains no ALTER/DROP/DELETE-of-data inside.
	cleaned := strings.ToUpper(stripCommentsAndTrim(sqlStr))
	for _, banned := range []string{" ALTER ", " DROP ", " DELETE ", " UPDATE "} {
		if strings.Contains(cleaned, banned) {
			return fmt.Errorf("CREATE statement contains forbidden keyword %s", strings.TrimSpace(banned))
		}
	}
	return nil
}

// ValidateAlter rejects sqlStr unless the leading statement is ALTER.
func ValidateAlter(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed")
	}
	kw := firstKeyword(sqlStr)
	if kw != "ALTER" {
		return fmt.Errorf("expected ALTER; got %s", kw)
	}
	return nil
}

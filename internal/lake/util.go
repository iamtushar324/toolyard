package lake

import (
	"fmt"
	"sort"
	"strings"
)

// validIdent matches a single identifier component. We deliberately do NOT
// support DuckDB's quoted-identifier corner cases here — agents and ingest
// callers stick to ASCII names. This is a defense in depth check before we
// quote and interpolate the name into SQL.
func isValidIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case (r >= '0' && r <= '9') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validateQualifiedName accepts "schema.table", "table", or "schema.tbl.col".
// Each segment must be a valid identifier.
func validateQualifiedName(s string) error {
	if s == "" {
		return fmt.Errorf("name is required")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return fmt.Errorf("name %q has too many segments", s)
	}
	for _, p := range parts {
		if !isValidIdent(p) {
			return fmt.Errorf("invalid identifier %q in %q", p, s)
		}
	}
	return nil
}

func quoteIdent(s string) string {
	// Caller must have already passed isValidIdent — we still wrap in
	// double quotes so reserved words don't collide.
	return `"` + s + `"`
}

func quoteQualifiedName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// sortedKeys returns the keys of m sorted lex. Used so InsertRows builds
// deterministic SQL and so error messages list the offending column.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package dbcopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// kind is the class of a target (Postgres) column.
type kind int

const (
	kindText kind = iota + 1
	kindInt
	kindBytes
	kindFloat
)

func (k kind) String() string {
	switch k {
	case kindText:
		return "text"
	case kindInt:
		return "integer"
	case kindBytes:
		return "bytea"
	case kindFloat:
		return "double precision"
	}
	return "unknown"
}

// column is one column copied or compared, typed as the target holds it.
type column struct {
	Name     string
	Kind     kind
	Bits     int // width of an integer column: 16, 32 or 64
	Nullable bool
}

// columnOf types a target column from its information_schema data_type.
// Only the types toolyard's Postgres schema uses are handled: anything
// else (real, boolean, jsonb, ...) would need a coercion rule first.
func columnOf(table, name, dataType string, nullable bool) (column, error) {
	c := column{Name: name, Nullable: nullable}
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "text", "character varying", "character":
		c.Kind = kindText
	case "bigint":
		c.Kind, c.Bits = kindInt, 64
	case "integer":
		c.Kind, c.Bits = kindInt, 32
	case "smallint":
		c.Kind, c.Bits = kindInt, 16
	case "bytea":
		c.Kind = kindBytes
	case "double precision":
		c.Kind = kindFloat
	default:
		return c, fmt.Errorf("target column %s.%s has type %s, which db copy does not handle", table, name, dataType)
	}
	return c, nil
}

// maxExactInt is the largest integer magnitude a float64 holds exactly.
const maxExactInt = 1 << 53

// coerce converts a value read from SQLite (nil, int64, float64, string or
// []byte) to the value the target column holds. The rules are strict:
//
//   - text    <- text, or a blob that is valid UTF-8 (SQLite keeps a Go
//     []byte argument as a blob even in a TEXT column; it reads back as the
//     same string). Text must be valid UTF-8 without NUL bytes, which
//     Postgres text cannot hold. An integer or a real in a text column is
//     refused: SQLite's TEXT affinity turns numbers written to a TEXT
//     column into text, so one there means the column was not declared
//     TEXT, and the copy stops rather than guess its text form.
//   - integer <- integer in the column's range, or text that is a clean
//     decimal integer ("-12"; no sign +, no spaces), which is the value
//     database/sql scans from it. Reals and blobs are refused.
//   - bytea   <- blob only.
//   - double  <- real, or an integer a float64 holds exactly.
//   - NULL    <- NULL, only where the target column allows it.
//
// On a refusal it returns a reason that never contains the value.
func coerce(v any, c column) (any, string) {
	if v == nil {
		if !c.Nullable {
			return nil, "is NULL, but the target column is NOT NULL"
		}
		return nil, ""
	}
	switch c.Kind {
	case kindText:
		switch x := v.(type) {
		case string:
			if r := textProblem(x); r != "" {
				return nil, "is text that " + r
			}
			return x, ""
		case []byte:
			s := string(x)
			if r := textProblem(s); r != "" {
				return nil, "is a blob that " + r
			}
			return s, ""
		}
	case kindInt:
		switch x := v.(type) {
		case int64:
			if !fitsBits(x, c.Bits) {
				return nil, fmt.Sprintf("is an integer out of range for the target column (%d-bit)", c.Bits)
			}
			return x, ""
		case string:
			if n, ok := cleanInt(x); ok {
				if !fitsBits(n, c.Bits) {
					return nil, fmt.Sprintf("is an integer (stored as text) out of range for the target column (%d-bit)", c.Bits)
				}
				return n, ""
			}
			return nil, "is text that is not a clean decimal integer, but the target column is integer"
		}
	case kindBytes:
		if x, ok := v.([]byte); ok {
			return x, ""
		}
	case kindFloat:
		switch x := v.(type) {
		case float64:
			return x, ""
		case int64:
			if x < -maxExactInt || x > maxExactInt {
				return nil, "is an integer that a double precision column cannot hold exactly"
			}
			return float64(x), ""
		}
	}
	return nil, fmt.Sprintf("is %s, but the target column is %s", describeValue(v), c.Kind)
}

func textProblem(s string) string {
	if !utf8.ValidString(s) {
		return "is not valid UTF-8"
	}
	if strings.IndexByte(s, 0) >= 0 {
		return "contains a NUL byte, which Postgres text cannot hold"
	}
	return ""
}

func fitsBits(x int64, bits int) bool {
	if bits <= 0 || bits >= 64 {
		return true
	}
	limit := int64(1) << (bits - 1)
	return x >= -limit && x < limit
}

// cleanInt parses an optional '-' and decimal digits, nothing else.
func cleanInt(s string) (int64, bool) {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func describeValue(v any) string {
	switch v.(type) {
	case int64:
		return "an integer"
	case float64:
		return "a real"
	case string:
		return "text"
	case []byte:
		return "a blob"
	}
	return fmt.Sprintf("a %T value", v)
}

// Canonical value tags. Every encoding is self-delimiting (a fixed width,
// or a length prefix), so a row's values can be concatenated unambiguously.
const (
	tagNull  = 0x00
	tagInt   = 0x01
	tagFloat = 0x02
	tagText  = 0x03
	tagBytes = 0x04
)

// appendCanonical appends the canonical encoding of v as column c holds
// it: the value Copy writes for it (coerce), so a source value and the
// target value Copy made from it encode to the same bytes, whatever the
// engine. A value Copy refuses is encoded as what it is, so it never equals
// a value of the column's own type: an integer SQLite returns for a text
// column is a mismatch, never silently equal to its text form.
func appendCanonical(b []byte, v any, c column) ([]byte, error) {
	if cv, reason := coerce(v, c); reason == "" {
		v = cv
	}
	switch x := v.(type) {
	case nil:
		return append(b, tagNull), nil
	case int64:
		b = append(b, tagInt)
		return binary.BigEndian.AppendUint64(b, uint64(x)), nil
	case float64:
		b = append(b, tagFloat)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(x)), nil
	case string:
		b = append(b, tagText)
		b = binary.AppendUvarint(b, uint64(len(x)))
		return append(b, x...), nil
	case []byte:
		b = append(b, tagBytes)
		b = binary.AppendUvarint(b, uint64(len(x)))
		return append(b, x...), nil
	}
	return b, fmt.Errorf("unsupported value type %T", v)
}

// writeField writes s length-prefixed, so fields cannot run together.
func writeField(w io.Writer, s string) {
	var n [binary.MaxVarintLen64]byte
	_, _ = w.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
	_, _ = io.WriteString(w, s)
}

// digestDomain separates table hashes from every other SHA-256.
const digestDomain = "toolyard.db-verify.table.v1\x00"

// tableDigest is a table's row count and the SHA-256 of its rows.
type tableDigest struct {
	Rows    int64
	Skipped int64
	SHA256  string
}

// digestTable reads every row of t (t's columns only; rows a skip rule
// matches are counted, not hashed), hashes each row's canonical values,
// sorts the rows in Go by the canonical bytes of their primary key (so
// collation and scan order do not matter) and hashes the sorted row
// hashes. Equal data on SQLite and Postgres gives the same digest. Only
// hashes are kept in memory, never the values.
func digestTable(ctx context.Context, q store.Querier, t *table) (tableDigest, error) {
	var d tableDigest
	rows, err := q.QueryContext(ctx, t.selectSQL())
	if err != nil {
		return d, fmt.Errorf("read %s: %w", t.Name, err)
	}
	defer rows.Close()
	type entry struct {
		key []byte
		sum [sha256.Size]byte
	}
	var entries []entry
	var buf []byte
	vals, ptrs := scanTargets(len(t.Cols))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return d, fmt.Errorf("read %s: %w", t.Name, err)
		}
		if t.skipped(vals) {
			d.Skipped++
			continue
		}
		buf = buf[:0]
		for i, c := range t.Cols {
			if buf, err = appendCanonical(buf, vals[i], c); err != nil {
				return d, fmt.Errorf("read %s.%s: %w", t.Name, c.Name, err)
			}
		}
		e := entry{sum: sha256.Sum256(buf)}
		for _, i := range t.PK {
			if e.key, err = appendCanonical(e.key, vals[i], t.Cols[i]); err != nil {
				return d, fmt.Errorf("read %s.%s: %w", t.Name, t.Cols[i].Name, err)
			}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("read %s: %w", t.Name, err)
	}
	sort.Slice(entries, func(a, b int) bool {
		if c := bytes.Compare(entries[a].key, entries[b].key); c != 0 {
			return c < 0
		}
		return bytes.Compare(entries[a].sum[:], entries[b].sum[:]) < 0
	})
	h := sha256.New()
	_, _ = io.WriteString(h, digestDomain)
	writeField(h, t.Name)
	for _, c := range t.Cols {
		writeField(h, c.Name)
	}
	for i := range entries {
		_, _ = h.Write(entries[i].sum[:])
	}
	d.Rows = int64(len(entries))
	d.SHA256 = hex.EncodeToString(h.Sum(nil))
	return d, nil
}

// scanTargets returns n values and pointers to them for rows.Scan.
func scanTargets(n int) ([]any, []any) {
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return vals, ptrs
}

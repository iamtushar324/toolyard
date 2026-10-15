package store

import (
	"strings"
	"testing"
)

func TestRebind(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"no placeholders", "SELECT 1", "SELECT 1"},
		{"in order", "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"insert values", "INSERT INTO t(a,b) VALUES(?,?) ON CONFLICT (a) DO UPDATE SET b = excluded.b",
			"INSERT INTO t(a,b) VALUES($1,$2) ON CONFLICT (a) DO UPDATE SET b = excluded.b"},
		{"ten or more", "VALUES(?,?,?,?,?,?,?,?,?,?,?)", "VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)"},
		{"single-quoted string", "SELECT '?', ?", "SELECT '?', $1"},
		{"escaped quote in string", "SELECT 'it''s ?', ?", "SELECT 'it''s ?', $1"},
		{"string of one quote", "SELECT '''', ?", "SELECT '''', $1"},
		{"empty string", "SELECT '', ?", "SELECT '', $1"},
		{"double-quoted identifier", `SELECT "we?ird" FROM t WHERE x = ?`, `SELECT "we?ird" FROM t WHERE x = $1`},
		{"escaped quote in identifier", `SELECT "a""?b", ?`, `SELECT "a""?b", $1`},
		{"line comment", "SELECT ? -- what?\n, ?", "SELECT $1 -- what?\n, $2"},
		{"line comment at end", "SELECT ? -- trailing ?", "SELECT $1 -- trailing ?"},
		{"comment-like minus", "SELECT 1 - ?", "SELECT 1 - $1"},
		{"block comment", "SELECT ? /* a ? b */, ?", "SELECT $1 /* a ? b */, $2"},
		{"nested block comment", "SELECT ? /* a ? /* nested ? */ still ? */ , ?", "SELECT $1 /* a ? /* nested ? */ still ? */ , $2"},
		{"casts untouched", "SELECT ?::text, CAST(? AS BIGINT), a::jsonb ->> 'k'", "SELECT $1::text, CAST($2 AS BIGINT), a::jsonb ->> 'k'"},
		{"dollar-quoted string", "SELECT $$ ? $$, ?", "SELECT $$ ? $$, $1"},
		{"tagged dollar quote", "SELECT $fn$ ? $x$ ? $fn$, ?", "SELECT $fn$ ? $x$ ? $fn$, $1"},
		{"dollar inside identifier", "SELECT a$b, ?", "SELECT a$b, $1"},
		{"jsonpath filter in string", "SELECT jsonb_path_exists(doc::jsonb, '$.tools[*] ? (@.x == 1)') AND id = ?",
			"SELECT jsonb_path_exists(doc::jsonb, '$.tools[*] ? (@.x == 1)') AND id = $1"},
		{"unterminated string", "SELECT 'abc ?", "SELECT 'abc ?"},
		{"unterminated identifier", `SELECT "abc ?`, `SELECT "abc ?`},
		{"unterminated block comment", "SELECT ? /* ? ", "SELECT $1 /* ? "},
		{"unterminated dollar quote", "SELECT ?, $q$ ? ", "SELECT $1, $q$ ? "},
		{"string concatenation", "SELECT 'upload:' || sha256 FROM t WHERE agent_id = ?", "SELECT 'upload:' || sha256 FROM t WHERE agent_id = $1"},
		{"multi-line", "UPDATE t\n   SET a = ?,\n       b = 'x?'\n WHERE id = ?", "UPDATE t\n   SET a = $1,\n       b = 'x?'\n WHERE id = $2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Rebind(c.in); got != c.want {
				t.Fatalf("Rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// Rebind runs on every statement sent to Postgres: it must be idempotent
// (a prepared statement's text is rewritten again when it executes).
func TestRebindIsIdempotent(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t WHERE a = ? AND b = '?' AND c = ?",
		"INSERT INTO t(a, b) VALUES(?, ?) -- ?",
	} {
		once := Rebind(q)
		if strings.Count(once, "$") != 2 {
			t.Fatalf("Rebind(%q) = %q, want two placeholders", q, once)
		}
		if twice := Rebind(once); twice != once {
			t.Fatalf("Rebind(Rebind(%q)) = %q, want %q", q, twice, once)
		}
	}
}

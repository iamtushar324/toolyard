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
// statement type is caught here. Every validator also rejects calls to
// external-access table functions (url, remote, s3, file, ...), which would
// let the lake server reach outside the lake (rejectExternalAccess).

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
// External table functions are rejected (see rejectExternalAccess).
func ValidateSelect(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed for read queries")
	}
	kw := firstKeyword(sqlStr)
	switch kw {
	case "SELECT", "WITH", "SHOW", "DESCRIBE", "EXPLAIN", "VALUES", "TABLE":
		return rejectExternalAccess(sqlStr)
	case "":
		return errors.New("empty SQL")
	default:
		return fmt.Errorf("only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN/VALUES/TABLE allowed; got %s", kw)
	}
}

// ValidateInsert rejects sqlStr unless the leading statement is INSERT.
// INSERT ... SELECT and INSERT ... VALUES are both fine; external table
// functions (INSERT INTO FUNCTION url(...), SELECT ... FROM s3(...)) are not.
func ValidateInsert(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed")
	}
	kw := firstKeyword(sqlStr)
	if kw != "INSERT" {
		return fmt.Errorf("expected INSERT; got %s", kw)
	}
	return rejectExternalAccess(sqlStr)
}

// ValidateCreate rejects sqlStr unless it is one CREATE statement that adds
// a table, view, materialized view, index or database to the lake. CREATE OR
// REPLACE passes here: it is valid DDL, but it discards the replaced object's
// data, so lake.create_table sends it to approval first (IsReplacingDDL).
// Rejected:
//   - ALTER/DROP/DELETE/UPDATE inside the statement; those have their own
//     approval-gated tools;
//   - server-wide objects: CREATE USER, ROLE, QUOTA, [ROW|MASKING] POLICY,
//     [SETTINGS] PROFILE, FUNCTION, DICTIONARY, NAMED COLLECTION, WORKLOAD
//     and RESOURCE;
//   - a table engine outside the MergeTree family, the Log family (Log,
//     TinyLog, StripeLog), Memory and Null (see checkEngines);
//   - external table functions (see rejectExternalAccess).
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
	if err := rejectExternalAccess(sqlStr); err != nil {
		return err
	}
	// Keyword checks read the statement with quoted identifiers masked:
	// `engine` or "USER" is a name, not the keyword.
	code, err := scanSQL(sqlStr, false)
	if err != nil {
		return err
	}
	if m := globalObjectDDL.FindStringSubmatch(normalizeSQL(code)); m != nil {
		return fmt.Errorf("CREATE %s is not allowed here: it makes a server-wide object, not lake data", m[1])
	}
	return checkEngines(code)
}

// ValidateAlter rejects sqlStr unless the leading statement is ALTER.
// External table functions are rejected here too.
func ValidateAlter(sqlStr string) error {
	if hasMultipleStatements(sqlStr) {
		return errors.New("multi-statement SQL not allowed")
	}
	kw := firstKeyword(sqlStr)
	if kw != "ALTER" {
		return fmt.Errorf("expected ALTER; got %s", kw)
	}
	return rejectExternalAccess(sqlStr)
}

// ---- replacing DDL, global objects, engines and external access -------------

var (
	// replacingHead matches the leading keywords of DDL that replaces an
	// existing object, in normalized SQL (see normalizeSQL).
	replacingHead = regexp.MustCompile(`^(CREATE OR REPLACE|REPLACE|EXCHANGE)\b`)

	// globalObjectDDL matches CREATE of a server-wide object (access control,
	// UDFs, dictionaries, named collections, workload scheduling), in
	// normalized SQL. POLICY and PROFILE are ClickHouse's short forms of
	// ROW POLICY and SETTINGS PROFILE.
	globalObjectDDL = regexp.MustCompile(`^CREATE (?:OR REPLACE )?(?:TEMPORARY )?(USER|ROLE|QUOTA|ROW POLICY|MASKING POLICY|POLICY|SETTINGS PROFILE|PROFILE|FUNCTION|DICTIONARY|NAMED COLLECTION|WORKLOAD|RESOURCE)\b`)

	// engineKeyword finds the ENGINE keyword in scanned SQL; engineName reads
	// the optional '=' and the engine name after it.
	engineKeyword = regexp.MustCompile(`(?i)\bENGINE\b`)
	engineName    = regexp.MustCompile(`^\s*(=)?\s*([A-Za-z_][A-Za-z0-9_]*)?`)

	// queryStart finds the AS that starts the query of CREATE ... AS SELECT
	// (or AS WITH, AS (SELECT ...)) in scanned SQL.
	queryStart = regexp.MustCompile(`(?i)\bAS\s*(?:\(|SELECT\b|WITH\b)`)

	// externalCall matches a call of an external-access function in scanned
	// SQL; group 1 is the function name.
	externalCall = buildExternalCall(externalTableFunctions)
)

// externalTableFunctions are the ClickHouse table functions that read or
// write outside the lake's own tables: other servers, local files, object
// stores, other databases and programs. disk() is here too: a table's
// SETTINGS can declare a disk on an object store or web endpoint with it.
// A trailing * matches the family by prefix (icebergS3, deltaLakeCluster,
// hudiCluster, ...). Names match case-insensitively.
var externalTableFunctions = []string{
	"url", "urlCluster", "remote", "remoteSecure", "cluster", "clusterAllReplicas",
	"file", "fileCluster", "s3", "s3Cluster", "gcs", "cosn", "oss", "hdfs", "hdfsCluster",
	"mysql", "postgresql", "jdbc", "odbc", "mongodb", "redis", "sqlite", "executable",
	"azureBlobStorage", "azureBlobStorageCluster", "disk",
	"iceberg*", "deltaLake*", "hudi*",
}

func buildExternalCall(names []string) *regexp.Regexp {
	alts := make([]string, 0, len(names))
	for _, n := range names {
		if p, family := strings.CutSuffix(n, "*"); family {
			alts = append(alts, regexp.QuoteMeta(p)+`[A-Za-z0-9_]*`)
		} else {
			alts = append(alts, regexp.QuoteMeta(n))
		}
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(alts, "|") + `)\s*\(`)
}

// IsReplacingDDL reports whether sqlStr is DDL that replaces an existing
// object and so discards its data: CREATE OR REPLACE ..., ClickHouse's
// REPLACE TABLE ... and EXCHANGE TABLES|DICTIONARIES .... Only the leading
// keywords count, read with comments and string literals removed,
// whitespace collapsed and letters upper-cased, so a comment between CREATE
// and OR REPLACE does not hide it and a literal that mentions it does not
// trigger it. SQL that scanSQL refuses falls back to comment stripping;
// ValidateCreate rejects such SQL anyway.
func IsReplacingDDL(sqlStr string) bool {
	code, err := scanSQL(sqlStr, false)
	if err != nil {
		code = stripCommentsAndTrim(sqlStr)
	}
	return replacingHead.MatchString(normalizeSQL(code))
}

// rejectExternalAccess returns an error when sqlStr calls an external-access
// table function (externalTableFunctions), or when it cannot be scanned the
// way ClickHouse would read it (see scanSQL). Calls are matched as a
// function name followed by '(' on a word boundary, after scanSQL has
// removed comments and string literals, so a column or table named url,
// or a literal containing 'url(', is fine, while url (...), url/**/(...) and
// `url`(...) are all caught.
func rejectExternalAccess(sqlStr string) error {
	code, err := scanSQL(sqlStr, true)
	if err != nil {
		return err
	}
	if m := externalCall.FindStringSubmatch(code); m != nil {
		return fmt.Errorf("%s() reads or writes outside the lake (an external table function) and is not allowed", m[1])
	}
	return nil
}

// checkEngines rejects scanned DDL whose table engine is outside the MergeTree
// family, the Log family (Log, TinyLog, StripeLog), Memory and Null. It looks
// at each ENGINE keyword outside parentheses (a column named engine sits
// inside the column list):
//   - ENGINE followed by '=' must name an allowed engine wherever it is;
//     after the query of CREATE ... AS SELECT it is skipped only when no
//     name follows (engine = 'x' compares a column with a literal);
//   - ENGINE without '=' before that query (ClickHouse makes '=' optional)
//     must name an allowed engine too.
//
// A quoted engine name, and a column named engine compared with another
// column in the query, are therefore rejected; quote the column (`engine`)
// and write the engine name bare.
func checkEngines(code string) error {
	depth, balanced := parenDepths(code)
	if !balanced {
		return errors.New("unbalanced parentheses")
	}
	cut := len(code)
	for _, m := range queryStart.FindAllStringIndex(code, -1) {
		if depth[m[0]] == 0 {
			cut = m[0]
			break
		}
	}
	for _, m := range engineKeyword.FindAllStringIndex(code, -1) {
		if depth[m[0]] != 0 {
			continue
		}
		sub := engineName.FindStringSubmatch(code[m[1]:])
		hasEq, name := sub[1] != "", sub[2]
		if m[0] >= cut && (!hasEq || name == "") {
			continue
		}
		if !allowedEngine(name) {
			if name == "" {
				name = "(none)"
			}
			return fmt.Errorf("table engine %s is not allowed here; write ENGINE = a MergeTree-family engine, Log, TinyLog, StripeLog, Memory or Null (bare name), and quote a column named engine", name)
		}
	}
	return nil
}

func allowedEngine(name string) bool {
	up := strings.ToUpper(name)
	switch up {
	case "LOG", "TINYLOG", "STRIPELOG", "MEMORY", "NULL":
		return true
	}
	return strings.HasSuffix(up, "MERGETREE")
}

// parenDepths returns, for each byte offset of code, the parenthesis depth
// before that byte, and whether the parentheses balance.
func parenDepths(code string) ([]int, bool) {
	out := make([]int, len(code)+1)
	depth, balanced := 0, true
	for i := 0; i < len(code); i++ {
		out[i] = depth
		switch code[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				balanced = false
			} else {
				depth--
			}
		}
	}
	out[len(code)] = depth
	return out, balanced && depth == 0
}

// normalizeSQL collapses whitespace and upper-cases scanned SQL.
func normalizeSQL(code string) string {
	return strings.ToUpper(strings.Join(strings.Fields(code), " "))
}

// scanSQL returns the code of sqlStr as ClickHouse's lexer reads it: each
// comment becomes a space and each string literal becomes an empty literal
// (two single quotes). A quoted identifier (`x` or "x") becomes, between
// spaces, its name with every character other than an ASCII letter, digit
// or underscore replaced by '_' (keepQuotedNames, so `url`(...) still reads
// as a call), or the placeholder _quoted_ (so `engine` is not the ENGINE
// keyword). Words, operators and parentheses are kept, so patterns match
// code only.
//
// It follows ClickHouse's lexer: a doubled quote or a backslash escapes the
// next character inside a literal; -- and // start a comment to the end of
// the line; /* starts a comment that the first */ ends. Any # starts a
// comment to the end of the line: ClickHouse reads "# " and "#!" that way
// and rejects every other bare #, so treating all of them as comments hides
// nothing ClickHouse would run. ASCII control characters (vertical tab, form
// feed, ...) become spaces, since ClickHouse skips them as whitespace. Where
// the two could read the same text differently, it returns an error instead
// of guessing: a /* inside a block comment (ClickHouse nests block
// comments), any $ outside a literal (ClickHouse reads $tag$ ... $tag$ as a
// heredoc string), any non-ASCII byte outside a literal (Unicode whitespace
// and quotation marks), a carriage return inside a line comment other than
// just before its newline, and an unterminated literal or comment.
func scanSQL(sqlStr string, keepQuotedNames bool) (string, error) {
	s := sqlStr
	n := len(s)
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == '\'':
			end, ok := quotedEnd(s, i)
			if !ok {
				return "", errors.New("unterminated string literal")
			}
			b.WriteString("''")
			i = end
		case c == '`' || c == '"':
			end, ok := quotedEnd(s, i)
			if !ok {
				return "", errors.New("unterminated quoted identifier")
			}
			if keepQuotedNames {
				b.WriteString(" " + identWord(s[i+1:end-1]) + " ")
			} else {
				b.WriteString(" _quoted_ ")
			}
			i = end
		case c == '#', c == '-' && i+1 < n && s[i+1] == '-', c == '/' && i+1 < n && s[i+1] == '/':
			end := n
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				end = i + j
			}
			if k := strings.IndexByte(s[i:end], '\r'); k >= 0 && i+k != end-1 {
				return "", errors.New("carriage return inside a line comment is not allowed")
			}
			b.WriteByte(' ')
			i = end
		case c == '/' && i+1 < n && s[i+1] == '*':
			// Pairwise, as ClickHouse scans it: in "/* a /*/" the "/*"
			// that overlaps the closing "*/" opens a nested comment there.
			end := -1
			for k := i + 2; k+1 < n; k++ {
				if s[k] == '/' && s[k+1] == '*' {
					return "", errors.New("nested block comments are not allowed")
				}
				if s[k] == '*' && s[k+1] == '/' {
					end = k + 2
					break
				}
			}
			if end < 0 {
				return "", errors.New("unterminated block comment")
			}
			b.WriteByte(' ')
			i = end
		case c == '$':
			return "", errors.New("$ outside a string literal is not allowed (heredoc strings are not supported here)")
		case c >= 0x80:
			return "", errors.New("non-ASCII character outside a string literal or quoted identifier is not allowed")
		case c < 0x20 || c == 0x7f:
			b.WriteByte(' ')
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// quotedEnd returns the offset just past the literal or quoted identifier
// that opens at s[i], and false when it never closes. A doubled quote
// character and a backslash escape the next character, as in ClickHouse.
func quotedEnd(s string, i int) (int, bool) {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			if j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return 0, false
}

// identWord is name with every byte other than an ASCII letter, digit or
// underscore replaced by '_'.
func identWord(name string) string {
	w := []byte(name)
	for k, c := range w {
		if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			w[k] = '_'
		}
	}
	return string(w)
}

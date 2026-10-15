package store

import (
	"strconv"
	"strings"
)

// Rebind rewrites the ? placeholders of query to Postgres's $1, $2, ... in
// order. It leaves alone every ? inside a single-quoted string (a doubled
// quote is an escaped quote), a double-quoted identifier (likewise), a
// dollar-quoted string ($$...$$ or $tag$...$tag$), a -- line comment or a
// /* */ block comment (which nest, as in Postgres). Everything else,
// :: casts included, is copied unchanged. A query with no ? is returned as
// it is. Backslash escapes in E'...' strings are not understood: toolyard's
// SQL does not use them.
func Rebind(query string) string {
	if strings.IndexByte(query, '?') < 0 {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 0
	i := 0
	for i < len(query) {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			end := quotedEnd(query, i, c)
			b.WriteString(query[i:end])
			i = end
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query)
			} else {
				end += i
			}
			b.WriteString(query[i:end])
			i = end
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := blockCommentEnd(query, i)
			b.WriteString(query[i:end])
			i = end
		case c == '$' && (i == 0 || !identByte(query[i-1])):
			if end, ok := dollarQuotedEnd(query, i); ok {
				b.WriteString(query[i:end])
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// quotedEnd returns the index just past the closing quote of the string or
// identifier that opens at start with quote q (a doubled quote is an escaped
// quote), or len(s) when it is not closed.
func quotedEnd(s string, start int, q byte) int {
	j := start + 1
	for j < len(s) {
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

// blockCommentEnd returns the index just past the */ that closes the block
// comment opening at start, counting nested /* */ pairs, or len(s).
func blockCommentEnd(s string, start int) int {
	depth := 1
	j := start + 2
	for j < len(s) {
		switch {
		case s[j] == '/' && j+1 < len(s) && s[j+1] == '*':
			depth++
			j += 2
		case s[j] == '*' && j+1 < len(s) && s[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return len(s)
}

// dollarQuotedEnd reports whether a dollar-quoted string opens at start
// ($$ or $tag$, where tag is a letter or _ followed by letters, digits or _)
// and returns the index just past its closing delimiter, or len(s) when it
// is not closed. $1 and a lone $ are not dollar quotes.
func dollarQuotedEnd(s string, start int) (int, bool) {
	j := start + 1
	for j < len(s) && s[j] != '$' {
		c := s[j]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 || j > start+1 && c >= '0' && c <= '9') {
			return 0, false
		}
		j++
	}
	if j >= len(s) {
		return 0, false
	}
	delim := s[start : j+1]
	if k := strings.Index(s[j+1:], delim); k >= 0 {
		return j + 1 + k + len(delim), true
	}
	return len(s), true
}

// identByte reports a byte that can continue an unquoted identifier, so a $
// right after it is part of the identifier and not a dollar quote.
func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

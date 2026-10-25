package toolsearch

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Keyword search: BM25F. Each tool is a document of four fields, each
// with a weight and its own length normalisation; a query term's weighted
// frequency across the fields goes through BM25's saturation once, so a
// word in the name counts for more than the same word in a parameter
// description, without one field's long text drowning another.

type field int

const (
	fieldName field = iota
	fieldDescription
	fieldParamName
	fieldParamDescription
	numFields
)

var (
	// fieldWeight is how much one occurrence counts in each field.
	fieldWeight = [numFields]float64{3.0, 1.0, 1.5, 0.6}
	// fieldB is BM25's length normalisation per field: 0 ignores length,
	// 1 normalises fully.
	fieldB = [numFields]float64{0.5, 0.75, 0.6, 0.75}
)

const (
	// bm25K1 is BM25's term frequency saturation.
	bm25K1 = 1.2
	// exactNameBoost lifts a tool whose name is the query itself
	// ("get_client", "BkCoreServices.get_client") above word matches.
	exactNameBoost = 10.0
	// maxSchemaDepth bounds how deep parameter names are collected.
	maxSchemaDepth = 3
	// maxQueryTerms caps the distinct words of a keyword query that count.
	maxQueryTerms = 32
)

// doc is one indexed tool.
type doc struct {
	tool Tool
	tf   [numFields]map[string]int
	len  [numFields]int
	// names are the tool's normalised names (full, short, code name), for
	// the exact-name boost.
	names map[string]bool
}

// index is a keyword index of one catalog.
type index struct {
	docs   []*doc
	df     map[string]int
	avgLen [numFields]float64
}

func buildIndex(tools []Tool) *index {
	ix := &index{df: map[string]int{}}
	var total [numFields]int
	for _, t := range tools {
		d := &doc{tool: t, names: map[string]bool{}}
		texts := toolFields(t)
		seen := map[string]bool{}
		for f := field(0); f < numFields; f++ {
			d.tf[f] = map[string]int{}
			for _, text := range texts[f] {
				for _, tok := range tokens(text, false) {
					d.tf[f][tok]++
					d.len[f]++
					seen[tok] = true
				}
			}
			total[f] += d.len[f]
		}
		for tok := range seen {
			ix.df[tok]++
		}
		for _, n := range []string{t.Name, shortName(t.Name), t.CodeName, shortName(t.CodeName)} {
			if k := squash(n); k != "" {
				d.names[k] = true
			}
		}
		ix.docs = append(ix.docs, d)
	}
	if n := len(ix.docs); n > 0 {
		for f := field(0); f < numFields; f++ {
			ix.avgLen[f] = float64(total[f]) / float64(n)
		}
	}
	return ix
}

// search scores every document for query and returns those that match,
// best first. Only the first maxQueryTerms distinct terms count.
func (ix *index) search(query string) []Hit {
	terms := uniq(tokens(query, false))
	if len(terms) == 0 {
		// A query of stop words only ("the", "a"): match them anyway.
		terms = uniq(tokens(query, true))
	}
	if len(terms) > maxQueryTerms {
		terms = terms[:maxQueryTerms]
	}
	exact := squash(query)
	n := float64(len(ix.docs))
	var hits []Hit
	for _, d := range ix.docs {
		score := 0.0
		for _, t := range terms {
			df := ix.df[t]
			if df == 0 {
				continue
			}
			tfw := 0.0
			for f := field(0); f < numFields; f++ {
				tf := d.tf[f][t]
				if tf == 0 {
					continue
				}
				norm := 1.0
				if ix.avgLen[f] > 0 {
					norm = 1 - fieldB[f] + fieldB[f]*float64(d.len[f])/ix.avgLen[f]
				}
				tfw += fieldWeight[f] * float64(tf) / norm
			}
			if tfw == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
			score += idf * tfw * (bm25K1 + 1) / (bm25K1 + tfw)
		}
		if exact != "" && d.names[exact] {
			score += exactNameBoost
		}
		if score > 0 {
			hits = append(hits, Hit{Tool: d.tool, Score: score})
		}
	}
	sortHits(hits)
	return hits
}

// toolFields is the text of each field of t.
func toolFields(t Tool) [numFields][]string {
	var out [numFields][]string
	out[fieldName] = []string{t.Name}
	out[fieldDescription] = []string{t.Description}
	collectParams(t.InputSchema, 0, &out)
	return out
}

// collectParams adds the property names, descriptions and string enum
// values of schema (and of its nested objects and array items, up to
// maxSchemaDepth) to the parameter fields.
func collectParams(schema map[string]any, depth int, out *[numFields][]string) {
	if schema == nil || depth > maxSchemaDepth {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out[fieldParamName] = append(out[fieldParamName], name)
		p, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		if desc, ok := p["description"].(string); ok {
			out[fieldParamDescription] = append(out[fieldParamDescription], desc)
		}
		out[fieldParamDescription] = append(out[fieldParamDescription], enumWords(p)...)
		collectParams(p, depth+1, out)
		if items, ok := p["items"].(map[string]any); ok {
			collectParams(items, depth+1, out)
		}
	}
}

// enumWords are a property's string enum values.
func enumWords(p map[string]any) []string {
	var out []string
	switch v := p["enum"].(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, v...)
	}
	return out
}

// shortName is the part of a catalog name after its first dot.
func shortName(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// squash lowercases s and drops everything but letters and digits, so
// "get-client", "get_client" and "getClient" compare equal.
func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func uniq(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// stopWords carry no meaning in a tool search.
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"for": true, "from": true, "in": true, "into": true, "is": true, "it": true, "its": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "this": true, "to": true,
	"with": true, "me": true, "my": true, "i": true, "you": true, "your": true,
}

// tokens splits s into lowercase, stemmed words: at every character that
// is not a letter or digit, and inside words at camelCase boundaries
// (getClientID -> get, client, id). A camelCase word also counts whole
// (getclientid), so "github" finds GitHub and "GitHub" finds github.
// Stop words are dropped unless keepStop is set.
func tokens(s string, keepStop bool) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) == 0 {
			return
		}
		parts := splitCamel(word)
		for _, part := range parts {
			w := strings.ToLower(part)
			if !keepStop && stopWords[w] {
				continue
			}
			out = append(out, stem(w))
		}
		if len(parts) > 1 {
			out = append(out, stem(strings.ToLower(string(word))))
		}
		word = word[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// splitCamel splits a word at lower-to-upper boundaries (getClient) and
// before the last capital of a run followed by a lower-case letter
// (HTTPServer -> HTTP, Server).
func splitCamel(w []rune) []string {
	var parts []string
	start := 0
	for i := 1; i < len(w); i++ {
		prev, cur := w[i-1], w[i]
		boundary := unicode.IsLower(prev) && unicode.IsUpper(cur)
		if !boundary && unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(w) && unicode.IsLower(w[i+1]) {
			boundary = true
		}
		if boundary {
			parts = append(parts, string(w[start:i]))
			start = i
		}
	}
	return append(parts, string(w[start:]))
}

// stem is a light English suffix stripper, applied alike to tools and
// queries: issues/issue, creating/created/create, queries/query and
// listing/lists/list each meet in one form.
func stem(w string) string {
	n := len(w)
	switch {
	case n > 4 && strings.HasSuffix(w, "ies"):
		w = w[:n-3] + "y"
	case n > 4 && strings.HasSuffix(w, "sses"):
		w = w[:n-2]
	case n > 5 && strings.HasSuffix(w, "ing"):
		w = undouble(w[:n-3])
	case n > 4 && strings.HasSuffix(w, "ed"):
		w = undouble(w[:n-2])
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") &&
		!strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		w = w[:n-1]
	}
	if len(w) > 4 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

// undouble drops one of a doubled final consonant left by a stripped
// suffix (shipp -> ship, gett -> get).
func undouble(w string) string {
	n := len(w)
	if n >= 3 && w[n-1] == w[n-2] && strings.IndexByte("bgmnpt", w[n-1]) >= 0 {
		return w[:n-1]
	}
	return w
}

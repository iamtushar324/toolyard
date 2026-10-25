package toolsearch

import (
	"context"
	"fmt"
	"regexp"
	"regexp/syntax"
	"time"
	"unicode/utf8"
)

const (
	// maxRegexInst caps the compiled program of a pattern. RE2 matches in
	// time linear in the text, but the factor is the program's size, and
	// nested counted repetition ((a{50}){50}) makes huge programs from
	// short patterns.
	maxRegexInst = 5000
	// maxRegexRepeat caps one counted repetition ({n}, {n,m}).
	maxRegexRepeat = 100
	// regexBudget bounds one regex search: past it the search stops and
	// says so, whatever the catalog's size. The clock is read before each
	// text, and no text is longer than regexTextCap, so one match can
	// overrun it by one short text's worth at most.
	regexBudget = 250 * time.Millisecond
	// regexTextCap caps each text a pattern is matched against (a name, a
	// description, a parameter's), in bytes: a long description is matched
	// on its first 4 KiB.
	regexTextCap = 4 << 10
	// regexWorkBudget caps the program's instructions times the bytes of
	// text to match, a bound on RE2's work known before it starts. A
	// simple pattern fits any catalog; a pattern near maxRegexInst fits a
	// few hundred KiB of text.
	regexWorkBudget = 1 << 30
)

// compilePattern checks and compiles a search pattern: at most
// MaxPatternLen characters, no counted repetition above maxRegexRepeat,
// and a program of at most maxRegexInst instructions. Case-insensitive
// unless the pattern turns that off itself ((?-i)).
// The second result is the program's size in instructions.
func compilePattern(pattern string) (*regexp.Regexp, int, error) {
	if n := utf8.RuneCountInString(pattern); n > MaxPatternLen {
		return nil, 0, &QueryError{Message: fmt.Sprintf("the pattern is %d characters; a regex search takes at most %d", n, MaxPatternLen)}
	}
	full := "(?i)" + pattern
	tree, err := syntax.Parse(full, syntax.Perl)
	if err != nil {
		return nil, 0, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	if n := maxRepeat(tree); n > maxRegexRepeat {
		return nil, 0, &QueryError{Message: fmt.Sprintf("the pattern repeats something %d times; a regex search allows at most {%d}", n, maxRegexRepeat)}
	}
	prog, err := syntax.Compile(tree.Simplify())
	if err != nil {
		return nil, 0, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	if len(prog.Inst) > maxRegexInst {
		return nil, 0, &QueryError{Message: fmt.Sprintf("the pattern compiles to %d instructions; a regex search allows at most %d: make it simpler", len(prog.Inst), maxRegexInst)}
	}
	re, err := regexp.Compile(full)
	if err != nil {
		return nil, 0, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	return re, len(prog.Inst), nil
}

// capText is text cut to regexTextCap bytes, at a character boundary.
func capText(text string) string {
	if len(text) <= regexTextCap {
		return text
	}
	cut := regexTextCap
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// maxRepeat is the largest repeat count in tree.
func maxRepeat(re *syntax.Regexp) int {
	n := 0
	if re.Op == syntax.OpRepeat {
		n = max(re.Min, re.Max)
	}
	for _, sub := range re.Sub {
		n = max(n, maxRepeat(sub))
	}
	return n
}

// regexSearch matches pattern (Go RE2, so matching time is linear in the
// text whatever the pattern) against each field of each tool and scores a
// tool by the weights of the fields it matched.
//
// Its cost is bounded three ways: the program's size (compilePattern),
// the instructions times the bytes of (capped) text, checked before any
// matching, and a wall-clock budget read before each text.
func regexSearch(ctx context.Context, tools []Tool, pattern string) ([]Hit, error) {
	re, inst, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	fields := make([][numFields][]string, len(tools))
	total := 0
	for i, t := range tools {
		texts := toolFields(t)
		// The code-mode name counts as the name field.
		texts[fieldName] = append(texts[fieldName], t.CodeName)
		for f := range texts {
			for j, text := range texts[f] {
				texts[f][j] = capText(text)
				total += len(texts[f][j])
			}
		}
		fields[i] = texts
	}
	if work := int64(inst) * int64(total); work > regexWorkBudget {
		return nil, &QueryError{Message: fmt.Sprintf("the pattern (%d instructions) is too complex for the %d KiB of tool text it would scan; make it simpler or use mode \"keyword\"", inst, total>>10)}
	}
	deadline := time.Now().Add(regexBudget)
	tooSlow := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return &QueryError{Message: fmt.Sprintf("the pattern took longer than %s over %d tools; make it simpler or use mode \"keyword\"", regexBudget, len(tools))}
		}
		return nil
	}
	var hits []Hit
	for i, t := range tools {
		score := 0.0
		for f := field(0); f < numFields; f++ {
			for _, text := range fields[i][f] {
				if text == "" {
					continue
				}
				if err := tooSlow(); err != nil {
					return nil, err
				}
				if re.MatchString(text) {
					score += fieldWeight[f]
					break
				}
			}
		}
		if score > 0 {
			hits = append(hits, Hit{Tool: t, Score: score})
		}
	}
	sortHits(hits)
	return hits, nil
}

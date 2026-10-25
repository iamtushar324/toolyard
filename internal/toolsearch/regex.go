package toolsearch

import (
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
	// says so, whatever the catalog's size.
	regexBudget = 250 * time.Millisecond
)

// compilePattern checks and compiles a search pattern: at most
// MaxPatternLen characters, no counted repetition above maxRegexRepeat,
// and a program of at most maxRegexInst instructions. Case-insensitive
// unless the pattern turns that off itself ((?-i)).
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if n := utf8.RuneCountInString(pattern); n > MaxPatternLen {
		return nil, &QueryError{Message: fmt.Sprintf("the pattern is %d characters; a regex search takes at most %d", n, MaxPatternLen)}
	}
	full := "(?i)" + pattern
	tree, err := syntax.Parse(full, syntax.Perl)
	if err != nil {
		return nil, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	if n := maxRepeat(tree); n > maxRegexRepeat {
		return nil, &QueryError{Message: fmt.Sprintf("the pattern repeats something %d times; a regex search allows at most {%d}", n, maxRegexRepeat)}
	}
	prog, err := syntax.Compile(tree.Simplify())
	if err != nil {
		return nil, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	if len(prog.Inst) > maxRegexInst {
		return nil, &QueryError{Message: fmt.Sprintf("the pattern compiles to %d instructions; a regex search allows at most %d: make it simpler", len(prog.Inst), maxRegexInst)}
	}
	re, err := regexp.Compile(full)
	if err != nil {
		return nil, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	return re, nil
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
func regexSearch(tools []Tool, pattern string) ([]Hit, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(regexBudget)
	var hits []Hit
	for i, t := range tools {
		if i%16 == 15 && time.Now().After(deadline) {
			return nil, &QueryError{Message: fmt.Sprintf("the pattern took longer than %s over %d tools; make it simpler or use mode \"keyword\"", regexBudget, len(tools))}
		}
		texts := toolFields(t)
		// The code-mode name counts as the name field.
		texts[fieldName] = append(texts[fieldName], t.CodeName)
		score := 0.0
		for f := field(0); f < numFields; f++ {
			for _, text := range texts[f] {
				if text != "" && re.MatchString(text) {
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

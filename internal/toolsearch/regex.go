package toolsearch

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// regexSearch matches pattern (Go RE2, so matching time is linear in the
// text whatever the pattern) against each field of each tool and scores a
// tool by the weights of the fields it matched. The pattern is
// case-insensitive unless it turns that off itself ((?-i)).
func regexSearch(tools []Tool, pattern string) ([]Hit, error) {
	if n := utf8.RuneCountInString(pattern); n > MaxPatternLen {
		return nil, &QueryError{Message: fmt.Sprintf("the pattern is %d characters; a regex search takes at most %d", n, MaxPatternLen)}
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, &QueryError{Message: "the pattern is not a valid RE2 regular expression: " + err.Error()}
	}
	var hits []Hit
	for _, t := range tools {
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

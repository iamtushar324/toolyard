package skillhub

import (
	"fmt"
	"regexp"
	"strings"
)

// builtinExcludes are left out of every source.
var builtinExcludes = []string{".git/", "node_modules/", "__pycache__/", "*.pyc", ".DS_Store"}

// maxExcludes caps a source's exclude globs.
const maxExcludes = 100

// excluder decides which files a source leaves out. Globs follow
// .gitignore's shape: a glob without "/" matches a file or folder name at
// any depth; a glob with "/" matches from the source folder; a trailing
// "/" matches folders only. "*" matches within a name, "**" across
// folders, "?" one character, and [...] a character class.
type excluder struct {
	rules []excludeRule
}

type excludeRule struct {
	re       *regexp.Regexp
	dirOnly  bool
	anchored bool
}

// newExcluder compiles the built-in excludes and the source's globs.
func newExcluder(globs []string) (*excluder, error) {
	ex := &excluder{}
	for _, g := range append(append([]string{}, builtinExcludes...), globs...) {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		rule := excludeRule{dirOnly: strings.HasSuffix(g, "/")}
		g = strings.Trim(g, "/")
		if g == "" {
			continue
		}
		rule.anchored = strings.Contains(g, "/")
		re, err := compileGlob(g)
		if err != nil {
			return nil, err
		}
		rule.re = re
		ex.rules = append(ex.rules, rule)
	}
	return ex, nil
}

// excluded reports whether the file at p (relative to the source folder)
// is left out.
func (e *excluder) excluded(p string) bool {
	if e == nil {
		return false
	}
	segs := strings.Split(p, "/")
	for _, r := range e.rules {
		if r.matches(segs) {
			return true
		}
	}
	return false
}

func (r excludeRule) matches(segs []string) bool {
	for i := range segs {
		isDir := i < len(segs)-1
		if r.dirOnly && !isDir {
			continue
		}
		subject := segs[i]
		if r.anchored {
			subject = strings.Join(segs[:i+1], "/")
		}
		if r.re.MatchString(subject) {
			return true
		}
	}
	return false
}

// validateGlobs checks a list of exclude globs.
func validateGlobs(globs []string) ([]string, error) {
	if len(globs) > maxExcludes {
		return nil, fmt.Errorf("%w: at most %d exclude globs", ErrInvalid, maxExcludes)
	}
	var out []string
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if len(g) > 200 {
			return nil, fmt.Errorf("%w: exclude glob %q is longer than 200 characters", ErrInvalid, g)
		}
		if _, err := compileGlob(strings.Trim(g, "/")); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// compileGlob turns a glob into an anchored regular expression.
func compileGlob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				if i+2 < len(g) && g[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(g[i+1:], ']')
			if end <= 0 {
				return nil, fmt.Errorf("%w: glob %q has an unclosed or empty [", ErrInvalid, g)
			}
			class := g[i+1 : i+1+end]
			if strings.ContainsAny(class, "\\[/") {
				return nil, fmt.Errorf("%w: glob %q has an unsupported character class", ErrInvalid, g)
			}
			if class[0] == '!' {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(g[i : i+1]))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("%w: glob %q: %v", ErrInvalid, g, err)
	}
	return re, nil
}

// globMatch matches a grep path glob: without "/" against the file name,
// with "/" against the path relative to the source folder.
func globMatch(re *regexp.Regexp, anchored bool, p string) bool {
	if anchored {
		return re.MatchString(p)
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return re.MatchString(p)
}

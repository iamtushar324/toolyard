package codemode

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Naming follows Bifrost so a skill's hardcoded identifiers keep working.
// A tool is exposed under its canonical identifier: the upstream name
// lowercased, with '-' and spaces turned into '_', other punctuation
// dropped and trailing underscores trimmed (get-All-Clients -> get_all_clients).
// When the upstream name is itself a valid identifier once '-' becomes '_',
// that case-preserving form is bound too as an alias (getClient stays
// callable as getClient as well as getclient).
//
// toolyard adds one rule Bifrost lacks: two tools on one server that mangle
// to the same identifier are refused, not silently merged. Neither is bound
// under the shared name; a script that calls it gets an error naming both
// real tools, and each keeps whatever unique alias it has.

// canonicalName is the identifier a tool is exposed under.
func canonicalName(name string) string {
	var b strings.Builder
	for i, r := range []rune(name) {
		switch {
		case i == 0 && (unicode.IsLetter(r) || r == '_'):
			b.WriteRune(unicode.ToLower(r))
		case i == 0:
			b.WriteRune('_')
			if unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsSpace(r) || r == '-':
			if s := b.String(); s != "" && !strings.HasSuffix(s, "_") {
				b.WriteRune('_')
			}
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if out == "" {
		return "tool"
	}
	return out
}

// aliasName is the case-preserving compatibility alias, or "" when the raw
// name isn't an identifier after '-' -> '_' or equals the canonical name.
func aliasName(name string) string {
	alias := strings.ReplaceAll(name, "-", "_")
	if !isIdentifier(alias) || alias == canonicalName(name) {
		return ""
	}
	return alias
}

// isIdentifier reports whether s is a Starlark identifier.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if unicode.IsLetter(r) || r == '_' || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

// binding is one tool under the identifier it is exposed as.
type binding struct {
	tool  Tool
	ident string
}

// server is every tool of one server key, resolved to identifiers.
type server struct {
	name string
	// bound lists the tools that got an identifier, sorted by it. These are
	// the .pyi files and the def names.
	bound []binding
	// byIdent resolves every callable identifier (canonical names and
	// aliases) to its tool.
	byIdent map[string]*binding
	// ambiguous maps an identifier two or more tools mangle to onto their
	// real names. Such an identifier is never callable.
	ambiguous map[string][]string
}

// buildServers groups tools by server key and resolves identifiers.
func buildServers(tools []Tool) []*server {
	byName := map[string]*server{}
	for _, t := range tools {
		s, ok := byName[t.Server]
		if !ok {
			s = &server{name: t.Server, byIdent: map[string]*binding{}, ambiguous: map[string][]string{}}
			byName[t.Server] = s
		}
		s.bound = append(s.bound, binding{tool: t})
	}
	out := make([]*server, 0, len(byName))
	for _, s := range byName {
		s.resolve()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// resolve assigns identifiers. Every tool contributes its canonical name
// and its alias to a tally; an identifier claimed by exactly one tool is
// that tool's. A tool whose canonical name is contested falls back to its
// alias if that is uncontested, otherwise it stays unbound.
func (s *server) resolve() {
	claims := map[string]int{}
	for _, b := range s.bound {
		claims[canonicalName(b.tool.Name)]++
		if a := aliasName(b.tool.Name); a != "" {
			claims[a]++
		}
	}
	var bound []binding
	for _, b := range s.bound {
		canon := canonicalName(b.tool.Name)
		alias := aliasName(b.tool.Name)
		switch {
		case claims[canon] == 1:
			b.ident = canon
		case alias != "" && claims[alias] == 1:
			b.ident = alias
		default:
			s.ambiguous[canon] = append(s.ambiguous[canon], b.tool.Name)
			continue
		}
		if claims[canon] > 1 {
			s.ambiguous[canon] = append(s.ambiguous[canon], b.tool.Name)
		}
		bound = append(bound, b)
	}
	sort.Slice(bound, func(i, j int) bool { return bound[i].ident < bound[j].ident })
	s.bound = bound
	for i := range s.bound {
		b := &s.bound[i]
		s.byIdent[b.ident] = b
		if a := aliasName(b.tool.Name); a != "" && a != b.ident && claims[a] == 1 {
			s.byIdent[a] = b
		}
	}
	for ident, names := range s.ambiguous {
		sort.Strings(names)
		s.ambiguous[ident] = names
	}
}

// lookup finds a tool by any of its identifier forms, case-insensitively,
// the way Bifrost matches readToolFile and getToolDocs names. It returns
// the tool, or the real names an ambiguous identifier could mean.
func (s *server) lookup(name string) (*binding, []string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	// The def names in the stubs are authoritative, so an exact identifier
	// wins; then a contested identifier is reported before any loose match
	// could pick one of its tools.
	if b, ok := s.byIdent[name]; ok {
		return b, nil
	}
	if names, ok := s.ambiguous[canonicalName(name)]; ok {
		return nil, names
	}
	want := strings.ToLower(name)
	var found *binding
	for i := range s.bound {
		b := &s.bound[i]
		if want == strings.ToLower(b.ident) || want == strings.ToLower(b.tool.Name) ||
			want == strings.ToLower(aliasName(b.tool.Name)) {
			if found != nil && found != b {
				return nil, []string{found.tool.Name, b.tool.Name}
			}
			found = b
		}
	}
	return found, nil
}

// ambiguityError explains an identifier that names more than one tool.
func ambiguityError(serverName, ident string, names []string) error {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Errorf("%s.%s is ambiguous: it could be %s. toolyard does not guess; call the tool through tools.execute with its exact catalog name (%s.<name>)",
		serverName, ident, strings.Join(quoted, " or "), serverName)
}

// findServer matches a server key case-insensitively. It returns the
// server, or the names that all match when the key is not unique.
func findServer(servers []*server, name string) (*server, []string) {
	want := strings.ToLower(strings.TrimSpace(name))
	var matches []*server
	for _, s := range servers {
		if strings.ToLower(s.name) == want {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	}
	names := make([]string, len(matches))
	for i, s := range matches {
		names[i] = s.name
	}
	return nil, names
}

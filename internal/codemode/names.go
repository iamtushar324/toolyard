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
// A server key that is already an identifier (BkCoreServices, memory) is
// bound as itself. One that is not (bk-core, 9lives, load) is bound under a
// sanitised identifier, and the stubs, usage lines and server key list all
// show that identifier.
//
// toolyard adds one rule Bifrost lacks: two names that mangle to the same
// identifier, tools on one server or servers in one catalog, are refused,
// not silently merged. Neither is bound under the shared name; a script
// that uses it gets an error naming both real names.

// starlarkKeywords cannot be identifiers, so a name that is one gets a
// trailing underscore.
var starlarkKeywords = map[string]bool{
	"and": true, "break": true, "continue": true, "def": true, "elif": true, "else": true,
	"for": true, "if": true, "in": true, "lambda": true, "load": true, "not": true, "or": true,
	"pass": true, "return": true, "while": true,
	// Reserved for Python compatibility.
	"as": true, "assert": true, "async": true, "await": true, "class": true, "del": true,
	"except": true, "finally": true, "from": true, "global": true, "import": true, "is": true,
	"nonlocal": true, "raise": true, "try": true, "with": true, "yield": true,
}

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
	if starlarkKeywords[out] {
		out += "_"
	}
	return out
}

// aliasName is the case-preserving compatibility alias, or "" when the raw
// name isn't a bindable identifier after '-' -> '_' or equals the canonical
// name.
func aliasName(name string) string {
	alias := strings.ReplaceAll(name, "-", "_")
	if !isBindable(alias) || alias == canonicalName(name) {
		return ""
	}
	return alias
}

// serverIdent is the identifier a server key is bound as: the key itself
// when that works, else '-', '.' and spaces become '_', other punctuation
// is dropped, a leading digit gets a '_' prefix and a keyword a '_' suffix.
// "" means the key cannot be bound at all.
func serverIdent(name string) string {
	if isBindable(name) {
		return name
	}
	var b strings.Builder
	for i, r := range name {
		switch {
		case unicode.IsLetter(r) || r == '_':
			b.WriteRune(r)
		case unicode.IsDigit(r):
			if i == 0 {
				b.WriteRune('_')
			}
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '.':
			b.WriteRune('_')
		}
	}
	out := b.String()
	if starlarkKeywords[out] {
		out += "_"
	}
	if !isBindable(out) {
		return ""
	}
	return out
}

// isIdentifier reports whether s has the shape of a Starlark identifier.
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

// isBindable reports whether s can be a name in a script.
func isBindable(s string) bool { return isIdentifier(s) && !starlarkKeywords[s] }

// binding is one tool under the identifier it is exposed as.
type binding struct {
	tool  Tool
	ident string
}

// server is every tool of one server key, resolved to identifiers.
type server struct {
	// name is the server key (the upstream name); ident is what a script
	// writes.
	name  string
	ident string
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

// catalog is one caller's servers, resolved.
type catalog struct {
	// servers are the bound ones, sorted by identifier.
	servers []*server
	// refused maps an identifier two or more server keys mangle to (or ""
	// for keys that cannot be bound) onto those keys. None of them is bound.
	refused map[string][]string
}

// buildCatalog groups tools by server key and resolves identifiers.
func buildCatalog(tools []Tool) *catalog {
	byName := map[string]*server{}
	for _, t := range tools {
		s, ok := byName[t.Server]
		if !ok {
			s = &server{name: t.Server, ident: serverIdent(t.Server), byIdent: map[string]*binding{}, ambiguous: map[string][]string{}}
			byName[t.Server] = s
		}
		s.bound = append(s.bound, binding{tool: t})
	}
	claims := map[string]int{}
	for _, s := range byName {
		claims[s.ident]++
	}
	cat := &catalog{refused: map[string][]string{}}
	for _, s := range byName {
		if s.ident == "" || claims[s.ident] > 1 {
			cat.refused[s.ident] = append(cat.refused[s.ident], s.name)
			continue
		}
		s.resolve()
		cat.servers = append(cat.servers, s)
	}
	sort.Slice(cat.servers, func(i, j int) bool { return cat.servers[i].ident < cat.servers[j].ident })
	for ident, names := range cat.refused {
		sort.Strings(names)
		cat.refused[ident] = names
	}
	return cat
}

// idents lists the bound server identifiers, sorted: the "Available
// server keys".
func (c *catalog) idents() []string {
	out := make([]string, len(c.servers))
	for i, s := range c.servers {
		out[i] = s.ident
	}
	return out
}

// byIdent finds a bound server by its exact identifier.
func (c *catalog) byIdent(ident string) *server {
	for _, s := range c.servers {
		if s.ident == ident {
			return s
		}
	}
	return nil
}

// wire is the catalog as the worker binds it.
func (c *catalog) wire() []wireServer {
	out := make([]wireServer, 0, len(c.servers))
	for _, s := range c.servers {
		ws := wireServer{Ident: s.ident, Members: make([]string, 0, len(s.byIdent))}
		for ident := range s.byIdent {
			ws.Members = append(ws.Members, ident)
		}
		sort.Strings(ws.Members)
		for ident, names := range s.ambiguous {
			if _, callable := s.byIdent[ident]; callable {
				continue
			}
			if ws.Refused == nil {
				ws.Refused = map[string]string{}
			}
			ws.Refused[ident] = ambiguityError(s.ident, ident, names).Error()
		}
		out = append(out, ws)
	}
	return out
}

// refusedNotes are the header comments naming server keys that could not
// be bound.
func (c *catalog) refusedNotes() []string {
	idents := make([]string, 0, len(c.refused))
	for ident := range c.refused {
		idents = append(idents, ident)
	}
	sort.Strings(idents)
	var out []string
	for _, ident := range idents {
		names := quoteAll(c.refused[ident])
		if ident == "" {
			out = append(out, fmt.Sprintf("# Not callable: server %s (no identifier can be made of the name)", strings.Join(names, ", ")))
			continue
		}
		out = append(out, fmt.Sprintf("# Not callable: server %s (ambiguous, could be %s)", ident, strings.Join(names, " or ")))
	}
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
func ambiguityError(serverIdent, ident string, names []string) error {
	return fmt.Errorf("%s.%s is ambiguous: it could be %s. toolyard does not guess; call the tool through tools.execute with its exact catalog name (%s.<name>)",
		serverIdent, ident, strings.Join(quoteAll(names), " or "), serverIdent)
}

// findServer matches a server by identifier or key, case-insensitively. It
// returns the server; or the identifiers that all match when the name is
// not unique; or, for a key that was refused, the keys it collides with.
func findServer(c *catalog, name string) (srv *server, several []string, refused []string) {
	want := strings.ToLower(strings.TrimSpace(name))
	var matches []*server
	for _, s := range c.servers {
		if strings.ToLower(s.ident) == want || strings.ToLower(s.name) == want {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
	case 1:
		return matches[0], nil, nil
	default:
		names := make([]string, len(matches))
		for i, s := range matches {
			names[i] = s.ident
		}
		return nil, names, nil
	}
	for ident, keys := range c.refused {
		if strings.ToLower(ident) == want {
			return nil, nil, keys
		}
		for _, k := range keys {
			if strings.ToLower(k) == want {
				return nil, nil, keys
			}
		}
	}
	return nil, nil, nil
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

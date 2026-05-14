package skills

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Frontmatter is the parsed YAML preamble at the top of a SKILL.md file.
// Only the fields Claude Code reads + the toolyard `tags` extension are
// first-class; anything else is preserved in Extra so a round-trip write
// doesn't drop fields the user manually added (e.g. version). Hand-rolled
// YAML rather than pulling yaml.v3 directly because the schema is tiny and
// adding a direct dep isn't worth the supply-chain surface.
type Frontmatter struct {
	Name        string // required
	Description string // required by Claude Code in practice; we warn but don't reject
	// Tags is a flat list of categories (e.g. "coding", "personal-life") used
	// by skills.list / skills.list_tags so agents can browse the catalog
	// before pulling specific skills. Tags are normalised via KebabSlug on
	// parse, so "Personal Life", "personal_life", "personal-life" all
	// collapse to the same canonical form.
	Tags  []string
	Extra map[string]string // verbatim string-valued lines we didn't recognise
}

// Skill bundles a parsed SKILL.md plus the optional companion files that
// make up the on-disk layout.
type Skill struct {
	Slug        string // canonical, derived from Frontmatter.Name
	Frontmatter Frontmatter
	Body        string            // markdown body after the closing `---`
	AgentsYAML  string            // optional contents of agents/openai.yaml
	Scripts     map[string]string // optional scripts/ payloads, keyed by relative path under scripts/
}

// Limits — same order of magnitude as notes.maxIngestBytes.
const (
	maxSkillBytes   = 256 << 10 // 256 KiB total cap across SKILL.md + agents + scripts
	maxScriptKeyLen = 200
)

// Errors surfaced to callers.
var (
	ErrFrontmatterMissing  = errors.New("SKILL.md must start with a YAML frontmatter block delimited by `---`")
	ErrFrontmatterUnclosed = errors.New("SKILL.md YAML frontmatter is missing its closing `---`")
	ErrNameRequired        = errors.New("frontmatter.name is required")
	ErrSlugMismatch        = errors.New("slug does not match kebab-case of frontmatter.name")
	ErrScriptPathEscape    = errors.New("script path escapes the scripts/ directory")
	ErrScriptPathInvalid   = errors.New("script path contains characters that are not allowed")
	ErrPayloadTooLarge     = fmt.Errorf("skill payload exceeds %d bytes", maxSkillBytes)
)

var (
	// kebab segments: lowercase ASCII letters, digits, hyphens. We're
	// strict because Claude Code itself derives the on-disk dir name from
	// frontmatter.name and refuses spaces; better to reject early.
	slugRe        = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	scriptPathRe  = regexp.MustCompile(`^[A-Za-z0-9_./\-]+$`)
	frontmatterRe = regexp.MustCompile(`(?s)\A---\s*\n(.*?)\n---\s*\n?`)
)

// KebabSlug derives the canonical skill slug from frontmatter.name. Spaces
// and underscores collapse to hyphens, non-alphanumerics drop, mixed case
// lower-cases. Returns "" if nothing usable remains.
func KebabSlug(name string) string {
	var b strings.Builder
	prevHyphen := true // leading hyphens are stripped
	for _, r := range strings.TrimSpace(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevHyphen = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// bom is the UTF-8 byte-order-mark some editors prepend (notably VSCode
// on Windows). Written via a unicode escape so the Go scanner doesn't
// see a stray BOM mid-file.
const bom = "\ufeff"

// ParseSkillMD splits a SKILL.md blob into its frontmatter and body. The
// body is the markdown that follows the closing `---` line (no leading
// newline trimmed beyond what the closing delimiter consumed).
func ParseSkillMD(skillMD string) (Frontmatter, string, error) {
	// Strip a leading UTF-8 BOM if any so the regex anchor still matches.
	skillMD = strings.TrimPrefix(skillMD, bom)
	if !strings.HasPrefix(skillMD, "---") {
		return Frontmatter{}, "", ErrFrontmatterMissing
	}
	m := frontmatterRe.FindStringSubmatchIndex(skillMD)
	if m == nil {
		return Frontmatter{}, "", ErrFrontmatterUnclosed
	}
	yaml := skillMD[m[2]:m[3]]
	body := skillMD[m[1]:]
	fm, err := parseFrontmatterYAML(yaml)
	if err != nil {
		return Frontmatter{}, "", err
	}
	return fm, body, nil
}

// parseFrontmatterYAML accepts the SKILL.md flavour Claude Code itself
// writes: top-level `key: value` lines, optionally quoted, no nested
// mappings, no list values, no multi-line scalars. Anything fancier is
// rejected at validation time but stored verbatim in Extra so we can
// round-trip without dropping user-added fields.
func parseFrontmatterYAML(body string) (Frontmatter, error) {
	out := Frontmatter{Extra: map[string]string{}}
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexByte(trimmed, ':')
		if idx < 0 {
			return Frontmatter{}, fmt.Errorf("frontmatter line %d: missing ':'", i+1)
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.TrimSpace(trimmed[idx+1:])
		val = unquote(val)
		switch key {
		case "name":
			out.Name = val
		case "description":
			out.Description = val
		case "tags":
			out.Tags = ParseTagList(val)
		default:
			out.Extra[key] = val
		}
	}
	return out, nil
}

// ParseTagList accepts the two YAML flavours we support for the `tags`
// field: a flow-style list (`[coding, ai]`) or a comma-separated string
// (`coding, ai`). Each tag is run through KebabSlug so casing/spacing
// can't fragment the catalog. Empty input returns nil so a round-trip
// serialise produces no `tags:` line.
func ParseTagList(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(v, ",") {
		t := KebabSlug(unquote(strings.TrimSpace(raw)))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unquote strips a single matching pair of "..." or '...' around v.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// SerializeSkillMD writes the canonical SKILL.md layout: name first,
// description second, then any Extra keys (sorted not necessary in practice
// — preserving insertion order would require an OrderedMap; the simpler
// thing is to just put them after the two well-known keys).
func SerializeSkillMD(fm Frontmatter, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(yamlString(fm.Name))
	b.WriteByte('\n')
	if fm.Description != "" {
		b.WriteString("description: ")
		b.WriteString(yamlString(fm.Description))
		b.WriteByte('\n')
	}
	if len(fm.Tags) > 0 {
		b.WriteString("tags: [")
		for i, tag := range fm.Tags {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(tag)
		}
		b.WriteString("]\n")
	}
	for k, v := range fm.Extra {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(yamlString(v))
		b.WriteByte('\n')
	}
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

// yamlString quotes a value iff it contains characters that would confuse
// a YAML parser. Conservative: any `:`, `#`, `'`, `"`, leading/trailing
// space, or characters outside printable ASCII triggers quoting. The
// quoting style is double-quoted with backslash escapes.
func yamlString(v string) string {
	needsQuote := false
	for _, r := range v {
		if r == ':' || r == '#' || r == '"' || r == '\'' || r == '\n' || r == '\r' || r == '\t' {
			needsQuote = true
			break
		}
	}
	if !needsQuote && v == strings.TrimSpace(v) {
		return v
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(v)
	return `"` + esc + `"`
}

// Validate checks slug / name / payload-size invariants. Called by the
// service before any write hits disk.
func (s Skill) Validate() error {
	if strings.TrimSpace(s.Frontmatter.Name) == "" {
		return ErrNameRequired
	}
	canonical := KebabSlug(s.Frontmatter.Name)
	slug := strings.TrimSpace(s.Slug)
	if slug == "" {
		slug = canonical
	}
	if slug != canonical || !slugRe.MatchString(slug) {
		return fmt.Errorf("%w: have %q, want %q", ErrSlugMismatch, slug, canonical)
	}
	total := len(s.Body) + len(s.AgentsYAML)
	for k, v := range s.Scripts {
		if len(k) > maxScriptKeyLen {
			return fmt.Errorf("%w: %q", ErrScriptPathInvalid, k)
		}
		if !scriptPathRe.MatchString(k) {
			return fmt.Errorf("%w: %q", ErrScriptPathInvalid, k)
		}
		// Reject any `..` segment anywhere in the path — even mid-string
		// like "ok/../bad.sh", which filepath.Clean would normalise away
		// and obscure the caller's intent. Same goes for absolute or
		// `./`-prefixed paths.
		if strings.HasPrefix(k, "/") || strings.HasPrefix(k, "./") {
			return fmt.Errorf("%w: %q", ErrScriptPathEscape, k)
		}
		for _, seg := range strings.Split(k, "/") {
			if seg == ".." {
				return fmt.Errorf("%w: %q", ErrScriptPathEscape, k)
			}
		}
		total += len(v)
	}
	if total > maxSkillBytes {
		return ErrPayloadTooLarge
	}
	return nil
}

// CanonicalSlug returns Validate's view of the slug — useful for the
// service to derive the on-disk directory when the caller didn't supply
// one explicitly.
func (s Skill) CanonicalSlug() string {
	return KebabSlug(s.Frontmatter.Name)
}

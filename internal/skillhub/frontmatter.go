package skillhub

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// maxFrontmatterBytes caps the YAML a SKILL.md may open with.
	maxFrontmatterBytes = 64 << 10
	// maxYAMLNodes caps the values one frontmatter expands to (aliases
	// included), so a crafted file cannot blow up.
	maxYAMLNodes = 10000
	// maxYAMLDepth caps nesting.
	maxYAMLDepth = 32
	// maxDescription is the Agent Skills limit on description.
	maxDescription = 1024
	// maxNameLen is the Agent Skills limit on name.
	maxNameLen = 64
	// maxCompatibility is the Agent Skills limit on compatibility.
	maxCompatibility = 500
	// runsKey is the frontmatter metadata key that says where a skill runs.
	runsKey = "toolyard-runs"
)

// specFields are the frontmatter fields of the Agent Skills specification.
var specFields = map[string]bool{
	"name": true, "description": true, "license": true,
	"allowed-tools": true, "metadata": true, "compatibility": true,
}

var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

var errNoFrontmatter = errors.New("SKILL.md does not start with a YAML frontmatter block between --- lines")

// splitFrontmatter splits a SKILL.md into its YAML frontmatter and the
// body after the closing --- line.
func splitFrontmatter(md string) (yamlText, body string, err error) {
	s := strings.TrimPrefix(md, "\ufeff")
	nl := strings.IndexByte(s, '\n')
	if nl < 0 || strings.TrimRight(s[:nl], " \t\r") != "---" {
		return "", "", errNoFrontmatter
	}
	start := nl + 1
	for pos := start; pos <= len(s); {
		end := strings.IndexByte(s[pos:], '\n')
		lineEnd := len(s)
		if end >= 0 {
			lineEnd = pos + end
		}
		line := strings.TrimRight(s[pos:lineEnd], " \t\r")
		if line == "---" || line == "..." {
			yamlText = s[start:pos]
			if lineEnd < len(s) {
				body = s[lineEnd+1:]
			}
			return yamlText, body, nil
		}
		if end < 0 {
			break
		}
		pos = lineEnd + 1
	}
	return "", "", errors.New("SKILL.md frontmatter has no closing --- line")
}

// parseYAMLMap decodes frontmatter YAML into JSON-ready values. Scalars
// keep what they say: a timestamp stays its text, numbers stay numbers
// unless they do not fit JSON.
func parseYAMLMap(text string) (map[string]any, error) {
	if len(text) > maxFrontmatterBytes {
		return nil, fmt.Errorf("the frontmatter is larger than %d bytes", maxFrontmatterBytes)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return map[string]any{}, nil
	}
	c := &yamlConv{budget: maxYAMLNodes}
	v, err := c.value(&doc, 0)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the frontmatter is not a mapping of keys to values")
	}
	return m, nil
}

type yamlConv struct{ budget int }

func (c *yamlConv) value(n *yaml.Node, depth int) (any, error) {
	c.budget--
	if c.budget < 0 {
		return nil, fmt.Errorf("the frontmatter expands to more than %d values", maxYAMLNodes)
	}
	if depth > maxYAMLDepth {
		return nil, fmt.Errorf("the frontmatter nests deeper than %d levels", maxYAMLDepth)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return c.value(n.Content[0], depth)
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, nil
		}
		return c.value(n.Alias, depth+1)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := c.value(item, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		return c.mapping(n, depth)
	case yaml.ScalarNode:
		return scalarValue(n), nil
	}
	return nil, nil
}

func (c *yamlConv) mapping(n *yaml.Node, depth int) (any, error) {
	out := make(map[string]any, len(n.Content)/2)
	var merges []map[string]any
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.AliasNode && k.Alias != nil {
			k = k.Alias
		}
		if k.Kind != yaml.ScalarNode {
			return nil, errors.New("the frontmatter has a key that is not a plain value")
		}
		val, err := c.value(v, depth+1)
		if err != nil {
			return nil, err
		}
		if k.ShortTag() == "!!merge" {
			switch mv := val.(type) {
			case map[string]any:
				merges = append(merges, mv)
			case []any:
				for _, item := range mv {
					if m, ok := item.(map[string]any); ok {
						merges = append(merges, m)
					}
				}
			}
			continue
		}
		out[k.Value] = val
	}
	for _, m := range merges {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

// scalarValue is a scalar as JSON: null, bool, number or string.
func scalarValue(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		switch strings.ToLower(n.Value) {
		case "true", "yes", "on":
			return true
		case "false", "no", "off":
			return false
		}
	case "!!int":
		if v, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64); err == nil {
			return v
		}
	case "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f
		}
	}
	return n.Value
}

// storableText reports text a database text column takes on both
// engines: valid UTF-8 without NUL bytes.
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// skillMeta is what the hub reads from one SKILL.md.
type skillMeta struct {
	name        string
	description string
	runs        Runs // metadata.toolyard-runs; "" when unset
	frontmatter map[string]any
}

// parseSkillMD validates one SKILL.md. folderName is the last segment of
// the skill's folder (the source folder's own name, or the repository's,
// for a skill at the source folder itself). repoFile is the SKILL.md's
// repository-relative path, for the warnings. skip means the skill is not
// served.
func parseSkillMD(repoFile, folderName string, content []byte) (meta skillMeta, warns []Warning, skip bool) {
	warn := func(code, msg string) {
		warns = append(warns, Warning{Path: repoFile, Code: code, Message: msg, Severity: "warn"})
	}
	skipWith := func(code, msg string) (skillMeta, []Warning, bool) {
		warns = append(warns, Warning{Path: repoFile, Code: code, Message: msg, Severity: "skip"})
		return skillMeta{}, warns, true
	}
	text, _, err := splitFrontmatter(string(content))
	if err != nil {
		return skipWith("missing_frontmatter", "skipped: "+err.Error())
	}
	fm, err := parseYAMLMap(text)
	if err != nil {
		return skipWith("invalid_yaml", "skipped: the frontmatter is not valid YAML: "+cleanMessage(err.Error()))
	}
	name, _ := fm["name"].(string)
	desc, _ := fm["description"].(string)
	name, desc = strings.TrimSpace(name), strings.TrimSpace(desc)
	switch {
	case !storableText(name) || !storableText(desc):
		// A YAML escape ("\0") can put a NUL byte in a string, which
		// Postgres text refuses.
		return skipWith("invalid_text", "skipped: the name or description holds a NUL byte or invalid UTF-8")
	case name == "":
		return skipWith("missing_field", "skipped: the frontmatter has no name")
	case desc == "":
		return skipWith("missing_field", "skipped: the frontmatter has no description")
	case name != folderName:
		return skipWith("name_mismatch", fmt.Sprintf("skipped: name %q differs from its folder %q (the Agent Skills specification requires them to match)", name, folderName))
	}
	if len(name) > maxNameLen || !skillNameRe.MatchString(name) {
		warn("invalid_name", "name should be 1-64 lowercase letters, digits and single hyphens")
	}
	if n := utf8.RuneCountInString(desc); n > maxDescription {
		warn("description_too_long", fmt.Sprintf("description is %d characters; the limit is %d", n, maxDescription))
	}
	if comp, ok := fm["compatibility"].(string); ok && utf8.RuneCountInString(comp) > maxCompatibility {
		warn("compatibility_too_long", fmt.Sprintf("compatibility is longer than %d characters", maxCompatibility))
	}
	for _, k := range sortedPaths(fm) {
		if !specFields[k] {
			warn("unknown_field", fmt.Sprintf("frontmatter field %q is not in the Agent Skills specification", k))
		}
	}
	meta = skillMeta{name: name, description: desc, frontmatter: fm}
	if md, ok := fm["metadata"]; ok {
		mm, isMap := md.(map[string]any)
		if !isMap {
			warn("invalid_metadata", "metadata should be a mapping of keys to values")
		}
		if r, ok := mm[runsKey]; ok {
			switch rs, _ := r.(string); Runs(strings.ToLower(strings.TrimSpace(rs))) {
			case RunsHost:
				meta.runs = RunsHost
			case RunsAnywhere:
				meta.runs = RunsAnywhere
			default:
				warn("invalid_runs", fmt.Sprintf("metadata.%s should be %q or %q", runsKey, RunsHost, RunsAnywhere))
			}
		}
	}
	return meta, warns, false
}

package skillhub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"path"
	"sort"
	"strings"
)

// skillFile is the file name that makes a folder a skill.
const skillFile = "SKILL.md"

// maxWarnings caps the warnings one version records.
const maxWarnings = 1000

// fileLevelCodes are the warnings about one file's content (not about a
// skill): an incremental update carries them over for unchanged files.
var fileLevelCodes = map[string]bool{
	"binary": true, "too_large": true, "secret": true, "symlink": true,
	"special": true, "excluded": true,
}

// builtSkill is one skill of a version being built.
type builtSkill struct {
	path        string // folder relative to the source folder; "" for the folder itself
	meta        skillMeta
	skillMD     *candidate
	filesDigest string
}

// builtFile is one served file of a version being built.
type builtFile struct {
	*candidate
	skill string // innermost skill folder holding it
}

// built is a version ready to store.
type built struct {
	skills     []*builtSkill // sorted by path
	files      []builtFile   // sorted by path
	warnings   []Warning
	totalBytes int64
}

// discover finds the skills in a file set and keeps the files inside
// skill folders. contents holds the bytes of SKILL.md files whose
// candidate has none (an incremental update), by digest. folder is the
// source folder and rootName the name a skill at the folder itself must
// have.
func discover(fs *fileSet, folder, rootName string, contents map[string][]byte) (*built, error) {
	b := &built{}
	var warns []Warning
	skills := map[string]*builtSkill{}
	for _, p := range sortedPaths(fs.files) {
		if path.Base(p) != skillFile {
			continue
		}
		dir := skillDir(p)
		c := fs.files[p]
		data := c.data
		if data == nil {
			stored, ok := contents[c.sha]
			if !ok {
				return nil, fmt.Errorf("skillhub: the stored copy of %s is missing", repoPath(folder, p))
			}
			data = stored
		}
		name := rootName
		if dir != "" {
			name = path.Base(dir)
		}
		meta, w, skip := parseSkillMD(repoPath(folder, p), name, data)
		warns = append(warns, w...)
		if skip {
			continue
		}
		skills[dir] = &builtSkill{path: dir, meta: meta, skillMD: c}
	}
	if len(skills) > fs.lim.skills {
		return nil, fmt.Errorf("%w: %d skills, over the limit of %d", ErrLimit, len(skills), fs.lim.skills)
	}
	digests := map[string]hash.Hash{}
	for _, p := range sortedPaths(fs.files) {
		inner, ok := innermostSkill(p, skills)
		if !ok {
			continue
		}
		c := fs.files[p]
		b.files = append(b.files, builtFile{candidate: c, skill: inner})
		b.totalBytes += c.size
		for _, sp := range enclosingSkills(p, skills) {
			h := digests[sp]
			if h == nil {
				h = sha256.New()
				digests[sp] = h
			}
			fmt.Fprintf(h, "%s\x00%s\n", relTo(sp, p), c.sha)
		}
	}
	if len(b.files) > fs.lim.files {
		return nil, fmt.Errorf("%w: %d files in skill folders, over the limit of %d", ErrLimit, len(b.files), fs.lim.files)
	}
	for _, dir := range sortedPaths(skills) {
		sk := skills[dir]
		if h := digests[dir]; h != nil {
			sk.filesDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
		}
		b.skills = append(b.skills, sk)
	}
	// File warnings matter only inside skill folders.
	for _, p := range sortedPaths(fs.omitted) {
		if _, ok := innermostSkill(p, skills); ok {
			w := fs.omitted[p]
			w.Path = repoPath(folder, p)
			warns = append(warns, w)
		}
	}
	excluded := append([]string(nil), fs.excluded...)
	sort.Strings(excluded)
	for i, p := range excluded {
		if i > 0 && excluded[i-1] == p {
			continue
		}
		if _, ok := innermostSkill(p, skills); ok {
			warns = append(warns, Warning{Path: repoPath(folder, p), Code: "excluded", Message: "omitted: matches an exclude glob", Severity: "skip"})
		}
	}
	b.warnings = capWarnings(warns)
	return b, nil
}

// capWarnings keeps the first maxWarnings warnings and says how many more
// there were.
func capWarnings(warns []Warning) []Warning {
	if len(warns) <= maxWarnings {
		return warns
	}
	out := append([]Warning(nil), warns[:maxWarnings]...)
	return append(out, Warning{Code: "too_many_warnings", Severity: "warn",
		Message: fmt.Sprintf("%d more warnings not shown", len(warns)-maxWarnings)})
}

// skillDir is the folder of a SKILL.md path ("" at the source folder).
func skillDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// innermostSkill returns the deepest skill folder that holds the file at
// p.
func innermostSkill[V any](p string, skills map[string]V) (string, bool) {
	for d := skillDir(p); ; d = skillDir(d) {
		if _, ok := skills[d]; ok {
			return d, true
		}
		if d == "" {
			return "", false
		}
	}
}

// enclosingSkills returns every skill folder that holds the file at p,
// innermost first.
func enclosingSkills[V any](p string, skills map[string]V) []string {
	var out []string
	for d := skillDir(p); ; d = skillDir(d) {
		if _, ok := skills[d]; ok {
			out = append(out, d)
		}
		if d == "" {
			return out
		}
	}
}

// relTo is p relative to the skill folder dir.
func relTo(dir, p string) string {
	if dir == "" {
		return p
	}
	return strings.TrimPrefix(p, dir+"/")
}

// inFolder reports whether p is dir or inside it ("" holds everything).
func inFolder(dir, p string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

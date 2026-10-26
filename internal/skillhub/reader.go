package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// Reader limits.
const (
	searchDefaultLimit = 10
	searchMaxLimit     = 50
	listDefaultLimit   = 100
	listMaxLimit       = 500
	grepDefaultLimit   = 50
	grepMaxHits        = 200
	grepMaxFiles       = 5000
	grepMaxBytes       = 20 << 20
	grepTimeout        = 2 * time.Second
	grepMaxPattern     = 500
	grepMaxLine        = 500
	// bundleMaxBytes caps the files one bundle carries.
	bundleMaxBytes = 64 << 20
	// bundleMaxSkills caps the skills one bundle carries.
	bundleMaxSkills = 200
)

const uriScheme = "skill://"

// Skill URIs (SEP-2640) are skill://<slug>/<path>, where each segment of
// the path is percent-encoded (url.PathEscape: everything but RFC 3986
// unreserved characters, sub-delimiters, ":" and "@"), so a file whose
// name holds a space, non-ASCII text, "%", "#", "?" or "{}|^\"<>" still
// has a URI a client can send back. splitURI decodes it again. The source
// slug never needs encoding (^[a-z0-9]+(-[a-z0-9]+)*$).

// SkillURI is the URI of a skill's SKILL.md.
func SkillURI(slug, skillPath string) string {
	if skillPath == "" {
		return uriScheme + slug + "/" + skillFile
	}
	return uriScheme + slug + "/" + escapePath(skillPath) + "/" + skillFile
}

// FileURI is the URI of a file or folder of a source (path relative to
// the source folder; a folder has no trailing slash).
func FileURI(slug, p string) string {
	if p == "" {
		return uriScheme + slug
	}
	return uriScheme + slug + "/" + escapePath(p)
}

// splitURI splits skill://<slug>/<rest> and decodes rest. It refuses an
// invalid percent-escape, a segment that decodes to one holding "/" or
// NUL, a "." or ".." segment and an empty one between two others.
func splitURI(uri string) (slug, rest string, ok bool) {
	if !strings.HasPrefix(uri, uriScheme) {
		return "", "", false
	}
	s := strings.TrimPrefix(uri, uriScheme)
	slug, raw, _ := strings.Cut(s, "/")
	if slug == "" || strings.ContainsAny(slug, "%?#") {
		return "", "", false
	}
	raw = strings.Trim(raw, "/")
	if raw == "" {
		return slug, "", true
	}
	segs := strings.Split(raw, "/")
	for i, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil || dec == "" || dec == "." || dec == ".." || strings.ContainsAny(dec, "/\x00") {
			return "", "", false
		}
		segs[i] = dec
	}
	return slug, strings.Join(segs, "/"), true
}

// A skill at the source folder itself has the URI skill://<slug>/SKILL.md,
// whose last skill-path segment is the slug, not the skill's name, unless
// the two are equal. SEP-2640 requires that segment to be the name, so
// such a skill is served under its name instead:
// skill://<slug>/<name>/SKILL.md, its files as skill://<slug>/<name>/<path>.
// The old form still resolves. When the source already has a file or
// folder at <name> (the two URIs would collide), the skill keeps
// skill://<slug>/SKILL.md.

// rootAlias is the folder name the source folder's own skill is served
// under, or "" when it needs none (see above).
func (si *srcIndex) rootAlias() string {
	e := si.byPath[""]
	if e == nil || e.name == "" || e.name == si.src.Slug {
		return ""
	}
	if si.byPath[e.name] != nil || si.byFile[e.name] != nil {
		return ""
	}
	prefix := e.name + "/"
	i := sort.Search(len(si.files), func(i int) bool { return si.files[i].path >= prefix })
	if i < len(si.files) && strings.HasPrefix(si.files[i].path, prefix) {
		return ""
	}
	return e.name
}

// skillURI is the URI of the SKILL.md of the skill at skillPath.
func (si *srcIndex) skillURI(skillPath string) string {
	if skillPath == "" {
		if a := si.rootAlias(); a != "" {
			return uriScheme + si.src.Slug + "/" + url.PathEscape(a) + "/" + skillFile
		}
	}
	return SkillURI(si.src.Slug, skillPath)
}

// fileURI is the URI of the file or folder p as part of the skill at
// skillPath: under the root alias for the source folder's own skill.
func (si *srcIndex) fileURI(skillPath, p string) string {
	if skillPath == "" {
		if a := si.rootAlias(); a != "" {
			if p == "" {
				return uriScheme + si.src.Slug + "/" + url.PathEscape(a)
			}
			return uriScheme + si.src.Slug + "/" + url.PathEscape(a) + "/" + escapePath(p)
		}
	}
	return FileURI(si.src.Slug, p)
}

// unalias maps a decoded path of a URI into the source folder: a path
// under the root alias names the source folder's own skill's files.
func (si *srcIndex) unalias(p string) (string, bool) {
	a := si.rootAlias()
	if a == "" {
		return p, false
	}
	if p == a {
		return "", true
	}
	if rest, ok := strings.CutPrefix(p, a+"/"); ok {
		return rest, true
	}
	return p, false
}

// Sources lists the sources the viewer sees that serve a version.
func (s *Service) Sources(_ context.Context, v Viewer) ([]SourceInfo, error) {
	var out []SourceInfo
	for _, si := range s.idx.snapshot() {
		if !si.visibleTo(v) {
			continue
		}
		out = append(out, SourceInfo{
			Slug: si.src.Slug, Repo: si.repoLabel(), Branch: si.src.Branch, Path: si.src.Path,
			Commit: si.sha, SyncedAt: si.syncedAt, SkillCount: len(si.skills), HTMLURL: si.htmlURL(""),
		})
	}
	return out, nil
}

// htmlURL links to a folder of the source at its commit.
func (si *srcIndex) htmlURL(rel string) string {
	if si.src.IsBuiltin() {
		return "" // compiled into toolyard: no repository to link
	}
	u := "https://github.com/" + si.src.Owner + "/" + si.src.Repo + "/tree/" + si.sha
	if p := repoPath(si.src.Path, rel); p != "" {
		u += "/" + escapePath(p)
	}
	return u
}

// summary is a skill as a search result or listing entry.
func (si *srcIndex) summary(e *skillEntry) SkillSummary {
	return SkillSummary{
		URI: si.skillURI(e.path), Name: e.name, Description: e.description,
		Source: si.src.Slug, Path: e.path, Repo: si.repoLabel(), Commit: si.sha, Runs: e.runs,
	}
}

// visibleSources returns the sources the viewer sees, optionally one slug.
func (s *Service) visibleSources(v Viewer, slug string) []*srcIndex {
	var out []*srcIndex
	for _, si := range s.idx.snapshot() {
		if (slug == "" || si.src.Slug == slug) && si.visibleTo(v) {
			out = append(out, si)
		}
	}
	return out
}

// Search ranks the viewer's skills by name, description and path:
// keyword (BM25) or an RE2 regex.
func (s *Service) Search(ctx context.Context, v Viewer, q SearchQuery) ([]SkillSummary, error) {
	mode := toolsearch.ModeKeyword
	switch strings.ToLower(strings.TrimSpace(q.Mode)) {
	case "", "keyword":
	case "regex":
		mode = toolsearch.ModeRegex
	default:
		return nil, fmt.Errorf("%w: search mode %q: use keyword or regex", ErrInvalid, q.Mode)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = searchDefaultLimit
	case limit > searchMaxLimit:
		limit = searchMaxLimit
	}
	type ref struct {
		si *srcIndex
		e  *skillEntry
	}
	refs := map[string]ref{}
	var tools []toolsearch.Tool
	for _, si := range s.visibleSources(v, q.Source) {
		for _, e := range si.skills {
			if !showRuns(v, q.IncludeHost, e.runs) {
				continue
			}
			// The name field is slug/path (a skill at the source folder
			// itself: slug/name); the code name is the skill's name, for
			// the exact-name boost and regex matches on the name.
			name := si.src.Slug + "/" + e.path
			if e.path == "" {
				name = si.src.Slug + "/" + e.name
				if si.byPath[e.name] != nil {
					name = si.src.Slug + "/"
				}
			}
			refs[name] = ref{si, e}
			tools = append(tools, toolsearch.Tool{Name: name, Server: si.src.Slug, Description: e.description, CodeName: e.name})
		}
	}
	caller := v.AgentID
	if caller == "" {
		caller = v.UserID
	}
	res, err := s.search.Search(ctx, tools, toolsearch.Query{Text: q.Text, Mode: mode, Limit: limit, Caller: caller})
	if err != nil {
		var qe *toolsearch.QueryError
		if errors.As(err, &qe) {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, qe.Message)
		}
		return nil, err
	}
	out := make([]SkillSummary, 0, len(res.Hits))
	for _, h := range res.Hits {
		r, ok := refs[h.Tool.Name]
		if !ok {
			continue
		}
		sum := r.si.summary(r.e)
		sum.Score = h.Score
		out = append(out, sum)
	}
	return out, nil
}

// resolve finds a visible skill by URI, "slug/skill-path" or a bare name
// that is unique among the viewer's skills. Where a skill runs does not
// hide it from a direct reference.
func (s *Service) resolve(v Viewer, ref string) (*srcIndex, *skillEntry, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil, fmt.Errorf("%w: a skill reference is required", ErrInvalid)
	}
	var slug, p string
	switch {
	case strings.HasPrefix(ref, uriScheme):
		var ok bool
		if slug, p, ok = splitURI(ref); !ok {
			return nil, nil, fmt.Errorf("%w: %q is not a skill URI", ErrInvalid, ref)
		}
	case strings.Contains(ref, "/"):
		slug, p, _ = strings.Cut(strings.Trim(ref, "/"), "/")
	default:
		return s.resolveName(v, ref)
	}
	if p == skillFile {
		p = ""
	}
	p = strings.TrimSuffix(p, "/"+skillFile)
	si := s.idx.slug(slug)
	if si == nil || !si.visibleTo(v) {
		return nil, nil, fmt.Errorf("%w: skill %s", ErrNotFound, ref)
	}
	if si.byPath[p] == nil {
		p, _ = si.unalias(p)
	}
	e := si.byPath[p]
	if e == nil {
		return nil, nil, fmt.Errorf("%w: skill %s", ErrNotFound, ref)
	}
	return si, e, nil
}

// resolveName finds the one visible skill with a name.
func (s *Service) resolveName(v Viewer, name string) (*srcIndex, *skillEntry, error) {
	var hits []string
	var hitSI *srcIndex
	var hitE *skillEntry
	for _, si := range s.visibleSources(v, "") {
		for _, e := range si.skills {
			if e.name == name {
				hits = append(hits, si.skillURI(e.path))
				hitSI, hitE = si, e
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil, fmt.Errorf("%w: skill %q", ErrNotFound, name)
	case 1:
		return hitSI, hitE, nil
	}
	if len(hits) > 10 {
		hits = append(hits[:10], "...")
	}
	return nil, nil, fmt.Errorf("%w: %d skills are named %q; use a URI: %s", ErrConflict, len(hits), name, strings.Join(hits, ", "))
}

// fileRefs is the complete file list of a skill (nested skills' files
// included), with paths relative to the skill folder.
func (si *srcIndex) fileRefs(e *skillEntry) []FileRef {
	files := si.skillFiles[e.path]
	out := make([]FileRef, 0, len(files))
	for _, f := range files {
		out = append(out, FileRef{URI: si.fileURI(e.path, f.path), Path: relTo(e.path, f.path), Digest: f.sha, Size: f.size, Type: f.media})
	}
	return out
}

// omitted is the version's warnings about files inside a skill's folder.
func (si *srcIndex) omitted(e *skillEntry) []Warning {
	return si.skillOmitted[e.path]
}

// skillOf builds the Skill (frontmatter and file list; body when raw is
// given).
func (si *srcIndex) skillOf(e *skillEntry, raw []byte, includeFiles bool) *Skill {
	sk := &Skill{SkillSummary: si.summary(e), Frontmatter: map[string]any{}}
	// Numbers stay json.Number, so a big integer or a precise decimal in
	// the frontmatter is passed on exactly as written (SEP-2640: verbatim).
	dec := json.NewDecoder(bytes.NewReader(e.frontmatter))
	dec.UseNumber()
	_ = dec.Decode(&sk.Frontmatter)
	if sk.Frontmatter == nil {
		sk.Frontmatter = map[string]any{}
	}
	if raw != nil {
		sk.Raw = string(raw)
		if _, body, err := splitFrontmatter(sk.Raw); err == nil {
			sk.Body = body
		}
	}
	if includeFiles {
		sk.Files = si.fileRefs(e)
		sk.Omitted = si.omitted(e)
	}
	return sk
}

// Get returns a skill: its SKILL.md body and, with includeFiles, its
// complete file list with digests. It counts a read.
func (s *Service) Get(ctx context.Context, v Viewer, ref string, includeFiles bool) (*Skill, error) {
	si, e, err := s.resolve(v, ref)
	if err != nil {
		return nil, err
	}
	raw, err := s.loadBlob(ctx, e.skillMD)
	if err != nil {
		return nil, err
	}
	s.countRead(si.src.ID, e.path)
	return si.skillOf(e, raw, includeFiles), nil
}

// fileOf finds a visible file by URI. aliased reports a URI under the
// root alias (rootAlias).
func (s *Service) fileOf(v Viewer, uri string) (si *srcIndex, f *fileEntry, aliased bool, err error) {
	slug, p, ok := splitURI(uri)
	if !ok {
		return nil, nil, false, fmt.Errorf("%w: %q is not a skill URI", ErrInvalid, uri)
	}
	si = s.idx.slug(slug)
	if si == nil || !si.visibleTo(v) {
		return nil, nil, false, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	if f = si.byFile[p]; f == nil {
		if p, aliased = si.unalias(p); aliased {
			f = si.byFile[p]
		}
	}
	if f == nil {
		return nil, nil, false, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	return si, f, aliased, nil
}

// Read returns a file's text, from line offset (0-based), limit lines (0:
// to the end). It counts a read of the file's skill.
func (s *Service) Read(ctx context.Context, v Viewer, uri string, offset, limit int) (*File, error) {
	if offset < 0 || limit < 0 {
		return nil, fmt.Errorf("%w: offset and limit must not be negative", ErrInvalid)
	}
	si, f, aliased, err := s.fileOf(v, uri)
	if err != nil {
		return nil, err
	}
	data, err := s.loadBlob(ctx, f.sha)
	if err != nil {
		return nil, err
	}
	s.countRead(si.src.ID, f.skill)
	content, total, truncated := sliceLines(string(data), offset, limit)
	// The URI as asked: a nested skill's file read through the enclosing
	// root skill's alias keeps that form.
	fileURI, skill := si.fileURI(f.skill, f.path), f.skill
	if aliased {
		fileURI, skill = si.fileURI("", f.path), ""
	}
	return &File{
		FileRef: FileRef{URI: fileURI, Path: relTo(skill, f.path), Digest: f.sha, Size: f.size, Type: f.media},
		Skill:   si.skillURI(skill), Repo: si.repoLabel(), Commit: si.sha,
		Content: content, Offset: offset, TotalLines: total, Truncated: truncated,
	}, nil
}

// sliceLines returns lines [offset, offset+limit) of text (limit 0: to
// the end), the number of lines, and whether lines follow the slice.
func sliceLines(text string, offset, limit int) (string, int, bool) {
	if text == "" {
		return "", 0, false
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset >= total {
		return "", total, false
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], ""), total, end < total
}

// ListDir lists the files and folders directly inside a folder of a
// source (resources/directory/read). Folder URIs have no trailing slash
// (SEP-2640). A folder inside a skill lists everything in it: naming the
// skill's folder is a direct reference, like Get. The source folder and
// the folders above skills leave out what only host-only skills hold
// unless the viewer is on the host, as listings do.
func (s *Service) ListDir(_ context.Context, v Viewer, dirURI string) ([]DirEntry, error) {
	slug, dir, ok := splitURI(dirURI)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a skill URI", ErrInvalid, dirURI)
	}
	si := s.idx.slug(slug)
	if si == nil || !si.visibleTo(v) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dirURI)
	}
	if si.byFile[dir] != nil {
		return nil, fmt.Errorf("%w: %s is a file, not a folder", ErrNotFound, dirURI)
	}
	var aliased bool
	if !si.hasFolder(dir) {
		dir, aliased = si.unalias(dir)
	}
	inSkill := si.inSkill(dir)
	seen := map[string]bool{}
	var out []DirEntry
	for _, f := range si.files {
		if dir != "" && !strings.HasPrefix(f.path, dir+"/") {
			continue
		}
		if !inSkill && !si.fileShown(v, false, f) {
			continue
		}
		rest := relTo(dir, f.path)
		name, _, isDir := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		child := repoPath(dir, name)
		uri := FileURI(slug, child)
		if aliased {
			uri = si.fileURI("", child)
		}
		if isDir {
			out = append(out, DirEntry{URI: uri, Name: name, IsDir: true})
		} else {
			out = append(out, DirEntry{URI: uri, Name: name, Size: f.size})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s is not a folder of the source", ErrNotFound, dirURI)
	}
	return out, nil
}

// hasFolder reports whether dir is the source folder or a folder that
// holds files of the version.
func (si *srcIndex) hasFolder(dir string) bool {
	if dir == "" {
		return true
	}
	prefix := dir + "/"
	i := sort.Search(len(si.files), func(i int) bool { return si.files[i].path >= prefix })
	return i < len(si.files) && strings.HasPrefix(si.files[i].path, prefix)
}

// inSkill reports whether the folder dir is a skill's folder or inside one.
func (si *srcIndex) inSkill(dir string) bool {
	for d := dir; ; d = skillDir(d) {
		if si.byPath[d] != nil {
			return true
		}
		if d == "" {
			return false
		}
	}
}

// List pages through the viewer's skills, by source slug and path, each
// with its frontmatter and complete file list (SEP-2640 skills/list).
func (s *Service) List(_ context.Context, v Viewer, q ListQuery) ([]Skill, string, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = listDefaultLimit
	case limit > listMaxLimit:
		limit = listMaxLimit
	}
	afterSlug, afterPath, err := decodeCursor(q.Cursor)
	if err != nil {
		return nil, "", err
	}
	var out []Skill
	for _, si := range s.visibleSources(v, q.Source) {
		if q.Cursor != "" && si.src.Slug < afterSlug {
			continue
		}
		for _, e := range si.skills {
			if q.Cursor != "" && si.src.Slug == afterSlug && e.path <= afterPath {
				continue
			}
			if !showRuns(v, q.IncludeHost, e.runs) {
				continue
			}
			if len(out) == limit {
				last := out[len(out)-1]
				return out, encodeCursor(last.Source, last.Path), nil
			}
			out = append(out, *si.skillOf(e, nil, true))
		}
	}
	return out, "", nil
}

func encodeCursor(slug, p string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(slug + "\x00" + p))
}

func decodeCursor(c string) (slug, p string, err error) {
	if c == "" {
		return "", "", nil
	}
	b, derr := base64.RawURLEncoding.DecodeString(c)
	if derr != nil {
		return "", "", fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	slug, p, ok := strings.Cut(string(b), "\x00")
	if !ok {
		return "", "", fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	return slug, p, nil
}

// Grep searches the file contents of the viewer's skills with an RE2
// pattern. It stops at grepMaxHits hits (or q.Limit), grepMaxFiles files,
// grepMaxBytes bytes or grepTimeout, and returns what it found by then.
func (s *Service) Grep(ctx context.Context, v Viewer, q GrepQuery) ([]GrepHit, error) {
	re, err := compileGrep(q)
	if err != nil {
		return nil, err
	}
	var glob *regexp.Regexp
	anchored := false
	if g := strings.Trim(strings.TrimSpace(q.PathGlob), "/"); g != "" {
		if glob, err = compileGlob(g); err != nil {
			return nil, err
		}
		anchored = strings.Contains(g, "/")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = grepDefaultLimit
	}
	limit = min(limit, grepMaxHits)
	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()
	g := &grepRun{s: s, re: re, limit: limit}
	for _, si := range s.visibleSources(v, q.Source) {
		var batch []*fileEntry
		for _, f := range si.files {
			if glob != nil && !globMatch(glob, anchored, f.path) {
				continue
			}
			if !si.fileShown(v, q.IncludeHost, f) {
				continue
			}
			batch = append(batch, f)
			if len(batch) == blobBatch {
				if g.scan(ctx, si, batch) {
					return g.hits, nil
				}
				batch = batch[:0]
			}
		}
		if len(batch) > 0 && g.scan(ctx, si, batch) {
			return g.hits, nil
		}
	}
	return g.hits, nil
}

// compileGrep checks and compiles a grep pattern.
func compileGrep(q GrepQuery) (*regexp.Regexp, error) {
	p := q.Pattern
	if strings.TrimSpace(p) == "" {
		return nil, fmt.Errorf("%w: a pattern is required", ErrInvalid)
	}
	if len(p) > grepMaxPattern {
		return nil, fmt.Errorf("%w: the pattern is longer than %d characters", ErrInvalid, grepMaxPattern)
	}
	if q.IgnoreCase {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("%w: pattern: %v", ErrInvalid, err)
	}
	return re, nil
}

// fileShown reports whether a grep looks at a file: some skill holding it
// is shown to the viewer where it runs.
func (si *srcIndex) fileShown(v Viewer, includeHost bool, f *fileEntry) bool {
	for d := f.skill; ; d = skillDir(d) {
		if e := si.byPath[d]; e != nil && showRuns(v, includeHost, e.runs) {
			return true
		}
		if d == "" {
			return false
		}
	}
}

// grepRun is one grep's progress against its caps.
type grepRun struct {
	s     *Service
	re    *regexp.Regexp
	limit int
	hits  []GrepHit
	files int
	bytes int64
}

// scan greps a batch of files of one source; true means a cap was hit.
func (g *grepRun) scan(ctx context.Context, si *srcIndex, files []*fileEntry) bool {
	if ctx.Err() != nil || g.files >= grepMaxFiles || g.bytes >= grepMaxBytes {
		return true
	}
	// Load only what the caps leave room for.
	room, budget := grepMaxFiles-g.files, grepMaxBytes-g.bytes
	n := 0
	for n < len(files) && n < room && files[n].size <= budget {
		budget -= files[n].size
		n++
	}
	capped := n < len(files)
	files = files[:n]
	if len(files) == 0 {
		return true
	}
	shas := make([]string, len(files))
	for i, f := range files {
		shas[i] = f.sha
	}
	contents, err := g.s.loadBlobs(ctx, shas)
	if err != nil {
		return true
	}
	for _, f := range files {
		if ctx.Err() != nil || g.files >= grepMaxFiles || g.bytes >= grepMaxBytes {
			return true
		}
		data := contents[f.sha]
		g.files++
		g.bytes += int64(len(data))
		lines := strings.Split(string(data), "\n")
		if n := len(lines); lines[n-1] == "" {
			lines = lines[:n-1]
		}
		for i, text := range lines {
			line := i + 1
			text = strings.TrimSuffix(text, "\r")
			if !g.re.MatchString(text) {
				continue
			}
			g.hits = append(g.hits, GrepHit{
				URI: si.fileURI(f.skill, f.path), Skill: si.skillURI(f.skill), Line: line,
				Text: capLine(text), Repo: si.repoLabel(), Commit: si.sha,
			})
			if len(g.hits) >= g.limit {
				return true
			}
		}
	}
	return capped
}

// capLine cuts a line to grepMaxLine bytes at a character boundary.
func capLine(s string) string {
	if len(s) <= grepMaxLine {
		return s
	}
	cut := grepMaxLine
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// Bundle writes a .tar.gz of the skills in the Agent Skills layout: one
// folder per skill, named after it, with all its files.
func (s *Service) Bundle(ctx context.Context, v Viewer, refs []string, w io.Writer) error {
	if len(refs) == 0 {
		return fmt.Errorf("%w: name at least one skill", ErrInvalid)
	}
	if len(refs) > bundleMaxSkills {
		return fmt.Errorf("%w: at most %d skills in one bundle", ErrInvalid, bundleMaxSkills)
	}
	type chosen struct {
		si *srcIndex
		e  *skillEntry
	}
	var picks []chosen
	names := map[string]string{}
	var total int64
	for _, ref := range refs {
		si, e, err := s.resolve(v, ref)
		if err != nil {
			return err
		}
		uri := si.skillURI(e.path)
		if prev, dup := names[e.name]; dup {
			if prev == uri {
				continue
			}
			return fmt.Errorf("%w: two chosen skills are named %q (%s, %s)", ErrConflict, e.name, prev, uri)
		}
		names[e.name] = uri
		for _, f := range si.skillFiles[e.path] {
			total += f.size
		}
		picks = append(picks, chosen{si, e})
	}
	if total > bundleMaxBytes {
		return fmt.Errorf("%w: the bundle would hold %d bytes; the limit is %d", ErrLimit, total, bundleMaxBytes)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, p := range picks {
		if err := s.bundleSkill(ctx, tw, p.si, p.e); err != nil {
			return err
		}
		s.countRead(p.si.src.ID, p.e.path)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("skillhub: write bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("skillhub: write bundle: %w", err)
	}
	return nil
}

// bundleSkill writes one skill's folder into a bundle.
func (s *Service) bundleSkill(ctx context.Context, tw *tar.Writer, si *srcIndex, e *skillEntry) error {
	files := si.skillFiles[e.path] // by path
	for start := 0; start < len(files); start += blobBatch {
		batch := files[start:min(start+blobBatch, len(files))]
		shas := make([]string, len(batch))
		for i, f := range batch {
			shas[i] = f.sha
		}
		contents, err := s.loadBlobs(ctx, shas)
		if err != nil {
			return err
		}
		for _, f := range batch {
			data, ok := contents[f.sha]
			if !ok {
				return fmt.Errorf("skillhub: the stored copy of %s is missing", FileURI(si.src.Slug, f.path))
			}
			hdr := &tar.Header{
				Name: e.name + "/" + relTo(e.path, f.path), Mode: 0o644, Size: int64(len(data)),
				ModTime: si.syncedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("skillhub: write bundle: %w", err)
			}
			if _, err := tw.Write(data); err != nil {
				return fmt.Errorf("skillhub: write bundle: %w", err)
			}
		}
	}
	return nil
}

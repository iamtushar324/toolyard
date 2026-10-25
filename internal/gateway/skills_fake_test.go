package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// fakeSkillReader is an in-memory skillhub.Reader over two sources:
// "team" (acme/playbooks, every caller sees it) and "private" (acme/secret,
// admins only). Host skills (runs: host) are left out of listings, search
// and grep unless the viewer is on the host or asks for them; Get and Read
// by an explicit reference only check the source. It records the viewers
// and queries it was given.
type fakeSkillReader struct {
	mu       sync.Mutex
	viewers  []skillhub.Viewer
	searches []skillhub.SearchQuery
	greps    []skillhub.GrepQuery
	lists    []skillhub.ListQuery
	bundles  [][]string
	subs     []func(string)
	// fail, when set for a method name, is returned by that method.
	fail map[string]error
}

type fakeSkillSource struct {
	slug, owner, repo, sha string
	public                 bool
	skills                 []fakeSkill
}

type fakeSkill struct {
	path, name, description string
	runs                    skillhub.Runs
	files                   map[string]string // relative path -> content
}

const (
	fakeTeamSHA    = "0123456789abcdef0123456789abcdef01234567"
	fakePrivateSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

var fakeSkillSources = []fakeSkillSource{
	{slug: "team", owner: "acme", repo: "playbooks", sha: fakeTeamSHA, public: true, skills: []fakeSkill{
		{path: "ops/deploy", name: "deploy", description: "Deploy a service safely", runs: skillhub.RunsAnywhere, files: map[string]string{
			"SKILL.md":                 "---\nname: deploy\ndescription: Deploy a service safely\n---\n# Deploy\nRun references/checklist.md first.\n",
			"references/checklist.md":  "1. build\n2. ship\n3. verify\n",
			"scripts/rollback.sh":      "#!/bin/sh\necho rollback\n",
			"references/deep/notes.md": "deep notes\n",
		}},
		{path: "host-ops", name: "host-ops", description: "Restart services on the T3 host", runs: skillhub.RunsHost, files: map[string]string{
			"SKILL.md": "---\nname: host-ops\ndescription: Restart services on the T3 host\n---\nUse tools/restart.sh.\n",
		}},
		{path: "review", name: "review", description: "Review a pull request", runs: skillhub.RunsAnywhere, files: map[string]string{
			"SKILL.md": "---\nname: review\ndescription: Review a pull request\n---\nRead the diff.\n",
		}},
	}},
	{slug: "private", owner: "acme", repo: "secret", sha: fakePrivateSHA, public: false, skills: []fakeSkill{
		{path: "secret-sauce", name: "secret-sauce", description: "The secret deploy recipe", runs: skillhub.RunsAnywhere, files: map[string]string{
			"SKILL.md": "---\nname: secret-sauce\ndescription: The secret deploy recipe\n---\nShh.\n",
		}},
		{path: "review", name: "review", description: "Review a pull request the secret way", runs: skillhub.RunsAnywhere, files: map[string]string{
			"SKILL.md": "---\nname: review\ndescription: Review a pull request the secret way\n---\nRead it twice.\n",
		}},
	}},
}

func (f *fakeSkillReader) record(v skillhub.Viewer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viewers = append(f.viewers, v)
}

func (f *fakeSkillReader) lastViewer(t *testing.T) skillhub.Viewer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.viewers) == 0 {
		t.Fatal("the reader was never called")
	}
	return f.viewers[len(f.viewers)-1]
}

func (f *fakeSkillReader) failure(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail[method]
}

func (f *fakeSkillReader) setFail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]error{}
	}
	f.fail[method] = err
}

func (src fakeSkillSource) visible(v skillhub.Viewer) bool { return src.public || v.Admin }

func listed(sk fakeSkill, v skillhub.Viewer, includeHost bool) bool {
	return sk.runs != skillhub.RunsHost || v.OnHost || includeHost
}

func fakeSkillURI(src fakeSkillSource, sk fakeSkill, file string) string {
	return "skill://" + src.slug + "/" + sk.path + "/" + file
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (src fakeSkillSource) summary(sk fakeSkill) skillhub.SkillSummary {
	return skillhub.SkillSummary{
		URI: fakeSkillURI(src, sk, "SKILL.md"), Name: sk.name, Description: sk.description, Source: src.slug, Path: sk.path,
		Repo: skillhub.RepoLabel(src.owner, src.repo, src.sha), Commit: src.sha, Runs: sk.runs,
	}
}

func (src fakeSkillSource) skill(sk fakeSkill, includeFiles bool) *skillhub.Skill {
	s := &skillhub.Skill{
		SkillSummary: src.summary(sk),
		Frontmatter:  map[string]any{"name": sk.name, "description": sk.description},
		Body:         strings.SplitN(sk.files["SKILL.md"], "---\n", 3)[2],
		Raw:          sk.files["SKILL.md"],
	}
	if includeFiles {
		names := make([]string, 0, len(sk.files))
		for n := range sk.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s.Files = append(s.Files, skillhub.FileRef{URI: fakeSkillURI(src, sk, n), Path: n, Digest: digestOf(sk.files[n]), Size: int64(len(sk.files[n]))})
		}
	}
	return s
}

func (f *fakeSkillReader) Sources(_ context.Context, v skillhub.Viewer) ([]skillhub.SourceInfo, error) {
	f.record(v)
	if err := f.failure("Sources"); err != nil {
		return nil, err
	}
	var out []skillhub.SourceInfo
	for _, src := range fakeSkillSources {
		if src.visible(v) {
			out = append(out, skillhub.SourceInfo{
				Slug: src.slug, Repo: skillhub.RepoLabel(src.owner, src.repo, src.sha), Branch: "main", Commit: src.sha,
				SyncedAt: time.Unix(1_700_000_000, 0).UTC(), SkillCount: len(src.skills), HTMLURL: "https://github.com/" + src.owner + "/" + src.repo,
			})
		}
	}
	return out, nil
}

func (f *fakeSkillReader) Search(_ context.Context, v skillhub.Viewer, q skillhub.SearchQuery) ([]skillhub.SkillSummary, error) {
	f.record(v)
	f.mu.Lock()
	f.searches = append(f.searches, q)
	f.mu.Unlock()
	if err := f.failure("Search"); err != nil {
		return nil, err
	}
	match := func(s string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(q.Text)) }
	if q.Mode == "regex" {
		re, err := regexp.Compile(q.Text)
		if err != nil {
			return nil, fmt.Errorf("%w: pattern: %v", skillhub.ErrInvalid, err)
		}
		match = re.MatchString
	}
	var out []skillhub.SkillSummary
	for _, src := range fakeSkillSources {
		if !src.visible(v) || (q.Source != "" && q.Source != src.slug) {
			continue
		}
		for _, sk := range src.skills {
			if listed(sk, v, q.IncludeHost) && (match(sk.name) || match(sk.description) || match(sk.path)) {
				out = append(out, src.summary(sk))
			}
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// find resolves a skill reference among the sources v sees.
func (f *fakeSkillReader) find(v skillhub.Viewer, ref string) (fakeSkillSource, fakeSkill, error) {
	type hit struct {
		src fakeSkillSource
		sk  fakeSkill
	}
	var byName []hit
	for _, src := range fakeSkillSources {
		if !src.visible(v) {
			continue
		}
		for _, sk := range src.skills {
			if ref == fakeSkillURI(src, sk, "SKILL.md") || ref == src.slug+"/"+sk.path {
				return src, sk, nil
			}
			if ref == sk.name {
				byName = append(byName, hit{src, sk})
			}
		}
	}
	switch len(byName) {
	case 0:
		return fakeSkillSource{}, fakeSkill{}, skillhub.ErrNotFound
	case 1:
		return byName[0].src, byName[0].sk, nil
	}
	return fakeSkillSource{}, fakeSkill{}, skillhub.ErrConflict
}

func (f *fakeSkillReader) Get(_ context.Context, v skillhub.Viewer, ref string, includeFiles bool) (*skillhub.Skill, error) {
	f.record(v)
	if err := f.failure("Get"); err != nil {
		return nil, err
	}
	src, sk, err := f.find(v, ref)
	if err != nil {
		return nil, err
	}
	return src.skill(sk, includeFiles), nil
}

// fileAt resolves a file URI among the sources v sees.
func fileAt(v skillhub.Viewer, uri string) (fakeSkillSource, fakeSkill, string, bool) {
	for _, src := range fakeSkillSources {
		if !src.visible(v) {
			continue
		}
		for _, sk := range src.skills {
			for name := range sk.files {
				if uri == fakeSkillURI(src, sk, name) {
					return src, sk, name, true
				}
			}
		}
	}
	return fakeSkillSource{}, fakeSkill{}, "", false
}

func (f *fakeSkillReader) Read(_ context.Context, v skillhub.Viewer, uri string, offset, limit int) (*skillhub.File, error) {
	f.record(v)
	if err := f.failure("Read"); err != nil {
		return nil, err
	}
	src, sk, name, ok := fileAt(v, uri)
	if !ok {
		return nil, skillhub.ErrNotFound
	}
	content := sk.files[name]
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out := &skillhub.File{
		FileRef: skillhub.FileRef{URI: uri, Path: name, Digest: digestOf(content), Size: int64(len(content))},
		Skill:   fakeSkillURI(src, sk, "SKILL.md"), Repo: skillhub.RepoLabel(src.owner, src.repo, src.sha), Commit: src.sha,
		Offset: offset, TotalLines: len(lines),
	}
	if offset > len(lines) {
		offset = len(lines)
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
		out.Truncated = true
	}
	out.Content = strings.Join(lines[offset:end], "")
	return out, nil
}

func (f *fakeSkillReader) ListDir(_ context.Context, v skillhub.Viewer, dirURI string) ([]skillhub.DirEntry, error) {
	f.record(v)
	if err := f.failure("ListDir"); err != nil {
		return nil, err
	}
	for _, src := range fakeSkillSources {
		if !src.visible(v) {
			continue
		}
		for _, sk := range src.skills {
			root := "skill://" + src.slug + "/" + sk.path
			if dirURI != root && !strings.HasPrefix(dirURI, root+"/") {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(dirURI, root), "/")
			children := map[string]skillhub.DirEntry{}
			for name, content := range sk.files {
				if rel == name {
					return nil, fmt.Errorf("%w: %s is a file", skillhub.ErrNotFound, dirURI)
				}
				rest, ok := strings.CutPrefix(name, rel)
				if rel != "" {
					rest, ok = strings.CutPrefix(name, rel+"/")
				}
				if !ok {
					continue
				}
				child, _, isDir := strings.Cut(rest, "/")
				e := skillhub.DirEntry{URI: dirURI + "/" + child, Name: child, IsDir: isDir}
				if !isDir {
					e.Size = int64(len(content))
				}
				children[child] = e
			}
			if len(children) == 0 {
				return nil, skillhub.ErrNotFound
			}
			out := make([]skillhub.DirEntry, 0, len(children))
			for _, e := range children {
				out = append(out, e)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		}
	}
	return nil, skillhub.ErrNotFound
}

func (f *fakeSkillReader) Grep(_ context.Context, v skillhub.Viewer, q skillhub.GrepQuery) ([]skillhub.GrepHit, error) {
	f.record(v)
	f.mu.Lock()
	f.greps = append(f.greps, q)
	f.mu.Unlock()
	if err := f.failure("Grep"); err != nil {
		return nil, err
	}
	pattern := q.Pattern
	if q.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: pattern: %v", skillhub.ErrInvalid, err)
	}
	var out []skillhub.GrepHit
	for _, src := range fakeSkillSources {
		if !src.visible(v) || (q.Source != "" && q.Source != src.slug) {
			continue
		}
		for _, sk := range src.skills {
			if !listed(sk, v, q.IncludeHost) {
				continue
			}
			names := make([]string, 0, len(sk.files))
			for n := range sk.files {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if q.PathGlob != "" {
					if ok, _ := path.Match(q.PathGlob, n); !ok {
						continue
					}
				}
				for i, line := range strings.Split(sk.files[n], "\n") {
					if re.MatchString(line) {
						out = append(out, skillhub.GrepHit{URI: fakeSkillURI(src, sk, n), Skill: fakeSkillURI(src, sk, "SKILL.md"), Line: i + 1, Text: line,
							Repo: skillhub.RepoLabel(src.owner, src.repo, src.sha), Commit: src.sha})
					}
				}
			}
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeSkillReader) List(_ context.Context, v skillhub.Viewer, q skillhub.ListQuery) ([]skillhub.Skill, string, error) {
	f.record(v)
	f.mu.Lock()
	f.lists = append(f.lists, q)
	f.mu.Unlock()
	if err := f.failure("List"); err != nil {
		return nil, "", err
	}
	var all []skillhub.Skill
	for _, src := range fakeSkillSources {
		if !src.visible(v) || (q.Source != "" && q.Source != src.slug) {
			continue
		}
		for _, sk := range src.skills {
			if listed(sk, v, q.IncludeHost) {
				all = append(all, *src.skill(sk, true))
			}
		}
	}
	start := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(q.Cursor)
		if err != nil || n < 0 || n > len(all) {
			return nil, "", fmt.Errorf("%w: cursor", skillhub.ErrInvalid)
		}
		start = n
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	end := min(start+limit, len(all))
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return all[start:end], next, nil
}

func (f *fakeSkillReader) Bundle(_ context.Context, v skillhub.Viewer, refs []string, w io.Writer) error {
	f.record(v)
	f.mu.Lock()
	f.bundles = append(f.bundles, append([]string(nil), refs...))
	f.mu.Unlock()
	if err := f.failure("Bundle"); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, _, err := f.find(v, ref); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "bundle:"+strings.Join(refs, ","))
	return err
}

func (f *fakeSkillReader) Subscribe(fn func(slug string)) (cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
	return func() {}
}

// changed tells every subscriber that slug's active version changed.
func (f *fakeSkillReader) changed(slug string) {
	f.mu.Lock()
	subs := append([]func(string){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		fn(slug)
	}
}

// fakeProposals records skill proposals.
type fakeProposals struct {
	mu      sync.Mutex
	sources []skillhub.SourceInput
	reasons []string
	changes []skillhub.ChangeInput
	viewers []skillhub.Viewer
	err     error
}

func (p *fakeProposals) ProposeSource(_ context.Context, v skillhub.Viewer, in skillhub.SourceInput, reason string) (*skillhub.Proposal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	p.sources, p.reasons, p.viewers = append(p.sources, in), append(p.reasons, reason), append(p.viewers, v)
	return &skillhub.Proposal{ID: "skp_source", Kind: "source", Status: "pending", InboxID: "rq_source", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}, nil
}

func (p *fakeProposals) ProposeChange(_ context.Context, v skillhub.Viewer, in skillhub.ChangeInput) (*skillhub.Proposal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	p.changes, p.viewers = append(p.changes, in), append(p.viewers, v)
	return &skillhub.Proposal{ID: "skp_change", Kind: "change", Status: "pending", InboxID: "rq_change", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}, nil
}

func (p *fakeProposals) GetProposal(context.Context, skillhub.Viewer, string) (*skillhub.Proposal, error) {
	return nil, skillhub.ErrNotFound
}

// fakeHosts is SkillHub.OnHost over a fixed set of host agents, counting
// lookups; "ag_lookup_fails" fails.
type fakeHosts struct {
	mu      sync.Mutex
	on      map[string]bool
	lookups map[string]int
}

func (h *fakeHosts) OnHost(_ context.Context, agentID string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lookups == nil {
		h.lookups = map[string]int{}
	}
	h.lookups[agentID]++
	if agentID == "ag_lookup_fails" {
		return false, errors.New("database is down")
	}
	return h.on[agentID], nil
}

func (h *fakeHosts) count(agentID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lookups[agentID]
}

// skillOwners maps agent ids to the users who own them (OwnerResolver).
type skillOwners map[string]string

func (o skillOwners) OwnerUser(_ context.Context, callerID string) (string, error) {
	return o[callerID], nil
}

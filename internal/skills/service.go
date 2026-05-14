// Package skills wires the toolyard "skills" workspace — a centralised
// directory of Claude Code skills under <dataDir>/skills/ — to the rest
// of the gateway. Sibling to internal/notes, deliberately the same shape
// where possible:
//
//   - Agent calls skills.publish(slug?, skill_md, [agents_openai_yaml],
//     [scripts]) — we validate the SKILL.md frontmatter, write the skill
//     tree via the filesystem MCP upstream registered as "skills", and
//     index the SKILL.md body into MemPalace's diary so semantic search
//     ("find me a skill that does X") works out of the box.
//
//   - Agent calls skills.install(slug, [mode], [target]) — symlink (the
//     default) or copy <skillsDir>/<slug> into ~/.claude/skills/<slug>
//     so Claude Code picks it up.
//
//   - Background scanner walks $SKILLS_DIR every N seconds and ingests
//     any SKILL.md whose mtime advanced since the last sync, so skills
//     dropped in by hand (git pull, scp, the user editing in their
//     IDE) still end up indexed.
//
// State lives in `skills_sync` (one row per slug). The filesystem MCP
// upstream handles raw read/list/grep — we never expose CRUD by hand.
package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Dispatcher is the narrow gateway behaviour we depend on. Identical
// shape to internal/notes.Dispatcher — copied here rather than re-using
// because we don't want a hard import dependency between the two
// sibling packages.
type Dispatcher interface {
	HasTool(name string) bool
	CallInternal(ctx context.Context, viaTool, target string, args map[string]any) (*mcp.CallToolResult, error)
}

// Service owns the skills-dir lifecycle and in-process sync state.
type Service struct {
	db        *store.DB
	gw        Dispatcher
	skillsDir string
	interval  time.Duration
	enabled   bool

	// Tool names we dispatch to. Strings rather than imports to keep this
	// package free of internal/notes and internal/mempalace dependencies.
	writeTool string // "skills.write_file"
	diaryTool string // "mempalace.mempalace_diary_write"

	// claudeSkillsDir is where skills.install symlinks/copies into when
	// the caller doesn't override `target`. Defaults to "" (caller must
	// supply target), so a misconfigured gateway can't accidentally
	// litter someone's home directory.
	claudeSkillsDir string

	mu      sync.Mutex
	syncing bool
}

// Defaults used when callers don't override.
const (
	DefaultDiaryTool = "mempalace.mempalace_diary_write"
	DefaultWriteTool = "skills.write_file"
	DefaultScanEvery = 30 * time.Second
	maxIngestBytes   = 64 << 10
	skillMDName      = "SKILL.md"
	agentsYAMLPath   = "agents/openai.yaml"
)

// Options controls Service construction.
type Options struct {
	DB        *store.DB
	Gateway   Dispatcher
	SkillsDir string
	Interval  time.Duration
	WriteTool string
	DiaryTool string
	// ClaudeSkillsDir, when set, is the default `target` for
	// skills.install. Typically `~/.claude/skills`. When empty, callers
	// must pass `target` explicitly.
	ClaudeSkillsDir string
}

// New builds a Service. Returns nil if SkillsDir is empty (caller can
// treat nil as "feature disabled"; every public method is nil-safe).
func New(opts Options) *Service {
	if strings.TrimSpace(opts.SkillsDir) == "" {
		return nil
	}
	interval := opts.Interval
	if interval == 0 {
		interval = DefaultScanEvery
	}
	write := opts.WriteTool
	if write == "" {
		write = DefaultWriteTool
	}
	diary := opts.DiaryTool
	if diary == "" {
		diary = DefaultDiaryTool
	}
	dir := filepath.Clean(opts.SkillsDir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		dir = resolved
	}
	return &Service{
		db:              opts.DB,
		gw:              opts.Gateway,
		skillsDir:       dir,
		interval:        interval,
		enabled:         true,
		writeTool:       write,
		diaryTool:       diary,
		claudeSkillsDir: opts.ClaudeSkillsDir,
	}
}

// SkillsDir returns the configured directory. Nil-safe.
func (s *Service) SkillsDir() string {
	if s == nil {
		return ""
	}
	return s.skillsDir
}

// Enabled reports whether the service is wired up. Nil-safe.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// Errors surfaced to callers.
var (
	ErrDisabled      = errors.New("skills service is disabled (-skills=off or no skills-dir)")
	ErrSlugRequired  = errors.New("slug is required (and could not be derived from frontmatter.name)")
	ErrSkillNotFound = errors.New("skill not found")
	ErrInstallTarget = errors.New("install target is empty and no default ClaudeSkillsDir configured")
	ErrInstallExists = errors.New("install target already exists")
	ErrUnknownMode   = errors.New("unknown install mode (want link|copy)")
)

// PublishResult is what skills.publish returns to the caller.
type PublishResult struct {
	Slug       string   `json:"slug"`
	Path       string   `json:"path"`  // relative to skills dir, e.g. "<slug>/SKILL.md"
	Files      []string `json:"files"` // every relative path we wrote
	Bytes      int      `json:"bytes"` // total bytes written
	Indexed    bool     `json:"indexed"`
	PalaceID   string   `json:"palace_id,omitempty"`
	IndexError string   `json:"index_error,omitempty"`
}

// Publish validates a skill, writes its files via the filesystem MCP,
// and indexes the SKILL.md body into MemPalace. Indexing is best-effort:
// a write success with an index failure still returns OK so the file
// persists, and the next scanner tick retries the ingest.
func (s *Service) Publish(ctx context.Context, agentID, slug, skillMD, agentsYAML string, scripts map[string]string, topic string) (*PublishResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	fm, body, err := ParseSkillMD(skillMD)
	if err != nil {
		return nil, fmt.Errorf("parse SKILL.md: %w", err)
	}
	skill := Skill{
		Slug:        strings.TrimSpace(slug),
		Frontmatter: fm,
		Body:        body,
		AgentsYAML:  agentsYAML,
		Scripts:     scripts,
	}
	if skill.Slug == "" {
		skill.Slug = skill.CanonicalSlug()
	}
	if skill.Slug == "" {
		return nil, ErrSlugRequired
	}
	if err := skill.Validate(); err != nil {
		return nil, err
	}

	skillDir := filepath.Join(s.skillsDir, skill.Slug)
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir skill dir: %w", err)
	}

	out := &PublishResult{Slug: skill.Slug, Path: filepath.Join(skill.Slug, skillMDName)}

	// Re-serialise the SKILL.md so what lands on disk is canonical
	// (frontmatter normalised, body untouched). Going through serialise
	// rather than writing the raw input means an LLM can pass back a
	// slightly malformed YAML and we self-correct.
	canonical := SerializeSkillMD(skill.Frontmatter, skill.Body)
	if err := s.writeFile(ctx, filepath.Join(skillDir, skillMDName), canonical); err != nil {
		return nil, err
	}
	out.Files = append(out.Files, filepath.Join(skill.Slug, skillMDName))
	out.Bytes += len(canonical)

	if strings.TrimSpace(skill.AgentsYAML) != "" {
		if err := os.MkdirAll(filepath.Join(skillDir, "agents"), 0o750); err != nil {
			return nil, fmt.Errorf("mkdir agents: %w", err)
		}
		path := filepath.Join(skillDir, agentsYAMLPath)
		if err := s.writeFile(ctx, path, skill.AgentsYAML); err != nil {
			return nil, err
		}
		out.Files = append(out.Files, filepath.Join(skill.Slug, agentsYAMLPath))
		out.Bytes += len(skill.AgentsYAML)
	}

	for rel, payload := range skill.Scripts {
		abs := filepath.Join(skillDir, "scripts", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return nil, fmt.Errorf("mkdir script parent: %w", err)
		}
		if err := s.writeFile(ctx, abs, payload); err != nil {
			return nil, err
		}
		out.Files = append(out.Files, filepath.Join(skill.Slug, "scripts", rel))
		out.Bytes += len(payload)
	}

	// Index the SKILL.md (frontmatter + body) into mempalace.
	palaceID, ierr := s.ingest(ctx, agentID, skill.Slug, canonical, topic)
	if ierr != nil {
		out.IndexError = ierr.Error()
	} else {
		out.Indexed = true
		out.PalaceID = palaceID
	}

	if info, err := os.Stat(filepath.Join(skillDir, skillMDName)); err == nil {
		s.recordSync(ctx, skill.Slug, info.ModTime().UnixMilli(), palaceID)
	}
	return out, nil
}

// writeFile dispatches through skills.write_file when the upstream is
// registered, otherwise falls back to direct disk writes. Same pattern
// as internal/notes.Publish so the upstream's audit trail captures all
// writes once the filesystem MCP is up.
func (s *Service) writeFile(ctx context.Context, abs, content string) error {
	if s.gw != nil && s.gw.HasTool(s.writeTool) {
		args := map[string]any{"path": abs, "content": content}
		res, err := s.gw.CallInternal(ctx, "skills.publish", s.writeTool, args)
		if err != nil {
			return fmt.Errorf("write via %s: %w", s.writeTool, err)
		}
		if res != nil && res.IsError {
			return fmt.Errorf("write via %s: %s", s.writeTool, summariseResult(res))
		}
		return nil
	}
	return os.WriteFile(abs, []byte(content), 0o640)
}

// InstallResult is what skills.install returns.
type InstallResult struct {
	Slug   string `json:"slug"`
	Mode   string `json:"mode"`   // "link" or "copy"
	Target string `json:"target"` // resolved absolute path on disk
	Source string `json:"source"` // resolved absolute path under skills dir
}

// Install makes <skillsDir>/<slug> visible to a Claude Code installation
// by symlinking (default) or copying it into target/<slug>. Refuses to
// clobber an existing target unless it's already a symlink pointing at
// the same source — that case we treat as idempotent.
func (s *Service) Install(ctx context.Context, agentID, slug, mode, target string) (*InstallResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrSlugRequired
	}
	src := filepath.Join(s.skillsDir, slug)
	if _, err := os.Stat(filepath.Join(src, skillMDName)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, slug)
		}
		return nil, err
	}

	if mode == "" {
		mode = "link"
	}
	switch mode {
	case "link", "copy":
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, mode)
	}

	root := strings.TrimSpace(target)
	if root == "" {
		root = s.claudeSkillsDir
	}
	if root == "" {
		return nil, ErrInstallTarget
	}
	root = filepath.Clean(expandUser(root))
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir target: %w", err)
	}
	dst := filepath.Join(root, slug)

	// Idempotent if `dst` is already a symlink to `src`.
	if info, lerr := os.Lstat(dst); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if existing, err := os.Readlink(dst); err == nil && filepath.Clean(existing) == src {
				return &InstallResult{Slug: slug, Mode: "link", Target: dst, Source: src}, nil
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrInstallExists, dst)
	}

	switch mode {
	case "link":
		if err := os.Symlink(src, dst); err != nil {
			return nil, fmt.Errorf("symlink: %w", err)
		}
	case "copy":
		if err := copyTree(src, dst); err != nil {
			return nil, fmt.Errorf("copy: %w", err)
		}
	}
	return &InstallResult{Slug: slug, Mode: mode, Target: dst, Source: src}, nil
}

// Sync walks $SKILLS_DIR and ingests any SKILL.md whose mtime advanced
// (or whose previous ingest failed). Returns the number of skills
// ingested in this pass.
func (s *Service) Sync(ctx context.Context) (int, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		return 0, nil
	}
	s.syncing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.syncing = false
		s.mu.Unlock()
	}()

	if s.gw == nil || !s.gw.HasTool(s.diaryTool) {
		return 0, nil
	}

	known, err := s.loadSyncIndex(ctx)
	if err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(s.skillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	var ingested int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		slug := e.Name()
		skillMDPath := filepath.Join(s.skillsDir, slug, skillMDName)
		info, err := os.Stat(skillMDPath)
		if err != nil {
			continue // not a skill dir
		}
		mtime := info.ModTime().UnixMilli()
		prev, ok := known[slug]
		if ok && prev.mtime == mtime && prev.palaceID != "" {
			continue
		}
		body, err := os.ReadFile(skillMDPath)
		if err != nil {
			continue
		}
		palaceID, ierr := s.ingest(ctx, "skills-sync", slug, string(body), "")
		if ierr != nil {
			log.Printf("skills-sync: %s: %v", slug, ierr)
			continue
		}
		s.recordSync(ctx, slug, mtime, palaceID)
		ingested++
	}
	return ingested, nil
}

// StartScanner runs Sync on a ticker until ctx is cancelled. Negative
// interval disables the loop entirely.
func (s *Service) StartScanner(ctx context.Context) {
	if !s.Enabled() || s.interval < 0 {
		return
	}
	first := time.NewTimer(5 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	if n, err := s.Sync(ctx); err != nil {
		log.Printf("skills-sync: initial: %v", err)
	} else if n > 0 {
		log.Printf("skills-sync: initial: ingested %d skill(s)", n)
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sync(ctx); err != nil {
				log.Printf("skills-sync: %v", err)
			} else if n > 0 {
				log.Printf("skills-sync: ingested %d skill(s)", n)
			}
		}
	}
}

// ingest forwards a SKILL.md body to mempalace_diary_write via the
// gateway's internal-call path. Entry is the SKILL.md text prefixed with
// the slug so the search returns a self-identifying hit.
func (s *Service) ingest(ctx context.Context, agentID, slug, content, topic string) (string, error) {
	if len(content) > maxIngestBytes {
		content = content[:maxIngestBytes] + "\n\n[…truncated by toolyard skills-sync; read the file for the full body]"
	}
	if topic == "" {
		topic = "skills"
	}
	args := map[string]any{
		"entry":      "[skill:" + slug + "]\n\n" + content,
		"topic":      topic,
		"wing":       "skills",
		"agent_name": agentID,
	}
	res, err := s.gw.CallInternal(ctx, "skills.publish", s.diaryTool, args)
	if err != nil {
		return "", err
	}
	if res != nil && res.IsError {
		return "", fmt.Errorf("%s", summariseResult(res))
	}
	return extractPalaceID(res), nil
}

func (s *Service) recordSync(ctx context.Context, slug string, mtime int64, palaceID string) {
	if s.db == nil {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO skills_sync(slug, mtime, palace_id, synced_at)
         VALUES(?, ?, ?, ?)
         ON CONFLICT(slug) DO UPDATE SET
            mtime = excluded.mtime,
            palace_id = COALESCE(NULLIF(excluded.palace_id, ''), skills_sync.palace_id),
            synced_at = excluded.synced_at`,
		slug, mtime, nullString(palaceID), now)
}

type syncRow struct {
	mtime    int64
	palaceID string
}

func (s *Service) loadSyncIndex(ctx context.Context) (map[string]syncRow, error) {
	out := map[string]syncRow{}
	if s.db == nil {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT slug, mtime, COALESCE(palace_id,'') FROM skills_sync`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var slug, palace string
		var mtime int64
		if err := rows.Scan(&slug, &mtime, &palace); err != nil {
			return nil, err
		}
		out[slug] = syncRow{mtime: mtime, palaceID: palace}
	}
	return out, rows.Err()
}

// Status is the lightweight dashboard snapshot.
type Status struct {
	Enabled    bool   `json:"enabled"`
	SkillsDir  string `json:"skills_dir"`
	Interval   string `json:"interval"`
	Tracked    int    `json:"tracked"`
	LastSynced int64  `json:"last_synced_ms,omitempty"`
}

// Snapshot returns the dashboard snapshot. Nil-safe.
func (s *Service) Snapshot(ctx context.Context) Status {
	if s == nil {
		return Status{Enabled: false}
	}
	st := Status{
		Enabled:   s.enabled,
		SkillsDir: s.skillsDir,
		Interval:  s.interval.String(),
	}
	if s.db != nil {
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(synced_at),0) FROM skills_sync`).
			Scan(&st.Tracked, &st.LastSynced)
	}
	return st
}

// ---- read-only surface (browse / fetch / bulk download) -------------------

// SkillSummary is the lightweight row returned by List / ListTags. Tailored
// for the agent's "browse the catalog before pulling raw text" flow:
// frontmatter only, no body, no scripts.
type SkillSummary struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// SkillContents is the full read response. SkillMD is always populated;
// AgentsOpenAIYAML + Scripts are populated only when the caller asks for
// companion files (saves an agent's context budget on the common case).
type SkillContents struct {
	Slug             string            `json:"slug"`
	SkillMD          string            `json:"skill_md"`
	AgentsOpenAIYAML string            `json:"agents_openai_yaml,omitempty"`
	Scripts          map[string]string `json:"scripts,omitempty"`
	Files            []string          `json:"files,omitempty"` // all relative paths present under the skill dir
	Bytes            int               `json:"bytes"`
}

// BundleResult is what skills.bundle returns: a base64-encoded gzipped tar
// of the requested skills' directories. The agent decodes + extracts on
// their side. We cap to a sane size so a confused caller can't try to pull
// the whole store as one blob.
type BundleResult struct {
	Slugs    []string `json:"slugs"`
	TarGzB64 string   `json:"tar_gz_b64"`
	Bytes    int      `json:"bytes"` // size of the *encoded* base64 payload
	RawBytes int      `json:"raw_bytes"`
}

const (
	maxBundleRawBytes = 16 << 20 // 16 MiB cap on the uncompressed tar payload
	maxScriptsRead    = 64 << 10 // 64 KiB per script file we'll inline in a Get response
)

// List walks the skills dir, parses every SKILL.md, and returns one
// SkillSummary per skill. Optional `tagFilter` keeps only skills carrying
// at least one of the listed tags (post-KebabSlug-normalised). Returns
// summaries sorted by slug for stable agent-side display.
func (s *Service) List(ctx context.Context, tagFilter []string) ([]SkillSummary, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	wantTags := map[string]bool{}
	for _, t := range tagFilter {
		if canon := KebabSlug(t); canon != "" {
			wantTags[canon] = true
		}
	}
	rows, err := s.walkSkills()
	if err != nil {
		return nil, err
	}
	out := make([]SkillSummary, 0, len(rows))
	for _, r := range rows {
		if len(wantTags) > 0 {
			match := false
			for _, t := range r.fm.Tags {
				if wantTags[t] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, SkillSummary{
			Slug:        r.slug,
			Name:        r.fm.Name,
			Description: r.fm.Description,
			Tags:        r.fm.Tags,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// ListTags returns the set of every tag currently in use, mapped to the
// count of skills carrying it. This is the agent's entry point: "what
// categories of skill exist here?".
func (s *Service) ListTags(ctx context.Context) (map[string]int, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	rows, err := s.walkSkills()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, r := range rows {
		for _, t := range r.fm.Tags {
			out[t]++
		}
	}
	return out, nil
}

// Get returns one skill's contents. When includeFiles is false the
// response is just SKILL.md (cheap browse). When true, agents/openai.yaml
// + scripts/* are inlined (subject to maxScriptsRead per script).
func (s *Service) Get(ctx context.Context, slug string, includeFiles bool) (*SkillContents, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrSlugRequired
	}
	dir := filepath.Join(s.skillsDir, slug)
	skillMDBytes, err := os.ReadFile(filepath.Join(dir, skillMDName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, slug)
		}
		return nil, err
	}
	out := &SkillContents{
		Slug:    slug,
		SkillMD: string(skillMDBytes),
		Bytes:   len(skillMDBytes),
	}
	if !includeFiles {
		out.Files = []string{skillMDName}
		return out, nil
	}
	// Walk the directory once; pick up agents/openai.yaml and scripts/**
	// in a single pass so the response order is deterministic.
	out.Scripts = map[string]string{}
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out.Files = append(out.Files, rel)
		switch {
		case rel == skillMDName:
			// already loaded
		case rel == agentsYAMLPath:
			body, _ := os.ReadFile(p)
			out.AgentsOpenAIYAML = string(body)
			out.Bytes += len(body)
		case strings.HasPrefix(rel, "scripts/"):
			info, _ := d.Info()
			if info != nil && info.Size() > maxScriptsRead {
				out.Scripts[strings.TrimPrefix(rel, "scripts/")] = fmt.Sprintf("[…toolyard skipped %d bytes; fetch via skills.read_file]", info.Size())
				return nil
			}
			body, _ := os.ReadFile(p)
			out.Scripts[strings.TrimPrefix(rel, "scripts/")] = string(body)
			out.Bytes += len(body)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(out.Files)
	return out, nil
}

// Bundle tars + gzips the requested slugs and returns the result
// base64-encoded. The agent decodes with `base64 -d | tar -xzf -`. Empty
// slugs list bundles every skill in the store.
func (s *Service) Bundle(ctx context.Context, slugs []string) (*BundleResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if len(slugs) == 0 {
		rows, err := s.walkSkills()
		if err != nil {
			return nil, err
		}
		slugs = make([]string, 0, len(rows))
		for _, r := range rows {
			slugs = append(slugs, r.slug)
		}
	}
	// Dedupe while preserving order — base64 size compounds quickly.
	seen := map[string]bool{}
	uniq := slugs[:0]
	for _, sl := range slugs {
		sl = strings.TrimSpace(sl)
		if sl == "" || seen[sl] {
			continue
		}
		seen[sl] = true
		uniq = append(uniq, sl)
	}
	slugs = uniq

	var rawBuf bytes.Buffer
	gz := gzip.NewWriter(&rawBuf)
	tw := tar.NewWriter(gz)
	var rawBytes int

	for _, slug := range slugs {
		dir := filepath.Join(s.skillsDir, slug)
		if _, err := os.Stat(filepath.Join(dir, skillMDName)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, slug)
			}
			return nil, err
		}
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			rel, rerr := filepath.Rel(s.skillsDir, p)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			hdr, herr := tar.FileInfoHeader(info, "")
			if herr != nil {
				return herr
			}
			hdr.Name = rel
			if d.IsDir() {
				hdr.Name = rel + "/"
				return tw.WriteHeader(hdr)
			}
			if !info.Mode().IsRegular() {
				return nil // skip symlinks etc; bundle is for portability
			}
			body, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rawBytes += len(body)
			if rawBytes > maxBundleRawBytes {
				return fmt.Errorf("bundle exceeds %d bytes — narrow the slug list", maxBundleRawBytes)
			}
			hdr.Size = int64(len(body))
			if werr := tw.WriteHeader(hdr); werr != nil {
				return werr
			}
			_, werr = tw.Write(body)
			return werr
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(rawBuf.Bytes())
	return &BundleResult{
		Slugs:    slugs,
		TarGzB64: encoded,
		Bytes:    len(encoded),
		RawBytes: rawBytes,
	}, nil
}

// skillRow is the in-memory shape walkSkills returns: one parsed SKILL.md
// per skill dir. Used by List, ListTags, and Bundle's no-args path.
type skillRow struct {
	slug string
	fm   Frontmatter
}

// walkSkills enumerates skill directories under s.skillsDir, parsing each
// SKILL.md's frontmatter. Skills with malformed frontmatter are skipped
// silently — we'd rather degrade gracefully than 500 a list request
// because one file is broken. Errors are still logged for diagnosis.
func (s *Service) walkSkills() ([]skillRow, error) {
	entries, err := os.ReadDir(s.skillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]skillRow, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		slug := e.Name()
		body, rerr := os.ReadFile(filepath.Join(s.skillsDir, slug, skillMDName))
		if rerr != nil {
			continue // not a skill dir
		}
		fm, _, perr := ParseSkillMD(string(body))
		if perr != nil {
			log.Printf("skills: skipping %s: %v", slug, perr)
			continue
		}
		out = append(out, skillRow{slug: slug, fm: fm})
	}
	return out, nil
}

// ---- helpers ---------------------------------------------------------------

// expandUser turns a leading ~/ into the current user's home dir. Trims
// nothing else. Best-effort: a HOME-less environment returns the input
// unchanged, which Install will then surface as a not-found error.
func expandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// copyTree recursively copies src → dst. Symlinks are followed
// (Install copy mode is "give me a full snapshot, no surprises").
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o640
		}
		return os.WriteFile(target, body, mode)
	})
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func summariseResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			s := strings.TrimSpace(t.Text)
			if len(s) > 400 {
				return s[:400] + "…"
			}
			return s
		}
	}
	return ""
}

func extractPalaceID(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		t, ok := mcp.AsTextContent(c)
		if !ok {
			continue
		}
		var blob map[string]any
		if err := json.Unmarshal([]byte(t.Text), &blob); err == nil {
			if v, ok := blob["entry_id"].(string); ok {
				return v
			}
		}
	}
	return ""
}

var _ = sql.ErrNoRows

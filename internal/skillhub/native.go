package skillhub

// Native skills: a native source (kind "native", a "collection") holds
// skills that agents and people write through toolyard's API (the
// skills.create and skills.update tools, REST) instead of a GitHub
// repository. Every write builds a new version of the collection through
// the same pipeline as a GitHub commit (the file checks, discover,
// insertVersion, activate, retain), so search, get, read, grep, listings,
// bundles, the sharing rule, the MCP Skills extension, tools.search,
// skills.recommend and the cluster messages serve native skills unchanged,
// and a write is visible to every reader as soon as it returns: the
// writing node reloads its index before it answers, the other nodes on the
// cluster message.
//
// The version's "commit" is the SHA-256 of the collection's file tree
// (every path and the digest of its content), like the built-in source's.
// Each write of a skill is also one revision of that skill
// (skill_revisions): a number per skill (1, 2, ...), who wrote it, their
// note, and the skill's files (skill_revision_files), kept after the
// collection's version retention drops the version, so every revision
// stays readable.
//
// Who may do what (the rule):
//
//   - Read: the sharing rule of every source (index.go). A collection's
//     audience, block list and per-skill sharing decide who sees which
//     skill; a write never shows a skill to anyone the rule hides it from.
//   - Write: a viewer who sees the collection and is an admin, its creator,
//     or one of its editors (Source.Editors). An agent writes as its owner
//     (Viewer.UserID). Others may still propose a change with
//     skills.propose_change: the collection's creator decides.
//   - Approval: a write by an agent that is sensitive (sensitiveReasons: a
//     collection in review mode, a script, a change to allowed-tools or to
//     where the skill runs, a protected skill) does not go live: it becomes
//     a change proposal, one Inbox question for the collection's creator,
//     who approves it in person; the approval applies exactly what the
//     question showed, on the revision it was based on. People write
//     directly: they are accountable for their own writes, and the
//     dashboard and REST are theirs.
//
// Each source keeps its fixed repository settings, like the built-in one:
// an owner with a dot (no GitHub account has one), branch "native", a next
// check in year 9999, so no poller of this build or an older one (after a
// rollback) ever reads GitHub for it, and an older build serves its last
// version read-only.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
)

// KindNative is a source of skills written through toolyard's API.
const KindNative = "native"

const (
	// nativeOwner and nativeBranch are a native source's fixed repository
	// settings: an owner with a dot names no GitHub account (see
	// builtinOwner), so nothing can ever be fetched for it.
	nativeOwner  = "toolyard.native"
	nativeBranch = "native"

	// MaxNativeNote caps a write's note (characters).
	MaxNativeNote = 500
	// MaxNativeDepth caps the folders of a skill path in a collection.
	MaxNativeDepth = 4
	// MaxRevisionsList caps one listing of a skill's revisions.
	MaxRevisionsList = 100
	// defaultRevisionsList is a listing's default length.
	defaultRevisionsList = 20
	// nativeWriteTimeout bounds one write (the lock, the build and the
	// activation).
	nativeWriteTimeout = time.Minute
)

// Write outcomes.
const (
	// WriteLive: the write is live for every reader.
	WriteLive = "live"
	// WritePending: the write waits for the collection owner's approval
	// (Proposal); nothing changed yet.
	WritePending = "pending_approval"
)

// Revision operations.
const (
	revisionCreate = "create"
	revisionUpdate = "update"
)

// IsNative reports whether src is a native source.
func (src *Source) IsNative() bool { return src != nil && src.Kind == KindNative }

// SkillWrite is one write of a native skill: a new skill (Collection, with
// Path or the name in its SKILL.md) or an update of one (Skill).
type SkillWrite struct {
	// Collection is the native source's slug (create).
	Collection string `json:"collection,omitempty"`
	// Path is the new skill's folder in the collection (create); its last
	// segment is the skill's name. Default: the name in SKILL.md.
	Path string `json:"path,omitempty"`
	// Skill is the skill to update: its URI, slug/path, or a bare name
	// unique among the viewer's skills.
	Skill string `json:"skill,omitempty"`
	// Files maps a path in the skill folder to its new full content; ""
	// deletes the file (update only). A create needs SKILL.md.
	Files map[string]string `json:"files"`
	// BaseRevision, when set, is the revision the writer read: the update
	// is refused with ErrConflict when the skill moved on since.
	BaseRevision int `json:"base_revision,omitempty"`
	// Note says what changed and why (the revision's note, the Inbox
	// question's summary).
	Note string `json:"note,omitempty"`
}

// WriteResult is what a write did.
type WriteResult struct {
	Status   string `json:"status"` // WriteLive or WritePending
	URI      string `json:"uri"`    // the skill's URI (load it with skills.get)
	Revision int    `json:"revision,omitempty"`
	// Commit is the collection's new version (live writes).
	Commit string `json:"commit,omitempty"`
	// Files are the paths in the skill folder the write added, changed or
	// deleted, with the operation.
	Files []WrittenFile `json:"files,omitempty"`
	// Proposal is the change waiting for approval (pending writes); poll
	// it with skills.proposal.
	Proposal *Proposal `json:"proposal,omitempty"`
	// ApprovalReasons say why the write needs approval.
	ApprovalReasons []string `json:"approval_reasons,omitempty"`
	// Warnings are what the build noticed about the skill (served anyway).
	Warnings []Warning `json:"warnings,omitempty"`
}

// WrittenFile is one file of a write.
type WrittenFile struct {
	Path string `json:"path"`
	Op   string `json:"op"` // add | change | delete
}

// Revision is one write of a native skill.
type Revision struct {
	Revision    int       `json:"revision"`
	Op          string    `json:"op"` // create | update
	URI         string    `json:"uri"`
	Name        string    `json:"name"`
	Author      string    `json:"author,omitempty"` // user ID
	Agent       string    `json:"agent,omitempty"`  // agent ID, when an agent wrote it
	Note        string    `json:"note,omitempty"`
	ProposalID  string    `json:"proposal_id,omitempty"` // the approved change proposal
	CreatedAt   time.Time `json:"created_at"`
	FileCount   int       `json:"file_count"`
	TotalBytes  int64     `json:"total_bytes"`
	SkillMD     string    `json:"skill_md_digest"`
	FilesDigest string    `json:"files_digest"`
	// Current marks the revision whose content is live now.
	Current bool `json:"current,omitempty"`
}

// Registry writes native skills and reads their history. *Service
// implements it. Every method shows only what the viewer may see; a
// skill the viewer cannot see is ErrNotFound.
type Registry interface {
	// WriteSkill creates or updates a native skill, or turns a sensitive
	// agent write into a change proposal (WriteResult.Status).
	WriteSkill(ctx context.Context, v Viewer, w SkillWrite) (*WriteResult, error)
	// SkillRevisions lists a native skill's revisions, newest first.
	SkillRevisions(ctx context.Context, v Viewer, ref string, limit int) ([]Revision, error)
	// GetRevision returns a native skill as one revision wrote it: its
	// SKILL.md and, with includeFiles, its file list (URIs carry
	// ?revision=N; read them with ReadRevision).
	GetRevision(ctx context.Context, v Viewer, ref string, revision int, includeFiles bool) (*Skill, error)
	// ReadRevision reads one file of a native skill as a revision wrote
	// it; offset and limit are lines, as Read.
	ReadRevision(ctx context.Context, v Viewer, uri string, revision, offset, limit int) (*File, error)
}

var _ Registry = (*Service)(nil)

// nativeState is the Service's native-registry state.
type nativeState struct {
	// approvals asks a collection's creator to approve a sensitive agent
	// write (SetNativeApprovals); nil refuses such writes.
	approvals *ChangeProposals
	// audit records every write (SetAudit); nil records nothing beyond
	// the gateway's own tool-call row.
	audit *audit.Logger
}

// SetNativeApprovals routes sensitive agent writes to change proposals.
// Call it at start, before the gateway serves.
func (s *Service) SetNativeApprovals(c *ChangeProposals) {
	s.nat.approvals = c
	if c != nil {
		c.native = s
	}
}

// SetAudit records native writes in the audit log. Call it at start.
func (s *Service) SetAudit(l *audit.Logger) { s.nat.audit = l }

// ---- collections -----------------------------------------------------------

// createNative stores a new native source (CreateSource with kind native):
// its slug, mode (auto: editors' writes go live; review: every agent write
// waits for the creator), audience, block list, editors and where its
// skills run. A collection holds no repository settings, credential or
// webhook.
func (s *Service) createNative(ctx context.Context, by string, in SourceInput) (*Source, error) {
	switch {
	case in.PendingApproval:
		return nil, fmt.Errorf("%w: only an admin can create a native skill collection", ErrForbidden)
	case strings.TrimSpace(in.URL) != "", strings.TrimSpace(in.Branch) != "", strings.TrimSpace(in.Path) != "",
		len(in.Exclude) > 0, strings.TrimSpace(in.PinnedSHA) != "", in.Auth != "", strings.TrimSpace(in.Token) != "",
		in.PollSeconds != 0:
		return nil, fmt.Errorf("%w: a native collection has no repository: give only slug, mode, visibility, members, blocked, editors, default_runs and limits", ErrInvalid)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return nil, fmt.Errorf("%w: slug is required: the collection's name in every skill URI (skill://<slug>/...)", ErrInvalid)
	}
	r := &sourceRow{Source: Source{
		ID: newID("src_"), Kind: KindNative, Owner: nativeOwner, Repo: slug, Branch: nativeBranch, Auth: AuthNone,
		Mode: in.Mode, PollSeconds: DefaultPollSeconds, Enabled: true, Visibility: in.Visibility, Members: in.Members,
		Blocked: in.Blocked, DefaultRuns: in.DefaultRuns, Editors: in.Editors, CreatedBy: by,
	}}
	if in.Enabled != nil && !*in.Enabled {
		return nil, fmt.Errorf("%w: a native collection cannot start paused; hide it instead", ErrInvalid)
	}
	if r.Mode == "" {
		r.Mode = ModeAuto
	}
	if in.Limits != nil {
		r.Limits = *in.Limits
	}
	if err := normalizeNative(r); err != nil {
		return nil, err
	}
	if err := s.insertWithSlug(ctx, r, slug); err != nil {
		return nil, err
	}
	s.changed(ctx, r.ID)
	return s.GetSource(ctx, r.ID)
}

// normalizeNative checks a native source's settings.
func normalizeNative(r *sourceRow) error {
	switch r.Mode {
	case ModeAuto, ModeReview:
	default:
		return fmt.Errorf("%w: a native collection's mode is auto (editors' changes go live) or review (every agent change waits for the owner's approval), not %q", ErrInvalid, r.Mode)
	}
	if err := normalizeSource(r); err != nil {
		return err
	}
	var err error
	r.Editors, err = cleanUserList(r.Editors, "editors")
	return err
}

// refuseNativeUpdate refuses an update of a native source's repository
// settings; editors are for native sources only.
func refuseNativeUpdate(r *sourceRow, up SourceUpdate) error {
	if !r.IsNative() {
		if up.Editors != nil && len(*up.Editors) > 0 {
			return fmt.Errorf("%w: editors are for native skill collections; a GitHub source's skills change on GitHub", ErrInvalid)
		}
		return nil
	}
	var fields []string
	add := func(changed bool, name string) {
		if changed {
			fields = append(fields, name)
		}
	}
	add(up.Branch != nil && strings.TrimSpace(*up.Branch) != r.Branch, "branch")
	add(up.Path != nil && strings.Trim(strings.TrimSpace(*up.Path), "/") != r.Path, "folder")
	add(up.Exclude != nil && len(*up.Exclude) > 0, "exclude globs")
	add(up.PinnedSHA != nil && strings.TrimSpace(*up.PinnedSHA) != "", "pinned commit")
	add(up.Auth != nil && *up.Auth != r.Auth, "read access")
	add((up.Token != nil && strings.TrimSpace(*up.Token) != "") || up.ClearToken, "token")
	add(up.PollSeconds != nil && *up.PollSeconds != r.PollSeconds, "check interval")
	add(up.Enabled != nil && !*up.Enabled, "paused state (hide it instead)")
	add(up.Mode != nil && *up.Mode != ModeAuto && *up.Mode != ModeReview, "mode (auto or review only)")
	if len(fields) == 0 {
		return nil
	}
	return fmt.Errorf("%w: a native collection has no repository: its %s cannot change", ErrConflict, strings.Join(fields, ", "))
}

// applyNativeUpdate applies the settings a native source has (after
// refuseNativeUpdate) and validates the result. Nothing is fetched, so no
// check becomes due.
func applyNativeUpdate(r *sourceRow, up SourceUpdate) error {
	if up.Mode != nil {
		r.Mode = *up.Mode
	}
	if up.Limits != nil {
		r.Limits = *up.Limits
	}
	if up.Visibility != nil {
		r.Visibility = *up.Visibility
	}
	if up.Members != nil {
		r.Members = *up.Members
	}
	if up.Blocked != nil {
		r.Blocked = *up.Blocked
	}
	if up.Hidden != nil {
		r.Hidden = *up.Hidden
	}
	if up.DefaultRuns != nil {
		r.DefaultRuns = *up.DefaultRuns
	}
	if up.Editors != nil {
		r.Editors = *up.Editors
	}
	return normalizeNative(r)
}

// errNativeNoSync answers what only a repository has.
func errNativeNoSync(what string) error {
	return fmt.Errorf("%w: a native collection has no repository: %s; its skills change through skills.create and skills.update", ErrConflict, what)
}

// refuseNative refuses an action that needs a repository.
func (s *Service) refuseNative(ctx context.Context, id, what string) error {
	if cur := s.idx.get(id); cur != nil {
		if cur.src.IsNative() {
			return errNativeNoSync(what)
		}
		return nil
	}
	r, err := s.getSourceRow(ctx, s.db, id)
	if err == nil && r.IsNative() {
		return errNativeNoSync(what)
	}
	return nil
}

// canEdit reports whether the viewer may write a collection's skills.
func canEdit(v Viewer, src *Source) bool {
	if v.UserID == "" {
		return false
	}
	if v.Admin || v.UserID == src.CreatedBy {
		return true
	}
	for _, id := range src.Editors {
		if id == v.UserID {
			return true
		}
	}
	return false
}

// ---- writes ----------------------------------------------------------------

// nativeApply is one write to apply under the collection's lock.
type nativeApply struct {
	sourceID     string
	skillPath    string
	create       bool
	files        []changeFile
	baseRevision int // 0: not checked
	author       string
	agent        string
	note         string
	proposalID   string // the approved proposal ("" for a direct write)
}

// nativeApplied is what an applied write made.
type nativeApplied struct {
	revision int
	commit   string
	warnings []Warning
}

// WriteSkill creates or updates a native skill. See the rule at the top of
// this file.
func (s *Service) WriteSkill(ctx context.Context, v Viewer, w SkillWrite) (*WriteResult, error) {
	if v.UserID == "" {
		return nil, fmt.Errorf("%w: writing a skill needs a signed-in person or an agent with an owner", ErrForbidden)
	}
	note := strings.TrimSpace(changeClean(w.Note, true, false))
	switch {
	case utf8.RuneCountInString(note) > MaxNativeNote:
		return nil, fmt.Errorf("%w: note is longer than %d characters", ErrInvalid, MaxNativeNote)
	case len(w.Files) == 0:
		return nil, fmt.Errorf("%w: files: give at least one file (path in the skill folder → its full content)", ErrInvalid)
	case len(w.Files) > MaxChangeFiles:
		return nil, fmt.Errorf("%w: files: at most %d files in one write", ErrLimit, MaxChangeFiles)
	case w.BaseRevision < 0:
		return nil, fmt.Errorf("%w: base_revision must be 1 or more", ErrInvalid)
	}
	create := strings.TrimSpace(w.Skill) == ""
	var (
		si        *srcIndex
		e         *skillEntry
		skillPath string
	)
	if create {
		if w.BaseRevision != 0 {
			return nil, fmt.Errorf("%w: base_revision is for updates; a new skill has none", ErrInvalid)
		}
		var err error
		if si, skillPath, err = s.createTarget(v, w); err != nil {
			return nil, err
		}
	} else {
		var err error
		if si, e, err = s.resolve(v, w.Skill); err != nil {
			return nil, err
		}
		if !si.src.IsNative() {
			return nil, fmt.Errorf("%w: %s comes from the repository %s; propose a change with skills.propose_change (its owner opens a pull request)",
				ErrInvalid, si.skillURI(e.path), RepoLabel(si.src.Owner, si.src.Repo, ""))
		}
		skillPath = e.path
		if w.BaseRevision > 0 && e.revision != w.BaseRevision {
			return nil, fmt.Errorf("%w: %s is at revision %d, not %d; read it again (skills.get) and write on top of it", ErrConflict, si.skillURI(e.path), e.revision, w.BaseRevision)
		}
	}
	src := si.src
	if !canEdit(v, &src) {
		return nil, fmt.Errorf("%w: you may read the collection %s but not write to it: ask its owner to add your owner as an editor, or propose the change with skills.propose_change",
			ErrForbidden, src.Slug)
	}
	files, err := s.nativeFiles(ctx, si, e, skillPath, w.Files)
	if err != nil {
		return nil, err
	}
	res := &WriteResult{URI: si.skillURI(skillPath)}
	for _, f := range files {
		res.Files = append(res.Files, WrittenFile{Path: f.Path, Op: f.Op})
	}
	if v.Delegated {
		reasons := sensitiveReasons(&src, e, files)
		if len(reasons) > 0 {
			if v.AgentID == "" {
				return nil, fmt.Errorf("%w: this change needs the collection owner's approval (%s), which only an agent's write can ask for; make it in the dashboard",
					ErrForbidden, strings.Join(reasons, "; "))
			}
			if s.nat.approvals == nil {
				return nil, fmt.Errorf("%w: this change needs the collection owner's approval (%s), and this server cannot ask for it", ErrForbidden, strings.Join(reasons, "; "))
			}
			name := path.Base(skillPath)
			if e != nil {
				name = e.name
			}
			base := 0
			if e != nil {
				base = e.revision
			}
			p, err := s.nat.approvals.proposeNative(ctx, v, nativeProposal{src: src, uri: res.URI, skillPath: skillPath, name: name,
				create: create, baseRevision: base, files: files, note: note, reasons: reasons})
			if err != nil {
				return nil, err
			}
			res.Status, res.Proposal, res.ApprovalReasons = WritePending, p, reasons
			return res, nil
		}
	}
	base := 0
	if !create {
		base = e.revision
		if base == 0 {
			base = -1 // no revision records the live content: check by content instead
		}
	}
	done, err := s.applyNative(ctx, nativeApply{sourceID: src.ID, skillPath: skillPath, create: create, files: files,
		baseRevision: base, author: v.UserID, agent: v.AgentID, note: note})
	if err != nil {
		return nil, err
	}
	res.Status, res.Revision, res.Commit, res.Warnings = WriteLive, done.revision, done.commit, done.warnings
	return res, nil
}

// skillPathRe is one folder of a native skill path.
var skillPathRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// createTarget finds the collection and the new skill's folder.
func (s *Service) createTarget(v Viewer, w SkillWrite) (*srcIndex, string, error) {
	slug := strings.TrimSpace(w.Collection)
	if slug == "" {
		return nil, "", fmt.Errorf("%w: collection is required for a new skill: the slug of a native collection (skills.sources lists those you see)", ErrInvalid)
	}
	si := s.idx.slug(slug)
	if si == nil || !si.visibleTo(v) {
		return nil, "", fmt.Errorf("%w: collection %s", ErrNotFound, slug)
	}
	if !si.src.IsNative() {
		return nil, "", fmt.Errorf("%w: %s is a GitHub source; new skills go in a native collection (or in the repository itself)", ErrInvalid, slug)
	}
	p := strings.Trim(strings.TrimSpace(w.Path), "/")
	if p == "" {
		md, ok := w.Files[skillFile]
		if !ok {
			return nil, "", fmt.Errorf("%w: files: a new skill needs SKILL.md", ErrInvalid)
		}
		if text, _, err := splitFrontmatter(md); err == nil {
			if fm, err := parseYAMLMap(text); err == nil {
				p, _ = fm["name"].(string)
				p = strings.TrimSpace(p)
			}
		}
		if p == "" {
			return nil, "", fmt.Errorf("%w: files: SKILL.md needs YAML frontmatter with a name and a description", ErrInvalid)
		}
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxNativeDepth {
		return nil, "", fmt.Errorf("%w: path %q: at most %d folders", ErrInvalid, p, MaxNativeDepth)
	}
	for _, seg := range segs {
		if len(seg) > maxNameLen || !skillPathRe.MatchString(seg) {
			return nil, "", fmt.Errorf("%w: path %q: each folder is 1-%d lowercase letters, digits and single hyphens", ErrInvalid, p, maxNameLen)
		}
	}
	if si.byPath[p] != nil {
		return nil, "", fmt.Errorf("%w: %s already exists; update it with skills.update", ErrConflict, si.skillURI(p))
	}
	if _, ok := si.byFile[p]; ok {
		return nil, "", fmt.Errorf("%w: path %q is a file of another skill", ErrConflict, p)
	}
	return si, p, nil
}

// nativeFiles checks a write's files against the skill as it is now (nil
// e: a new skill at skillPath) and returns them sorted by path, without
// the ones that would not change.
func (s *Service) nativeFiles(ctx context.Context, si *srcIndex, e *skillEntry, skillPath string, in map[string]string) ([]changeFile, error) {
	maxFile := si.src.Limits.effective().fileBytes
	existing := map[string]*fileEntry{}
	if e != nil {
		for _, f := range si.skillFiles[e.path] {
			// Only this skill's own files: a nested skill's files are its
			// own skill's to write.
			if f.skill == e.path {
				existing[relTo(e.path, f.path)] = f
			}
		}
	}
	nested := map[string]bool{}
	for p := range si.byPath {
		if e != nil && p != e.path && inFolder(e.path, p) {
			nested[relTo(e.path, p)] = true
		}
	}
	name := path.Base(skillPath)
	if e != nil {
		name = e.name
	}
	paths := sortedPaths(in)
	var out []changeFile
	total := 0
	var olds []string
	for _, p := range paths {
		if _, ok := existing[p]; ok {
			olds = append(olds, existing[p].sha)
		}
	}
	contents, err := s.loadBlobs(ctx, olds)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		content := in[p]
		if err := checkChangePath(p); err != nil {
			return nil, fmt.Errorf("%w: files: %s: %v", ErrInvalid, reviewCode(changeLine(p)), err)
		}
		for n := range nested {
			if p == n || inFolder(n, p) {
				return nil, fmt.Errorf("%w: files: %s belongs to the nested skill %s; write that skill instead", ErrInvalid, reviewCode(p), reviewCode(n))
			}
		}
		cur, has := existing[p]
		if content == "" {
			switch {
			case p == skillFile:
				return nil, fmt.Errorf("%w: files: SKILL.md cannot be deleted; a skill needs it", ErrInvalid)
			case !has:
				return nil, fmt.Errorf("%w: files: %s does not exist in the skill, so it cannot be deleted", ErrInvalid, reviewCode(p))
			}
			f := changeFile{Path: p, Op: changeOpDelete}
			if old, ok := contents[cur.sha]; ok {
				f.Old, f.HasOld = string(old), true
			}
			out = append(out, f)
			continue
		}
		switch {
		case int64(len(content)) > maxFile:
			return nil, fmt.Errorf("%w: files: %s is %d bytes; the collection allows %d per file", ErrLimit, reviewCode(p), len(content), maxFile)
		case !utf8.ValidString(content) || strings.ContainsRune(content, 0):
			return nil, fmt.Errorf("%w: files: %s must be UTF-8 text without NUL bytes", ErrInvalid, reviewCode(p))
		}
		if what := findSecret([]byte(content)); what != "" {
			return nil, fmt.Errorf("%w: files: %s looks like it holds %s; remove it, skills are shared text", ErrInvalid, reviewCode(p), what)
		}
		if p == skillFile {
			if _, warns, skip := parseSkillMD(repoPath(skillPath, p), name, []byte(content)); skip {
				msg := "it is not a valid skill"
				for _, w := range warns {
					if w.Severity == "skip" {
						msg = strings.TrimPrefix(w.Message, "skipped: ")
					}
				}
				return nil, fmt.Errorf("%w: files: SKILL.md: %s; it needs YAML frontmatter with name %q and a description", ErrInvalid, msg, name)
			}
		}
		f := changeFile{Path: p, Op: changeOpAdd, Content: content}
		if has {
			f.Op = changeOpChange
			if old, ok := contents[cur.sha]; ok {
				if string(old) == content {
					continue // no change
				}
				f.Old, f.HasOld = string(old), true
			}
		}
		total += len(content)
		if total > MaxChangeBytes {
			return nil, fmt.Errorf("%w: files: the new contents are over %d bytes together", ErrLimit, MaxChangeBytes)
		}
		out = append(out, f)
	}
	if e == nil {
		if _, ok := in[skillFile]; !ok {
			return nil, fmt.Errorf("%w: files: a new skill needs SKILL.md", ErrInvalid)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: nothing changes: every file already has that content", ErrInvalid)
	}
	// After the write, no file may sit where a folder is, or inside a file.
	final := map[string]bool{}
	for p := range existing {
		final[p] = true
	}
	for _, f := range out {
		final[f.Path] = f.Op != changeOpDelete
	}
	for _, f := range out {
		if f.Op == changeOpDelete {
			continue
		}
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if final[d] {
				return nil, fmt.Errorf("%w: files: %s is a file, so %s cannot be inside it", ErrInvalid, reviewCode(d), reviewCode(f.Path))
			}
		}
		for q, ok := range final {
			if ok && strings.HasPrefix(q, f.Path+"/") {
				return nil, fmt.Errorf("%w: files: %s is a folder in the skill", ErrInvalid, reviewCode(f.Path))
			}
		}
	}
	return out, nil
}

// scriptExts are the file extensions sensitiveReasons counts as code.
var scriptExts = map[string]bool{
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true,
	".ts": true, ".rb": true, ".pl": true, ".php": true, ".lua": true, ".ps1": true, ".bat": true, ".cmd": true,
	".go": true, ".rs": true, ".java": true, ".kt": true, ".swift": true, ".r": true, ".sql": true, ".exe": true,
}

// isScript reports a file an agent following the skill may run: one in a
// scripts folder, one with a code extension, or one that starts with #!.
func isScript(p, content string) bool {
	if scriptExts[strings.ToLower(path.Ext(p))] || strings.HasPrefix(content, "#!") {
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if strings.EqualFold(seg, "scripts") || strings.EqualFold(seg, "bin") {
			return true
		}
	}
	return false
}

// protectedKey is the frontmatter metadata key that marks a skill whose
// every agent change needs approval.
const protectedKey = "toolyard-protected"

// sensitiveReasons says why an agent's write needs the collection owner's
// approval, or nothing. e is the skill as it is (nil for a new skill).
func sensitiveReasons(src *Source, e *skillEntry, files []changeFile) []string {
	var out []string
	if src.Mode == ModeReview {
		out = append(out, "the collection asks for approval of every agent change")
	}
	var oldFM map[string]any
	if e != nil {
		_ = json.Unmarshal(e.frontmatter, &oldFM)
		if truthy(metaValue(oldFM, protectedKey)) {
			out = append(out, "the skill is protected (metadata."+protectedKey+")")
		}
	}
	var scripts []string
	for _, f := range files {
		content := f.Content
		if f.Op == changeOpDelete {
			content = f.Old
		}
		if isScript(f.Path, content) {
			scripts = append(scripts, f.Path)
		}
	}
	if len(scripts) > 0 {
		if len(scripts) > 3 {
			scripts = append(scripts[:3], "…")
		}
		out = append(out, "it writes code an agent may run ("+strings.Join(scripts, ", ")+")")
	}
	for _, f := range files {
		if f.Path != skillFile || f.Op == changeOpDelete {
			continue
		}
		var newFM map[string]any
		if text, _, err := splitFrontmatter(f.Content); err == nil {
			newFM, _ = parseYAMLMap(text)
		}
		if !sameJSON(oldFM["allowed-tools"], newFM["allowed-tools"]) {
			out = append(out, "it changes allowed-tools")
		}
		oldRuns, newRuns := metaValue(oldFM, runsKey), metaValue(newFM, runsKey)
		if !sameJSON(oldRuns, newRuns) && (e != nil || strings.EqualFold(fmt.Sprint(newRuns), string(RunsHost))) {
			out = append(out, "it changes where the skill runs (metadata."+runsKey+")")
		}
		if !sameJSON(metaValue(oldFM, protectedKey), metaValue(newFM, protectedKey)) && (e != nil || truthy(metaValue(newFM, protectedKey))) {
			out = append(out, "it changes metadata."+protectedKey)
		}
	}
	return out
}

// metaValue is frontmatter metadata[key], or nil.
func metaValue(fm map[string]any, key string) any {
	m, _ := fm["metadata"].(map[string]any)
	return m[key]
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "1", "on":
			return true
		}
	}
	return false
}

// applyNative builds the collection with the write, makes it live and
// records the revision, under the collection's sync lock (so writes of one
// collection never overlap on any node). A proposal's write is applied
// once: a retry finds its revision.
func (s *Service) applyNative(ctx context.Context, a nativeApply) (*nativeApplied, error) {
	ctx, cancel := context.WithTimeout(ctx, nativeWriteTimeout)
	defer cancel()
	lctx, unlock, err := s.cluster.LockCtx(ctx, "skillhub:sync:"+a.sourceID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: lock the collection: %w", err)
	}
	defer unlock()
	if a.proposalID != "" {
		if rev, err := s.revisionOfProposal(lctx, a.proposalID); err != nil || rev != nil {
			if rev != nil {
				return &nativeApplied{revision: rev.Revision}, nil
			}
			return nil, err
		}
	}
	row, err := s.getSourceRow(lctx, s.db, a.sourceID)
	if err != nil {
		return nil, err
	}
	if !row.IsNative() {
		return nil, fmt.Errorf("%w: source %s is not a native collection", ErrInvalid, row.Slug)
	}
	lim := row.Limits.effective()
	fs, dirs := newFileSet(lim), map[string]bool{}
	var active *versionRow
	if row.ActiveVersionID != "" {
		if active, err = s.getVersionRow(lctx, s.db, row.ID, row.ActiveVersionID); err != nil {
			return nil, err
		}
		if fs, dirs, err = s.baseFileSet(lctx, active, "", lim); err != nil {
			return nil, err
		}
	}
	switch {
	case a.create && dirs[a.skillPath]:
		return nil, fmt.Errorf("%w: skill://%s/%s already exists; update it instead", ErrConflict, row.Slug, a.skillPath)
	case !a.create && !dirs[a.skillPath]:
		return nil, fmt.Errorf("%w: skill://%s/%s is gone", ErrNotFound, row.Slug, a.skillPath)
	}
	latest, err := s.latestRevision(lctx, row.ID, a.skillPath)
	if err != nil {
		return nil, err
	}
	if !a.create && a.baseRevision != 0 {
		// The skill must still be what the writer (or the approver) saw.
		cur := 0
		if active != nil {
			if cur, err = s.currentRevision(lctx, row.ID, active.ID, a.skillPath); err != nil {
				return nil, err
			}
		}
		if (a.baseRevision > 0 && cur != a.baseRevision) || (a.baseRevision < 0 && cur != 0) {
			return nil, fmt.Errorf("%w: skill://%s/%s changed since it was read (it is at revision %d now); read it again and write on top of it",
				ErrConflict, row.Slug, a.skillPath, cur)
		}
	}
	for _, f := range a.files {
		full := repoPath(a.skillPath, f.Path)
		fs.remove(full)
		if f.Op == changeOpDelete {
			continue
		}
		if err := fs.add(full, []byte(f.Content)); err != nil {
			return nil, err
		}
		if w, ok := fs.omitted[full]; ok {
			return nil, fmt.Errorf("%w: files: %s: %s", ErrInvalid, reviewCode(f.Path), strings.TrimPrefix(w.Message, "omitted: "))
		}
	}
	contents, err := s.loadSkillMDs(lctx, fs)
	if err != nil {
		return nil, err
	}
	b, err := discover(fs, "", row.rootName(), contents)
	if err != nil {
		return nil, err
	}
	var sk *builtSkill
	for _, x := range b.skills {
		if x.path == a.skillPath {
			sk = x
		}
	}
	if sk == nil {
		msg := "it would not be a valid skill"
		for _, w := range b.warnings {
			if w.Severity == "skip" && w.Path == repoPath(a.skillPath, skillFile) {
				msg = strings.TrimPrefix(w.Message, "skipped: ")
			}
		}
		return nil, fmt.Errorf("%w: SKILL.md: %s", ErrInvalid, msg)
	}
	sha := treeDigest(fs)
	cfg := row.configHash()
	known, err := s.findVersion(lctx, row.ID, sha, cfg)
	if err != nil {
		return nil, err
	}
	versionID := ""
	if known != nil && known.Status != VersionFailed && known.Status != VersionRejected {
		versionID = known.ID // the collection is back to a tree it held before
	} else {
		diff, err := s.diffAgainstActive(lctx, row, sha, b)
		if err != nil {
			return nil, err
		}
		v, err := s.insertVersion(lctx, newVersion{src: row, sha: sha, config: cfg, b: b, full: true, diff: diff})
		if err != nil {
			return nil, err
		}
		versionID = v.ID
	}
	st := stateOf(row)
	if _, _, err := s.activate(lctx, activation{by: a.author, sourceID: row.ID, versionID: versionID, fromSync: true, expect: &st}); err != nil {
		if errors.Is(err, errSourceChanged) {
			return nil, fmt.Errorf("%w: the collection changed during the write; try again", ErrConflict)
		}
		return nil, err
	}
	op := revisionUpdate
	if a.create {
		op = revisionCreate
	}
	rev := latest + 1
	if err := s.insertRevision(context.WithoutCancel(lctx), row.ID, versionID, rev, op, sk, b, a); err != nil {
		// The write is live; only its history entry is missing.
		s.log.Warn("skillhub: recording a skill revision failed", "source", row.Slug, "skill", a.skillPath, "err", err.Error())
		rev = 0
	}
	s.changed(lctx, row.ID)
	if err := s.retain(lctx, row.ID); err != nil {
		s.log.Warn("skillhub: retention failed", "source", row.Slug, "err", err.Error())
	}
	s.auditWrite(lctx, row, a, rev, sha)
	var warns []Warning
	prefix := a.skillPath + "/"
	for _, w := range b.warnings {
		if w.Path == a.skillPath || strings.HasPrefix(w.Path, prefix) {
			warns = append(warns, w)
		}
	}
	return &nativeApplied{revision: rev, commit: sha, warnings: warns}, nil
}

// treeDigest is a native version's commit: the SHA-256 of every path and
// its content digest, in path order (the built-in source's digest).
func treeDigest(fs *fileSet) string {
	h := sha256.New()
	for _, p := range sortedPaths(fs.files) {
		fmt.Fprintf(h, "%s\x00%s\n", p, strings.TrimPrefix(fs.files[p].sha, "sha256:"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// auditWrite records a live write.
func (s *Service) auditWrite(ctx context.Context, row *sourceRow, a nativeApply, rev int, sha string) {
	if s.nat.audit == nil {
		return
	}
	ev := "skills.native_update"
	if a.create {
		ev = "skills.native_create"
	}
	var paths []string
	for _, f := range a.files {
		paths = append(paths, f.Op+" "+f.Path)
	}
	reason := a.note
	if reason == "" {
		reason = "native skill write"
	}
	_ = s.nat.audit.Write(ctx, audit.Event{EventType: ev, AgentID: a.agent, Reason: reason, ApprovalID: a.proposalID,
		DecidedByUserID: a.author,
		ResultSummary: reviewClip(fmt.Sprintf("skill://%s/%s revision %d (%s): %s", row.Slug, a.skillPath, rev,
			RepoLabel(row.Owner, row.Repo, sha), strings.Join(paths, ", ")), 1000)})
}

// ---- revisions -------------------------------------------------------------

// insertRevision records one write: the revision and the skill's own files
// (not a nested skill's).
func (s *Service) insertRevision(ctx context.Context, sourceID, versionID string, rev int, op string, sk *builtSkill, b *built, a nativeApply) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	var bytes int64
	for _, f := range b.files {
		if f.skill != sk.path {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_revision_files (source_id, skill_path, revision, path, sha256, size, media_type)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, sourceID, sk.path, int64(rev), relTo(sk.path, f.path), f.sha, f.size, f.media); err != nil {
			return err
		}
		count++
		bytes += f.size
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_revisions (source_id, skill_path, revision, op, version_id, name, files_digest,
		skill_md_sha256, file_count, total_bytes, author_user_id, author_agent_id, note, proposal_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, sk.path, int64(rev), op, versionID, sk.meta.name, sk.filesDigest, sk.skillMD.sha, int64(count), bytes,
		a.author, a.agent, a.note, a.proposalID, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// latestRevision is a skill's highest revision number (0: none).
func (s *Service) latestRevision(ctx context.Context, sourceID, skillPath string) (int, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(revision) FROM skill_revisions WHERE source_id = ? AND skill_path = ?`,
		sourceID, skillPath).Scan(&n); err != nil {
		return 0, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	return int(n.Int64), nil
}

// currentRevision is the revision whose content a version serves for a
// skill: the newest revision with the skill's files digest (0: none, as
// after a rollback to content no revision wrote).
func (s *Service) currentRevision(ctx context.Context, sourceID, versionID, skillPath string) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(r.revision) FROM skill_revisions r JOIN skill_entries e
		ON e.version_id = ? AND e.path = r.skill_path AND e.files_digest = r.files_digest
		WHERE r.source_id = ? AND r.skill_path = ?`, versionID, sourceID, skillPath).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	return int(n.Int64), nil
}

// currentRevisions is currentRevision for every skill of a version, by
// path (index.go).
func (s *Service) currentRevisions(ctx context.Context, sourceID, versionID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.skill_path, MAX(r.revision) FROM skill_revisions r JOIN skill_entries e
		ON e.version_id = ? AND e.path = r.skill_path AND e.files_digest = r.files_digest
		WHERE r.source_id = ? GROUP BY r.skill_path`, versionID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			return nil, fmt.Errorf("skillhub: read revisions: %w", err)
		}
		out[p] = int(n)
	}
	return out, rows.Err()
}

const revisionColumns = `revision, op, name, files_digest, skill_md_sha256, file_count, total_bytes, author_user_id,
	author_agent_id, note, proposal_id, created_at`

func scanRevision(sc rowScanner) (*Revision, error) {
	var r Revision
	var rev, files, created int64
	if err := sc.Scan(&rev, &r.Op, &r.Name, &r.FilesDigest, &r.SkillMD, &files, &r.TotalBytes, &r.Author,
		&r.Agent, &r.Note, &r.ProposalID, &created); err != nil {
		return nil, err
	}
	r.Revision, r.FileCount, r.CreatedAt = int(rev), int(files), time.UnixMilli(created).UTC()
	return &r, nil
}

// revisionOfProposal is the revision an approved proposal wrote, or nil.
func (s *Service) revisionOfProposal(ctx context.Context, proposalID string) (*Revision, error) {
	r, err := scanRevision(s.db.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM skill_revisions WHERE proposal_id = ?`, proposalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	return r, nil
}

// nativeSkill resolves a visible native skill by reference.
func (s *Service) nativeSkill(v Viewer, ref string) (*srcIndex, *skillEntry, error) {
	si, e, err := s.resolve(v, ref)
	if err != nil {
		return nil, nil, err
	}
	if !si.src.IsNative() {
		return nil, nil, fmt.Errorf("%w: %s comes from a repository; its history is the repository's commits (%s)", ErrInvalid,
			si.skillURI(e.path), si.htmlURL(e.path))
	}
	return si, e, nil
}

// SkillRevisions lists a native skill's revisions, newest first.
func (s *Service) SkillRevisions(ctx context.Context, v Viewer, ref string, limit int) ([]Revision, error) {
	si, e, err := s.nativeSkill(v, ref)
	if err != nil {
		return nil, err
	}
	switch {
	case limit <= 0:
		limit = defaultRevisionsList
	case limit > MaxRevisionsList:
		limit = MaxRevisionsList
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+revisionColumns+` FROM skill_revisions WHERE source_id = ? AND skill_path = ?
		ORDER BY revision DESC LIMIT ?`, si.src.ID, e.path, limit)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("skillhub: read revisions: %w", err)
		}
		r.URI = si.skillURI(e.path)
		r.Current = r.Revision == e.revision
		out = append(out, *r)
	}
	return out, rows.Err()
}

// revisionFile is one file of a revision.
type revisionFile struct {
	path, sha, media string
	size             int64
}

// revisionFiles reads a revision's files, by path; ErrNotFound when there
// is no such revision.
func (s *Service) revisionFiles(ctx context.Context, si *srcIndex, skillPath string, revision int) (*Revision, []revisionFile, error) {
	r, err := scanRevision(s.db.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM skill_revisions
		WHERE source_id = ? AND skill_path = ? AND revision = ?`, si.src.ID, skillPath, int64(revision)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: revision %d of %s", ErrNotFound, revision, si.skillURI(skillPath))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, sha256, size, media_type FROM skill_revision_files
		WHERE source_id = ? AND skill_path = ? AND revision = ? ORDER BY path`, si.src.ID, skillPath, int64(revision))
	if err != nil {
		return nil, nil, fmt.Errorf("skillhub: read revisions: %w", err)
	}
	defer rows.Close()
	var files []revisionFile
	for rows.Next() {
		var f revisionFile
		if err := rows.Scan(&f.path, &f.sha, &f.size, &f.media); err != nil {
			return nil, nil, fmt.Errorf("skillhub: read revisions: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	r.URI = si.skillURI(skillPath)
	return r, files, nil
}

// revisionURI is a file URI of a revision.
func revisionURI(uri string, revision int) string {
	return fmt.Sprintf("%s?revision=%d", uri, revision)
}

// GetRevision returns a native skill as one revision wrote it.
func (s *Service) GetRevision(ctx context.Context, v Viewer, ref string, revision int, includeFiles bool) (*Skill, error) {
	if revision <= 0 {
		return nil, fmt.Errorf("%w: revision must be 1 or more", ErrInvalid)
	}
	si, e, err := s.nativeSkill(v, ref)
	if err != nil {
		return nil, err
	}
	r, files, err := s.revisionFiles(ctx, si, e.path, revision)
	if err != nil {
		return nil, err
	}
	raw, err := s.loadBlob(ctx, r.SkillMD)
	if err != nil {
		return nil, err
	}
	text, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, fmt.Errorf("skillhub: revision %d of %s: %w", revision, si.skillURI(e.path), err)
	}
	fm, err := parseYAMLMap(text)
	if err != nil {
		return nil, fmt.Errorf("skillhub: revision %d of %s: %w", revision, si.skillURI(e.path), err)
	}
	desc, _ := fm["description"].(string)
	sum := si.summary(e)
	sum.Name, sum.Description, sum.Revision = r.Name, strings.TrimSpace(desc), r.Revision
	sk := &Skill{SkillSummary: sum, Frontmatter: fm, Body: body, Raw: string(raw)}
	if includeFiles {
		for _, f := range files {
			sk.Files = append(sk.Files, FileRef{URI: revisionURI(si.fileURI(e.path, repoPath(e.path, f.path)), revision),
				Path: f.path, Digest: f.sha, Size: f.size, Type: f.media})
		}
	}
	return sk, nil
}

// ReadRevision reads one file of a native skill as a revision wrote it.
// uri is a file URI, with or without its ?revision=N (revision wins when
// both are given and differ: ErrInvalid).
func (s *Service) ReadRevision(ctx context.Context, v Viewer, uri string, revision, offset, limit int) (*File, error) {
	uri = strings.TrimSpace(uri)
	if i := strings.Index(uri, "?revision="); i >= 0 {
		var n int
		if _, err := fmt.Sscanf(uri[i+len("?revision="):], "%d", &n); err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: uri: ?revision= must be a number", ErrInvalid)
		}
		if revision != 0 && revision != n {
			return nil, fmt.Errorf("%w: the uri names revision %d and revision says %d", ErrInvalid, n, revision)
		}
		revision, uri = n, uri[:i]
	}
	if revision <= 0 {
		return nil, fmt.Errorf("%w: revision must be 1 or more", ErrInvalid)
	}
	slug, rest, ok := splitURI(uri)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a skill file URI", ErrInvalid, uri)
	}
	si := s.idx.slug(slug)
	if si == nil || !si.visibleTo(v) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	if p, ok := si.unalias(rest); ok {
		rest = p
	}
	// The skill is the innermost visible skill folder that holds the
	// file; it must still exist (a revision of a skill that is gone is
	// not served).
	sp, ok := innermostSkill(rest, si.byPath)
	if !ok || !si.skillVisible(v, sp) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	e := si.byPath[sp]
	if !si.src.IsNative() {
		return nil, fmt.Errorf("%w: %s comes from a repository; it has no revisions", ErrInvalid, uri)
	}
	_, files, err := s.revisionFiles(ctx, si, e.path, revision)
	if err != nil {
		return nil, err
	}
	rel := relTo(e.path, rest)
	for _, f := range files {
		if f.path != rel {
			continue
		}
		data, err := s.loadBlob(ctx, f.sha)
		if err != nil {
			return nil, err
		}
		content, total, truncated := sliceLines(string(data), offset, limit)
		return &File{
			FileRef: FileRef{URI: revisionURI(si.fileURI(e.path, rest), revision), Path: rel, Digest: f.sha, Size: f.size, Type: f.media},
			Skill:   si.skillURI(e.path), Repo: si.repoLabel(), Commit: si.sha, Content: content, Offset: offset,
			TotalLines: total, Truncated: truncated,
		}, nil
	}
	return nil, fmt.Errorf("%w: %s at revision %d", ErrNotFound, uri, revision)
}

// ---- composing a SKILL.md --------------------------------------------------

// SkillDraft is a new skill's SKILL.md in parts: ComposeSkillMD writes the
// frontmatter (name, description, and metadata triggers and tags as the
// Agent Skills string values) and the body.
type SkillDraft struct {
	Name        string
	Description string
	// Triggers say when to use the skill ("the user asks to deploy").
	// They are stored as metadata.triggers, one per line.
	Triggers []string
	// Tags are short labels for filtering, stored as metadata.tags,
	// comma-separated.
	Tags []string
	// Metadata adds other metadata keys (string values).
	Metadata map[string]string
	// AllowedTools is the frontmatter allowed-tools, "" for none.
	AllowedTools string
	Body         string
}

// ComposeSkillMD writes a SKILL.md from its parts. The YAML is generated,
// so any text in the parts stays data.
func ComposeSkillMD(d SkillDraft) (string, error) {
	name, desc := strings.TrimSpace(d.Name), strings.TrimSpace(d.Description)
	switch {
	case name == "" || len(name) > maxNameLen || !skillNameRe.MatchString(name):
		return "", fmt.Errorf("%w: name: 1-%d lowercase letters, digits and single hyphens", ErrInvalid, maxNameLen)
	case desc == "":
		return "", fmt.Errorf("%w: description is required: what the skill does and when to use it", ErrInvalid)
	case utf8.RuneCountInString(desc) > maxDescription:
		return "", fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxDescription)
	}
	meta := map[string]string{}
	for k, v := range d.Metadata {
		if k = strings.TrimSpace(k); k != "" {
			meta[k] = v
		}
	}
	var triggers []string
	for _, t := range d.Triggers {
		if t = strings.TrimSpace(strings.ReplaceAll(t, "\n", " ")); t != "" {
			triggers = append(triggers, t)
		}
	}
	if len(triggers) > 0 {
		meta["triggers"] = strings.Join(triggers, "\n")
	}
	var tags []string
	for _, t := range d.Tags {
		if t = strings.TrimSpace(strings.ReplaceAll(t, ",", " ")); t != "" {
			tags = append(tags, strings.ToLower(t))
		}
	}
	if len(tags) > 0 {
		meta["tags"] = strings.Join(tags, ", ")
	}
	type frontmatter struct {
		Name         string            `yaml:"name"`
		Description  string            `yaml:"description"`
		AllowedTools string            `yaml:"allowed-tools,omitempty"`
		Metadata     map[string]string `yaml:"metadata,omitempty"`
	}
	y, err := yaml.Marshal(frontmatter{Name: name, Description: desc, AllowedTools: strings.TrimSpace(d.AllowedTools), Metadata: meta})
	if err != nil {
		return "", fmt.Errorf("skillhub: write the frontmatter: %w", err)
	}
	body := strings.TrimLeft(d.Body, "\n")
	if strings.TrimSpace(body) == "" {
		body = "# " + name + "\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return "---\n" + string(y) + "---\n\n" + body, nil
}

// SkillPatch changes parts of a SKILL.md: nil keeps a part as it is.
type SkillPatch struct {
	Description *string
	Body        *string
	Triggers    *[]string
	Tags        *[]string
}

// IsZero reports a patch that changes nothing.
func (p SkillPatch) IsZero() bool {
	return p.Description == nil && p.Body == nil && p.Triggers == nil && p.Tags == nil
}

// PatchSkillMD applies a patch to a SKILL.md and keeps everything else of
// its frontmatter (other fields, their order and their metadata) as it is.
// An empty Triggers or Tags list removes that metadata key.
func PatchSkillMD(raw string, p SkillPatch) (string, error) {
	text, body, err := splitFrontmatter(raw)
	if err != nil {
		return "", fmt.Errorf("%w: SKILL.md: %v", ErrInvalid, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("%w: SKILL.md: the frontmatter is not valid YAML", ErrInvalid)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("%w: SKILL.md: the frontmatter is not a mapping", ErrInvalid)
	}
	top := doc.Content[0]
	if p.Description != nil {
		d := strings.TrimSpace(*p.Description)
		switch {
		case d == "":
			return "", fmt.Errorf("%w: description cannot be empty", ErrInvalid)
		case utf8.RuneCountInString(d) > maxDescription:
			return "", fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxDescription)
		}
		yamlSet(top, "description", yamlString(d))
	}
	if p.Triggers != nil || p.Tags != nil {
		meta := yamlGet(top, "metadata")
		if meta == nil || meta.Kind != yaml.MappingNode {
			meta = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			yamlSet(top, "metadata", meta)
		}
		if p.Triggers != nil {
			var ts []string
			for _, t := range *p.Triggers {
				if t = strings.TrimSpace(strings.ReplaceAll(t, "\n", " ")); t != "" {
					ts = append(ts, t)
				}
			}
			if len(ts) == 0 {
				yamlDel(meta, "triggers")
			} else {
				yamlSet(meta, "triggers", yamlString(strings.Join(ts, "\n")))
			}
		}
		if p.Tags != nil {
			var ts []string
			for _, t := range *p.Tags {
				if t = strings.TrimSpace(strings.ReplaceAll(t, ",", " ")); t != "" {
					ts = append(ts, strings.ToLower(t))
				}
			}
			if len(ts) == 0 {
				yamlDel(meta, "tags")
			} else {
				yamlSet(meta, "tags", yamlString(strings.Join(ts, ", ")))
			}
		}
		if len(meta.Content) == 0 {
			yamlDel(top, "metadata")
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("skillhub: write the frontmatter: %w", err)
	}
	if p.Body != nil {
		body = strings.TrimLeft(*p.Body, "\n")
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body = "\n" + body
	}
	return "---\n" + string(out) + "---\n" + body, nil
}

// yamlString is a string scalar; a multi-line one is a literal block.
func yamlString(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	if strings.Contains(s, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}

// yamlGet is the value of key in a mapping, or nil.
func yamlGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlSet sets key in a mapping, in place when it is there, else at the
// end.
func yamlSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// yamlDel removes key from a mapping.
func yamlDel(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

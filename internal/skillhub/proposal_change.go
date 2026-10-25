package skillhub

// ChangeProposals implements skills.propose_change: an agent's edit of one
// skill becomes one Inbox question for the person who added the skill's
// source (the GitHub account whose connection writes), with a short diff of
// every file. Their approval opens a pull request on the source repository
// through their own toolyard GitHub connection: a branch
// toolyard/skills/<proposal id> from the commit the agent saw, one commit
// with the files, and a pull request into the source branch. Nothing is
// merged; the source picks the change up like any other commit once
// someone merges it on GitHub.
//
// GitHub writes run only after the account owner's own decision in the
// Inbox: never on a grant, a policy or an automatic rule. Each write is
// reconciled before it is made (the branch and the pull request carry the
// proposal ID), and a write whose outcome is uncertain is never repeated
// blindly: the proposal stays approved, and the next attempt (the Inbox
// sweep, or the agent's poll, after a few minutes) looks for the branch and
// the pull request first.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// ChangeProposalItemKind is the Inbox system item kind of a change
// proposal.
const ChangeProposalItemKind = "skillhub.change_proposal"

// Limits on change proposals.
const (
	// MaxChangeFiles is how many files one proposal may touch.
	MaxChangeFiles = 50
	// MaxChangeTitle and MaxChangeSummary bound the pull request title
	// and body the agent writes (characters). The owner's Inbox item shows
	// both in full, so the pull request carries exactly what they read.
	MaxChangeTitle   = 200
	MaxChangeSummary = 3000
	// MaxChangeBytes bounds the new contents of one proposal together.
	MaxChangeBytes = 4 << 20
	// MaxPendingChangeProposals is how many change proposals one agent
	// may have waiting at once.
	MaxPendingChangeProposals = 5
	// ChangeBranchPrefix starts the branch of every proposal.
	ChangeBranchPrefix = "toolyard/skills/"
)

const (
	proposalKindChange = "change"
	// changeMarkerPrefix ends the commit message: it ties a branch found
	// on GitHub to its proposal.
	changeMarkerPrefix = "toolyard-proposal: "
	// changeStaleAfter is how long an approved proposal may stay
	// unfinished before a poll resumes it (longer than the Inbox handler
	// may run).
	changeStaleAfter = 3 * time.Minute
	// changeApplyTimeout bounds one attempt at the GitHub writes.
	changeApplyTimeout = 2 * time.Minute
	// changeMaxAge and changeMaxAttempts bound the retries of an approved
	// proposal whose pull request is not confirmed: after either, it fails.
	changeMaxAge      = 24 * time.Hour
	changeMaxAttempts = 10
)

// ChangeOptions configure ChangeProposals.
type ChangeOptions struct {
	DB    *store.DB
	Inbox *inbox.Service
	// Reader resolves the skill as the agent sees it; Admin finds its
	// source.
	Reader Reader
	Admin  Admin
	// Tokens returns a person's toolyard GitHub connection token
	// (GitHubConnections.TokenSource). nil turns change proposals off.
	Tokens TokenSource
	// HTTPClient talks to the GitHub API (tests: a fake); nil is the
	// default client.
	HTTPClient *http.Client
	// AllowBuildFiles reports the setting skills_change_allow_build_files:
	// when true, a proposal may write CI or build configuration that sits
	// directly in the skill's own folder (changeBuildPath). nil or false
	// refuses every such path.
	AllowBuildFiles func(ctx context.Context) bool
	// AgentName and UserName name an agent and a person in the item and
	// the pull request ("" falls back to the ID).
	AgentName func(ctx context.Context, agentID string) string
	UserName  func(ctx context.Context, userID string) string
	// Audit, when set, records every proposal, decision and pull request.
	Audit *audit.Logger
	Now   func() time.Time
}

// ChangeProposals takes agents' change proposals. Create it with
// NewChangeProposals and give it to SourceProposals.SetChangeProposals.
type ChangeProposals struct {
	o     ChangeOptions
	store proposalStore
}

// NewChangeProposals registers the change item handler with the Inbox.
func NewChangeProposals(o ChangeOptions) (*ChangeProposals, error) {
	if o.DB == nil || o.Inbox == nil || o.Reader == nil || o.Admin == nil {
		return nil, errors.New("skillhub: change proposals need a database, the Inbox, the reader and the admin service")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &ChangeProposals{o: o, store: proposalStore{db: o.DB, now: o.Now}}
	// HumanOnly: the Inbox itself refuses an answer that is not the
	// creator's own (an operator token, a batch, a rule, a grant).
	o.Inbox.HandleSystem(ChangeProposalItemKind, c.decide, inbox.SystemOptions{HumanOnly: true})
	return c, nil
}

// SetChangeProposals makes skills.propose_change (and the polls of change
// proposals) go to c. Call it at start, before the gateway serves.
func (p *SourceProposals) SetChangeProposals(c *ChangeProposals) { p.changes = c }

// changeInput is what a change row keeps: where the change goes, what the
// owner approved, and the files.
type changeInput struct {
	Skill      string       `json:"skill"`      // skill URI
	SkillPath  string       `json:"skill_path"` // in the source folder ("" for a skill at the folder itself)
	Name       string       `json:"name"`
	Slug       string       `json:"slug"`
	Owner      string       `json:"owner"`
	Repo       string       `json:"repo"`
	Branch     string       `json:"branch"` // the pull request's base
	Folder     string       `json:"folder"` // the source folder in the repository
	BaseSHA    string       `json:"base_sha"`
	AgentOwner string       `json:"agent_owner"` // user ID of the proposing agent's owner
	Title      string       `json:"title"`
	Summary    string       `json:"summary,omitempty"`
	Files      []changeFile `json:"files"`
	// HeadBranch is the pull request's branch: ChangeBranchPrefix, the
	// proposal ID and a random suffix the agent never sees, so nobody can
	// prepare that branch before the approval.
	HeadBranch string `json:"head_branch,omitempty"`
	// Attempts counts the resumed attempts at the GitHub writes.
	Attempts int `json:"attempts,omitempty"`
}

// changeFile is one file of a proposal.
type changeFile struct {
	Path    string `json:"path"` // in the skill folder
	Op      string `json:"op"`   // add | change | delete
	Content string `json:"content,omitempty"`
	// Old is the current text, kept only for the item's diff (not stored).
	Old    string `json:"-"`
	HasOld bool   `json:"-"`
}

const (
	changeOpAdd    = "add"
	changeOpChange = "change"
	changeOpDelete = "delete"
)

// skillDir is the skill's folder in the repository.
func (ci *changeInput) skillDir() string { return repoPath(ci.Folder, ci.SkillPath) }

// branchFor is the pull request's branch of proposal id.
func (ci *changeInput) branchFor(id string) string {
	if ci.HeadBranch != "" {
		return ci.HeadBranch
	}
	return ChangeBranchPrefix + id
}

// changeBranchSuffix is a random suffix for a proposal's branch (48 bits).
func changeBranchSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("skillhub: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ProposeChange checks the edit against the version of the skill the agent
// sees and asks the source's creator in the Inbox. Nothing is written to
// GitHub before they approve.
func (c *ChangeProposals) ProposeChange(ctx context.Context, v Viewer, in ChangeInput) (*Proposal, error) {
	title := changeLine(in.Title)
	summary := strings.TrimSpace(in.Summary)
	switch {
	case v.AgentID == "" || v.UserID == "":
		return nil, fmt.Errorf("%w: only an agent can propose a skill change", ErrInvalid)
	case strings.TrimSpace(in.Skill) == "":
		return nil, fmt.Errorf("%w: skill is required", ErrInvalid)
	case title == "":
		return nil, fmt.Errorf("%w: title is required: a short title for the pull request", ErrInvalid)
	case utf8.RuneCountInString(title) > MaxChangeTitle:
		return nil, fmt.Errorf("%w: title is longer than %d characters", ErrInvalid, MaxChangeTitle)
	case !utf8.ValidString(summary) || utf8.RuneCountInString(summary) > MaxChangeSummary:
		return nil, fmt.Errorf("%w: summary must be UTF-8 text of at most %d characters", ErrInvalid, MaxChangeSummary)
	case len(in.Files) == 0:
		return nil, fmt.Errorf("%w: files: give at least one file (path in the skill folder → its new content)", ErrInvalid)
	case len(in.Files) > MaxChangeFiles:
		return nil, fmt.Errorf("%w: files: at most %d files in one proposal", ErrLimit, MaxChangeFiles)
	case c.o.Tokens == nil:
		return nil, fmt.Errorf("%w: change proposals need toolyard's GitHub connections, which this server does not have; tell your owner what should change", ErrInvalid)
	}
	sk, err := c.o.Reader.Get(ctx, v, strings.TrimSpace(in.Skill), true)
	if err != nil {
		return nil, err
	}
	src, err := c.sourceBySlug(ctx, sk.Source)
	if err != nil {
		return nil, err
	}
	switch {
	case src.CreatedBy == "":
		return nil, fmt.Errorf("%w: the skill source %s has no owner who could open a pull request", ErrInvalid, src.Slug)
	case sk.Commit == "":
		return nil, fmt.Errorf("%w: the skill has no commit to base a change on", ErrInvalid)
	}
	n, err := c.store.countPending(ctx, v.AgentID, proposalKindChange)
	if err != nil {
		return nil, err
	}
	if n >= MaxPendingChangeProposals {
		return nil, fmt.Errorf("%w: you already have %d change proposals waiting; wait for a decision first", ErrConflict, n)
	}
	files, err := c.checkFiles(ctx, v, sk, src, in.Files)
	if err != nil {
		return nil, err
	}
	if tok, err := c.o.Tokens(ctx, src.CreatedBy); err != nil || tok == "" {
		return nil, fmt.Errorf("%w: the owner of the skill source %s has no usable toolyard GitHub connection, so toolyard cannot open a pull request; tell your owner what should change", ErrInvalid, src.Slug)
	}
	id := proposalNewID()
	ci := changeInput{Skill: sk.URI, SkillPath: sk.Path, Name: sk.Name, Slug: src.Slug, Owner: src.Owner, Repo: src.Repo,
		Branch: src.Branch, Folder: src.Path, BaseSHA: sk.Commit, AgentOwner: v.UserID, Title: title, Summary: summary, Files: files,
		HeadBranch: ChangeBranchPrefix + id + "-" + changeBranchSuffix()}
	agent := c.agentName(ctx, v.AgentID)
	// The owner approves exactly what the item shows: every path, the
	// whole summary and every diff. A change that does not fit is refused
	// rather than shown in part.
	message := changeMessage(agent, c.userName(ctx, v.UserID), &ci)
	if n := utf8.RuneCountInString(message); n > inbox.MaxMessage {
		return nil, fmt.Errorf("%w: the change is too large to show in full in the owner's Inbox (%d of at most %d characters with its diffs); propose fewer or smaller files", ErrLimit, n, inbox.MaxMessage)
	}
	stored, err := json.Marshal(ci)
	if err != nil {
		return nil, err
	}
	row := &proposalRow{ID: id, Kind: proposalKindChange, Status: proposalPending, AgentID: v.AgentID,
		UserID: src.CreatedBy, SourceID: src.ID, Input: string(stored), Reason: summary}
	if src.ActiveSHA == sk.Commit {
		row.VersionID = src.ActiveVersionID
	}
	if err := c.store.insert(ctx, row); err != nil {
		return nil, err
	}
	item, err := c.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ChangeProposalItemKind, Ref: row.ID, UserID: src.CreatedBy,
		Title:     reviewClip("Skill change: "+title, inbox.MaxTitle),
		Summary:   reviewClip(changeItemSummary(agent, &ci), inbox.MaxSummary),
		Message:   message,
		Questions: reviewQuestion("Open a pull request on "+src.Owner+"/"+src.Repo+"?", "Approve: open the pull request with my GitHub account", "Reject: send nothing to GitHub"),
		Activity:  "Proposed by " + agent + " with skills.propose_change",
	})
	if err != nil {
		_, _ = c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		if errors.Is(err, inbox.ErrTooManyPending) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	if err := c.store.setInbox(ctx, row.ID, item.ID); err != nil {
		return nil, err
	}
	row.InboxID = item.ID
	c.audit(ctx, audit.Event{EventType: "skills.change_propose", AgentID: v.AgentID, Reason: title,
		ResultSummary: fmt.Sprintf("%s %s: %d files", RepoLabel(ci.Owner, ci.Repo, ci.BaseSHA), ci.Skill, len(files)), ApprovalID: row.ID})
	return proposalView(row), nil
}

// sourceBySlug finds a source by its slug.
func (c *ChangeProposals) sourceBySlug(ctx context.Context, slug string) (*Source, error) {
	list, err := c.o.Admin.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Slug == slug {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("%w: skill source %s", ErrNotFound, slug)
}

// checkFiles validates the proposed files against the skill as the agent
// sees it, and returns them sorted by path, without the ones that would
// not change.
func (c *ChangeProposals) checkFiles(ctx context.Context, v Viewer, sk *Skill, src *Source, in map[string]string) ([]changeFile, error) {
	maxFile := src.Limits.MaxFileBytes
	if maxFile <= 0 {
		maxFile = DefaultMaxFileBytes
	}
	existing := map[string]FileRef{}
	for _, f := range sk.Files {
		existing[f.Path] = f
	}
	// Files toolyard does not serve (binary, too large, secret,
	// excluded) still exist in the repository.
	omitted := map[string]bool{}
	dir := repoPath(src.Path, sk.Path)
	for _, w := range sk.Omitted {
		if w.Path != "" && inFolder(dir, w.Path) && w.Path != dir {
			omitted[relTo(dir, w.Path)] = true
		}
	}
	exists := func(p string) bool {
		_, ok := existing[p]
		return ok || omitted[p]
	}
	paths := make([]string, 0, len(in))
	for p := range in {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	allowBuild := c.o.AllowBuildFiles != nil && c.o.AllowBuildFiles(ctx)
	var out []changeFile
	total := 0
	for _, p := range paths {
		content := in[p]
		if err := checkChangePath(p); err != nil {
			return nil, fmt.Errorf("%w: files: %s: %v", ErrInvalid, reviewCode(changeLine(p)), err)
		}
		// CI and build configuration runs when the branch is pushed or the
		// pull request opens, before anyone reviews it. It is refused,
		// unless it sits directly in the skill's own folder and an admin
		// allowed that (skills_change_allow_build_files).
		if full := repoPath(dir, p); changeBuildPath(full) {
			parent := path.Dir(full)
			if parent == "." {
				parent = ""
			}
			if !allowBuild || parent != dir {
				return nil, fmt.Errorf("%w: files: %s is CI or build configuration (it would run on the pull request's branch before review); change it on GitHub yourself", ErrInvalid, reviewCode(full))
			}
		}
		if content == "" {
			switch {
			case p == skillFile:
				return nil, fmt.Errorf("%w: files: SKILL.md cannot be deleted; a skill needs it", ErrInvalid)
			case !exists(p):
				return nil, fmt.Errorf("%w: files: %s does not exist in the skill, so it cannot be deleted", ErrInvalid, reviewCode(p))
			}
			f := changeFile{Path: p, Op: changeOpDelete}
			if ref, ok := existing[p]; ok {
				if cur, err := c.o.Reader.Read(ctx, v, ref.URI, 0, 0); err == nil {
					f.Old, f.HasOld = cur.Content, true
				}
			}
			out = append(out, f)
			continue
		}
		switch {
		case int64(len(content)) > maxFile:
			return nil, fmt.Errorf("%w: files: %s is %d bytes; the source allows %d per file", ErrLimit, reviewCode(p), len(content), maxFile)
		case !utf8.ValidString(content) || strings.ContainsRune(content, 0):
			return nil, fmt.Errorf("%w: files: %s must be UTF-8 text without NUL bytes", ErrInvalid, reviewCode(p))
		}
		if what := findSecret([]byte(content)); what != "" {
			return nil, fmt.Errorf("%w: files: %s looks like it holds %s; remove it before you propose the change", ErrInvalid, reviewCode(p), what)
		}
		if p == skillFile {
			if _, warns, skip := parseSkillMD(repoPath(dir, p), sk.Name, []byte(content)); skip {
				msg := "it would no longer be a valid skill"
				for _, w := range warns {
					if w.Severity == "skip" {
						msg = strings.TrimPrefix(w.Message, "skipped: ")
					}
				}
				return nil, fmt.Errorf("%w: files: SKILL.md: %s; keep YAML frontmatter with name %q and a description", ErrInvalid, msg, sk.Name)
			}
		}
		f := changeFile{Path: p, Op: changeOpAdd, Content: content}
		if exists(p) {
			f.Op = changeOpChange
		}
		if ref, ok := existing[p]; ok {
			if cur, err := c.o.Reader.Read(ctx, v, ref.URI, 0, 0); err == nil && !cur.Truncated {
				if cur.Content == content {
					continue // no change
				}
				f.Old, f.HasOld = cur.Content, true
			}
		}
		total += len(content)
		if total > MaxChangeBytes {
			return nil, fmt.Errorf("%w: files: the new contents are over %d bytes together", ErrLimit, MaxChangeBytes)
		}
		if _, _, _, ok := changeUnifiedDiff(f.Old, f.Content, changeDiffContext); !ok {
			return nil, fmt.Errorf("%w: files: the change of %s is too large to show for approval; propose a smaller change", ErrLimit, reviewCode(p))
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: nothing changes: every file already has that content", ErrInvalid)
	}
	// After the change, no file may sit where a folder is, or inside a
	// file.
	final := map[string]bool{}
	for p := range existing {
		final[p] = true
	}
	for p := range omitted {
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

// checkChangePath accepts a plain relative path inside the skill folder.
func checkChangePath(p string) error {
	switch {
	case p == "":
		return errors.New("the path is empty")
	case len(p) > 400:
		return errors.New("the path is longer than 400 bytes")
	case !utf8.ValidString(p):
		return errors.New("the path is not UTF-8")
	case strings.HasPrefix(p, "/"):
		return errors.New("the path must be relative to the skill folder, not absolute")
	case strings.Contains(p, `\`):
		return errors.New(`use "/" between folders, not "\"`)
	case path.Clean(p) != p:
		return errors.New(`the path must be plain: no "..", ".", empty folders or a trailing "/"`)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || changeInvisible(r) {
			return errors.New("the path has a control character")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == ".." || seg == ".":
			return errors.New(`the path must stay inside the skill folder (no ".." or ".")`)
		case strings.EqualFold(seg, ".git"):
			return errors.New("the path must not touch .git")
		}
	}
	return nil
}

// changeBuildNames are file names that CI services or build tools run or
// read on their own (compared in lower case).
var changeBuildNames = map[string]bool{
	".gitlab-ci.yml": true, ".travis.yml": true, ".drone.yml": true, ".drone.yaml": true, "azure-pipelines.yml": true,
	"azure-pipelines.yaml": true, "bitbucket-pipelines.yml": true, "cloudbuild.yaml": true, "cloudbuild.yml": true,
	"buildspec.yml": true, "buildspec.yaml": true, "appveyor.yml": true, ".appveyor.yml": true, "codecov.yml": true,
	".pre-commit-config.yaml": true, "jenkinsfile": true, "makefile": true, "gnumakefile": true, "justfile": true,
	"taskfile.yml": true, "taskfile.yaml": true, "dockerfile": true, "containerfile": true, "docker-compose.yml": true,
	"docker-compose.yaml": true, "compose.yml": true, "compose.yaml": true, "package.json": true, "package-lock.json": true,
	"npm-shrinkwrap.json": true, "pnpm-lock.yaml": true, "pnpm-workspace.yaml": true, ".npmrc": true, ".yarnrc": true,
	".yarnrc.yml": true, "go.mod": true, "go.sum": true, "go.work": true, "cargo.toml": true, "build.rs": true,
	"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "tox.ini": true, "noxfile.py": true, "pipfile": true,
	"gemfile": true, "rakefile": true, "build.gradle": true, "build.gradle.kts": true, "settings.gradle": true,
	"pom.xml": true, "build.xml": true, "cmakelists.txt": true, "meson.build": true, "vercel.json": true,
	"netlify.toml": true, "renovate.json": true, ".gitmodules": true, ".gitattributes": true, "codeowners": true,
}

// changeBuildDirs are folders whose files CI services run (any segment,
// lower case).
var changeBuildDirs = map[string]bool{
	".github": true, ".gitlab": true, ".circleci": true, ".buildkite": true, ".azure-pipelines": true,
	".husky": true, ".devcontainer": true, ".woodpecker": true, ".forgejo": true, ".gitea": true, ".tekton": true,
}

// changeBuildPath reports whether a repository path is CI or build
// configuration: a file in a CI folder (.github/**, .circleci/**, ...), a
// well-known build or pipeline file (Makefile, package.json,
// Dockerfile, Jenkinsfile, .gitlab-ci.yml, ...), a lock file, or a name
// that looks like a workflow or pipeline definition.
func changeBuildPath(p string) bool {
	segs := strings.Split(strings.ToLower(p), "/")
	for _, seg := range segs[:len(segs)-1] {
		if changeBuildDirs[seg] {
			return true
		}
	}
	name := segs[len(segs)-1]
	switch {
	case changeBuildNames[name],
		strings.HasSuffix(name, ".lock"),
		strings.HasPrefix(name, "dockerfile."), strings.HasSuffix(name, ".dockerfile"),
		strings.HasPrefix(name, "jenkinsfile"), strings.HasPrefix(name, "makefile."),
		strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"):
		return true
	}
	if stem, ok := strings.CutSuffix(name, ".yml"); ok || strings.HasSuffix(name, ".yaml") {
		if !ok {
			stem = strings.TrimSuffix(name, ".yaml")
		}
		if strings.Contains(stem, "workflow") || strings.Contains(stem, "pipeline") || stem == "ci" || stem == "action" ||
			strings.HasSuffix(stem, "-ci") || strings.HasSuffix(stem, "_ci") || strings.HasSuffix(stem, ".ci") {
			return true
		}
	}
	return false
}

// GetProposal returns one change proposal to the agent that sent it, or
// to the person it was sent to or the agent's owner (dashboard). A
// pending proposal whose Inbox item was decided while toolyard could not
// act is settled now; an approved one that did not finish is resumed.
func (c *ChangeProposals) GetProposal(ctx context.Context, v Viewer, id string) (*Proposal, error) {
	row, err := c.store.get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if row.Kind != proposalKindChange || !c.mine(row, v) {
		return nil, ErrNotFound
	}
	switch {
	case row.Status == proposalPending:
		if err := c.reconcile(ctx, row); err != nil {
			return nil, err
		}
	case row.Status == proposalApproved && c.o.Now().UnixMilli()-row.UpdatedAt >= changeStaleAfter.Milliseconds():
		c.resume(ctx, row)
	default:
		return proposalView(row), nil
	}
	if row, err = c.store.get(ctx, row.ID); err != nil {
		return nil, err
	}
	return proposalView(row), nil
}

func (c *ChangeProposals) mine(row *proposalRow, v Viewer) bool {
	if v.AgentID != "" {
		return row.AgentID == v.AgentID
	}
	if v.UserID == "" {
		return false
	}
	if row.UserID == v.UserID {
		return true
	}
	var ci changeInput
	return json.Unmarshal([]byte(row.Input), &ci) == nil && ci.AgentOwner == v.UserID
}

// Sweep settles what the Inbox handler could not: a pending proposal whose
// item was decided while no handler ran, expired or was withdrawn, and an
// approved proposal whose GitHub writes stopped or got an uncertain answer
// (resumed after changeStaleAfter, reconciling with GitHub first). Run it
// on one node at a time (RunInboxSweeps). One row's failure does not stop
// the others; the first error is returned.
func (c *ChangeProposals) Sweep(ctx context.Context) error {
	if c == nil {
		return nil
	}
	rows, err := c.store.pendingRows(ctx, proposalKindChange, "", inboxSweepBatch)
	if err != nil {
		return err
	}
	var first error
	for i := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.reconcile(ctx, &rows[i]); err != nil && first == nil {
			first = err
		}
	}
	stale, err := c.store.changeStaleApproved(ctx, c.o.Now().Add(-changeStaleAfter).UnixMilli(), inboxSweepBatch)
	if err != nil {
		if first == nil {
			first = err
		}
		return first
	}
	for i := range stale {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.resume(ctx, &stale[i])
	}
	return first
}

// reconcile settles a pending proposal from its Inbox item.
func (c *ChangeProposals) reconcile(ctx context.Context, row *proposalRow) error {
	if row.InboxID == "" {
		if c.o.Now().UnixMilli()-row.CreatedAt > inboxOrphanAfter.Milliseconds() {
			_, err := c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item was never created"})
			return err
		}
		return nil
	}
	item, err := c.o.Inbox.Get(ctx, row.InboxID)
	if errors.Is(err, inbox.ErrNotFound) {
		_, err = c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item is gone"})
		return err
	}
	if err != nil {
		return err
	}
	switch item.Status {
	case inbox.StatusAnswered, inbox.StatusDenied:
		if item.System != nil && item.System.Kind == ChangeProposalItemKind {
			_, _ = c.decide(ctx, item) // records its own outcome on the row
		}
	case inbox.StatusCancelled:
		_, err = c.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{Error: "the Inbox item was withdrawn"})
	case inbox.StatusExpired:
		_, err = c.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{Error: "the Inbox item expired without a decision"})
	case inbox.StatusPending:
		// Past its deadline the item can no longer be decided.
		if item.ExpiresAt > 0 && item.ExpiresAt <= c.o.Now().UnixMilli() {
			_, err = c.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{Error: "the Inbox item expired without a decision"})
		}
	}
	return err
}

// resume runs the GitHub writes of an approved proposal again: an earlier
// attempt stopped or got an uncertain answer. Only one caller wins the
// claim; the writes reconcile with GitHub first.
func (c *ChangeProposals) resume(ctx context.Context, row *proposalRow) {
	var ci changeInput
	_ = json.Unmarshal([]byte(row.Input), &ci)
	approvedAt, err := c.store.changeDecidedAt(ctx, row.ID)
	if err != nil {
		return
	}
	if approvedAt == 0 {
		approvedAt = row.CreatedAt
	}
	if c.o.Now().UnixMilli()-approvedAt > changeMaxAge.Milliseconds() || ci.Attempts >= changeMaxAttempts {
		msg := fmt.Sprintf("toolyard gave up: GitHub did not confirm the pull request in %d attempts (toolyard stops after %d attempts or a day); look for the branch %s on GitHub, and ask the agent to propose again if it is not there",
			ci.Attempts+1, changeMaxAttempts+1, ci.branchFor(row.ID))
		if ok, _ := c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg); ok {
			c.audit(ctx, audit.Event{EventType: "skills.change_proposal_fail", AgentID: row.AgentID, Reason: msg, ApprovalID: row.ID,
				DecidedByUserID: row.DecidedBy})
		}
		return
	}
	ci.Attempts++
	input, err := json.Marshal(ci)
	if err != nil {
		return
	}
	ok, err := c.store.changeClaim(ctx, row.ID, proposalApproved, row.UpdatedAt, string(input))
	if err != nil || !ok {
		return
	}
	row.Input = string(input)
	_, _ = c.finish(ctx, row)
}

// decide is the Inbox handler of change proposals.
func (c *ChangeProposals) decide(ctx context.Context, r *inbox.Request) (string, error) {
	row, err := c.store.get(ctx, r.System.Ref)
	if err != nil {
		return "", fmt.Errorf("proposal %s: %w", r.System.Ref, err)
	}
	if row.Kind != proposalKindChange {
		return "", fmt.Errorf("proposal %s: not a change proposal", row.ID)
	}
	if row.Status != proposalPending {
		return "This proposal was already settled (" + row.Status + ").", nil
	}
	decider := r.DeciderUserID
	// The GitHub account owner decides, with their own hand: only the
	// person the item went to (the source's creator, whose connection
	// writes), never another admin, a grant or an automatic rule.
	if err := checkDecider(ctx, nil, decider, row.UserID); err != nil {
		_, _ = c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: err.Error()})
		return "", err
	}
	if r.SystemChoice(reviewQuestionID) != reviewApprove {
		why := "rejected by the source's owner"
		if note := strings.TrimSpace(r.OwnerNote); note != "" {
			why += ": " + reviewClip(note, 500)
		}
		if ok, err := c.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{DecidedBy: decider, Error: why}); err != nil || !ok {
			return "This proposal was already settled.", err
		}
		c.audit(ctx, audit.Event{EventType: "skills.change_proposal_reject", AgentID: row.AgentID, Reason: r.OwnerNote,
			ApprovalID: row.ID, DecidedByUserID: decider})
		return "Proposal rejected; nothing was sent to GitHub.", nil
	}
	// Only the owner in person may approve a write with their GitHub
	// account: the dashboard, their own notification, or a passkey (the
	// set the approval bus accepts for a personal GitHub write, without
	// the batch click). Anything else, an empty or unknown instrument
	// included, fails closed. The Inbox refuses these before they are
	// recorded (HumanOnly); this is the second check.
	switch r.DeciderVia {
	case actor.ViaDashboard, actor.ViaPushToken, actor.ViaPasskey:
	default:
		err := errors.New("a GitHub write needs the account owner's own decision in the dashboard, from their notification, or with a passkey")
		_, _ = c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: err.Error()})
		return "", err
	}
	if ok, err := c.store.move(ctx, row.ID, []string{proposalPending}, proposalApproved, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
		return "This proposal was already settled.", err
	}
	row.DecidedBy = decider
	return c.finish(ctx, row)
}

// finish runs the GitHub writes of an approved row and records the
// outcome: done with the pull request URL, failed with the reason, or
// still approved when GitHub's answer was uncertain.
func (c *ChangeProposals) finish(ctx context.Context, row *proposalRow) (string, error) {
	actx, cancel := context.WithTimeout(ctx, changeApplyTimeout)
	defer cancel()
	pr, err := c.apply(actx, row)
	if err == nil {
		if _, err := c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalDone, pr, ""); err != nil {
			return "", err
		}
		c.audit(ctx, audit.Event{EventType: "skills.change_pull_request", AgentID: row.AgentID, Reason: "approved proposal " + row.ID,
			ResultSummary: pr, ApprovalID: row.ID, DecidedByUserID: row.DecidedBy})
		return "Opened the pull request " + pr + ".", nil
	}
	msg := reviewClip(err.Error(), 500)
	var ce *changeError
	if errors.As(err, &ce) && ce.uncertain {
		msg = reviewClip(err.Error()+"; toolyard checks GitHub for the branch and the pull request before it writes again (within a few minutes)", 500)
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalApproved, "", msg)
		return "", errors.New(msg)
	}
	_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg)
	c.audit(ctx, audit.Event{EventType: "skills.change_proposal_fail", AgentID: row.AgentID, Reason: msg, ApprovalID: row.ID,
		DecidedByUserID: row.DecidedBy})
	return "", errors.New(msg)
}

func (c *ChangeProposals) agentName(ctx context.Context, agentID string) string {
	if c.o.AgentName != nil {
		if n := strings.TrimSpace(c.o.AgentName(ctx, agentID)); n != "" {
			return changeLine(n)
		}
	}
	return "agent " + agentID
}

func (c *ChangeProposals) userName(ctx context.Context, userID string) string {
	if c.o.UserName != nil && userID != "" {
		if n := strings.TrimSpace(c.o.UserName(ctx, userID)); n != "" {
			return changeLine(n)
		}
	}
	return userID
}

func (c *ChangeProposals) audit(ctx context.Context, ev audit.Event) {
	if c.o.Audit != nil {
		_ = c.o.Audit.Write(ctx, ev)
	}
}

// changeClaim moves a row's updated_at to now and stores input (with its
// attempt count) when the row is still in status with updatedAt; it
// reports whether this caller won.
func (st proposalStore) changeClaim(ctx context.Context, id, status string, updatedAt int64, input string) (bool, error) {
	res, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET updated_at = ?, input = ? WHERE id = ? AND status = ? AND updated_at = ?`,
		st.now().UnixMilli(), input, id, status, updatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// changeDecidedAt is when a row was decided (Unix ms), 0 when unknown.
func (st proposalStore) changeDecidedAt(ctx context.Context, id string) (int64, error) {
	var at sql.NullInt64
	if err := st.db.QueryRowContext(ctx, `SELECT decided_at FROM skill_proposals WHERE id = ?`, id).Scan(&at); err != nil {
		return 0, err
	}
	return at.Int64, nil
}

// changeStaleApproved lists change rows still approved (being applied)
// whose last update is older than before.
func (st proposalStore) changeStaleApproved(ctx context.Context, before int64, limit int) ([]proposalRow, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE kind = ? AND status = ? AND updated_at < ? ORDER BY updated_at LIMIT ?`,
		proposalKindChange, proposalApproved, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proposalRow
	for rows.Next() {
		r, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// changeSetOutcome sets a row's status, result and error (both may be
// empty) when it is in status from.
func (st proposalStore) changeSetOutcome(ctx context.Context, id, from, to, result, errText string) (bool, error) {
	res, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET status = ?, result = ?, error = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, result, errText, st.now().UnixMilli(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

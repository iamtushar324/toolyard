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
// blindly: the proposal stays approved, and the next attempt (when the
// agent polls it) looks for the branch and the pull request first.

import (
	"context"
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
	// and body the agent writes (characters).
	MaxChangeTitle   = 200
	MaxChangeSummary = 10000
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
	o.Inbox.HandleSystem(ChangeProposalItemKind, c.decide)
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
	ci := changeInput{Skill: sk.URI, SkillPath: sk.Path, Name: sk.Name, Slug: src.Slug, Owner: src.Owner, Repo: src.Repo,
		Branch: src.Branch, Folder: src.Path, BaseSHA: sk.Commit, AgentOwner: v.UserID, Title: title, Summary: summary, Files: files}
	stored, err := json.Marshal(ci)
	if err != nil {
		return nil, err
	}
	row := &proposalRow{ID: proposalNewID(), Kind: proposalKindChange, Status: proposalPending, AgentID: v.AgentID,
		UserID: src.CreatedBy, SourceID: src.ID, Input: string(stored), Reason: summary}
	if src.ActiveSHA == sk.Commit {
		row.VersionID = src.ActiveVersionID
	}
	if err := c.store.insert(ctx, row); err != nil {
		return nil, err
	}
	agent := c.agentName(ctx, v.AgentID)
	item, err := c.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ChangeProposalItemKind, Ref: row.ID, UserID: src.CreatedBy,
		Title:     reviewClip("Skill change: "+title, inbox.MaxTitle),
		Summary:   reviewClip(changeItemSummary(agent, &ci), inbox.MaxSummary),
		Message:   changeMessage(agent, c.userName(ctx, v.UserID), row.ID, &ci),
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
	var out []changeFile
	total := 0
	for _, p := range paths {
		content := in[p]
		if err := checkChangePath(p); err != nil {
			return nil, fmt.Errorf("%w: files: %s: %v", ErrInvalid, reviewCode(changeLine(p)), err)
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
	case row.Status == proposalPending && row.InboxID != "":
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

// reconcile settles a pending proposal from its Inbox item.
func (c *ChangeProposals) reconcile(ctx context.Context, row *proposalRow) error {
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
	}
	return err
}

// resume runs the GitHub writes of an approved proposal again: an earlier
// attempt stopped or got an uncertain answer. Only one caller wins the
// claim; the writes reconcile with GitHub first.
func (c *ChangeProposals) resume(ctx context.Context, row *proposalRow) {
	ok, err := c.store.changeClaim(ctx, row.ID, proposalApproved, row.UpdatedAt)
	if err != nil || !ok {
		return
	}
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
	switch r.DeciderVia {
	case actor.ViaAutoRule, actor.ViaPolicy, actor.ViaInboxGrant, actor.ViaAgentCancel, actor.ViaExpiry:
		err := errors.New("a GitHub write needs the account owner's own decision, not an automatic one")
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
		return "Opened the pull request " + pr + " (branch " + ChangeBranchPrefix + row.ID + ").", nil
	}
	msg := reviewClip(err.Error(), 500)
	var ce *changeError
	if errors.As(err, &ce) && ce.uncertain {
		msg = reviewClip(err.Error()+"; toolyard checks GitHub for the branch and the pull request before it writes again (when the agent polls this proposal)", 500)
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

// changeClaim moves a row's updated_at to now when it is still in status
// with updatedAt; it reports whether this caller won.
func (st proposalStore) changeClaim(ctx context.Context, id, status string, updatedAt int64) (bool, error) {
	res, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET updated_at = ? WHERE id = ? AND status = ? AND updated_at = ?`,
		st.now().UnixMilli(), id, status, updatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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

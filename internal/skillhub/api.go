// Package skillhub serves Agent Skills (https://agentskills.io/specification)
// from GitHub repositories ("skill sources") to every toolyard client: MCP
// tools, code mode, REST and the MCP Skills extension (SEP-2640).
//
// toolyard copies each source's skills at an exact commit into its database
// (a "version"), keeps that copy current from a leader loop and GitHub push
// webhooks, and serves only the copy: agents never query GitHub themselves.
// See docs/design/skills-hub.md for the design and docs/skills-hub.md for
// the guide.
//
// This file holds the shared types and interfaces. *Service implements
// Reader and Admin; the gateway (agent surface) and the API (admin surface)
// program against the interfaces.
package skillhub

import (
	"context"
	"errors"
	"io"
	"time"
)

// UpdateMode says what a new commit on a source's branch does.
type UpdateMode string

const (
	// ModeAuto validates a new commit and makes it the active version.
	ModeAuto UpdateMode = "auto"
	// ModeReview stages a new commit; an approval makes it active.
	ModeReview UpdateMode = "review"
	// ModePinned ignores new commits and serves PinnedSHA.
	ModePinned UpdateMode = "pinned"
)

// AuthMode says how toolyard reads a source's repository.
type AuthMode string

const (
	// AuthNone reads a public repository without credentials.
	AuthNone AuthMode = "none"
	// AuthToken reads with a read-only token sealed in toolyard.
	AuthToken AuthMode = "token"
	// AuthGitHub reads with the source creator's toolyard GitHub connection.
	AuthGitHub AuthMode = "github"
)

// Visibility is the audience of a source, or of one skill (SkillSharing).
// Admins and the owner see every skill whose audience is not nobody; an
// agent sees what its owner sees. See the sharing rule in index.go
// (srcIndex.skillVisible).
type Visibility string

const (
	// VisibleEveryone shows the skills to every active member.
	VisibleEveryone Visibility = "everyone"
	// VisibleMembers shows the skills only to the listed members.
	VisibleMembers Visibility = "members"
	// VisibleNobody hides the skills from every agent: only admins and the
	// source's creator see them, in the dashboard.
	VisibleNobody Visibility = "nobody"
	// VisibleInherit is a skill's own setting that follows its source's
	// audience (SkillSharing.Mode).
	VisibleInherit Visibility = ""
)

// Approval is where a skill contributor's source stands with the admins.
// Agents see nothing of a source that is pending or rejected.
type Approval string

const (
	// ApprovalNone is a source an admin added: nothing to approve.
	ApprovalNone Approval = ""
	// ApprovalPending is a contributor's source that waits for an admin:
	// it syncs, and agents see nothing of it.
	ApprovalPending Approval = "pending"
	// ApprovalApproved is a contributor's source an admin approved.
	ApprovalApproved Approval = "approved"
	// ApprovalRejected is a contributor's source an admin rejected; it is
	// disabled.
	ApprovalRejected Approval = "rejected"
)

// Live reports whether agents may see a source in this approval state.
func (a Approval) Live() bool { return a == ApprovalNone || a == ApprovalApproved }

// Runs says where a skill can be followed.
type Runs string

const (
	// RunsAnywhere skills work in any client.
	RunsAnywhere Runs = "anywhere"
	// RunsHost skills need the T3 host (its paths and tools); callers that
	// are not on the host see them only when they ask for host skills.
	RunsHost Runs = "host"
)

// VersionStatus is the state of one version of a source.
type VersionStatus string

const (
	VersionStaged   VersionStatus = "staged"
	VersionActive   VersionStatus = "active"
	VersionRetired  VersionStatus = "retired"
	VersionRejected VersionStatus = "rejected"
	VersionFailed   VersionStatus = "failed"
)

// Limits caps what one source may hold. A zero field means the default.
type Limits struct {
	MaxFileBytes  int64 `json:"max_file_bytes,omitempty"`  // default 1 MiB
	MaxTotalBytes int64 `json:"max_total_bytes,omitempty"` // default 50 MB
	MaxSkills     int   `json:"max_skills,omitempty"`      // default 2,000
	MaxFiles      int   `json:"max_files,omitempty"`       // default 20,000
}

// Default limits.
const (
	DefaultMaxFileBytes  int64 = 1 << 20
	DefaultMaxTotalBytes int64 = 50_000_000
	DefaultMaxSkills           = 2000
	DefaultMaxFiles            = 20000
	DefaultPollSeconds         = 300
	MinPollSeconds             = 60
	KeepVersions               = 10
)

// Source is one repository + branch (+ folder) that holds skills.
type Source struct {
	ID          string     `json:"id"`   // "src_<hex>"
	Slug        string     `json:"slug"` // URI prefix: skill://<slug>/...
	Kind        string     `json:"kind"` // "github"
	Owner       string     `json:"owner"`
	Repo        string     `json:"repo"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"` // folder in the repository; "" is the root
	Exclude     []string   `json:"exclude,omitempty"`
	Mode        UpdateMode `json:"mode"`
	PinnedSHA   string     `json:"pinned_sha,omitempty"`
	Auth        AuthMode   `json:"auth"`
	HasToken    bool       `json:"has_token"` // the token itself is never returned
	PollSeconds int        `json:"poll_seconds"`
	Enabled     bool       `json:"enabled"`
	Visibility  Visibility `json:"visibility"`
	Members     []string   `json:"members,omitempty"` // user IDs when Visibility is members
	// Blocked are user IDs who never see the source's skills, whatever
	// the audience (admins excepted).
	Blocked     []string `json:"blocked,omitempty"`
	DefaultRuns Runs     `json:"default_runs"`
	Limits      Limits   `json:"limits"`

	// Hidden is an admin's lock: no agent sees any of the source's skills,
	// whatever its audience or its skills' own settings. Only an admin can
	// set or clear it.
	Hidden bool `json:"hidden,omitempty"`

	// Approval is a contributor's source's state with the admins ("" for
	// a source an admin added); ApprovalNote is a rejection's reason and
	// ApprovalBy the deciding admin.
	Approval     Approval `json:"approval,omitempty"`
	ApprovalNote string   `json:"approval_note,omitempty"`
	ApprovalBy   string   `json:"approval_by,omitempty"`

	CreatedBy string    `json:"created_by"` // user ID
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	ActiveVersionID  string     `json:"active_version_id,omitempty"`
	ActiveSHA        string     `json:"active_sha,omitempty"`
	PendingVersionID string     `json:"pending_version_id,omitempty"` // staged, waiting for review
	LastCheckedAt    *time.Time `json:"last_checked_at,omitempty"`
	NextCheckAt      *time.Time `json:"next_check_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	SkillCount       int        `json:"skill_count"`
	WarningCount     int        `json:"warning_count"`
	WebhookURLPath   string     `json:"webhook_url_path,omitempty"` // "/v1/skills/sources/<id>/webhook"
	HasWebhookSecret bool       `json:"has_webhook_secret"`
}

// RepoLabel is "owner/repo@<12-hex commit>", the version label every answer
// carries.
func RepoLabel(owner, repo, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if sha == "" {
		return owner + "/" + repo
	}
	return owner + "/" + repo + "@" + sha
}

// SourceInput creates a source, or (with Preview) describes one to try.
// Empty fields take their defaults.
type SourceInput struct {
	URL         string     `json:"url"` // owner/repo, repo URL, or a /tree/<branch>/<path> URL
	Slug        string     `json:"slug,omitempty"`
	Branch      string     `json:"branch,omitempty"` // default: the repository's default branch, or the URL's
	Path        string     `json:"path,omitempty"`   // default: the URL's path, or the root
	Exclude     []string   `json:"exclude,omitempty"`
	Mode        UpdateMode `json:"mode,omitempty"` // default: auto for trusted owners, else review
	PinnedSHA   string     `json:"pinned_sha,omitempty"`
	Auth        AuthMode   `json:"auth,omitempty"`  // default: token when Token is set, else none
	Token       string     `json:"token,omitempty"` // write-only
	PollSeconds int        `json:"poll_seconds,omitempty"`
	Enabled     *bool      `json:"enabled,omitempty"` // default true
	Visibility  Visibility `json:"visibility,omitempty"`
	Members     []string   `json:"members,omitempty"`
	Blocked     []string   `json:"blocked,omitempty"`
	DefaultRuns Runs       `json:"default_runs,omitempty"`
	Limits      *Limits    `json:"limits,omitempty"`
	// PendingApproval starts the source pending an admin's approval (a
	// skill contributor's source). It is never read from JSON: the API
	// sets it from who is asking.
	PendingApproval bool `json:"-"`
}

// SourceUpdate changes a source. Nil fields stay as they are. Token "" with
// ClearToken false keeps the stored token.
type SourceUpdate struct {
	Branch      *string     `json:"branch,omitempty"`
	Path        *string     `json:"path,omitempty"`
	Exclude     *[]string   `json:"exclude,omitempty"`
	Mode        *UpdateMode `json:"mode,omitempty"`
	PinnedSHA   *string     `json:"pinned_sha,omitempty"`
	Auth        *AuthMode   `json:"auth,omitempty"`
	Token       *string     `json:"token,omitempty"`
	ClearToken  bool        `json:"clear_token,omitempty"`
	PollSeconds *int        `json:"poll_seconds,omitempty"`
	Enabled     *bool       `json:"enabled,omitempty"`
	Visibility  *Visibility `json:"visibility,omitempty"`
	Members     *[]string   `json:"members,omitempty"`
	Blocked     *[]string   `json:"blocked,omitempty"`
	Hidden      *bool       `json:"hidden,omitempty"` // admins only (the API refuses it from anyone else)
	DefaultRuns *Runs       `json:"default_runs,omitempty"`
	Limits      *Limits     `json:"limits,omitempty"`
}

// RepoRef is a parsed GitHub URL.
type RepoRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"` // from a /tree/ or /blob/ URL
	Path   string `json:"path,omitempty"`   // from a /tree/ or /blob/ URL
}

// RepoInfo is what Inspect learns about a repository.
type RepoInfo struct {
	RepoRef
	Private       bool     `json:"private"`
	DefaultBranch string   `json:"default_branch"`
	Branches      []string `json:"branches"`
	BranchesMore  bool     `json:"branches_more,omitempty"` // more than the cap
	NeedsToken    bool     `json:"needs_token"`             // not readable without a token
	HTMLURL       string   `json:"html_url"`
}

// Warning is one problem found while building a version. Severity "skip"
// means the skill or file was not served; "warn" means it was served.
type Warning struct {
	Path     string `json:"path"` // repository-relative path
	Code     string `json:"code"` // name_mismatch, missing_field, description_too_long, binary, too_large, secret, excluded, invalid_yaml, ...
	Message  string `json:"message"`
	Severity string `json:"severity"` // "skip" | "warn"
}

// Diff compares a version with the active one.
type Diff struct {
	FromSHA string   `json:"from_sha,omitempty"`
	ToSHA   string   `json:"to_sha"`
	Added   []string `json:"added"`   // skill paths
	Changed []string `json:"changed"` // skill paths
	Removed []string `json:"removed"` // skill paths
}

// Version is the skills of a source at one commit.
type Version struct {
	ID          string        `json:"id"` // "skv_<hex>"
	SourceID    string        `json:"source_id"`
	SHA         string        `json:"sha"`
	Status      VersionStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	ActivatedAt *time.Time    `json:"activated_at,omitempty"`
	DecidedBy   string        `json:"decided_by,omitempty"` // user ID of an activation, rejection or approval
	Note        string        `json:"note,omitempty"`
	SkillCount  int           `json:"skill_count"`
	FileCount   int           `json:"file_count"`
	TotalBytes  int64         `json:"total_bytes"`
	Warnings    []Warning     `json:"warnings,omitempty"`
	Diff        *Diff         `json:"diff,omitempty"`
	Error       string        `json:"error,omitempty"` // for a failed version
}

// Preview is a dry run of a source: nothing is stored.
type Preview struct {
	Source     Source         `json:"source"` // the source as it would be created
	SHA        string         `json:"sha"`
	SkillCount int            `json:"skill_count"`
	FileCount  int            `json:"file_count"`
	TotalBytes int64          `json:"total_bytes"`
	Warnings   []Warning      `json:"warnings"`
	Sample     []SkillSummary `json:"sample"` // up to 20
}

// SyncResult reports one sync.
type SyncResult struct {
	SourceID  string `json:"source_id"`
	SHA       string `json:"sha"`
	Unchanged bool   `json:"unchanged"` // the branch head did not move
	VersionID string `json:"version_id,omitempty"`
	Activated bool   `json:"activated"`
	Staged    bool   `json:"staged"` // waiting for review
	Full      bool   `json:"full"`   // full download (else incremental)
	Diff      *Diff  `json:"diff,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SkillUsage is the reads of one skill.
type SkillUsage struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Runs         Runs   `json:"runs"`
	RunsOverride Runs   `json:"runs_override,omitempty"`
	Reads30d     int    `json:"reads_30d"`
	// Sharing is the skill's own sharing setting; nil when it follows the
	// source entirely.
	Sharing *SkillSharing `json:"sharing,omitempty"`
}

// SkillSharing is one skill's own sharing setting, kept by source and skill
// path so it survives new versions. Mode VisibleInherit ("") follows the
// source's audience; everyone, members (Members) or nobody replace it for
// this skill. Blocked always adds to the source's block list.
type SkillSharing struct {
	Mode    Visibility `json:"mode"`
	Members []string   `json:"members,omitempty"`
	Blocked []string   `json:"blocked,omitempty"`
}

// IsZero reports a setting that follows the source entirely.
func (sh SkillSharing) IsZero() bool { return sh.Mode == VisibleInherit && len(sh.Blocked) == 0 }

// Contributor is a member an admin allowed to add skill sources.
type Contributor struct {
	UserID  string    `json:"user_id"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
}

// Viewer is who asks. The gateway and the API build it from the caller.
type Viewer struct {
	UserID  string // owner user of the calling agent, or the signed-in user
	AgentID string // "" for a dashboard user
	Admin   bool   // owner or admin role: sees every source
	// OnHost is true for callers that run on the T3 host (host-connection
	// and federation agents). Others see RunsHost skills only with
	// SearchQuery.IncludeHost / ListQuery.IncludeHost.
	OnHost bool
	// Delegated is every caller but a person in their own dashboard page:
	// an agent, a caller that acts for the person (an operator token's
	// CLI, the voice assistant) and a caller with no ID at all. The sharing
	// rule treats it as an agent, so it never sees audience nobody.
	Delegated bool
}

// SkillSummary is one skill in a search result or listing.
type SkillSummary struct {
	URI         string  `json:"uri"` // skill://<slug>/<path>/SKILL.md
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Source      string  `json:"source"` // slug
	Path        string  `json:"path"`   // skill path in the source
	Repo        string  `json:"repo"`   // owner/repo@<12-hex>
	Commit      string  `json:"commit"` // full SHA
	Runs        Runs    `json:"runs"`
	Score       float64 `json:"score,omitempty"`
}

// FileRef is one file of a skill (SEP-2640 resources entry).
type FileRef struct {
	URI    string `json:"uri"`
	Path   string `json:"path"`   // relative to the skill folder
	Digest string `json:"digest"` // "sha256:<64 hex>"
	Size   int64  `json:"size"`
	Type   string `json:"mime_type,omitempty"`
}

// Skill is one skill with its SKILL.md and file list.
type Skill struct {
	SkillSummary
	Frontmatter map[string]any `json:"frontmatter"` // verbatim, as JSON
	Body        string         `json:"body"`        // SKILL.md after the frontmatter
	Raw         string         `json:"-"`           // SKILL.md as stored
	Files       []FileRef      `json:"files,omitempty"`
	Omitted     []Warning      `json:"omitted,omitempty"` // files not served, and why
}

// File is the content of one skill file.
type File struct {
	FileRef
	Skill      string `json:"skill"` // skill URI
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	Content    string `json:"content"`
	Offset     int    `json:"offset,omitempty"` // first line returned (0-based)
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// DirEntry is one child of a skill folder (resources/directory/read).
type DirEntry struct {
	URI   string `json:"uri"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

// SearchQuery searches names, descriptions and paths.
type SearchQuery struct {
	Text        string `json:"query"`
	Mode        string `json:"mode,omitempty"` // "keyword" (default) | "regex"
	Source      string `json:"source,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	IncludeHost bool   `json:"include_host,omitempty"`
}

// GrepQuery searches file contents.
type GrepQuery struct {
	Pattern     string `json:"pattern"` // RE2
	Source      string `json:"source,omitempty"`
	PathGlob    string `json:"path,omitempty"`
	IgnoreCase  bool   `json:"ignore_case,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	IncludeHost bool   `json:"include_host,omitempty"`
}

// GrepHit is one matching line.
type GrepHit struct {
	URI    string `json:"uri"`
	Skill  string `json:"skill"`
	Line   int    `json:"line"` // 1-based
	Text   string `json:"text"`
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// ListQuery pages through the visible skills (SEP-2640 skills/list).
type ListQuery struct {
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"` // default 100, max 500
	Source      string `json:"source,omitempty"`
	IncludeHost bool   `json:"include_host,omitempty"`
}

// SourceInfo is a source as agents see it.
type SourceInfo struct {
	Slug       string    `json:"slug"`
	Repo       string    `json:"repo"` // owner/repo@<12-hex>
	Branch     string    `json:"branch"`
	Path       string    `json:"path,omitempty"`
	Commit     string    `json:"commit"`
	SyncedAt   time.Time `json:"synced_at"`
	SkillCount int       `json:"skill_count"`
	HTMLURL    string    `json:"html_url"`
}

// Errors.
var (
	ErrNotFound     = errors.New("skillhub: not found")
	ErrInvalid      = errors.New("skillhub: invalid input")
	ErrConflict     = errors.New("skillhub: conflict")
	ErrNeedsToken   = errors.New("skillhub: the repository is private or missing; a read-only token is needed")
	ErrLimit        = errors.New("skillhub: a source limit was exceeded")
	ErrBadSignature = errors.New("skillhub: webhook signature does not match")
	ErrDisabled     = errors.New("skillhub: the skills hub is off")
)

// Reader is what agents use. Every method shows only what the viewer may
// see; an invisible skill is ErrNotFound.
type Reader interface {
	Sources(ctx context.Context, v Viewer) ([]SourceInfo, error)
	Search(ctx context.Context, v Viewer, q SearchQuery) ([]SkillSummary, error)
	// Get takes a URI, "slug/skill-path", or a bare name when it is unique
	// among the visible skills (ErrConflict when it is not).
	Get(ctx context.Context, v Viewer, ref string, includeFiles bool) (*Skill, error)
	// Read takes a file URI; offset and limit are lines (limit 0: all, up
	// to the file).
	Read(ctx context.Context, v Viewer, uri string, offset, limit int) (*File, error)
	ListDir(ctx context.Context, v Viewer, dirURI string) ([]DirEntry, error)
	Grep(ctx context.Context, v Viewer, q GrepQuery) ([]GrepHit, error)
	// List returns entries with their complete file lists and the next
	// cursor ("" at the end).
	List(ctx context.Context, v Viewer, q ListQuery) ([]Skill, string, error)
	// Bundle writes a .tar.gz of the skills (Agent Skills layout).
	Bundle(ctx context.Context, v Viewer, refs []string, w io.Writer) error
	// Subscribe calls fn with a source slug after its active version
	// changed on this or another node.
	Subscribe(fn func(slug string)) (cancel func())
}

// Admin is what admins use. by is the acting user ID.
type Admin interface {
	ParseURL(raw string) (RepoRef, error)
	Inspect(ctx context.Context, url, token string) (*RepoInfo, error)
	Preview(ctx context.Context, by string, in SourceInput) (*Preview, error)
	CreateSource(ctx context.Context, by string, in SourceInput) (*Source, error)
	UpdateSource(ctx context.Context, by, id string, in SourceUpdate) (*Source, error)
	DeleteSource(ctx context.Context, by, id string) error
	GetSource(ctx context.Context, id string) (*Source, error)
	ListSources(ctx context.Context) ([]Source, error)
	SyncNow(ctx context.Context, id string) (*SyncResult, error)
	Versions(ctx context.Context, id string, limit int) ([]Version, error)
	GetVersion(ctx context.Context, id, versionID string) (*Version, error)
	// Activate makes a staged or retired version active (approval or
	// rollback).
	Activate(ctx context.Context, by, id, versionID string) error
	Reject(ctx context.Context, by, id, versionID, note string) error
	Skills(ctx context.Context, id string) ([]SkillUsage, error)
	// SetRuns overrides where one skill runs; "" clears the override.
	SetRuns(ctx context.Context, by, id, skillPath string, runs Runs) error
	// RotateWebhookSecret returns the new secret once.
	RotateWebhookSecret(ctx context.Context, by, id string) (string, error)
	HandleWebhook(ctx context.Context, id, event string, body []byte, signature string) error
	SetReviewHook(h ReviewHook)
	// SetTokenSource lets AuthGitHub sources read with their creator's
	// GitHub connection (slice D).
	SetTokenSource(ts TokenSource)
	// TrustedOwner reports whether a GitHub owner is in the setting
	// skills_trusted_github_owners (default mode auto).
	TrustedOwner(ctx context.Context, owner string) bool
}

// SharingAdmin is the per-skill sharing and the source approval part of
// the admin service (*Service implements it).
type SharingAdmin interface {
	// SetSkillSharing sets one skill's own sharing (a zero setting clears
	// it) and returns the setting it replaced. Every node applies it
	// within moments.
	SetSkillSharing(ctx context.Context, by, id, skillPath string, sh SkillSharing) (SkillSharing, error)
	// DecideApproval approves or rejects (the source is disabled, with
	// note) a contributor's source that waits for approval. Approving
	// makes the source live; its waiting version goes live with it only
	// when that version is at sha, the commit the approver saw (else it
	// waits for a normal review). Deciding a source that needs no
	// approval is ErrConflict.
	DecideApproval(ctx context.Context, by, id string, approve bool, note, sha string) (*Source, error)
	// PreviewSource is a dry run of a stored source with its own settings
	// and credential (a new approval question after a contributor changed
	// what is fetched).
	PreviewSource(ctx context.Context, id string) (*Preview, error)
	// InspectAnonymous is Inspect without any token, the default one
	// included: what a skill contributor without their own credential may
	// read (public repositories only).
	InspectAnonymous(ctx context.Context, url string) (*RepoInfo, error)
}

// ContributorsAdmin keeps the skill contributors (*Service implements it).
type ContributorsAdmin interface {
	Contributors(ctx context.Context) ([]Contributor, error)
	IsContributor(ctx context.Context, userID string) (bool, error)
	// SetContributor adds (on) or removes a contributor; both are
	// idempotent.
	SetContributor(ctx context.Context, by, userID string, on bool) error
}

// ReviewHook is called when a review-mode source has a new staged version.
type ReviewHook interface {
	PendingVersion(ctx context.Context, src Source, v Version) error
}

// TokenSource returns a GitHub token that can read a repository for a user.
type TokenSource func(ctx context.Context, userID string) (string, error)

// Proposal is an agent's request that waits for its owner in the Inbox.
type Proposal struct {
	ID        string    `json:"id"`     // "skp_<hex>"
	Kind      string    `json:"kind"`   // "source" | "change"
	Status    string    `json:"status"` // pending | approved | rejected | failed | done
	InboxID   string    `json:"inbox_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Result    string    `json:"result,omitempty"` // source ID, or pull request URL
	Error     string    `json:"error,omitempty"`
}

// ChangeInput is a proposed edit of one skill.
type ChangeInput struct {
	Skill   string            `json:"skill"` // URI or slug/skill-path
	Files   map[string]string `json:"files"` // path in the skill -> new content; "" deletes
	Title   string            `json:"title"`
	Summary string            `json:"summary"`
}

// Proposals takes agents' proposals (skills.propose_source and
// skills.propose_change).
type Proposals interface {
	ProposeSource(ctx context.Context, v Viewer, in SourceInput, reason string) (*Proposal, error)
	ProposeChange(ctx context.Context, v Viewer, in ChangeInput) (*Proposal, error)
	GetProposal(ctx context.Context, v Viewer, id string) (*Proposal, error)
}

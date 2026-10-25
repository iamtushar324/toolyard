package skillhub

// ReviewInbox is the ReviewHook: a review-mode source's new staged version
// becomes one Inbox question for the source's creator ("Make this version
// live?"), and the answer calls Admin.Activate or Admin.Reject. The Inbox
// carries the decision natively (inbox system items, internal/inbox
// system.go); the dashboard's Approve and Reject buttons on the source
// page decide the same version and close the open item (Settle). An answer
// applies only while its version is still the source's pending version, so
// an outdated question can never roll back a newer live commit. Sweep
// (review_sweep.go) settles what the Inbox handler could not.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// ReviewItemKind is the Inbox system item kind of a version review.
const ReviewItemKind = "skillhub.review"

// reviewQuestionID is the ID of the one question review and proposal
// items ask; its options are reviewApprove and reviewReject.
const (
	reviewQuestionID = "decision"
	reviewApprove    = "approve"
	reviewReject     = "reject"
	// reviewListCap is how many skill paths one list of an item names.
	reviewListCap = 20
)

// ReviewOptions configure a ReviewInbox.
type ReviewOptions struct {
	DB    *store.DB
	Inbox *inbox.Service
	Admin Admin
	// IsAdmin reports whether a user holds the admin role (and is active).
	// A decision by anyone else is refused (the creator may have lost the
	// role since), and a review goes to FallbackReviewer instead of a
	// creator who is no admin. nil skips both checks.
	IsAdmin func(ctx context.Context, userID string) bool
	// FallbackReviewer names who gets a review when the source's creator
	// can no longer decide it (no longer an admin, or blocked): the owner.
	// nil, or an error, leaves the review with the creator.
	FallbackReviewer func(ctx context.Context) (string, error)
	// DashboardURL is toolyard's public base URL; when set, items link to
	// the source page (<DashboardURL>/#skills/<source id>).
	DashboardURL string
	// Audit, when set, records every applied decision.
	Audit *audit.Logger
	Now   func() time.Time
}

// ReviewInbox implements ReviewHook with Inbox items. Create it with
// NewReviewInbox and give it to Admin.SetReviewHook.
type ReviewInbox struct {
	o     ReviewOptions
	store proposalStore
}

var _ ReviewHook = (*ReviewInbox)(nil)

// NewReviewInbox registers the review item handler with the Inbox.
func NewReviewInbox(o ReviewOptions) (*ReviewInbox, error) {
	if o.DB == nil || o.Inbox == nil || o.Admin == nil {
		return nil, errors.New("skillhub: review inbox needs a database, the Inbox and the admin service")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ri := &ReviewInbox{o: o, store: proposalStore{db: o.DB, now: o.Now}}
	o.Inbox.HandleSystem(ReviewItemKind, ri.decide)
	return ri, nil
}

// reviewInput is what a review row keeps of the version it asks about.
type reviewInput struct {
	SHA      string `json:"sha"`
	FromSHA  string `json:"from_sha,omitempty"`
	Added    int    `json:"added"`
	Changed  int    `json:"changed"`
	Removed  int    `json:"removed"`
	Warnings int    `json:"warnings"`
}

// PendingVersion puts one review item for v in the Inbox of src's creator,
// or of the fallback reviewer when the creator can no longer decide it.
// An open item for an older staged version of the same source is
// cancelled first: only the newest commit waits for review.
//
// The sync engine calls it again at every check while v waits, so it is
// idempotent: a version whose question is open (or whose answer is being
// applied) adds nothing. A version whose earlier question expired, or whose
// answer could not be applied, gets a new question; a call whose Inbox item
// could not be created leaves no row, so the next check simply tries again.
func (ri *ReviewInbox) PendingVersion(ctx context.Context, src Source, v Version) error {
	reviewer := ri.reviewer(ctx, src.CreatedBy)
	if reviewer == "" {
		return fmt.Errorf("%w: source %s has no creator to review it", ErrInvalid, src.ID)
	}
	if v.ID == "" || v.SHA == "" {
		return fmt.Errorf("%w: the version has no ID or commit", ErrInvalid)
	}
	if asked, err := ri.store.openReviewFor(ctx, src.ID, v.ID); err != nil || asked {
		return err
	}
	open, err := ri.store.pendingReviews(ctx, src.ID, "")
	if err != nil {
		return err
	}
	if err := ri.settleRows(ctx, open, "a newer commit is waiting for review"); err != nil {
		return err
	}
	diff := v.Diff
	if diff == nil {
		diff = &Diff{ToSHA: v.SHA}
	}
	in, _ := json.Marshal(reviewInput{SHA: v.SHA, FromSHA: diff.FromSHA, Added: len(diff.Added), Changed: len(diff.Changed),
		Removed: len(diff.Removed), Warnings: len(v.Warnings)})
	row := &proposalRow{ID: proposalNewID(), Kind: proposalKindReview, Status: proposalPending, UserID: reviewer,
		SourceID: src.ID, VersionID: v.ID, Input: string(in)}
	if err := ri.store.insert(ctx, row); err != nil {
		return err
	}
	msg := reviewMessage(src, v, diff, ri.o.DashboardURL)
	if reviewer != src.CreatedBy {
		msg = "You get this review because the person who added this source can no longer approve it.\n\n" + msg
	}
	item, err := ri.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ReviewItemKind, Ref: row.ID, UserID: reviewer,
		Title:     reviewClip(reviewPlain("Review skills update: "+src.Slug), inbox.MaxTitle),
		Summary:   reviewClip(reviewPlain(reviewSummary(src, v, diff)), inbox.MaxSummary),
		Message:   msg,
		Questions: reviewQuestion("Make this version live?", "Approve: make it live", "Reject: keep the current version"),
	})
	if err != nil {
		// No row without an item: the next check calls again, and failed
		// attempts do not pile up.
		_ = ri.store.remove(ctx, row.ID)
		return fmt.Errorf("skillhub: review item for %s: %w", src.Slug, err)
	}
	return ri.store.setInbox(ctx, row.ID, item.ID)
}

// reviewer is who decides a source's reviews: its creator while they are an
// admin, else the fallback reviewer (the owner), else still the creator.
func (ri *ReviewInbox) reviewer(ctx context.Context, creator string) string {
	if creator != "" && (ri.o.IsAdmin == nil || ri.o.IsAdmin(ctx, creator)) {
		return creator
	}
	if ri.o.FallbackReviewer != nil {
		if uid, err := ri.o.FallbackReviewer(ctx); err == nil && uid != "" {
			return uid
		}
	}
	return creator
}

// Settle closes the open review items of a source after an admin decided
// in the dashboard (Activate, Reject) or deleted the source. versionID ""
// closes the items of every version. reason is shown on the items.
func (ri *ReviewInbox) Settle(ctx context.Context, sourceID, versionID, reason string) error {
	open, err := ri.store.pendingReviews(ctx, sourceID, versionID)
	if err != nil {
		return err
	}
	return ri.settleRows(ctx, open, reason)
}

func (ri *ReviewInbox) settleRows(ctx context.Context, rows []proposalRow, reason string) error {
	return closeProposalRows(ctx, ri.store, ri.o.Inbox, rows, proposalCancelled, reason)
}

// SourceDeleted closes every open question about a deleted source: its
// review items, and the agents' change proposals against it (rejected, so
// the proposing agent sees why; their Inbox items are withdrawn).
func (ri *ReviewInbox) SourceDeleted(ctx context.Context, sourceID, reason string) error {
	if err := ri.Settle(ctx, sourceID, "", reason); err != nil {
		return err
	}
	rows, err := ri.store.pendingForSource(ctx, proposalKindChange, sourceID)
	if err != nil {
		return err
	}
	return closeProposalRows(ctx, ri.store, ri.o.Inbox, rows, proposalRejected, reason)
}

// decide is the Inbox handler of review items: approve activates the
// staged version, reject (or a deny) rejects it.
func (ri *ReviewInbox) decide(ctx context.Context, r *inbox.Request) (string, error) {
	row, err := ri.store.get(ctx, r.System.Ref)
	if err != nil {
		return "", fmt.Errorf("review %s: %w", r.System.Ref, err)
	}
	if row.Kind != proposalKindReview {
		return "", fmt.Errorf("review %s: not a review", row.ID)
	}
	if row.Status != proposalPending {
		return "This review was already settled (" + row.Status + ").", nil
	}
	decider := r.DeciderUserID
	if err := checkDecider(ctx, ri.o.IsAdmin, decider, row.UserID); err != nil {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: err.Error()})
		return "", err
	}
	var in reviewInput
	_ = json.Unmarshal([]byte(row.Input), &in)
	label := reviewShort(in.SHA)
	// Only the source's pending version may be decided here: approving an
	// outdated question must never roll back a newer live commit.
	src, err := ri.o.Admin.GetSource(ctx, row.SourceID)
	if errors.Is(err, ErrNotFound) {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: "the source was deleted"})
		return "", errors.New("the source was deleted")
	}
	if err != nil {
		return "", fmt.Errorf("read the source: %w", err) // the row stays pending; Sweep tries again
	}
	if src.PendingVersionID != row.VersionID {
		why := "version " + label + " no longer waits for review: a newer commit or a change of the source's settings replaced it, and the live version stays"
		_, _ = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: why})
		return "", errors.New(why)
	}
	if r.SystemChoice(reviewQuestionID) == reviewApprove {
		if ok, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalApproved, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
			return "This review was already settled.", err
		}
		return ri.applyApproved(ctx, row, decider, label, "inbox")
	}
	note := strings.TrimSpace(r.OwnerNote)
	if note == "" {
		note = "rejected in the Inbox"
	}
	if ok, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
		return "This review was already settled.", err
	}
	if err := ri.o.Admin.Reject(ctx, decider, row.SourceID, row.VersionID, note); err != nil {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalRejected}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("reject version %s: %w", label, err)
	}
	ri.audit(ctx, "skills.version_reject", decider, row, note)
	return "Version " + label + " was rejected; the current version stays live.", nil
}

// applyApproved makes an approved review's version live and records the
// outcome; the row is approved (the caller won that move or a resume's
// claim).
func (ri *ReviewInbox) applyApproved(ctx context.Context, row *proposalRow, decider, label, via string) (string, error) {
	if err := ri.o.Admin.Activate(ctx, decider, row.SourceID, row.VersionID); err != nil {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("make version %s live: %w", label, err)
	}
	_, _ = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalDone, proposalUpdate{})
	ri.audit(ctx, "skills.version_activate", decider, row, via)
	return "Version " + label + " is live.", nil
}

func (ri *ReviewInbox) audit(ctx context.Context, event, by string, row *proposalRow, reason string) {
	if ri.o.Audit == nil {
		return
	}
	_ = ri.o.Audit.Write(ctx, audit.Event{EventType: event, AgentID: "user:" + by, Reason: reason,
		ResultSummary: row.SourceID + " " + row.VersionID, ApprovalID: row.ID})
}

// checkDecider refuses a decision that names no person, a person other
// than want (when want is set), or one who is not an admin (when isAdmin
// is set).
func checkDecider(ctx context.Context, isAdmin func(context.Context, string) bool, decider, want string) error {
	switch {
	case decider == "":
		return errors.New("the decision does not name a person; decide in the dashboard")
	case want != "" && decider != want:
		return errors.New("only the person this item was sent to can decide it")
	case isAdmin != nil && !isAdmin(ctx, decider):
		return errors.New("only an admin can decide this; ask an admin to decide on the Skills page")
	}
	return nil
}

// ---- item text (shared with source proposals) ---------------------------------

// reviewQuestion is the single approve/reject question of an item.
func reviewQuestion(prompt, approve, reject string) []inbox.QuestionSpec {
	return []inbox.QuestionSpec{{ID: reviewQuestionID, Type: inbox.QuestionSingleChoice, Prompt: prompt,
		Options: []inbox.Option{{ID: reviewApprove, Label: approve}, {ID: reviewReject, Label: reject}}}}
}

func reviewSummary(src Source, v Version, d *Diff) string {
	return fmt.Sprintf("%s: %d added, %d changed, %d removed, %d warning%s",
		RepoLabel(src.Owner, src.Repo, v.SHA), len(d.Added), len(d.Changed), len(d.Removed), len(v.Warnings), reviewPlural(len(v.Warnings)))
}

func reviewMessage(src Source, v Version, d *Diff, dashboard string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A new commit of **%s/%s** (branch %s, %s) waits for your review before agents see it.\n\n",
		reviewMD(src.Owner), reviewMD(src.Repo), reviewCode(src.Branch), reviewFolder(src.Path))
	fmt.Fprintf(&b, "- New commit: [%s](%s)\n", reviewCode(reviewShort(v.SHA)), reviewCommitURL(src.Owner, src.Repo, v.SHA))
	if d.FromSHA != "" {
		fmt.Fprintf(&b, "- Live now: [%s](%s)\n", reviewCode(reviewShort(d.FromSHA)), reviewCommitURL(src.Owner, src.Repo, d.FromSHA))
	} else {
		b.WriteString("- Live now: no version yet\n")
	}
	fmt.Fprintf(&b, "- Skills in this version: %d (%d files)\n\n", v.SkillCount, v.FileCount)
	fmt.Fprintf(&b, "**Added (%d):** %s\n\n", len(d.Added), reviewList(d.Added))
	fmt.Fprintf(&b, "**Changed (%d):** %s\n\n", len(d.Changed), reviewList(d.Changed))
	fmt.Fprintf(&b, "**Removed (%d):** %s\n\n", len(d.Removed), reviewList(d.Removed))
	fmt.Fprintf(&b, "**Warnings:** %d", len(v.Warnings))
	if link := reviewSourceLink(dashboard, src.ID); link != "" {
		fmt.Fprintf(&b, ". [Open the source in toolyard](%s) to read them and compare versions.", link)
	}
	b.WriteString("\n\nApprove makes this version live for agents at once. Reject keeps the current version; the next commit asks again.")
	return b.String()
}

// reviewSourceLink is the dashboard page of a source, or "" without a
// public URL.
func reviewSourceLink(dashboard, sourceID string) string {
	base := strings.TrimRight(strings.TrimSpace(dashboard), "/")
	if base == "" || sourceID == "" {
		return ""
	}
	return base + "/#skills/" + url.PathEscape(sourceID)
}

func reviewCommitURL(owner, repo, sha string) string {
	return reviewRepoURL(owner, repo) + "/commit/" + url.PathEscape(sha)
}

func reviewRepoURL(owner, repo string) string {
	return "https://github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func reviewFolder(path string) string {
	if strings.Trim(path, "/") == "" {
		return "the repository root"
	}
	return "folder " + reviewCode(path)
}

// reviewList names up to reviewListCap skill paths as code, then how many
// more there are.
func reviewList(paths []string) string { return reviewListN(paths, reviewListCap) }

// reviewListN is reviewList with its own cap.
func reviewListN(paths []string, max int) string {
	if len(paths) == 0 {
		return "none"
	}
	n := len(paths)
	if n > max {
		n = max
	}
	parts := make([]string, 0, n+1)
	for _, p := range paths[:n] {
		parts = append(parts, reviewCode(p))
	}
	if len(paths) > n {
		parts = append(parts, fmt.Sprintf("and %d more", len(paths)-n))
	}
	return strings.Join(parts, ", ")
}

// reviewCode is s as a Markdown code span. Skill paths and branch names
// come from the repository, so neither a backtick nor a line break in them
// can end the span or start a new Markdown line.
func reviewCode(s string) string {
	return "`" + strings.ReplaceAll(reviewClip(reviewPlain(s), 200), "`", "'") + "`"
}

// reviewMD escapes the Markdown punctuation that could change how plain
// repository text renders, after reviewPlain.
func reviewMD(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "`", `\`+"`", "<", "&lt;", ">", "&gt;")
	return r.Replace(reviewPlain(s))
}

// reviewPlain replaces control characters (line breaks, tabs, carriage
// returns, the Unicode line and paragraph separators) and the
// bidirectional overrides with spaces, so text from a repository or an
// agent stays on its line in an item toolyard sends.
func reviewPlain(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == '\u2028', r == '\u2029',
			r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			return ' '
		}
		return r
	}, s)
}

// reviewShort is the 12-character commit label.
func reviewShort(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// reviewClip shortens s to at most n runes, ending with "…" when cut.
func reviewClip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func reviewPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

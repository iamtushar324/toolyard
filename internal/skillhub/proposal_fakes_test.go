package skillhub

// Test doubles for the review and proposal tests: a fake Admin that records
// every call, and an environment with a real Inbox on the test database.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// proposalFakeAdmin implements Admin for tests. Only what the review and
// proposal code calls does anything; the rest answers ErrNotFound.
type proposalFakeAdmin struct {
	mu sync.Mutex

	preview     *Preview
	previewErr  error
	createErr   error
	activateErr error
	rejectErr   error
	syncResult  SyncResult
	syncErr     error
	trusted     map[string]bool

	previews  []SourceInput
	previewBy []string
	creates   []SourceInput
	createBy  []string
	activates [][3]string // by, source, version
	rejects   [][4]string // by, source, version, note
	syncs     []string
}

var _ Admin = (*proposalFakeAdmin)(nil)

func newProposalFakeAdmin() *proposalFakeAdmin {
	return &proposalFakeAdmin{
		preview: &Preview{
			Source:     Source{Owner: "acme", Repo: "skills", Branch: "main", Path: "skills", Visibility: VisibleEveryone},
			SHA:        "0123456789abcdef0123456789abcdef01234567",
			SkillCount: 3, FileCount: 7, TotalBytes: 4096,
			Warnings: []Warning{
				{Path: "skills/a/SKILL.md", Code: "name_mismatch", Severity: "skip"},
				{Path: "skills/b/SKILL.md", Code: "name_mismatch", Severity: "skip"},
				{Path: "skills/c/logo.png", Code: "binary", Severity: "skip"},
			},
			Sample: []SkillSummary{{Path: "pdf", Name: "pdf", Description: "Read PDF files"}},
		},
		syncResult: SyncResult{SHA: "0123456789abcdef0123456789abcdef01234567", Activated: true},
		trusted:    map[string]bool{"anthropics": true},
	}
}

func (f *proposalFakeAdmin) ParseURL(raw string) (RepoRef, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "https://github.com/")
	owner, repo, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || repo == "" {
		return RepoRef{}, ErrInvalid
	}
	repo, _, _ = strings.Cut(repo, "/")
	return RepoRef{Owner: owner, Repo: repo}, nil
}

func (f *proposalFakeAdmin) Inspect(context.Context, string, string) (*RepoInfo, error) {
	return nil, ErrNotFound
}

func (f *proposalFakeAdmin) Preview(_ context.Context, by string, in SourceInput) (*Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews = append(f.previews, in)
	f.previewBy = append(f.previewBy, by)
	if f.previewErr != nil {
		return nil, f.previewErr
	}
	p := *f.preview
	return &p, nil
}

func (f *proposalFakeAdmin) CreateSource(_ context.Context, by string, in SourceInput) (*Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	f.createBy = append(f.createBy, by)
	if f.createErr != nil {
		return nil, f.createErr
	}
	ref, _ := f.ParseURL(in.URL)
	return &Source{ID: "src_00000001", Slug: ref.Repo, Owner: ref.Owner, Repo: ref.Repo, Branch: in.Branch, Path: in.Path, Mode: in.Mode, CreatedBy: by}, nil
}

func (f *proposalFakeAdmin) UpdateSource(context.Context, string, string, SourceUpdate) (*Source, error) {
	return nil, ErrNotFound
}
func (f *proposalFakeAdmin) DeleteSource(context.Context, string, string) error { return ErrNotFound }
func (f *proposalFakeAdmin) GetSource(context.Context, string) (*Source, error) {
	return nil, ErrNotFound
}
func (f *proposalFakeAdmin) ListSources(context.Context) ([]Source, error) { return nil, nil }

func (f *proposalFakeAdmin) SyncNow(_ context.Context, id string) (*SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, id)
	if f.syncErr != nil {
		return nil, f.syncErr
	}
	r := f.syncResult
	r.SourceID = id
	return &r, nil
}

func (f *proposalFakeAdmin) Versions(context.Context, string, int) ([]Version, error) {
	return nil, nil
}
func (f *proposalFakeAdmin) GetVersion(context.Context, string, string) (*Version, error) {
	return nil, ErrNotFound
}

func (f *proposalFakeAdmin) Activate(_ context.Context, by, id, versionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activates = append(f.activates, [3]string{by, id, versionID})
	return f.activateErr
}

func (f *proposalFakeAdmin) Reject(_ context.Context, by, id, versionID, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejects = append(f.rejects, [4]string{by, id, versionID, note})
	return f.rejectErr
}

func (f *proposalFakeAdmin) Skills(context.Context, string) ([]SkillUsage, error) { return nil, nil }
func (f *proposalFakeAdmin) SetRuns(context.Context, string, string, string, Runs) error {
	return ErrNotFound
}
func (f *proposalFakeAdmin) RotateWebhookSecret(context.Context, string, string) (string, error) {
	return "", ErrNotFound
}
func (f *proposalFakeAdmin) HandleWebhook(context.Context, string, string, []byte, string) error {
	return ErrNotFound
}
func (f *proposalFakeAdmin) SetReviewHook(ReviewHook)   {}
func (f *proposalFakeAdmin) SetTokenSource(TokenSource) {}
func (f *proposalFakeAdmin) TrustedOwner(_ context.Context, owner string) bool {
	return f.trusted[owner]
}

// proposalEnv is a real Inbox on the test database, a fake Admin, and a
// clock the test moves.
type proposalEnv struct {
	db     *store.DB
	inbox  *inbox.Service
	admin  *proposalFakeAdmin
	admins map[string]bool
	mu     sync.Mutex
	now    time.Time
}

func newProposalEnv(t *testing.T) *proposalEnv {
	t.Helper()
	e := &proposalEnv{db: storetest.Open(t), admin: newProposalFakeAdmin(),
		admins: map[string]bool{"u_owner": true}, now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	svc, err := inbox.New(context.Background(), inbox.Options{DB: e.db, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Flush)
	e.inbox = svc
	return e
}

func (e *proposalEnv) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

func (e *proposalEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func (e *proposalEnv) isAdmin(_ context.Context, uid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.admins[uid]
}

// answer decides an item as user uid with the approve or reject option.
func (e *proposalEnv) answer(t *testing.T, id, uid, choice string) *inbox.Request {
	t.Helper()
	r, err := e.inbox.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.inbox.Decide(context.Background(), id, inbox.Decision{Action: "answer", RequestRevision: r.Revision,
		SubmissionID: "sub-" + id + "-" + choice, Answers: map[string]inbox.Answer{reviewQuestionID: {Value: choice}},
		Decider: actor.Decider{UserID: uid, Name: uid, Via: actor.ViaDashboard}})
	if err != nil {
		t.Fatalf("decide %s: %v", id, err)
	}
	return got
}

func proposalActivity(r *inbox.Request) string {
	var out []string
	for _, a := range r.Activity {
		out = append(out, a.Text)
	}
	return strings.Join(out, " | ")
}

// proposalDecider is a dashboard decision by uid.
func proposalDecider(uid string) actor.Decider {
	return actor.Decider{UserID: uid, Name: uid, Via: actor.ViaDashboard}
}

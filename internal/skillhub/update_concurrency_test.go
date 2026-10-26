package skillhub

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A source update writes only the fields it names, only while the source
// is as it was read, and moves updated_at forward with bumpUpdatedAt: an
// update read before an admin's hide never clears the hide lock.
func TestUpdateSourceKeepsAConcurrentHide(t *testing.T) {
	svc, _, src := sharingFixture(t)
	ctx := context.Background()
	stale, err := svc.getSourceRow(ctx, svc.db, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Hidden: &on}); err != nil {
		t.Fatal(err)
	}
	// A write from the read before the hide is refused, and changes
	// nothing.
	vis, members := VisibleMembers, []string{"usr_team"}
	up := SourceUpdate{Visibility: &vis, Members: &members}
	stale.Visibility, stale.Members = vis, members
	if ok, err := svc.updateSourceSettings(ctx, stale, up, stale.UpdatedAt.UnixMilli(), false, false); err != nil || ok {
		t.Fatalf("a write from a stale read: ok %v, err %v", ok, err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); !got.Hidden || got.Visibility != VisibleEveryone {
		t.Fatalf("after the stale write: hidden %v visibility %q", got.Hidden, got.Visibility)
	}
	// UpdateSource reads again: the change lands and the lock stays.
	got, err := svc.UpdateSource(ctx, "usr_contrib", src.ID, up)
	if err != nil || !got.Hidden || got.Visibility != VisibleMembers || len(got.Members) != 1 {
		t.Fatalf("update after the hide: %+v %v", got, err)
	}
	if srcs, _ := svc.Sources(ctx, Viewer{UserID: "usr_team", AgentID: "agt_team"}); len(srcs) != 0 {
		t.Fatalf("a hidden source is shown after an update: %+v", srcs)
	}
	// updated_at always moves forward, even past a value ahead of this
	// node's clock (another node's write).
	ahead := time.Now().Add(time.Hour).UnixMilli()
	if _, err := svc.db.ExecContext(ctx, `UPDATE skill_sources SET updated_at = ? WHERE id = ?`, ahead, src.ID); err != nil {
		t.Fatal(err)
	}
	poll := 600
	got, err = svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{PollSeconds: &poll})
	if err != nil || got.UpdatedAt.UnixMilli() != ahead+1 || got.PollSeconds != poll || !got.Hidden {
		t.Fatalf("update past a newer updated_at: %+v %v (want updated_at %d)", got, err, ahead+1)
	}
	if err := svc.SetRuns(ctx, "usr_admin", src.ID, "pr-review", RunsHost); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.UpdatedAt.UnixMilli() != ahead+2 {
		t.Fatalf("SetRuns moved updated_at to %d, want %d", got.UpdatedAt.UnixMilli(), ahead+2)
	}
	// A deleted source answers not found.
	if err := svc.DeleteSource(ctx, "usr_admin", src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{PollSeconds: &poll}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a deleted source: %v", err)
	}
}

// An update checked against an older read of the source (the API's
// before) is refused when the source changed since that read: an admin's
// rejection in between is never undone by a contributor's
// {"enabled": true}.
func TestUpdateSourceChecksTheCallersRead(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeReview, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	before, err := svc.GetSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", src.ID, false, "not ours", ""); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := svc.UpdateSource(ctx, "usr_contrib", src.ID, SourceUpdate{Enabled: &on, IfUpdatedAt: before.UpdatedAt}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an update checked against the read before the rejection: %v", err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.Enabled || got.Approval != ApprovalRejected {
		t.Fatalf("a rejected source was turned on: %+v", got)
	}
	cur, err := svc.GetSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	poll := 900
	if got, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{PollSeconds: &poll, IfUpdatedAt: cur.UpdatedAt}); err != nil || got.PollSeconds != poll {
		t.Fatalf("an update checked against the current read: %+v %v", got, err)
	}
}

// The guard of a retried update covers the credential (read access and
// token), what is fetched (excludes, limits) and the paused state, as well
// as the approval.
func TestUpdateGuardCoversTheCredentialAndWhatIsFetched(t *testing.T) {
	base := sourceRow{Source: Source{Mode: ModeReview, Branch: "main", Path: "skills", Auth: AuthToken}, tokenSealed: "sealed-1"}
	g := guardOf(&base)
	for name, change := range map[string]func(r *sourceRow){
		"token":    func(r *sourceRow) { r.tokenSealed = "sealed-2" },
		"auth":     func(r *sourceRow) { r.Auth = AuthGitHub },
		"exclude":  func(r *sourceRow) { r.Exclude = []string{"*.tmp"} },
		"limits":   func(r *sourceRow) { r.Limits.MaxSkills = 5 },
		"enabled":  func(r *sourceRow) { r.Enabled = true },
		"approval": func(r *sourceRow) { r.Approval = ApprovalRejected },
		"hidden":   func(r *sourceRow) { r.Hidden = true },
	} {
		r := base
		change(&r)
		if guardOf(&r) == g {
			t.Errorf("a change of %s passes the guard", name)
		}
	}
	if same := base; guardOf(&same) != g {
		t.Fatal("an unchanged source fails the guard")
	}
}

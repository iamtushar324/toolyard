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
	if ok, err := svc.updateSourceSettings(ctx, stale, up, stale.UpdatedAt.UnixMilli(), false); err != nil || ok {
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

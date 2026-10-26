package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// toolyard owner show|transfer: the host break-glass for the owner role.
func TestOwnerCommand(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.New(db)
	run := func(sub, to string, unblock bool) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := ownerCommand(ctx, ids, sub, to, unblock, &out)
		return out.String(), err
	}

	if out, err := run("show", "", false); err != nil || !strings.Contains(out, "no owner yet") {
		t.Fatalf("show on an empty install: %q %v", out, err)
	}
	admin, err := ids.CreateUser(ctx, "admin", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ids.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user_m", Email: "m@example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run("show", "", false); err != nil || !strings.Contains(out, "owner admin ("+admin.ID+")") {
		t.Fatalf("show: %q %v", out, err)
	}
	if _, err := run("transfer", "", false); err == nil {
		t.Fatal("transfer without --to succeeded")
	}
	if _, err := run("transfer", "nobody", false); err == nil {
		t.Fatal("transfer to an unknown user succeeded")
	}
	// The owner left the organisation and the sync suspended them.
	if changed, err := ids.SuspendDepartedOwner(ctx, admin.ID); err != nil || !changed {
		t.Fatalf("suspend: %v %v", changed, err)
	}
	if out, err := run("show", "", false); err != nil || !strings.Contains(out, "blocked (left_org)") {
		t.Fatalf("show a blocked owner: %q %v", out, err)
	}
	out, err := run("transfer", "m@example.com", false)
	if err != nil || !strings.Contains(out, "owner admin -> "+m.Username) || !strings.Contains(out, identity.ActorCLI) {
		t.Fatalf("transfer: %q %v", out, err)
	}
	if o, err := ids.Owner(ctx); err != nil || o.ID != m.ID || o.Role != identity.RoleAdmin {
		t.Fatalf("owner after the transfer = %+v %v", o, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = 'user.owner' AND agent_id = ?`, identity.ActorCLI).Scan(&n); err != nil || n != 1 {
		t.Fatalf("break-glass audit rows = %d %v, want 1", n, err)
	}
	if _, err := run("transfer", m.Username, false); err == nil || !strings.Contains(err.Error(), "already the owner") {
		t.Fatalf("transfer to the owner: %v", err)
	}
	// Back to the blocked previous owner: refused without --unblock.
	if _, err := run("transfer", "admin", false); err == nil || !strings.Contains(err.Error(), "--unblock") {
		t.Fatalf("transfer to a blocked user: %v", err)
	}
	if out, err := run("transfer", "admin", true); err != nil || !strings.Contains(out, "owner "+m.Username+" -> admin") {
		t.Fatalf("transfer with --unblock: %q %v", out, err)
	}
	if u, _ := ids.GetUserByID(ctx, admin.ID); !u.Owner || u.Status != identity.StatusActive {
		t.Fatalf("unblocked owner = %+v", u)
	}
	if _, err := run("rename", "", false); err == nil {
		t.Fatal("unknown subcommand succeeded")
	}
}

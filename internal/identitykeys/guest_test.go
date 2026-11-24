package identitykeys

import (
	"context"
	"errors"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// TestGuestHasNoIdentityKey: a key left on a row that became a guest is
// never forwarded or revealed, and a guest can be issued, rotated,
// registered or provisioned none; the key can still be revoked.
func TestGuestHasNoIdentityKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	if _, err := e.svc.Issue(ctx, e.admin.ID, uid); err != nil {
		t.Fatal(err)
	}
	if key, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || key == "" {
		t.Fatalf("member's key not forwarded: %v", err)
	}
	if _, err := e.db.Exec(`UPDATE users SET is_guest = 1 WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	e.svc.Invalidate(uid)

	if key, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); !errors.Is(err, gateway.ErrNoIdentityKey) || key != "" {
		t.Errorf("guest ForwardKey: got a key %v, err %v; want ErrNoIdentityKey", key != "", err)
	}
	if _, _, err := e.svc.Reveal(ctx, uid); !errors.Is(err, ErrGuest) {
		t.Errorf("guest Reveal = %v, want ErrGuest", err)
	}
	if _, err := e.svc.Rotate(ctx, e.admin.ID, uid); !errors.Is(err, ErrGuest) {
		t.Errorf("guest Rotate = %v, want ErrGuest", err)
	}
	if _, err := e.svc.Register(ctx, e.admin.ID, uid); !errors.Is(err, ErrGuest) {
		t.Errorf("guest Register = %v, want ErrGuest", err)
	}
	if _, err := e.svc.Provision(ctx, e.admin.ID, uid); !errors.Is(err, ErrGuest) {
		t.Errorf("guest Provision = %v, want ErrGuest", err)
	}
	if _, err := e.svc.Revoke(ctx, e.admin.ID, uid); err != nil {
		t.Fatalf("revoke a guest's key: %v", err)
	}
	if has, err := e.svc.HasKey(ctx, uid); err != nil || has {
		t.Errorf("after revoke HasKey = %v, %v", has, err)
	}
	if _, err := e.svc.Issue(ctx, e.admin.ID, uid); !errors.Is(err, ErrGuest) {
		t.Errorf("guest Issue = %v, want ErrGuest", err)
	}
}

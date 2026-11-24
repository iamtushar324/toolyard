package store_test

import (
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// TestMigration0079GuestInvites: users gains is_guest (0 by default, 0 or
// 1 only) and guest_invites keys invites by a lower-case email.
func TestMigration0079GuestInvites(t *testing.T) {
	db := storetest.Open(t)
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at, role, status)
		VALUES('u_a','a','x',?,?,'member','active')`, now, now); err != nil {
		t.Fatal(err)
	}
	var guest int
	if err := db.QueryRow(`SELECT is_guest FROM users WHERE id = 'u_a'`).Scan(&guest); err != nil || guest != 0 {
		t.Fatalf("is_guest = %d, %v; want 0", guest, err)
	}
	if _, err := db.Exec(`UPDATE users SET is_guest = 2 WHERE id = 'u_a'`); err == nil {
		t.Error("is_guest = 2 accepted: the CHECK is missing")
	}
	if _, err := db.Exec(`INSERT INTO guest_invites(email, servers, created_at, updated_at) VALUES('ext@example.org','["github"]',?,?)`, now, now); err != nil {
		t.Fatalf("insert invite: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO guest_invites(email, created_at, updated_at) VALUES('Ext@Example.org',?,?)`, now, now); err == nil {
		t.Error("an upper-case email was accepted: the CHECK is missing")
	}
	var tools, note string
	var expires *int64
	if err := db.QueryRow(`SELECT tools, note, expires_at FROM guest_invites WHERE email = 'ext@example.org'`).Scan(&tools, &note, &expires); err != nil {
		t.Fatal(err)
	}
	if tools != "[]" || note != "" || expires != nil {
		t.Errorf("defaults = %q %q %v", tools, note, expires)
	}
}

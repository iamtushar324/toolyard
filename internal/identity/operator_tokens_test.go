package identity

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newOpTestService(t *testing.T) (*Service, *User) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	u, err := s.CreateUser(context.Background(), "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	return s, u
}

func TestOperatorTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s, u := newOpTestService(t)

	tok, ot, err := s.CreateOperatorToken(ctx, u.ID, "claude", nil, 0, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, OperatorTokenPrefix) || !strings.HasPrefix(ot.ID, "op_") {
		t.Fatalf("shape: %s %s", tok[:8], ot.ID)
	}
	if strings.Join(ot.Scopes, " ") != "read write" {
		t.Fatalf("default scopes = %v", ot.Scopes)
	}
	got, gu, err := s.VerifyOperatorToken(ctx, tok)
	if err != nil || got.ID != ot.ID || gu.ID != u.ID {
		t.Fatalf("verify: %v %v %v", got, gu, err)
	}
	for _, bad := range []string{"", "tyop_", tok + "x", strings.Replace(tok, ".", ".A", 1), ot.ID} {
		if _, _, err := s.VerifyOperatorToken(ctx, bad); err == nil {
			t.Errorf("verify accepted %q", bad)
		}
	}
	// The stored row never holds the token.
	var hash string
	_ = s.db.QueryRowContext(ctx, `SELECT token_hash FROM operator_tokens WHERE id=?`, ot.ID).Scan(&hash)
	if hash == "" || strings.Contains(hash, tok) {
		t.Fatal("token stored in clear")
	}

	if err := s.RevokeOperatorToken(ctx, "someone-else", ot.ID); err != ErrOperatorTokenNotFound {
		t.Fatalf("revoke by non-owner: %v", err)
	}
	if err := s.RevokeOperatorToken(ctx, u.ID, ot.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyOperatorToken(ctx, tok); err == nil {
		t.Fatal("revoked token still verifies")
	}
}

func TestOperatorTokenExpiryAndScopes(t *testing.T) {
	ctx := context.Background()
	s, u := newOpTestService(t)
	tok, _, err := s.CreateOperatorToken(ctx, u.ID, "short", []string{"owner"}, time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, err := s.VerifyOperatorToken(ctx, tok); err == nil {
		t.Fatal("expired token verifies")
	}
	got, err := NormalizeOperatorScopes([]string{"owner, write"})
	if err != nil || strings.Join(got, " ") != "owner read write" {
		t.Fatalf("normalize = %v %v", got, err)
	}
	if _, err := NormalizeOperatorScopes([]string{"root"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

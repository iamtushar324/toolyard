package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// race runs fn(0..n-1) in n goroutines released together by a barrier and
// returns each call's error by index. On SQLite the store's single
// connection serialises the transactions; on Postgres they really race.
func race(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			errs[i] = fn(i)
		}(i)
	}
	close(barrier)
	wg.Wait()
	return errs
}

// Exchanging one enrollment code several times at once mints exactly one
// token; every other exchange is ErrEnrollNotFound.
func TestExchangeEnrollmentOnce(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	code, ag, err := s.CreateEnrollment(ctx, owner, "bot", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	tokens := make([]string, n)
	errs := race(n, func(i int) error {
		tok, _, err := s.ExchangeEnrollment(ctx, code)
		tokens[i] = tok
		return err
	})
	var won []string
	for i, err := range errs {
		switch {
		case err == nil:
			won = append(won, tokens[i])
		case errors.Is(err, ErrEnrollNotFound):
		default:
			t.Fatalf("exchange %d: unexpected error %v", i, err)
		}
	}
	if len(won) != 1 {
		t.Fatalf("%d exchanges got a token, want exactly 1", len(won))
	}
	got, err := s.VerifyAgentToken(ctx, won[0])
	if err != nil || got.ID != ag.ID {
		t.Fatalf("the one token: %+v, %v", got, err)
	}
	// The code is spent.
	if _, _, err := s.ExchangeEnrollment(ctx, code); !errors.Is(err, ErrEnrollNotFound) {
		t.Errorf("exchange after the claim: %v, want ErrEnrollNotFound", err)
	}
}

// An expired code is refused and cleared, and a later exchange of it is
// still refused.
func TestExchangeEnrollmentExpired(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	code, _, err := s.CreateEnrollment(ctx, owner, "bot", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET enroll_expires = ? WHERE enroll_code = ?`,
		time.Now().Add(-time.Minute).UnixMilli(), code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ExchangeEnrollment(ctx, code); !errors.Is(err, ErrEnrollNotFound) {
		t.Fatalf("expired exchange: %v, want ErrEnrollNotFound", err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE enroll_code = ?`, code).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("expired code still on %d rows", left)
	}
}

// Two setups at once on an empty store: exactly one creates the owner.
func TestConcurrentCreateUserOneOwner(t *testing.T) {
	s := newEmptyIdentity(t)
	ctx := context.Background()
	const n = 3 // each attempt hashes a password with argon2id (64 MiB)
	errs := race(n, func(i int) error {
		_, err := s.CreateUser(ctx, fmt.Sprintf("owner%d", i), "long-enough-password")
		return err
	})
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrUserExists):
		default:
			t.Fatalf("setup %d: unexpected error %v", i, err)
		}
	}
	var users, admins int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || users != 1 || admins != 1 {
		t.Fatalf("setups: %d succeeded, %d users, %d admins; want 1, 1, 1", ok, users, admins)
	}
}

// The same person's first sign-in arriving several times at once makes one
// row, and every call returns it.
func TestConcurrentFirstClerkSignInOneRow(t *testing.T) {
	s, _ := newTestIdentity(t)
	ctx := context.Background()
	const n = 4
	ids := make([]string, n)
	errs := race(n, func(i int) error {
		u, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_dup", Email: "dup@beknown.work"}, "")
		if err == nil {
			ids[i] = u.ID
		}
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sign-in %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("sign-ins returned different users: %v", ids)
		}
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE clerk_user_id = ?`, "user_dup").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one Clerk id, want 1", rows)
	}
}

// Demoting or blocking the last two active admins at the same time leaves
// exactly one: the active admin rows are locked before the count, so the
// second transaction sees the first one's change. Postgres only: SQLite's
// single connection never interleaves the two.
func TestConcurrentLastTwoAdminsKeepsOne(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	s, first := newTestIdentity(t)
	ctx := context.Background()
	second := clerkUser(t, s, "user_second", "second@beknown.work", "")
	activeAdmins := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ? AND status = ?`,
			RoleAdmin, StatusActive).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ops := []struct {
		name string
		a, b func() error
	}{
		{"demote both",
			func() error { return s.SetRole(ctx, first, RoleMember) },
			func() error { return s.SetRole(ctx, second.ID, RoleMember) }},
		{"block both",
			func() error { return s.SetStatus(ctx, first, StatusBlocked, "x") },
			func() error { return s.SetStatus(ctx, second.ID, StatusBlocked, "x") }},
		{"demote one, block the other",
			func() error { return s.SetRole(ctx, first, RoleMember) },
			func() error { return s.SetStatus(ctx, second.ID, StatusBlocked, "x") }},
	}
	for round := 0; round < 10; round++ {
		for _, op := range ops {
			// Both are active admins again.
			for _, id := range []string{first, second.ID} {
				if err := s.SetStatus(ctx, id, StatusActive, ""); err != nil {
					t.Fatal(err)
				}
				if err := s.SetRole(ctx, id, RoleAdmin); err != nil {
					t.Fatal(err)
				}
			}
			if got := activeAdmins(); got != 2 {
				t.Fatalf("%s: setup has %d active admins, want 2", op.name, got)
			}
			errs := race(2, func(i int) error {
				if i == 0 {
					return op.a()
				}
				return op.b()
			})
			ok, refused := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrLastAdmin):
					refused++
				default:
					t.Fatalf("%s: unexpected error %v", op.name, err)
				}
			}
			if ok != 1 || refused != 1 {
				t.Fatalf("%s (round %d): %d succeeded, %d refused; want 1 and 1", op.name, round, ok, refused)
			}
			if got := activeAdmins(); got != 1 {
				t.Fatalf("%s (round %d): %d active admins left, want 1", op.name, round, got)
			}
		}
	}
}

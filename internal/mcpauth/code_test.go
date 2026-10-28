package mcpauth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// TestConfirmCodeForm: codes are six symbols of the unambiguous alphabet;
// typed codes are read loosely (case, spaces, dashes) and anything else is
// no code.
func TestConfirmCodeForm(t *testing.T) {
	re := regexp.MustCompile(`^[2-9A-HJKMNP-Z]{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := newConfirmCode()
		if err != nil || !re.MatchString(c) {
			t.Fatalf("code %q (%v)", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
	if strings.ContainsAny(codeAlphabet, "01OIL") || len(codeAlphabet) != 31 {
		t.Fatalf("alphabet %q", codeAlphabet)
	}
	if got := FormatCode("K7P2QX"); got != "K7P-2QX" {
		t.Fatalf("FormatCode: %q", got)
	}
	for in, want := range map[string]string{
		"K7P-2QX": "K7P2QX", " k7p 2qx ": "K7P2QX", "k7p2qx": "K7P2QX",
		"K7P-2Q": "", "K7P-2QX9": "", "K0P-2QX": "", "K7P-2QÖ": "", "": "",
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	if codeHash("req-a", "K7P2QX") == codeHash("req-b", "K7P2QX") {
		t.Fatal("the code's hash is not keyed with the request id")
	}
}

// TestAllowFromAnotherBrowserNeedsTheCode: without SameBrowser, an allow
// needs the request's code; a missing or wrong code is counted; the right
// one, typed loosely, allows. Deny needs no code. SameBrowser needs none.
func TestAllowFromAnotherBrowserNeedsTheCode(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	if !req.HasCode || len(req.Code) != codeLen {
		t.Fatalf("request code %q (has %v)", req.Code, req.HasCode)
	}
	if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_requests WHERE id = ? AND code_hash = ?`, req.ID, codeHash(req.ID, req.Code)) != 1 ||
		f.count(t, `SELECT COUNT(*) FROM mcp_oauth_requests WHERE code_hash = ?`, req.Code) != 0 {
		t.Fatal("the code is not stored by its keyed hash alone")
	}
	pending, err := f.svc.PendingRequest(ctx, req.ID)
	if err != nil || !pending.HasCode || pending.Code != "" {
		t.Fatalf("PendingRequest: %+v %v", pending, err)
	}
	wrong := "222222"
	if req.Code == wrong {
		wrong = "333333"
	}
	for i, typed := range []string{"", wrong, "not a code"} {
		if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Other", Code: typed}); !errors.Is(err, ErrCodeWrong) {
			t.Fatalf("allow with %q: %v, want ErrCodeWrong", typed, err)
		}
		if n := f.count(t, `SELECT code_attempts FROM mcp_oauth_requests WHERE id = ?`, req.ID); n != i+1 {
			t.Fatalf("%d attempts after %d wrong codes", n, i+1)
		}
	}
	if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_grants`) != 0 {
		t.Fatal("a wrong code created a grant")
	}
	loose := strings.ToLower(FormatCode(req.Code))
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Other", Code: loose}); err != nil {
		t.Fatalf("allow with the right code: %v", err)
	}
	f.wantStatus(t, req, WaitApproved)

	same := f.begin(t)
	if _, err := f.svc.Approve(ctx, same.ID, f.user, Consent{Nickname: "Same", SameBrowser: true}); err != nil {
		t.Fatalf("same-browser allow: %v", err)
	}
	denied := f.begin(t)
	if _, err := f.svc.Deny(ctx, denied.ID, f.user); err != nil {
		t.Fatalf("deny without a code: %v", err)
	}
	if n := f.count(t, `SELECT code_attempts FROM mcp_oauth_requests WHERE id = ?`, denied.ID); n != 0 {
		t.Fatalf("deny counted %d attempts", n)
	}
}

// TestTooManyWrongCodesEndTheRequest: MaxCodeAttempts wrong codes end the
// request: the last returns ErrCodeLocked, nothing can answer it any more
// (not even with the right code), the waiting page reports expired, and an
// audit row records it without the code.
func TestTooManyWrongCodesEndTheRequest(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	wrong := "222222"
	if req.Code == wrong {
		wrong = "333333"
	}
	for i := 1; i < MaxCodeAttempts; i++ {
		if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Guess", Code: wrong}); !errors.Is(err, ErrCodeWrong) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	f.wantStatus(t, req, WaitPending)
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Guess", Code: wrong}); !errors.Is(err, ErrCodeLocked) {
		t.Fatalf("wrong code %d: %v, want ErrCodeLocked", MaxCodeAttempts, err)
	}
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Guess", Code: req.Code}); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("the right code after the request ended: %v, want ErrRequestNotFound", err)
	}
	if _, err := f.svc.Deny(ctx, req.ID, f.user); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("deny after the request ended: %v, want ErrRequestNotFound", err)
	}
	f.wantStatus(t, req, WaitExpired)
	if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnExpired) {
		t.Fatalf("Continue after the request ended: %v", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM audit_events WHERE event_type = 'mcp_oauth.code_locked'`) != 1 ||
		f.count(t, `SELECT COUNT(*) FROM audit_events WHERE arguments LIKE ?`, "%"+req.Code+"%") != 0 {
		t.Fatal("the ended request has no audit row, or the row carries the code")
	}
}

// TestRequestWithoutACodeIsAllowedAsBefore: a request stored before codes
// existed (no code hash) is allowed from any browser without a code.
func TestRequestWithoutACodeIsAllowedAsBefore(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	if _, err := f.db.Exec(`UPDATE mcp_oauth_requests SET code_hash = NULL WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.svc.PendingRequest(ctx, req.ID); err != nil || pending.HasCode {
		t.Fatalf("PendingRequest of an old request: %+v %v", pending, err)
	}
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Old"}); err != nil {
		t.Fatalf("allow of an old request: %v", err)
	}
}

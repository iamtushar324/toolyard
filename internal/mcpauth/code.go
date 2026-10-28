package mcpauth

// code.go is the confirmation code. The stop page of /oauth/authorize shows
// a short code ("Your code: K7P-2QX"), and an allow from a browser that did
// not render that stop page needs it typed on the consent screen. The
// window where a connection started can finish it after an allow (the
// waiting page, wait.go, or the return link, return.go), so without the
// code a person who opens a consent link someone else sent them and allows
// would hand that someone the connection. With it, they must also type a
// code that only the starting window shows.
//
// The HTTP layer tells Approve whether the answering browser rendered the
// stop page (Consent.SameBrowser, proved by the stop page's cookie): then
// no code is needed. Deny never needs it. Only an HMAC of the code, keyed
// with the request id, is stored; the code is compared in constant time,
// and MaxCodeAttempts wrong codes end the request.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	// codeAlphabet is digits and capital letters without 0, O, 1, I and L,
	// which people mistake for each other: 31 symbols.
	codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	// codeLen symbols, shown as XXX-XXX: 31^6, about 887 million codes.
	codeLen = 6
	// MaxCodeAttempts wrong codes end the request.
	MaxCodeAttempts = 5
	// codeMACLabel separates the code's HMAC from any other use of the key.
	codeMACLabel = "toolyard.oauth-confirm-code.v1|"
)

// newConfirmCode returns codeLen symbols of codeAlphabet, uniformly random
// (bytes at or above the largest multiple of the alphabet's size are
// dropped, so no symbol is likelier than another).
func newConfirmCode() (string, error) {
	limit := 256 - 256%len(codeAlphabet)
	out := make([]byte, 0, codeLen)
	buf := make([]byte, 16)
	for len(out) < codeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < codeLen {
				out = append(out, codeAlphabet[int(b)%len(codeAlphabet)])
			}
		}
	}
	return string(out), nil
}

// FormatCode shows a code as XXX-XXX.
func FormatCode(code string) string {
	if len(code) != codeLen {
		return code
	}
	return code[:3] + "-" + code[3:]
}

// normalizeCode turns what a person typed into a code: capitals, without
// spaces and dashes. It returns "" when the result cannot be a code.
func normalizeCode(typed string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(typed) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r < 128 && strings.ContainsRune(codeAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	if b.Len() != codeLen {
		return ""
	}
	return b.String()
}

// codeHash is the stored form of request requestID's code: HMAC-SHA256
// keyed with the request id, in hex.
func codeHash(requestID, code string) string {
	m := hmac.New(sha256.New, []byte(requestID))
	m.Write([]byte(codeMACLabel + code))
	return hex.EncodeToString(m.Sum(nil))
}

// checkCode lets an allow of req through when the answering browser
// rendered the request's stop page (c.SameBrowser), when the request has
// no code, or when c.Code is its code. A wrong (or missing) code counts an
// attempt and returns ErrCodeWrong; the MaxCodeAttempts-th ends the request
// (used, verdict locked, so nothing can answer it any more) and returns
// ErrCodeLocked.
func (s *Service) checkCode(ctx context.Context, req *AuthorizeRequest, c Consent) error {
	if c.SameBrowser {
		return nil
	}
	if req.codeHash == "" {
		// Stored before confirmation codes existed: it is allowed as before.
		// Only during the rollout: a request lives RequestTTL (10 minutes).
		return nil
	}
	if code := normalizeCode(c.Code); code != "" && subtle.ConstantTimeCompare([]byte(codeHash(req.ID, code)), []byte(req.codeHash)) == 1 {
		return nil
	}
	now := s.now().UnixMilli()
	var attempts int
	err := s.db.QueryRowContext(ctx,
		`UPDATE mcp_oauth_requests SET code_attempts = code_attempts + 1,
             used_at = CASE WHEN code_attempts + 1 >= CAST(? AS BIGINT) THEN CAST(? AS BIGINT) ELSE used_at END,
             verdict = CASE WHEN code_attempts + 1 >= CAST(? AS BIGINT) THEN CAST(? AS TEXT) ELSE verdict END
         WHERE id = ? AND used_at IS NULL AND expires_at > ?
         RETURNING code_attempts`, MaxCodeAttempts, now, MaxCodeAttempts, verdictLocked, req.ID, now).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRequestNotFound
	}
	if err != nil {
		return err
	}
	if attempts >= MaxCodeAttempts {
		return ErrCodeLocked
	}
	return ErrCodeWrong
}

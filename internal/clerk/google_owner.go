package clerk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// backendUser is the slice of a Backend API user (GET /v1/users/{id}) the
// identity checks read.
type backendUser struct {
	ID             string `json:"id"`
	Banned         bool   `json:"banned"`
	Locked         bool   `json:"locked"`
	PrimaryEmailID string `json:"primary_email_address_id"`
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	ImageURL       string `json:"image_url"`
	Emails         []struct {
		ID           string `json:"id"`
		Email        string `json:"email_address"`
		Verification struct {
			Status string `json:"status"`
		} `json:"verification"`
	} `json:"email_addresses"`
	ExternalAccounts []struct {
		Provider     string `json:"provider"`
		Email        string `json:"email_address"`
		Verification struct {
			Status string `json:"status"`
		} `json:"verification"`
	} `json:"external_accounts"`
}

// fetchUser reads userID from the Backend API. ErrNotMember for an unknown,
// banned or locked user; ErrUnavailable when Clerk cannot be asked.
func (c *Client) fetchUser(ctx context.Context, userID string) (*backendUser, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrNotMember
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: user request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: user request", ErrUnavailable)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNotMember
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%w: user HTTP %d", ErrUnavailable, res.StatusCode)
	}
	var user backendUser
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponseBody)).Decode(&user); err != nil {
		return nil, fmt.Errorf("%w: invalid user response", ErrUnavailable)
	}
	if user.ID != userID || user.Banned || user.Locked {
		return nil, ErrNotMember
	}
	return &user, nil
}

// verifiedPrimary is the user's primary email when it is verified, else "".
func (u *backendUser) verifiedPrimary() string {
	for _, e := range u.Emails {
		if e.ID == u.PrimaryEmailID && e.Verification.Status == "verified" {
			return e.Email
		}
	}
	return ""
}

// googleAccount checks that the user's verified primary email is allowed
// (ASCII-only, case-insensitive) and that a verified Google account has
// that same address. ErrNotMember otherwise.
func (u *backendUser) googleAccount(allowed string) (Member, error) {
	email := u.verifiedPrimary()
	if email == "" || !asciiEqualFold(email, allowed) {
		return Member{}, ErrNotMember
	}
	for _, a := range u.ExternalAccounts {
		if (a.Provider == "oauth_google" || a.Provider == "google") && a.Verification.Status == "verified" && asciiEqualFold(a.Email, email) {
			return Member{IsMember: true, Role: "org:admin", Email: email, FirstName: u.FirstName, LastName: u.LastName, ImageURL: u.ImageURL, EmailVerified: true}, nil
		}
	}
	return Member{}, ErrNotMember
}

// googleOwner reads identity from Clerk's trusted Backend API, never from
// a browser-supplied email or editable metadata. Both identifications must
// be verified: Clerk can return unverified external accounts too.
func (c *Client) googleOwner(ctx context.Context, userID string) (Member, error) {
	if strings.TrimSpace(userID) == "" {
		return Member{}, ErrNotMember
	}
	allowed := c.allowedGoogleEmail
	if c.allowedEmailFn != nil {
		e, err := c.allowedEmailFn(ctx)
		if err != nil {
			return Member{}, fmt.Errorf("%w: allowed email lookup", ErrUnavailable)
		}
		if e = strings.TrimSpace(e); e != "" {
			allowed = e
		}
	}
	user, err := c.fetchUser(ctx, userID)
	if err != nil {
		return Member{}, err
	}
	return user.googleAccount(allowed)
}

// VerifiedPrimaryEmail is userID's primary email address when Clerk has
// verified it. ErrNotMember when the user is unknown, banned or locked, or
// the primary address is not verified; ErrUnavailable when Clerk cannot be
// asked. toolyard checks it before an organisation sign-in with the owner
// email links to the setup account or claims the owner role: an
// organisation membership's identifier alone is not proof.
func (c *Client) VerifiedPrimaryEmail(ctx context.Context, userID string) (string, error) {
	user, err := c.fetchUser(ctx, userID)
	if err != nil {
		return "", err
	}
	email := user.verifiedPrimary()
	if email == "" {
		return "", ErrNotMember
	}
	return email, nil
}

// VerifyGoogleAccount checks that userID can pass personal access with
// email: a verified primary address equal to email and a verified Google
// account with it. ErrNotMember when not, ErrUnavailable when Clerk cannot
// be asked. toolyard checks a new owner with it before a transfer in
// personal sign-in mode.
func (c *Client) VerifyGoogleAccount(ctx context.Context, userID, email string) error {
	user, err := c.fetchUser(ctx, userID)
	if err != nil {
		return err
	}
	_, err = user.googleAccount(email)
	return err
}

// asciiEqualFold compares two email addresses ASCII-only and
// case-insensitively (only A-Z fold), so Unicode case folding cannot make
// a different address match; an empty address matches nothing. It is
// identity.EmailEqual, repeated here because identity does not import
// clerk.
func asciiEqualFold(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

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

// googleOwner reads identity from Clerk's trusted Backend API, never from
// a browser-supplied email or editable metadata. Both identifications must
// be verified: Clerk can return unverified external accounts too.
func (c *Client) googleOwner(ctx context.Context, userID string) (Member, error) {
	if strings.TrimSpace(userID) == "" {
		return Member{}, ErrNotMember
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		return Member{}, fmt.Errorf("%w: user request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return Member{}, fmt.Errorf("%w: user request", ErrUnavailable)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return Member{}, ErrNotMember
	}
	if res.StatusCode/100 != 2 {
		return Member{}, fmt.Errorf("%w: user HTTP %d", ErrUnavailable, res.StatusCode)
	}
	var user struct {
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
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponseBody)).Decode(&user); err != nil {
		return Member{}, fmt.Errorf("%w: invalid user response", ErrUnavailable)
	}
	if user.ID != userID || user.Banned || user.Locked {
		return Member{}, ErrNotMember
	}
	email := ""
	for _, e := range user.Emails {
		if e.ID == user.PrimaryEmailID && e.Verification.Status == "verified" && strings.EqualFold(e.Email, c.allowedGoogleEmail) {
			email = e.Email
			break
		}
	}
	if email == "" {
		return Member{}, ErrNotMember
	}
	for _, a := range user.ExternalAccounts {
		if (a.Provider == "oauth_google" || a.Provider == "google") && a.Verification.Status == "verified" && strings.EqualFold(a.Email, email) {
			return Member{IsMember: true, Role: "org:admin", Email: email, FirstName: user.FirstName, LastName: user.LastName, ImageURL: user.ImageURL}, nil
		}
	}
	return Member{}, ErrNotMember
}

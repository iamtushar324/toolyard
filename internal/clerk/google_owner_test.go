package clerk

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestPersonalGoogleOwner(t *testing.T) {
	for _, name := range []string{"owner", "wrong-email", "unverified-email", "secondary-email", "unverified-google", "other-provider", "wrong-google-email", "banned", "locked", "wrong-user", "missing-user", "outage", "malformed"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeClerk(t)
			c := f.client(t)
			c.allowedGoogleEmail = "owner@example.com"
			f.membership = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/users/user_123" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				email := map[string]any{"id": "email_1", "email_address": "Owner@example.com", "verification": map[string]any{"status": "verified"}}
				google := map[string]any{"provider": "oauth_google", "email_address": "owner@example.com", "verification": map[string]any{"status": "verified"}}
				u := map[string]any{"id": "user_123", "primary_email_address_id": "email_1", "email_addresses": []any{email}, "external_accounts": []any{google}}
				switch name {
				case "wrong-email":
					email["email_address"] = "other@example.com"
				case "unverified-email":
					email["verification"] = map[string]any{"status": "unverified"}
				case "secondary-email":
					u["primary_email_address_id"] = "email_2"
				case "unverified-google":
					google["verification"] = map[string]any{"status": "unverified"}
				case "other-provider":
					google["provider"] = "oauth_github"
				case "wrong-google-email":
					google["email_address"] = "other@example.com"
				case "banned":
					u["banned"] = true
				case "locked":
					u["locked"] = true
				case "wrong-user":
					u["id"] = "user_other"
				case "missing-user":
					w.WriteHeader(404)
					return
				case "outage":
					w.WriteHeader(503)
					return
				case "malformed":
					w.Write([]byte("{"))
					return
				}
				json.NewEncoder(w).Encode(u)
			}
			m, err := c.OrgMembership(t.Context(), "user_123")
			if name == "owner" {
				if err != nil || m.Email != "Owner@example.com" || !m.IsMember {
					t.Fatalf("owner: %+v %v", m, err)
				}
				return
			}
			want := ErrNotMember
			if name == "outage" || name == "malformed" {
				want = ErrUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %+v %v, want %v", m, err, want)
			}
		})
	}
}

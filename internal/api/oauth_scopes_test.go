package api

import (
	"reflect"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// TestPickScopesGoogleNoOfflineAccess (TEC-2072): Google rejects the
// "offline_access" scope, so it is not added for accounts.google.com; other
// issuers keep getting it.
func TestPickScopesGoogleNoOfflineAccess(t *testing.T) {
	drive := []string{"https://www.googleapis.com/auth/drive"}
	for _, iss := range []string{"https://accounts.google.com", "https://accounts.google.com/"} {
		got := pickScopes(&oauth.ASMetadata{Issuer: iss, ScopesSupported: drive}, nil)
		if !reflect.DeepEqual(got, drive) {
			t.Errorf("issuer %q: scopes = %v", iss, got)
		}
	}
	got := pickScopes(&oauth.ASMetadata{Issuer: "https://mcp.linear.app", ScopesSupported: []string{"read"}}, nil)
	if !reflect.DeepEqual(got, []string{"read", "offline_access"}) {
		t.Errorf("other issuer: scopes = %v", got)
	}
	if got := pickScopes(&oauth.ASMetadata{Issuer: "https://accounts.google.com"}, []string{"a"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("requested scopes must win: %v", got)
	}
}

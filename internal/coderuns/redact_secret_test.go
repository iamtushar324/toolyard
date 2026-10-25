package coderuns

import (
	"strings"
	"testing"
)

// A webhook secret (callbacks.register or callbacks.update with a
// generated secret) never stays in a stored call record, so such a call
// is not replayed on a resume.
func TestRedactGrantsRemovesWebhookSecrets(t *testing.T) {
	in := `{"callback_ref":"cb_1","secret":"whsec_QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo0NTY3ODk=","grant":"tyg_aWQ.c2lnbmF0dXJlLWJ5dGVzLW9mLWFuLWVkMjU1MTktc2lnbmF0dXJlLXRoYXQtaXMtbG9uZw"}`
	out := redactGrants(in)
	if strings.Contains(out, "QUJDREVG") || !strings.Contains(out, redactedSecret) || !strings.Contains(out, redactedGrant) {
		t.Fatalf("redacted: %s", out)
	}
	if got := redactGrants(`{"note":"whsec_ is the secret prefix"}`); got != `{"note":"whsec_ is the secret prefix"}` {
		t.Fatalf("a mention of the prefix changed: %s", got)
	}
}

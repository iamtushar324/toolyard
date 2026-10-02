package audit

import (
	"strings"
	"testing"
)

// TestRedactConnectLink: a connect link's ticket never survives into an
// audit row, however it got into the text; the route prefix with no
// ticket, and other paths, are left alone.
func TestRedactConnectLink(t *testing.T) {
	ticket := "hfBgdyrV9-_i2uWngcS73fFI0ETfR8guODHjVdkKgvQ"
	in := "linear (per_user, needs_signin): connect link (one-time, 10 minutes): https://toolyard.example/v1/connect/link/" + ticket + " — then ask me to retry."
	out := RedactString(in)
	if strings.Contains(out, ticket) {
		t.Fatalf("ticket survived redaction: %s", out)
	}
	if !strings.Contains(out, "https://toolyard.example"+redactionPlaceholder+" — then ask me to retry.") {
		t.Fatalf("redacted text = %s", out)
	}
	for _, keep := range []string{"/v1/connect/link/", "/v1/connect/t3", "/v1/me/connections/linear/begin"} {
		if RedactString(keep) != keep {
			t.Errorf("%q was redacted to %q", keep, RedactString(keep))
		}
	}
	js := RedactJSONBytes([]byte(`{"text":"open https://x/v1/connect/link/` + ticket + ` now","n":1}`))
	if strings.Contains(string(js), ticket) || !strings.Contains(string(js), `"n":1`) {
		t.Fatalf("json redaction = %s", js)
	}
}

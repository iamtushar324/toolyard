package audit

import (
	"encoding/json"
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

// TestRedactGrantToken (TEC-1807): a grant token inside script source never
// survives into an audit row or the live feed; a grant id without its
// secret part is left alone.
func TestRedactGrantToken(t *testing.T) {
	token := "tyg_8f2d1c0b." + strings.Repeat("Ab9_-", 9)
	code := `return await tools.memory.set({ key: "k", value: "v", _grant: "` + token + `" })`
	if out := RedactString(code); strings.Contains(out, token) || !strings.Contains(out, redactionPlaceholder) {
		t.Fatalf("grant survived redaction: %s", out)
	}
	js := RedactJSONBytes([]byte(`{"language":"javascript","code":` + strconvQuote(code) + `}`))
	if strings.Contains(string(js), token) {
		t.Fatalf("json redaction = %s", js)
	}
	if keep := "tyg_8f2d1c0b"; RedactString(keep) != keep {
		t.Errorf("a grant id was redacted to %q", RedactString(keep))
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

package docs

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// TyServedURLLine is the line of docs/ty/ty that names its default
// TOOLYARD_URL. The copy in the repository has none (ty then refuses to
// run without TOOLYARD_URL); ServedTy fills in the toolyard that serves
// the script, so no copy ever points at a host the reader did not choose.
const TyServedURLLine = `DEFAULT_URL = ""  # toolyard:served-url`

// openAPIServerURL is the placeholder server URL in docs/openapi/code.json.
const openAPIServerURL = `"url": "https://toolyard.example.com"`

// safeURL is the characters a served base URL may hold: it lands inside a
// Python string literal and a JSON string, so no quote, backslash, space
// or control character.
var safeURL = regexp.MustCompile(`^[A-Za-z0-9._~:/\[\]%-]+$`)

// ServedBaseURL returns base without a trailing slash when it is a plain
// http(s) base URL that is safe to embed, else "".
func ServedBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || !safeURL.MatchString(base) {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return base
}

// ServedTy is the ty command as GET /v1/code/ty serves it: its default
// TOOLYARD_URL is base, the toolyard that serves it. An unusable base
// leaves no default (ty then asks for TOOLYARD_URL).
func ServedTy(base string) string {
	base = ServedBaseURL(base)
	if base == "" {
		return TyScript
	}
	return strings.Replace(TyScript, TyServedURLLine, `DEFAULT_URL = "`+base+`"  # the toolyard that served this copy`, 1)
}

// ServedCodeOpenAPI is docs/openapi/code.json as GET /v1/code/openapi.json
// serves it: its server is base, the toolyard that serves it.
func ServedCodeOpenAPI(base string) string {
	base = ServedBaseURL(base)
	if base == "" {
		return CodeOpenAPI
	}
	quoted, _ := json.Marshal(base)
	return strings.Replace(CodeOpenAPI, openAPIServerURL, `"url": `+string(quoted), 1)
}

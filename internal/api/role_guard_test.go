package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// newRecorder is a tiny alias so helper tests read naturally.
func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

var handleFuncRE = regexp.MustCompile(`mux\.HandleFunc\(\s*(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*))\s*,`)

// registeredRoutePaths scans this package's non-test sources for every
// mux.HandleFunc registration, so the guard test covers routes nobody
// remembered to list — including ones added after this test was written.
func registeredRoutePaths(t *testing.T) []string {
	t.Helper()
	consts := map[string]string{
		"MemoryWebhookIngestPath": MemoryWebhookIngestPath,
		"MemoryWebhookJobsPrefix": MemoryWebhookJobsPrefix,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range handleFuncRE.FindAllStringSubmatch(string(src), -1) {
			switch {
			case m[1] != "":
				seen[m[1]] = true
			case m[2] != "":
				p, ok := consts[m[2]]
				if !ok {
					t.Fatalf("%s: mux.HandleFunc(%s, …) uses a constant this test doesn't know; add it to consts", f, m[2])
				}
				seen[p] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < 90 {
		t.Fatalf("route scan found only %d routes; the regexp is probably stale", len(out))
	}
	return out
}

// concretePaths turns a registered pattern into request paths: exact
// routes as-is, prefix routes with one and two extra segments, plus the
// per-agent action paths the allowlist names.
func concretePaths(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			add(p + "x")
			add(p + "x/y")
			continue
		}
		add(p)
	}
	for _, action := range []string{"rotate", "disable", "enable"} {
		add("/v1/agents/x/" + action)
	}
	sort.Strings(out)
	return out
}

// memberAllowlist is the contract: every (method, path) a member may call.
// Everything else on every registered route must be admin_only.
func memberAllowlist() map[string]bool {
	allowed := map[string]bool{}
	for _, mp := range []string{
		"GET /v1/health",
		"GET /v1/auth/me", "GET /v1/auth/config",
		"POST /v1/auth/logout", "POST /v1/auth/login", "POST /v1/auth/clerk/session",
		"GET /v1/agents", "POST /v1/agents", "POST /v1/agents/enroll",
		"POST /v1/agents/x/rotate", "POST /v1/agents/x/disable", "POST /v1/agents/x/enable",
		"DELETE /v1/agents/x",
		"GET /v1/me/servers",
	} {
		allowed[mp] = true
	}
	for _, p := range []string{
		"/v1/agents/exchange", "/v1/agents/whoami", "/v1/agents/tools", "/v1/agents/tools/run",
		"/v1/agents/approvals/x", "/v1/agents/approvals/x/y",
	} {
		for _, m := range guardMethods {
			allowed[m+" "+p] = true
		}
	}
	return allowed
}

var guardMethods = []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete}

func TestRoleGuardEveryRoute(t *testing.T) {
	e := newAccessTestServer(t)
	m := e.member(t, "user_m", "m@beknown.work")
	adminCookie := e.cookieFor(t, e.admin.ID)
	memberCookie := e.cookieFor(t, m.ID)

	// The guard in front of a stub: 204 means "passed through". The stub
	// also records whether the guard cached the user in the context.
	var sawUser *identity.User
	guard := e.srv.RoleGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUser, _ = r.Context().Value(ctxUserKey).(*identity.User)
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(cookie *http.Cookie, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		return rec
	}
	isAdminOnly := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), `"admin_only"`)
	}

	allowed := memberAllowlist()
	paths := concretePaths(registeredRoutePaths(t))
	checked := 0
	for _, p := range paths {
		for _, method := range guardMethods {
			key := method + " " + p
			rec := call(memberCookie, method, p)
			switch {
			case allowed[key] && rec.Code != http.StatusNoContent:
				t.Errorf("member %s: got %d %s, want pass-through", key, rec.Code, strings.TrimSpace(rec.Body.String()))
			case !allowed[key] && !isAdminOnly(rec):
				t.Errorf("member %s: got %d %s, want 403 admin_only", key, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			sawUser = nil
			if rec := call(adminCookie, method, p); rec.Code != http.StatusNoContent || isAdminOnly(rec) {
				t.Errorf("admin %s: got %d %s, want pass-through", key, rec.Code, strings.TrimSpace(rec.Body.String()))
			} else if sawUser == nil || sawUser.ID != e.admin.ID {
				t.Errorf("admin %s: user not cached in context (%v)", key, sawUser)
			}
			checked++
		}
	}
	// Every allowlisted entry must correspond to a real registered route,
	// otherwise the allowlist is drifting from the router.
	pathSet := map[string]bool{}
	for _, p := range paths {
		pathSet[p] = true
	}
	for key := range allowed {
		_, p, _ := strings.Cut(key, " ")
		if !pathSet[p] {
			t.Errorf("allowlist names %q, which no registered route serves", key)
		}
	}
	t.Logf("checked %d method×path combinations over %d paths", checked, len(paths))
}

func TestRoleGuardPassThrough(t *testing.T) {
	e := newAccessTestServer(t)
	m := e.member(t, "user_m", "m@beknown.work")
	guard := e.srv.RoleGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(cookie *http.Cookie, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		return rec.Code
	}
	// No cookie: the handler decides (bearer routes, login, 401s).
	if c := call(nil, http.MethodGet, "/v1/settings"); c != http.StatusNoContent {
		t.Errorf("no cookie: %d", c)
	}
	// Garbage cookie: same.
	if c := call(&http.Cookie{Name: sessionCookieName, Value: "nonsense"}, http.MethodGet, "/v1/settings"); c != http.StatusNoContent {
		t.Errorf("garbage cookie: %d", c)
	}
	// Blocked member: the cookie no longer resolves, so pass through and
	// let the handler 401.
	memberCookie := e.cookieFor(t, m.ID)
	if c := call(memberCookie, http.MethodGet, "/v1/settings"); c != http.StatusForbidden {
		t.Errorf("active member on admin route: %d, want 403", c)
	}
	if err := e.id.SetStatus(t.Context(), m.ID, identity.StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	if c := call(memberCookie, http.MethodGet, "/v1/settings"); c != http.StatusNoContent {
		t.Errorf("blocked member: %d, want pass-through", c)
	}
	if err := e.id.SetStatus(t.Context(), m.ID, identity.StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	// Outside /v1 the guard does nothing.
	memberCookie = e.cookieFor(t, m.ID)
	if c := call(memberCookie, http.MethodGet, "/login"); c != http.StatusNoContent {
		t.Errorf("member on /login: %d", c)
	}
	// Unclean paths are refused for members: the mux would redirect them
	// anyway, but the guard doesn't guess.
	if c := call(memberCookie, http.MethodGet, "/v1/agents/../users"); c != http.StatusForbidden {
		t.Errorf("member unclean path: %d, want 403", c)
	}
	// Full chain: a member hitting an admin route through the real mux gets
	// the guard's 403, not the handler's answer.
	rec := e.do(t, memberCookie, http.MethodGet, "/v1/settings", "")
	if rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "admin_only" {
		t.Errorf("member GET /v1/settings via chain: %d %s", rec.Code, rec.Body.String())
	}
	// And an allowed member route reaches its handler.
	if rec := e.do(t, memberCookie, http.MethodGet, "/v1/agents", ""); rec.Code != http.StatusOK {
		t.Errorf("member GET /v1/agents via chain: %d %s", rec.Code, rec.Body.String())
	}
}

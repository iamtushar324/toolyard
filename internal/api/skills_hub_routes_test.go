package api

// skillhub: the agent routes (slice B, skills_routes.go) and the admin
// routes (slice C, skills_admin_routes.go) share /v1/skills/sources. B
// registers the plain path and serves only a GET with an agent or session
// bearer; C registers the method pattern POST /v1/skills/sources (create)
// and the /v1/skills/sources/ subtree. These tests pin that layout: the
// mux builds without a duplicate-pattern panic, GET reaches B and POST
// reaches C.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func TestSkillsRoutesMuxBuilds(t *testing.T) {
	db := storetest.Open(t)
	auditLog := audit.New(db)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(), Access: access.New(db),
	})
	gw.RegisterBuiltins()
	gw.RegisterSkillHub(gateway.SkillHub{Reader: &restSkills{}, Tickets: gateway.NewSkillBundleTickets(db)})
	t.Cleanup(func() { _ = gw.Close() })
	srvCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{
		Identity: identity.New(db), Audit: auditLog, Gateway: gw,
		SkillsAdmin: &skillsAdminFake{},
		SessionKey:  []byte("0123456789abcdef0123456789abcdef"),
	})
	mux := http.NewServeMux()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("registering the routes panicked (a duplicate pattern?): %v", r)
			}
		}()
		srv.Routes(mux)
	}()
	for _, c := range []struct{ method, path, pattern string }{
		{http.MethodGet, "/v1/skills/sources", "/v1/skills/sources"},
		{http.MethodPost, "/v1/skills/sources", "POST /v1/skills/sources"},
		{http.MethodPatch, "/v1/skills/sources", "/v1/skills/sources"},
		{http.MethodGet, "/v1/skills/sources/src_1", "/v1/skills/sources/"},
		{http.MethodPost, "/v1/skills/sources/inspect", "/v1/skills/sources/"},
		{http.MethodPost, "/v1/skills/sources/src_1/webhook", "/v1/skills/sources/"},
		{http.MethodGet, "/v1/skills/admin/sources", "/v1/skills/admin/sources"},
		{http.MethodGet, "/v1/skills/search", "/v1/skills/search"},
		{http.MethodPost, "/v1/skills/bundle", "/v1/skills/bundle"},
		{http.MethodGet, "/v1/skills/bundle/tyskb_x", "/v1/skills/bundle/"},
	} {
		if _, got := mux.Handler(httptest.NewRequest(c.method, c.path, nil)); got != c.pattern {
			t.Errorf("%s %s routes to %q, want %q", c.method, c.path, got, c.pattern)
		}
	}
}

func TestSkillsSourcesSharedPath(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	e.gw.RegisterSkillHub(gateway.SkillHub{Reader: &restSkills{}})
	tok, _ := e.agentFor(t, e.admin.ID)

	// GET with an agent bearer: slice B's agent listing (skills.sources).
	rec := e.bearer(t, tok, http.MethodGet, "/v1/skills/sources", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("agent GET /v1/skills/sources = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := skillsJSON(t, rec)["sources"]; !ok {
		t.Fatalf("agent GET /v1/skills/sources body %s: no sources", rec.Body.String())
	}
	if fake.called("ListSources") {
		t.Fatal("the agent GET reached the admin service")
	}

	// POST with an admin session: slice C's create.
	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills"}`)
	if rec.Code != http.StatusCreated || !fake.called("CreateSource") {
		t.Fatalf("admin POST /v1/skills/sources = %d %s (calls %v)", rec.Code, rec.Body.String(), fake.calls)
	}

	// GET with an admin session and no bearer: not the agent listing; the
	// admin list is GET /v1/skills/admin/sources.
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/sources", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("admin GET /v1/skills/sources = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/admin/sources", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin GET /v1/skills/admin/sources = %d %s", rec.Code, rec.Body.String())
	}

	// POST with an agent bearer reaches the admin create, which refuses
	// anyone without an admin session.
	fake.mu.Lock()
	fake.calls = nil
	fake.mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/sources", strings.NewReader(`{"url":"acme/skills"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || fake.called("CreateSource") {
		t.Fatalf("agent POST /v1/skills/sources = %d %s (calls %v), want 401 and no create", rec.Code, rec.Body.String(), fake.calls)
	}
}

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// fakeClerk satisfies clerkDirectory with programmable answers.
type fakeClerk struct {
	mu         sync.Mutex
	claims     clerk.Claims
	verifyErr  error
	member     clerk.Member
	memberErr  error
	members    map[string]clerk.Member
	membersErr error
	calls      int
}

func (f *fakeClerk) set(fn func(*fakeClerk)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

func (f *fakeClerk) VerifySessionToken(_ context.Context, token string) (clerk.Claims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyErr != nil {
		return clerk.Claims{}, f.verifyErr
	}
	if token != "good-token" {
		return clerk.Claims{}, clerk.ErrInvalidToken
	}
	return f.claims, nil
}

func (f *fakeClerk) OrgMembership(_ context.Context, _ string) (clerk.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberErr != nil {
		return clerk.Member{}, f.memberErr
	}
	return f.member, nil
}

func (f *fakeClerk) OrgMembers(_ context.Context) (map[string]clerk.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.membersErr != nil {
		return nil, f.membersErr
	}
	return f.members, nil
}

// accessTestEnv is a Server with identity, audit, access, a gateway with
// the built-in tools registered, and a fake Clerk, behind the
// production-like HardenAPI -> RoleGuard -> mux chain.
type accessTestEnv struct {
	srv     *Server
	handler http.Handler
	db      *store.DB
	id      *identity.Service
	access  *access.Service
	audit   *audit.Logger
	gw      *gateway.Gateway
	clerk   *fakeClerk
	admin   *identity.User
}

func newAccessTestServer(t *testing.T) *accessTestEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "access-api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	id := identity.New(db)
	admin, err := id.CreateUser(ctx, "admin", "long-enough-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	auditLog := audit.New(db)
	acc := access.New(db)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(), Access: acc,
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	upSvc := upstreams.New(db, gw)
	fc := &fakeClerk{
		claims: clerk.Claims{Subject: "user_ada", SessionID: "sess_1", AuthorizedParty: "https://toolyard.example.com"},
		member: clerk.Member{IsMember: true, Role: "org:member", Email: "ada@beknown.work", FirstName: "Ada", LastName: "Lovelace", ImageURL: "https://img.clerk.com/ada"},
	}

	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{
		Identity:   id,
		Audit:      auditLog,
		Access:     acc,
		Gateway:    gw,
		Upstreams:  upSvc,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		OwnerEmail: "owner@beknown.work",
	})
	srv.clerk = fc
	srv.clerkPublishableKey = "pk_test_Y2xlcmsuZXhhbXBsZS50ZXN0JA"
	srv.clerkFrontendAPI = "clerk.example.test"

	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	return &accessTestEnv{srv: srv, handler: h, db: db, id: id, access: acc, audit: auditLog, gw: gw, clerk: fc, admin: admin}
}

// seedServer inserts an upstream server row without dialling anything.
func (e *accessTestEnv) seedServer(t *testing.T, name, status string, enabled bool) {
	t.Helper()
	en := 0
	if enabled {
		en = 1
	}
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO upstream_servers(name, transport, url, enabled, last_status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`, name, "http", "http://127.0.0.1:1/mcp", en, status, now, now); err != nil {
		t.Fatalf("seed server %s: %v", name, err)
	}
}

// member creates a Clerk-linked member directly through identity.
func (e *accessTestEnv) member(t *testing.T, clerkID, email string) *identity.User {
	t.Helper()
	u, err := e.id.UpsertClerkUser(context.Background(), identity.ClerkProfile{ClerkUserID: clerkID, Email: email, DisplayName: email}, "")
	if err != nil {
		t.Fatalf("upsert member %s: %v", clerkID, err)
	}
	return u
}

// cookieFor mints a session cookie for uid the way the login routes do.
func (e *accessTestEnv) cookieFor(t *testing.T, uid string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := e.srv.issueSession(rec, req, uid); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("no session cookie issued")
	return nil
}

// do sends a dashboard-style request (CSRF header, JSON content type when
// there is a body) through the full handler chain.
func (e *accessTestEnv) do(t *testing.T, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// auditRows counts audit events of one type, optionally filtered on reason.
func (e *accessTestEnv) auditRows(t *testing.T, eventType, reason string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM audit_events WHERE event_type = ?`
	args := []any{eventType}
	if reason != "" {
		q += ` AND reason = ?`
		args = append(args, reason)
	}
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

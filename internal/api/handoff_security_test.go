package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

const handoffTestOrigin = "https://toolyard.example"

// handoffTestEnv is the inbox API fixture with federation wired, a public URL,
// and the production middleware chain in front of the mux.
type handoffTestEnv struct {
	*inboxAPIFixture
	fed     *federation.Service
	handler http.Handler
	// userID is the signed-in fixture user; pendingID is a pending Inbox
	// request that user owns.
	userID    string
	pendingID string
}

func newHandoffTestEnv(t *testing.T) *handoffTestEnv {
	t.Helper()
	f := newInboxAPIFixture(t)
	sc := f.agentRun(t, "inbox.request", accessRequest("memory.delete"))
	pendingID, _ := sc["request_id"].(string)
	if sc["ok"] != true || pendingID == "" {
		t.Fatalf("inbox request: %v", sc)
	}
	f.svc.Flush()
	cipher, err := sealbox.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fed, err := federation.New(t.Context(), f.db, f.srv.identity, cipher)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.federation = fed
	f.srv.security.PublicURL = handoffTestOrigin
	handler := f.srv.SecurityHeaders(f.srv.EnforceOriginOnMutations(f.srv.HardenAPI(f.srv.RoleGuard(f.mux))))
	return &handoffTestEnv{inboxAPIFixture: f, fed: fed, handler: handler, userID: f.ownerID(t), pendingID: pendingID}
}

func (e *handoffTestEnv) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// post sends JSON the way a server-side client does: an optional bearer, no
// cookie and no Origin.
func (e *handoffTestEnv) post(t *testing.T, bearer, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return e.serve(req)
}

// postAsDashboard sends a mutation with the dashboard's own Origin, its CSRF
// header and a JSON body, but no session cookie.
func (e *handoffTestEnv) postAsDashboard(t *testing.T, path string, body any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", handoffTestOrigin)
	req.Header.Set("X-Requested-With", "toolyard")
	return e.serve(req).Code
}

func (e *handoffTestEnv) sessionRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// linkURI reads a {"url": ...} handoff response and returns the request URI
// to open on this instance.
func (e *handoffTestEnv) linkURI(t *testing.T, rec *httptest.ResponseRecorder, path string) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("handoff link: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(out["url"])
	if err != nil || target.Scheme+"://"+target.Host != handoffTestOrigin || target.Path != path || target.Query().Get("code") == "" {
		t.Fatalf("handoff link destination: %v", err)
	}
	return target.RequestURI()
}

// localLink mints a handoff link with a local derived credential.
func (e *handoffTestEnv) localLink(t *testing.T) string {
	t.Helper()
	rec := e.post(t, e.token, "/v1/connections/api-key", map[string]any{"environment_id": "handoff-env", "local_user_id": "owner", "instance_id": e.fed.InstanceID, "expected_version": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("key exchange: %d %s", rec.Code, rec.Body.String())
	}
	var credential federation.Credential
	if err := json.Unmarshal(rec.Body.Bytes(), &credential); err != nil || credential.Token == "" {
		t.Fatalf("local credential: %v", err)
	}
	return e.linkURI(t, e.post(t, credential.Token, "/v1/connections/handoff", map[string]string{}), "/connections/handoff")
}

// hostLink runs the host consent flow, then mints a handoff link with the
// claimed host credential.
func (e *handoffTestEnv) hostLink(t *testing.T) string {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := uuid.NewString()
	proof := func(action string) map[string]string {
		now := time.Now()
		c := federation.HostClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "handoff-host", Subject: "local-user", Audience: jwt.ClaimStrings{e.fed.InstanceID}, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Action: action, RequestID: request, PublicKey: base64.StdEncoding.EncodeToString(pub), HostName: "Handoff host", Platform: "linux"}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{"proof": raw}
	}
	begin := e.post(t, "", "/v1/connections/host/begin", proof("begin"))
	if begin.Code != http.StatusOK {
		t.Fatalf("host begin: %d %s", begin.Code, begin.Body.String())
	}
	var started map[string]string
	if err = json.Unmarshal(begin.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	consent, err := url.Parse(started["authorization_url"])
	if err != nil {
		t.Fatal(err)
	}
	// The owner's decision, as the signed-in consent form records it.
	if _, err = e.fed.DecideHost(t.Context(), consent.Query().Get("request"), e.userID, true); err != nil {
		t.Fatal("host consent", err)
	}
	poll := e.post(t, "", "/v1/connections/host/poll", proof("poll"))
	var claimed struct {
		federation.Credential
		Status string `json:"status"`
	}
	if err = json.Unmarshal(poll.Body.Bytes(), &claimed); err != nil || poll.Code != http.StatusOK || claimed.Status != "approved" || claimed.Token == "" {
		t.Fatalf("host claim: %d %v", poll.Code, err)
	}
	return e.linkURI(t, e.post(t, claimed.Token, "/v1/connections/host/handoff", map[string]string{}), "/connections/host/handoff")
}

// federationLink registers a federated environment, connects a person with
// a signed assertion, then mints a handoff link with another assertion.
func (e *handoffTestEnv) federationLink(t *testing.T) string {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.fed.Membership = func(_ context.Context, subject string) (federation.Profile, error) {
		return federation.Profile{Email: subject + "@example.test", Name: subject}, nil
	}
	const issuer, origin = "handoff-env", "https://t3.example.test"
	if err = e.fed.Register(t.Context(), federation.Trust{Issuer: issuer, PublicKey: base64.StdEncoding.EncodeToString(pub), Kid: "key-1", Origin: origin}, e.userID); err != nil {
		t.Fatal(err)
	}
	assertion := func() string {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, federation.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: "handoff-person", Audience: jwt.ClaimStrings{e.fed.InstanceID}, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}})
		tok.Header["kid"] = "key-1"
		raw, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if rec := e.post(t, "", "/v1/federation/connect", map[string]any{"assertion": assertion(), "expected_version": 0}); rec.Code != http.StatusOK {
		t.Fatalf("federation connect: %d %s", rec.Code, rec.Body.String())
	}
	return e.linkURI(t, e.post(t, "", "/v1/federation/handoff", map[string]any{"assertion": assertion(), "return_url": origin + "/settings"}), "/federation/handoff")
}

// openLink follows a handoff link as a browser does, with cookie (nil for a
// browser that is not signed in), and checks that the response redirects to
// the Inbox and sets no cookie.
func (e *handoffTestEnv) openLink(t *testing.T, uri string, cookie *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := e.serve(req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/#inbox" {
		t.Fatalf("handoff open: %d Location=%q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("handoff link set %d cookie(s)", len(got))
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("handoff headers: Cache-Control=%q Referrer-Policy=%q", rec.Header().Get("Cache-Control"), rec.Header().Get("Referrer-Policy"))
	}
}

// Handoff links open the Inbox; they never sign a browser in.
func TestHandoffLinksNeverIssueSessions(t *testing.T) {
	for _, tc := range []struct {
		source string
		link   func(*handoffTestEnv, *testing.T) string
	}{
		{handoffSourceLocal, (*handoffTestEnv).localLink},
		{handoffSourceHost, (*handoffTestEnv).hostLink},
		{handoffSourceFederation, (*handoffTestEnv).federationLink},
	} {
		t.Run(tc.source, func(t *testing.T) {
			e := newHandoffTestEnv(t)
			uri := tc.link(e, t)
			before := e.sessionRows(t)
			e.openLink(t, uri, nil)
			if after := e.sessionRows(t); after != before {
				t.Fatalf("sessions rows changed: %d -> %d", before, after)
			}
			var opened int
			if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='dashboard.handoff_opened' AND reason=?`, tc.source).Scan(&opened); err != nil || opened != 1 {
				t.Fatalf("handoff audit event: %d %v", opened, err)
			}
			// The browser that opened the link holds no session, so dashboard
			// mutations from it are refused even with the dashboard's headers.
			decide := map[string]any{"action": "submit", "request_revision": 1, "submission_id": "handoff_check",
				"verdicts": map[string]any{"delete_preview": map[string]string{"verdict": "accepted"}}}
			if code := e.postAsDashboard(t, "/v1/inbox/"+e.pendingID+"/decide", decide); code != http.StatusUnauthorized {
				t.Fatalf("inbox decide without a session: %d", code)
			}
			if code := e.postAsDashboard(t, "/v1/policies", map[string]any{"scope": "tool", "target": "memory.delete", "action": "allow"}); code != http.StatusUnauthorized {
				t.Fatalf("policy change without a session: %d", code)
			}
			if req, err := e.svc.Get(t.Context(), e.pendingID); err != nil || req.Status != inbox.StatusPending {
				t.Fatalf("inbox request changed: %v", err)
			}
			// A code works once.
			if rec := e.serve(httptest.NewRequest(http.MethodGet, uri, nil)); rec.Code != http.StatusUnauthorized {
				t.Fatalf("handoff replay: %d", rec.Code)
			}
		})
	}
}

// A browser that is already signed in keeps its own session and gets no new one.
func TestHandoffWithSignedInBrowserKeepsExistingSession(t *testing.T) {
	e := newHandoffTestEnv(t)
	uri := e.localLink(t)
	before := e.sessionRows(t)
	e.openLink(t, uri, e.cookie)
	if after := e.sessionRows(t); after != before {
		t.Fatalf("sessions rows changed: %d -> %d", before, after)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
	req.AddCookie(e.cookie)
	if rec := e.serve(req); rec.Code != http.StatusOK {
		t.Fatalf("existing session after handoff: %d", rec.Code)
	}
}

// Only password, Clerk and first-run sign-in may create a dashboard session.
func TestIssueSessionOnlyAfterHumanProof(t *testing.T) {
	allowed := map[string]bool{"authSetup": true, "authLogin": true, "authClerkSession": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			caller, node := "package scope", ast.Node(decl)
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Name.Name == "issueSession" && fn.Recv != nil {
					declared = true
				}
				if fn.Body == nil {
					continue
				}
				caller, node = fn.Name.Name, fn.Body
			}
			// Any reference counts: a call, a method value or a function literal.
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "issueSession" && !allowed[caller] {
					t.Errorf("%s: %s uses issueSession; only authSetup, authLogin and authClerkSession may create a session", fset.Position(id.Pos()), caller)
				}
				return true
			})
		}
	}
	if !declared {
		t.Fatal("issueSession declaration not found in the package sources")
	}
}

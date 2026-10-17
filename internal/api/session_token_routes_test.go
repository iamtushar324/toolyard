package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// sessTokEnv is a Server with the session token routes behind the
// production-like HardenAPI -> RoleGuard -> mux chain, an owner with two
// agents, and an operator token.
type sessTokEnv struct {
	db      *store.DB
	srv     *Server
	handler http.Handler
	id      *identity.Service
	owner   *identity.User
	tokA    string
	agA     *identity.Agent
	tokB    string
	agB     *identity.Agent
	opTok   string
}

func newSessTokEnv(t *testing.T) *sessTokEnv {
	t.Helper()
	db := storetest.Open(t)
	ctx := context.Background()
	id := identity.New(db)
	owner, err := id.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	tokA, agA, err := id.CreateAgentWithToken(ctx, owner.ID, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, agB, err := id.CreateAgentWithToken(ctx, owner.ID, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	opTok, _, err := id.CreateOperatorToken(ctx, owner.ID, "cli", nil, 0, "cli")
	if err != nil {
		t.Fatal(err)
	}
	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{Identity: id, Audit: audit.New(db), SessionKey: []byte("0123456789abcdef0123456789abcdef")})
	mux := http.NewServeMux()
	srv.registerSessionTokenRoutes(mux)
	return &sessTokEnv{db: db, srv: srv, handler: srv.HardenAPI(srv.RoleGuard(mux)), id: id, owner: owner,
		tokA: tokA, agA: agA, tokB: tokB, agB: agB, opTok: opTok}
}

// call sends a bearer request (no cookie); token "" sends no Authorization.
func (e *sessTokEnv) call(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// mint mints a session token over the route and fails the test unless 201.
func (e *sessTokEnv) mint(t *testing.T, agentTok, sessionID string) sessionTokenView {
	t.Helper()
	rec := e.call(t, http.MethodPost, sessionTokensPath, agentTok, `{"session_id":"`+sessionID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var v sessionTokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// caller runs codeCaller on a request carrying token.
func (e *sessTokEnv) caller(token string) (*identity.Agent, string, bool, error) {
	req := httptest.NewRequest(http.MethodPost, "/v1/code/run", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return e.srv.codeCaller(req)
}

func TestSessionTokenRouteMintAndCodeCaller(t *testing.T) {
	e := newSessTokEnv(t)
	before := time.Now().UnixMilli()
	rec := e.call(t, http.MethodPost, sessionTokensPath, e.tokA, `{"session_id":"t3-thread-123","ttl_seconds":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var v sessionTokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.Token, "tys_"+v.TokenID+".") || !strings.HasPrefix(v.TokenID, "st_") ||
		v.AgentID != e.agA.ID || v.SessionID != "t3-thread-123" {
		t.Fatalf("view = token %.12s… %+v", v.Token, sessionTokenView{TokenID: v.TokenID, AgentID: v.AgentID, SessionID: v.SessionID})
	}
	if v.ExpiresAt < before+600_000 || v.ExpiresAt > time.Now().UnixMilli()+600_000 {
		t.Fatalf("expires_at %d: want 600 s from now", v.ExpiresAt)
	}

	// codeCaller: the session token resolves to its agent and session.
	ag, sid, via, err := e.caller(v.Token)
	if err != nil || ag.ID != e.agA.ID || ag.Owner != e.owner.ID || sid != "t3-thread-123" || !via {
		t.Fatalf("codeCaller(session) = %v %q %v %v", ag, sid, via, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/code/run", nil)
	req.Header.Set(headerClientKind, "t3")
	rs := e.srv.codeRaiser(req, ag, sid, via, clientKindCLI, viaCLI)
	if rs.CallerID != e.agA.ID || rs.OwnerUserID != e.owner.ID || rs.ClientSessionID != "t3-thread-123" || rs.ClientSessionClaimed || rs.ClientKind != "t3" {
		t.Fatalf("codeRaiser(session) = %+v", rs)
	}
	// ... and an agent token resolves to its agent with no session.
	ag, sid, via, err = e.caller(e.tokA)
	if err != nil || ag.ID != e.agA.ID || sid != "" || via {
		t.Fatalf("codeCaller(agent) = %v %q %v %v", ag, sid, via, err)
	}
	if rs := e.srv.codeRaiser(req, ag, sid, via, clientKindCLI, viaCLI); rs.ClientSessionID != "" || rs.ClientSessionClaimed {
		t.Fatalf("codeRaiser(agent) = %+v", rs)
	}

	// The mint is audited with the token id, never the token.
	var agentID, summary string
	if err := e.db.QueryRow(`SELECT agent_id, result_summary FROM audit_events WHERE event_type = ?`, eventSessionTokenMint).Scan(&agentID, &summary); err != nil {
		t.Fatal(err)
	}
	if agentID != e.agA.ID || summary != v.TokenID {
		t.Fatalf("audit row: agent %q summary %q", agentID, summary)
	}
	assertNoTokenInAudit(t, e.db, v.Token)
}

// assertNoTokenInAudit fails when any audit row holds token or its secret.
func assertNoTokenInAudit(t *testing.T, db *store.DB, token string) {
	t.Helper()
	secret := token[strings.IndexByte(token, '.')+1:]
	rows, err := db.Query(`SELECT COALESCE(arguments,''), COALESCE(result_summary,''), COALESCE(reason,'') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(a+b+c, secret) {
			t.Fatal("an audit row holds the session token")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionTokenRouteMintRefusals(t *testing.T) {
	e := newSessTokEnv(t)
	sess := e.mint(t, e.tokA, "s1")
	body := `{"session_id":"s2"}`
	for name, c := range map[string]struct {
		method, token, body string
		want                int
		errText             string
	}{
		"no bearer":           {http.MethodPost, "", body, http.StatusUnauthorized, "bearer token required"},
		"bad agent token":     {http.MethodPost, e.agA.ID + ".wrong", body, http.StatusUnauthorized, "invalid agent token"},
		"bad session token":   {http.MethodPost, sess.Token + "x", body, http.StatusUnauthorized, "invalid session token"},
		"session token mints": {http.MethodPost, sess.Token, body, http.StatusForbidden, "a session token cannot mint session tokens"},
		"operator token":      {http.MethodPost, e.opTok, body, http.StatusForbidden, ""},
		"no session_id":       {http.MethodPost, e.tokA, `{}`, http.StatusBadRequest, identity.ErrSessionIDInvalid.Error()},
		"long session_id":     {http.MethodPost, e.tokA, `{"session_id":"` + strings.Repeat("a", 129) + `"}`, http.StatusBadRequest, identity.ErrSessionIDInvalid.Error()},
		"control character":   {http.MethodPost, e.tokA, `{"session_id":"a\u0007b"}`, http.StatusBadRequest, identity.ErrSessionIDInvalid.Error()},
		"negative ttl":        {http.MethodPost, e.tokA, `{"session_id":"s","ttl_seconds":-5}`, http.StatusBadRequest, ""},
		"unknown field":       {http.MethodPost, e.tokA, `{"session_id":"s","scope":"all"}`, http.StatusBadRequest, ""},
		"not json":            {http.MethodPost, e.tokA, `{"session_id":`, http.StatusBadRequest, ""},
		"GET":                 {http.MethodGet, e.tokA, "", http.StatusMethodNotAllowed, ""},
	} {
		rec := e.call(t, c.method, sessionTokensPath, c.token, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), c.want)
			continue
		}
		if c.errText != "" && !strings.Contains(rec.Body.String(), c.errText) {
			t.Errorf("%s: body %s, want %q", name, rec.Body.String(), c.errText)
		}
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM agent_session_tokens`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d %v: a refused mint wrote a token", n, err)
	}

	// ttl_seconds is clamped to 60..3600; absent or 0 is 3600.
	for ttl, want := range map[string]int64{"1": 60, "59": 60, "3600": 3600, "99999999999999999": 3600, "0": 3600, "": 3600} {
		b := `{"session_id":"s"}`
		if ttl != "" {
			b = `{"session_id":"s","ttl_seconds":` + ttl + `}`
		}
		before := time.Now().UnixMilli()
		rec := e.call(t, http.MethodPost, sessionTokensPath, e.tokA, b)
		if rec.Code != http.StatusCreated {
			t.Fatalf("ttl %q: %d %s", ttl, rec.Code, rec.Body.String())
		}
		var v sessionTokenView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.ExpiresAt < before+want*1000 || v.ExpiresAt > time.Now().UnixMilli()+want*1000 {
			t.Errorf("ttl %q: expires_at %d, want %d s from now", ttl, v.ExpiresAt, want)
		}
	}
}

func TestSessionTokenRouteRevoke(t *testing.T) {
	e := newSessTokEnv(t)
	a1 := e.mint(t, e.tokA, "a1")
	a2 := e.mint(t, e.tokA, "a2")
	a3 := e.mint(t, e.tokA, "a3")
	b1 := e.mint(t, e.tokB, "b1")
	path := func(id string) string { return sessionTokensPrefix + id }

	// The parent agent revokes its own token: 204, then 204 again.
	if rec := e.call(t, http.MethodDelete, path(a1.TokenID), e.tokA, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, _, err := e.caller(a1.Token); !errors.Is(err, errInvalidSessionToken) {
		t.Fatalf("revoked token: %v", err)
	}
	if rec := e.call(t, http.MethodDelete, path(a1.TokenID), e.tokA, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke again: %d %s", rec.Code, rec.Body.String())
	}
	// An unknown id is 204; another agent's id is 404 and changes nothing.
	if rec := e.call(t, http.MethodDelete, path("st_00112233445566778899aabb"), e.tokA, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unknown id: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.call(t, http.MethodDelete, path(b1.TokenID), e.tokA, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another agent's id: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, _, err := e.caller(b1.Token); err != nil {
		t.Fatalf("a refused revoke changed the other agent's token: %v", err)
	}

	// A session token revokes itself (also twice), never another token.
	if rec := e.call(t, http.MethodDelete, path(a3.TokenID), a2.Token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("session token revoking a sibling: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.call(t, http.MethodDelete, path(b1.TokenID), a2.Token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("session token revoking another agent's token: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 2; i++ {
		if rec := e.call(t, http.MethodDelete, path(a2.TokenID), a2.Token, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("self revoke %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if _, _, _, err := e.caller(a2.Token); !errors.Is(err, errInvalidSessionToken) {
		t.Fatalf("self-revoked token: %v", err)
	}
	// A revoked session token naming another id is refused as a bad token.
	if rec := e.call(t, http.MethodDelete, path(a3.TokenID), a2.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session token: %d %s", rec.Code, rec.Body.String())
	}
	// The self revoke is audited with the verified client session.
	var sid string
	var claimed int
	if err := e.db.QueryRow(`SELECT COALESCE(client_session_id,''), client_session_claimed FROM audit_events WHERE event_type = ? AND result_summary = ?`,
		eventSessionTokenRevoke, a2.TokenID).Scan(&sid, &claimed); err != nil {
		t.Fatal(err)
	}
	if sid != "a2" || claimed != 0 {
		t.Fatalf("self revoke audit: session %q claimed %d", sid, claimed)
	}

	for name, c := range map[string]struct {
		method, path, token string
		want                int
	}{
		"operator token":  {http.MethodDelete, path(a3.TokenID), e.opTok, http.StatusForbidden},
		"no bearer":       {http.MethodDelete, path(a3.TokenID), "", http.StatusUnauthorized},
		"bad agent token": {http.MethodDelete, path(a3.TokenID), e.agA.ID + ".wrong", http.StatusUnauthorized},
		"wrong secret":    {http.MethodDelete, path(a3.TokenID), strings.Replace(a3.Token, ".", ".X", 1), http.StatusUnauthorized},
		"GET":             {http.MethodGet, path(a3.TokenID), e.tokA, http.StatusMethodNotAllowed},
		"no id":           {http.MethodDelete, sessionTokensPrefix, e.tokA, http.StatusNotFound},
		"nested path":     {http.MethodDelete, path(a3.TokenID) + "/x", e.tokA, http.StatusNotFound},
	} {
		if rec := e.call(t, c.method, c.path, c.token, ""); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if _, _, _, err := e.caller(a3.Token); err != nil {
		t.Fatalf("a3 was revoked by a refused request: %v", err)
	}
	assertNoTokenInAudit(t, e.db, a3.Token)
}

// TestCodeCallerFollowsParentAndRefusesOperators: a session token stops with
// its parent agent token; operator tokens and missing bearers get their own
// errors, and writeCallerError maps each to its answer.
func TestCodeCallerFollowsParentAndRefusesOperators(t *testing.T) {
	e := newSessTokEnv(t)
	sess := e.mint(t, e.tokA, "s")
	if _, err := e.id.RotateAgentToken(context.Background(), e.owner.ID, e.agA.ID, 0); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		token string
		want  error
		code  int
	}{
		"rotated parent":  {sess.Token, errInvalidSessionToken, http.StatusUnauthorized},
		"rotated agent":   {e.tokA, errInvalidAgentToken, http.StatusUnauthorized},
		"operator token":  {e.opTok, errOperatorToken, http.StatusForbidden},
		"no bearer":       {"", errBearerRequired, http.StatusUnauthorized},
		"garbage session": {"tys_nope", errInvalidSessionToken, http.StatusUnauthorized},
	} {
		_, _, _, err := e.caller(c.token)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
			continue
		}
		rec := httptest.NewRecorder()
		writeCallerError(rec, err)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want.Error()) {
			t.Errorf("%s: wrote %d %s, want %d", name, rec.Code, rec.Body.String(), c.code)
		}
	}
	// Agent B's token still works, and a failed check is 503, not 401.
	if ag, _, _, err := e.caller(e.tokB); err != nil || ag.ID != e.agB.ID {
		t.Fatalf("agent B: %v", err)
	}
	if got := callerError(errors.New("database is down"), errInvalidAgentToken); !errors.Is(got, errCallerCheckFailed) {
		t.Fatalf("callerError(db) = %v", got)
	}
	rec := httptest.NewRecorder()
	writeCallerError(rec, errCallerCheckFailed)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("check failure: %d %v", rec.Code, rec.Header())
	}
}

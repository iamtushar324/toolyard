package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// registryFake stands in for the gateway behind the identity key service:
// it records each registry call's target and caller, keeps the registered
// fingerprints (delete of an unknown hash is "not found", list returns
// them, as prime does) and fails a target that is marked failing.
type registryFake struct {
	mu      sync.Mutex
	targets []string
	callers []string
	fail    map[string]bool
	hashes  map[string]bool
}

func (f *registryFake) CallInternal(ctx context.Context, _, target string, args map[string]any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, target)
	f.callers = append(f.callers, gateway.AgentIDFromContext(ctx))
	if f.fail[target] {
		return mcp.NewToolResultError("registry says no"), nil
	}
	if f.hashes == nil {
		f.hashes = map[string]bool{}
	}
	hash, _ := args["virtualKeyHash"].(string)
	_, tool, _ := strings.Cut(target, ".")
	switch tool {
	case identitykeys.ToolUpsert:
		f.hashes[hash] = true
	case identitykeys.ToolDelete:
		if !f.hashes[hash] {
			return mcp.NewToolResultError("Virtual Key mapping not found"), nil
		}
		delete(f.hashes, hash)
	case identitykeys.ToolList:
		var b strings.Builder
		for h := range f.hashes {
			b.WriteString(`{"virtualKeyHash":"` + h + `"},`)
		}
		return mcp.NewToolResultText("[" + strings.TrimSuffix(b.String(), ",") + "]"), nil
	}
	return mcp.NewToolResultText(`{"ok":true}`), nil
}

func (f *registryFake) has(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hashes[hash]
}

func (f *registryFake) take() (targets, callers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	targets, callers = f.targets, f.callers
	f.targets, f.callers = nil, nil
	return targets, callers
}

// withIdentityKeys wires an identity key service (one registry upstream,
// "BkCoreServices") into the test server and returns the registry fake.
func withIdentityKeys(t *testing.T, e *accessTestEnv) *registryFake {
	t.Helper()
	key := make([]byte, sealbox.MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := sealbox.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	reg := &registryFake{fail: map[string]bool{}}
	svc := identitykeys.New(e.db, cipher)
	svc.SetAudit(e.audit)
	svc.SetCaller(reg)
	svc.SetRegistries(identitykeys.RegistryListerFunc(func(context.Context) ([]string, error) {
		return []string{"BkCoreServices"}, nil
	}))
	e.srv.identityKeys = svc
	return reg
}

func decodeStatus(t *testing.T, body []byte) identitykeys.Status {
	t.Helper()
	var st identitykeys.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode status: %v: %s", err, body)
	}
	return st
}

func TestIdentityKeyRoutesLifecycle(t *testing.T) {
	e := newAccessTestServer(t)
	reg := withIdentityKeys(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "mira@beknown.work")
	mCookie := e.cookieFor(t, m.ID)
	path := "/v1/users/" + m.ID + "/identity-key"

	// Before: nothing, for the member and on the Users page.
	rec := e.do(t, mCookie, http.MethodGet, "/v1/me/identity-key", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member status: %d %s", rec.Code, rec.Body.String())
	}
	if st := decodeStatus(t, rec.Body.Bytes()); st.HasKey || st.Registrations == nil || len(st.Registrations) != 0 {
		t.Errorf("empty status = %s", rec.Body.String())
	}
	if rec := e.do(t, mCookie, http.MethodPost, "/v1/me/identity-key/reveal", ""); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "no_key" {
		t.Errorf("reveal without key: %d %s", rec.Code, rec.Body.String())
	}

	// Admin provisions: issued, registered as the admin.
	rec = e.do(t, admin, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
	}
	st := decodeStatus(t, rec.Body.Bytes())
	if !st.HasKey || st.Revealed || len(st.Fingerprint) != 64 || st.CreatedAt == 0 {
		t.Errorf("issued status = %+v", st)
	}
	if len(st.Registrations) != 1 || st.Registrations[0].Upstream != "BkCoreServices" || st.Registrations[0].Status != identitykeys.StatusRegistered {
		t.Errorf("registrations = %+v", st.Registrations)
	}
	targets, callers := reg.take()
	if len(targets) != 1 || targets[0] != "BkCoreServices.upsert-bifrost-virtual-key-actor" || callers[0] != "dashboard:"+e.admin.ID {
		t.Errorf("registry calls = %v as %v", targets, callers)
	}
	if e.auditRows(t, identitykeys.EventIssue, "") != 1 {
		t.Error("issue not audited")
	}
	fp1 := st.Fingerprint

	// The Users page carries the status per row.
	users := e.listUsers(t, admin)
	for _, row := range users.Users {
		ik, _ := row["identity_key"].(map[string]any)
		switch row["id"] {
		case m.ID:
			if ik == nil || ik["has_key"] != true || ik["fingerprint"] != fp1 {
				t.Errorf("member identity_key = %v", row["identity_key"])
			}
			if regs, _ := ik["registrations"].([]any); len(regs) != 1 {
				t.Errorf("member registrations = %v", ik["registrations"])
			}
		case e.admin.ID:
			if ik == nil || ik["has_key"] != false {
				t.Errorf("admin identity_key = %v", row["identity_key"])
			}
		}
	}

	// The member sees it and reveals it once.
	rec = e.do(t, mCookie, http.MethodGet, "/v1/me/identity-key", "")
	if st := decodeStatus(t, rec.Body.Bytes()); !st.HasKey || st.Revealed || st.Fingerprint != fp1 {
		t.Errorf("member status after issue = %s", rec.Body.String())
	}
	rec = e.do(t, mCookie, http.MethodPost, "/v1/me/identity-key/reveal", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	key, _ := got["key"].(string)
	if key == "" || got["fingerprint"] != fp1 || identity.HashToken(key) != fp1 {
		t.Errorf("reveal body = %v", got)
	}
	if rec := e.do(t, mCookie, http.MethodPost, "/v1/me/identity-key/reveal", ""); rec.Code != http.StatusConflict || decodeJSON(t, rec)["error"] != "already_revealed" {
		t.Errorf("second reveal: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/me/identity-key", ""); !decodeStatus(t, rec.Body.Bytes()).Revealed {
		t.Error("status.revealed false after reveal")
	}
	// The key is the identity agent's token: it authenticates like one.
	if ag, err := e.id.VerifyAgentToken(t.Context(), key); err != nil || ag.Owner != m.ID || ag.Kind != identity.AgentKindIdentity {
		t.Errorf("revealed key does not authenticate: %+v %v", ag, err)
	}

	// Provision again rotates: new fingerprint, unrevealed, old key dead.
	rec = e.do(t, admin, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	st = decodeStatus(t, rec.Body.Bytes())
	if !st.HasKey || st.Revealed || st.Fingerprint == fp1 {
		t.Errorf("rotated status = %+v", st)
	}
	targets, _ = reg.take()
	if len(targets) != 2 || targets[0] != "BkCoreServices.upsert-bifrost-virtual-key-actor" || targets[1] != "BkCoreServices.delete-bifrost-virtual-key-actor" {
		t.Errorf("rotate registry calls = %v", targets)
	}
	if _, err := e.id.VerifyAgentToken(t.Context(), key); err == nil {
		t.Error("old key still authenticates after rotate")
	}
	if e.auditRows(t, identitykeys.EventRotate, "") != 1 {
		t.Error("rotate not audited")
	}

	// Retry registration: idempotent upsert, still as the admin.
	reg.fail["BkCoreServices.upsert-bifrost-virtual-key-actor"] = true
	rec = e.do(t, admin, http.MethodPost, path+"/register", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register (failing): %d %s", rec.Code, rec.Body.String())
	}
	if st := decodeStatus(t, rec.Body.Bytes()); !st.HasKey || st.Registrations[0].Status != identitykeys.StatusError || st.Registrations[0].Error != "registry says no" {
		t.Errorf("status after failed register = %+v", st)
	}
	delete(reg.fail, "BkCoreServices.upsert-bifrost-virtual-key-actor")
	rec = e.do(t, admin, http.MethodPost, path+"/register", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if st := decodeStatus(t, rec.Body.Bytes()); st.Registrations[0].Status != identitykeys.StatusRegistered || st.Registrations[0].Error != "" {
		t.Errorf("status after retry = %+v", st)
	}
	if _, callers := reg.take(); len(callers) != 2 || callers[1] != "dashboard:"+e.admin.ID {
		t.Errorf("register callers = %v", callers)
	}

	// Revoke.
	rec = e.do(t, admin, http.MethodDelete, path, "")
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["ok"] != true {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if targets, _ := reg.take(); len(targets) != 1 || targets[0] != "BkCoreServices.delete-bifrost-virtual-key-actor" {
		t.Errorf("revoke registry calls = %v", targets)
	}
	rec = e.do(t, mCookie, http.MethodGet, "/v1/me/identity-key", "")
	if st := decodeStatus(t, rec.Body.Bytes()); st.HasKey || len(st.Registrations) != 1 || st.Registrations[0].Status != identitykeys.StatusRevoked {
		t.Errorf("status after revoke = %s", rec.Body.String())
	}
	if rec := e.do(t, admin, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "no_key" {
		t.Errorf("second revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, admin, http.MethodPost, path+"/register", ""); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "no_key" {
		t.Errorf("register after revoke: %d %s", rec.Code, rec.Body.String())
	}
	if e.auditRows(t, identitykeys.EventRevoke, "") != 1 {
		t.Error("revoke not audited")
	}
}

func TestIdentityKeyRoutesRefusals(t *testing.T) {
	e := newAccessTestServer(t)
	withIdentityKeys(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "mira@beknown.work")
	mCookie := e.cookieFor(t, m.ID)

	// The password-only admin has no Clerk identity: no key for them
	// until they sign in with Google once.
	rec := e.do(t, admin, http.MethodPost, "/v1/users/"+e.admin.ID+"/identity-key", "")
	if rec.Code != http.StatusConflict || decodeJSON(t, rec)["error"] != "no_clerk_identity" {
		t.Errorf("provision without clerk identity: %d %s", rec.Code, rec.Body.String())
	}
	// Unknown user.
	if rec := e.do(t, admin, http.MethodPost, "/v1/users/u_missing/identity-key", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: %d", rec.Code)
	}
	// Wrong methods.
	if rec := e.do(t, admin, http.MethodGet, "/v1/users/"+m.ID+"/identity-key", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET identity-key: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodDelete, "/v1/users/"+m.ID+"/identity-key/register", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE register: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodPost, "/v1/me/identity-key", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST me/identity-key: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodGet, "/v1/me/identity-key/reveal", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reveal: %d", rec.Code)
	}
	// Members can't provision, revoke or register anyone (the guard).
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/users/" + m.ID + "/identity-key"},
		{http.MethodDelete, "/v1/users/" + m.ID + "/identity-key"},
		{http.MethodPost, "/v1/users/" + m.ID + "/identity-key/register"},
	} {
		if rec := e.do(t, mCookie, c.method, c.path, ""); rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "admin_only" {
			t.Errorf("member %s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// Anonymous callers.
	if rec := e.do(t, nil, http.MethodGet, "/v1/me/identity-key", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon me: %d", rec.Code)
	}
	if rec := e.do(t, nil, http.MethodPost, "/v1/users/"+m.ID+"/identity-key", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon provision: %d", rec.Code)
	}
}

func TestIdentityKeyRoutesUnwired(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "mira@beknown.work")
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodGet, "/v1/me/identity-key", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("me status unwired: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodPost, "/v1/users/"+m.ID+"/identity-key", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("provision unwired: %d", rec.Code)
	}
	// The Users page simply omits the field.
	for _, row := range e.listUsers(t, admin).Users {
		if _, ok := row["identity_key"]; ok {
			t.Errorf("identity_key present while unwired: %v", row)
		}
	}
}

func TestAgentsListShowsIdentityAgentAndRefusesActions(t *testing.T) {
	e := newAccessTestServer(t)
	withIdentityKeys(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "mira@beknown.work")
	mCookie := e.cookieFor(t, m.ID)
	if rec := e.do(t, admin, http.MethodPost, "/v1/users/"+m.ID+"/identity-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
	}
	_, botID := e.agentFor(t, m.ID)
	if err := e.id.SetAgentDisabled(t.Context(), m.ID, botID, true); err != nil {
		t.Fatal(err)
	}

	rec := e.do(t, mCookie, http.MethodGet, "/v1/agents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list agents: %d %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("agents = %v", rows)
	}
	var identityID string
	for _, row := range rows {
		switch row["kind"] {
		case identity.AgentKindIdentity:
			identityID, _ = row["id"].(string)
			if row["name"] != identitykeys.AgentName || row["disabled"] != false {
				t.Errorf("identity agent row = %v", row)
			}
		case identity.AgentKindAgent:
			if row["id"] != botID || row["disabled"] != true {
				t.Errorf("plain agent row = %v", row)
			}
		default:
			t.Errorf("row without kind: %v", row)
		}
	}
	if identityID == "" {
		t.Fatal("identity agent not listed")
	}
	// The generic actions refuse the identity agent...
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/agents/" + identityID + "/rotate"},
		{http.MethodPost, "/v1/agents/" + identityID + "/disable"},
		{http.MethodPost, "/v1/agents/" + identityID + "/enable"},
		{http.MethodDelete, "/v1/agents/" + identityID},
	} {
		rec := e.do(t, mCookie, c.method, c.path, "")
		if rec.Code != http.StatusConflict || decodeJSON(t, rec)["error"] != "identity_agent" {
			t.Errorf("%s %s: %d %s, want 409 identity_agent", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// ...and still work on a plain agent; unknown ids stay 404.
	if rec := e.do(t, mCookie, http.MethodPost, "/v1/agents/"+botID+"/enable", ""); rec.Code != http.StatusOK {
		t.Errorf("enable plain agent: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, mCookie, http.MethodDelete, "/v1/agents/ag_missing", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown agent: %d", rec.Code)
	}
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/me/identity-key", ""); !decodeStatus(t, rec.Body.Bytes()).HasKey {
		t.Error("identity key lost after refused agent actions")
	}
}

// A revoke whose registry delete failed leaves a pending removal; the
// admin's Retry runs it even though the user has no key any more.
func TestIdentityKeyRegisterRunsPendingRemovalsWithoutKey(t *testing.T) {
	e := newAccessTestServer(t)
	reg := withIdentityKeys(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "mira@beknown.work")
	path := "/v1/users/" + m.ID + "/identity-key"

	rec := e.do(t, admin, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
	}
	fp := decodeStatus(t, rec.Body.Bytes()).Fingerprint
	if st := decodeStatus(t, rec.Body.Bytes()); st.PendingRemovals == nil || len(st.PendingRemovals) != 0 {
		t.Errorf("pending_removals after issue = %s", rec.Body.String())
	}

	reg.fail["BkCoreServices."+identitykeys.ToolDelete] = true
	if rec := e.do(t, admin, http.MethodDelete, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, e.cookieFor(t, m.ID), http.MethodGet, "/v1/me/identity-key", "")
	st := decodeStatus(t, rec.Body.Bytes())
	if st.HasKey || len(st.PendingRemovals) != 1 || st.PendingRemovals[0].Upstream != "BkCoreServices" ||
		st.PendingRemovals[0].Fingerprint != fp || st.PendingRemovals[0].Error != "registry says no" || st.PendingRemovals[0].UpdatedAt == 0 {
		t.Fatalf("status after failed revoke = %s", rec.Body.String())
	}
	if !reg.has(fp) {
		t.Fatal("precondition: registry no longer holds the fingerprint")
	}
	for _, row := range e.listUsers(t, admin).Users {
		if row["id"] != m.ID {
			continue
		}
		ik, _ := row["identity_key"].(map[string]any)
		if p, _ := ik["pending_removals"].([]any); len(p) != 1 {
			t.Errorf("Users page pending_removals = %v", ik["pending_removals"])
		}
	}

	// Retry with no key: 200, and the fingerprint leaves the registry.
	delete(reg.fail, "BkCoreServices."+identitykeys.ToolDelete)
	rec = e.do(t, admin, http.MethodPost, path+"/register", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register without key: %d %s", rec.Code, rec.Body.String())
	}
	if st := decodeStatus(t, rec.Body.Bytes()); st.HasKey || len(st.PendingRemovals) != 0 {
		t.Errorf("status after cleanup = %s", rec.Body.String())
	}
	if reg.has(fp) {
		t.Error("registry still holds the revoked fingerprint")
	}
	// Nothing pending and no key: back to no_key.
	if rec := e.do(t, admin, http.MethodPost, path+"/register", ""); rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "no_key" {
		t.Errorf("register with nothing to do: %d %s", rec.Code, rec.Body.String())
	}
}

// The Clerk org sync blocking a leaver must stop their key being forwarded
// at once, not when the resolver's cache expires.
func TestClerkSyncBlockInvalidatesIdentityKeyCache(t *testing.T) {
	e := newAccessTestServer(t)
	withIdentityKeys(t, e)
	ctx := context.Background()
	left := e.member(t, "user_left", "left@beknown.work")
	if _, err := e.srv.identityKeys.Issue(ctx, e.admin.ID, left.ID); err != nil {
		t.Fatal(err)
	}
	// Prime the cache the way a tool call would.
	if _, err := e.srv.identityKeys.ForwardKey(ctx, "dashboard:"+left.ID); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_stay": {IsMember: true}}
	})
	if n := e.srv.runClerkSync(ctx); n != 1 {
		t.Fatalf("blocked = %d, want 1", n)
	}
	if _, err := e.srv.identityKeys.ForwardKey(ctx, "dashboard:"+left.ID); !errors.Is(err, gateway.ErrNoIdentityKey) {
		t.Errorf("leaver's key still forwarded right after the sync: %v", err)
	}
}

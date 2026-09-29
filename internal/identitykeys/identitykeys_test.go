package identitykeys

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// fakeCall is one registry call the fake gateway saw: the exact tool name,
// the arguments, the caller id on the ctx and the via tag.
type fakeCall struct {
	Target string
	Args   map[string]any
	Caller string
	Via    string
}

// fakeCaller records CallInternal calls and answers per target: an error
// string makes the tool return isError with that text; "" succeeds.
type fakeCaller struct {
	mu    sync.Mutex
	calls []fakeCall
	fail  map[string]string // target -> error text
	err   error             // transport error for every call
}

func (f *fakeCaller) CallInternal(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Target: target, Args: args, Caller: gateway.AgentIDFromContext(ctx), Via: via})
	if f.err != nil {
		return nil, f.err
	}
	if msg, ok := f.fail[target]; ok {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(`{"ok":true}`), nil
}

func (f *fakeCaller) setFail(target, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]string{}
	}
	if msg == "" {
		delete(f.fail, target)
		return
	}
	f.fail[target] = msg
}

// take returns the calls so far and clears them.
func (f *fakeCaller) take() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

type env struct {
	db     *store.DB
	id     *identity.Service
	audit  *audit.Logger
	svc    *Service
	caller *fakeCaller
	admin  *identity.User // password owner, linked to Clerk
	member *identity.User // Clerk member
	plain  *identity.User // password-only: no Clerk identity
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ik.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id := identity.New(db)
	if _, err := id.CreateUser(ctx, "owner", "long-enough-password"); err != nil {
		t.Fatal(err)
	}
	// The owner signs in with Google once, which links the Clerk id.
	admin, err := id.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user_admin", Email: "owner@beknown.work", DisplayName: "Owner"}, "owner@beknown.work")
	if err != nil {
		t.Fatal(err)
	}
	member, err := id.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user_m", Email: "mira@beknown.work", DisplayName: "Mira"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// A password-only user with no Clerk identity, inserted directly.
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at)
		VALUES('u_plain','plain','x','member','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	plain, err := id.GetUserByID(ctx, "u_plain")
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, sealbox.MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := sealbox.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	auditLog := audit.New(db)
	caller := &fakeCaller{}
	svc := New(db, cipher)
	svc.SetAudit(auditLog)
	svc.SetCaller(caller)
	svc.SetRegistries(RegistryListerFunc(func(context.Context) ([]string, error) {
		return []string{"BkCoreServicesProd", "BkCoreServices"}, nil
	}))
	return &env{db: db, id: id, audit: auditLog, svc: svc, caller: caller, admin: admin, member: member, plain: plain}
}

func (e *env) auditRows(t *testing.T, eventType string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = ?`, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditMentions reports whether any audit row carries s in any text column.
func (e *env) auditMentions(t *testing.T, s string) bool {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM audit_events WHERE instr(COALESCE(agent_id,''),?) > 0 OR instr(COALESCE(reason,''),?) > 0
		OR instr(COALESCE(result_summary,''),?) > 0 OR instr(COALESCE(arguments,''),?) > 0`
	if err := e.db.QueryRow(q, s, s, s, s).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func regByUpstream(st *Status) map[string]Registration {
	out := map[string]Registration{}
	for _, r := range st.Registrations {
		out[r.Upstream] = r
	}
	return out
}

func TestIssueRevealRotateRevoke(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID

	// Nothing yet.
	st, err := e.svc.Status(ctx, uid)
	if err != nil || st.HasKey || len(st.Registrations) != 0 {
		t.Fatalf("empty status = %+v, %v", st, err)
	}
	if _, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); !errors.Is(err, gateway.ErrNoIdentityKey) {
		t.Errorf("ForwardKey before issue: %v", err)
	}

	// Issue.
	st, err = e.svc.Provision(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !st.HasKey || st.Revealed || !hexRE.MatchString(st.Fingerprint) || st.CreatedAt == 0 {
		t.Errorf("issued status = %+v", st)
	}
	regs := regByUpstream(st)
	if len(regs) != 2 || regs["BkCoreServices"].Status != StatusRegistered || regs["BkCoreServicesProd"].Status != StatusRegistered {
		t.Errorf("registrations = %+v", st.Registrations)
	}
	if regs["BkCoreServices"].Error != "" || regs["BkCoreServices"].UpdatedAt == 0 {
		t.Errorf("registration row = %+v", regs["BkCoreServices"])
	}
	// Sorted by upstream.
	if st.Registrations[0].Upstream != "BkCoreServices" {
		t.Errorf("registrations not sorted: %+v", st.Registrations)
	}
	fp1 := st.Fingerprint
	calls := e.caller.take()
	if len(calls) != 2 || calls[0].Target != "BkCoreServices."+ToolUpsert || calls[1].Target != "BkCoreServicesProd."+ToolUpsert {
		t.Fatalf("issue calls = %+v", calls)
	}
	if e.auditRows(t, EventIssue) != 1 || e.auditRows(t, EventRegister) != 2 {
		t.Errorf("audit: issue=%d register=%d", e.auditRows(t, EventIssue), e.auditRows(t, EventRegister))
	}
	// Issue again is refused; Provision rotates instead (tested below).
	if _, err := e.svc.Issue(ctx, e.admin.ID, uid); !errors.Is(err, ErrKeyExists) {
		t.Errorf("second issue: %v", err)
	}

	// The identity agent exists under the user, named, kind identity, and
	// is the only agent.
	agents, err := e.id.ListAgents(ctx, uid)
	if err != nil || len(agents) != 1 || agents[0].Kind != identity.AgentKindIdentity || agents[0].Name != AgentName {
		t.Fatalf("agents after issue = %+v, %v", agents, err)
	}
	identityAgent := agents[0].ID

	// Reveal once.
	key, fp, err := e.svc.Reveal(ctx, uid)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if fp != fp1 || identity.HashToken(key) != fp1 || !strings.HasPrefix(key, identityAgent+".") {
		t.Errorf("revealed key does not match fingerprint/agent")
	}
	if _, _, err := e.svc.Reveal(ctx, uid); !errors.Is(err, ErrAlreadyRevealed) {
		t.Errorf("second reveal: %v", err)
	}
	if st, _ := e.svc.Status(ctx, uid); !st.Revealed {
		t.Error("status.revealed false after reveal")
	}
	if e.auditRows(t, EventReveal) != 1 {
		t.Error("reveal not audited")
	}
	// The key authenticates at /mcp like an agent token.
	ag, err := e.id.VerifyAgentToken(ctx, key)
	if err != nil || ag.ID != identityAgent || ag.Owner != uid || ag.Kind != identity.AgentKindIdentity {
		t.Fatalf("verify key = %+v, %v", ag, err)
	}
	// And is what gets forwarded.
	if got, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || got != key {
		t.Errorf("ForwardKey after reveal: match=%v err=%v", got == key, err)
	}
	// The key never lands in the audit log.
	if e.auditMentions(t, key) {
		t.Error("raw key found in audit rows")
	}

	// Rotate: new fingerprint registered first, then the old one deleted,
	// then the token swapped; the old key is dead at once.
	st, err = e.svc.Provision(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !st.HasKey || st.Revealed || st.Fingerprint == fp1 || !hexRE.MatchString(st.Fingerprint) {
		t.Errorf("rotated status = %+v", st)
	}
	fp2 := st.Fingerprint
	calls = e.caller.take()
	if len(calls) != 4 {
		t.Fatalf("rotate calls = %+v", calls)
	}
	for i, up := range []string{"BkCoreServices", "BkCoreServicesProd"} {
		u, d := calls[2*i], calls[2*i+1]
		if u.Target != up+"."+ToolUpsert || u.Args["virtualKeyHash"] != fp2 {
			t.Errorf("rotate upsert %d = %+v", i, u)
		}
		if d.Target != up+"."+ToolDelete || d.Args["virtualKeyHash"] != fp1 || d.Args["confirm"] != true {
			t.Errorf("rotate delete %d = %+v", i, d)
		}
	}
	if _, err := e.id.VerifyAgentToken(ctx, key); !errors.Is(err, identity.ErrAgentTokenInvalid) {
		t.Errorf("old key still authenticates after rotate: %v", err)
	}
	if got, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || got == key {
		t.Errorf("ForwardKey after rotate still the old key (err=%v)", err)
	}
	key2, fp, err := e.svc.Reveal(ctx, uid)
	if err != nil || fp != fp2 || identity.HashToken(key2) != fp2 {
		t.Fatalf("reveal after rotate: %v", err)
	}
	if ag, err := e.id.VerifyAgentToken(ctx, key2); err != nil || ag.ID != identityAgent {
		t.Errorf("new key rejected: %v", err)
	}
	if e.auditRows(t, EventRotate) != 1 {
		t.Error("rotate not audited")
	}

	// Revoke: delete everywhere, then the key and the agent go.
	st, err = e.svc.Revoke(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if st.HasKey || st.Fingerprint != "" {
		t.Errorf("revoked status = %+v", st)
	}
	regs = regByUpstream(st)
	if regs["BkCoreServices"].Status != StatusRevoked || regs["BkCoreServicesProd"].Status != StatusRevoked {
		t.Errorf("registrations after revoke = %+v", st.Registrations)
	}
	calls = e.caller.take()
	if len(calls) != 2 || calls[0].Target != "BkCoreServices."+ToolDelete || calls[0].Args["virtualKeyHash"] != fp2 ||
		calls[1].Target != "BkCoreServicesProd."+ToolDelete {
		t.Errorf("revoke calls = %+v", calls)
	}
	if _, err := e.id.VerifyAgentToken(ctx, key2); !errors.Is(err, identity.ErrAgentTokenInvalid) {
		t.Errorf("revoked key still authenticates: %v", err)
	}
	if agents, _ := e.id.ListAgents(ctx, uid); len(agents) != 0 {
		t.Errorf("identity agent survived revoke: %+v", agents)
	}
	if _, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); !errors.Is(err, gateway.ErrNoIdentityKey) {
		t.Errorf("ForwardKey after revoke: %v", err)
	}
	if _, err := e.svc.Revoke(ctx, e.admin.ID, uid); !errors.Is(err, ErrNoKey) {
		t.Errorf("second revoke: %v", err)
	}
	if _, _, err := e.svc.Reveal(ctx, uid); !errors.Is(err, ErrNoKey) {
		t.Errorf("reveal without key: %v", err)
	}
	if e.auditRows(t, EventRevoke) != 1 {
		t.Error("revoke not audited")
	}
	if e.auditMentions(t, key) || e.auditMentions(t, key2) {
		t.Error("raw key found in audit rows")
	}
}

func TestRegistrationCallShape(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, err := e.svc.Issue(ctx, e.admin.ID, e.member.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := e.caller.take()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if c.Target != "BkCoreServices.upsert-bifrost-virtual-key-actor" {
		t.Errorf("target = %q", c.Target)
	}
	if c.Caller != "dashboard:"+e.admin.ID {
		t.Errorf("caller ctx = %q, want the provisioning admin", c.Caller)
	}
	if c.Via != ViaTool {
		t.Errorf("via = %q", c.Via)
	}
	want := map[string]any{
		"virtualKeyHash": st.Fingerprint,
		"userId":         "user_m",
		"email":          "mira@beknown.work",
		"label":          "toolyard: mira",
		"isActive":       true,
	}
	if len(c.Args) != len(want) {
		t.Errorf("args = %v, want %v", c.Args, want)
	}
	for k, v := range want {
		if c.Args[k] != v {
			t.Errorf("args[%s] = %v, want %v", k, c.Args[k], v)
		}
	}
	// The fingerprint is the sha256 of the raw key.
	key, _, err := e.svc.Reveal(ctx, e.member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if identity.HashToken(key) != st.Fingerprint {
		t.Error("fingerprint is not sha256(key)")
	}
}

func TestIssueRequiresClerkIdentity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Provision(ctx, e.admin.ID, e.plain.ID); !errors.Is(err, ErrNoClerkIdentity) {
		t.Fatalf("issue for password-only user: %v", err)
	}
	if st, _ := e.svc.Status(ctx, e.plain.ID); st.HasKey {
		t.Error("key created despite refusal")
	}
	if agents, _ := e.id.ListAgents(ctx, e.plain.ID); len(agents) != 0 {
		t.Error("identity agent created despite refusal")
	}
	if len(e.caller.take()) != 0 {
		t.Error("registry called despite refusal")
	}
	if _, err := e.svc.Provision(ctx, e.admin.ID, "u_missing"); !errors.Is(err, identity.ErrNoUser) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestRegistrationErrorThenRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	// Bootstrap-like failure: the registry refuses the caller on one
	// upstream (prod), a transport error on none.
	e.caller.setFail("BkCoreServicesProd."+ToolUpsert, "caller is not a BkAdmin")
	st, err := e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("issue with registry failure: %v", err)
	}
	if !st.HasKey {
		t.Fatal("key not kept after registration failure")
	}
	regs := regByUpstream(st)
	if regs["BkCoreServices"].Status != StatusRegistered {
		t.Errorf("dev = %+v", regs["BkCoreServices"])
	}
	if p := regs["BkCoreServicesProd"]; p.Status != StatusError || p.Error != "caller is not a BkAdmin" {
		t.Errorf("prod = %+v", p)
	}
	e.caller.take()

	// Retry as another admin: idempotent upsert of the same fingerprint on
	// every registry; the error clears.
	e.caller.setFail("BkCoreServicesProd."+ToolUpsert, "")
	st2, err := e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st2.Fingerprint != st.Fingerprint {
		t.Error("retry changed the fingerprint")
	}
	regs = regByUpstream(st2)
	if p := regs["BkCoreServicesProd"]; p.Status != StatusRegistered || p.Error != "" {
		t.Errorf("prod after retry = %+v", p)
	}
	calls := e.caller.take()
	if len(calls) != 2 {
		t.Fatalf("retry calls = %+v", calls)
	}
	for _, c := range calls {
		if !strings.HasSuffix(c.Target, "."+ToolUpsert) || c.Args["virtualKeyHash"] != st.Fingerprint {
			t.Errorf("retry call = %+v", c)
		}
	}

	// A transport error is recorded the same way, and a retry without a
	// key is ErrNoKey.
	e.caller.err = errors.New("upstream BkCoreServices: dial tcp: connection refused")
	st3, err := e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("retry with transport error: %v", err)
	}
	if p := regByUpstream(st3)["BkCoreServices"]; p.Status != StatusError || !strings.Contains(p.Error, "connection refused") {
		t.Errorf("transport error row = %+v", p)
	}
	e.caller.err = nil
	if _, err := e.svc.Register(ctx, e.admin.ID, e.admin.ID); !errors.Is(err, ErrNoKey) {
		t.Errorf("register without key: %v", err)
	}
	// Without a caller wired every registration is an error, not a panic.
	e.svc.SetCaller(nil)
	st4, err := e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if p := regByUpstream(st4)["BkCoreServices"]; p.Status != StatusError || p.Error != ErrNotWired.Error() {
		t.Errorf("unwired row = %+v", p)
	}
}

func TestRevokeRegistryFailureKeepsStaleRowAndCleansUpLater(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	st, err := e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := st.Fingerprint
	e.caller.take()

	e.caller.setFail("BkCoreServicesProd."+ToolDelete, "prod is read-only right now")
	st, err = e.svc.Revoke(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The key is gone locally whatever the registry said.
	if st.HasKey {
		t.Error("key kept after revoke")
	}
	if agents, _ := e.id.ListAgents(ctx, uid); len(agents) != 0 {
		t.Error("identity agent kept after revoke")
	}
	regs := regByUpstream(st)
	if regs["BkCoreServices"].Status != StatusRevoked {
		t.Errorf("dev = %+v", regs["BkCoreServices"])
	}
	if p := regs["BkCoreServicesProd"]; p.Status != StatusRegistered || !strings.HasPrefix(p.Error, "revoke: prod is read-only") {
		t.Errorf("prod = %+v", p)
	}
	e.caller.take()

	// The next issue registers the new fingerprint and removes the stale
	// one on prod; dev (revoked) needs no delete.
	e.caller.setFail("BkCoreServicesProd."+ToolDelete, "")
	st, err = e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	calls := e.caller.take()
	var targets []string
	for _, c := range calls {
		targets = append(targets, c.Target)
	}
	want := []string{"BkCoreServices." + ToolUpsert, "BkCoreServicesProd." + ToolUpsert, "BkCoreServicesProd." + ToolDelete}
	if strings.Join(targets, ",") != strings.Join(want, ",") {
		t.Errorf("re-issue calls = %v, want %v", targets, want)
	}
	if calls[2].Args["virtualKeyHash"] != fp1 {
		t.Errorf("stale delete hash = %v, want %s", calls[2].Args["virtualKeyHash"], fp1)
	}
	regs = regByUpstream(st)
	if p := regs["BkCoreServicesProd"]; p.Status != StatusRegistered || p.Error != "" {
		t.Errorf("prod after re-issue = %+v", p)
	}

	// A failed stale delete leaves a note but the new key stays registered.
	e.caller.take()
	e.caller.setFail("BkCoreServices."+ToolDelete, "not found")
	st, err = e.svc.Rotate(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if p := regByUpstream(st)["BkCoreServices"]; p.Status != StatusRegistered || !strings.Contains(p.Error, "previous fingerprint not removed: not found") {
		t.Errorf("dev after rotate with failed delete = %+v", p)
	}
}

func TestForwardKeyCallers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	if _, err := e.svc.Issue(ctx, e.admin.ID, uid); err != nil {
		t.Fatal(err)
	}
	key, _, err := e.svc.Reveal(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	// A plain agent of the member's, and the identity agent itself.
	_, bot, err := e.id.CreateAgentWithToken(ctx, uid, "bot")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.id.ListAgents(ctx, uid)
	var identityAgent string
	for _, a := range agents {
		if a.Kind == identity.AgentKindIdentity {
			identityAgent = a.ID
		}
	}
	for _, caller := range []string{"dashboard:" + uid, "voice:" + uid, bot.ID, identityAgent} {
		got, err := e.svc.ForwardKey(ctx, caller)
		if err != nil || got != key {
			t.Errorf("ForwardKey(%q): match=%v err=%v", caller, got == key, err)
		}
	}
	// Callers with no key: the admin (none issued), an unknown agent, an
	// empty or foreign caller id, another user's agent.
	_, adminBot, _ := e.id.CreateAgentWithToken(ctx, e.admin.ID, "adminbot")
	for _, caller := range []string{"dashboard:" + e.admin.ID, "voice:" + e.admin.ID, adminBot.ID, "ag_missing", "", "svc:x", "dashboard:"} {
		if _, err := e.svc.ForwardKey(ctx, caller); !errors.Is(err, gateway.ErrNoIdentityKey) {
			t.Errorf("ForwardKey(%q) = %v, want ErrNoIdentityKey", caller, err)
		}
	}

	// Blocked owner: refused, through both the dashboard and the agent
	// paths, as soon as the cache is dropped; re-activating restores it.
	if err := e.id.SetStatus(ctx, uid, identity.StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	e.svc.Invalidate(uid)
	for _, caller := range []string{"dashboard:" + uid, bot.ID} {
		if _, err := e.svc.ForwardKey(ctx, caller); !errors.Is(err, gateway.ErrNoIdentityKey) {
			t.Errorf("blocked ForwardKey(%q) = %v", caller, err)
		}
	}
	if err := e.id.SetStatus(ctx, uid, identity.StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	e.svc.Invalidate(uid)
	if got, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || got != key {
		t.Errorf("ForwardKey after unblock: err=%v", err)
	}

	// The cache serves for cacheTTL without Invalidate, then re-reads.
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	if _, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM user_identity_keys WHERE user_id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || got != key {
		t.Errorf("cached ForwardKey after direct delete: err=%v", err)
	}
	e.svc.now = func() time.Time { return now.Add(cacheTTL + time.Second) }
	if _, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); !errors.Is(err, gateway.ErrNoIdentityKey) {
		t.Errorf("ForwardKey after cache expiry = %v", err)
	}
}

func TestSealedKeyIsBoundToUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Issue(ctx, e.admin.ID, e.member.ID); err != nil {
		t.Fatal(err)
	}
	// The stored form is not the token, and opening it under another
	// user's id fails (AAD = user id).
	var sealed string
	if err := e.db.QueryRow(`SELECT key_sealed FROM user_identity_keys WHERE user_id = ?`, e.member.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(sealed, "ag_") {
		t.Error("key stored in the clear")
	}
	if _, err := e.svc.cipher.Open(sealed, []byte(e.admin.ID)); err == nil {
		t.Error("sealed key opened under another user's id")
	}
	raw, err := e.svc.cipher.Open(sealed, []byte(e.member.ID))
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := e.db.QueryRow(`SELECT token_hash FROM agents WHERE owner_user = ? AND kind = ?`, e.member.ID, identity.AgentKindIdentity).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if identity.HashToken(string(raw)) != hash {
		t.Error("agents.token_hash is not sha256(key)")
	}
}

func TestNoRegistriesStillIssues(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.SetRegistries(nil)
	st, err := e.svc.Issue(ctx, e.admin.ID, e.member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasKey || len(st.Registrations) != 0 {
		t.Errorf("status without registries = %+v", st)
	}
	if len(e.caller.take()) != 0 {
		t.Error("registry called with no registries")
	}
	e.svc.SetRegistries(RegistryListerFunc(func(context.Context) ([]string, error) { return nil, errors.New("db down") }))
	if _, err := e.svc.Register(ctx, e.admin.ID, e.member.ID); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("lister error not surfaced: %v", err)
	}
}

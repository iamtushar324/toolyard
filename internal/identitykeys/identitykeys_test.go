package identitykeys

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
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

// fakeCaller stands in for the gateway in front of prime's registry: it
// records every CallInternal, keeps the registered fingerprints per
// upstream (upsert adds, delete removes and answers "not found" for an
// unknown hash exactly as prime does, list returns them), and answers a
// target marked failing with isError and that text.
type fakeCaller struct {
	mu       sync.Mutex
	calls    []fakeCall
	fail     map[string]string // target -> error text
	err      error             // transport error for every call
	registry map[string]map[string]bool
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
	up, tool, _ := strings.Cut(target, ".")
	if f.registry == nil {
		f.registry = map[string]map[string]bool{}
	}
	if f.registry[up] == nil {
		f.registry[up] = map[string]bool{}
	}
	hash, _ := args["virtualKeyHash"].(string)
	switch tool {
	case ToolUpsert:
		f.registry[up][hash] = true
		return mcp.NewToolResultText(`{"ok":true}`), nil
	case ToolDelete:
		if !f.registry[up][hash] {
			return mcp.NewToolResultError("Virtual Key mapping not found"), nil
		}
		delete(f.registry[up], hash)
		return mcp.NewToolResultText(`{"deleted":true}`), nil
	case ToolList:
		var out []map[string]string
		for _, h := range f.hashesLocked(up) {
			out = append(out, map[string]string{"virtualKeyHash": h})
		}
		b, _ := json.Marshal(out)
		return mcp.NewToolResultText(string(b)), nil
	}
	return mcp.NewToolResultError("unknown tool " + target), nil
}

func (f *fakeCaller) hashesLocked(up string) []string {
	var out []string
	for h := range f.registry[up] {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// hashes is what the fake registry holds for up, sorted.
func (f *fakeCaller) hashes(up string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hashesLocked(up)
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

// down marks every registry tool of up failing with msg ("" restores it).
func (f *fakeCaller) down(up, msg string) {
	for _, tool := range []string{ToolUpsert, ToolDelete, ToolList} {
		f.setFail(up+"."+tool, msg)
	}
}

// take returns the calls so far and clears them.
func (f *fakeCaller) take() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func targetsOf(calls []fakeCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Target)
	}
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

// auditDecisions counts identity_key.register rows with one decision.
func (e *env) auditDecisions(t *testing.T, decision string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = ? AND decision = ?`, EventRegister, decision).Scan(&n); err != nil {
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

// pendingKeys renders pending removals as "upstream:fingerprint" sorted.
func pendingKeys(st *Status) []string {
	out := []string{}
	for _, p := range st.PendingRemovals {
		out = append(out, p.Upstream+":"+p.Fingerprint)
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// sorted returns a sorted copy (fingerprints compare lexicographically).
func sorted(v ...string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

func TestIssueRevealRotateRevoke(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID

	// Nothing yet.
	st, err := e.svc.Status(ctx, uid)
	if err != nil || st.HasKey || len(st.Registrations) != 0 || len(st.PendingRemovals) != 0 {
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
	if got := targetsOf(e.caller.take()); !eq(got, []string{"BkCoreServices." + ToolUpsert, "BkCoreServicesProd." + ToolUpsert}) {
		t.Fatalf("issue calls = %v", got)
	}
	if !eq(e.caller.hashes("BkCoreServices"), []string{fp1}) || !eq(e.caller.hashes("BkCoreServicesProd"), []string{fp1}) {
		t.Errorf("registry after issue: dev=%v prod=%v", e.caller.hashes("BkCoreServices"), e.caller.hashes("BkCoreServicesProd"))
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

	// Rotate: the new fingerprint is registered everywhere, then the old
	// one is deleted everywhere, then the token swapped; the old key is
	// dead at once and nothing is pending.
	st, err = e.svc.Provision(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !st.HasKey || st.Revealed || st.Fingerprint == fp1 || !hexRE.MatchString(st.Fingerprint) || len(st.PendingRemovals) != 0 {
		t.Errorf("rotated status = %+v", st)
	}
	fp2 := st.Fingerprint
	calls := e.caller.take()
	if got := targetsOf(calls); !eq(got, []string{
		"BkCoreServices." + ToolUpsert, "BkCoreServicesProd." + ToolUpsert,
		"BkCoreServices." + ToolDelete, "BkCoreServicesProd." + ToolDelete,
	}) {
		t.Fatalf("rotate calls = %v", got)
	}
	if calls[0].Args["virtualKeyHash"] != fp2 || calls[2].Args["virtualKeyHash"] != fp1 || calls[2].Args["confirm"] != true {
		t.Errorf("rotate args = %+v", calls)
	}
	if !eq(e.caller.hashes("BkCoreServices"), []string{fp2}) || !eq(e.caller.hashes("BkCoreServicesProd"), []string{fp2}) {
		t.Errorf("registry after rotate: dev=%v prod=%v", e.caller.hashes("BkCoreServices"), e.caller.hashes("BkCoreServicesProd"))
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
	if st.HasKey || st.Fingerprint != "" || len(st.PendingRemovals) != 0 {
		t.Errorf("revoked status = %+v", st)
	}
	regs = regByUpstream(st)
	if regs["BkCoreServices"].Status != StatusRevoked || regs["BkCoreServicesProd"].Status != StatusRevoked {
		t.Errorf("registrations after revoke = %+v", st.Registrations)
	}
	calls = e.caller.take()
	if got := targetsOf(calls); !eq(got, []string{"BkCoreServices." + ToolDelete, "BkCoreServicesProd." + ToolDelete}) || calls[0].Args["virtualKeyHash"] != fp2 {
		t.Errorf("revoke calls = %+v", calls)
	}
	if len(e.caller.hashes("BkCoreServices")) != 0 || len(e.caller.hashes("BkCoreServicesProd")) != 0 {
		t.Errorf("registry not empty after revoke")
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
	if _, err := e.svc.Register(ctx, e.admin.ID, uid); !errors.Is(err, ErrNoKey) {
		t.Errorf("register with nothing to do: %v", err)
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
	if len(st.PendingRemovals) != 0 {
		t.Errorf("pending removals after a first issue = %+v", st.PendingRemovals)
	}
	e.caller.take()

	// Retry: idempotent upsert of the same fingerprint on every registry;
	// the error clears.
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

	// A transport error is recorded the same way, and a retry with
	// nothing to do is ErrNoKey.
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

// The reviewer's case: a rotate while one registry is down must not
// orphan the old fingerprint there, through the retry, a revoke while it
// is still down, and the eventual cleanup without a key.
func TestRotateWithFailingUpsertNeverLosesOldFingerprint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	const dev, prod = "BkCoreServices", "BkCoreServicesProd"

	st, err := e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := st.Fingerprint
	key1, _, _ := e.svc.Reveal(ctx, uid)
	e.caller.take()

	// Prod goes away entirely.
	e.caller.down(prod, "prod unreachable")
	st, err = e.svc.Rotate(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	fp2 := st.Fingerprint
	if fp2 == fp1 {
		t.Fatal("rotate did not change the fingerprint")
	}
	// Dev moved on; prod still holds fp1, which is now a pending removal
	// with the failure recorded; the prod row says the new key isn't
	// registered there.
	if !eq(e.caller.hashes(dev), []string{fp2}) || !eq(e.caller.hashes(prod), []string{fp1}) {
		t.Errorf("registry: dev=%v prod=%v", e.caller.hashes(dev), e.caller.hashes(prod))
	}
	regs := regByUpstream(st)
	if regs[dev].Status != StatusRegistered || regs[prod].Status != StatusError || regs[prod].Error != "prod unreachable" {
		t.Errorf("rows after rotate = %+v", st.Registrations)
	}
	if !eq(pendingKeys(st), []string{prod + ":" + fp1}) {
		t.Fatalf("pending after rotate = %+v", st.PendingRemovals)
	}
	if p := st.PendingRemovals[0]; p.Error != "prod unreachable" || p.UpdatedAt == 0 {
		t.Errorf("pending entry = %+v", p)
	}
	// The token swap is immediate regardless.
	if _, err := e.id.VerifyAgentToken(ctx, key1); !errors.Is(err, identity.ErrAgentTokenInvalid) {
		t.Error("old key still authenticates after a rotate with a registry failure")
	}
	if got, err := e.svc.ForwardKey(ctx, "dashboard:"+uid); err != nil || identity.HashToken(got) != fp2 {
		t.Errorf("ForwardKey after rotate: hash match=%v err=%v", identity.HashToken(got) == fp2, err)
	}

	// Retry while prod is still down: the entry stays, nothing is lost.
	st, err = e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(pendingKeys(st), []string{prod + ":" + fp1}) || regByUpstream(st)[prod].Status != StatusError {
		t.Errorf("after retry while down: pending=%v rows=%+v", pendingKeys(st), st.Registrations)
	}

	// Revoke while prod is still down: the key dies locally; both prod
	// fingerprints are pending; dev is clean.
	st, err = e.svc.Revoke(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if st.HasKey {
		t.Error("key kept after revoke")
	}
	if !eq(pendingKeys(st), sorted(prod+":"+fp1, prod+":"+fp2)) {
		t.Errorf("pending after revoke = %v", pendingKeys(st))
	}
	if len(e.caller.hashes(dev)) != 0 {
		t.Errorf("dev after revoke = %v", e.caller.hashes(dev))
	}
	for _, r := range st.Registrations {
		if r.Status != StatusRevoked {
			t.Errorf("row after revoke = %+v", r)
		}
	}
	e.caller.take()

	// Prod is back: Register with no key runs the pending removals as the
	// retrying admin; fp1 and fp2 are gone from prod and nothing remains.
	e.caller.down(prod, "")
	st, err = e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("register without key: %v", err)
	}
	if st.HasKey || len(st.PendingRemovals) != 0 {
		t.Errorf("after cleanup = %+v", st)
	}
	if len(e.caller.hashes(prod)) != 0 {
		t.Errorf("prod still holds %v", e.caller.hashes(prod))
	}
	// fp1 was there and is deleted; fp2 never reached prod (its upsert
	// failed), so its delete is "not found" and the list confirms it is
	// absent. Both run as the retrying admin.
	calls := e.caller.take()
	deletes, lists := 0, 0
	for _, c := range calls {
		switch c.Target {
		case prod + "." + ToolDelete:
			deletes++
		case prod + "." + ToolList:
			lists++
		default:
			t.Errorf("unexpected cleanup call %s", c.Target)
		}
		if c.Caller != "dashboard:"+e.admin.ID {
			t.Errorf("cleanup ran %s as %q", c.Target, c.Caller)
		}
	}
	if deletes != 2 || lists != 1 {
		t.Errorf("cleanup calls = %v", targetsOf(calls))
	}
	// Nothing left to do now.
	if _, err := e.svc.Register(ctx, e.admin.ID, uid); !errors.Is(err, ErrNoKey) {
		t.Errorf("register after cleanup: %v", err)
	}
	if e.auditDecisions(t, RemovalRemoved) != 3 || e.auditDecisions(t, RemovalAbsent) != 1 || e.auditDecisions(t, RemovalError) < 2 {
		t.Errorf("removal audit: removed=%d absent=%d errors=%d",
			e.auditDecisions(t, RemovalRemoved), e.auditDecisions(t, RemovalAbsent), e.auditDecisions(t, RemovalError))
	}
}

func TestFailedOldHashDeleteIsRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	const dev, prod = "BkCoreServices", "BkCoreServicesProd"

	st, err := e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := st.Fingerprint
	e.caller.take()

	// Only the delete fails on dev: the new key is registered, the old
	// fingerprint stays pending (the list still shows it).
	e.caller.setFail(dev+"."+ToolDelete, "write lock timeout")
	st, err = e.svc.Rotate(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := st.Fingerprint
	if r := regByUpstream(st)[dev]; r.Status != StatusRegistered || r.Error != "" {
		t.Errorf("dev row = %+v", r)
	}
	if !eq(pendingKeys(st), []string{dev + ":" + fp1}) || st.PendingRemovals[0].Error != "write lock timeout" {
		t.Errorf("pending = %+v", st.PendingRemovals)
	}
	if got := targetsOf(e.caller.take()); !eq(got, []string{
		dev + "." + ToolUpsert, prod + "." + ToolUpsert,
		dev + "." + ToolDelete, dev + "." + ToolList, prod + "." + ToolDelete,
	}) {
		t.Errorf("rotate calls = %v", got)
	}
	if !eq(e.caller.hashes(dev), sorted(fp1, fp2)) {
		t.Errorf("dev registry = %v", e.caller.hashes(dev))
	}

	// The retry deletes it.
	e.caller.setFail(dev+"."+ToolDelete, "")
	st, err = e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingRemovals) != 0 || !eq(e.caller.hashes(dev), []string{fp2}) {
		t.Errorf("after retry: pending=%+v dev=%v", st.PendingRemovals, e.caller.hashes(dev))
	}
	e.caller.take()

	// A fingerprint retired from an "error" row was never registered:
	// prime answers "not found", the list confirms it is absent, and the
	// entry resolves instead of pending forever.
	e.caller.setFail(prod+"."+ToolUpsert, "caller is not a BkAdmin")
	st, err = e.svc.Rotate(ctx, e.admin.ID, uid) // prod: row error(fp3); fp2 removed from prod
	if err != nil {
		t.Fatal(err)
	}
	fp3 := st.Fingerprint
	if len(st.PendingRemovals) != 0 || len(e.caller.hashes(prod)) != 0 {
		t.Fatalf("precondition: pending=%+v prod=%v", st.PendingRemovals, e.caller.hashes(prod))
	}
	e.caller.take()
	st, err = e.svc.Rotate(ctx, e.admin.ID, uid) // prod: fp3 retired though never registered
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingRemovals) != 0 {
		t.Errorf("never-registered fingerprint left pending: %+v", st.PendingRemovals)
	}
	calls := e.caller.take()
	var sawList bool
	for _, c := range calls {
		if c.Target == prod+"."+ToolList {
			sawList = true
		}
		if c.Target == prod+"."+ToolDelete && c.Args["virtualKeyHash"] != fp3 {
			t.Errorf("unexpected prod delete of %v", c.Args["virtualKeyHash"])
		}
	}
	if !sawList {
		t.Errorf("registry list not consulted after a failed delete: %v", targetsOf(calls))
	}
	if e.auditDecisions(t, RemovalAbsent) != 1 {
		t.Errorf("absent outcome audited %d times", e.auditDecisions(t, RemovalAbsent))
	}
}

func TestRevokeWithFailingDeleteThenRetryWithoutKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	const dev, prod = "BkCoreServices", "BkCoreServicesProd"

	st, err := e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := st.Fingerprint
	e.caller.take()

	e.caller.setFail(prod+"."+ToolDelete, "prod is read-only right now")
	st, err = e.svc.Revoke(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if st.HasKey {
		t.Error("key kept after revoke")
	}
	if agents, _ := e.id.ListAgents(ctx, uid); len(agents) != 0 {
		t.Error("identity agent kept after revoke")
	}
	if !eq(pendingKeys(st), []string{prod + ":" + fp1}) || !strings.Contains(st.PendingRemovals[0].Error, "read-only") {
		t.Errorf("pending after revoke = %+v", st.PendingRemovals)
	}
	if len(e.caller.hashes(dev)) != 0 || !eq(e.caller.hashes(prod), []string{fp1}) {
		t.Errorf("registry: dev=%v prod=%v", e.caller.hashes(dev), e.caller.hashes(prod))
	}
	e.caller.take()

	// Retry without a key: not ErrNoKey while something is pending.
	e.caller.setFail(prod+"."+ToolDelete, "")
	st, err = e.svc.Register(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatalf("register without key: %v", err)
	}
	if st.HasKey || len(st.PendingRemovals) != 0 || len(e.caller.hashes(prod)) != 0 {
		t.Errorf("after retry: status=%+v prod=%v", st, e.caller.hashes(prod))
	}
	if got := targetsOf(e.caller.take()); !eq(got, []string{prod + "." + ToolDelete}) {
		t.Errorf("retry calls = %v", got)
	}
	if _, err := e.svc.Register(ctx, e.admin.ID, uid); !errors.Is(err, ErrNoKey) {
		t.Errorf("register with nothing pending: %v", err)
	}
	// And a fresh issue afterwards starts clean.
	st, err = e.svc.Issue(ctx, e.admin.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasKey || len(st.PendingRemovals) != 0 || !eq(e.caller.hashes(prod), []string{st.Fingerprint}) {
		t.Errorf("re-issue = %+v prod=%v", st, e.caller.hashes(prod))
	}
}

func TestStatusJSONShape(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, err := e.svc.Status(ctx, e.member.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if regs, ok := m["registrations"].([]any); !ok || len(regs) != 0 {
		t.Errorf("registrations = %v (want [])", m["registrations"])
	}
	if p, ok := m["pending_removals"].([]any); !ok || len(p) != 0 {
		t.Errorf("pending_removals = %v (want [])", m["pending_removals"])
	}
	// With one pending entry the fields are exactly these.
	if _, err := e.svc.Issue(ctx, e.admin.ID, e.member.ID); err != nil {
		t.Fatal(err)
	}
	e.caller.setFail("BkCoreServices."+ToolDelete, "nope")
	if _, err := e.svc.Revoke(ctx, e.admin.ID, e.member.ID); err != nil {
		t.Fatal(err)
	}
	st, _ = e.svc.Status(ctx, e.member.ID)
	b, _ = json.Marshal(st)
	_ = json.Unmarshal(b, &m)
	p, _ := m["pending_removals"].([]any)
	if len(p) != 1 {
		t.Fatalf("pending_removals = %v", m["pending_removals"])
	}
	entry, _ := p[0].(map[string]any)
	for _, k := range []string{"upstream", "fingerprint", "error", "updated_at"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("pending removal lacks %q: %v", k, entry)
		}
	}
	if len(entry) != 4 || entry["upstream"] != "BkCoreServices" || entry["error"] != "nope" {
		t.Errorf("pending removal = %v", entry)
	}
}

func TestBackgroundRetryUsesRecordedAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.member.ID
	const dev, prod = "BkCoreServices", "BkCoreServicesProd"
	if _, err := e.svc.Issue(ctx, e.admin.ID, uid); err != nil {
		t.Fatal(err)
	}
	// A second admin revokes while prod's delete fails: the pending entry
	// records them.
	e.caller.setFail(prod+"."+ToolDelete, "busy")
	st, err := e.svc.Revoke(ctx, "u_admin2", uid)
	if err != nil {
		t.Fatal(err)
	}
	fp := st.PendingRemovals[0].Fingerprint
	e.caller.take()

	// No registries configured: the pass does nothing at all.
	e.svc.SetRegistries(nil)
	if resolved, remaining, err := e.svc.RetryRemovals(ctx); err != nil || resolved != 0 || remaining != 0 || len(e.caller.take()) != 0 {
		t.Errorf("pass without registries: %d %d %v", resolved, remaining, err)
	}
	e.svc.SetRegistries(RegistryListerFunc(func(context.Context) ([]string, error) { return []string{}, nil }))
	if _, _, _ = e.svc.RetryRemovals(ctx); len(e.caller.take()) != 0 {
		t.Error("pass with an empty registry list called the gateway")
	}
	e.svc.SetRegistries(RegistryListerFunc(func(context.Context) ([]string, error) { return []string{dev, prod}, nil }))

	// Still failing: the pass reports it and keeps the entry.
	resolved, remaining, err := e.svc.RetryRemovals(ctx)
	if err != nil || resolved != 0 || remaining != 1 {
		t.Errorf("pass while failing = %d %d %v", resolved, remaining, err)
	}
	calls := e.caller.take()
	for _, c := range calls {
		if c.Caller != "dashboard:u_admin2" {
			t.Errorf("background pass ran %s as %q, want the recorded admin", c.Target, c.Caller)
		}
	}
	// Recovered: the ticker's pass resolves it as that admin.
	e.caller.setFail(prod+"."+ToolDelete, "")
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.svc.StartRemovalRetry(tctx, 10*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := e.svc.Status(ctx, uid); len(st.PendingRemovals) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if st, _ := e.svc.Status(ctx, uid); len(st.PendingRemovals) != 0 {
		t.Fatalf("background retry did not resolve the removal: %+v", st.PendingRemovals)
	}
	if e.caller.registry[prod][fp] {
		t.Error("prod still holds the revoked fingerprint")
	}
	for _, c := range e.caller.take() {
		if c.Caller != "dashboard:u_admin2" {
			t.Errorf("ticker pass ran %s as %q", c.Target, c.Caller)
		}
	}
	// An entry without a recorded admin is left alone, with the reason.
	if err := e.svc.retire(ctx, dev, strings.Repeat("a", 64), uid, "", "orphan"); err != nil {
		t.Fatal(err)
	}
	if _, remaining, _ := e.svc.RetryRemovals(ctx); remaining != 1 {
		t.Errorf("entry without admin: remaining=%d", remaining)
	}
	if st, _ := e.svc.Status(ctx, uid); len(st.PendingRemovals) != 1 || !errors.Is(errNoActor, errNoActor) || st.PendingRemovals[0].Error != errNoActor.Error() {
		t.Errorf("entry without admin = %+v", st.PendingRemovals)
	}
	if len(e.caller.take()) != 0 {
		t.Error("gateway called for an entry with no admin")
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

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/trustedmcp"
)

// These tests are the normal Toolyard connector against the actual pinned BKS
// build_app, IdentityMiddleware and Python MCP SDK. No HTTP response is mocked.
// Cloud CI explicitly enables them; ordinary unit tests do not launch a service.
type actualBKSOptions struct {
	initializeDelay          float64
	loseFirstCreateResponse  bool
	sourceDelay              float64
	reflectSnapshotFragments bool
}

func actualBKSFixture(t *testing.T, options actualBKSOptions) string {
	t.Helper()
	if os.Getenv("BKS_MCP_INTEGRATION") != "1" {
		t.Skip("actual Python MCP fixture requires the scoped hosted-CI step")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("actual MCP fixture may run only on a verified GitHub-hosted runner")
	}
	python := os.Getenv("BKS_MCP_PYTHON")
	if !filepath.IsAbs(python) {
		t.Fatal("pinned CI Python executable unavailable")
	}
	root, err := filepath.Abs("../trustedmcp/testdata/bks_server")
	if err != nil {
		t.Fatal("fixture directory unavailable")
	}
	for name, expected := range map[string]string{
		"identity.py":      "09605014985c1770de1abbd4514bedaab78c4d678d604853e31a9ea6144051f6",
		"leases.py":        "c5a2916ee605269de949d497cbc62d0afeb2336f97a294cfce9b7f326d739e35",
		"mcp_server.py":    "29e917a9c72bf34e8d013b1cd41c16de63ee2bedc249eeb5d2adcecf002f1737",
		"sources.py":       "fa8723db37a4ae60b8705f8792bfe740fb9a138db2153faa0702874d132fa815",
		"requirements.txt": "264921497f93f18312e9a83f60212af10f5a7767eafd2bfc321a29ba65c484ca",
	} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal("pinned server fixture unavailable")
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != expected {
			t.Fatal("pinned BKS server integrity changed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, python, filepath.Join(root, "harness.py"))
	// No inherited credential variables or issuer material enter argv/env/logs.
	cmd.Env = []string{"PYTHONDONTWRITEBYTECODE=1", "PATH=" + filepath.Dir(python)}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal("fixture stdin unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stdin.Close()
		t.Fatal("fixture readiness pipe unavailable")
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		t.Fatal("actual Python MCP fixture did not start")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		stdin.Close()
		cancel()
		_ = cmd.Process.Kill()
		stdout.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture process did not exit after cancellation")
		}
	})
	key := make([]int, 32)
	for i := range key {
		key[i] = int('i')
	}
	if json.NewEncoder(stdin).Encode(map[string]any{"key": key, "initialize_delay_seconds": options.initializeDelay, "lose_first_create_response": options.loseFirstCreateResponse, "source_delay_seconds": options.sourceDelay, "reflect_snapshot_fragments": options.reflectSnapshotFragments}) != nil {
		t.Fatal("synthetic fixture configuration unavailable")
	}
	type readiness struct {
		Ready bool   `json:"ready"`
		Port  int    `json:"port"`
		SDK   string `json:"sdk"`
	}
	ready := make(chan readiness, 1)
	go func() {
		var result readiness
		if json.NewDecoder(io.LimitReader(stdout, 1024)).Decode(&result) == nil {
			ready <- result
		}
	}()
	select {
	case result := <-ready:
		if !result.Ready || result.Port < 1 || result.Port > 65535 || result.SDK != "2.3.0" {
			t.Fatal("fixture readiness or SDK version invalid")
		}
		return fmt.Sprintf("http://127.0.0.1:%d/mcp", result.Port)
	case <-time.After(15 * time.Second):
		t.Fatal("actual pinned MCP fixture readiness timed out")
	}
	return ""
}

func actualBKSProfile(t *testing.T, endpoint string) trustedmcp.Profile {
	t.Helper()
	id := func() map[string]any {
		return map[string]any{"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}
	}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": append([]string{}, required...), "additionalProperties": false}
	}
	p := trustedmcp.Profile{
		Version: 1, Endpoint: endpoint, Issuer: "toolyard", Audience: "bk-agent-test-pilot-preview-v1",
		KeyID: "toolyard-preview-v1", ProtocolVersion: "2025-11-25", TimeoutSeconds: 65, HeaderTimeoutSeconds: 35,
		Tools: []trustedmcp.ToolProfile{
			{Alias: "create", Operation: "preview_create", Description: "Queue a bounded synthetic preview", IdempotencyField: "request_id", Schema: schema(map[string]any{"source_ref": map[string]any{"type": "string", "minLength": 1, "maxLength": 210}, "request_id": id(), "snapshot_id": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9-]{0,62}$"}, "pool": map[string]any{"type": "string", "enum": []string{"large", "medium"}, "default": "large"}, "lifetime_seconds": map[string]any{"type": "integer", "minimum": 300, "maximum": 21600, "default": 1200}}, "source_ref", "request_id", "snapshot_id")},
			{Alias: "inspect", Operation: "preview_inspect", Description: "Inspect the owned preview", ReadOnly: true, Schema: schema(map[string]any{"environment_id": id()}, "environment_id")},
			{Alias: "heartbeat", Operation: "preview_heartbeat", Description: "Report owned activity", Schema: schema(map[string]any{"environment_id": id(), "job_id": id()}, "environment_id")},
			{Alias: "release", Operation: "preview_release", Description: "Release owned activity", Destructive: true, Schema: schema(map[string]any{"environment_id": id(), "job_id": id(), "outcome": map[string]any{"type": "string", "enum": []string{"released", "success", "failure"}, "default": "released"}}, "environment_id")},
			{Alias: "snapshots", Operation: "preview_snapshots", Description: "List reviewed synthetic snapshots", ReadOnly: true, Schema: schema(map[string]any{})},
			{Alias: "results", Operation: "preview_results", Description: "Read owned durable receipts", ReadOnly: true, Schema: schema(map[string]any{"environment_id": id()}, "environment_id")},
		},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal("test profile encoding failed")
	}
	profile, err := trustedmcp.ParseProfile(raw)
	if err != nil {
		t.Fatal("reviewed test profile refused")
	}
	return profile
}

type actualBKSIdentity struct {
	db       *store.DB
	registry string
	issuer   string
	bindings []trustedmcp.Binding
	agents   []string
	owners   []string
}

func actualBKSEnrollment(t *testing.T) *actualBKSIdentity {
	t.Helper()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private fixture directory unavailable")
	}
	db, err := store.Open(filepath.Join(dir, "synthetic.db"))
	if err != nil {
		t.Fatal("synthetic enrollment database unavailable")
	}
	t.Cleanup(func() { db.Close() })
	result := &actualBKSIdentity{db: db, registry: filepath.Join(dir, "bindings.json"), issuer: filepath.Join(dir, "issuer.key")}
	if os.WriteFile(result.issuer, bytes.Repeat([]byte{'i'}, 32), 0600) != nil {
		t.Fatal("private synthetic issuer file unavailable")
	}
	service := identity.New(db)
	for i, owner := range []string{"u_b145e35d-fbc0-4d81-875f-407c4b126966", "u_2c6050ad-cb61-45c2-bb3d-d59bdb9d0322"} {
		if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,1,1)`, owner, owner, "synthetic-test-password-hash"); err != nil {
			t.Fatal("synthetic owner unavailable")
		}
		code, _, err := service.CreateEnrollment(context.Background(), owner, fmt.Sprintf("synthetic-agent-%d", i), time.Minute)
		if err != nil {
			t.Fatal("synthetic enrollment creation failed")
		}
		token, _, err := service.ExchangeEnrollment(context.Background(), code)
		if err != nil {
			t.Fatal("synthetic enrollment exchange failed")
		}
		agent, err := service.VerifyAgentToken(context.Background(), token)
		if err != nil {
			t.Fatal("synthetic enrolled token verification failed")
		}
		now := time.Now().Unix()
		sid := []string{"12d7f08e-6d9d-471d-b74c-80e70ef51ea7", "59188cff-ff1e-4c49-8f16-f2dc5e9a3599"}[i]
		result.bindings = append(result.bindings, trustedmcp.Binding{PrincipalID: agent.ID, OwnerUserID: owner, SessionID: sid, IssuedAt: now - 1, ExpiresAt: now + 600, CredentialMode: "dedicated-per-session", ProofSHA256: strings.Repeat("c", 64), ApprovedBy: owner})
		result.agents, result.owners = append(result.agents, agent.ID), append(result.owners, owner)
	}
	result.write(t, result.bindings)
	return result
}

func (i *actualBKSIdentity) write(t *testing.T, bindings []trustedmcp.Binding) {
	t.Helper()
	raw, err := json.Marshal(trustedmcp.Registry{Version: 1, Bindings: bindings})
	if err != nil || os.WriteFile(i.registry, raw, 0600) != nil {
		t.Fatal("private synthetic operator registry unavailable")
	}
}

func (i *actualBKSIdentity) client(t *testing.T, profile trustedmcp.Profile) *trustedmcp.Client {
	t.Helper()
	signer, err := trustedmcp.NewSigner(bytes.Repeat([]byte{'i'}, 32), &trustedmcp.FileRegistry{Path: i.registry, OwnerUID: uint32(os.Geteuid())}, func(ctx context.Context, agent string) (trustedmcp.Principal, error) {
		return trustedmcp.CurrentPrincipal(ctx, i.db.DB, agent)
	}, &trustedmcp.SQLLedger{DB: i.db.DB})
	if err != nil {
		t.Fatal("trusted fixture signer unavailable")
	}
	signer.KeySource = func() ([]byte, error) {
		return trustedmcp.PrivateFile(i.issuer, uint32(os.Geteuid()), 4096)
	}
	client, err := trustedmcp.NewClient(signer, profile)
	if err != nil {
		t.Fatal("trusted fixture connector unavailable")
	}
	profilePath := filepath.Join(filepath.Dir(i.registry), "profile.json")
	profileBytes, err := json.Marshal(profile)
	if err != nil || os.WriteFile(profilePath, profileBytes, 0600) != nil {
		t.Fatal("private reviewed fixture profile unavailable")
	}
	expectedDigest := sha256.Sum256(profileBytes)
	client.Check = func(ctx context.Context) error {
		if ctx.Err() != nil {
			return trustedmcp.ErrIdentity
		}
		current, err := trustedmcp.PrivateFile(profilePath, uint32(os.Geteuid()), 65536)
		if err != nil || sha256.Sum256(current) != expectedDigest {
			return trustedmcp.ErrIdentity
		}
		return nil
	}
	return client
}

func actualBKSGateway(t *testing.T, client *trustedmcp.Client, profile trustedmcp.Profile) *actorFixture {
	t.Helper()
	f := newActorFixture(t)
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "bks_preview", Transport: "http", URL: profile.Endpoint, PerUser: true, Trusted: client}); err != nil {
		t.Fatal("normal trusted upstream registration failed")
	}
	for _, tool := range profile.Tools {
		if _, err := f.gw.policy.Set(context.Background(), "tool", "bks_preview."+tool.Alias, "allow", "synthetic CI policy only", true); err != nil {
			t.Fatal("synthetic fixture policy unavailable")
		}
	}
	return f
}

func actualBKSCall(t *testing.T, f *actorFixture, agent, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result := f.call(t, WithAgentID(context.Background(), agent), "cloud-contract", "bks_preview."+tool, args)
	if result.IsError {
		// Never print response content: a failing reflection boundary can contain
		// the assertion whose absence this integration must guarantee.
		t.Fatalf("actual MCP operation %s failed", tool)
	}
	return result
}

func TestBKSPreviewMCPActualSDKDiscoverySixOperationsAndReconnect(t *testing.T) {
	endpoint := actualBKSFixture(t, actualBKSOptions{loseFirstCreateResponse: true, sourceDelay: 3.25})
	profile := actualBKSProfile(t, endpoint)
	enrolled := actualBKSEnrollment(t)
	client := enrolled.client(t, profile)
	f := actualBKSGateway(t, client, profile)
	ctx := WithAgentID(context.Background(), enrolled.agents[0])
	discovered, err := client.Discover(ctx, enrolled.agents[0])
	if err != nil || len(discovered) != 6 {
		t.Fatal("actual Python SDK discovery did not return the reviewed six tools")
	}
	names := map[string]bool{}
	for _, tool := range discovered {
		names[tool.Name] = true
	}
	for _, alias := range []string{"create", "inspect", "heartbeat", "release", "snapshots", "results"} {
		if !names[alias] || !f.gw.HasTool("bks_preview."+alias) {
			t.Fatalf("reviewed alias %s missing from real discovery or normal registration", alias)
		}
	}
	if text := textOf(actualBKSCall(t, f, enrolled.agents[0], "snapshots", nil)); !strings.Contains(text, "synthetic-v1") {
		t.Fatal("actual snapshot catalog did not contain the fictional fixture")
	}
	request := "c779294a-a07b-4da0-8939-bfa6f2a1a57a"
	create := map[string]any{"source_ref": "pr:42", "request_id": request, "snapshot_id": "synthetic-v1", "lifetime_seconds": 300}
	started := time.Now()
	unknown := f.call(t, ctx, "cloud-contract", "bks_preview.create", create)
	if elapsed := time.Since(started); elapsed < 3200*time.Millisecond {
		t.Fatal("real resolver response did not pass the previous three-second header limit")
	}
	if !unknown.IsError || !strings.Contains(textOf(unknown), "unknown") || !strings.Contains(textOf(unknown), "original request ID") {
		t.Fatal("lost real control receipt did not preserve unknown-outcome retry guidance")
	}
	// The first control call committed before its response failed. Reconnect
	// and retain exactly the original request ID and parameters.
	retry := actualBKSGateway(t, enrolled.client(t, profile), profile)
	created := structured(t, actualBKSCall(t, retry, enrolled.agents[0], "create", create))
	environment, ok := created["environment_id"].(string)
	if !ok || environment == "" || created["state"] != "queued" {
		t.Fatal("actual create did not queue an isolated synthetic environment")
	}
	source, ok := created["source"].(map[string]any)
	if !ok || source["commit_sha"] != strings.Repeat("a", 40) || source["draft"] != true {
		t.Fatal("actual create did not preserve the immutable synthetic draft source")
	}
	// A fresh normal gateway and client reconnect to the same Python SDK server.
	// The resolver would return a different head if the control lost idempotence.
	reconnected := actualBKSGateway(t, enrolled.client(t, profile), profile)
	again := structured(t, actualBKSCall(t, reconnected, enrolled.agents[0], "create", create))
	againSource, ok := again["source"].(map[string]any)
	if !ok || again["environment_id"] != environment || againSource["commit_sha"] != strings.Repeat("a", 40) {
		t.Fatal("reconnect retry changed the pinned environment or source")
	}
	args := map[string]any{"environment_id": environment}
	if structured(t, actualBKSCall(t, f, enrolled.agents[0], "inspect", args))["environment_id"] != environment {
		t.Fatal("actual inspect lost the owned environment")
	}
	actualBKSCall(t, f, enrolled.agents[0], "heartbeat", args)
	if structured(t, actualBKSCall(t, f, enrolled.agents[0], "results", args))["state"] != "queued" {
		t.Fatal("actual results misreported readiness")
	}
	foreign := f.call(t, WithAgentID(context.Background(), enrolled.agents[1]), "cloud-contract", "bks_preview.release", args)
	if !foreign.IsError || strings.Contains(textOf(foreign), "owned by this caller") {
		t.Fatal("another enrolled owner gained access or received raw control diagnostics")
	}
	actualBKSCall(t, f, enrolled.agents[0], "inspect", args)
	failed := f.call(t, ctx, "cloud-contract", "bks_preview.create", map[string]any{"source_ref": "branch:private-error", "request_id": "eac526ba-d9e9-4775-b563-a1202c50c152", "snapshot_id": "synthetic-v1"})
	if !failed.IsError || strings.Contains(textOf(failed), "synthetic private diagnostic") {
		t.Fatal("actual control error escaped the connector redaction boundary")
	}
	for _, event := range f.drainAudit() {
		if strings.Contains(event.ResultSummary, "synthetic private diagnostic") {
			t.Fatal("actual control error escaped into gateway audit")
		}
	}
	actualBKSCall(t, f, enrolled.agents[0], "release", map[string]any{"environment_id": environment, "outcome": "success"})
}

func TestBKSPreviewMCPActualSDKDeferredApprovalAndRevocation(t *testing.T) {
	endpoint := actualBKSFixture(t, actualBKSOptions{})
	profile := actualBKSProfile(t, endpoint)
	enrolled := actualBKSEnrollment(t)
	f := actualBKSGateway(t, enrolled.client(t, profile), profile)
	if _, err := f.gw.policy.Set(context.Background(), "tool", "bks_preview.snapshots", "ask", "synthetic deferred CI policy", true); err != nil {
		t.Fatal("synthetic deferred policy unavailable")
	}
	ctx := WithAgentID(context.Background(), enrolled.agents[0])
	f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", nil)
	id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	if id == "" {
		t.Fatal("actual trusted connector skipped deferred approval")
	}
	req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, enrolled.owners[0])
	if err != nil {
		t.Fatal("synthetic deferred decision failed")
	}
	f.gw.Execute(context.Background(), req)
	actualBKSCall(t, f, enrolled.agents[0], "snapshots", map[string]any{ApprovalIDField: id})
	f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", nil)
	id = f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	enrolled.write(t, enrolled.bindings[1:])
	req, err = f.bus.Decide(context.Background(), id, approval.StatusAllowed, enrolled.owners[0])
	if err != nil {
		t.Fatal("synthetic revoked decision failed")
	}
	f.gw.Execute(context.Background(), req)
	res := f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
	if !res.IsError {
		t.Fatal("revoked registry executed through the actual SDK after deferred approval")
	}
}

func TestBKSPreviewMCPActualSDKApprovalExpiryBoundsInitialize(t *testing.T) {
	endpoint := actualBKSFixture(t, actualBKSOptions{initializeDelay: 0.5})
	profile := actualBKSProfile(t, endpoint)
	enrolled := actualBKSEnrollment(t)
	f := actualBKSGateway(t, enrolled.client(t, profile), profile)
	if _, err := f.gw.policy.Set(context.Background(), "tool", "bks_preview.snapshots", "ask", "synthetic expiry CI policy", true); err != nil {
		t.Fatal("synthetic expiry policy unavailable")
	}
	f.bus.SetTTL(200 * time.Millisecond)
	ctx := WithAgentID(context.Background(), enrolled.agents[0])
	f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", nil)
	id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, enrolled.owners[0])
	if err != nil {
		t.Fatal("synthetic expiring decision failed")
	}
	f.gw.Execute(context.Background(), req)
	res := f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
	if !res.IsError {
		t.Fatal("actual delayed initialize outlived its approved request")
	}
}

func TestBKSPreviewMCPActualSDKFragmentedAssertionRefusedBeforeDeferredCache(t *testing.T) {
	endpoint := actualBKSFixture(t, actualBKSOptions{reflectSnapshotFragments: true})
	profile := actualBKSProfile(t, endpoint)
	enrolled := actualBKSEnrollment(t)
	f := actualBKSGateway(t, enrolled.client(t, profile), profile)
	if _, err := f.gw.policy.Set(context.Background(), "tool", "bks_preview.snapshots", "ask", "synthetic reflection CI policy", true); err != nil {
		t.Fatal("synthetic reflection policy unavailable")
	}
	ctx := WithAgentID(context.Background(), enrolled.agents[0])
	f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", nil)
	id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	if id == "" {
		t.Fatal("reflection test did not enter real deferred approval")
	}
	req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, enrolled.owners[0])
	if err != nil {
		t.Fatal("synthetic reflection decision failed")
	}
	f.gw.Execute(context.Background(), req)
	cached, err := f.bus.Get(context.Background(), id)
	if err != nil || !cached.ResultIsError || cached.ResultExecutedAt == 0 {
		t.Fatal("fragmented real assertion reached the persisted success cache")
	}
	for _, text := range []string{cached.ResultEnvelope, cached.ResultError} {
		if strings.Contains(text, "eyJ") || strings.Contains(text, "Bearer ") || strings.Contains(text, "chunks") {
			t.Fatal("fragmented real assertion reached the deferred result row")
		}
	}
	result := f.call(t, ctx, "cloud-contract", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
	if !result.IsError || strings.Contains(textOf(result), "eyJ") || strings.Contains(textOf(result), "Bearer ") {
		t.Fatal("fragmented real assertion reached the resumed caller")
	}
	for _, event := range f.drainAudit() {
		if strings.Contains(event.ResultSummary, "eyJ") || strings.Contains(event.ResultSummary, "Bearer ") {
			t.Fatal("fragmented real assertion reached gateway audit")
		}
	}
}

package previewassertion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const agentA = "ag_15c39202-4355-48bd-9b7c-dda16cd837c6"
const agentB = "ag_2ba2fbf0-ed45-4349-9459-31d314b9d3a3"
const owner = "u_ccff10d3-59cd-46ec-92ca-4bf657e312e8"
const sessionA = "2d7debf2-ee5d-42b6-bc05-360a320eff72"
const sessionB = "2c705f36-52fe-4d34-b9cc-eaa07fd23872"
const now = int64(1800000000)

func binding(id, sid string) Binding {
	return Binding{id, owner, sid, now - 10, now + 120, "dedicated-per-session", strings.Repeat("f", 64), owner}
}

type lookupFunc func(context.Context, string, int64) (Binding, error)

func (f lookupFunc) Lookup(ctx context.Context, id string, n int64) (Binding, error) {
	return f(ctx, id, n)
}

type testLedger struct {
	mu       sync.Mutex
	bindings map[string]Binding
}

type ledgerFunc func(context.Context, Binding) error

func (f ledgerFunc) Commit(ctx context.Context, b Binding) error { return f(ctx, b) }

type approvalLedgerFuncs struct {
	commit func(context.Context, Binding) error
	before func(context.Context, string, int64) error
}

func (l approvalLedgerFuncs) Commit(ctx context.Context, b Binding) error {
	return l.commit(ctx, b)
}

func (l approvalLedgerFuncs) CommittedBefore(ctx context.Context, id string, created int64) error {
	return l.before(ctx, id, created)
}

func (l *testLedger) Commit(_ context.Context, b Binding) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bindings == nil {
		l.bindings = map[string]Binding{}
	}
	if old, ok := l.bindings[b.PrincipalID]; ok && (old.OwnerUserID != b.OwnerUserID || old.SessionID != b.SessionID || old.CredentialMode != b.CredentialMode || old.ProofSHA256 != b.ProofSHA256 || old.ApprovedBy != b.ApprovedBy) {
		return ErrIdentity
	}
	l.bindings[b.PrincipalID] = b
	return nil
}
func signer(t *testing.T, bindings ...Binding) *Signer {
	t.Helper()
	byID := map[string]Binding{}
	for _, b := range bindings {
		byID[b.PrincipalID] = b
	}
	s, err := NewSigner(bytes.Repeat([]byte{'q'}, 32), lookupFunc(func(_ context.Context, id string, _ int64) (Binding, error) {
		b, ok := byID[id]
		if !ok {
			return Binding{}, ErrIdentity
		}
		return b, nil
	}), func(_ context.Context, id string) (Principal, error) { return Principal{id, owner, false}, nil }, &testLedger{})
	if err != nil {
		t.Fatal(err)
	}
	s.Clock = func() time.Time { return time.Unix(now, 0) }
	return s
}
func claims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid token structure")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal("invalid token encoding")
	}
	var c map[string]any
	if json.Unmarshal(raw, &c) != nil {
		t.Fatal("invalid token claims")
	}
	return c
}
func TestPreviewAssertionInteroperatesWithPinnedPythonVerifier(t *testing.T) {
	root, err := filepath.Abs("testdata/python_contract")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"identity.py": "09605014985c1770de1abbd4514bedaab78c4d678d604853e31a9ea6144051f6", "leases.py": "c5a2916ee605269de949d497cbc62d0afeb2336f97a294cfce9b7f326d739e35"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("pinned Python contract integrity changed")
		}
	}
	s := signer(t, binding(agentA, sessionA))
	token, err := s.Assertion(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	program := `import json,sys
sys.path.insert(0,sys.argv[1])
from identity import verify_assertion
data=json.load(sys.stdin)
key=bytes(data['key'])
caller=verify_assertion(data['token'],key,1800000000)
assert caller.subject=='toolyard:agent:ag_15c39202-4355-48bd-9b7c-dda16cd837c6'
assert caller.session_id=='2d7debf2-ee5d-42b6-bc05-360a320eff72'
assert caller.human is False
for key,clock in [(bytes([key[0]^1])*len(key),1800000000),(key,1800000030)]:
    try:
        verify_assertion(data['token'],key,clock)
    except PermissionError:
        pass
    else:
        raise AssertionError('invalid assertion accepted')
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", program, root)
	raw, _ := json.Marshal(map[string]any{"token": token, "key": func() []int {
		key := make([]int, 32)
		for i := range key {
			key[i] = int('q')
		}
		return key
	}()})
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python contract failed (%v), output bytes %d", err, len(output))
	}
}
func TestPreviewParallelCallersKeepIndependentThirtySecondAssertions(t *testing.T) {
	s := signer(t, binding(agentA, sessionA), binding(agentB, sessionB))
	var wg sync.WaitGroup
	for id, sid := range map[string]string{agentA: sessionA, agentB: sessionB} {
		id, sid := id, sid
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				token, err := s.Assertion(context.Background(), id)
				if err != nil {
					t.Error(err)
					return
				}
				c := claims(t, token)
				if c["sid"] != sid || c["sub"] != "toolyard:agent:"+id || c["iss"] != "toolyard" || c["aud"] != Audience || c["human"] != false || c["exp"] != float64(now+30) {
					t.Error("assertion trust contract changed")
				}
			}
		}()
	}
	wg.Wait()
}
func TestPreviewAssertionRejectsMissingDisabledExpiredAndCrossOwnerBindings(t *testing.T) {
	for _, id := range []string{"", agentB, "dashboard:" + owner, "ag_------------------------------------"} {
		if _, err := signer(t, binding(agentA, sessionA)).Assertion(context.Background(), id); !errors.Is(err, ErrIdentity) {
			t.Error("untrusted identity accepted")
		}
	}
	for _, b := range []Binding{func() Binding { b := binding(agentA, sessionA); b.ExpiresAt = now; return b }(), func() Binding { b := binding(agentA, sessionA); b.IssuedAt = now + 1; return b }(), func() Binding { b := binding(agentA, sessionA); b.OwnerUserID = ""; return b }(), func() Binding { b := binding(agentA, sessionA); b.ProofSHA256 = ""; return b }()} {
		if _, err := signer(t, b).Assertion(context.Background(), agentA); !errors.Is(err, ErrIdentity) {
			t.Error("invalid binding accepted")
		}
	}
	for _, p := range []Principal{{agentA, owner, true}, {agentA, "u_577a6be4-c5fc-4616-8d70-9068c5cc1ea3", false}, {agentA, "", false}} {
		s := signer(t, binding(agentA, sessionA))
		p := p
		s.Principal = func(context.Context, string) (Principal, error) { return p, nil }
		if _, err := s.Assertion(context.Background(), agentA); !errors.Is(err, ErrIdentity) {
			t.Error("invalid owner accepted")
		}
	}
}
func TestPreviewPreflightPinsIdentityAcrossDeferredApprovalAndCapsExpiry(t *testing.T) {
	b := binding(agentA, sessionA)
	b.ExpiresAt = now + 2
	s := signer(t, b)
	s.Registry = lookupFunc(func(context.Context, string, int64) (Binding, error) { return b, nil })
	if err := s.Preflight(context.Background(), agentA); err != nil {
		t.Fatal(err)
	}
	token, err := s.Assertion(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if claims(t, token)["exp"] != float64(now+2) {
		t.Fatal("binding expiry extended")
	}
	b.SessionID = sessionB
	if _, err := s.Assertion(context.Background(), agentA); !errors.Is(err, ErrIdentity) {
		t.Fatal("deferred identity changed")
	}
}
func TestPreviewSignerRequiresLedgerAndRereadsIssuerWithoutRetainingTransientKey(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	if _, err := NewSigner(bytes.Repeat([]byte{'q'}, 32), s.Registry, s.Principal, nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("missing ledger accepted")
	}
	key := bytes.Repeat([]byte{'r'}, 32)
	s.KeySource = func() ([]byte, error) { return key, nil }
	if _, err := s.Assertion(context.Background(), agentA); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("transient issuer retained")
	}
	s.KeySource = func() ([]byte, error) { return nil, errors.New("private-path-detail") }
	if _, err := s.Assertion(context.Background(), agentA); !errors.Is(err, ErrIdentity) || strings.Contains(SafeError(err), "private-path-detail") {
		t.Fatal("issuer failure exposed")
	}
}

func TestPreviewRevocationDuringDurableCommitPreventsAssertion(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	revoked := false
	s.Registry = lookupFunc(func(context.Context, string, int64) (Binding, error) {
		if revoked {
			return Binding{}, ErrIdentity
		}
		return binding(agentA, sessionA), nil
	})
	s.Ledger = ledgerFunc(func(context.Context, Binding) error { revoked = true; return nil })
	if _, err := s.Assertion(context.Background(), agentA); !errors.Is(err, ErrIdentity) {
		t.Fatal("revocation during durable enrollment proceeded")
	}
}

func TestPreviewPreflightRejectsIssuerRemovalAndUnsafeRefresh(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	valid := true
	s.KeySource = func() ([]byte, error) {
		if !valid {
			return nil, errors.New("private issuer unavailable")
		}
		return bytes.Repeat([]byte{'q'}, 32), nil
	}
	if err := s.Preflight(context.Background(), agentA); err != nil {
		t.Fatal("valid runtime issuer refused")
	}
	valid = false
	if err := s.Preflight(context.Background(), agentA); !errors.Is(err, ErrIdentity) {
		t.Fatal("removed issuer accepted before approval")
	}
	short := bytes.Repeat([]byte{'q'}, 31)
	s.KeySource = func() ([]byte, error) { return short, nil }
	if err := s.Preflight(context.Background(), agentA); !errors.Is(err, ErrIdentity) || !bytes.Equal(short, make([]byte, 31)) {
		t.Fatal("malformed issuer accepted or retained")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func goodResponse(req *http.Request) *http.Response {
	raw, _ := io.ReadAll(req.Body)
	var body struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &body)
	switch body.Method {
	case "initialize":
		return response(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
	case "notifications/initialized":
		return response(202, "")
	default:
		return response(200, `{"jsonrpc":"2.0","id":2,"result":{"content":[],"structuredContent":{"state":"queued"}}}`)
	}
}
func TestPreviewFixedHandshakeRejectsForgedArgumentsWithoutNetwork(t *testing.T) {
	c, _ := NewClient(signer(t, binding(agentA, sessionA)))
	posts := 0
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		posts++
		if req.URL.String() != Endpoint || req.Method != "POST" || req.Header.Get("MCP-Protocol-Version") != ProtocolVersion || req.Header.Get("Accept") != "application/json, text/event-stream" || claims(t, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))["sid"] != sessionA {
			t.Fatal("transport escaped trust boundary")
		}
		return goodResponse(req), nil
	})
	if err := c.Preflight(context.Background(), agentA, "preview_snapshots", nil); err != nil || posts != 0 {
		t.Fatal("preflight used network")
	}
	if _, err := c.Call(context.Background(), agentA, "preview_snapshots", nil); err != nil || posts != 3 {
		t.Fatal("bounded stateless handshake failed")
	}
	for _, args := range []map[string]any{{"sid": sessionB}, {"session_id": sessionB}, {"Authorization": "forged"}, {"endpoint": "https://public.invalid"}, {"image": "caller-image"}} {
		before := posts
		if _, err := c.Call(context.Background(), agentA, "preview_snapshots", args); !errors.Is(err, ErrArguments) || posts != before {
			t.Fatal("forged identity reached transport")
		}
	}
}
func TestPreviewRevocationStopsEverySubsequentPost(t *testing.T) {
	for _, revokeAfter := range []int{0, 1, 2} {
		t.Run(string(rune('0'+revokeAfter)), func(t *testing.T) {
			s := signer(t, binding(agentA, sessionA))
			posts := 0
			s.Principal = func(_ context.Context, id string) (Principal, error) {
				return Principal{id, owner, posts >= revokeAfter}, nil
			}
			c, _ := NewClient(s)
			c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) { posts++; return goodResponse(req), nil })
			if _, err := c.Call(context.Background(), agentA, "preview_snapshots", nil); !errors.Is(err, ErrIdentity) || posts != revokeAfter {
				t.Fatal("revoked caller continued")
			}
		})
	}
}
func TestPreviewTransportDeniesRedirectStatefulAndMalformedResponses(t *testing.T) {
	for _, mode := range []string{"redirect", "session", "empty-session", "notification-session", "notification-body", "notification-size", "notification-content-type", "content-type", "version", "version-case", "oversize", "rpc-id", "rpc-null-id", "rpc-error", "duplicate", "rpc-case", "content-null", "content-case"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := NewClient(signer(t, binding(agentA, sessionA)))
			posts := 0
			c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				posts++
				r := goodResponse(req)
				if strings.HasPrefix(mode, "notification-") && posts != 2 {
					return r, nil
				}
				switch mode {
				case "redirect":
					r.StatusCode = 302
					r.Header.Set("Location", "https://public.invalid")
				case "session", "notification-session":
					r.Header.Set("Mcp-Session-Id", "unexpected")
				case "empty-session":
					r.Header.Set("Mcp-Session-Id", "")
				case "notification-body":
					r.Body = io.NopCloser(strings.NewReader("unexpected"))
				case "notification-size":
					r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", 1025)))
				case "content-type", "notification-content-type":
					r.Header.Set("Content-Type", "application/json-forged")
				case "version":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-01-01"}}`))
				case "version-case":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"ProtocolVersion":"2025-11-25"}}`))
				case "oversize":
					r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", 1048577)))
				case "rpc-id":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":9,"result":{}}`))
				case "rpc-null-id":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":null,"result":{}}`))
				case "rpc-error":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"synthetic-secret"}}`))
				case "duplicate":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"id":1,"result":{"protocolVersion":"2025-11-25"}}`))
				case "rpc-case":
					r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":9,"ID":1,"result":{"protocolVersion":"2025-11-25"}}`))
				case "content-null":
					if posts == 3 {
						r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{"content":null}}`))
					}
				case "content-case":
					if posts == 3 {
						r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{"Content":[]}}`))
					}
				}
				return r, nil
			})
			if _, err := c.Call(context.Background(), agentA, "preview_snapshots", nil); !errors.Is(err, ErrTransport) || strings.Contains(SafeError(err), "synthetic-secret") {
				t.Fatal("unsafe transport accepted")
			}
		})
	}
	c, _ := NewClient(signer(t, binding(agentA, sessionA)))
	transport := c.http.Transport.(*http.Transport)
	if transport.Proxy != nil || c.http.Timeout != 5*time.Second || !transport.DisableKeepAlives {
		t.Fatal("transport bounds changed")
	}
	if c.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect enabled")
	}
}

func TestPreviewCallBoundsPreflightAndSendsCanonicalPythonIntegers(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	s.Registry = lookupFunc(func(ctx context.Context, _ string, _ int64) (Binding, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("identity preflight lacks whole-operation deadline")
		}
		return binding(agentA, sessionA), nil
	})
	c, _ := NewClient(s)
	posts := 0
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		posts++
		raw, _ := io.ReadAll(req.Body)
		if posts == 3 {
			var body struct {
				Params struct {
					Arguments map[string]json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(raw, &body) != nil || string(body.Params.Arguments["lifetime_seconds"]) != "300" {
				t.Fatal("Python lifetime is not a canonical integer")
			}
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return goodResponse(req), nil
	})
	args := map[string]any{"source_ref": "pr:1039", "request_id": sessionA, "snapshot_id": "synthetic-v1", "lifetime_seconds": json.Number("300e0")}
	if _, err := c.Call(context.Background(), agentA, "preview_create", args); err != nil || posts != 3 {
		t.Fatal("bounded integer-normalized request failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(ctx, agentA, "preview_snapshots", nil); !errors.Is(err, ErrIdentity) || posts != 3 {
		t.Fatal("cancelled request reached transport")
	}
}

func TestPreviewStandalonePreflightHasWholeOperationDeadline(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	s.Registry = lookupFunc(func(ctx context.Context, _ string, _ int64) (Binding, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("standalone preflight lacks bounded deadline")
		}
		return binding(agentA, sessionA), nil
	})
	c, _ := NewClient(s)
	if err := c.Preflight(context.Background(), agentA, "preview_snapshots", nil); err != nil {
		t.Fatal("bounded standalone preflight failed")
	}
}

func TestPreviewHistoricalApprovalCannotCreateFirstEnrollment(t *testing.T) {
	s := signer(t, binding(agentA, sessionA))
	c, _ := NewClient(s)
	created := now * 1000
	if err := c.RequireApprovalBinding(context.Background(), agentA, created); !errors.Is(err, ErrIdentity) {
		t.Fatal("ledger without historical proof accepted")
	}
	alreadyCommitted := false
	commits := 0
	immutable := &testLedger{}
	if immutable.Commit(context.Background(), binding(agentA, sessionA)) != nil {
		t.Fatal("test enrollment unavailable")
	}
	s.Ledger = approvalLedgerFuncs{
		before: func(ctx context.Context, id string, cutoff int64) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 10*time.Second || id != agentA || cutoff != created {
				t.Fatal("historical enrollment proof lacks bounded exact request context")
			}
			if !alreadyCommitted {
				return ErrIdentity
			}
			return nil
		},
		commit: func(ctx context.Context, b Binding) error {
			commits++
			return immutable.Commit(ctx, b)
		},
	}
	if err := c.RequireApprovalBinding(context.Background(), agentA, created); !errors.Is(err, ErrIdentity) || commits != 0 {
		t.Fatal("historical approval created its first enrollment after approval")
	}
	alreadyCommitted = true
	if err := c.RequireApprovalBinding(context.Background(), agentA, created); err != nil || commits != 1 {
		t.Fatal("pre-existing exact enrollment refused")
	}
	s.Registry = lookupFunc(func(context.Context, string, int64) (Binding, error) { return binding(agentA, sessionB), nil })
	if err := c.RequireApprovalBinding(context.Background(), agentA, created); !errors.Is(err, ErrIdentity) {
		t.Fatal("historical approval switched immutable session")
	}
	if err := c.RequireApprovalBinding(context.Background(), agentA, 0); !errors.Is(err, ErrIdentity) {
		t.Fatal("missing approval timestamp accepted")
	}
}

func TestPreviewToolFailuresAndReflectedAssertionsNeverReachCaller(t *testing.T) {
	for _, mode := range []string{"tool-error", "wrong-error-type", "null-error-type", "error-reflection", "success-reflection", "escaped-reflection", "structured-reflection", "metadata-reflection", "previous-assertion-reflection"} {
		t.Run(mode, func(t *testing.T) {
			s := signer(t, binding(agentA, sessionA))
			tick := int64(0)
			s.Clock = func() time.Time { tick++; return time.Unix(now+tick, 0) }
			c, _ := NewClient(s)
			posts := 0
			var initialAssertion string
			c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				posts++
				assertion := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
				if posts == 1 {
					initialAssertion = assertion
				}
				if posts != 3 {
					return goodResponse(req), nil
				}
				result := map[string]any{"content": []any{map[string]any{"type": "text", "text": "safe synthetic receipt"}}, "isError": false}
				switch mode {
				case "tool-error":
					result["isError"] = true
					result["content"] = []any{map[string]any{"type": "text", "text": "untrusted remote diagnostic"}}
				case "wrong-error-type":
					result["isError"] = "false"
				case "null-error-type":
					result["isError"] = nil
				case "error-reflection":
					result["isError"] = true
					result["content"] = []any{map[string]any{"type": "text", "text": "Bearer " + assertion}}
				case "success-reflection", "escaped-reflection":
					result["content"] = []any{map[string]any{"type": "text", "text": assertion}}
				case "structured-reflection":
					result["structuredContent"] = map[string]any{"authorization": assertion}
				case "metadata-reflection":
					result["_meta"] = map[string]any{"authorization": assertion}
				case "previous-assertion-reflection":
					if initialAssertion == assertion {
						t.Fatal("test clock did not produce distinct assertions")
					}
					result["content"] = []any{map[string]any{"type": "text", "text": initialAssertion}}
				}
				raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": result})
				if mode == "escaped-reflection" {
					raw = []byte(strings.Replace(string(raw), assertion, `\u0065`+assertion[1:], 1))
				}
				return response(200, string(raw)), nil
			})
			result, err := c.Call(context.Background(), agentA, "preview_snapshots", nil)
			if !errors.Is(err, ErrTransport) || len(result) != 0 || SafeError(err) != ErrTransport.Error() {
				t.Fatal("unsafe tool response reached caller")
			}
		})
	}
}

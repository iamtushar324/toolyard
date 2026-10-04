// Package previewassertion implements the disabled-by-default fixed preview adapter.
// Caller identity comes only from trusted enrollment and operator bindings.
package previewassertion

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const Endpoint = "http://127.0.0.1:18791/mcp"
const Audience = "bk-agent-test-pilot-preview-v1"
const ProtocolVersion = "2025-11-25"

var ErrIdentity = errors.New("trusted preview caller/session binding unavailable")
var ErrTransport = errors.New("private preview transport unavailable or invalid")
var ErrArguments = errors.New("preview request arguments rejected")
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var agentRE = regexp.MustCompile(`^ag_[0-9a-f-]{36}$`)
var userRE = regexp.MustCompile(`^u_[0-9a-f-]{36}$`)
var snapshotRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Principal struct {
	ID, OwnerUserID string
	Disabled        bool
}
type PrincipalLookup func(context.Context, string) (Principal, error)
type Binding struct {
	PrincipalID    string `json:"principal_id"`
	OwnerUserID    string `json:"owner_user_id"`
	SessionID      string `json:"session_id"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      int64  `json:"expires_at"`
	CredentialMode string `json:"credential_mode"`
	ProofSHA256    string `json:"proof_sha256"`
	ApprovedBy     string `json:"approved_by"`
}
type BindingLookup interface {
	Lookup(context.Context, string, int64) (Binding, error)
}

// CommitmentLedger permanently records the first exact trusted enrollment tuple.
// Implementations must reject reassignment even after removal or restart.
type CommitmentLedger interface {
	Commit(context.Context, Binding) error
}

// ApprovalCommitmentLedger proves that enrollment was already committed when
// Toolyard created the approval request. It must never insert a missing record.
type ApprovalCommitmentLedger interface {
	CommittedBefore(context.Context, string, int64) error
}

func validateBinding(b Binding, now int64) bool {
	return agentRE.MatchString(b.PrincipalID) && uuidRE.MatchString(strings.TrimPrefix(b.PrincipalID, "ag_")) &&
		userRE.MatchString(b.OwnerUserID) && uuidRE.MatchString(strings.TrimPrefix(b.OwnerUserID, "u_")) &&
		uuidRE.MatchString(b.SessionID) && b.ApprovedBy == b.OwnerUserID && b.CredentialMode == "dedicated-per-session" &&
		digestRE.MatchString(b.ProofSHA256) && b.IssuedAt > 0 && b.ExpiresAt > b.IssuedAt &&
		b.ExpiresAt-b.IssuedAt <= 21600 && now >= b.IssuedAt && now < b.ExpiresAt
}

// uniqueJSONKeys rejects ambiguous identity and protocol JSON at every level.
func uniqueJSONKeys(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, err := d.Token()
				k, ok := t.(string)
				if err != nil || !ok || seen[k] || !value(depth+1) {
					return false
				}
				seen[k] = true
			}
			t, err := d.Token()
			return err == nil && t == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			t, err := d.Token()
			return err == nil && t == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

type Signer struct {
	key       []byte
	Registry  BindingLookup
	Principal PrincipalLookup
	Ledger    CommitmentLedger
	Clock     func() time.Time
	// KeySource rereads the private issuer file for each assertion. Configure once
	// before concurrent use. Returned transient key bytes are cleared after use.
	KeySource func() ([]byte, error)
}

func NewSigner(key []byte, registry BindingLookup, principal PrincipalLookup, ledger CommitmentLedger) (*Signer, error) {
	if len(key) < 32 || len(key) > 4096 || registry == nil || principal == nil || ledger == nil {
		return nil, ErrIdentity
	}
	return &Signer{key: append([]byte(nil), key...), Registry: registry, Principal: principal, Ledger: ledger, Clock: time.Now}, nil
}
func (s *Signer) trusted(ctx context.Context, id string) (Binding, Principal, int64, error) {
	if s == nil || s.Clock == nil || s.Registry == nil || s.Principal == nil || s.Ledger == nil || ctx.Err() != nil || !agentRE.MatchString(id) || !uuidRE.MatchString(strings.TrimPrefix(id, "ag_")) {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	now := s.Clock().Unix()
	b, err := s.Registry.Lookup(ctx, id, now)
	if err != nil || b.PrincipalID != id || !validateBinding(b, now) {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	p, err := s.Principal(ctx, id)
	if err != nil || p.Disabled || p.ID != id || p.OwnerUserID != b.OwnerUserID {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	if s.Ledger.Commit(ctx, b) != nil || ctx.Err() != nil {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	// A durable write can wait on a database lock. Recheck live revocation and
	// the exact immutable enrollment after it finishes, not before that wait.
	now = s.Clock().Unix()
	current, err := s.Registry.Lookup(ctx, id, now)
	if err != nil || current.PrincipalID != b.PrincipalID || !validateBinding(current, now) ||
		current.OwnerUserID != b.OwnerUserID || current.SessionID != b.SessionID ||
		current.CredentialMode != b.CredentialMode || current.ProofSHA256 != b.ProofSHA256 || current.ApprovedBy != b.ApprovedBy {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	p, err = s.Principal(ctx, id)
	if err != nil || p.Disabled || p.ID != id || p.OwnerUserID != current.OwnerUserID || ctx.Err() != nil {
		return Binding{}, Principal{}, 0, ErrIdentity
	}
	b = current
	return b, p, now, nil
}

// Preflight pins trusted identity before a request enters deferred approval.
// It neither mints an assertion nor performs network requests.
func (s *Signer) Preflight(ctx context.Context, id string) error {
	if s == nil {
		return ErrIdentity
	}
	if s.KeySource != nil {
		key, err := s.KeySource()
		valid := err == nil && len(key) >= 32 && len(key) <= 4096
		clear(key)
		if !valid {
			return ErrIdentity
		}
	} else if len(s.key) < 32 || len(s.key) > 4096 {
		return ErrIdentity
	}
	_, _, _, err := s.trusted(ctx, id)
	return err
}
func (s *Signer) Assertion(ctx context.Context, id string) (string, error) {
	var key []byte
	if s == nil {
		return "", ErrIdentity
	}
	if s.KeySource != nil {
		var err error
		key, err = s.KeySource()
		if err != nil {
			clear(key)
			return "", ErrIdentity
		}
		defer clear(key)
	} else {
		key = s.key
	}
	if len(key) < 32 || len(key) > 4096 {
		return "", ErrIdentity
	}
	// Read identity last, after all file access, immediately before mint and POST.
	b, p, now, err := s.trusted(ctx, id)
	if err != nil {
		return "", ErrIdentity
	}
	expires := now + 30
	if b.ExpiresAt < expires {
		expires = b.ExpiresAt
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": "toolyard-preview-v1"})
	payload, _ := json.Marshal(map[string]any{"iss": "toolyard", "aud": Audience, "sub": "toolyard:agent:" + p.ID, "sid": b.SessionID, "iat": now, "exp": expires, "human": false})
	encode := base64.RawURLEncoding.EncodeToString
	signed := encode(header) + "." + encode(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signed))
	return signed + "." + encode(mac.Sum(nil)), nil
}

// Client has no startup discovery, proxy, redirect, session, or anonymous fallback.
type Client struct {
	Signer *Signer
	http   *http.Client
}

func NewClient(signer *Signer) (*Client, error) {
	if signer == nil {
		return nil, ErrIdentity
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 3 * time.Second, DisableKeepAlives: true}
	return &Client{Signer: signer, http: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) post(ctx context.Context, id string, rpcID int, method string, params any, notification bool, assertions *[]string) (json.RawMessage, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if !notification {
		body["id"] = rpcID
	}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 16384 {
		return nil, ErrTransport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	token, err := c.Signer.Assertion(ctx, id)
	if err != nil {
		return nil, ErrIdentity
	}
	req.Header.Set("Authorization", "Bearer "+token)
	*assertions = append(*assertions, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrTransport
	}
	defer resp.Body.Close()
	if len(resp.Header.Values("Mcp-Session-Id")) != 0 {
		return nil, ErrTransport
	}
	limit := int64(1048576)
	if notification {
		limit = 1024
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(out)) > limit {
		return nil, ErrTransport
	}
	if notification {
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			return nil, ErrTransport
		}
		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			media, _, err := mime.ParseMediaType(contentType)
			if err != nil || media != "application/json" {
				return nil, ErrTransport
			}
		}
		// Stateless notifications have no JSON-RPC response body.
		if len(bytes.TrimSpace(out)) != 0 {
			return nil, ErrTransport
		}
		return nil, nil
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || err != nil || media != "application/json" || !uniqueJSONKeys(out) {
		return nil, ErrTransport
	}
	// No control response may reflect any assertion from this handshake. Decode
	// strings before comparison so JSON unicode escaping cannot hide a credential.
	if reflectsAssertion(out, *assertions) {
		return nil, ErrTransport
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(out, &envelope) != nil {
		return nil, ErrTransport
	}
	for field := range envelope {
		if field != "jsonrpc" && field != "id" && field != "result" && field != "error" {
			return nil, ErrTransport
		}
	}
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int            `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(out, &rpc) != nil || rpc.JSONRPC != "2.0" || rpc.ID == nil || *rpc.ID != rpcID || len(rpc.Error) != 0 || len(rpc.Result) == 0 || string(rpc.Result) == "null" {
		return nil, ErrTransport
	}
	return rpc.Result, nil
}

func reflectsAssertion(raw []byte, assertions []string) bool {
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return true
	}
	var contains func(any) bool
	contains = func(value any) bool {
		switch value := value.(type) {
		case string:
			for _, assertion := range assertions {
				if strings.Contains(value, assertion) {
					return true
				}
			}
		case []any:
			for _, item := range value {
				if contains(item) {
					return true
				}
			}
		case map[string]any:
			for field, item := range value {
				if contains(field) || contains(item) {
					return true
				}
			}
		}
		return false
	}
	return contains(decoded)
}

var arguments = map[string]map[string]bool{
	"preview_create":  {"source_ref": true, "request_id": true, "snapshot_id": true, "pool": true, "lifetime_seconds": true},
	"preview_inspect": {"environment_id": true}, "preview_heartbeat": {"environment_id": true, "job_id": true},
	"preview_release": {"environment_id": true, "outcome": true, "job_id": true}, "preview_snapshots": {}, "preview_results": {"environment_id": true},
}

func validArguments(operation string, args map[string]any) bool {
	allowed, ok := arguments[operation]
	if !ok {
		return false
	}
	for k, v := range args {
		if !allowed[k] {
			return false
		}
		switch k {
		case "source_ref":
			s, ok := v.(string)
			if !ok || !utf8.ValidString(s) || utf8.RuneCountInString(s) < 1 || utf8.RuneCountInString(s) > 210 || strings.TrimSpace(s) == "" {
				return false
			}
		case "request_id", "environment_id", "job_id":
			s, ok := v.(string)
			if !ok || !uuidRE.MatchString(s) {
				return false
			}
		case "snapshot_id":
			s, ok := v.(string)
			if !ok || !snapshotRE.MatchString(s) {
				return false
			}
		case "pool":
			s, ok := v.(string)
			if !ok || (s != "large" && s != "medium") {
				return false
			}
		case "outcome":
			s, ok := v.(string)
			if !ok || (s != "released" && s != "success" && s != "failure") {
				return false
			}
		case "lifetime_seconds":
			var n float64
			switch v := v.(type) {
			case int:
				n = float64(v)
			case int64:
				n = float64(v)
			case float64:
				n = v
			case json.Number:
				var err error
				n, err = v.Float64()
				if err != nil {
					return false
				}
			default:
				return false
			}
			if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 300 || n > 21600 {
				return false
			}
		}
	}
	if operation == "preview_create" {
		return args["source_ref"] != nil && args["request_id"] != nil && args["snapshot_id"] != nil
	}
	return operation == "preview_snapshots" || args["environment_id"] != nil
}
func (c *Client) Preflight(ctx context.Context, id, operation string, args map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !validArguments(operation, args) {
		return ErrArguments
	}
	if c == nil || c.Signer == nil {
		return ErrIdentity
	}
	return c.Signer.Preflight(ctx, id)
}

// RequireApprovalBinding rejects historical approvals without a pre-existing
// immutable enrollment. Only after that proof may current identity be rechecked.
func (c *Client) RequireApprovalBinding(ctx context.Context, id string, createdAtMillis int64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c == nil || c.Signer == nil || c.Signer.Ledger == nil || createdAtMillis <= 0 {
		return ErrIdentity
	}
	ledger, ok := c.Signer.Ledger.(ApprovalCommitmentLedger)
	if !ok || ledger.CommittedBefore(ctx, id, createdAtMillis) != nil {
		return ErrIdentity
	}
	return c.Signer.Preflight(ctx, id)
}
func (c *Client) Call(ctx context.Context, id, operation string, args map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Preflight(ctx, id, operation, args); err != nil {
		return nil, err
	}
	// Keep a fixed primitive copy. Normalize integer-valued MCP JSON numbers so
	// Python receives an integer even for an accepted json.Number("300e0").
	fixedArgs := make(map[string]any, len(args))
	for field, value := range args {
		if field == "lifetime_seconds" {
			switch value := value.(type) {
			case int:
				fixedArgs[field] = int64(value)
			case int64:
				fixedArgs[field] = value
			case float64:
				fixedArgs[field] = int64(value)
			case json.Number:
				number, _ := value.Float64()
				fixedArgs[field] = int64(number)
			}
		} else {
			fixedArgs[field] = value
		}
	}
	assertions := make([]string, 0, 3)
	initial, err := c.post(ctx, id, 1, "initialize", map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "toolyard-bks-preview-adapter", "version": "0.1.0"}}, false, &assertions)
	if err != nil {
		return nil, err
	}
	var initialized map[string]json.RawMessage
	var version string
	if json.Unmarshal(initial, &initialized) != nil || json.Unmarshal(initialized["protocolVersion"], &version) != nil || version != ProtocolVersion {
		return nil, ErrTransport
	}
	if _, err = c.post(ctx, id, 0, "notifications/initialized", nil, true, &assertions); err != nil {
		return nil, err
	}
	result, err := c.post(ctx, id, 2, "tools/call", map[string]any{"name": operation, "arguments": fixedArgs}, false, &assertions)
	if err != nil {
		return nil, err
	}
	var tool map[string]json.RawMessage
	var content []json.RawMessage
	if json.Unmarshal(result, &tool) != nil || json.Unmarshal(tool["content"], &content) != nil || content == nil {
		return nil, ErrTransport
	}
	if failure, ok := tool["isError"]; ok {
		// Tool-level errors are not safe response content. Do not copy even a
		// well-formed remote error body to the caller or Toolyard's audit records.
		if string(bytes.TrimSpace(failure)) != "false" {
			return nil, ErrTransport
		}
	}
	return result, nil
}
func Operations() []string {
	return []string{"preview_create", "preview_inspect", "preview_heartbeat", "preview_release", "preview_snapshots", "preview_results"}
}
func SafeError(err error) string {
	if errors.Is(err, ErrIdentity) {
		return ErrIdentity.Error()
	}
	if errors.Is(err, ErrTransport) {
		return ErrTransport.Error()
	}
	return "preview request rejected"
}

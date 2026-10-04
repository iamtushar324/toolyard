// Package trustedmcp implements an authenticated, profile-limited MCP connector.
// Caller identity comes only from trusted enrollment and operator bindings.
package trustedmcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ErrIdentity = errors.New("trusted MCP caller/session binding unavailable")
var ErrTransport = errors.New("trusted MCP transport unavailable or invalid")
var ErrArguments = errors.New("trusted MCP request arguments rejected")
var ErrUnknownOutcome = errors.New("trusted MCP operation outcome is unknown; inspect before retry")
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var agentRE = regexp.MustCompile(`^ag_[0-9a-f-]{36}$`)
var userRE = regexp.MustCompile(`^u_[0-9a-f-]{36}$`)

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
	key                     []byte
	issuer, audience, keyID string
	Registry                BindingLookup
	Principal               PrincipalLookup
	Ledger                  CommitmentLedger
	Clock                   func() time.Time
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
	if s == nil || !assertionConfigRE.MatchString(s.issuer) || !assertionConfigRE.MatchString(s.audience) || !assertionConfigRE.MatchString(s.keyID) {
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
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": s.keyID})
	payload, _ := json.Marshal(map[string]any{"iss": s.issuer, "aud": s.audience, "sub": "toolyard:agent:" + p.ID, "sid": b.SessionID, "iat": now, "exp": expires, "human": false})
	encode := base64.RawURLEncoding.EncodeToString
	signed := encode(header) + "." + encode(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signed))
	return signed + "." + encode(mac.Sum(nil)), nil
}

// Client has no startup discovery, proxy, redirect, session, or anonymous fallback.
type Client struct {
	Signer  *Signer
	Profile Profile
	// Check rechecks service-owned profile immutability and enabled connection
	// state. Configure once before concurrent use; caller input cannot set it.
	Check            func(context.Context) error
	http             *http.Client
	profile          Profile
	approvalIdentity string
	schemas          map[string]*jsonschema.Schema
}

func NewClient(signer *Signer, profile Profile) (*Client, error) {
	if signer == nil || validateProfile(profile) != nil {
		return nil, ErrIdentity
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, ErrIdentity
	}
	fixed, err := ParseProfile(raw)
	if err != nil {
		return nil, ErrIdentity
	}
	public, err := ParseProfile(raw)
	if err != nil {
		return nil, ErrIdentity
	}
	snapshot := *signer
	snapshot.issuer = fixed.Issuer
	snapshot.audience = fixed.Audience
	snapshot.keyID = fixed.KeyID
	schemas := map[string]*jsonschema.Schema{}
	for _, tool := range fixed.Tools {
		compiled, err := compileSchema(tool.Schema)
		if err != nil {
			return nil, ErrIdentity
		}
		schemas[tool.Operation] = compiled
	}
	httpTimeout := time.Duration(fixed.HeaderTimeoutSeconds+5) * time.Second
	if httpTimeout > time.Duration(fixed.TimeoutSeconds)*time.Second {
		httpTimeout = time.Duration(fixed.TimeoutSeconds) * time.Second
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: time.Duration(fixed.HeaderTimeoutSeconds) * time.Second, DisableKeepAlives: true}
	canonical, err := json.Marshal(fixed)
	if err != nil {
		return nil, ErrIdentity
	}
	digest := sha256.Sum256(canonical)
	return &Client{Signer: &snapshot, Profile: public, profile: fixed, approvalIdentity: hex.EncodeToString(digest[:]), schemas: schemas, http: &http.Client{Transport: transport, Timeout: httpTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// ApprovalIdentity binds pending approvals to the full private profile snapshot.
// Public metadata edits cannot change it; a different profile requires approval.
func (c *Client) ApprovalIdentity() string {
	if c == nil {
		return ""
	}
	return c.approvalIdentity
}
func (c *Client) post(ctx context.Context, id string, rpcID int, method string, params any, notification bool, assertions *[]string) (json.RawMessage, error) {
	if c == nil || c.http == nil || ctx.Err() != nil {
		return nil, ErrTransport
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.profile.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", c.profile.ProtocolVersion)
	if ctx.Err() != nil {
		return nil, ErrTransport
	}
	if c.Check == nil || c.Check(ctx) != nil {
		return nil, ErrIdentity
	}
	token, err := c.Signer.Assertion(ctx, id)
	if err != nil {
		return nil, ErrIdentity
	}
	req.Header.Set("Authorization", "Bearer "+token)
	*assertions = append(*assertions, token)
	if c.Check == nil || c.Check(ctx) != nil {
		return nil, ErrIdentity
	}
	if ctx.Err() != nil {
		return nil, ErrTransport
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, postError(ctx, method, err)
	}
	defer resp.Body.Close()
	if ctx.Err() != nil {
		return nil, postError(ctx, method, ctx.Err())
	}
	if len(resp.Header.Values("Mcp-Session-Id")) != 0 {
		return nil, ErrTransport
	}
	limit := int64(1048576)
	if notification {
		limit = 1024
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(out)) > limit {
		return nil, postError(ctx, method, err)
	}
	if ctx.Err() != nil {
		return nil, postError(ctx, method, ctx.Err())
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

func postError(ctx context.Context, method string, err error) error {
	var timeout net.Error
	if method == "tools/call" && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()) {
		return ErrUnknownOutcome
	}
	return ErrTransport
}

// reflectsAssertion conservatively rejects any eight-byte credential fragment,
// including JSON-escaped text, JSON nested in strings, and combined text chunks.
// False-positive refusal is safer than credential persistence in gateway audits.
func reflectsAssertion(raw []byte, assertions []string) bool {
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return true
	}
	fragments := map[string]bool{}
	for _, token := range assertions {
		for i := 0; i+8 <= len(token); i++ {
			fragments[token[i:i+8]] = true
		}
	}
	matches := func(text string) bool {
		for i := 0; i+8 <= len(text); i++ {
			if fragments[text[i:i+8]] {
				return true
			}
		}
		return false
	}
	var all, texts []string
	total := 0
	var visit func(any, int, int) bool
	visit = func(value any, depth, decodedStrings int) bool {
		if depth > 36 {
			return true
		}
		switch value := value.(type) {
		case string:
			total += len(value)
			if total > 4194304 || matches(value) {
				return true
			}
			all = append(all, value)
			if decodedStrings < 4 && json.Valid([]byte(value)) {
				var nested any
				if json.Unmarshal([]byte(value), &nested) == nil && visit(nested, depth+1, decodedStrings+1) {
					return true
				}
			}
		case []any:
			for _, item := range value {
				if visit(item, depth+1, decodedStrings) {
					return true
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if matches(key) {
					return true
				}
				if key == "text" {
					if text, ok := value[key].(string); ok {
						texts = append(texts, text)
					}
				}
				if visit(value[key], depth+1, decodedStrings) {
					return true
				}
			}
		}
		return false
	}
	if visit(decoded, 0, 0) {
		return true
	}
	return matches(strings.Join(texts, "")) || matches(strings.Join(all, ""))
}

// validateArguments uses only the reviewed local schema. No remote validator or
// caller-supplied schema can choose fields, defaults, identity, or transport.
func (c *Client) validateArguments(operation string, args map[string]any) (map[string]any, error) {
	if c == nil {
		return nil, ErrIdentity
	}
	schema, ok := c.schemas[operation]
	if !ok {
		return nil, ErrArguments
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil || len(raw) > 16384 {
		return nil, ErrArguments
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil || schema.Validate(instance) != nil {
		return nil, ErrArguments
	}
	fixed, ok := instance.(map[string]any)
	if !ok {
		return nil, ErrArguments
	}
	var reviewed map[string]any
	for _, tool := range c.profile.Tools {
		if tool.Operation == operation {
			reviewed = tool.Schema["properties"].(map[string]any)
		}
	}
	for field, value := range fixed {
		property, ok := reviewed[field].(map[string]any)
		if !ok || property["type"] != "integer" {
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return nil, ErrArguments
		}
		exact, ok := new(big.Rat).SetString(number.String())
		if !ok || !exact.IsInt() || !exact.Num().IsInt64() {
			return nil, ErrArguments
		}
		fixed[field] = exact.Num().Int64()
	}
	return fixed, nil
}
func (c *Client) Preflight(ctx context.Context, id, operation string, args map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.validateArguments(operation, args); err != nil {
		return err
	}
	if c == nil || c.Signer == nil || ctx.Err() != nil {
		return ErrIdentity
	}
	if c.Check == nil || c.Check(ctx) != nil {
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
	if c.Check == nil || c.Check(ctx) != nil {
		return ErrIdentity
	}
	ledger, ok := c.Signer.Ledger.(ApprovalCommitmentLedger)
	if !ok || ledger.CommittedBefore(ctx, id, createdAtMillis) != nil {
		return ErrIdentity
	}
	return c.Signer.Preflight(ctx, id)
}
func (c *Client) Call(ctx context.Context, id, operation string, args map[string]any) (json.RawMessage, error) {
	if c == nil {
		return nil, ErrIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.profile.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := c.Preflight(ctx, id, operation, args); err != nil {
		return nil, err
	}
	fixedArgs, err := c.validateArguments(operation, args)
	if err != nil {
		return nil, err
	}
	assertions := make([]string, 0, 4)
	if _, err := c.discover(ctx, id, &assertions); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrTransport
	}
	result, err := c.post(ctx, id, 3, "tools/call", map[string]any{"name": operation, "arguments": fixedArgs}, false, &assertions)
	if err != nil {
		return nil, err
	}
	var tool map[string]json.RawMessage
	var content []json.RawMessage
	if json.Unmarshal(result, &tool) != nil || json.Unmarshal(tool["content"], &content) != nil || content == nil {
		return nil, ErrTransport
	}
	if failure, ok := tool["isError"]; ok && string(bytes.TrimSpace(failure)) != "false" {
		return nil, ErrTransport
	}
	return result, nil
}
func SafeError(err error) string {
	if errors.Is(err, ErrIdentity) {
		return ErrIdentity.Error()
	}
	if errors.Is(err, ErrUnknownOutcome) {
		return ErrUnknownOutcome.Error()
	}
	if errors.Is(err, ErrTransport) {
		return ErrTransport.Error()
	}
	return "trusted MCP request rejected"
}

package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Once the owner registers a passkey, allowing any high-risk tool (see
// HighRisk) needs a fresh passkey assertion — Face ID on an iPhone. The
// assertion's challenge is bound to the exact decision (request, tools
// allowed, narrowed parameters, TTL) via DecisionDigest, so a confirmation
// for one decision can't be replayed for another. The WebAuthn ceremony
// itself lives in internal/passkey; the inbox only asks it to verify.

// PasskeyAssertion is the browser's WebAuthn response for a decision.
type PasskeyAssertion struct {
	SessionID string          `json:"session_id"`
	Response  json.RawMessage `json:"response"`
}

// PasskeyGate verifies passkey assertions.
type PasskeyGate interface {
	// Enabled reports whether the owner has registered a passkey.
	CheckEnabled(ctx context.Context) (bool, error)
	// VerifyCredential checks an assertion made for the decision with
	// this digest and returns the id of the credential that signed it, so
	// the decision can record which passkey confirmed it.
	VerifyCredential(ctx context.Context, digest string, a *PasskeyAssertion) (credentialID string, err error)
}

// Passkey errors.
var (
	ErrPasskeyRequired = errors.New("allowing high-risk tools needs your passkey (Face ID)")
	ErrPasskeyFailed   = errors.New("the passkey check failed")
)

// DecisionDigest is what a passkey confirmation commits to.
func DecisionDigest(id string, d Decision) string {
	b, _ := json.Marshal(struct {
		ID     string                        `json:"id"`
		Action string                        `json:"action"`
		Allow  []bool                        `json:"allow"`
		Params map[int]map[string]Constraint `json:"params,omitempty"`
		TTL    int                           `json:"ttl_seconds,omitempty"`
	}{id, d.Action, d.Allow, d.Params, d.TTLSeconds})
	sum := sha256.Sum256(append([]byte("toolyard-decide-v1\n"), b...))
	return hex.EncodeToString(sum[:])
}

// NeedsPasskey reports whether allowing these tools of r is high risk.
func NeedsPasskey(r *Request, allow []bool) bool {
	for i, t := range r.Tools {
		if i < len(allow) && allow[i] && toolHighRisk(t) {
			return true
		}
	}
	return false
}

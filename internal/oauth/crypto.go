// Package oauth handles OAuth 2.1 / PKCE flows for remote MCP upstreams,
// including discovery, Dynamic Client Registration, token storage with
// AES-GCM encryption, refresh-keeping, paste-back fallback, and the RFC
// 8628 device flow.
//
// crypto.go is the at-rest envelope. The actual AES-256-GCM primitive now
// lives in internal/sealbox (extracted so the secrets broker and sealed bot
// tokens can share it). The names below are thin aliases so existing oauth
// call-sites are unchanged.
package oauth

import (
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// MasterKeySize is the size of the AES key — 32 bytes for AES-256.
const MasterKeySize = sealbox.MasterKeySize

// Cipher is an alias for sealbox.Cipher. See internal/sealbox.
type Cipher = sealbox.Cipher

// NewCipher wraps a 32-byte key. Smaller keys are rejected.
func NewCipher(key []byte) (*Cipher, error) { return sealbox.NewCipher(key) }

// LoadOrCreateKey reads (or creates) the oauth master key file. We
// deliberately do NOT reuse the session.key — leaking the cookie HMAC
// shouldn't automatically expose every upstream's refresh token.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	return sealbox.LoadOrCreateKey(dataDir, "oauth.key")
}

// AAD builds an AEAD additional-data discriminator that binds an envelope
// to its (upstream, kind) tuple. A leaked/copied row can't be decrypted
// in another role because the AAD won't match.
func AAD(upstream, kind string) []byte {
	return []byte("toolyard.oauth.v1|" + upstream + "|" + kind)
}

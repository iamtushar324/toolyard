// Package oauth handles OAuth 2.1 / PKCE flows for remote MCP upstreams,
// including discovery, Dynamic Client Registration, token storage with
// AES-GCM encryption, refresh-keeping, paste-back fallback, and the RFC
// 8628 device flow.
//
// crypto.go is the at-rest envelope. Tokens are short on the wire; long
// at rest is unacceptable. AES-256-GCM with a per-row random nonce, key
// derived from a 32-byte master file (oauth.key) chmod 0600 in data dir.
package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MasterKeySize is the size of the AES key — 32 bytes for AES-256.
const MasterKeySize = 32

// Cipher wraps a 32-byte master key and exposes Seal / Open helpers
// scoped by an "aad" (additional authenticated data) discriminator so a
// row that says "access_token for upstream X" can never be replayed as
// "refresh_token for upstream Y" — the GCM tag would fail validation.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher wraps a 32-byte key. Smaller keys are rejected.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("oauth crypto: master key must be %d bytes, got %d", MasterKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal returns base64(nonce || ct || tag). Empty input encodes to the
// empty string so callers can persist NULL for missing tokens.
func (c *Cipher) Seal(plaintext, aad []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, plaintext, aad)
	return base64.RawStdEncoding.EncodeToString(ct), nil
}

// Open is the inverse. Empty input returns nil, nil.
func (c *Cipher) Open(s string, aad []byte) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("oauth crypto: ciphertext too short")
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	return c.aead.Open(nil, nonce, ct, aad)
}

// LoadOrCreateKey reads (or creates) the master key file. We deliberately
// do NOT reuse the session.key — leaking the cookie HMAC shouldn't
// automatically expose every upstream's refresh token.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "oauth.key")
	if b, err := os.ReadFile(path); err == nil {
		out, derr := base64.RawStdEncoding.DecodeString(string(b))
		if derr == nil && len(out) == MasterKeySize {
			return out, nil
		}
	}
	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// AAD builds an AEAD additional-data discriminator that binds an envelope
// to its (upstream, kind) tuple. A leaked/copied row can't be decrypted
// in another role because the AAD won't match.
func AAD(upstream, kind string) []byte {
	return []byte("toolyard.oauth.v1|" + upstream + "|" + kind)
}

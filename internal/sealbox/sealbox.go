// Package sealbox is toolyard's shared at-rest encryption envelope.
//
// It was extracted verbatim from internal/oauth/crypto.go so that more than
// one subsystem (OAuth tokens, the secrets broker, sealed bot tokens) can
// share the same AES-256-GCM primitive without importing internal/oauth.
//
// AES-256-GCM with a per-row random nonce, key derived from a 32-byte master
// file (e.g. oauth.key, secrets.key) chmod 0600 in the data dir. A caller-
// supplied AAD ("additional authenticated data") discriminator binds each
// envelope to its role so a row that says "access_token for upstream X" can
// never be replayed as "refresh_token for upstream Y" — the GCM tag fails.
package sealbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// MasterKeySize is the size of the AES key — 32 bytes for AES-256.
const MasterKeySize = 32

// Cipher wraps a 32-byte master key and exposes Seal / Open helpers scoped by
// an "aad" (additional authenticated data) discriminator.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher wraps a 32-byte key. Smaller keys are rejected.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("sealbox: master key must be %d bytes, got %d", MasterKeySize, len(key))
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

// Seal returns base64(nonce || ct || tag). Empty input encodes to the empty
// string so callers can persist NULL for missing values.
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
		return nil, errors.New("sealbox: ciphertext too short")
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	return c.aead.Open(nil, nonce, ct, aad)
}

// ErrBadKey is returned by DecodeKey for contents that are not a master key.
var ErrBadKey = errors.New("sealbox: not a base64-encoded 32-byte key")

// DecodeKey parses the contents of a master key file: the base64 encoding
// of exactly MasterKeySize bytes, unpadded (as toolyard writes it) or
// padded (as `openssl rand -base64 32` writes it). Surrounding whitespace,
// such as the trailing newline of a Docker or systemd secret, is ignored.
func DecodeKey(b []byte) ([]byte, error) {
	s := string(bytes.TrimSpace(b))
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding} {
		if out, err := enc.DecodeString(s); err == nil && len(out) == MasterKeySize {
			return out, nil
		}
	}
	return nil, ErrBadKey
}

// EncodeKey is the file form of a master key: unpadded standard base64,
// which every toolyard version reads.
func EncodeKey(key []byte) []byte {
	return []byte(base64.RawStdEncoding.EncodeToString(key))
}

// CreateKeyFile writes a new random master key to path and returns it. It
// opens path with O_CREATE|O_EXCL, so it never writes over an existing
// file (the error then wraps fs.ErrExist), creates it with mode 0600, and
// fsyncs the file and then its directory so the key survives a crash.
func CreateKeyFile(path string) ([]byte, error) {
	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(EncodeKey(key)); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return key, nil
}

// syncDir fsyncs a directory so a file just created in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// LoadOrCreateKey reads the 32-byte master key file named filename under
// dataDir, creating it when it does not exist. An unreadable or corrupt
// file is an error and is never replaced. Blast-radius partitioning: each
// subsystem keeps its own key file (oauth.key, secrets.key) so leaking one
// doesn't expose the others.
//
// It is for tests and tools. `toolyard serve` loads its keys through
// internal/keyguard, which also searches the credentials directories and
// never creates a key for a database that is already in use.
func LoadOrCreateKey(dataDir, filename string) ([]byte, error) {
	path := filepath.Join(dataDir, filename)
	for attempt := 0; ; attempt++ {
		b, err := os.ReadFile(path)
		if err == nil {
			key, derr := DecodeKey(b)
			if derr != nil {
				return nil, fmt.Errorf("sealbox: key file %s: %w (the file was left unchanged)", path, derr)
			}
			return key, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		key, err := CreateKeyFile(path)
		if errors.Is(err, fs.ErrExist) && attempt == 0 {
			continue // created concurrently: read that one
		}
		return key, err
	}
}

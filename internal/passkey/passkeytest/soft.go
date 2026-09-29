// Package passkeytest is a software WebAuthn authenticator for tests: an
// ES256 key that answers navigator.credentials.create/get the way a
// phone's platform authenticator (Face ID) would.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
)

// Authenticator holds one credential.
type Authenticator struct {
	Key    *ecdsa.PrivateKey
	CredID []byte
	Count  uint32
	// Origin is what the "browser" writes into clientDataJSON.
	Origin string
	// NoUV makes the authenticator report no user verification.
	NoUV bool
}

// New creates an authenticator for a page on origin.
func New(origin string) *Authenticator {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &Authenticator{Key: k, CredID: id, Origin: origin}
}

var b64 = base64.RawURLEncoding

func (a *Authenticator) flags(extra byte) byte {
	f := byte(0x01) | extra // user present
	if !a.NoUV {
		f |= 0x04 // user verified
	}
	return f
}

// Create answers a registration.
func (a *Authenticator) Create(opts *protocol.CredentialCreation) ([]byte, error) {
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": opts.Response.Challenge.String(), "origin": a.Origin})
	x := a.Key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.Key.PublicKey.Y.FillBytes(make([]byte, 32))
	em, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		return nil, err
	}
	cose, err := em.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		return nil, err
	}
	rp := sha256.Sum256([]byte(opts.Response.RelyingParty.ID))
	ad := append([]byte{}, rp[:]...)
	ad = append(ad, a.flags(0x40)) // attested credential data
	ad = binary.BigEndian.AppendUint32(ad, a.Count)
	ad = append(ad, make([]byte, 16)...) // aaguid
	ad = binary.BigEndian.AppendUint16(ad, uint16(len(a.CredID)))
	ad = append(ad, a.CredID...)
	ad = append(ad, cose...)
	ao, err := em.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.CredID), "rawId": b64.EncodeToString(a.CredID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cdj), "attestationObject": b64.EncodeToString(ao)},
	})
}

// Get answers an assertion for the user with this handle.
func (a *Authenticator) Get(opts *protocol.CredentialAssertion, userHandle string) ([]byte, error) {
	a.Count++
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": opts.Response.Challenge.String(), "origin": a.Origin})
	rp := sha256.Sum256([]byte(opts.Response.RelyingPartyID))
	ad := append([]byte{}, rp[:]...)
	ad = append(ad, a.flags(0))
	ad = binary.BigEndian.AppendUint32(ad, a.Count)
	ch := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.Key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.CredID), "rawId": b64.EncodeToString(a.CredID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cdj), "authenticatorData": b64.EncodeToString(ad),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString([]byte(userHandle))},
	})
}

// Package identitykeys issues each dashboard user one identity key and
// forwards it to Beknown services so their audit logs record the person.
//
// The key is the token of the user's dedicated identity agent
// (agents.kind = 'identity'): agents present it at /mcp as a bearer token
// or in x-bf-vk, exactly where a Bifrost virtual key went before. toolyard
// keeps the raw key sealed, forwards it on tool calls to upstreams whose
// identity setting names a header (x-bk-bifrost-vk), and registers its
// sha256 fingerprint in prime-service's bifrost_virtual_key_actors registry
// through each registry upstream's upsert-bifrost-virtual-key-actor tool,
// acting as the admin who provisioned it.
package identitykeys

import (
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Service owns identity keys and their registrations.
type Service struct {
	db     *store.DB
	cipher *sealbox.Cipher
}

// New returns a Service that seals keys with cipher.
func New(db *store.DB, cipher *sealbox.Cipher) *Service {
	return &Service{db: db, cipher: cipher}
}

package trustedmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// Registry contains only operator-verified dedicated-agent commitments.
// It is not an enrollment receipt and does not prove ownership by itself.
type Registry struct {
	Version  int       `json:"version"`
	Bindings []Binding `json:"bindings"`
}

// FileRegistry deliberately retains no identity cache. The durable ledger,
// not this reloadable file or process memory, prevents agent-ID reassignment.
type FileRegistry struct {
	Path     string
	OwnerUID uint32
}

// ValidateRegistryFile checks startup configuration without enrollment, network
// calls, or the presence of any active caller. Empty registries are permitted.
func ValidateRegistryFile(path string, ownerUID uint32) error {
	raw, err := PrivateFile(path, ownerUID, 65536)
	if err != nil {
		return ErrIdentity
	}
	_, err = decodeRegistry(raw)
	return err
}

func decodeRegistry(raw []byte) (Registry, error) {
	var r Registry
	if !uniqueJSONKeys(raw) {
		return r, ErrIdentity
	}
	// encoding/json struct decoding accepts case-insensitive field aliases.
	// Require exact canonical names before decoding trusted identity fields.
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 2 || envelope["version"] == nil || envelope["bindings"] == nil {
		return r, ErrIdentity
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(envelope["bindings"], &entries) != nil || len(entries) > 128 {
		return r, ErrIdentity
	}
	fields := []string{"principal_id", "owner_user_id", "session_id", "issued_at", "expires_at", "credential_mode", "proof_sha256", "approved_by"}
	for _, entry := range entries {
		if len(entry) != len(fields) {
			return r, ErrIdentity
		}
		for _, field := range fields {
			if entry[field] == nil {
				return r, ErrIdentity
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || r.Version != 1 || len(r.Bindings) > 128 {
		return r, ErrIdentity
	}
	seen := make(map[string]bool)
	for _, b := range r.Bindings {
		// Validate structural bounds independently of current expiry. One expired
		// entry must not revoke unrelated valid agents in the same registry.
		if !validateBinding(b, b.IssuedAt) || seen[b.PrincipalID] {
			return Registry{}, ErrIdentity
		}
		seen[b.PrincipalID] = true
	}
	return r, nil
}

func (r *FileRegistry) Lookup(ctx context.Context, id string, now int64) (Binding, error) {
	if r == nil || ctx.Err() != nil {
		return Binding{}, ErrIdentity
	}
	raw, err := PrivateFile(r.Path, r.OwnerUID, 65536)
	if err != nil {
		return Binding{}, ErrIdentity
	}
	registry, err := decodeRegistry(raw)
	if err != nil {
		return Binding{}, ErrIdentity
	}
	for _, b := range registry.Bindings {
		if b.PrincipalID == id {
			if !validateBinding(b, now) {
				return Binding{}, ErrIdentity
			}
			return b, nil
		}
	}
	return Binding{}, ErrIdentity
}

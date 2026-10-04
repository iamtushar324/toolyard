package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/previewassertion"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// configureBKSPreview never creates runtime files, calls the network, or makes
// configuration errors fatal to ordinary Toolyard startup. It is default-off.
func configureBKSPreview(gw *gateway.Gateway, db *store.DB, dataDir string, enabled bool) error {
	if !enabled {
		return nil
	}
	client, err := preparedPreviewClient(db, dataDir)
	if err != nil {
		return previewassertion.ErrIdentity
	}
	gw.RegisterBKSPreviewTools(client)
	return nil
}

func preparedPreviewClient(db *store.DB, dataDir string) (*previewassertion.Client, error) {
	if db == nil || db.DB == nil {
		return nil, previewassertion.ErrIdentity
	}
	uid := uint32(os.Geteuid())
	dir := filepath.Join(dataDir, "bks-preview")
	keyPath := filepath.Join(dir, "assertion.key")
	registryPath := filepath.Join(dir, "bindings.json")
	key, err := previewassertion.PrivateFile(keyPath, uid, 4096)
	if err != nil {
		return nil, previewassertion.ErrIdentity
	}
	defer clear(key)
	// Validate the operator file without making an enrollment commitment or
	// requiring any caller at startup. An empty registry remains fully closed.
	if err := previewassertion.ValidateRegistryFile(registryPath, uid); err != nil {
		return nil, previewassertion.ErrIdentity
	}
	lookup := func(ctx context.Context, id string) (previewassertion.Principal, error) {
		return previewassertion.CurrentPrincipal(ctx, db.DB, id)
	}
	signer, err := previewassertion.NewSigner(key, &previewassertion.FileRegistry{Path: registryPath, OwnerUID: uid}, lookup, &previewassertion.SQLLedger{DB: db.DB})
	if err != nil {
		return nil, previewassertion.ErrIdentity
	}
	signer.KeySource = func() ([]byte, error) { return previewassertion.PrivateFile(keyPath, uid, 4096) }
	return previewassertion.NewClient(signer)
}

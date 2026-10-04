package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/trustedmcp"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

var assertionProfileID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// This gate installs a reusable credential provider, not a server or enrollment.
// A missing profile refuses activation without affecting ordinary startup.
func configureAssertionProvider(svc *upstreams.Service, db *store.DB, dataDir string, enabled bool) {
	svc.SetAssertionProvider(func(ctx context.Context, srv upstreams.Server) (gateway.TrustedUpstream, error) {
		if !enabled || !assertionProfileID.MatchString(srv.AssertionProfile) {
			return nil, trustedmcp.ErrIdentity
		}
		profilePath := filepath.Join(dataDir, "trusted-mcp", srv.AssertionProfile, "profile.json")
		raw, err := trustedmcp.PrivateFile(profilePath, uint32(os.Geteuid()), 65536)
		if err != nil {
			return nil, trustedmcp.ErrIdentity
		}
		digest := sha256.Sum256(raw)
		expected, err := trustedmcp.ParseProfile(raw)
		if err != nil {
			return nil, trustedmcp.ErrIdentity
		}
		client, err := preparedAssertionClient(db, dataDir, srv.AssertionProfile, srv.URL)
		if err != nil {
			return nil, trustedmcp.ErrIdentity
		}
		after, err := trustedmcp.PrivateFile(profilePath, uint32(os.Geteuid()), 65536)
		if err != nil || sha256.Sum256(after) != digest {
			return nil, trustedmcp.ErrIdentity
		}
		expectedJSON, _ := json.Marshal(expected)
		clientJSON, _ := json.Marshal(client.Profile)
		if !bytes.Equal(expectedJSON, clientJSON) {
			return nil, trustedmcp.ErrIdentity
		}
		client.Check = func(ctx context.Context) error {
			if svc.AssertEnabled(ctx, srv.Name, srv.AssertionProfile, srv.URL) != nil {
				return trustedmcp.ErrIdentity
			}
			current, err := trustedmcp.PrivateFile(profilePath, uint32(os.Geteuid()), 65536)
			if err != nil || sha256.Sum256(current) != digest {
				return trustedmcp.ErrIdentity
			}
			return nil
		}
		return client, nil
	})
}

func preparedAssertionClient(db *store.DB, dataDir, profileID, endpoint string) (*trustedmcp.Client, error) {
	if db == nil || db.DB == nil || !assertionProfileID.MatchString(profileID) {
		return nil, trustedmcp.ErrIdentity
	}
	uid := uint32(os.Geteuid())
	dir := filepath.Join(dataDir, "trusted-mcp", profileID)
	profile, err := trustedmcp.LoadProfile(filepath.Join(dir, "profile.json"), uid)
	if err != nil || profile.Endpoint != endpoint {
		return nil, trustedmcp.ErrIdentity
	}
	kp, rp := filepath.Join(dir, "assertion.key"), filepath.Join(dir, "bindings.json")
	key, err := trustedmcp.PrivateFile(kp, uid, 4096)
	if err != nil {
		return nil, trustedmcp.ErrIdentity
	}
	defer clear(key)
	if trustedmcp.ValidateRegistryFile(rp, uid) != nil {
		return nil, trustedmcp.ErrIdentity
	}
	signer, err := trustedmcp.NewSigner(key, &trustedmcp.FileRegistry{Path: rp, OwnerUID: uid}, func(ctx context.Context, id string) (trustedmcp.Principal, error) {
		return trustedmcp.CurrentPrincipal(ctx, db.DB, id)
	}, &trustedmcp.SQLLedger{DB: db.DB})
	if err != nil {
		return nil, trustedmcp.ErrIdentity
	}
	signer.KeySource = func() ([]byte, error) { return trustedmcp.PrivateFile(kp, uid, 4096) }
	return trustedmcp.NewClient(signer, profile)
}

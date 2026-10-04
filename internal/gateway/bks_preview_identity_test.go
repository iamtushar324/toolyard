package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/trustedmcp"
)

// This control seam keeps the real registry, principal query and SQL ledger.
// Only the external control response is fake. It never sends network traffic.
type persistentPreviewControl struct {
	signer *trustedmcp.Signer
	posts  int
}

func (p *persistentPreviewControl) Preflight(ctx context.Context, id, operation string, args map[string]any) error {
	return p.signer.Preflight(ctx, id)
}

func (p *persistentPreviewControl) RequireApprovalBinding(ctx context.Context, id string, createdAtMillis int64) error {
	l, ok := p.signer.Ledger.(interface {
		CommittedBefore(context.Context, string, int64) error
	})
	if !ok || l.CommittedBefore(ctx, id, createdAtMillis) != nil {
		return trustedmcp.ErrIdentity
	}
	return p.signer.Preflight(ctx, id)
}

func (p *persistentPreviewControl) Call(ctx context.Context, id, operation string, args map[string]any) (json.RawMessage, error) {
	if err := p.signer.Preflight(ctx, id); err != nil {
		return nil, err
	}
	p.posts++
	return json.RawMessage(`{"content":[{"type":"text","text":"queued"}]}`), nil
}

func TestBKSPreviewDeferredApprovalRechecksRealRegistryPrincipalAndLedger(t *testing.T) {
	const agent = "ag_f7d57e98-1b79-4e66-93d5-7d565d43a1b0"
	const owner = "u_190c9df5-8703-48cf-a337-aa2e3a7901bb"
	const otherOwner = "u_359fe8a6-193a-4909-b9f8-d21d0924e309"
	const sid = "b9693d26-0366-4c10-81e3-fd4e3d435141"
	for _, scenario := range []string{"valid", "registry-removal", "session-reassignment", "proof-replacement", "expiry", "disabled", "owner-blocked", "owner-removed", "owner-reassignment"} {
		t.Run(scenario, func(t *testing.T) {
			f := newActorFixture(t)
			// Use real SQLite identity state and real gateway approval persistence.
			db, err := store.Open(filepath.Join(t.TempDir(), "identity.db"))
			if err != nil {
				t.Fatal("test identity database unavailable")
			}
			t.Cleanup(func() { db.Close() })
			for _, user := range []string{owner, otherOwner} {
				if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,1,1)`, user, user, "test-password-hash"); err != nil {
					t.Fatal("test owner failed")
				}
			}
			if _, err = db.Exec(`INSERT INTO agents(id,name,owner_user,token_hash,created_at) VALUES(?,?,?,?,1)`, agent, "dedicated-test-agent", owner, strings.Repeat("a", 64)); err != nil {
				t.Fatal("test agent failed")
			}
			now := int64(1800000000)
			b := trustedmcp.Binding{PrincipalID: agent, OwnerUserID: owner, SessionID: sid, IssuedAt: now - 1, ExpiresAt: now + 300, CredentialMode: "dedicated-per-session", ProofSHA256: strings.Repeat("c", 64), ApprovedBy: owner}
			registryDir := t.TempDir()
			if err := os.Chmod(registryDir, 0700); err != nil {
				t.Fatal("private test registry directory unavailable")
			}
			rp := filepath.Join(registryDir, "bindings.json")
			write := func(bindings []trustedmcp.Binding) {
				raw, err := json.Marshal(trustedmcp.Registry{Version: 1, Bindings: bindings})
				if err != nil || os.WriteFile(rp, raw, 0600) != nil {
					t.Fatal("test registry failed")
				}
			}
			write([]trustedmcp.Binding{b})
			newSigner := func() *trustedmcp.Signer {
				s, err := trustedmcp.NewSigner(bytes.Repeat([]byte{'q'}, 32), &trustedmcp.FileRegistry{Path: rp, OwnerUID: uint32(os.Geteuid())}, func(ctx context.Context, id string) (trustedmcp.Principal, error) {
					return trustedmcp.CurrentPrincipal(ctx, db.DB, id)
				}, &trustedmcp.SQLLedger{DB: db.DB})
				if err != nil {
					t.Fatal("test signer failed")
				}
				s.Clock = func() time.Time { return time.Unix(now, 0) }
				return s
			}
			control := &persistentPreviewControl{signer: newSigner()}
			f.gw.RegisterBKSPreviewTools(control)
			ctx := WithAgentID(context.Background(), agent)
			f.call(t, ctx, "test", "bks_preview.snapshots", nil)
			id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
			if id == "" {
				t.Fatal("valid identity did not enter deferred approval")
			}
			var mutation string
			switch scenario {
			case "registry-removal":
				write(nil)
			case "session-reassignment":
				b.SessionID = "d569a5e5-05dc-4d55-b6a2-a657b8a4eb78"
				write([]trustedmcp.Binding{b})
			case "proof-replacement":
				b.ProofSHA256 = strings.Repeat("d", 64)
				write([]trustedmcp.Binding{b})
			case "expiry":
				now = b.ExpiresAt
			case "disabled":
				mutation = `UPDATE agents SET disabled=1`
			case "owner-blocked":
				mutation = `UPDATE users SET status='blocked'`
			case "owner-removed":
				mutation = `DELETE FROM users WHERE id='` + owner + `'`
			case "owner-reassignment":
				mutation = `UPDATE agents SET owner_user='` + otherOwner + `'`
				b.OwnerUserID = otherOwner
				b.ApprovedBy = otherOwner
				write([]trustedmcp.Binding{b})
			}
			if mutation != "" {
				if _, err = db.Exec(mutation); err != nil {
					t.Fatal("test revocation failed")
				}
			}
			// A fresh signer cannot forget the pre-approval ledger commitment.
			control.signer = newSigner()
			req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, owner)
			if err != nil {
				t.Fatal("test approval failed")
			}
			f.gw.Execute(context.Background(), req)
			res := f.call(t, ctx, "test", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
			if scenario == "valid" {
				if res.IsError || control.posts != 1 {
					t.Fatal("valid approved call failed")
				}
			} else if !res.IsError || control.posts != 0 {
				t.Fatal("revoked or reassigned identity executed after deferred approval")
			}
		})
	}
}

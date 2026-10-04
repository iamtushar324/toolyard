package previewassertion

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const ledgerAgent = "ag_528cc45c-2f89-41dc-9e9b-89ee3b78c8b9"
const ledgerOwner = "u_0f7d4ecf-e540-4e67-a965-819e31c8eea8"
const ledgerOtherOwner = "u_39b9b7a9-3382-4217-8d1f-f8adf1e3d29f"
const ledgerSID = "dd9d9b20-3f3c-4699-a9d4-6e2e2f0fc3b4"
const ledgerOtherSID = "5d41ec49-6c9b-4405-bbfc-cc727ed23e90"
const ledgerNow = int64(1800000000)

func ledgerBinding() Binding {
	return Binding{PrincipalID: ledgerAgent, OwnerUserID: ledgerOwner, SessionID: ledgerSID, IssuedAt: ledgerNow - 1, ExpiresAt: ledgerNow + 300, CredentialMode: "dedicated-per-session", ProofSHA256: strings.Repeat("c", 64), ApprovedBy: ledgerOwner}
}

func ledgerDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal("test database unavailable")
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedLedgerPrincipal(t *testing.T, db *store.DB) {
	t.Helper()
	for _, id := range []string{ledgerOwner, ledgerOtherOwner} {
		if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,1,1)`, id, id, "synthetic-test-password-hash"); err != nil {
			t.Fatal("test owner unavailable")
		}
	}
	if _, err := db.Exec(`INSERT INTO agents(id,name,owner_user,token_hash,created_at) VALUES(?,?,?,?,1)`, ledgerAgent, "test-preview", ledgerOwner, strings.Repeat("a", 64)); err != nil {
		t.Fatal("test agent unavailable")
	}
}

func writeLedgerRegistry(t *testing.T, path string, bindings []Binding) {
	t.Helper()
	raw, err := json.Marshal(Registry{Version: 1, Bindings: bindings})
	if err != nil {
		t.Fatal("test registry encoding failed")
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal("test registry write failed")
	}
}

func persistentSigner(t *testing.T, db *store.DB, path string) *Signer {
	t.Helper()
	s, err := NewSigner([]byte(strings.Repeat("q", 32)), &FileRegistry{Path: path, OwnerUID: uint32(os.Geteuid())}, func(ctx context.Context, id string) (Principal, error) { return CurrentPrincipal(ctx, db.DB, id) }, &SQLLedger{DB: db.DB})
	if err != nil {
		t.Fatal("test signer unavailable")
	}
	s.Clock = func() time.Time { return time.Unix(ledgerNow, 0) }
	return s
}

func TestPreviewLedgerRestartRemovalRotationAndSessionReassignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	db := ledgerDB(t, path)
	seedLedgerPrincipal(t, db)
	registryPath := filepath.Join(dir, "bindings.json")
	b := ledgerBinding()
	writeLedgerRegistry(t, registryPath, []Binding{b})
	s := persistentSigner(t, db, registryPath)
	ctx := context.Background()
	if s.Preflight(ctx, ledgerAgent) != nil {
		t.Fatal("valid operator commitment refused")
	}
	// Credential rotation cannot change the agent's immutable owner/session.
	if _, err := identity.New(db).RotateAgentToken(ctx, ledgerOwner, ledgerAgent, time.Minute); err != nil {
		t.Fatal("test rotation failed")
	}
	if s.Preflight(ctx, ledgerAgent) != nil {
		t.Fatal("same binding refused after rotation")
	}
	writeLedgerRegistry(t, registryPath, nil)
	if s.Preflight(ctx, ledgerAgent) == nil {
		t.Fatal("removed binding accepted")
	}
	if db.Close() != nil {
		t.Fatal("test restart close failed")
	}
	db = ledgerDB(t, path)
	s = persistentSigner(t, db, registryPath)
	b.SessionID = ledgerOtherSID
	writeLedgerRegistry(t, registryPath, []Binding{b})
	if s.Preflight(ctx, ledgerAgent) == nil {
		t.Fatal("agent ID reassigned after restart/removal")
	}
	b = ledgerBinding()
	b.ExpiresAt += 300
	writeLedgerRegistry(t, registryPath, []Binding{b})
	if s.Preflight(ctx, ledgerAgent) != nil {
		t.Fatal("validity renewal changed immutable binding")
	}
	b.ProofSHA256 = strings.Repeat("d", 64)
	writeLedgerRegistry(t, registryPath, []Binding{b})
	if s.Preflight(ctx, ledgerAgent) == nil {
		t.Fatal("proof replacement accepted")
	}
}

func TestPreviewLedgerSurvivesAgentDeletionAndCrossOwnerReassignment(t *testing.T) {
	dir := t.TempDir()
	db := ledgerDB(t, filepath.Join(dir, "ledger.db"))
	seedLedgerPrincipal(t, db)
	rp := filepath.Join(dir, "bindings.json")
	b := ledgerBinding()
	writeLedgerRegistry(t, rp, []Binding{b})
	s := persistentSigner(t, db, rp)
	ctx := context.Background()
	if s.Preflight(ctx, ledgerAgent) != nil {
		t.Fatal("initial commitment refused")
	}
	if _, err := db.Exec(`DELETE FROM agents WHERE id=?`, ledgerAgent); err != nil {
		t.Fatal("test deletion failed")
	}
	if _, err := db.Exec(`INSERT INTO agents(id,name,owner_user,token_hash,created_at) VALUES(?,?,?,?,1)`, ledgerAgent, "reused", ledgerOtherOwner, strings.Repeat("b", 64)); err != nil {
		t.Fatal("test recreation failed")
	}
	b.OwnerUserID = ledgerOtherOwner
	b.ApprovedBy = ledgerOtherOwner
	writeLedgerRegistry(t, rp, []Binding{b})
	if persistentSigner(t, db, rp).Preflight(ctx, ledgerAgent) == nil {
		t.Fatal("deleted agent ID reassigned to another owner")
	}
}

func TestPreviewLedgerRejectsUpdateDeleteAndReplace(t *testing.T) {
	db := ledgerDB(t, filepath.Join(t.TempDir(), "ledger.db"))
	seedLedgerPrincipal(t, db)
	l := &SQLLedger{DB: db.DB}
	b := ledgerBinding()
	ctx := context.Background()
	if l.Commit(ctx, b) != nil {
		t.Fatal("initial commitment refused")
	}
	var synchronous int
	if db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous) != nil || synchronous != 2 {
		t.Fatal("enrollment commitment did not enforce full disk synchronization")
	}
	for _, statement := range []string{
		`UPDATE bks_preview_enrollment_ledger SET session_id='changed'`,
		`DELETE FROM bks_preview_enrollment_ledger`,
		`INSERT OR REPLACE INTO bks_preview_enrollment_ledger SELECT agent_id,owner_user_id,'changed',proof_sha256,credential_mode,approved_by,recorded_at FROM bks_preview_enrollment_ledger`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("immutable ledger mutation accepted")
		}
	}
	if l.Commit(ctx, b) != nil {
		t.Fatal("mutation attempt damaged original commitment")
	}
}

func TestPreviewLedgerConcurrentFirstUseCommitsOneSession(t *testing.T) {
	db := ledgerDB(t, filepath.Join(t.TempDir(), "ledger.db"))
	seedLedgerPrincipal(t, db)
	l := &SQLLedger{DB: db.DB}
	a := ledgerBinding()
	b := a
	b.SessionID = ledgerOtherSID
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, binding := range []Binding{a, b} {
		wg.Add(1)
		go func(b Binding) { defer wg.Done(); errs <- l.Commit(context.Background(), b) }(binding)
	}
	wg.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent first use did not enforce one immutable session")
	}
}

func TestPreviewLedgerRequiresExistingCommitBeforeDeferredApproval(t *testing.T) {
	db := ledgerDB(t, filepath.Join(t.TempDir(), "ledger.db"))
	seedLedgerPrincipal(t, db)
	l := &SQLLedger{DB: db.DB}
	ctx := context.Background()
	before := time.Now().UnixMilli() - 1
	if l.CommittedBefore(ctx, ledgerAgent, time.Now().UnixMilli()) == nil {
		t.Fatal("historical approval created enrollment authority")
	}
	if l.Commit(ctx, ledgerBinding()) != nil {
		t.Fatal("initial commitment refused")
	}
	if l.CommittedBefore(ctx, ledgerAgent, before) == nil {
		t.Fatal("later commitment authorized earlier approval")
	}
	if l.CommittedBefore(ctx, ledgerAgent, time.Now().UnixMilli()) != nil {
		t.Fatal("prior commitment refused")
	}
}

func TestPreviewPrincipalStrictOwnerEnrollmentAndRevocation(t *testing.T) {
	for _, statement := range []string{
		`UPDATE users SET status='blocked' WHERE id='` + ledgerOwner + `'`,
		`DELETE FROM users WHERE id='` + ledgerOwner + `'`,
		`UPDATE agents SET kind='identity'`,
		`UPDATE agents SET disabled=1`,
		`UPDATE agents SET token_hash=''`,
		`UPDATE agents SET enroll_code='pending'`,
	} {
		t.Run(statement[:6]+statement[7:12], func(t *testing.T) {
			db := ledgerDB(t, filepath.Join(t.TempDir(), "ledger.db"))
			seedLedgerPrincipal(t, db)
			if _, err := CurrentPrincipal(context.Background(), db.DB, ledgerAgent); err != nil {
				t.Fatal("valid principal refused")
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatal("test state change failed")
			}
			if _, err := CurrentPrincipal(context.Background(), db.DB, ledgerAgent); err == nil {
				t.Fatal("untrusted principal accepted")
			}
			if (&SQLLedger{DB: db.DB}).Commit(context.Background(), ledgerBinding()) == nil {
				t.Fatal("untrusted principal committed")
			}
		})
	}
}

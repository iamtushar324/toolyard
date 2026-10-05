package federation

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func TestLocalConnectionIsolationRecoveryAndParentRevocation(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, parent, err := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	b := LocalBinding{EnvironmentID: "mac-A", LocalUserID: "local-owner", InstanceID: s.InstanceID}
	c, err := s.ConnectLocal(ctx, key, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentID == parent.ID || c.InstanceID != s.InstanceID || c.UserID != u.ID {
		t.Fatal("binding mismatch")
	}
	// Recover the same token after process state loss.
	restarted := &Service{db: s.db, identity: s.identity, cipher: s.cipher, InstanceID: s.InstanceID, now: s.now}
	recovered, err := restarted.ConnectLocal(ctx, key, b)
	if err != nil || recovered.Token != c.Token || recovered.AgentID != c.AgentID {
		t.Fatal("lost response", err)
	}
	b.LocalUserID = "second-owner"
	if _, err = s.ConnectLocal(ctx, key, b); !errors.Is(err, ErrConflict) {
		t.Fatal("same key shared users", err)
	}
	b.EnvironmentID = "mac-B"
	second, err := s.ConnectLocal(ctx, key, b)
	if err != nil || second.AgentID == c.AgentID {
		t.Fatal("environment collision", err)
	}
	other, err := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "other", Email: "other@example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, err := s.identity.CreateAgentWithToken(ctx, other.ID, "Other key")
	if err != nil {
		t.Fatal(err)
	}
	b.EnvironmentID = "mac-A"
	b.LocalUserID = "local-owner"
	if _, err = s.ConnectLocal(ctx, otherKey, b); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong owner", err)
	}
	b.LocalUserID = "other-owner"
	if _, err = s.ConnectLocal(ctx, otherKey, b); err != nil {
		t.Fatal("second user failed", err)
	}
	if _, err = s.VerifyAgent(ctx, c.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.identity.RotateAgentToken(ctx, u.ID, parent.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
		t.Fatal("parent rotation left derived access")
	}
	if _, err = s.RenewLocal(ctx, c.Token, c.Version); !errors.Is(err, ErrRevoked) {
		t.Fatal("revoked renewed", err)
	}
	if _, _, err = s.LocalHandoff(ctx, c.Token); err == nil {
		t.Fatal("revoked handoff")
	}
}
func TestLocalConnectionRenewConcurrencyAndHandoff(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
	b := LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID}
	c, err := s.ConnectLocal(ctx, key, b)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
		t.Fatal("expired credential passed ingress")
	}
	var wg sync.WaitGroup
	results := make(chan *Credential, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); next, e := s.RenewLocal(ctx, c.Token, c.Version); results <- next; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var next *Credential
	for r := range results {
		if next != nil && next.Token != r.Token {
			t.Fatal("renew duplicate")
		}
		next = r
	}
	if next.Version != 2 || next.AgentID != c.AgentID || next.Generation != c.Generation || next.Token != c.Token {
		t.Fatal("identity changed")
	}
	if _, err = s.RenewLocal(ctx, next.Token, 99); !errors.Is(err, ErrConflict) {
		t.Fatal("future version", err)
	}
	code, _, err := s.LocalHandoff(ctx, next.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got, e := s.ConsumeLocalHandoff(ctx, code); e != nil || got != u.ID {
		t.Fatal("handoff", e)
	}
	if _, err = s.ConsumeLocalHandoff(ctx, code); err == nil {
		t.Fatal("handoff reused")
	}
	code, _, err = s.LocalHandoff(ctx, next.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeLocal(ctx, next.Token); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeLocal(ctx, next.Token); err != nil {
		t.Fatal("revoke retry", err)
	}
	if _, err = s.ConsumeLocalHandoff(ctx, code); err == nil {
		t.Fatal("revoked pending handoff")
	}
	if _, err = s.ConnectLocal(ctx, key, b); !errors.Is(err, ErrRevoked) {
		t.Fatal("revoked silently recreated", err)
	}
}
func TestLocalConnectionMissingOwnerAndCredentialChains(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	ghost, _, _ := s.identity.CreateAgentWithToken(ctx, "missing-owner", "Ghost")
	b := LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID}
	if _, err := s.ConnectLocal(ctx, ghost, b); err == nil {
		t.Fatal("missing real owner permitted")
	}
	key, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
	c, err := s.ConnectLocal(ctx, key, b)
	if err != nil {
		t.Fatal(err)
	}
	b.EnvironmentID = "new-mac"
	if _, err = s.ConnectLocal(ctx, c.Token, b); err == nil {
		t.Fatal("derived credential chain")
	}
}

func TestLocalConnectionDisabledOwnerAndExternalRotationStayRevoked(t *testing.T) {
	for _, reason := range []string{"owner_disabled", "derived_rotated"} {
		t.Run(reason, func(t *testing.T) {
			s, u, _ := federationFixture(t)
			ctx := t.Context()
			key, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
			c, err := s.ConnectLocal(ctx, key, LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID})
			if err != nil {
				t.Fatal(err)
			}
			if reason == "owner_disabled" {
				_, err = s.db.Exec(`UPDATE users SET status='blocked' WHERE id=?`, u.ID)
			} else {
				_, err = s.identity.RotateAgentToken(ctx, u.ID, c.AgentID, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
				t.Fatal("invalid connection accepted")
			}
			if reason == "owner_disabled" {
				_, err = s.db.Exec(`UPDATE users SET status='active' WHERE id=?`, u.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
				t.Fatal("invalid connection automatically resumed")
			}
			var state string
			s.db.QueryRow(`SELECT status FROM local_connections WHERE agent_id=?`, c.AgentID).Scan(&state)
			if state != "revoked" {
				t.Fatal("missing revocation tombstone")
			}
		})
	}
}

func TestLocalConnectionExplicitReconnectKeepsIdentityAndRetiresCallbacks(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
	b := LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID}
	c, err := s.ConnectLocal(ctx, key, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatal("first generation")
	}
	_, err = s.db.Exec(`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES('old-hook',?,'mac','session','','secret',0,'pull')`, c.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('old-event','req',1,'old-hook',1,'{}',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeLocal(ctx, c.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConnectLocal(ctx, key, b); !errors.Is(err, ErrRevoked) {
		t.Fatal("automatic tombstone revival", err)
	}
	b.Reconnect = true
	next, err := s.ConnectLocal(ctx, key, b)
	if err != nil {
		t.Fatal("manual reconnect", err)
	}
	if next.AgentID != c.AgentID || next.Generation != 2 || next.Version != 2 || next.Token == c.Token {
		t.Fatal("reconnect identity/generation")
	}
	if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
		t.Fatal("prior credential accepted")
	}
	if _, err = s.VerifyAgent(ctx, next.Token); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ConnectLocal(ctx, key, b)
	if err != nil || retry.Token != next.Token || retry.Generation != next.Generation {
		t.Fatal("manual response recovery", err)
	}
	var receiver, event string
	s.db.QueryRow(`SELECT status FROM callback_receivers WHERE id='old-hook'`).Scan(&receiver)
	s.db.QueryRow(`SELECT status FROM callback_outbox WHERE id='old-event'`).Scan(&event)
	if receiver != "removed" || event != "failed" {
		t.Fatal("prior callbacks revived")
	}
	other, err := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "other", Email: "other@example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, _ := s.identity.CreateAgentWithToken(ctx, other.ID, "other")
	if _, err = s.ConnectLocal(ctx, otherKey, b); !errors.Is(err, ErrConflict) {
		t.Fatal("manual changedowner", err)
	}
	newKey, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "replacement")
	changed, err := s.ConnectLocal(ctx, newKey, b)
	if err != nil || changed.Generation != 3 || changed.AgentID != c.AgentID {
		t.Fatal("explicit newkey replace", err)
	}
}

func TestLocalConnectionLostRenewResponseAfterGraceAndRestart(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, _, _ := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
	c, err := s.ConnectLocal(ctx, key, LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	initial := s.now()
	renewTime := initial.Add(29*24*time.Hour + time.Hour)
	s.now = func() time.Time { return renewTime }
	// Commit upstream renewal but pretend its response never reached T3.
	committed, err := s.RenewLocal(ctx, c.Token, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Version != c.Version+1 || committed.Token != c.Token {
		t.Fatal("renew rotated bearer")
	}
	restartTime := renewTime.Add(identity.DefaultRotateGrace + time.Hour)
	restarted := &Service{db: s.db, identity: s.identity, cipher: s.cipher, InstanceID: s.InstanceID, now: func() time.Time { return restartTime }}
	recovered, err := restarted.RenewLocal(ctx, c.Token, c.Version)
	if err != nil {
		t.Fatal("lost response unavailable after grace", err)
	}
	if recovered.Token != c.Token || recovered.Version != committed.Version || recovered.ExpiresAt != committed.ExpiresAt || recovered.Generation != c.Generation {
		t.Fatal("recovery created additional credential")
	}
	if _, err = restarted.VerifyAgent(ctx, c.Token); err != nil {
		t.Fatal("persisted old credential invalid", err)
	}
	_, err = s.db.Exec(`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES('renew-hook',?,'mac','session','','secret',0,'pull')`, c.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('renew-event','req',1,'renew-hook',1,'{}',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.RevokeLocal(ctx, c.Token); err != nil {
		t.Fatal("old persisted credential could not revoke after renew", err)
	}
	var receiver, event string
	s.db.QueryRow(`SELECT status FROM callback_receivers WHERE id='renew-hook'`).Scan(&receiver)
	s.db.QueryRow(`SELECT status FROM callback_outbox WHERE id='renew-event'`).Scan(&event)
	if receiver != "removed" || event != "failed" {
		t.Fatal("renew cleanup missing")
	}
	if _, err = restarted.VerifyAgent(ctx, committed.Token); err == nil {
		t.Fatal("renewed credential survived revoke")
	}
}

func TestLocalConnectionIdentityRevocationWhileOffline(t *testing.T) {
	for _, action := range []string{"disable_parent", "disable_derived", "block_owner"} {
		t.Run(action, func(t *testing.T) {
			s, _, _ := federationFixture(t)
			ctx := t.Context()
			u, err := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "member", Email: "member@example.test"}, "")
			if err != nil {
				t.Fatal(err)
			}
			key, parent, err := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
			if err != nil {
				t.Fatal(err)
			}
			b := LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID}
			c, err := s.ConnectLocal(ctx, key, b)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.Exec(`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES('offline-hook',?,'mac','session','','secret',0,'pull')`, c.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('offline-event','req',1,'offline-hook',1,'{}',0)`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.Exec(`INSERT INTO inbox_grants(id,request_id,tool_index,agent_id,tool,params,max_uses,uses,status,token_hash,created_at,expires_at) VALUES('offline-grant','req',0,?,'fixture.echo','{}',1,0,'active','hash',0,9999999999999)`, c.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			// No authentication, polling or callback code runs between these mutations.
			switch action {
			case "disable_parent":
				if err = s.identity.SetAgentDisabled(ctx, u.ID, parent.ID, true); err != nil {
					t.Fatal(err)
				}
				if err = s.identity.SetAgentDisabled(ctx, u.ID, parent.ID, false); err != nil {
					t.Fatal(err)
				}
			case "disable_derived":
				if err = s.identity.SetAgentDisabled(ctx, u.ID, c.AgentID, true); err != nil {
					t.Fatal(err)
				}
				if err = s.identity.SetAgentDisabled(ctx, u.ID, c.AgentID, false); err != nil {
					t.Fatal(err)
				}
			case "block_owner":
				if err = s.identity.SetStatus(ctx, u.ID, identity.StatusBlocked, "offline test"); err != nil {
					t.Fatal(err)
				}
				if err = s.identity.SetStatus(ctx, u.ID, identity.StatusActive, ""); err != nil {
					t.Fatal(err)
				}
			}
			var connection, grant, receiver, event string
			s.db.QueryRow(`SELECT status FROM local_connections WHERE agent_id=?`, c.AgentID).Scan(&connection)
			s.db.QueryRow(`SELECT status FROM inbox_grants WHERE id='offline-grant'`).Scan(&grant)
			s.db.QueryRow(`SELECT status FROM callback_receivers WHERE id='offline-hook'`).Scan(&receiver)
			s.db.QueryRow(`SELECT status FROM callback_outbox WHERE id='offline-event'`).Scan(&event)
			if connection != "revoked" || grant != "revoked" || receiver != "removed" || event != "failed" {
				t.Fatalf("offline revocation did not commit: %s %s %s %s", connection, grant, receiver, event)
			}
			if _, err = s.VerifyAgent(ctx, c.Token); err == nil {
				t.Fatal("old derived credential revived")
			}
			if _, err = s.RenewLocal(ctx, c.Token, c.Version); !errors.Is(err, ErrRevoked) {
				t.Fatal("old derived credential renewed", err)
			}
			if _, err = s.ConnectLocal(ctx, key, b); !errors.Is(err, ErrRevoked) {
				t.Fatal("silent bootstrap revived", err)
			}
			b.Reconnect = true
			next, err := s.ConnectLocal(ctx, key, b)
			if err != nil || next.AgentID != c.AgentID || next.Generation != c.Generation+1 {
				t.Fatal("explicit reconnect failed", err)
			}
		})
	}
}

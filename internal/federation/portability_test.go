package federation

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// TestConnectRotationGraceColumns covers the CASE expressions Connect writes
// into agents: a renewal of an active connection keeps the old token for the
// rotate grace (prev_token_expires is a BIGINT taken from a parameter), and a
// reconnect after an environment restore keeps nothing.
func TestConnectRotationGraceColumns(t *testing.T) {
	s, admin, key := federationFixture(t)
	ctx := t.Context()
	p := principal(t, s, key, "stage", "alice")
	c, e := s.Connect(ctx, p, 0)
	if e != nil {
		t.Fatal(e)
	}
	var oldHash string
	if e = s.db.QueryRowContext(ctx, `SELECT token_hash FROM agents WHERE id=?`, c.AgentID).Scan(&oldHash); e != nil {
		t.Fatal(e)
	}

	renewAt := time.Now().Add(29*24*time.Hour + time.Hour)
	s.now = func() time.Time { return renewAt }
	next, e := s.Connect(ctx, p, c.Version)
	if e != nil {
		t.Fatal(e)
	}
	if next.Token == c.Token || next.Version != c.Version+1 {
		t.Fatalf("renewal did not rotate: version %d -> %d", c.Version, next.Version)
	}
	var prevHash sql.NullString
	var prevExpires sql.NullInt64
	if e = s.db.QueryRowContext(ctx, `SELECT prev_token_hash,prev_token_expires FROM agents WHERE id=?`, c.AgentID).Scan(&prevHash, &prevExpires); e != nil {
		t.Fatal(e)
	}
	if !prevHash.Valid || prevHash.String != oldHash {
		t.Fatalf("prev_token_hash = %+v, want the old hash", prevHash)
	}
	if want := renewAt.Add(identity.DefaultRotateGrace).UnixMilli(); !prevExpires.Valid || prevExpires.Int64 != want {
		t.Fatalf("prev_token_expires = %+v, want %d", prevExpires, want)
	}

	s.now = time.Now
	ap := &Principal{Issuer: "stage", Subject: admin.ClerkUserID, Origin: p.Origin, User: admin}
	if e = s.Revoke(ctx, ap, "environment"); e != nil {
		t.Fatal(e)
	}
	var trust Trust
	if e = s.db.QueryRowContext(ctx, `SELECT issuer,public_key,kid,origin FROM federation_issuers WHERE issuer='stage'`).Scan(&trust.Issuer, &trust.PublicKey, &trust.Kid, &trust.Origin); e != nil {
		t.Fatal(e)
	}
	if e = s.Register(ctx, trust, admin.ID); e != nil {
		t.Fatal(e)
	}
	restored, e := s.Connect(ctx, p, 0)
	if e != nil || restored.AgentID != c.AgentID {
		t.Fatal("reconnect", e)
	}
	if e = s.db.QueryRowContext(ctx, `SELECT prev_token_hash,prev_token_expires FROM agents WHERE id=?`, c.AgentID).Scan(&prevHash, &prevExpires); e != nil {
		t.Fatal(e)
	}
	if prevHash.Valid || prevExpires.Valid {
		t.Fatalf("reconnect kept a grace token: %+v %+v", prevHash, prevExpires)
	}
}

// TestLocalValidAllowExpiredArgument checks both values of the allowExpired
// flag, which is passed to SQL as 0/1: an expired local credential is valid
// for renewal only, and checking it does not revoke it.
func TestLocalValidAllowExpiredArgument(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, _, err := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ConnectLocal(ctx, key, LocalBinding{EnvironmentID: "mac", LocalUserID: "owner", InstanceID: s.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.localValid(ctx, c.AgentID, false); err != nil {
		t.Fatal("fresh credential invalid", err)
	}
	expired := time.Now().Add(31 * 24 * time.Hour)
	s.now = func() time.Time { return expired }
	if err = s.localValid(ctx, c.AgentID, true); err != nil {
		t.Fatal("expired credential refused for renewal", err)
	}
	if err = s.localValid(ctx, c.AgentID, false); !errors.Is(err, ErrRevoked) {
		t.Fatal("expired credential accepted", err)
	}
	if err = s.localValid(ctx, c.AgentID, true); err != nil {
		t.Fatal("expiry check revoked the connection", err)
	}
}

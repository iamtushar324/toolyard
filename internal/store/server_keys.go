package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// LoadOrCreateServerKey returns the server_keys row of purpose (private
// and public key bytes), creating it with gen when there is none. Several
// processes that start at once on a new database all end with the same
// row: the insert does nothing on a conflict (server_keys.purpose is
// unique) and the row is then read back, so every replica signs with one
// key.
func LoadOrCreateServerKey(ctx context.Context, q Querier, purpose string, gen func() (priv, pub []byte, err error)) (priv, pub []byte, err error) {
	err = q.QueryRowContext(ctx, `SELECT private_key, public_key FROM server_keys WHERE purpose = ?`, purpose).Scan(&priv, &pub)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return priv, pub, err
	}
	newPriv, newPub, err := gen()
	if err != nil {
		return nil, nil, err
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	if _, err := q.ExecContext(ctx,
		`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?,?,?,?,?) ON CONFLICT (purpose) DO NOTHING`,
		"key_"+hex.EncodeToString(b[:]), purpose, newPriv, newPub, time.Now().UnixMilli()); err != nil {
		return nil, nil, err
	}
	err = q.QueryRowContext(ctx, `SELECT private_key, public_key FROM server_keys WHERE purpose = ?`, purpose).Scan(&priv, &pub)
	return priv, pub, err
}

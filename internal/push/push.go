// Package push delivers Web Push notifications via VAPID. v0.1 only.
package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const vapidKeyPurpose = "vapid"

type Service struct {
	db          *store.DB
	subject     string
	vapidPublic string
	vapidPriv   string
}

// New loads or generates a VAPID keypair stored in server_keys.
// `subject` is the mailto: contact required by the VAPID spec.
func New(ctx context.Context, db *store.DB, subject string) (*Service, error) {
	if subject == "" {
		subject = "mailto:admin@example.invalid"
	}
	pub, priv, err := loadOrCreateVapid(ctx, db)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, subject: subject, vapidPublic: pub, vapidPriv: priv}, nil
}

func (s *Service) PublicKey() string { return s.vapidPublic }

// Subscription is the browser-side PushSubscription payload.
type Subscription struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	UserAgent string `json:"user_agent"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Service) Subscribe(ctx context.Context, userID, endpoint, p256dh, auth, userAgent string) (*Subscription, error) {
	if endpoint == "" || p256dh == "" || auth == "" {
		return nil, errors.New("push subscription missing fields")
	}
	id := "sub_" + uuid.NewString()
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions(id, user_id, endpoint, p256dh, auth, user_agent, created_at)
         VALUES(?,?,?,?,?,?,?)
         ON CONFLICT(endpoint) DO UPDATE SET user_id=excluded.user_id,
            p256dh=excluded.p256dh, auth=excluded.auth, user_agent=excluded.user_agent`,
		id, userID, endpoint, p256dh, auth, userAgent, now); err != nil {
		return nil, err
	}
	return &Subscription{
		ID: id, UserID: userID, Endpoint: endpoint, P256dh: p256dh, Auth: auth,
		UserAgent: userAgent, CreatedAt: now,
	}, nil
}

func (s *Service) ListForUser(ctx context.Context, userID string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, endpoint, p256dh, auth, COALESCE(user_agent,''), created_at
         FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth,
			&sub.UserAgent, &sub.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Notify fans the payload out to all subscriptions for userID. Errors are
// logged but not returned; one bad subscription should not block the rest.
func (s *Service) Notify(ctx context.Context, userID string, payload any) error {
	if s == nil {
		return nil
	}
	subs, err := s.ListForUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		ws := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
		}
		resp, err := webpush.SendNotification(body, ws, &webpush.Options{
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.vapidPublic,
			VAPIDPrivateKey: s.vapidPriv,
			TTL:             60,
		})
		if err != nil {
			log.Printf("push: send to %s: %v", sub.Endpoint, err)
			continue
		}
		// 404/410 -> subscription is dead, drop it
		if resp != nil && (resp.StatusCode == 404 || resp.StatusCode == 410) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, sub.ID)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	return nil
}

// loadOrCreateVapid returns base64-url-safe public/private keys for VAPID.
// Stored as DER in server_keys (purpose='vapid').
func loadOrCreateVapid(ctx context.Context, db *store.DB) (string, string, error) {
	var pubB64, privB64 string
	row := db.QueryRowContext(ctx,
		`SELECT public_key, private_key FROM server_keys WHERE purpose = ?`, vapidKeyPurpose)
	var pub, priv []byte
	err := row.Scan(&pub, &priv)
	if err == nil {
		pubB64 = base64.RawURLEncoding.EncodeToString(pub)
		privB64 = base64.RawURLEncoding.EncodeToString(priv)
		return pubB64, privB64, nil
	}
	if err != sql.ErrNoRows {
		return "", "", err
	}

	// generate fresh P-256 keypair
	priv2, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	rawPriv := priv2.D.Bytes()
	// pad to 32 bytes
	if len(rawPriv) < 32 {
		buf := make([]byte, 32)
		copy(buf[32-len(rawPriv):], rawPriv)
		rawPriv = buf
	}
	rawPub := elliptic.Marshal(elliptic.P256(), priv2.PublicKey.X, priv2.PublicKey.Y) // 65 bytes uncompressed
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?,?,?,?,?)`,
		"key_"+uuid.NewString(), vapidKeyPurpose, rawPriv, rawPub, time.Now().UnixMilli()); err != nil {
		return "", "", err
	}
	_ = x509.MarshalECPrivateKey // silence unused-import linters; kept for future PEM export
	return base64.RawURLEncoding.EncodeToString(rawPub), base64.RawURLEncoding.EncodeToString(rawPriv), nil
}

// Notification is the JSON payload pushed to the browser. The dashboard's
// service worker shows a notification with these fields.
type Notification struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	Approval string `json:"approval_id,omitempty"`
	Tag      string `json:"tag,omitempty"`
}

func (n Notification) String() string { return fmt.Sprintf("%s — %s", n.Title, n.Body) }

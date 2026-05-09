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
	"io"
	"log"
	"strings"
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
// `subject` is the contact required by the VAPID spec — typically an
// email address or an https URL.
//
// SherClockHolmes/webpush-go v1.4.0 has a quirk: its
// getVAPIDAuthorizationHeader auto-prepends "mailto:" to anything that
// doesn't start with "https:". If we forward a pre-prefixed
// "mailto:foo@bar", the JWT ends up with "sub": "mailto:mailto:foo@bar"
// — which Apple's APNS rejects as BadJwtToken. Strip a leading
// "mailto:" so the library can add it cleanly.
func New(ctx context.Context, db *store.DB, subject string) (*Service, error) {
	if subject == "" {
		subject = "admin@example.invalid"
	}
	subject = strings.TrimPrefix(subject, "mailto:")
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

// MaxSubsPerUser caps how many push subscriptions a single user can have.
// Each subscription is one device + one browser-profile, so 16 leaves
// plenty of headroom (phone + tablet + 4 laptops + spares) without letting
// a runaway loop keep upserting fresh ones until Notify() crawls.
const MaxSubsPerUser = 16

func (s *Service) Subscribe(ctx context.Context, userID, endpoint, p256dh, auth, userAgent string) (*Subscription, error) {
	if endpoint == "" || p256dh == "" || auth == "" {
		return nil, errors.New("push subscription missing fields")
	}
	if len(endpoint) > 1024 || len(p256dh) > 256 || len(auth) > 256 {
		return nil, errors.New("push subscription field too long")
	}
	// Reject if the user is at the cap and this endpoint is new — UPSERT
	// of an existing endpoint stays allowed (re-subscribe path).
	var existing int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, userID).
		Scan(&existing); err == nil && existing >= MaxSubsPerUser {
		var matches int
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`,
			userID, endpoint).Scan(&matches)
		if matches == 0 {
			return nil, fmt.Errorf("user has reached the push subscription cap (%d); revoke an old device first", MaxSubsPerUser)
		}
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

// SendResult captures per-subscription push delivery outcome. Used by
// /v1/push/test so the operator can see why pushes are silently dropped.
type SendResult struct {
	SubscriptionID string `json:"subscription_id"`
	Endpoint       string `json:"endpoint"`
	Status         int    `json:"status"`           // HTTP status from the push service (0 if no response)
	Error          string `json:"error,omitempty"`  // transport/encryption error
	Pruned         bool   `json:"pruned,omitempty"` // 404/410: row was deleted
	UserAgent      string `json:"user_agent,omitempty"`
}

// Notify fans the payload out to all subscriptions for userID. Errors are
// logged but not returned; one bad subscription should not block the rest.
func (s *Service) Notify(ctx context.Context, userID string, payload any) error {
	_, err := s.NotifyDetailed(ctx, userID, payload)
	return err
}

// NotifyDetailed is the diagnostic-friendly variant: it returns per-sub
// delivery results so the dashboard's "test push" button can surface
// what's actually happening (Apple subject rejection, dead VAPID, 410
// Gone, etc.). Behaviour is otherwise identical to Notify.
func (s *Service) NotifyDetailed(ctx context.Context, userID string, payload any) ([]SendResult, error) {
	if s == nil {
		return nil, nil
	}
	subs, err := s.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out := make([]SendResult, 0, len(subs))
	for _, sub := range subs {
		ws := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
		}
		res := SendResult{SubscriptionID: sub.ID, Endpoint: sub.Endpoint, UserAgent: sub.UserAgent}
		resp, err := webpush.SendNotification(body, ws, &webpush.Options{
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.vapidPublic,
			VAPIDPrivateKey: s.vapidPriv,
			TTL:             60,
		})
		if err != nil {
			res.Error = err.Error()
			log.Printf("push: send to %s: %v", sub.Endpoint, err)
			out = append(out, res)
			continue
		}
		if resp != nil {
			res.Status = resp.StatusCode
			if resp.StatusCode == 404 || resp.StatusCode == 410 {
				_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, sub.ID)
				res.Pruned = true
			} else if resp.StatusCode/100 != 2 {
				// Capture Apple/FCM's error body — that's where they
				// say *why* (BadJwtToken, BadVAPIDPublicKey, MissingChannel,
				// etc.). Without this the 403 is opaque.
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				bodyStr := strings.TrimSpace(string(body))
				if bodyStr != "" {
					res.Error = fmt.Sprintf("%s: %s", resp.Status, bodyStr)
				} else {
					res.Error = resp.Status
				}
				log.Printf("push: %s returned %s — %s", sub.Endpoint, resp.Status, bodyStr)
			}
			_ = resp.Body.Close()
		}
		out = append(out, res)
	}
	return out, nil
}

// RotateKeys generates a fresh VAPID keypair using webpush-go's own
// generator (guaranteed-correct format), replaces the stored row, wipes
// in-memory state, and deletes every push_subscription (they were bound
// to the OLD public key — Apple/FCM would 403 forever otherwise).
// Returns the new public key.
func (s *Service) RotateKeys(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("push service nil")
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", fmt.Errorf("generate vapid: %w", err)
	}
	// webpush.GenerateVAPIDKeys returns base64-encoded strings. Decode
	// before storing so we keep the existing schema (server_keys row
	// stores raw bytes), then re-encode on read.
	privBytes, err := base64.RawURLEncoding.DecodeString(priv)
	if err != nil {
		return "", err
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(pub)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_keys WHERE purpose = ?`, vapidKeyPurpose); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?,?,?,?,?)`,
		"key_"+uuid.NewString(), vapidKeyPurpose, privBytes, pubBytes, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions`); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.vapidPublic = pub
	s.vapidPriv = priv
	return pub, nil
}

// JWTPreview builds a VAPID JWT exactly the way SendNotification would
// (same private key, same subject, same audience derivation) and
// returns its decoded claims for diagnostic display in the dashboard.
// We never expose the signing material — only the public claims and a
// fingerprint of the public key.
func (s *Service) JWTPreview(ctx context.Context, userID string) ([]map[string]any, error) {
	if s == nil {
		return nil, errors.New("push service nil")
	}
	subs, err := s.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(subs))
	privBytes, err := base64.RawURLEncoding.DecodeString(s.vapidPriv)
	if err != nil {
		return nil, fmt.Errorf("decode priv: %w", err)
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(s.vapidPublic)
	if err != nil {
		return nil, fmt.Errorf("decode pub: %w", err)
	}
	for _, sub := range subs {
		entry := map[string]any{
			"endpoint":         sub.Endpoint,
			"user_agent":       sub.UserAgent,
			"vapid_priv_len":   len(privBytes),
			"vapid_pub_len":    len(pubBytes),
			"vapid_pub_prefix": fmt.Sprintf("%x", firstN(pubBytes, 1)),
			"subject":          s.Subject(), // what actually lands in the JWT sub claim
		}
		// audience is the scheme://host of the push endpoint.
		host := sub.Endpoint
		if i := strings.Index(host, "://"); i >= 0 {
			rest := host[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rest = rest[:j]
			}
			entry["aud"] = host[:i+3] + rest
		}
		entry["exp_seconds_from_now"] = int(12 * 60 * 60)
		// Rederive the public key from the private to verify they match
		// (this is exactly what webpush-go does under the hood).
		curve := elliptic.P256()
		px, py := curve.ScalarMult(curve.Params().Gx, curve.Params().Gy, privBytes)
		derivedPub := elliptic.Marshal(curve, px, py)
		entry["derived_pub_matches_stored"] = bytesEqual(derivedPub, pubBytes)
		entry["derived_pub_b64"] = base64.RawURLEncoding.EncodeToString(derivedPub)
		entry["stored_pub_b64"] = s.vapidPublic
		out = append(out, entry)
	}
	return out, nil
}

func firstN(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DeleteAllForUser drops every push_subscription row owned by a user.
// Returns the count removed. Used by the dashboard's "Wipe & re-enroll"
// button when existing subscriptions are bound to a stale VAPID key
// (Apple/Google reject all sends with 401 in that case and the only
// recovery is to ask the browser for a fresh subscription).
func (s *Service) DeleteAllForUser(ctx context.Context, userID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Subject returns the VAPID subject as it appears in the signed JWT
// (i.e. with the "mailto:" prefix that webpush-go re-adds for non-https
// values). Surfaced by the dashboard's Diagnostics panel so the
// operator can verify what Apple actually receives, not what we store.
func (s *Service) Subject() string {
	if s == nil {
		return ""
	}
	if strings.HasPrefix(s.subject, "https:") {
		return s.subject
	}
	return "mailto:" + s.subject
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

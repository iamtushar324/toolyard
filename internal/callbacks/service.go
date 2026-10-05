// Package callbacks delivers committed Inbox decisions. Delivery is at least
// once; receivers deduplicate the stable event id before any agent dispatch.
package callbacks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var ErrReceiver = errors.New("callback receiver is unavailable or belongs to another agent")
var ErrConflict = errors.New("callback receiver revision or registration changed")

type Receiver struct {
	ID               string `json:"callback_ref"`
	AgentID          string `json:"-"`
	EnvironmentID    string `json:"environment_id"`
	ClientReceiverID string `json:"client_receiver_id"`
	Destination      string `json:"destination"`
	Transport        string `json:"transport"`
	Revision         int    `json:"revision"`
	Status           string `json:"status"`
	PrivateAllowed   bool   `json:"-"`
	secret           string
}

type Service struct {
	db     *store.DB
	cipher *sealbox.Cipher
	now    func() time.Time
	mu     sync.Mutex
	// Resolver is injectable for deterministic DNS rebinding tests.
	Resolver func(context.Context, string) ([]net.IPAddr, error)
}

func New(db *store.DB, cipher *sealbox.Cipher) *Service {
	return &Service{db: db, cipher: cipher, now: time.Now, Resolver: net.DefaultResolver.LookupIPAddr}
}

func secretBytes(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("secret must use Standard Webhooks whsec_ encoding")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(b) < 24 || len(b) > 64 {
		return nil, errors.New("secret must contain 24 to 64 random bytes")
	}
	return b, nil
}

func (s *Service) Register(ctx context.Context, agentID, environmentID, clientID, destination, secret string, privateAllowed bool) (*Receiver, error) {
	return s.register(ctx, agentID, environmentID, clientID, destination, secret, privateAllowed, "push")
}
func (s *Service) RegisterPull(ctx context.Context, agentID, environmentID, clientID, secret string) (*Receiver, error) {
	if environmentID == "" {
		return nil, ErrReceiver
	}
	return s.register(ctx, agentID, environmentID, clientID, "", secret, false, "pull")
}
func (s *Service) register(ctx context.Context, agentID, environmentID, clientID, destination, secret string, privateAllowed bool, transport string) (*Receiver, error) {
	if agentID == "" || clientID == "" || len(clientID) > 120 {
		return nil, ErrReceiver
	}
	if _, err := secretBytes(secret); err != nil {
		return nil, err
	}
	if transport == "push" {
		if _, err := s.resolve(ctx, destination, privateAllowed); err != nil {
			return nil, err
		}
	}
	// Retries of identical registration return the same opaque reference.
	var previousID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM callback_receivers WHERE agent_id=? AND client_receiver_id=?`, agentID, clientID).Scan(&previousID)
	if err == nil {
		r, err := s.Get(ctx, agentID, previousID)
		if err != nil {
			return nil, err
		}
		plain, err := s.cipher.Open(r.secret, []byte("callback|"+r.ID))
		if err != nil {
			return nil, err
		}
		if r.Transport != transport || r.Destination != destination || r.EnvironmentID != environmentID || !hmac.Equal(plain, []byte(secret)) || r.Status != "active" {
			return nil, ErrConflict
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	id := "cb_" + uuid.NewString()
	enc, err := s.cipher.Seal([]byte(secret), []byte("callback|"+id))
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,private_allowed,created_at,transport) VALUES(?,?,?,?,?,?,?,?,?)`, id, agentID, environmentID, clientID, destination, enc, privateAllowed, s.now().UnixMilli(), transport)
	if err != nil {
		// A concurrent identical registration may own the unique client binding.
		var won string
		if e := s.db.QueryRowContext(ctx, `SELECT id FROM callback_receivers WHERE agent_id=? AND client_receiver_id=?`, agentID, clientID).Scan(&won); e == nil {
			return s.register(ctx, agentID, environmentID, clientID, destination, secret, privateAllowed, transport)
		}
		return nil, err
	}
	return s.Get(ctx, agentID, id)
}

func scanReceiver(row interface{ Scan(...any) error }) (*Receiver, error) {
	r := new(Receiver)
	err := row.Scan(&r.ID, &r.AgentID, &r.EnvironmentID, &r.ClientReceiverID, &r.Destination, &r.secret, &r.Revision, &r.Status, &r.PrivateAllowed, &r.Transport)
	return r, err
}

const receiverColumns = `id,agent_id,environment_id,client_receiver_id,destination,secret_enc,revision,status,private_allowed,transport`

func (s *Service) Get(ctx context.Context, agentID, id string) (*Receiver, error) {
	r, err := scanReceiver(s.db.QueryRowContext(ctx, `SELECT `+receiverColumns+` FROM callback_receivers WHERE id=? AND agent_id=?`, id, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReceiver
	}
	return r, err
}

func (s *Service) Change(ctx context.Context, agentID, id, action, secret string, revision int) (*Receiver, error) {
	r, err := s.Get(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	if r.Revision == revision+1 {
		if (action == "disable" && r.Status == "disabled") || (action == "remove" && r.Status == "removed") {
			return r, nil
		}
		if action == "rotate" && r.Status == "active" {
			plain, e := s.cipher.Open(r.secret, []byte("callback|"+id))
			if e == nil && hmac.Equal(plain, []byte(secret)) {
				return r, nil
			}
		}
	}
	if r.Revision != revision {
		return nil, ErrConflict
	}
	enc := r.secret
	status := r.Status
	switch action {
	case "disable":
		status = "disabled"
	case "remove":
		status = "removed"
	case "rotate":
		if status != "active" {
			return nil, ErrReceiver
		}
		if _, err = secretBytes(secret); err != nil {
			return nil, err
		}
		enc, err = s.cipher.Seal([]byte(secret), []byte("callback|"+id))
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("action must be disable, rotate or remove")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE callback_receivers SET status=?,secret_enc=?,revision=revision+1 WHERE id=? AND agent_id=? AND revision=? AND status<>'removed'`, status, enc, id, agentID, revision)
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		tx.Rollback()
		return s.Change(ctx, agentID, id, action, secret, revision)
	}
	if action == "rotate" {
		// Key rotation preserves the fixed destination. Pending subscriptions
		// bind to its new key revision, never to an altered URL.
		_, err = tx.ExecContext(ctx, `UPDATE callback_subscriptions SET receiver_revision=? WHERE receiver_id=?`, revision+1, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET receiver_revision=? WHERE receiver_id=? AND status IN ('pending','delivering')`, revision+1, id)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason=? WHERE receiver_id=? AND status IN ('pending','delivering')`, "receiver_"+status, id)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, agentID, id)
}

// RegisterTx participates in Inbox's insert transaction. No database query
// may use the pool while this transaction holds its one SQLite connection.
func (s *Service) RegisterTx(ctx context.Context, tx *sql.Tx, r *inbox.Request, ref string) error {
	if ref == "" {
		return nil
	}
	var revision int
	err := tx.QueryRowContext(ctx, `SELECT revision FROM callback_receivers WHERE id=? AND agent_id=? AND status='active'`, ref, r.AgentID).Scan(&revision)
	if err != nil {
		return ErrReceiver
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES(?,?,?)`, r.ID, ref, revision)
	return err
}

type Outcome struct {
	CallID  string `json:"call_id"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}
type EventData struct {
	InboxID          string                `json:"inbox_id"`
	DecisionRevision int                   `json:"decision_revision"`
	Status           string                `json:"status"`
	Calls            []Outcome             `json:"calls"`
	OverallNote      string                `json:"overall_note,omitempty"`
	RequestExpiresAt int64                 `json:"request_expires_at"`
	GrantExpiresAt   int64                 `json:"grant_expires_at,omitempty"`
	StatusRef        string                `json:"status_ref"`
	Response         *inbox.AnswerResponse `json:"response,omitempty"`
}
type Event struct {
	Type      string    `json:"type"`
	EventID   string    `json:"event_id"`
	Timestamp string    `json:"timestamp"`
	Data      EventData `json:"data"`
}

// DecisionTx writes one logical event with all outcomes, in the same
// transaction as verdicts, grants and audit. Drafts never call this hook.
func (s *Service) DecisionTx(ctx context.Context, tx *sql.Tx, r *inbox.Request) error {
	var receiver string
	var revision int
	err := tx.QueryRowContext(ctx, `SELECT receiver_id,receiver_revision FROM callback_subscriptions WHERE request_id=?`, r.ID).Scan(&receiver, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	kind, status := "inbox.decision", "decided"
	if r.Status == inbox.StatusExpired {
		kind, status = "inbox.expired", "expired"
	}
	if r.Status == inbox.StatusCancelled {
		kind, status = "inbox.cancelled", "cancelled"
	}
	outcomes := make([]Outcome, 0, len(r.Tools))
	for _, t := range r.Tools {
		verdict := "rejected"
		if t.Decision == inbox.ToolAllowed {
			verdict = "accepted"
		}
		outcomes = append(outcomes, Outcome{CallID: t.CallID, Verdict: verdict, Reason: t.Reason})
	}
	id := "evt_" + uuid.NewString()
	event := Event{Type: kind, EventID: id, Timestamp: s.now().UTC().Format(time.RFC3339Nano), Data: EventData{InboxID: r.ID, DecisionRevision: r.Revision, Status: status, Calls: outcomes, OverallNote: r.OwnerNote, RequestExpiresAt: r.ExpiresAt, GrantExpiresAt: r.GrantsExpire, StatusRef: r.ID, Response: r.Response}}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES(?,?,?,?,?,?,?)`, id, r.ID, r.Revision, receiver, revision, string(payload), s.now().UnixMilli())
	return err
}

func privateIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || !ip.IsGlobalUnicast()
}
func (s *Service) resolve(ctx context.Context, raw string, allowPrivate bool) ([]net.IPAddr, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("destination must be an HTTPS URL without credentials or fragment")
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("destination must use HTTPS port 443")
	}
	ips, err := s.Resolver(ctx, u.Hostname())
	if err != nil || len(ips) == 0 {
		return nil, errors.New("destination DNS resolution failed")
	}
	for _, a := range ips {
		if privateIP(a.IP) && !allowPrivate {
			return nil, errors.New("private destinations require administrator registration")
		}
	}
	return ips, nil
}

type Delivery struct {
	History        []Attempt     `json:"history"`
	ID             string        `json:"event_id"`
	RequestID      string        `json:"inbox_id"`
	Status         string        `json:"status"`
	Attempts       int           `json:"attempts"`
	TerminalReason string        `json:"terminal_reason,omitempty"`
	DeliveredAt    sql.NullInt64 `json:"-"`
}
type Attempt struct {
	Attempt    int           `json:"attempt"`
	At         int64         `json:"at"`
	HTTPStatus sql.NullInt64 `json:"-"`
	StatusCode int           `json:"http_status,omitempty"`
	Outcome    string        `json:"outcome"`
}

func (s *Service) History(ctx context.Context, agentID, requestID string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.request_id,o.status,o.attempts,o.terminal_reason,o.delivered_at FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE r.agent_id=? AND o.request_id=?`, agentID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.RequestID, &d.Status, &d.Attempts, &d.TerminalReason, &d.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].History, err = s.attemptHistory(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Service) ReceiverHistory(ctx context.Context, agentID, ref string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,request_id,status,attempts,terminal_reason,delivered_at FROM callback_outbox WHERE receiver_id IN (SELECT id FROM callback_receivers WHERE id=? AND agent_id=?) ORDER BY next_attempt_at DESC LIMIT 100`, ref, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.RequestID, &d.Status, &d.Attempts, &d.TerminalReason, &d.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].History, err = s.attemptHistory(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Service) Retry(ctx context.Context, agentID, eventID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE callback_outbox SET status='pending',retry_base=attempts,next_attempt_at=?,terminal_reason='' WHERE id=? AND status='failed' AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id=? AND status='active')`, s.now().UnixMilli(), eventID, agentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrReceiver
	}
	return nil
}

var retryDelays = []time.Duration{5 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// Run resumes persistent delivery after restarts. Claims have leases, so a
// lost acknowledgement may cause a physical duplicate with the same event id.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.DeliverOne(ctx)
		}
	}
}

func (s *Service) DeliverOne(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixMilli()
	var id, payload, ref string
	var attempts, rev int
	err := s.db.QueryRowContext(ctx, `SELECT id,payload,receiver_id,attempts,receiver_revision FROM callback_outbox WHERE receiver_id IN (SELECT id FROM callback_receivers WHERE transport='push') AND ((status='pending' AND next_attempt_at<=?) OR (status='delivering' AND claim_until<=?)) ORDER BY next_attempt_at LIMIT 1`, now, now).Scan(&id, &payload, &ref, &attempts, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE callback_outbox SET status='delivering',claim_until=?,attempts=attempts+1 WHERE id=? AND ((status='pending' AND next_attempt_at<=?) OR (status='delivering' AND claim_until<=?))`, now+60000, id, now, now)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil
	}
	receiver, err := scanReceiver(s.db.QueryRowContext(ctx, `SELECT `+receiverColumns+` FROM callback_receivers WHERE id=?`, ref))
	if err != nil || receiver.Status != "active" {
		return s.finish(ctx, id, attempts+1, 0, "receiver_unavailable", true)
	}
	if receiver.Revision != rev {
		return s.finish(ctx, id, attempts+1, 0, "receiver_revision_changed", false)
	}
	// Revoked agents, users or environments never keep receiving decisions.
	var active int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM agents a JOIN users u ON u.id=a.owner_user WHERE a.id=? AND a.disabled=0 AND u.status='active'`, receiver.AgentID).Scan(&active)
	if err != nil || active != 1 {
		return s.finish(ctx, id, attempts+1, 0, "owner_or_agent_disabled", true)
	}
	var fedCount int
	if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM federation_connections WHERE agent_id=?`, receiver.AgentID).Scan(&fedCount); e != nil {
		return s.finish(ctx, id, attempts+1, 0, "connection_unavailable", false)
	}
	if fedCount > 0 {
		var credential, issuer, subject, tokenHash string
		e := s.db.QueryRowContext(ctx, `SELECT c.credential_enc,c.issuer,c.subject,a.token_hash FROM federation_connections c JOIN agents a ON a.id=c.agent_id WHERE c.agent_id=?`, receiver.AgentID).Scan(&credential, &issuer, &subject, &tokenHash)
		if e != nil {
			return s.finish(ctx, id, attempts+1, 0, "connection_unavailable", false)
		}
		plain, e := s.cipher.Open(credential, []byte("federation|"+issuer+"|"+subject))
		sum := sha256.Sum256(plain)
		if e != nil || hex.EncodeToString(sum[:]) != tokenHash {
			return s.finish(ctx, id, attempts+1, 0, "connection_revoked", true)
		}

		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM federation_connections c JOIN federation_issuers i ON i.issuer=c.issuer WHERE c.agent_id=? AND c.status='active' AND c.expires_at>? AND i.status='active'`, receiver.AgentID, s.now().UnixMilli()).Scan(&active)
		if err != nil || active != 1 {
			return s.finish(ctx, id, attempts+1, 0, "connection_revoked_or_expired", true)
		}
	}
	if receiver.EnvironmentID != "" {
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM federation_issuers WHERE issuer=? AND status='active'`, receiver.EnvironmentID).Scan(&active)
		if err != nil || active != 1 {
			return s.finish(ctx, id, attempts+1, 0, "environment_revoked", true)
		}
	}
	ips, err := s.resolve(ctx, receiver.Destination, receiver.PrivateAllowed)
	if err != nil {
		return s.finish(ctx, id, attempts+1, 0, "destination_invalid", true)
	}
	raw, err := s.cipher.Open(receiver.secret, []byte("callback|"+receiver.ID))
	if err != nil {
		return s.finish(ctx, id, attempts+1, 0, "secret_unavailable", true)
	}
	key, err := secretBytes(string(raw))
	if err != nil {
		return s.finish(ctx, id, attempts+1, 0, "secret_invalid", true)
	}
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "." + payload))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.Destination, bytes.NewBufferString(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", timestamp)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	// Pin the validated address while retaining TLS verification of hostname.
	// Resolve every attempt; never follow redirects to a different trust target.
	dialer := net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return s.finish(ctx, id, attempts+1, 0, "network_error", false)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	code := response.StatusCode
	if code >= 200 && code < 300 {
		return s.finish(ctx, id, attempts+1, code, "delivered", false)
	}
	return s.finish(ctx, id, attempts+1, code, fmt.Sprintf("http_%d", code), code == 410 || code >= 300 && code < 400)
}

func (s *Service) finish(ctx context.Context, id string, attempt, code int, outcome string, terminal bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_attempts(event_id,attempt,attempted_at,http_status,outcome) VALUES(?,?,?,?,?)`, id, attempt, now, code, outcome)
	if err != nil {
		return err
	}
	status, reason, next := "pending", "", now
	var base int
	if err = tx.QueryRowContext(ctx, `SELECT retry_base FROM callback_outbox WHERE id=?`, id).Scan(&base); err != nil {
		return err
	}
	var delivered any
	if outcome == "delivered" {
		status = "delivered"
		delivered = now
	} else if terminal || attempt-base > len(retryDelays) {
		status = "failed"
		reason = outcome
	} else {
		next += retryDelays[attempt-base-1].Milliseconds()
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status=?,terminal_reason=?,next_attempt_at=?,claim_until=0,delivered_at=? WHERE id=? AND status='delivering'`, status, reason, next, delivered, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) attemptHistory(ctx context.Context, eventID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempt,attempted_at,http_status,outcome FROM callback_attempts WHERE event_id=? ORDER BY attempt`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err = rows.Scan(&a.Attempt, &a.At, &a.HTTPStatus, &a.Outcome); err != nil {
			return nil, err
		}
		if a.HTTPStatus.Valid {
			a.StatusCode = int(a.HTTPStatus.Int64)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

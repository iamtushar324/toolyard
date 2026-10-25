// Package callbacks delivers committed Inbox decisions and the other
// webhook events (events.go): execution outcomes, legacy approval
// decisions and results, pings, and the events other packages emit with
// EmitTx. Delivery is at least once; receivers deduplicate the stable event
// id before any agent dispatch.
package callbacks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var ErrReceiver = errors.New("callback receiver is unavailable or belongs to another agent")
var ErrConflict = errors.New("callback receiver revision or registration changed")

// ErrTooManyReceivers refuses a registration beyond MaxReceiversPerAgent.
var ErrTooManyReceivers = fmt.Errorf("an agent may keep at most %d receivers; remove one first", MaxReceiversPerAgent)

// MaxReceiversPerAgent bounds the receivers an agent keeps (removed ones
// do not count).
const MaxReceiversPerAgent = 20

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
	// Events are the event types the receiver accepts ("*", exact names,
	// "prefix.*"); V3 opts it into the schema version 3 decision shape.
	Events []string `json:"events"`
	V3     bool     `json:"v3"`
	// Default is set by List: the agent's default receiver.
	Default bool `json:"default,omitempty"`
	secret  string
}

// Subscription is what a new receiver accepts. The zero value is the
// legacy receiver: inbox.decision, inbox.expired and inbox.cancelled in
// the version 2 shape.
type Subscription struct {
	Events []string
	V3     bool
}

type Service struct {
	db     *store.DB
	cipher *sealbox.Cipher
	now    func() time.Time
	// slots bounds the deliveries in flight in this process (delivery.go).
	slots chan struct{}
	// Resolver is injectable for deterministic DNS rebinding tests.
	Resolver func(context.Context, string) ([]net.IPAddr, error)
	// ackHook, when set (tests), runs inside Ack between its read and its
	// update.
	ackHook func()
}

func New(db *store.DB, cipher *sealbox.Cipher) *Service {
	return &Service{db: db, cipher: cipher, now: time.Now, Resolver: net.DefaultResolver.LookupIPAddr, slots: make(chan struct{}, deliveryWorkers)}
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

// Register registers a legacy push receiver (Subscription{}).
func (s *Service) Register(ctx context.Context, agentID, environmentID, clientID, destination, secret string, privateAllowed bool) (*Receiver, error) {
	return s.RegisterWith(ctx, agentID, environmentID, clientID, destination, secret, privateAllowed, Subscription{})
}

// RegisterWith registers a push receiver that accepts sub.
func (s *Service) RegisterWith(ctx context.Context, agentID, environmentID, clientID, destination, secret string, privateAllowed bool, sub Subscription) (*Receiver, error) {
	return s.register(ctx, agentID, environmentID, clientID, destination, secret, privateAllowed, "push", sub)
}

// RegisterPull registers a legacy pull receiver (Subscription{}).
func (s *Service) RegisterPull(ctx context.Context, agentID, environmentID, clientID, secret string) (*Receiver, error) {
	return s.RegisterPullWith(ctx, agentID, environmentID, clientID, secret, Subscription{})
}

// RegisterPullWith registers a pull receiver that accepts sub.
func (s *Service) RegisterPullWith(ctx context.Context, agentID, environmentID, clientID, secret string, sub Subscription) (*Receiver, error) {
	if environmentID == "" {
		return nil, ErrReceiver
	}
	return s.register(ctx, agentID, environmentID, clientID, "", secret, false, "pull", sub)
}
func (s *Service) register(ctx context.Context, agentID, environmentID, clientID, destination, secret string, privateAllowed bool, transport string, sub Subscription) (*Receiver, error) {
	if agentID == "" || clientID == "" || len(clientID) > 120 {
		return nil, ErrReceiver
	}
	if _, err := secretBytes(secret); err != nil {
		return nil, err
	}
	events, err := NormalizeEvents(sub.Events)
	if err != nil {
		return nil, err
	}
	if transport == "push" {
		if _, err := s.resolve(ctx, destination, privateAllowed); err != nil {
			return nil, err
		}
	}
	if err := hostConnectionActive(ctx, s.db, agentID, s.now().UnixMilli()); err != nil {
		return nil, err
	}
	// Retries of identical registration return the same opaque reference.
	var previousID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM callback_receivers WHERE agent_id=? AND client_receiver_id=?`, agentID, clientID).Scan(&previousID)
	if err == nil {
		r, err := s.Get(ctx, agentID, previousID)
		if err != nil {
			return nil, err
		}
		plain, err := s.cipher.Open(r.secret, []byte("callback|"+r.ID))
		if err != nil {
			return nil, err
		}
		if r.Transport != transport || r.Destination != destination || r.EnvironmentID != environmentID || !hmac.Equal(plain, []byte(secret)) || r.Status != "active" ||
			strings.Join(r.Events, ",") != strings.Join(events, ",") || r.V3 != sub.V3 {
			return nil, ErrConflict
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var kept int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM callback_receivers WHERE agent_id=? AND status<>'removed'`, agentID).Scan(&kept); err != nil {
		return nil, err
	}
	if kept >= MaxReceiversPerAgent {
		return nil, ErrTooManyReceivers
	}
	id := "cb_" + uuid.NewString()
	enc, err := s.cipher.Seal([]byte(secret), []byte("callback|"+id))
	if err != nil {
		return nil, err
	}
	n, err := s.insertReceiver(ctx, id, agentID, environmentID, clientID, destination, enc, privateAllowed, transport, strings.Join(events, ","), sub.V3)
	if err != nil {
		// A concurrent identical registration may own the unique client binding.
		var won string
		if e := s.db.QueryRowContext(ctx, `SELECT id FROM callback_receivers WHERE agent_id=? AND client_receiver_id=?`, agentID, clientID).Scan(&won); e == nil {
			return s.register(ctx, agentID, environmentID, clientID, destination, secret, privateAllowed, transport, sub)
		}
		return nil, err
	}
	if n != 1 {
		return nil, ErrReceiver
	}
	return s.Get(ctx, agentID, id)
}

// insertReceiver stores a receiver unless the agent's host connection is
// inactive, and returns the rows inserted (0 or 1). The host connection
// rows are locked first (Postgres FOR UPDATE), so a revocation that
// updates host_connections and then removes the agent's receivers either
// waits for this insert and removes it too, or commits first and this
// insert sees the revoked connection. SQLite's single connection already
// serializes both. The transaction ends before insertReceiver returns.
func (s *Service) insertReceiver(ctx context.Context, id, agentID, environmentID, clientID, destination, enc string, privateAllowed bool, transport, events string, v3 bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if s.db.Dialect.IsPostgres() {
		if _, err = tx.ExecContext(ctx, `SELECT agent_id FROM host_connections WHERE agent_id=?`+s.db.Dialect.ForUpdate(), agentID); err != nil {
			return 0, err
		}
	}
	// Parameters in a SELECT list have no column type on Postgres: cast them
	// to the column types (the casts change nothing on SQLite).
	now := s.now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,private_allowed,created_at,transport,events,v3) SELECT CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT) WHERE NOT EXISTS (`+inactiveHostPredicate+`)`, id, agentID, environmentID, clientID, destination, enc, boolInt(privateAllowed), now, transport, events, boolInt(v3), agentID, now)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanReceiver(row interface{ Scan(...any) error }) (*Receiver, error) {
	r := new(Receiver)
	var events string
	err := row.Scan(&r.ID, &r.AgentID, &r.EnvironmentID, &r.ClientReceiverID, &r.Destination, &r.secret, &r.Revision, &r.Status, &r.PrivateAllowed, &r.Transport, &events, &r.V3)
	r.Events = splitEvents(events)
	return r, err
}

const receiverColumns = `id,agent_id,environment_id,client_receiver_id,destination,secret_enc,revision,status,private_allowed,transport,events,v3`

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
			_, err = tx.ExecContext(ctx, `UPDATE callback_subject_subscriptions SET receiver_revision=? WHERE receiver_id=?`, revision+1, id)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET receiver_revision=? WHERE receiver_id=? AND status IN ('pending','delivering')`, revision+1, id)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason=? WHERE receiver_id=? AND status IN ('pending','delivering')`, "receiver_"+status, id)
		if err == nil && action == "remove" {
			// A removed receiver stops being the agent's default.
			_, err = tx.ExecContext(ctx, `DELETE FROM callback_defaults WHERE receiver_id=?`, id)
		}
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
// Inbox calls it once per receiver the request names. With ref "" it
// subscribes the agent's default receiver (callbacks.set_default), when it
// has an active one and its connection is active; otherwise nothing.
func (s *Service) RegisterTx(ctx context.Context, tx *sql.Tx, r *inbox.Request, ref string) error {
	if ref == "" {
		def, revision, err := defaultReceiver(ctx, tx, r.AgentID)
		if err != nil || def == "" {
			return err
		}
		if hostConnectionActive(ctx, tx, r.AgentID, s.now().UnixMilli()) != nil {
			return nil
		}
		// A default receiver that cannot take this item's shape skips it.
		if v3, err := receiverV3(ctx, tx, def); err != nil || (needsV3(r) && !v3) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES(?,?,?) ON CONFLICT DO NOTHING`, r.ID, def, revision)
		return err
	}
	if err := hostConnectionActive(ctx, tx, r.AgentID, s.now().UnixMilli()); err != nil {
		return err
	}
	var revision int
	var v3 bool
	err := tx.QueryRowContext(ctx, `SELECT revision,v3 FROM callback_receivers WHERE id=? AND agent_id=? AND status='active'`, ref, r.AgentID).Scan(&revision, &v3)
	if err != nil {
		return ErrReceiver
	}
	if needsV3(r) && !v3 {
		return ErrNeedsV3
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES(?,?,?) ON CONFLICT DO NOTHING`, r.ID, ref, revision)
	return err
}

// ErrNeedsV3 refuses a receiver without the v3 capability for an item whose
// decision only the version 3 shape can carry (more than 12 calls).
var ErrNeedsV3 = fmt.Errorf("this receiver takes the version 2 decision shape, which carries at most %d calls; register a receiver with v3: true for this item", maxLegacyCalls)

// maxLegacyCalls is the most calls a version 2 decision event carries.
const maxLegacyCalls = 12

// needsV3 reports whether r's decision event needs the version 3 shape.
func needsV3(r *inbox.Request) bool { return len(r.Tools) > maxLegacyCalls }

func receiverV3(ctx context.Context, q hostQuery, ref string) (bool, error) {
	var v3 bool
	err := q.QueryRowContext(ctx, `SELECT v3 FROM callback_receivers WHERE id=?`, ref).Scan(&v3)
	return v3, err
}

// CheckSubscription implements inbox.CallbackChecker: ref must be an
// active receiver of the agent that can carry r's decision. Submit asks it
// for every named receiver, dry runs included.
func (s *Service) CheckSubscription(ctx context.Context, agentID, ref string, r *inbox.Request) error {
	rec, err := s.Get(ctx, agentID, ref)
	if err != nil {
		return err
	}
	if rec.Status != "active" {
		return ErrReceiver
	}
	if needsV3(r) && !rec.V3 {
		return ErrNeedsV3
	}
	return nil
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
	// Answers are a version 3 item's answers, keyed by question ID.
	Answers map[string]inbox.Answer `json:"answers,omitempty"`
}
type Event struct {
	Type      string    `json:"type"`
	EventID   string    `json:"event_id"`
	Timestamp string    `json:"timestamp"`
	Data      EventData `json:"data"`
}

// DecisionTx writes one logical event with all outcomes, for every
// receiver that follows the item, in the same transaction as verdicts,
// answers, grants and audit. Drafts never call this hook. A legacy item is
// a deferred approval: its followers get approval.decided instead.
func (s *Service) DecisionTx(ctx context.Context, tx *sql.Tx, r *inbox.Request) error {
	if r.ExecutionMode == "legacy" {
		return s.legacyDecisionTx(ctx, tx, r)
	}
	subs, err := subscriptionsTx(ctx, tx, `SELECT receiver_id,receiver_revision FROM callback_subscriptions WHERE request_id=? ORDER BY receiver_id`, r.ID)
	if err != nil || len(subs) == 0 {
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
	data := EventData{InboxID: r.ID, DecisionRevision: r.Revision, Status: status, Calls: outcomes, OverallNote: r.OwnerNote, RequestExpiresAt: r.ExpiresAt, GrantExpiresAt: r.GrantsExpire, StatusRef: r.ID, Response: r.Response, Answers: r.Answers}
	// A receiver without the v3 capability gets the version 2 shape: no
	// answers, and no event at all when the calls do not fit it.
	legacy := data
	legacy.Answers = nil
	q := queued{agentID: r.AgentID, subject: r.ID, revision: r.Revision, eventType: kind, dedupe: fmt.Sprintf("inbox:%s:%d", r.ID, r.Revision),
		data: data, legacy: legacy, legacySkip: len(outcomes) > maxLegacyCalls}
	for _, sub := range subs {
		q.receiver, q.receiverRevision = sub.receiver, sub.revision
		// A repeated logical event fails the transaction (dedupe key).
		if _, err := s.queueTx(ctx, tx, q); err != nil {
			return err
		}
	}
	return nil
}

// deniedPrefixes are every special-use range a public receiver must not
// resolve to (RFC 6890 and its updates): this host, private, shared (CGN),
// loopback, link-local, documentation, benchmarking, protocol assignments,
// multicast, reserved and broadcast, and their IPv6 equivalents, including
// the prefixes that embed an IPv4 address (NAT64, 6to4, Teredo).
var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0/96", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// privateIP reports whether ip is not a public unicast address: an IPv4
// address mapped into IPv6 is checked as IPv4.
func privateIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
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
	Type           string        `json:"type,omitempty"`
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
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.request_id,o.event_type,o.status,o.attempts,o.terminal_reason,o.delivered_at FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE r.agent_id=? AND o.request_id=? ORDER BY o.next_attempt_at,o.id`, agentID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.RequestID, &d.Type, &d.Status, &d.Attempts, &d.TerminalReason, &d.DeliveredAt); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,status,attempts,terminal_reason,delivered_at FROM callback_outbox WHERE receiver_id IN (SELECT id FROM callback_receivers WHERE id=? AND agent_id=?) ORDER BY next_attempt_at DESC LIMIT 100`, ref, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.RequestID, &d.Type, &d.Status, &d.Attempts, &d.TerminalReason, &d.DeliveredAt); err != nil {
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

// DeliverOne delivers the oldest due push event in the caller's goroutine
// (tests and tools; Run delivers in batches, delivery.go).
func (s *Service) DeliverOne(ctx context.Context) error {
	ids, err := s.pickDue(ctx, 1)
	if err != nil || len(ids) == 0 {
		return err
	}
	return s.deliver(ctx, ids[0])
}

// deliver claims one due event and sends it. Claims have leases, so a lost
// acknowledgement may cause a physical duplicate with the same event id;
// a claim another node (or worker) took first makes this a no-op.
func (s *Service) deliver(ctx context.Context, id string) error {
	now := s.now().UnixMilli()
	var payload, ref string
	var rev int
	// The claim returns the attempt number it took, so finish is fenced on
	// this claim even if another claim ran between the read and the claim.
	var claimed int
	err := s.db.QueryRowContext(ctx, `UPDATE callback_outbox SET status='delivering',claim_until=?,attempts=attempts+1 WHERE id=? AND ((status='pending' AND next_attempt_at<=?) OR (status='delivering' AND claim_until<=?)) RETURNING attempts,payload,receiver_id,receiver_revision`, now+60000, id, now, now).Scan(&claimed, &payload, &ref, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	attempts := claimed - 1 // every attempts+1 below is this claim's attempt
	receiver, err := scanReceiver(s.db.QueryRowContext(ctx, `SELECT `+receiverColumns+` FROM callback_receivers WHERE id=?`, ref))
	if err != nil || receiver.Status != "active" {
		return s.finish(ctx, id, attempts+1, 0, "receiver_unavailable", true)
	}
	if receiver.Revision != rev {
		return s.finish(ctx, id, attempts+1, 0, "receiver_revision_changed", false)
	}
	if err := hostConnectionActive(ctx, s.db, receiver.AgentID, s.now().UnixMilli()); err != nil {
		if errors.Is(err, ErrReceiver) {
			return s.finish(ctx, id, attempts+1, 0, "connection_revoked_or_expired", true)
		}
		return s.finish(ctx, id, attempts+1, 0, "connection_unavailable", false)
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

// finish records attempt and moves the event out of delivering. The state
// change is fenced on the claim (status delivering and attempts equal to
// attempt): when the claim was lost (its lease expired and a newer claim
// took the event, or the receiver was disabled meanwhile), the attempt is
// still recorded in the history, but the event's state is left to its
// current owner.
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
	// A lost claim updates no row: nothing more to do (see above).
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status=?,terminal_reason=?,next_attempt_at=?,claim_until=0,delivered_at=? WHERE id=? AND status='delivering' AND attempts=?`, status, reason, next, delivered, id, attempt)
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

// Connection revocation and registration use the same SQLite write boundary.
// Generic receivers retain their existing authorization path.
const inactiveHostPredicate = `SELECT 1 FROM host_connections h WHERE h.agent_id=? AND (h.status<>'active' OR h.expires_at<=? OR NOT EXISTS(SELECT 1 FROM agents a JOIN users u ON u.id=h.user_id WHERE a.id=h.agent_id AND a.disabled=0 AND a.owner_user=h.user_id AND a.token_hash=h.credential_hash AND u.status='active'))`

type hostQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func hostConnectionActive(ctx context.Context, q hostQuery, agent string, now int64) error {
	var count int
	// The derived table has an alias: Postgres before 16 requires one.
	if e := q.QueryRowContext(ctx, `SELECT count(*) FROM (`+inactiveHostPredicate+`) AS inactive`, agent, now).Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		return ErrReceiver
	}
	return nil
}

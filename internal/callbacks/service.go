// Package callbacks delivers committed Inbox decisions and the other
// webhook events (events.go): execution outcomes, legacy approval
// decisions and results, pings, and the events other packages emit with
// EmitForAgentTx. Delivery is at least once; receivers deduplicate the
// stable event id before any agent dispatch. Events have no order:
// retries and parallel deliveries can deliver a later event first, so a
// receiver reads the current state from the event's status_ref (Inbox
// items: inbox.status) instead of trusting the order of arrival.
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/netguard"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var ErrReceiver = errors.New("callback receiver is unavailable or belongs to another agent")
var ErrConflict = errors.New("callback receiver revision or registration changed")

// ErrTooManyReceivers refuses a new registration while the agent keeps
// as many active receivers as the cap (Service.MaxReceivers). Errors
// that match it say the cap.
var ErrTooManyReceivers = errors.New("too many active receivers")

// DefaultMaxReceiversPerAgent is the default cap on an agent's active
// receivers (setting callback_max_receivers_per_agent). Disabled, idle and
// removed receivers do not count: T3 Code registers one receiver per
// session thread, all under its user's one host-connection agent.
const DefaultMaxReceiversPerAgent = 500

// maxReceiversLimit bounds what the setting may ask for.
const maxReceiversLimit = 100000

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
	// DisabledReason is "idle" when the retention sweep disabled the
	// receiver (DisableIdle); naming it again wakes it.
	DisabledReason string `json:"disabled_reason,omitempty"`
	secret         string
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
	// MaxReceivers returns the cap on an agent's active receivers; nil, or
	// a value below 1, means DefaultMaxReceiversPerAgent.
	MaxReceivers func() int
}

// maxReceivers is the cap in force.
func (s *Service) maxReceivers() int {
	if s.MaxReceivers == nil {
		return DefaultMaxReceiversPerAgent
	}
	n := s.MaxReceivers()
	if n < 1 {
		return DefaultMaxReceiversPerAgent
	}
	if n > maxReceiversLimit {
		return maxReceiversLimit
	}
	return n
}

// tooManyReceivers is ErrTooManyReceivers with the cap in force.
func (s *Service) tooManyReceivers() error {
	return fmt.Errorf("%w: an agent may keep at most %d active receivers; remove or disable one first", ErrTooManyReceivers, s.maxReceivers())
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
		idle := r.Status == "disabled" && r.DisabledReason == DisabledIdle
		if r.Transport != transport || r.Destination != destination || r.EnvironmentID != environmentID || !hmac.Equal(plain, []byte(secret)) || (r.Status != "active" && !idle) ||
			!SameEvents(r.Events, events) || r.V3 != sub.V3 {
			return nil, ErrConflict
		}
		if idle {
			// The identical registration again: the client still uses the
			// receiver. It is woken whatever the cap (it was counted when it
			// was registered).
			if err := s.wakeIdle(ctx, s.db, agentID, r.ID); err != nil {
				return nil, err
			}
			return s.Get(ctx, agentID, r.ID)
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// A quick answer before sealing the secret; insertReceiver checks the
	// cap again under the agent lock. Only active receivers count.
	var kept int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM callback_receivers WHERE agent_id=? AND status='active'`, agentID).Scan(&kept); err != nil {
		return nil, err
	}
	if kept >= s.maxReceivers() {
		return nil, s.tooManyReceivers()
	}
	id := "cb_" + uuid.NewString()
	enc, err := s.cipher.Seal([]byte(secret), []byte("callback|"+id))
	if err != nil {
		return nil, err
	}
	n, err := s.insertReceiver(ctx, id, agentID, environmentID, clientID, destination, enc, privateAllowed, transport, strings.Join(events, ","), sub.V3)
	if err != nil {
		// A concurrent identical registration may own the unique client
		// binding (or have taken the last place under the cap).
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

// readCommitted opens the transactions that take a lock and then count
// (receivers, pings, claims): each statement after the lock must see what
// the previous holder committed, which a snapshot taken at the start of
// the transaction (REPEATABLE READ or SERIALIZABLE as the server default)
// would not. SQLite ignores it: one connection runs one transaction.
var readCommitted = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// lockAgentTx serializes, on Postgres, the transactions that count an
// agent's callback rows before they insert one (receivers, pings): the
// count read after the lock holds until the insert commits, on every node.
// The lock ends with tx. SQLite's single connection already serializes.
func (s *Service) lockAgentTx(ctx context.Context, tx *sql.Tx, agentID string) error {
	if !s.db.Dialect.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(CAST(? AS TEXT)))`, "callbacks|"+agentID)
	return err
}

// insertReceiver stores a receiver unless the agent's host connection is
// inactive or the agent keeps as many active receivers as the cap
// (ErrTooManyReceivers), and returns the rows inserted (0 or 1). The count
// is read under the agent lock, so concurrent registrations never pass
// the cap together. The host connection rows are locked first (Postgres
// FOR UPDATE), so a revocation that updates host_connections and then
// removes the agent's receivers either waits for this insert and removes
// it too, or commits first and this insert sees the revoked connection.
// SQLite's single connection already serializes both. The transaction
// ends before insertReceiver returns.
func (s *Service) insertReceiver(ctx context.Context, id, agentID, environmentID, clientID, destination, enc string, privateAllowed bool, transport, events string, v3 bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = s.lockAgentTx(ctx, tx, agentID); err != nil {
		return 0, err
	}
	if s.db.Dialect.IsPostgres() {
		if _, err = tx.ExecContext(ctx, `SELECT agent_id FROM host_connections WHERE agent_id=?`+s.db.Dialect.ForUpdate(), agentID); err != nil {
			return 0, err
		}
	}
	var kept int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM callback_receivers WHERE agent_id=? AND status='active'`, agentID).Scan(&kept); err != nil {
		return 0, err
	}
	if kept >= s.maxReceivers() {
		return 0, s.tooManyReceivers()
	}
	// Parameters in a SELECT list have no column type on Postgres: cast them
	// to the column types (the casts change nothing on SQLite).
	now := s.now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,private_allowed,created_at,transport,events,v3,host) SELECT CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS TEXT) WHERE NOT EXISTS (`+inactiveHostPredicate+`)`, id, agentID, environmentID, clientID, destination, enc, boolInt(privateAllowed), now, transport, events, boolInt(v3), destinationHost(destination), agentID, now)
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
	err := row.Scan(&r.ID, &r.AgentID, &r.EnvironmentID, &r.ClientReceiverID, &r.Destination, &r.secret, &r.Revision, &r.Status, &r.PrivateAllowed, &r.Transport, &events, &r.V3, &r.DisabledReason)
	// Sorted, like NormalizeEvents: the column default of receivers from
	// before migration 0060 lists the legacy types in another order.
	r.Events = splitEvents(events)
	sort.Strings(r.Events)
	return r, err
}

const receiverColumns = `id,agent_id,environment_id,client_receiver_id,destination,secret_enc,revision,status,private_allowed,transport,events,v3,disabled_reason`

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
	// An explicit change clears disabled_reason: a receiver its agent
	// disabled is never woken by use.
	result, err := tx.ExecContext(ctx, `UPDATE callback_receivers SET status=?,secret_enc=?,revision=revision+1,disabled_reason='' WHERE id=? AND agent_id=? AND revision=? AND status<>'removed'`, status, enc, id, agentID, revision)
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
	// A new item to follow wakes an idle receiver (named, or the default).
	if err := s.wakeIdle(ctx, tx, r.AgentID, ref); err != nil {
		return err
	}
	if ref == "" {
		def, revision, err := defaultReceiver(ctx, tx, r.AgentID)
		if err != nil || def == "" {
			return err
		}
		if hostConnectionActive(ctx, tx, r.AgentID, s.now().UnixMilli()) != nil {
			return nil
		}
		// A default receiver that cannot take this item's shape, or takes
		// no inbox.* event, skips it.
		if v3, err := receiverV3(ctx, tx, def); err != nil || (needsV3(r) && !v3) {
			return err
		}
		if ok, err := acceptsInboxTx(ctx, tx, def); err != nil || !ok {
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
	if ok, err := acceptsInboxTx(ctx, tx, ref); err != nil || !ok {
		if err == nil {
			err = ErrNoInboxEvents
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES(?,?,?) ON CONFLICT DO NOTHING`, r.ID, ref, revision)
	return err
}

// ErrNoInboxEvents refuses a named receiver that accepts none of the
// inbox.* events: it would follow the item and never hear of it.
var ErrNoInboxEvents = errors.New("this receiver accepts no inbox.* events (inbox.decision, inbox.expired, inbox.cancelled), so it would never hear of this item; name a receiver that does, or poll inbox.status")

// inboxEvents are the events that announce an Inbox item's end.
var inboxEvents = []string{EventInboxDecision, EventInboxExpired, EventInboxCancelled}

func acceptsAny(events []string, types []string) bool {
	for _, t := range types {
		if acceptsEvent(events, t) {
			return true
		}
	}
	return false
}

// acceptsInboxTx reports whether receiver ref takes any inbox.* event.
func acceptsInboxTx(ctx context.Context, q hostQuery, ref string) (bool, error) {
	var events string
	if err := q.QueryRowContext(ctx, `SELECT events FROM callback_receivers WHERE id=?`, ref).Scan(&events); err != nil {
		return false, err
	}
	return acceptsAny(splitEvents(events), inboxEvents), nil
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
	if err := s.wakeIdle(ctx, s.db, agentID, ref); err != nil {
		return err
	}
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
	if !acceptsAny(rec.Events, inboxEvents) {
		return ErrNoInboxEvents
	}
	return nil
}

// DefaultWarning implements inbox.DefaultWarner: why the agent's default
// receiver will not follow r, an item that names no receiver ("" when it
// will, or the agent has none). RegisterTx then skips the default
// receiver; Submit warns the agent so it polls or names another receiver.
func (s *Service) DefaultWarning(ctx context.Context, agentID string, r *inbox.Request) string {
	def, _, err := defaultReceiver(ctx, s.db, agentID)
	if err != nil || def == "" {
		return ""
	}
	rec, err := s.Get(ctx, agentID, def)
	if err != nil {
		return ""
	}
	if needsV3(r) && !rec.V3 {
		return fmt.Sprintf("your default receiver takes the version 2 decision shape (at most %d calls), so it will not hear of this item; name a receiver with v3: true in callback_ref, or poll inbox.status", maxLegacyCalls)
	}
	if !acceptsAny(rec.Events, inboxEvents) {
		return "your default receiver accepts no inbox.* events, so it will not hear of this item; name a receiver that does in callback_ref, or poll inbox.status"
	}
	return ""
}

type Outcome struct {
	CallID  string `json:"call_id"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}
type EventData struct {
	InboxID          string `json:"inbox_id"`
	DecisionRevision int    `json:"decision_revision"`
	// Status is the event's outcome: decided (inbox.decision), expired or
	// cancelled. It stays "decided" for every decision because receivers
	// built for the version 2 shape (T3 Code) accept only these three.
	Status string `json:"status"`
	// ItemStatus is the item's status as inbox.status reports it
	// (approved, denied, returned, answered, expired, cancelled). Only v3
	// receivers get it: version 2 receivers refuse unknown fields.
	ItemStatus       string                `json:"item_status,omitempty"`
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
	data := EventData{InboxID: r.ID, DecisionRevision: r.Revision, Status: status, ItemStatus: r.Status, Calls: outcomes, OverallNote: r.OwnerNote, RequestExpiresAt: r.ExpiresAt, GrantExpiresAt: r.GrantsExpire, StatusRef: r.ID, Response: r.Response, Answers: r.Answers}
	// A receiver without the v3 capability gets the version 2 shape: no
	// answers and no item_status, and no event at all when the calls do
	// not fit it.
	legacy := data
	legacy.Answers = nil
	legacy.ItemStatus = ""
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

// privateIP reports whether ip is not a public unicast address
// (netguard.Public: IPv6 only in 2000::/3, and no special-use range).
func privateIP(ip net.IP) bool { return !netguard.Public(ip) }

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
	result, err := s.db.ExecContext(ctx, `UPDATE callback_outbox SET status='pending',retry_base=attempts,next_attempt_at=?,terminal_reason='' WHERE id=? AND status='failed' AND event_type<>'ping' AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id=? AND status='active')`, s.now().UnixMilli(), eventID, agentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		var kind string
		e := s.db.QueryRowContext(ctx, `SELECT o.event_type FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE o.id=? AND r.agent_id=?`, eventID, agentID).Scan(&kind)
		if e == nil && kind == EventPing {
			return ErrPingNotRetried
		}
		return ErrReceiver
	}
	return nil
}

// ErrPingNotRetried refuses a manual retry of a ping: a receiver keeps at
// most one open ping, so callbacks.test sends a new one. It matches
// ErrConflict (409 conflict).
var ErrPingNotRetried error = pingRetryError{}

type pingRetryError struct{}

func (pingRetryError) Error() string {
	return "a ping is not retried; send a new one with callbacks.test"
}
func (pingRetryError) Is(target error) bool { return target == ErrConflict }

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

// claim takes one due event for delivery and returns the attempt number
// it took, so finish is fenced on this claim even if another claim ran
// between the read and the claim. sql.ErrNoRows means the event is not due
// (another node took it, or it finished) or its receiver already has
// deliveryPerReceiver events in flight. The cap is checked in the claim
// itself, and claims of one receiver take turns on Postgres (a
// transaction-scoped lock on the receiver), so every node together never
// keeps more than deliveryPerReceiver events of a receiver in flight.
func (s *Service) claim(ctx context.Context, id string) (claimed int, payload, ref string, rev int, err error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return 0, "", "", 0, err
	}
	defer tx.Rollback()
	if s.db.Dialect.IsPostgres() {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('callback-receiver|' || receiver_id)) FROM callback_outbox WHERE id=?`, id); err != nil {
			return 0, "", "", 0, err
		}
	}
	err = tx.QueryRowContext(ctx, `UPDATE callback_outbox SET status='delivering',claim_until=?,attempts=attempts+1 WHERE id=? AND ((status='pending' AND next_attempt_at<=?) OR (status='delivering' AND claim_until<=?)) AND (SELECT count(*) FROM callback_outbox busy WHERE busy.receiver_id=callback_outbox.receiver_id AND busy.status='delivering' AND busy.claim_until>? AND busy.id<>callback_outbox.id)<? RETURNING attempts,payload,receiver_id,receiver_revision`,
		now+60000, id, now, now, now, deliveryPerReceiver).Scan(&claimed, &payload, &ref, &rev)
	if err != nil {
		return 0, "", "", 0, err
	}
	return claimed, payload, ref, rev, tx.Commit()
}

// deliver claims one due event and sends it. Claims have leases, so a lost
// acknowledgement may cause a physical duplicate with the same event id;
// a claim another node (or worker) took first, or a receiver at its
// in-flight cap, makes this a no-op.
func (s *Service) deliver(ctx context.Context, id string) error {
	claimed, payload, ref, rev, err := s.claim(ctx, id)
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

// DisabledIdle is the disabled_reason of a receiver the retention sweep
// disabled (DisableIdle).
const DisabledIdle = "idle"

// IdleReceiverAfter is how long an active push receiver may go without a
// successful delivery and without anything new to follow before
// DisableIdle disables it.
const IdleReceiverAfter = 30 * 24 * time.Hour

// execer runs a statement on the pool or in a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// wakeIdle makes ref active again when the retention sweep disabled it for
// idleness and its agent uses it again: idleness is not a decision anyone
// made. ref "" means the agent's default receiver. A receiver its agent or
// owner disabled stays disabled. woken_at keeps the next sweep from
// disabling it again at once. The cap does not apply: the receiver was
// counted when it was registered. q is the pool, or the caller's
// transaction (never the pool inside one).
func (s *Service) wakeIdle(ctx context.Context, q execer, agentID, ref string) error {
	_, err := q.ExecContext(ctx, `UPDATE callback_receivers SET status='active',disabled_reason='',woken_at=? WHERE agent_id=? AND status='disabled' AND disabled_reason=? AND id=COALESCE(NULLIF(CAST(? AS TEXT),''),(SELECT receiver_id FROM callback_defaults WHERE agent_id=?))`,
		s.now().UnixMilli(), agentID, DisabledIdle, ref, agentID)
	return err
}

// DisableIdle disables every active push receiver that, for
// IdleReceiverAfter, was not registered or woken, had no successful
// delivery, and had nothing new to follow: no event queued for it, no
// Inbox item or legacy approval that names it, and no default change to
// it. It sets disabled_reason "idle", so the receiver stops counting
// toward the cap and wakes when its agent names it again. Pull receivers
// are left alone. The revision does not change (it tracks the agent's own
// changes). The update is idempotent, so the sweep is safe on every node.
func (s *Service) DisableIdle(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-IdleReceiverAfter).UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE callback_receivers SET status='disabled',disabled_reason=? WHERE status='active' AND transport='push' AND created_at<? AND woken_at<?
 AND NOT EXISTS (SELECT 1 FROM callback_outbox o WHERE o.receiver_id=callback_receivers.id AND (o.created_at>=? OR o.delivered_at>=?))
 AND NOT EXISTS (SELECT 1 FROM callback_subscriptions s JOIN inbox_requests q ON q.id=s.request_id WHERE s.receiver_id=callback_receivers.id AND q.created_at>=?)
 AND NOT EXISTS (SELECT 1 FROM callback_subject_subscriptions j WHERE j.receiver_id=callback_receivers.id AND j.created_at>=?)
 AND NOT EXISTS (SELECT 1 FROM callback_defaults d WHERE d.receiver_id=callback_receivers.id AND d.updated_at>=?)`,
		DisabledIdle, cutoff, cutoff, cutoff, cutoff, cutoff, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

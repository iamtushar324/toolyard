package callbacks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// Event types. Every event has the same signed envelope (Envelope); the
// data depends on the type.
const (
	// EventInboxDecision: an Inbox item is fully answered (every call's
	// verdict and every required question's answer). EventData.
	EventInboxDecision = "inbox.decision"
	// EventInboxExpired and EventInboxCancelled: the item closed without
	// a decision. EventData.
	EventInboxExpired   = "inbox.expired"
	EventInboxCancelled = "inbox.cancelled"
	// EventExecutionCompleted: a granted call ran and toolyard recorded
	// its outcome. ExecutionData.
	EventExecutionCompleted = "execution.completed"
	// EventApprovalDecided and EventApprovalExecuted: a legacy deferred
	// approval was decided, and its call ran. ApprovalData.
	EventApprovalDecided  = approval.EventApprovalDecided
	EventApprovalExecuted = approval.EventApprovalExecuted
	// EventPing: callbacks.test. PingData.
	EventPing = "ping"
)

// eventTypeShape is what EmitTx accepts: lowercase dotted words, such as
// job.completed.
var eventTypeShape = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// Envelope is the signed body of every event: its type, a stable event_id
// (also the webhook-id header), an RFC 3339 timestamp, and the data.
type Envelope struct {
	Type      string `json:"type"`
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`
	Data      any    `json:"data"`
}

// ExecutionData is the data of execution.completed. The tool's result is
// not included: read inbox.status, whose execution state is authoritative.
type ExecutionData struct {
	InboxID    string `json:"inbox_id"`
	CallID     string `json:"call_id"`
	GrantID    string `json:"grant_id"`
	Tool       string `json:"tool"`
	State      string `json:"state"` // succeeded | failed | outcome_unknown
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at"`
	Detail     string `json:"detail,omitempty"`
	StatusRef  string `json:"status_ref"`
}

// ApprovalData is the data of approval.decided and approval.executed,
// keyed by the approval ID of a legacy deferred approval. The call's
// arguments and result are not included: read tools.poll_approval.
type ApprovalData struct {
	ApprovalID string `json:"approval_id"`
	Status     string `json:"status"` // allowed | denied | expired | cancelled
	Tool       string `json:"tool,omitempty"`
	Upstream   string `json:"upstream,omitempty"`
	DecidedAt  int64  `json:"decided_at,omitempty"`
	OwnerNote  string `json:"owner_note,omitempty"`
	// approval.executed only.
	ExecutionState string `json:"execution_state,omitempty"` // succeeded | failed | outcome_unknown
	IsError        bool   `json:"is_error,omitempty"`
	Error          string `json:"error,omitempty"`
	ExecutedAt     int64  `json:"executed_at,omitempty"`
	StatusRef      string `json:"status_ref"`
}

// PingData is the data of a ping (callbacks.test).
type PingData struct {
	CallbackRef string `json:"callback_ref"`
	Message     string `json:"message"`
}

type subscription struct {
	receiver string
	revision int
}

// subscriptionsTx reads the receivers that follow one subject. The rows are
// read to the end before the caller writes: a Postgres connection runs one
// statement at a time.
func subscriptionsTx(ctx context.Context, tx *sql.Tx, query, subject string) ([]subscription, error) {
	rows, err := tx.QueryContext(ctx, query, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscription
	for rows.Next() {
		var s subscription
		if err := rows.Scan(&s.receiver, &s.revision); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// queued is one event for one receiver.
type queued struct {
	agentID          string
	subject          string // callback_outbox.request_id: what the event is about
	revision         int    // callback_outbox.decision_revision (Inbox events)
	receiver         string
	receiverRevision int
	eventType        string
	dedupe           string // "" stores NULL: no deduplication
	ignoreDuplicate  bool   // a repeated dedupe key is a no-op, not an error
	data             any
}

// queueTx writes one event to the outbox inside tx and returns its id. The
// event fails at once when the agent's host connection is revoked or
// expired, so its history says why it never left.
func (s *Service) queueTx(ctx context.Context, tx *sql.Tx, q queued) (string, error) {
	id := "evt_" + uuid.NewString()
	payload, err := json.Marshal(Envelope{Type: q.eventType, EventID: id, Timestamp: s.now().UTC().Format(time.RFC3339Nano), Data: q.data})
	if err != nil {
		return "", err
	}
	status, reason := "pending", ""
	if e := hostConnectionActive(ctx, tx, q.agentID, s.now().UnixMilli()); e != nil {
		if !errors.Is(e, ErrReceiver) {
			return "", e
		}
		status, reason = "failed", "connection_revoked_or_expired"
	}
	var dedupe any
	if q.dedupe != "" {
		dedupe = q.dedupe
	}
	query := `INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at,status,terminal_reason,event_type,dedupe_key) VALUES(?,?,?,?,?,?,?,?,?,?,?)`
	if q.ignoreDuplicate {
		query += ` ON CONFLICT DO NOTHING`
	}
	if _, err = tx.ExecContext(ctx, query, id, q.subject, q.revision, q.receiver, q.receiverRevision, string(payload), s.now().UnixMilli(), status, reason, q.eventType, dedupe); err != nil {
		return "", err
	}
	return id, nil
}

// queueAllTx queues one event for each subscription; a repeated logical
// event (the same dedupe key for the same receiver) is skipped.
func (s *Service) queueAllTx(ctx context.Context, tx *sql.Tx, agentID, subject string, subs []subscription, eventType, dedupe string, data any) error {
	for _, sub := range subs {
		if _, err := s.queueTx(ctx, tx, queued{agentID: agentID, subject: subject, receiver: sub.receiver, receiverRevision: sub.revision,
			eventType: eventType, dedupe: dedupe, ignoreDuplicate: true, data: data}); err != nil {
			return err
		}
	}
	return nil
}

// EmitTx queues one event for ref inside tx (at-least-once, signed, retried).
//
// ref is a callback_ref (callbacks.register); the caller checked that it
// belongs to the agent the event is for (CheckRef). eventType is lowercase
// dotted words (job.completed); data is any JSON value, sent as the
// envelope's data. The event commits or rolls back with tx, and the
// delivery loop sends it like an Inbox decision: Standard Webhooks
// signature, retries, attempt history. The delivery history files it under
// the first of data's inbox_id, approval_id, job_id or subject.
//
// EmitTx returns ErrReceiver only when ref names no receiver. A disabled or
// removed receiver drops the event and returns nil, so an agent that turned
// its receiver off never fails the caller's transaction.
func (s *Service) EmitTx(ctx context.Context, tx *sql.Tx, ref, eventType string, data any) error {
	_, err := s.emitTx(ctx, tx, ref, eventType, data)
	return err
}

func (s *Service) emitTx(ctx context.Context, tx *sql.Tx, ref, eventType string, data any) (string, error) {
	if len(eventType) > 64 || !eventTypeShape.MatchString(eventType) {
		return "", fmt.Errorf("callbacks: invalid event type %q", eventType)
	}
	var agentID, status string
	var revision int
	err := tx.QueryRowContext(ctx, `SELECT agent_id,revision,status FROM callback_receivers WHERE id=?`, ref).Scan(&agentID, &revision, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrReceiver
	}
	if err != nil {
		return "", err
	}
	if status != "active" {
		return "", nil
	}
	return s.queueTx(ctx, tx, queued{agentID: agentID, subject: subjectOf(data), receiver: ref, receiverRevision: revision, eventType: eventType, data: data})
}

// subjectOf names what an event is about, for the delivery history: the
// first of inbox_id, approval_id, job_id or subject in data.
func subjectOf(data any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	for _, k := range []string{"inbox_id", "approval_id", "job_id", "subject"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// CheckRef reports whether ref is an active receiver of the agent
// (ErrReceiver otherwise). Callers that store a callback_ref for later
// events (EmitTx) check it when they store it.
func (s *Service) CheckRef(ctx context.Context, agentID, ref string) error {
	r, err := s.Get(ctx, agentID, ref)
	if err != nil {
		return err
	}
	if r.Status != "active" {
		return ErrReceiver
	}
	return nil
}

// defaultReceiver returns the agent's default receiver and its revision
// when it is active, or "".
func defaultReceiver(ctx context.Context, q hostQuery, agentID string) (string, int, error) {
	var id string
	var revision int
	err := q.QueryRowContext(ctx, `SELECT r.id,r.revision FROM callback_defaults d JOIN callback_receivers r ON r.id=d.receiver_id WHERE d.agent_id=? AND r.agent_id=? AND r.status='active'`, agentID, agentID).Scan(&id, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return id, revision, err
}

// DefaultRef returns the agent's active default receiver, or "".
func (s *Service) DefaultRef(ctx context.Context, agentID string) (string, error) {
	id, _, err := defaultReceiver(ctx, s.db, agentID)
	return id, err
}

// SetDefault makes ref the agent's default receiver: the one that follows
// an Inbox item (and a legacy deferred approval) when the call names none.
// ref "" clears it.
func (s *Service) SetDefault(ctx context.Context, agentID, ref string) error {
	if agentID == "" {
		return ErrReceiver
	}
	if ref == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM callback_defaults WHERE agent_id=?`, agentID)
		return err
	}
	if err := s.CheckRef(ctx, agentID, ref); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO callback_defaults(agent_id,receiver_id,updated_at) VALUES(?,?,?) ON CONFLICT(agent_id) DO UPDATE SET receiver_id=excluded.receiver_id,updated_at=excluded.updated_at`, agentID, ref, s.now().UnixMilli())
	return err
}

// List returns the agent's receivers, newest first, with Default set on
// its default receiver. Removed receivers are listed only on request.
func (s *Service) List(ctx context.Context, agentID string, withRemoved bool) ([]Receiver, error) {
	q := `SELECT ` + receiverColumns + ` FROM callback_receivers WHERE agent_id=?`
	if !withRemoved {
		q += ` AND status<>'removed'`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC,id LIMIT 100`, agentID)
	if err != nil {
		return nil, err
	}
	out := []Receiver{}
	for rows.Next() {
		r, err := scanReceiver(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	def, err := s.DefaultRef(ctx, agentID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Default = out[i].ID == def
	}
	return out, nil
}

// ByClientID returns the agent's receiver registered under clientID.
func (s *Service) ByClientID(ctx context.Context, agentID, clientID string) (*Receiver, error) {
	r, err := scanReceiver(s.db.QueryRowContext(ctx, `SELECT `+receiverColumns+` FROM callback_receivers WHERE agent_id=? AND client_receiver_id=?`, agentID, clientID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReceiver
	}
	return r, err
}

// GenerateSecret returns a new Standard Webhooks secret: whsec_ and 32
// random bytes in Base64.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}

// Test queues a ping for one of the agent's active receivers and returns
// its event id. The ping is signed and retried like every other event.
func (s *Service) Test(ctx context.Context, agentID, ref string) (string, error) {
	if err := s.CheckRef(ctx, agentID, ref); err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := s.emitTx(ctx, tx, ref, EventPing, PingData{CallbackRef: ref, Message: "Test event from toolyard. Verify the signature, store the event id, and answer 2xx."})
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", ErrReceiver
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ExecutionTx implements inbox.ExecutionCallbacks: every receiver that
// follows the item hears execution.completed once per grant, in the
// transaction that records the outcome.
func (s *Service) ExecutionTx(ctx context.Context, tx *sql.Tx, e *inbox.Execution, agentID, tool string) error {
	subs, err := subscriptionsTx(ctx, tx, `SELECT receiver_id,receiver_revision FROM callback_subscriptions WHERE request_id=? ORDER BY receiver_id`, e.RequestID)
	if err != nil || len(subs) == 0 {
		return err
	}
	data := ExecutionData{InboxID: e.RequestID, CallID: e.CallID, GrantID: e.GrantID, Tool: tool, State: e.State,
		StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, Detail: e.Error, StatusRef: e.RequestID}
	return s.queueAllTx(ctx, tx, agentID, e.RequestID, subs, EventExecutionCompleted, "execution:"+e.GrantID, data)
}

// SubscribeApproval implements approval.EventSink: ref, or the agent's
// default receiver when ref is "", follows a legacy deferred approval.
// It returns ErrReceiver when ref is not one of the agent's active
// receivers.
func (s *Service) SubscribeApproval(ctx context.Context, req *approval.Request, ref string) error {
	if req == nil || req.AgentID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int
	if ref == "" {
		ref, revision, err = defaultReceiver(ctx, tx, req.AgentID)
		if err != nil || ref == "" {
			return err
		}
	} else if err = tx.QueryRowContext(ctx, `SELECT revision FROM callback_receivers WHERE id=? AND agent_id=? AND status='active'`, ref, req.AgentID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReceiver
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO callback_subject_subscriptions(subject_id,receiver_id,receiver_revision,created_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, req.ID, ref, revision, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ApprovalEventTx implements approval.EventSink: every receiver that
// follows the approval hears eventType once, in the transaction that
// records the decision or the result.
func (s *Service) ApprovalEventTx(ctx context.Context, tx *sql.Tx, req *approval.Request, eventType string) error {
	subs, err := subscriptionsTx(ctx, tx, `SELECT receiver_id,receiver_revision FROM callback_subject_subscriptions WHERE subject_id=? ORDER BY receiver_id`, req.ID)
	if err != nil || len(subs) == 0 {
		return err
	}
	data := ApprovalData{ApprovalID: req.ID, Status: req.Status, Tool: req.ToolName, Upstream: req.UpstreamName, DecidedAt: req.DecidedAt, StatusRef: req.ID}
	if eventType == EventApprovalExecuted {
		data.ExecutionState, data.IsError, data.Error, data.ExecutedAt = req.ExecutionState, req.ResultIsError, req.ResultError, req.ResultExecutedAt
	}
	return s.queueAllTx(ctx, tx, req.AgentID, req.ID, subs, eventType, eventType+":"+req.ID, data)
}

// legacyDecisionTx announces the Inbox decision of a legacy item (a
// deferred approval, same ID) to the receivers that follow the approval.
func (s *Service) legacyDecisionTx(ctx context.Context, tx *sql.Tx, r *inbox.Request) error {
	subs, err := subscriptionsTx(ctx, tx, `SELECT receiver_id,receiver_revision FROM callback_subject_subscriptions WHERE subject_id=? ORDER BY receiver_id`, r.ID)
	if err != nil || len(subs) == 0 {
		return err
	}
	status := r.Status
	if status == inbox.StatusApproved {
		status = approval.StatusAllowed
	}
	data := ApprovalData{ApprovalID: r.ID, Status: status, DecidedAt: r.DecidedAt, OwnerNote: r.OwnerNote, StatusRef: r.ID}
	if len(r.Tools) > 0 {
		data.Tool, data.Upstream = r.Tools[0].Tool, r.Tools[0].Upstream
	}
	return s.queueAllTx(ctx, tx, r.AgentID, r.ID, subs, EventApprovalDecided, EventApprovalDecided+":"+r.ID, data)
}

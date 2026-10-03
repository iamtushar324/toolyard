// Package chatnotify fans approval-bus events out to chat channels (Telegram
// first; the Channel interface is pluggable so Slack can follow). It registers
// as an approval.Notifier: on approval.create it posts a message with
// Approve/Deny buttons; on decide/expire/cancel/executed it edits that message
// in place and strips the buttons.
//
// The Registry owns the chat_messages table (one row per approval+channel),
// coalesces duplicate sends, and runs a reconciler that heals the two drift
// directions after a restart: pending approvals with no message get one,
// resolved approvals with a live message get edited + marked resolved.
package chatnotify

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// MessageRef identifies a posted chat message so we can edit it later.
type MessageRef struct {
	ChatID    string
	MessageID string
}

// Channel is one chat backend. Implementations must be safe for concurrent
// use; the Registry may call Send/Update from the notifier goroutine and the
// reconciler goroutine at once.
type Channel interface {
	// Name is the channel discriminator persisted in chat_messages.channel
	// (e.g. "telegram").
	Name() string
	// Ready reports whether the channel is configured + paired and able to
	// deliver. The Registry skips unready channels.
	Ready() bool
	// SendApproval posts a new approval message with Approve/Deny controls
	// keyed by approvalID, returning a ref to the posted message.
	SendApproval(ctx context.Context, approvalID, text string) (MessageRef, error)
	// UpdateApproval edits a previously-posted message's text and removes
	// its action buttons (the decision is final).
	UpdateApproval(ctx context.Context, ref MessageRef, text string) error
}

// Registry implements approval.Notifier and owns chat_messages persistence.
type Registry struct {
	db             *store.DB
	bus            *approval.Bus
	includeDetails func() bool
	log            *slog.Logger

	mu       sync.Mutex
	channels []Channel
}

// NewRegistry builds a Registry. includeDetails is read per-event so a
// settings toggle applies without a restart; nil means details-on.
func NewRegistry(db *store.DB, bus *approval.Bus, includeDetails func() bool) *Registry {
	if includeDetails == nil {
		includeDetails = func() bool { return true }
	}
	return &Registry{
		db:             db,
		bus:            bus,
		includeDetails: includeDetails,
		log:            logx.For("chatnotify"),
	}
}

// Register adds a channel. Safe to call before AddNotifier wiring.
func (r *Registry) Register(ch Channel) {
	r.mu.Lock()
	r.channels = append(r.channels, ch)
	r.mu.Unlock()
}

func (r *Registry) readyChannels() []Channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Channel, 0, len(r.channels))
	for _, ch := range r.channels {
		if ch.Ready() {
			out = append(out, ch)
		}
	}
	return out
}

// OnApproval implements approval.Notifier.
func (r *Registry) OnApproval(ctx context.Context, req *approval.Request, eventType string) {
	// Instance-wide chat channels must not expose a person's GitHub proposal.
	if req.PersonalOwner() != "" {
		return
	}
	switch eventType {
	case "approval.create":
		r.send(ctx, req)
	case "approval.decide", "approval.expire", "approval.cancel", "approval.executed":
		r.update(ctx, req)
	}
}

// send posts a new approval message to every ready channel that doesn't
// already have a row for this approval (coalesce dedupe via the PK).
func (r *Registry) send(ctx context.Context, req *approval.Request) {
	if req.Status != approval.StatusPending {
		return
	}
	text := render(req, r.includeDetails())
	for _, ch := range r.readyChannels() {
		if r.hasMessage(ctx, req.ID, ch.Name()) {
			continue // already sent (coalesced Hold, or duplicate event)
		}
		ref, err := ch.SendApproval(ctx, req.ID, text)
		if err != nil {
			r.log.Warn("send approval failed", "channel", ch.Name(), "approval", req.ID, "err", err.Error())
			continue
		}
		r.insertMessage(ctx, req.ID, ch.Name(), ref)
	}
}

// update edits the message on every channel that has an unresolved row for
// this approval, then marks the row resolved.
func (r *Registry) update(ctx context.Context, req *approval.Request) {
	text := render(req, r.includeDetails())
	for _, m := range r.messagesFor(ctx, req.ID) {
		if m.resolved {
			// Already resolved; only re-edit when an executed result arrived.
			if req.ResultExecutedAt == 0 {
				continue
			}
		}
		ch := r.channelByName(m.channel)
		if ch == nil {
			continue
		}
		if err := ch.UpdateApproval(ctx, MessageRef{ChatID: m.chatID, MessageID: m.messageID}, text); err != nil {
			r.log.Warn("update approval failed", "channel", m.channel, "approval", req.ID, "err", err.Error())
			continue
		}
		if req.Status != approval.StatusPending {
			r.markResolved(ctx, req.ID, m.channel)
		}
	}
}

// RunReconciler heals message/approval drift on startup and every `every`.
// Implements the goroutines.Supervise fn shape via a blocking loop.
func (r *Registry) RunReconciler(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 2 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	r.reconcile(ctx) // immediate pass at startup
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.reconcile(ctx)
		}
	}
}

// reconcile: (1) pending approvals with no message → send; (2) unresolved
// rows whose approval is no longer pending → edit + resolve.
func (r *Registry) reconcile(ctx context.Context) {
	if len(r.readyChannels()) == 0 {
		return
	}
	pending, err := r.bus.ListPending(ctx)
	if err != nil {
		r.log.Warn("reconcile list pending", "err", err.Error())
		return
	}
	pendingIDs := map[string]struct{}{}
	for i := range pending {
		pendingIDs[pending[i].ID] = struct{}{}
		req := pending[i]
		r.send(ctx, &req)
	}
	// Direction 2: unresolved rows whose approval is no longer pending.
	rows, err := r.unresolvedMessages(ctx)
	if err != nil {
		r.log.Warn("reconcile scan unresolved", "err", err.Error())
		return
	}
	for _, m := range rows {
		if _, stillPending := pendingIDs[m.approvalID]; stillPending {
			continue
		}
		req, err := r.bus.Get(ctx, m.approvalID)
		if err != nil {
			// Approval row gone entirely — resolve the dangling chat row.
			r.markResolved(ctx, m.approvalID, m.channel)
			continue
		}
		r.update(ctx, req)
	}
}

func (r *Registry) channelByName(name string) Channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.channels {
		if ch.Name() == name {
			return ch
		}
	}
	return nil
}

// ---- chat_messages CRUD -----------------------------------------------------

type chatRow struct {
	approvalID string
	channel    string
	chatID     string
	messageID  string
	resolved   bool
}

func (r *Registry) hasMessage(ctx context.Context, approvalID, channel string) bool {
	var one int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM chat_messages WHERE approval_id=? AND channel=?`, approvalID, channel).Scan(&one)
	return err == nil
}

func (r *Registry) insertMessage(ctx context.Context, approvalID, channel string, ref MessageRef) {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_messages(approval_id, channel, chat_id, message_id, created_at)
         VALUES(?,?,?,?,?)`,
		approvalID, channel, ref.ChatID, ref.MessageID, time.Now().UnixMilli())
	if err != nil {
		r.log.Warn("insert chat_message", "err", err.Error())
	}
}

func (r *Registry) markResolved(ctx context.Context, approvalID, channel string) {
	_, _ = r.db.ExecContext(ctx,
		`UPDATE chat_messages SET resolved_at=? WHERE approval_id=? AND channel=? AND resolved_at IS NULL`,
		time.Now().UnixMilli(), approvalID, channel)
}

func (r *Registry) messagesFor(ctx context.Context, approvalID string) []chatRow {
	rows, err := r.db.QueryContext(ctx,
		`SELECT approval_id, channel, chat_id, message_id, resolved_at IS NOT NULL
         FROM chat_messages WHERE approval_id=?`, approvalID)
	if err != nil {
		return nil
	}
	return scanChatRows(rows)
}

func (r *Registry) unresolvedMessages(ctx context.Context) ([]chatRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT approval_id, channel, chat_id, message_id, 0
         FROM chat_messages WHERE resolved_at IS NULL`)
	if err != nil {
		return nil, err
	}
	return scanChatRows(rows), rows.Err()
}

func scanChatRows(rows *sql.Rows) []chatRow {
	defer rows.Close()
	var out []chatRow
	for rows.Next() {
		var m chatRow
		var resolved int
		if err := rows.Scan(&m.approvalID, &m.channel, &m.chatID, &m.messageID, &resolved); err != nil {
			return out
		}
		m.resolved = resolved != 0
		out = append(out, m)
	}
	return out
}

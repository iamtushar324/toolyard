package callbacks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
)

type PullEvent struct {
	EventID string            `json:"event_id"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// Pull authenticates through the API before entry. Reads never consume or
// claim events. The current receiver key signs the unchanged durable payload.
func (s *Service) Pull(ctx context.Context, agent, ref string) ([]PullEvent, error) {
	r, err := s.Get(ctx, agent, ref)
	if err != nil {
		return nil, err
	}
	if r.Status != "active" || r.Transport != "pull" {
		return nil, ErrReceiver
	}
	raw, err := s.cipher.Open(r.secret, []byte("callback|"+r.ID))
	if err != nil {
		return nil, err
	}
	key, err := secretBytes(string(raw))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.payload FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE o.receiver_id=? AND r.agent_id=? AND r.transport='pull' AND r.status='active' AND r.revision=? AND o.receiver_revision=r.revision AND o.status='pending' ORDER BY o.next_attempt_at,o.id LIMIT 10`, ref, agent, r.Revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PullEvent{}
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	for rows.Next() {
		var e PullEvent
		if err = rows.Scan(&e.EventID, &e.Body); err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(e.EventID + "." + timestamp + "." + e.Body))
		e.Headers = map[string]string{"webhook-id": e.EventID, "webhook-timestamp": timestamp, "webhook-signature": "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ack commits one delivery receipt after the receiver has persisted the event.
// A lost acknowledgement response can safely be repeated.
func (s *Service) Ack(ctx context.Context, agent, ref, event string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT o.status,o.attempts FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE o.id=? AND r.id=? AND r.agent_id=? AND r.status='active' AND r.transport='pull' AND o.receiver_revision=r.revision`, event, ref, agent).Scan(&status, &attempts)
	if err != nil {
		return ErrReceiver
	}
	if status == "delivered" {
		return nil
	}
	if status != "pending" {
		return ErrReceiver
	}
	now := s.now().UnixMilli()
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='delivered',attempts=attempts+1,delivered_at=?,terminal_reason='' WHERE id=? AND status='pending'`, now, event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO callback_attempts(event_id,attempt,attempted_at,http_status,outcome) VALUES(?,?,?,?,?)`, event, attempts+1, now, 200, "pull_acknowledged")
	if err != nil {
		return err
	}
	return tx.Commit()
}

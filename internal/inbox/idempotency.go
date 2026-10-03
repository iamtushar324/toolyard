package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxIdempotencyKey bounds an agent's opaque retry key.
const MaxIdempotencyKey = 200

// ErrIdempotencyConflict means a retry key already identifies different work.
var ErrIdempotencyConflict = errors.New("idempotency_key already identifies a different submission; reuse the original payload or use a new key")

func validateIdempotencyKey(key string) error {
	if key != "" && (strings.TrimSpace(key) == "" || utf8.RuneCountInString(key) > MaxIdempotencyKey) {
		return fmt.Errorf("must contain non-whitespace text and be at most %d characters", MaxIdempotencyKey)
	}
	return nil
}

func submissionHash(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

// replaySubmission reads the live request, without collecting any grant tokens.
// Its fingerprint stays independent of subsequent decisions and background checks.
func (s *Service) replaySubmission(ctx context.Context, agentID, key, hash string) (*SubmitResult, error) {
	var storedHash, doc string
	err := s.db.QueryRowContext(ctx, `SELECT submission_hash, doc FROM inbox_requests WHERE agent_id = ? AND idempotency_key = ?`, agentID, key).Scan(&storedHash, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedHash != hash {
		return nil, ErrIdempotencyConflict
	}
	var r Request
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil, err
	}
	return &SubmitResult{OK: true, Replayed: true, RequestID: r.ID, Status: r.Status,
		Problems: []Problem{}, Flags: toolFlags(&r), Preview: preview(&r), ExpiresAt: r.ExpiresAt}, nil
}

func (s *Service) replaySession(ctx context.Context, agentID, key, hash string) (*Session, error) {
	var storedHash, id string
	err := s.db.QueryRowContext(ctx, `SELECT submission_hash, id FROM agent_sessions WHERE agent_id = ? AND idempotency_key = ?`, agentID, key).Scan(&storedHash, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedHash != hash {
		return nil, ErrIdempotencyConflict
	}
	return s.GetSession(ctx, id)
}

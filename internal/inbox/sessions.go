package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Session statuses an agent can report.
const (
	SessionWorking        = "working"
	SessionWaitingOnOwner = "waiting_on_owner"
	SessionBlockedOnOwner = "blocked_on_owner"
	SessionIdle           = "idle"
	SessionDone           = "done"
)

var sessionStatuses = map[string]bool{SessionWorking: true, SessionWaitingOnOwner: true, SessionBlockedOnOwner: true, SessionIdle: true, SessionDone: true}

// Session is one named unit of agent work.
type Session struct {
	ID              string `json:"id"`
	AgentID         string `json:"agent_id"`
	Title           string `json:"title"`
	Repo            string `json:"repo,omitempty"`
	Branch          string `json:"branch,omitempty"`
	Host            string `json:"host,omitempty"`
	Status          string `json:"status"`
	Note            string `json:"note,omitempty"`
	StartedAt       int64  `json:"started_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	EndedAt         int64  `json:"ended_at,omitempty"`
}

// StartSession registers a new session for an agent.
func (s *Service) StartSession(ctx context.Context, agentID, title, repo, branch, host string) (*Session, error) {
	return s.StartSessionWithKey(ctx, agentID, title, repo, branch, host, "")
}

// StartSessionWithKey registers a session once per agent/key. A retry returns
// the current session without resetting its status or heartbeat.
func (s *Service) StartSessionWithKey(ctx context.Context, agentID, title, repo, branch, host, key string) (*Session, error) {
	if err := validateIdempotencyKey(key); err != nil {
		return nil, fmt.Errorf("idempotency_key: %w", err)
	}
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 120 {
		return nil, errors.New("title is required (at most 120 characters): what you're working on, as a person would say it")
	}
	repo, branch, host = strings.TrimSpace(repo), strings.TrimSpace(branch), strings.TrimSpace(host)
	var hash string
	if key != "" {
		var err error
		hash, err = submissionHash([]string{title, repo, branch, host})
		if err != nil {
			return nil, err
		}
		if replay, err := s.replaySession(ctx, agentID, key, hash); replay != nil || err != nil {
			return replay, err
		}
	}
	now := s.now().UnixMilli()
	ss := &Session{ID: "ses_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16], AgentID: agentID, Title: title,
		Repo: repo, Branch: branch, Host: host,
		Status: SessionWorking, StartedAt: now, LastHeartbeatAt: now}
	result, err := s.db.ExecContext(ctx, `INSERT INTO agent_sessions(id, agent_id, title, repo, branch, host, status, note, started_at, last_heartbeat_at, idempotency_key, submission_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_id, idempotency_key) DO NOTHING`, ss.ID, agentID, ss.Title, nullStr(ss.Repo), nullStr(ss.Branch), nullStr(ss.Host), ss.Status, nil, now, now, nullStr(key), nullStr(hash))
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return s.replaySession(ctx, agentID, key, hash)
	}
	s.publish("session", ss)
	return ss, nil
}

// UpdateSession records a status change (and doubles as a heartbeat).
func (s *Service) UpdateSession(ctx context.Context, agentID, id, status, note string) (*Session, error) {
	if !sessionStatuses[status] {
		return nil, fmt.Errorf("status must be one of working, waiting_on_owner, blocked_on_owner, idle, done")
	}
	if utf8.RuneCountInString(note) > 300 {
		return nil, errors.New("note: at most 300 characters")
	}
	ss, err := s.GetSession(ctx, id)
	if err != nil || ss.AgentID != agentID {
		return nil, ErrSessionNotYours
	}
	now := s.now().UnixMilli()
	var ended any
	if status == SessionDone {
		ended = now
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET status = ?, note = ?, last_heartbeat_at = ?, ended_at = COALESCE(?, ended_at) WHERE id = ?`,
		status, nullStr(strings.TrimSpace(note)), now, ended, id); err != nil {
		return nil, err
	}
	ss, err = s.GetSession(ctx, id)
	if err == nil {
		s.publish("session", ss)
	}
	return ss, err
}

// GetSession loads one session.
func (s *Service) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent_id, title, COALESCE(repo,''), COALESCE(branch,''), COALESCE(host,''), status,
		COALESCE(note,''), started_at, last_heartbeat_at, COALESCE(ended_at,0) FROM agent_sessions WHERE id = ?`, id)
	var ss Session
	if err := row.Scan(&ss.ID, &ss.AgentID, &ss.Title, &ss.Repo, &ss.Branch, &ss.Host, &ss.Status, &ss.Note, &ss.StartedAt, &ss.LastHeartbeatAt, &ss.EndedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &ss, nil
}

// ListSessions returns sessions that aren't done, or that ended in the last
// day, most recently active first.
func (s *Service) ListSessions(ctx context.Context) ([]Session, error) {
	since := s.now().UnixMilli() - 24*3600*1000
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent_id, title, COALESCE(repo,''), COALESCE(branch,''), COALESCE(host,''), status,
		COALESCE(note,''), started_at, last_heartbeat_at, COALESCE(ended_at,0) FROM agent_sessions
		WHERE ended_at IS NULL OR ended_at >= ? ORDER BY last_heartbeat_at DESC LIMIT 200`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var ss Session
		if err := rows.Scan(&ss.ID, &ss.AgentID, &ss.Title, &ss.Repo, &ss.Branch, &ss.Host, &ss.Status, &ss.Note, &ss.StartedAt, &ss.LastHeartbeatAt, &ss.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// HeartbeatSession bumps a session's last-seen time. The gateway calls it
// when a tool call is tagged with the session (`_session_id`).
func (s *Service) HeartbeatSession(ctx context.Context, sessionID string) {
	s.heartbeat(ctx, sessionID)
}

// Heartbeat bumps a session's last-seen time; called whenever the agent
// sends something tied to it.
func (s *Service) heartbeat(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agent_sessions SET last_heartbeat_at = ? WHERE id = ?`, s.now().UnixMilli(), sessionID)
}

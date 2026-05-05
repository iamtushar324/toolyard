// Package usage tracks per-(agent, tool) call counters and answers the
// top-N queries that drive agent-surface mode.
//
// Counters are incremented only on call.succeeded. Failures and denials do
// not count — top-N should reflect "what the agent actually uses,"
// not "what the agent tries and gives up on."
package usage

import (
	"context"
	"errors"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service { return &Service{db: db} }

// Row is one (agent, tool, count) tuple.
type Row struct {
	AgentID    string `json:"agent_id"`
	ToolName   string `json:"tool_name"`
	Count      int64  `json:"count"`
	LastUsedAt int64  `json:"last_used_at"`
}

// Increment bumps the counter for (agentID, toolName). agentID may be
// empty (stdio / unauthenticated).
func (s *Service) Increment(ctx context.Context, agentID, toolName string) error {
	if toolName == "" {
		return errors.New("tool name required")
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_usage(agent_id, tool_name, count, last_used_at)
         VALUES(?,?,1,?)
         ON CONFLICT(agent_id, tool_name) DO UPDATE
            SET count = count + 1, last_used_at = excluded.last_used_at`,
		agentID, toolName, now)
	return err
}

// TotalForAgent returns the sum of counts across all tools for one agent.
// Used to gate cold-start vs personalised top-N selection.
func (s *Service) TotalForAgent(ctx context.Context, agentID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(count), 0) FROM tool_usage WHERE agent_id = ?`,
		agentID).Scan(&n)
	return n, err
}

// TopForAgent returns the top n tool names by count for this agent
// (descending by count, then by last_used_at for stable tie-break).
func (s *Service) TopForAgent(ctx context.Context, agentID string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name FROM tool_usage WHERE agent_id = ?
         ORDER BY count DESC, last_used_at DESC LIMIT ?`,
		agentID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, n)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// TopOverall returns the top n tool names by SUM(count) across all agents.
// Cold-start fallback for new agents.
func (s *Service) TopOverall(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name FROM tool_usage
         GROUP BY tool_name
         ORDER BY SUM(count) DESC, MAX(last_used_at) DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, n)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// AggregatePerTool returns a flat (tool_name -> total count) map across all
// agents. Used by the dashboard's Tools tab "uses" column.
func (s *Service) AggregatePerTool(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name, SUM(count) FROM tool_usage GROUP BY tool_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// All returns counter rows, optionally filtered by agentID. Used by /v1/usage.
func (s *Service) All(ctx context.Context, agentID string) ([]Row, error) {
	q := `SELECT agent_id, tool_name, count, last_used_at FROM tool_usage`
	args := []any{}
	if agentID != "" {
		q += ` WHERE agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY count DESC, last_used_at DESC LIMIT 1000`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.AgentID, &r.ToolName, &r.Count, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

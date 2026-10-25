package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

const (
	ExecutionNotStarted = "not_started"
	ExecutionRunning    = "running"
	ExecutionSucceeded  = "succeeded"
	ExecutionFailed     = "failed"
	ExecutionUnknown    = "outcome_unknown"
)

type Execution struct {
	GrantID    string          `json:"grant_id"`
	RequestID  string          `json:"request_id"`
	CallID     string          `json:"call_id"`
	State      string          `json:"state"`
	AttemptID  string          `json:"attempt_id,omitempty"`
	StartedAt  int64           `json:"started_at,omitempty"`
	FinishedAt int64           `json:"finished_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func (s *Service) Execution(ctx context.Context, grantID, agentID string) (*Execution, error) {
	var e Execution
	var result sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT grant_id,request_id,call_id,state,attempt_id,started_at,COALESCE(finished_at,0),result,COALESCE(error,'') FROM inbox_executions WHERE grant_id=? AND agent_id=?`, grantID, agentID).Scan(&e.GrantID, &e.RequestID, &e.CallID, &e.State, &e.AttemptID, &e.StartedAt, &e.FinishedAt, &result, &e.Error)
	if errors.Is(err, sql.ErrNoRows) {
		g, _, ge := s.getGrantWithHash(ctx, grantID)
		if ge != nil || g.AgentID != agentID {
			return nil, ErrNotFound
		}
		return &Execution{GrantID: g.ID, RequestID: g.RequestID, CallID: g.CallID, State: ExecutionNotStarted}, nil
	}
	if err != nil {
		return nil, err
	}
	if result.Valid {
		e.Result = json.RawMessage(result.String)
	}
	return &e, nil
}

// FinishExecution records an outcome without releasing the consumed permission.
// Transport errors are uncertain: callers must not retry an upstream write.
func (s *Service) FinishExecution(ctx context.Context, g *Grant, state string, result any, detail string) error {
	if state != ExecutionSucceeded && state != ExecutionFailed && state != ExecutionUnknown {
		return fmt.Errorf("invalid execution outcome")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_executions SET state=?,finished_at=?,result=?,error=? WHERE grant_id=? AND agent_id=? AND attempt_id=? AND state=?`, state, s.now().UnixMilli(), string(encoded), nullStr(detail), g.ID, g.AgentID, g.AttemptID, ExecutionRunning)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	s.publish("execution", map[string]any{"request_id": g.RequestID, "call_id": g.CallID, "state": state})
	return nil
}

// RecoverExecutions records outcome_unknown for executions whose process
// stopped after the execution claim. No dispatch is repeated. It runs at
// startup (inbox.New) and, on a multi-node database, periodically on the
// leader (RunRecovery). With a single node every running execution belongs
// to a stopped process, as before; on a multi-node database it takes only
// executions whose lease ended (or that have none), so it never touches
// the live work of another node, nor of this one.
func (s *Service) RecoverExecutions(ctx context.Context) (int64, error) {
	q := `UPDATE inbox_executions SET state=?,finished_at=?,error=? WHERE state=?`
	args := []any{ExecutionUnknown, s.now().UnixMilli(), "process stopped after the execution claim; inspect upstream state before any new permission", ExecutionRunning}
	if s.multiNode() {
		q += ` AND (lease_until IS NULL OR lease_until < ?)`
		args = append(args, s.now().UnixMilli())
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	for i := int64(0); i < n; i++ {
		opsmetrics.IncOutcomeUnknown("inbox", "startup_recovery")
	}
	return n, err
}

// RunningExecutions returns the grant ids of this node's executions that
// have a claim and no outcome yet (on a single node: every one). Shutdown
// names them when they outlive the drain deadline; recovery records them
// outcome_unknown once their lease ends (at the next start on a single
// node).
func (s *Service) RunningExecutions(ctx context.Context) ([]string, error) {
	q := `SELECT grant_id FROM inbox_executions WHERE state=?`
	args := []any{ExecutionRunning}
	if s.multiNode() {
		q += ` AND owner_instance=?`
		args = append(args, s.instance())
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY started_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

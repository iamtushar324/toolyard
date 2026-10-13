package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// RecoverExecutions is called once at process startup. No dispatch is repeated.
func (s *Service) RecoverExecutions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_executions SET state=?,finished_at=?,error=? WHERE state=?`, ExecutionUnknown, s.now().UnixMilli(), "process stopped after the execution claim; inspect upstream state before any new permission", ExecutionRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

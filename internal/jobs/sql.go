package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
)

// jobColumns is the SELECT list scanJob reads (the inline result is read
// on its own by Result).
const jobColumns = `id, agent_id, owner_user_id, COALESCE(session_id,''), tool, upstream, params_hash, reason, via, status,
    progress, progress_total, COALESCE(progress_message,''), COALESCE(progress_at,0),
    COALESCE(result_blob,''), result_bytes, result_is_error, COALESCE(error,''), COALESCE(callback_ref,''),
    COALESCE(callback_error,''), COALESCE(idempotency_key,''), cancel_requested, owner_instance, lease_until,
    created_at, COALESCE(started_at,0), COALESCE(finished_at,0), updated_at, COALESCE(expires_at,0)`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (*Job, error) {
	var j Job
	var prog, total sql.NullFloat64
	var progMsg string
	var progAt, resultBytes, isErr, cancelReq int64
	if err := r.Scan(&j.ID, &j.AgentID, &j.OwnerUserID, &j.SessionID, &j.Tool, &j.Upstream, &j.ParamsHash, &j.Reason, &j.Via, &j.Status,
		&prog, &total, &progMsg, &progAt,
		&j.resultBlob, &resultBytes, &isErr, &j.Error, &j.CallbackRef,
		&j.CallbackError, &j.IdempotencyKey, &cancelReq, &j.OwnerInstance, &j.LeaseUntil,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.UpdatedAt, &j.ExpiresAt); err != nil {
		return nil, err
	}
	j.ResultBytes = int(resultBytes)
	j.ResultIsError = isErr != 0
	j.CancelRequested = cancelReq != 0
	if prog.Valid {
		p := &Progress{Progress: prog.Float64, Message: progMsg, At: progAt}
		if total.Valid {
			t := total.Float64
			p.Total = &t
		}
		j.Progress = p
	}
	return &j, nil
}

// dbNow is the database's clock in Unix milliseconds, as SQL: the
// cluster's work-lease clock (cluster.NowMillisSQL). Leases use it on every
// replica, so a replica whose clock drifts never ends another one's lease
// early.
func (s *Service) dbNow() string { return cluster.NowMillisSQL(s.db.Dialect) }

// capLockKey is the advisory lock the cap check and insert of req take on
// Postgres: the owner's, or the agent's when there is no owner.
func capLockKey(req StartRequest) int64 {
	h := fnv.New64a()
	if req.OwnerUserID != "" {
		_, _ = h.Write([]byte("tool_jobs:owner:" + req.OwnerUserID))
	} else {
		_, _ = h.Write([]byte("tool_jobs:agent:" + req.AgentID))
	}
	return int64(h.Sum64())
}

// BlobKeyPrefix is what a job's result blob key hashes before the result
// itself: blob key = sha256(BlobKeyPrefix(id) + result). The domain prefix
// keeps it from ever equalling an Inbox attachment's content hash, so the
// Inbox's sweeps never remove it (the store is shared on Postgres); the
// job id makes it the job's own, so one job's orphan cleanup or retention
// purge never deletes another job's identical result. dbcopy checks copied
// result files with it.
func BlobKeyPrefix(id string) string { return "tool_job\x00" + id + "\x00" }

// blobKey names job id's result in the blob store.
func blobKey(id, env string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(BlobKeyPrefix(id)))
	_, _ = h.Write([]byte(env))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Service) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cleanText makes s storable in a text column on both engines: valid
// UTF-8 without NUL.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "�")
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// newJobID returns "job_" and 32 hex characters.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("jobs: no randomness: %v", err))
	}
	return "job_" + hex.EncodeToString(b[:])
}

// defaultInstanceID names this process: host, process id and random hex.
func defaultInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "toolyard"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

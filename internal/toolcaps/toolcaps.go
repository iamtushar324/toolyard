// Package toolcaps keeps the admins' daily caps on successful tool calls
// (TEC-2060) and enforces them for the gateway (gateway.ToolCaps).
//
// A cap is set on one tool (its server and full catalog name): at most
// per_caller_daily successful calls by one caller and at most total_daily
// by everyone, each UTC day; 0 is no cap. Counters live in the database
// (tool_cap_usage), one row per day, tool and caller, plus the row of
// caller "*" that totals everyone, so a cap holds across replicas on
// Postgres as on SQLite.
//
// Counting is reserve-then-release: Reserve adds one to each counter in a
// single conditional upsert (the row only moves while it is below its
// limit), and the release it returns takes the one back when the call
// fails. A cap therefore never lets more calls through than its limit, even
// under concurrency or across replicas. While a call is in flight it holds
// its slot, so a concurrent call may be refused because of a call that then
// fails; a replica that dies mid-call leaves that one slot used for the
// rest of the day.
package toolcaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Cap and Usage are the gateway's types.
type (
	Cap   = gateway.ToolCap
	Usage = gateway.ToolCapUsage
)

// TotalCaller is the caller of the counter that totals everyone.
const TotalCaller = gateway.CapTotalCaller

// cacheTTL bounds how long a cap change made on another replica takes to
// apply here. A change on this replica applies at once.
const cacheTTL = 5 * time.Second

// releaseTimeout bounds the update that gives a slot back.
const releaseTimeout = 5 * time.Second

// DayLayout is the format of a UTC day.
const DayLayout = "2006-01-02"

// Day is t's UTC day.
func Day(t time.Time) string { return t.UTC().Format(DayLayout) }

// dayEnd is when t's UTC day ends: the next 00:00 UTC.
func dayEnd(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

type capKey struct{ upstream, tool string }

// Service implements gateway.ToolCaps.
type Service struct {
	db  *store.DB
	now func() time.Time

	mu       sync.Mutex
	caps     map[capKey]Cap
	loaded   bool
	loadedAt time.Time
}

var _ gateway.ToolCaps = (*Service)(nil)

// New returns the service on db.
func New(db *store.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

func (s *Service) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now()
}

// Today implements gateway.ToolCaps.
func (s *Service) Today() (string, time.Time) {
	now := s.clock()
	return Day(now), dayEnd(now)
}

// invalidate drops the cached caps, so the next call reads them again.
func (s *Service) invalidate() {
	s.mu.Lock()
	s.loaded = false
	s.mu.Unlock()
}

// capFor returns the cap on (upstream, tool) from the cache, reading the
// table again when the cache is older than cacheTTL. A read that fails
// keeps the last good copy; with none, the error is returned (the gateway
// then refuses the call rather than run a tool it cannot check).
func (s *Service) capFor(ctx context.Context, upstream, tool string) (Cap, bool, error) {
	s.mu.Lock()
	fresh := s.loaded && s.now().Sub(s.loadedAt) < cacheTTL
	if fresh {
		c, ok := s.caps[capKey{upstream, tool}]
		s.mu.Unlock()
		return c, ok, nil
	}
	s.mu.Unlock()

	list, err := s.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.caps == nil {
			return Cap{}, false, err
		}
		log.Printf("toolcaps: reading caps: %v (using the copy from %s)", err, s.loadedAt.UTC().Format(time.RFC3339))
		c, ok := s.caps[capKey{upstream, tool}]
		return c, ok, nil
	}
	m := make(map[capKey]Cap, len(list))
	for _, c := range list {
		m[capKey{c.Upstream, c.Tool}] = c
	}
	s.caps, s.loaded, s.loadedAt = m, true, s.now()
	c, ok := m[capKey{upstream, tool}]
	return c, ok, nil
}

// Reserve implements gateway.ToolCaps. Every capped tool keeps both
// counters (the caller's and the total), so an admin sees today's use of
// a limit that is 0 too.
func (s *Service) Reserve(ctx context.Context, upstream, tool string, callerFn func() string) (func(), *gateway.CapRefusal, error) {
	c, ok, err := s.capFor(ctx, upstream, tool)
	if err != nil {
		return nil, nil, fmt.Errorf("toolcaps: read caps: %w", err)
	}
	if !ok || (c.PerCallerDaily <= 0 && c.TotalDaily <= 0) {
		return nil, nil, nil
	}
	caller := callerFn()
	now := s.clock()
	day := Day(now)
	refusal := func(scope string, limit int64) *gateway.CapRefusal {
		return &gateway.CapRefusal{Upstream: upstream, Tool: tool, Scope: scope, Limit: limit, ResetAt: dayEnd(now)}
	}
	took, err := s.take(ctx, day, upstream, tool, caller, c.PerCallerDaily, now)
	if err != nil {
		return nil, nil, err
	}
	if !took {
		return nil, refusal(gateway.CapScopePerCaller, c.PerCallerDaily), nil
	}
	took, err = s.take(ctx, day, upstream, tool, TotalCaller, c.TotalDaily, now)
	if err != nil || !took {
		s.give(day, upstream, tool, caller)
		if err != nil {
			return nil, nil, err
		}
		return nil, refusal(gateway.CapScopeTotal, c.TotalDaily), nil
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.give(day, upstream, tool, caller)
			s.give(day, upstream, tool, TotalCaller)
		})
	}
	return release, nil, nil
}

// take adds one to a counter unless it already holds limit calls (limit
// 0: no limit). One statement, so concurrent takes, on any replica, never
// pass the limit: the conflicting row is locked and its WHERE is judged
// on the current value.
func (s *Service) take(ctx context.Context, day, upstream, tool, caller string, limit int64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO tool_cap_usage(day, upstream, tool, caller, calls, updated_at)
        VALUES(?, ?, ?, ?, 1, ?)
        ON CONFLICT(day, upstream, tool, caller) DO UPDATE SET calls = tool_cap_usage.calls + 1, updated_at = excluded.updated_at
        WHERE ? <= 0 OR tool_cap_usage.calls < ?`,
		day, upstream, tool, caller, now.UnixMilli(), limit, limit)
	if err != nil {
		return false, fmt.Errorf("toolcaps: count call: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("toolcaps: count call: %w", err)
	}
	// A new row (n=1) is always within a limit, which is at least 1.
	return n > 0, nil
}

// give takes back one call a take added, on its own short context: it
// runs when the call is over, whether or not the caller is still there.
func (s *Service) give(day, upstream, tool, caller string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE tool_cap_usage SET calls = calls - 1, updated_at = ?
        WHERE day = ? AND upstream = ? AND tool = ? AND caller = ? AND calls > 0`,
		s.clock().UnixMilli(), day, upstream, tool, caller); err != nil {
		log.Printf("toolcaps: give back a slot of %s for %s on %s: %v", tool, caller, day, err)
	}
}

// List implements gateway.ToolCaps: every cap, by server and tool.
func (s *Service) List(ctx context.Context) ([]Cap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT upstream, tool, per_caller_daily, total_daily, note,
        created_at, COALESCE(created_by, ''), updated_at, COALESCE(updated_by, '')
        FROM tool_caps ORDER BY upstream, tool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cap{}
	for rows.Next() {
		var c Cap
		if err := rows.Scan(&c.Upstream, &c.Tool, &c.PerCallerDaily, &c.TotalDaily, &c.Note,
			&c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns the stored cap on (upstream, tool).
func (s *Service) Get(ctx context.Context, upstream, tool string) (Cap, bool, error) {
	var c Cap
	err := s.db.QueryRowContext(ctx, `SELECT upstream, tool, per_caller_daily, total_daily, note,
        created_at, COALESCE(created_by, ''), updated_at, COALESCE(updated_by, '')
        FROM tool_caps WHERE upstream = ? AND tool = ?`, upstream, tool).
		Scan(&c.Upstream, &c.Tool, &c.PerCallerDaily, &c.TotalDaily, &c.Note, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Cap{}, false, nil
	}
	if err != nil {
		return Cap{}, false, err
	}
	return c, true, nil
}

// maxNoteLen bounds a cap's note.
const maxNoteLen = 500

// Set implements gateway.ToolCaps: it creates or replaces the cap on
// c.Upstream and c.Tool. Both limits 0 is refused (use Clear).
func (s *Service) Set(ctx context.Context, c Cap, by string) (Cap, error) {
	c.Upstream = strings.TrimSpace(c.Upstream)
	c.Tool = strings.TrimSpace(c.Tool)
	c.Note = strings.TrimSpace(c.Note)
	switch {
	case c.Upstream == "" || c.Tool == "":
		return Cap{}, fmt.Errorf("%w: server and tool are required", gateway.ErrInvalidCap)
	case c.PerCallerDaily < 0 || c.TotalDaily < 0:
		return Cap{}, fmt.Errorf("%w: a limit cannot be negative", gateway.ErrInvalidCap)
	case c.PerCallerDaily == 0 && c.TotalDaily == 0:
		return Cap{}, fmt.Errorf("%w: set per_caller_daily, total_daily or both (to remove a cap, clear it)", gateway.ErrInvalidCap)
	case len(c.Note) > maxNoteLen:
		return Cap{}, fmt.Errorf("%w: the note is longer than %d bytes", gateway.ErrInvalidCap, maxNoteLen)
	}
	now := s.clock().UnixMilli()
	var byArg any
	if by != "" {
		byArg = by
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tool_caps(upstream, tool, per_caller_daily, total_daily, note, created_at, created_by, updated_at, updated_by)
        VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(upstream, tool) DO UPDATE SET per_caller_daily = excluded.per_caller_daily,
            total_daily = excluded.total_daily, note = excluded.note,
            updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		c.Upstream, c.Tool, c.PerCallerDaily, c.TotalDaily, c.Note, now, byArg, now, byArg); err != nil {
		return Cap{}, err
	}
	s.invalidate()
	saved, _, err := s.Get(ctx, c.Upstream, c.Tool)
	return saved, err
}

// Clear implements gateway.ToolCaps. Today's counters stay, so the usage
// view still shows them; a new cap set the same day counts from them.
func (s *Service) Clear(ctx context.Context, upstream, tool string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tool_caps WHERE upstream = ? AND tool = ?`, upstream, tool)
	if err != nil {
		return false, err
	}
	s.invalidate()
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Usage implements gateway.ToolCaps: the counters of one UTC day, by
// server, tool and caller (the total, "*", first).
func (s *Service) Usage(ctx context.Context, day string) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, upstream, tool, caller, calls FROM tool_cap_usage
        WHERE day = ? ORDER BY upstream, tool, caller`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.Day, &u.Upstream, &u.Tool, &u.Caller, &u.Calls); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sorted here, byte-wise, not by the database's collation.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Upstream != b.Upstream {
			return a.Upstream < b.Upstream
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		if (a.Caller == TotalCaller) != (b.Caller == TotalCaller) {
			return a.Caller == TotalCaller
		}
		return a.Caller < b.Caller
	})
	return out, nil
}

// Prune deletes the counters of days before the last keepDays UTC days
// (today included). It returns how many rows it deleted.
func (s *Service) Prune(ctx context.Context, keepDays int) (int64, error) {
	if keepDays < 1 {
		keepDays = 1
	}
	oldest := Day(s.clock().AddDate(0, 0, -(keepDays - 1)))
	res, err := s.db.ExecContext(ctx, `DELETE FROM tool_cap_usage WHERE day < ?`, oldest)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Package autoapproval is the auto-approval engine: when an approval is
// about to be raised, it consults the rules table (and the metrics
// fingerprint state) to decide whether the request can be safely
// auto-decided without involving the human.
//
// Design summary (matching the plan in conversation):
//
//   - Three rule kinds, evaluated in this order:
//
//     1. static  : operator-pinned "always allow this exact thing".
//
//     2. pattern : learned per-(agent, fingerprint) — fires when the same
//     thing has been approved >= N times with 0 denials, and is not
//     in cool-off.
//
//     3. tool    : opt-in per-tool — fires when the tool's approval ratio
//     across all agents exceeds the configured threshold.
//
//   - Hard veto: tools with is_destructive=1 in tool_dim NEVER auto-approve
//     even if a rule matches.
//
//   - Cool-off: a denial sets cooloff_until on every matching rule, and
//     the engine refuses to auto-fire while cooled off.
//
//   - Rate limit: per-agent decisions per hour, capped by
//     AutoApprovalRateLimitPerHour. When tripped, the engine falls back to
//     human review until the next hour rolls over.
package autoapproval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Rule is the in-memory shape of a row in auto_approval_rules. Matches happen
// against a snapshot built every time the rule set changes.
type Rule struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`        // static | pattern | tool
	AgentID       string `json:"agent_id"`    // "" = any
	Fingerprint   string `json:"fingerprint"` // "" = any
	ToolName      string `json:"tool_name"`   // "" = any
	Enabled       bool   `json:"enabled"`
	Source        string `json:"source"` // user | proposer
	RationaleJSON string `json:"rationale_json,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	CoolOffUntil  int64  `json:"cool_off_until,omitempty"`
	HitCount      int64  `json:"hit_count"`
	LastHitTS     int64  `json:"last_hit_ts,omitempty"`
}

// Service wires the rules table, the metrics reader (for the rule proposer)
// and the settings cache.
type Service struct {
	db       *store.DB
	reader   *metrics.Reader
	settings *settings.Service

	mu    sync.RWMutex
	cache []Rule

	rateMu      sync.Mutex
	rateBuckets map[string]*rateBucket // agent -> bucket
}

type rateBucket struct {
	hourStart int64
	count     int
}

// New constructs the service and primes its rule cache. RunProposer must be
// started separately by the caller (so tests can opt out).
func New(db *store.DB, reader *metrics.Reader, set *settings.Service) *Service {
	s := &Service{
		db:          db,
		reader:      reader,
		settings:    set,
		rateBuckets: map[string]*rateBucket{},
	}
	if err := s.reload(context.Background()); err != nil {
		log.Printf("autoapproval: initial reload: %v", err)
	}
	return s
}

// reload reloads the in-memory rule cache from SQLite.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, COALESCE(agent_id,''),
        COALESCE(fingerprint,''), COALESCE(tool_name,''),
        enabled, source, COALESCE(rationale_json,''),
        created_at, COALESCE(cooloff_until,0), hit_count, COALESCE(last_hit_ts,0)
        FROM auto_approval_rules ORDER BY created_at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Kind, &r.AgentID, &r.Fingerprint, &r.ToolName,
			&enabled, &r.Source, &r.RationaleJSON, &r.CreatedAt, &r.CoolOffUntil,
			&r.HitCount, &r.LastHitTS); err != nil {
			return err
		}
		r.Enabled = enabled == 1
		out = append(out, r)
	}
	s.mu.Lock()
	s.cache = out
	s.mu.Unlock()
	return rows.Err()
}

// Match returns the first enabled rule that allows the supplied request, or
// nil if no rule matches. Implements the approval.AutoApprover.Match contract,
// so the return shape is the small *approval.AutoMatch tuple: callers don't
// need (and shouldn't have) the full rule body.
func (s *Service) Match(agentID, upstream, toolName, fingerprint string, isDestructive bool) *approval.AutoMatch {
	r := s.matchRule(agentID, toolName, fingerprint, isDestructive)
	if r == nil {
		return nil
	}
	return &approval.AutoMatch{ID: r.ID, Kind: r.Kind}
}

// matchRule is the private workhorse that evaluates rules in static → pattern
// → tool order, applying destructive veto, the global enabled flag, the
// per-rule cool-off, and the per-agent rate limit.
func (s *Service) matchRule(agentID, toolName, fingerprint string, isDestructive bool) *Rule {
	if isDestructive {
		return nil
	}
	if s.settings != nil && !s.settings.GetBool(settings.AutoApprovalEnabled) {
		return nil
	}
	if !s.allowedByRate(agentID) {
		return nil
	}

	now := time.Now().UnixMilli()
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Order: static, pattern, tool.
	for _, kind := range []string{"static", "pattern", "tool"} {
		for i := range s.cache {
			r := &s.cache[i]
			if r.Kind != kind {
				continue
			}
			if !r.Enabled {
				continue
			}
			if r.CoolOffUntil > 0 && r.CoolOffUntil > now {
				continue
			}
			if !ruleMatches(r, agentID, toolName, fingerprint) {
				continue
			}
			return r
		}
	}
	return nil
}

func ruleMatches(r *Rule, agentID, toolName, fingerprint string) bool {
	if r.AgentID != "" && r.AgentID != agentID {
		return false
	}
	if r.Fingerprint != "" && r.Fingerprint != fingerprint {
		return false
	}
	if r.ToolName != "" && r.ToolName != toolName {
		return false
	}
	switch r.Kind {
	case "static":
		// static rules require a fingerprint match (otherwise they degrade
		// into "auto-approve everything from this agent" which is unsafe).
		return r.Fingerprint != ""
	case "pattern":
		// pattern rules require fingerprint and at least one of (agent, tool).
		return r.Fingerprint != "" && (r.AgentID != "" || r.ToolName != "")
	case "tool":
		// tool rules require a tool match — they intentionally do not pin
		// to a fingerprint or agent.
		return r.ToolName != "" && r.Fingerprint == ""
	}
	return false
}

// allowedByRate reports whether the agent is under its per-hour cap. The
// counter is consumed in MarkHit, so Match itself doesn't burn budget; the
// caller is expected to invoke MarkHit only after acting on the rule.
func (s *Service) allowedByRate(agentID string) bool {
	if s.settings == nil {
		return true
	}
	cap := s.settings.GetInt(settings.AutoApprovalRateLimitPerHour, 100)
	if cap <= 0 {
		return true
	}
	now := time.Now()
	hourStart := now.Truncate(time.Hour).UnixMilli()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	b := s.rateBuckets[agentID]
	if b == nil || b.hourStart != hourStart {
		b = &rateBucket{hourStart: hourStart}
		s.rateBuckets[agentID] = b
	}
	return b.count < cap
}

// MarkHit increments the rule's hit counter, updates last_hit_ts, and bumps
// the rate bucket for the agent. Called after Decide() turns the auto rule
// into an actual approved approval.
func (s *Service) MarkHit(ctx context.Context, ruleID, agentID string) {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `UPDATE auto_approval_rules
        SET hit_count = hit_count + 1, last_hit_ts = ? WHERE id = ?`, now, ruleID); err != nil {
		log.Printf("autoapproval: mark hit: %v", err)
	}
	s.mu.Lock()
	for i := range s.cache {
		if s.cache[i].ID == ruleID {
			s.cache[i].HitCount++
			s.cache[i].LastHitTS = now
		}
	}
	s.mu.Unlock()

	hourStart := time.Now().Truncate(time.Hour).UnixMilli()
	s.rateMu.Lock()
	b := s.rateBuckets[agentID]
	if b == nil || b.hourStart != hourStart {
		b = &rateBucket{hourStart: hourStart}
		s.rateBuckets[agentID] = b
	}
	b.count++
	s.rateMu.Unlock()
}

// MarkDenial sets cool_off on every rule matching (agentID, fingerprint, tool)
// for the configured number of days, and disables matching pattern rules so
// the operator has to explicitly re-enable. Static rules are *not* disabled —
// they're operator-installed, so trust their author.
func (s *Service) MarkDenial(ctx context.Context, agentID, toolName, fingerprint string) {
	if s.settings == nil {
		return
	}
	days := s.settings.GetInt(settings.AutoApprovalCooloffDays, 30)
	if days <= 0 {
		days = 30
	}
	cool := time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
	_, err := s.db.ExecContext(ctx, `UPDATE auto_approval_rules
        SET cooloff_until = ?
        WHERE (fingerprint = ? OR (kind='tool' AND tool_name = ?))
          AND (agent_id IS NULL OR agent_id = '' OR agent_id = ?)`,
		cool, fingerprint, toolName, agentID)
	if err != nil {
		log.Printf("autoapproval: mark denial cool-off: %v", err)
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE auto_approval_rules
        SET enabled = 0
        WHERE kind = 'pattern' AND fingerprint = ?
          AND (agent_id IS NULL OR agent_id = '' OR agent_id = ?)`,
		fingerprint, agentID)
	if err := s.reload(ctx); err != nil {
		log.Printf("autoapproval: reload after denial: %v", err)
	}
}

// CreateOrUpdate writes a rule. If id is empty a new one is allocated.
func (s *Service) CreateOrUpdate(ctx context.Context, r Rule) (*Rule, error) {
	if r.ID == "" {
		r.ID = "ar_" + uuid.NewString()
	}
	if r.Source == "" {
		r.Source = "user"
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO auto_approval_rules(
        id, kind, agent_id, fingerprint, tool_name, enabled, source, rationale_json,
        created_at, cooloff_until, hit_count, last_hit_ts
    ) VALUES(?,?,?,?,?,?,?,?,?,?,0,0)
    ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, agent_id=excluded.agent_id,
        fingerprint=excluded.fingerprint, tool_name=excluded.tool_name,
        enabled=excluded.enabled, rationale_json=excluded.rationale_json,
        cooloff_until=excluded.cooloff_until`,
		r.ID, r.Kind, nullStr(r.AgentID), nullStr(r.Fingerprint), nullStr(r.ToolName),
		enabled, r.Source, nullStr(r.RationaleJSON), r.CreatedAt, nullInt(r.CoolOffUntil))
	if err != nil {
		return nil, err
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return &r, nil
}

// Enable sets enabled=1 on the rule and clears any existing cool-off so the
// operator's intent ("yes, this is fine") is unambiguous.
func (s *Service) Enable(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE auto_approval_rules
        SET enabled = 1, cooloff_until = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	return s.reload(ctx)
}

// Disable flips enabled to 0 — used for "revoke" and "dismiss".
func (s *Service) Disable(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE auto_approval_rules
        SET enabled = 0 WHERE id = ?`, id); err != nil {
		return err
	}
	return s.reload(ctx)
}

// Delete removes the row entirely; used for "dismiss forever" on suggested.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auto_approval_rules WHERE id = ?`, id); err != nil {
		return err
	}
	return s.reload(ctx)
}

// List returns every rule, regardless of enabled state. The dashboard splits
// active vs suggested by source/enabled in the UI.
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, len(s.cache))
	copy(out, s.cache)
	return out, nil
}

// Get returns one rule by ID.
func (s *Service) Get(ctx context.Context, id string) (*Rule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, COALESCE(agent_id,''),
        COALESCE(fingerprint,''), COALESCE(tool_name,''),
        enabled, source, COALESCE(rationale_json,''),
        created_at, COALESCE(cooloff_until,0), hit_count, COALESCE(last_hit_ts,0)
        FROM auto_approval_rules WHERE id = ?`, id)
	var r Rule
	var enabled int
	if err := row.Scan(&r.ID, &r.Kind, &r.AgentID, &r.Fingerprint, &r.ToolName,
		&enabled, &r.Source, &r.RationaleJSON, &r.CreatedAt, &r.CoolOffUntil,
		&r.HitCount, &r.LastHitTS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("rule %q not found", id)
		}
		return nil, err
	}
	r.Enabled = enabled == 1
	return &r, nil
}

// Propose runs the rule proposer pass: scans recent fingerprint stats and
// tool health, suggests new pattern / tool rules. New rows are written with
// enabled=0 so the operator must opt in.
func (s *Service) Propose(ctx context.Context) (int, error) {
	now := time.Now()
	from := now.Add(-30 * 24 * time.Hour)
	rg := metrics.Range{From: from, To: now}

	minApprovals := int64(s.settings.GetInt(settings.AutoApprovalPatternMinApprovals, 10))
	if minApprovals < 3 {
		minApprovals = 3
	}

	created := 0
	stats, err := s.reader.FingerprintStats(ctx, rg)
	if err != nil {
		return 0, err
	}
	for _, st := range stats {
		if st.AgentID == "" {
			continue
		}
		if st.Approved+st.AutoApproved < minApprovals {
			continue
		}
		if st.Denied != 0 {
			continue
		}
		// Skip if a matching rule already exists for this exact pair.
		if exists, _ := s.ruleExistsForFingerprint(ctx, st.AgentID, st.Fingerprint); exists {
			continue
		}
		rationale, _ := json.Marshal(map[string]any{
			"basis":           "pattern",
			"approved":        st.Approved + st.AutoApproved,
			"denied":          st.Denied,
			"window_days":     30,
			"last_decided_ts": st.LastDecidedTs,
			"tool_name":       st.ToolName,
		})
		_, err := s.CreateOrUpdate(ctx, Rule{
			Kind:          "pattern",
			AgentID:       st.AgentID,
			Fingerprint:   st.Fingerprint,
			ToolName:      st.ToolName,
			Enabled:       false, // opt-in
			Source:        "proposer",
			RationaleJSON: string(rationale),
		})
		if err != nil {
			log.Printf("autoapproval: propose pattern: %v", err)
			continue
		}
		created++
	}

	// Tool-wide proposals.
	toolHealth, err := s.reader.ToolHealth(ctx, rg)
	if err != nil {
		return created, err
	}
	for _, th := range toolHealth {
		if th.IsDestructive {
			continue
		}
		if th.Calls < 200 {
			continue
		}
		if th.ApprovalRatio < 0.98 {
			continue
		}
		if th.Denials > 0 {
			continue
		}
		if exists, _ := s.ruleExistsForTool(ctx, th.ToolName); exists {
			continue
		}
		rationale, _ := json.Marshal(map[string]any{
			"basis":           "tool",
			"calls":           th.Calls,
			"approval_ratio":  th.ApprovalRatio,
			"distinct_agents": th.DistinctAgents,
			"window_days":     30,
		})
		_, err := s.CreateOrUpdate(ctx, Rule{
			Kind:          "tool",
			ToolName:      th.ToolName,
			Enabled:       false,
			Source:        "proposer",
			RationaleJSON: string(rationale),
		})
		if err != nil {
			log.Printf("autoapproval: propose tool: %v", err)
			continue
		}
		created++
	}
	return created, nil
}

// RunProposer wakes every interval and runs Propose until the context is
// cancelled. Use a long interval (default 1h) — proposals are inherently
// stale-tolerant.
func (s *Service) RunProposer(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run once at startup so newly-installed deployments don't have to wait.
	if n, err := s.Propose(ctx); err == nil && n > 0 {
		log.Printf("autoapproval: proposed %d rules", n)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Propose(ctx); err != nil {
				log.Printf("autoapproval: proposer: %v", err)
			} else if n > 0 {
				log.Printf("autoapproval: proposed %d rules", n)
			}
		}
	}
}

func (s *Service) ruleExistsForFingerprint(ctx context.Context, agentID, fp string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM auto_approval_rules
        WHERE fingerprint = ? AND COALESCE(agent_id,'') = ? LIMIT 1`, fp, agentID)
	var x int
	if err := row.Scan(&x); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Service) ruleExistsForTool(ctx context.Context, tool string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM auto_approval_rules
        WHERE kind='tool' AND tool_name = ? LIMIT 1`, tool)
	var x int
	if err := row.Scan(&x); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// MarkToolDestructive lets the operator (or an automated heuristic) flag a
// tool so auto-approval will hard-veto it. Idempotent.
func (s *Service) MarkToolDestructive(ctx context.Context, toolName, upstream string, destructive bool) error {
	d := 0
	if destructive {
		d = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tool_dim(tool_name, upstream, is_destructive, last_seen_ts)
        VALUES(?,?,?,?)
        ON CONFLICT(tool_name) DO UPDATE SET is_destructive = excluded.is_destructive,
            upstream = excluded.upstream, last_seen_ts = excluded.last_seen_ts`,
		toolName, upstream, d, time.Now().UnixMilli())
	return err
}

// IsDestructive returns true if the tool has been marked destructive in
// tool_dim, or if its name matches one of the built-in heuristics
// (delete/drop/destroy/wipe/reset/force-push).
func (s *Service) IsDestructive(ctx context.Context, toolName string) bool {
	row := s.db.QueryRowContext(ctx, `SELECT is_destructive FROM tool_dim WHERE tool_name = ?`, toolName)
	var d int
	if err := row.Scan(&d); err == nil && d == 1 {
		return true
	}
	low := strings.ToLower(toolName)
	for _, marker := range destructiveMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// destructiveMarkers are name fragments that mark a tool as too risky to
// auto-approve absent an explicit operator override.
var destructiveMarkers = []string{
	"delete", "destroy", "drop", "wipe", "reset",
	"force_push", "force-push", "rm_rf", "purge",
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

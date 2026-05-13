// Package settings is a tiny key/value store backed by SQLite for
// system-wide toolyard configuration. Values are persisted as JSON so we can
// store booleans / strings / small structs uniformly. An in-memory cache is
// kept on top so the tool-filter hot path doesn't touch the DB on every
// tools/list.
package settings

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Known keys.
const (
	// SurfaceMode controls which tools the agent sees in tools/list.
	// One of "full" | "top_n" | "router_only". Defaults to "full".
	SurfaceMode = "surface_mode"
	// TopNCount is how many additional tools (beyond the pinned set) to
	// surface to each agent when surface_mode is "top_n". Default 20.
	TopNCount = "top_n_count"
	// TopNPersonalizeAfter is the per-agent total-call threshold that
	// switches an agent from the cold-start "overall top-N" to its own
	// "personal top-N." Default 100.
	TopNPersonalizeAfter = "top_n_personalize_after"

	// RouterOnlyMode is kept as a virtual alias for back-compat: writes are
	// translated into surface_mode = "router_only" / "full" and reads
	// reflect surface_mode == "router_only".
	RouterOnlyMode = "router_only_mode"

	// AutoApprovalEnabled toggles the auto-approval engine on/off globally.
	// Default false — the operator opts in once they're comfortable with
	// the audit trail.
	AutoApprovalEnabled = "auto_approval_enabled"
	// AutoApprovalPatternMinApprovals: how many human approvals (with zero
	// denials) of an exact (agent, fingerprint) pair are required before a
	// pattern rule will fire. Default 10.
	AutoApprovalPatternMinApprovals = "auto_approval_pattern_min_approvals"
	// AutoApprovalCooloffDays: a denial of a fingerprint locks any matching
	// auto-rule out of firing for this many days. Default 30.
	AutoApprovalCooloffDays = "auto_approval_cooloff_days"
	// AutoApprovalRateLimitPerHour: hard cap on auto-decisions per agent per
	// hour. Once tripped, the bus falls back to human approvals until the
	// hour rolls over. Default 100.
	AutoApprovalRateLimitPerHour = "auto_approval_rate_limit_per_hour"

	// MetricsRetentionDays: how many days of call_events to keep before the
	// retention compactor purges. Default 90.
	MetricsRetentionDays = "metrics_retention_days"
	// AnomalyRateZScore: how many standard deviations above the trailing
	// 7-day baseline triggers a rate_spike anomaly. Default 3.
	AnomalyRateZScore = "anomaly_rate_z_score"

	// CostInputUsdPerM: USD per 1M input tokens (default 0).
	CostInputUsdPerM = "cost_input_usd_per_m"
	// CostOutputUsdPerM: USD per 1M output tokens (default 0).
	CostOutputUsdPerM = "cost_output_usd_per_m"

	// LakeAPIToken is the static Bearer token external read-only consumers
	// (Grafana Infinity datasource, ad-hoc scripts) present to /v1/lake/*.
	// Generated on first gateway start if empty; rotatable from the UI.
	// Classified as a secret — never returned by All(), only by an
	// explicit reveal call.
	LakeAPIToken = "lake_api_token"
	// GrafanaOrigin, when non-empty, is added to the CSP frame-src
	// directive so the toolyard /lake/ page can iframe Grafana panels
	// from this origin (e.g. http://localhost:3030).
	GrafanaOrigin = "grafana_origin"
	// ClickhousePassword is the password for the `default` user of the
	// toolyard-clickhouse docker stack. Generated on first gateway start
	// if empty; rotatable from the UI. Toolyard renders it into a
	// runtime env file that the compose env_file directive consumes;
	// CH reads it at process start via the from_env="TOOLYARD_CH_PASSWORD"
	// reference in users.d/toolyard.xml.
	ClickhousePassword = "clickhouse_password"
)

// secretKeys lists settings whose values must not flow back through the
// generic GET /v1/settings response. Reads of these keys go through the
// dedicated Reveal() path so the dashboard can present a "show once"
// rotation flow with audit logging instead of a static token sitting in
// every page response.
var secretKeys = map[string]struct{}{
	LakeAPIToken:       {},
	ClickhousePassword: {},
}

// IsSecretKey reports whether key is classified as a secret.
func IsSecretKey(key string) bool {
	_, ok := secretKeys[key]
	return ok
}

// Surface modes.
const (
	SurfaceFull       = "full"
	SurfaceTopN       = "top_n"
	SurfaceRouterOnly = "router_only"
)

type Service struct {
	db *store.DB

	mu    sync.RWMutex
	cache map[string]json.RawMessage
}

func New(ctx context.Context, db *store.DB) (*Service, error) {
	s := &Service{db: db, cache: map[string]json.RawMessage{}}
	if err := s.reloadAll(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) reloadAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM system_settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cache := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		cache[k] = json.RawMessage(v)
	}
	s.mu.Lock()
	s.cache = cache
	s.mu.Unlock()
	return rows.Err()
}

// All returns a snapshot of every setting (decoded to interface{}). Used by
// the dashboard's GET /v1/settings. The result also fills in defaults +
// the virtual router_only_mode alias derived from surface_mode.
//
// Secret keys (see secretKeys) are not returned by value — instead a
// companion `<key>_present` boolean is included so the UI can render
// status without ever pulling the cleartext into a normal response.
// Use Reveal() to fetch the value for an explicit one-shot rotation flow.
func (s *Service) All(ctx context.Context) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.cache))
	for k, v := range s.cache {
		if IsSecretKey(k) {
			// Emit only the presence flag. A non-empty JSON string ("..."
			// is at minimum 2 bytes) means the operator has set it.
			out[k+"_present"] = len(v) > 2
			continue
		}
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			out[k] = string(v)
			continue
		}
		out[k] = x
	}
	// Always emit *_present for known secret keys so the UI can show a
	// "Not set" state without a separate request.
	for k := range secretKeys {
		flag := k + "_present"
		if _, ok := out[flag]; !ok {
			out[flag] = false
		}
	}
	// Defaults for the keys the UI expects to be present.
	if _, ok := out[SurfaceMode]; !ok {
		out[SurfaceMode] = SurfaceFull
	}
	if _, ok := out[TopNCount]; !ok {
		out[TopNCount] = float64(20)
	}
	if _, ok := out[TopNPersonalizeAfter]; !ok {
		out[TopNPersonalizeAfter] = float64(100)
	}
	// Virtual alias: keep router_only_mode boolean in sync for older callers.
	if mode, _ := out[SurfaceMode].(string); mode == SurfaceRouterOnly {
		out[RouterOnlyMode] = true
	} else {
		out[RouterOnlyMode] = false
	}
	return out, nil
}

// Reveal returns the cleartext value of a single setting. Intended for
// the dashboard's "show once" rotation flow on secret keys; for non-
// secret keys it's just GetString. Returns "" without error when the key
// is absent so the caller can distinguish empty from error.
func (s *Service) Reveal(_ context.Context, key string) (string, error) {
	if key == "" {
		return "", errors.New("key required")
	}
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return "", nil
	}
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return "", err
	}
	return str, nil
}

// EnsureLakeAPIToken makes sure lake_api_token is set, generating a fresh
// 32-byte random value on first boot. Returns the (current or newly
// minted) token plus a flag indicating whether a generation happened —
// the caller logs / surfaces this so the operator knows a one-time
// secret was created on their behalf.
func (s *Service) EnsureLakeAPIToken(ctx context.Context) (string, bool, error) {
	s.mu.RLock()
	v, ok := s.cache[LakeAPIToken]
	s.mu.RUnlock()
	if ok && len(v) > 2 {
		var existing string
		if err := json.Unmarshal(v, &existing); err == nil && existing != "" {
			return existing, false, nil
		}
	}
	tok, err := randomHex(32)
	if err != nil {
		return "", false, err
	}
	if err := s.Set(ctx, LakeAPIToken, tok); err != nil {
		return "", false, err
	}
	return tok, true, nil
}

// RotateLakeAPIToken replaces the existing token with a freshly generated
// one and returns it. Callers must follow up with whatever side effects
// the token's consumers expect (e.g. writing the runtime env file the
// Grafana container reads).
func (s *Service) RotateLakeAPIToken(ctx context.Context) (string, error) {
	tok, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if err := s.Set(ctx, LakeAPIToken, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// EnsureClickhousePassword mirrors EnsureLakeAPIToken for the CH stack:
// reads the existing password if any, mints one on first call, returns
// (current_or_new, was_generated, error). Hex output (64 chars) keeps it
// safe for shell-style env files — no quoting concerns.
func (s *Service) EnsureClickhousePassword(ctx context.Context) (string, bool, error) {
	s.mu.RLock()
	v, ok := s.cache[ClickhousePassword]
	s.mu.RUnlock()
	if ok && len(v) > 2 {
		var existing string
		if err := json.Unmarshal(v, &existing); err == nil && existing != "" {
			return existing, false, nil
		}
	}
	pw, err := randomHex(32)
	if err != nil {
		return "", false, err
	}
	if err := s.Set(ctx, ClickhousePassword, pw); err != nil {
		return "", false, err
	}
	return pw, true, nil
}

// RotateClickhousePassword replaces the existing CH password with a
// freshly generated one. Same caveat as RotateLakeAPIToken: the caller
// is responsible for re-rendering the runtime env file (and operationally
// for restarting the CH container so it re-reads it).
func (s *Service) RotateClickhousePassword(ctx context.Context) (string, error) {
	pw, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if err := s.Set(ctx, ClickhousePassword, pw); err != nil {
		return "", err
	}
	return pw, nil
}

// SetGrafanaOrigin validates and persists grafana_origin. We accept
// http(s):// origins only, with no path / query / fragment, so a stray
// trailing slash or full panel URL doesn't end up in the CSP header
// where it would either silently fail or mis-broaden the allow-list.
func (s *Service) SetGrafanaOrigin(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return s.Set(ctx, GrafanaOrigin, "")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("origin must use http or https")
	}
	if u.Host == "" {
		return errors.New("origin must include a host")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("origin must not include path/query/fragment")
	}
	canonical := u.Scheme + "://" + u.Host
	return s.Set(ctx, GrafanaOrigin, canonical)
}

// randomHex returns 2*n hex chars from crypto/rand. Used for the lake
// API token (n=32 -> 64 hex chars, 256 bits of entropy).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GetString reads a string setting from the in-memory cache, returning
// fallback if missing or wrong type.
func (s *Service) GetString(key, fallback string) string {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return fallback
	}
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return fallback
	}
	return str
}

// GetInt reads an integer setting from the in-memory cache.
func (s *Service) GetInt(key string, fallback int) int {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return fallback
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return fallback
	}
	return int(f)
}

// SurfaceModeOrDefault returns the surface_mode value, with a
// router_only_mode alias fallback for back-compat. When surface_mode is set,
// it always wins.
func (s *Service) SurfaceModeOrDefault() string {
	mode := s.GetString(SurfaceMode, "")
	if mode == SurfaceFull || mode == SurfaceTopN || mode == SurfaceRouterOnly {
		return mode
	}
	if s.GetBool(RouterOnlyMode) {
		return SurfaceRouterOnly
	}
	return SurfaceFull
}

// GetFloat reads a numeric setting as float64.
func (s *Service) GetFloat(key string, fallback float64) float64 {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return fallback
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return fallback
	}
	return f
}

// GetBool reads a boolean setting from the in-memory cache.
func (s *Service) GetBool(key string) bool {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

// Set persists value (must be JSON-serializable) and updates the cache.
func (s *Service) Set(ctx context.Context, key string, value any) error {
	if key == "" {
		return errors.New("key required")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?)
         ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(body), time.Now().UnixMilli()); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[key] = body
	s.mu.Unlock()
	return nil
}

// Patch merges a map of {key: value} updates in one go, rolled back on error.
// router_only_mode is normalised into surface_mode at write time so older
// callers continue working. Secret keys are rejected here — they have to
// flow through Rotate/Reveal so the audit trail and reveal-once UX stay
// consistent. grafana_origin is normalised through SetGrafanaOrigin to
// keep CSP-incompatible inputs out of the table.
func (s *Service) Patch(ctx context.Context, updates map[string]any) error {
	for k := range updates {
		if IsSecretKey(k) {
			return errors.New("secret keys are write-only via the rotate endpoint")
		}
	}
	if v, ok := updates[GrafanaOrigin]; ok {
		raw, _ := v.(string)
		if err := s.SetGrafanaOrigin(ctx, raw); err != nil {
			return err
		}
		delete(updates, GrafanaOrigin)
		if len(updates) == 0 {
			return nil
		}
	}
	updates = normalise(updates)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	type row struct {
		k string
		b []byte
	}
	rows := make([]row, 0, len(updates))
	for k, v := range updates {
		body, err := json.Marshal(v)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?)
             ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			k, string(body), now); err != nil {
			_ = tx.Rollback()
			return err
		}
		rows = append(rows, row{k, body})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range rows {
		s.cache[r.k] = r.b
	}
	s.mu.Unlock()
	return nil
}

// normalise rewrites virtual / aliased keys into their canonical form.
//   - router_only_mode: true  -> surface_mode = router_only
//   - router_only_mode: false -> surface_mode = full (only when surface_mode
//     isn't already in the same patch)
//   - top_n_count / top_n_personalize_after: clamped to sane ranges so a
//     bad input can't brick the UI.
func normalise(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	// router_only_mode aliasing.
	if _, sm := out[SurfaceMode]; !sm {
		if v, ok := out[RouterOnlyMode]; ok {
			if b, _ := v.(bool); b {
				out[SurfaceMode] = SurfaceRouterOnly
			} else {
				out[SurfaceMode] = SurfaceFull
			}
		}
	}
	delete(out, RouterOnlyMode)
	// Clamp surface_mode to a known value.
	if v, ok := out[SurfaceMode]; ok {
		s, _ := v.(string)
		switch s {
		case SurfaceFull, SurfaceTopN, SurfaceRouterOnly:
		default:
			out[SurfaceMode] = SurfaceFull
		}
	}
	// Clamp top_n_count to [1, 200].
	if v, ok := out[TopNCount]; ok {
		n := toInt(v)
		if n < 1 {
			n = 1
		}
		if n > 200 {
			n = 200
		}
		out[TopNCount] = n
	}
	// Clamp top_n_personalize_after to [0, 1_000_000].
	if v, ok := out[TopNPersonalizeAfter]; ok {
		n := toInt(v)
		if n < 0 {
			n = 0
		}
		if n > 1_000_000 {
			n = 1_000_000
		}
		out[TopNPersonalizeAfter] = n
	}
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	}
	return 0
}

// Compile-time check that the package compiles even if sql.ErrNoRows isn't
// referenced anywhere.
var _ = sql.ErrNoRows

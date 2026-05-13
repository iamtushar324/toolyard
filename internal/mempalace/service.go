// Package mempalace integrates the MemPalace local chat-memory MCP server
// (https://github.com/MemPalace/mempalace) into toolyard.
//
// Responsibilities:
//
//   - First-run bootstrap: make sure the `mempalace-mcp` binary is on PATH,
//     installing it via `uv tool install mempalace` (or `pipx`) when not.
//   - Initialize the palace directory on disk so the upstream process has
//     somewhere to write Chroma vectors and the knowledge-graph SQLite.
//   - Ingest: a thin wrapper over the gateway's internal-call path that
//     forwards POST /v1/mempalace/ingest payloads to mempalace's
//     diary_write tool, tagged with the calling agent's ID.
//
// The MCP-side plumbing — fork/connect, tool list, request routing — is all
// owned by the existing upstreams + gateway packages. This package adds the
// extra concerns that only apply to the built-in MemPalace integration.
package mempalace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// ToolPrefix is the upstream name MemPalace registers under, and therefore
// the prefix every wrapped tool carries (e.g. "mempalace.diary_write").
const ToolPrefix = "mempalace"

// BinaryName is the entry-point script `uv tool install mempalace` puts on
// PATH. Override via TOOLYARD_MEMPALACE_BIN for development against a
// non-published build.
const BinaryName = "mempalace-mcp"

// DiaryWriteTool is the canonical tool we ingest into. The upstream's own
// tools are themselves namespaced with a `mempalace_` prefix, so after
// toolyard's `<upstream>.<tool>` rewrap the final registered name is
// "mempalace.mempalace_diary_write".
const DiaryWriteTool = ToolPrefix + ".mempalace_diary_write"

// Mode controls install behavior at startup.
type Mode string

const (
	// ModeOff disables the integration entirely. No bootstrap, no upstream
	// registered, ingest endpoint returns 503.
	ModeOff Mode = "off"
	// ModeOn fails the boot if mempalace-mcp can't be made available.
	ModeOn Mode = "on"
	// ModeAuto installs if absent, logs and continues without if install
	// fails. Default — keeps a fresh `toolyard serve` working on machines
	// without `uv` while opportunistically lighting MemPalace up on the
	// ones that have it.
	ModeAuto Mode = "auto"
)

// ParseMode normalises a CLI flag value into a Mode. Unknown values fall
// back to ModeAuto.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "0", "false", "disabled":
		return ModeOff
	case "on", "1", "true", "enabled", "require", "required":
		return ModeOn
	default:
		return ModeAuto
	}
}

// Gateway is the slice of the gateway we depend on. Narrow on purpose so
// tests can supply a stub.
type Gateway interface {
	HasTool(name string) bool
	CallInternal(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error)
}

// Service owns mempalace lifecycle bits that don't naturally fit in the
// upstreams package.
type Service struct {
	db        *store.DB
	gw        Gateway
	auditLog  *audit.Logger
	dataDir   string
	palaceDir string
	binary    string
	mode      Mode

	mu        sync.Mutex
	installed bool // memoised so EnsureInstalled is cheap on retries
}

// IngestResult is the JSON envelope the HTTP handler returns to the agent.
type IngestResult struct {
	OK         bool   `json:"ok"`
	ApprovalID string `json:"approval_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Raw        any    `json:"raw,omitempty"`
}

// New builds a Service. `dataDir` is toolyard's data dir (the same one used
// for toolyard.db). The palace lives under `<dataDir>/palace` so backups of
// the data dir capture it.
func New(db *store.DB, gw Gateway, auditLog *audit.Logger, dataDir string, mode Mode) *Service {
	if mode == "" {
		mode = ModeAuto
	}
	bin := os.Getenv("TOOLYARD_MEMPALACE_BIN")
	if bin == "" {
		bin = BinaryName
	}
	return &Service{
		db:        db,
		gw:        gw,
		auditLog:  auditLog,
		dataDir:   dataDir,
		palaceDir: filepath.Join(dataDir, "palace"),
		binary:    bin,
		mode:      mode,
	}
}

// Mode reports the configured mode (used by the HTTP handler to short-circuit
// when disabled).
func (s *Service) Mode() Mode { return s.mode }

// Binary returns the resolved binary name/path (after honoring
// TOOLYARD_MEMPALACE_BIN).
func (s *Service) Binary() string { return s.binary }

// PalaceDir returns the on-disk palace directory.
func (s *Service) PalaceDir() string { return s.palaceDir }

// Disabled reports whether the integration is off. The HTTP handler and the
// startup wiring both check this so the same predicate decides every code path.
func (s *Service) Disabled() bool { return s == nil || s.mode == ModeOff }

// EnsureInstalled makes sure the mempalace-mcp binary is reachable. When it
// is already on PATH (or TOOLYARD_MEMPALACE_BIN points at a real file) this
// is a no-op. When absent, ModeAuto/ModeOn attempt an install via:
//
//	uv tool install mempalace      (preferred — what upstream documents)
//	pipx install mempalace         (fallback)
//
// Other install vectors are intentionally skipped; toolyard isn't a package
// manager. ModeOff is a no-op.
func (s *Service) EnsureInstalled(ctx context.Context) error {
	if s.Disabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installed {
		return nil
	}
	if _, err := exec.LookPath(s.binary); err == nil {
		s.installed = true
		return nil
	}
	// If the operator pointed us at a non-PATH path, accept it as long as
	// the file exists and is executable.
	if filepath.IsAbs(s.binary) {
		if info, err := os.Stat(s.binary); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			s.installed = true
			return nil
		}
	}

	installers := []struct {
		name string
		args []string
	}{
		{"uv", []string{"tool", "install", "mempalace"}},
		{"pipx", []string{"install", "mempalace"}},
	}
	var lastErr error
	for _, in := range installers {
		if _, err := exec.LookPath(in.name); err != nil {
			continue
		}
		// Install is allowed to take a while on a cold pip cache.
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		cmd := exec.CommandContext(runCtx, in.name, in.args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w (stderr: %s)", in.name, strings.Join(in.args, " "), err, strings.TrimSpace(stderr.String()))
			continue
		}
		if s.auditLog != nil {
			_ = s.auditLog.Write(ctx, audit.Event{
				EventType:     "mempalace.install",
				UpstreamName:  ToolPrefix,
				ResultSummary: fmt.Sprintf("installed via %s", in.name),
			})
		}
		// uv tool install drops the binary into ~/.local/bin (or the
		// platform equivalent) which may not be on the gateway's PATH.
		// Re-probe after install and, if still missing, fall back to a
		// known install location.
		if _, err := exec.LookPath(s.binary); err == nil {
			s.installed = true
			return nil
		}
		if home, herr := os.UserHomeDir(); herr == nil {
			candidate := filepath.Join(home, ".local", "bin", BinaryName)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				s.binary = candidate
				s.installed = true
				return nil
			}
		}
		// Fell through: install reported success but binary is still not
		// findable. Treat as a soft failure under ModeAuto.
		lastErr = fmt.Errorf("%s reported success but %s is not on PATH or in ~/.local/bin", in.name, BinaryName)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("neither uv nor pipx is on PATH; install one and re-run, or set TOOLYARD_MEMPALACE_BIN to a prebuilt binary")
	}
	if s.mode == ModeOn {
		return lastErr
	}
	// ModeAuto: log + continue. The dashboard's MemPalace card will show
	// the install-pending state via the lack of a registered upstream.
	if s.auditLog != nil {
		_ = s.auditLog.Write(ctx, audit.Event{
			EventType:     "mempalace.install_failed",
			UpstreamName:  ToolPrefix,
			ResultSummary: lastErr.Error(),
		})
	}
	return nil
}

// EnsureInitialized creates the palace directory if missing. MemPalace runs
// `mempalace-mcp init <path>` if it sees an empty directory on first
// connect; we pre-create the directory so the process has a stable target
// and dashboard backups always include it.
func (s *Service) EnsureInitialized(_ context.Context) error {
	if s.Disabled() {
		return nil
	}
	if err := os.MkdirAll(s.palaceDir, 0o700); err != nil {
		return fmt.Errorf("create palace dir: %w", err)
	}
	return nil
}

// Available reports whether the integration is currently usable: enabled,
// installed, and the diary_write tool is registered in the gateway catalog.
// The HTTP handler uses this to decide between 200/202 and 503.
func (s *Service) Available() bool {
	if s.Disabled() {
		return false
	}
	if s.gw == nil {
		return false
	}
	return s.gw.HasTool(DiaryWriteTool)
}

// Ingest forwards a chat-memory payload to mempalace.diary_write through
// the gateway's internal call path. It records a row in mempalace_agents so
// the dashboard can list active agents without walking the audit log.
//
// On a successful call the result envelope from MemPalace is returned
// verbatim under .Raw, and .OK reflects whether the upstream signalled an
// error. Approval is intentionally bypassed: chat capture must not block on
// a human tap, and the call is sourced from an authenticated agent via the
// trusted /v1/mempalace/ingest endpoint.
func (s *Service) Ingest(ctx context.Context, agentID, entry, topic, wing string) (*IngestResult, error) {
	if s.Disabled() {
		return nil, ErrDisabled
	}
	if !s.Available() {
		return nil, ErrNotReady
	}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil, ErrEntryRequired
	}
	args := map[string]any{
		"entry": entry,
	}
	if topic != "" {
		args["topic"] = topic
	}
	if wing != "" {
		args["wing"] = wing
	}
	if agentID != "" {
		// MemPalace's diary primitive accepts an agent_name tag that gets
		// stored alongside each entry so search results can be filtered by
		// who said what.
		args["agent_name"] = agentID
	}

	res, err := s.gw.CallInternal(ctx, "mempalace.ingest", DiaryWriteTool, args)
	if err != nil {
		return nil, err
	}
	out := &IngestResult{OK: !res.IsError}
	if !out.OK {
		out.Detail = summariseToolResult(res)
	} else {
		out.Detail = summariseToolResult(res)
	}
	if res.StructuredContent != nil {
		out.Raw = res.StructuredContent
	}
	if out.OK {
		s.recordAgent(ctx, agentID)
	}
	return out, nil
}

// recordAgent upserts a row in mempalace_agents. Best-effort: a write
// failure must not break ingest, so errors are swallowed.
func (s *Service) recordAgent(ctx context.Context, agentID string) {
	if s.db == nil || strings.TrimSpace(agentID) == "" {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO mempalace_agents(agent_id, first_seen, last_seen, entry_count)
         VALUES(?, ?, ?, 1)
         ON CONFLICT(agent_id) DO UPDATE SET
            last_seen = excluded.last_seen,
            entry_count = entry_count + 1`,
		agentID, now, now)
}

// AgentRow summarises one entry from mempalace_agents for the dashboard.
type AgentRow struct {
	AgentID    string `json:"agent_id"`
	FirstSeen  int64  `json:"first_seen"`
	LastSeen   int64  `json:"last_seen"`
	EntryCount int64  `json:"entry_count"`
}

// TopAgents returns the most active agents (by entry_count). limit <= 0
// defaults to 10.
func (s *Service) TopAgents(ctx context.Context, limit int) ([]AgentRow, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_id, first_seen, last_seen, entry_count
         FROM mempalace_agents
         ORDER BY entry_count DESC, last_seen DESC
         LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var r AgentRow
		if err := rows.Scan(&r.AgentID, &r.FirstSeen, &r.LastSeen, &r.EntryCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Status is the small JSON blob the dashboard polls so it can render a
// MemPalace card without firing several round-trips.
type Status struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	Installed  bool   `json:"installed"`
	Available  bool   `json:"available"`
	PalaceDir  string `json:"palace_dir"`
	Binary     string `json:"binary"`
	AgentCount int    `json:"agent_count"`
}

// Snapshot returns a Status object describing the current integration state.
func (s *Service) Snapshot(ctx context.Context) Status {
	st := Status{
		Mode:      string(s.mode),
		Enabled:   !s.Disabled(),
		Installed: s.binaryInstalled(),
		Available: s.Available(),
		PalaceDir: s.palaceDir,
		Binary:    s.binary,
	}
	if s.db != nil {
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mempalace_agents`).Scan(&st.AgentCount)
	}
	return st
}

func (s *Service) binaryInstalled() bool {
	if s == nil {
		return false
	}
	if _, err := exec.LookPath(s.binary); err == nil {
		return true
	}
	if filepath.IsAbs(s.binary) {
		info, err := os.Stat(s.binary)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	return false
}

// Errors surfaced to API callers.
var (
	ErrDisabled      = errors.New("mempalace integration is disabled (-mempalace=off)")
	ErrNotReady      = errors.New("mempalace upstream not connected yet")
	ErrEntryRequired = errors.New("entry is required")
)

func summariseToolResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			s := strings.TrimSpace(t.Text)
			if len(s) > 400 {
				return s[:400] + "…"
			}
			return s
		}
	}
	return ""
}

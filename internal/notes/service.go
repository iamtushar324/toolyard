// Package notes wires the toolyard "notes" markdown workspace to the
// MemPalace semantic store. Two flows:
//
//   - Agent calls `notes.publish(path, content, [topic])` — we write the
//     file (via the filesystem MCP upstream registered as "notes") and
//     index its body into MemPalace's diary in a single internal call,
//     bypassing the human approval gate. Same logic as
//     /v1/mempalace/ingest: capture must not block on a human tap.
//
//   - Background scanner walks $NOTES_DIR every N seconds and ingests
//     any file whose mtime advanced since the last sync. Covers anything
//     dropped by humans (vim, git pull, scp, syncthing) so the palace
//     stays current without the writer knowing toolyard exists.
//
// State lives in `notes_sync` (one row per relative path) so re-syncs
// are O(changed files), not O(everything).
package notes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Dispatcher is the gateway behaviour we depend on. Narrow on purpose so
// tests can fake it without spinning up an MCP server.
type Dispatcher interface {
	HasTool(name string) bool
	CallInternal(ctx context.Context, viaTool, target string, args map[string]any) (*mcp.CallToolResult, error)
}

// Service owns the notes-dir lifecycle and the in-process sync state.
type Service struct {
	db       *store.DB
	gw       Dispatcher
	notesDir string
	interval time.Duration
	enabled  bool

	// Tool names we dispatch to. Hardcoded here rather than imported from
	// internal/mempalace to keep this package free of that dependency —
	// notes can be useful even when mempalace is off.
	writeTool string // e.g. "notes.write_file"
	diaryTool string // e.g. "mempalace.mempalace_diary_write"

	mu      sync.Mutex
	syncing bool // serialize Sync() so two ticks can't overlap
}

// Defaults used when callers don't override.
const (
	DefaultDiaryTool = "mempalace.mempalace_diary_write"
	DefaultWriteTool = "notes.write_file"
	DefaultScanEvery = 30 * time.Second
	maxIngestBytes   = 64 << 10 // 64 KiB per file — entries larger than this are truncated for the diary; the file on disk is untouched
)

// Options controls Service construction.
type Options struct {
	DB       *store.DB
	Gateway  Dispatcher
	NotesDir string
	// Interval between scanner ticks. Zero falls back to DefaultScanEvery.
	// Negative disables the scanner (Publish still works).
	Interval time.Duration
	// Optional overrides for the underlying tools — useful for tests.
	WriteTool string
	DiaryTool string
}

// New builds a Service. Returns nil if NotesDir is empty (caller can treat
// nil as "feature disabled" without nil-checks at every callsite — every
// public method on Service is safe on nil).
func New(opts Options) *Service {
	if strings.TrimSpace(opts.NotesDir) == "" {
		return nil
	}
	interval := opts.Interval
	if interval == 0 {
		interval = DefaultScanEvery
	}
	write := opts.WriteTool
	if write == "" {
		write = DefaultWriteTool
	}
	diary := opts.DiaryTool
	if diary == "" {
		diary = DefaultDiaryTool
	}
	notesDir := filepath.Clean(opts.NotesDir)
	// Resolve symlinks so paths we hand to the filesystem MCP upstream
	// match what it normalises internally. Necessary on macOS where
	// /tmp is a symlink to /private/tmp and the upstream's allowed-dirs
	// check rejects the un-resolved form. On Linux this is a no-op for
	// the standard /var/lib/toolyard/notes case.
	if resolved, err := filepath.EvalSymlinks(notesDir); err == nil && resolved != "" {
		notesDir = resolved
	}
	return &Service{
		db:        opts.DB,
		gw:        opts.Gateway,
		notesDir:  notesDir,
		interval:  interval,
		enabled:   true,
		writeTool: write,
		diaryTool: diary,
	}
}

// NotesDir returns the configured directory. Useful for log lines.
func (s *Service) NotesDir() string {
	if s == nil {
		return ""
	}
	return s.notesDir
}

// Enabled reports whether the service is wired up. Nil-safe.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// Errors surfaced to callers.
var (
	ErrDisabled     = errors.New("notes service is disabled (-notes=off or no notes-dir)")
	ErrPathEscape   = errors.New("path escapes the notes directory")
	ErrPathRequired = errors.New("path is required")
	ErrEmptyContent = errors.New("content is required")
)

// safeJoin resolves rel against the notes dir and refuses any path that
// escapes (..) or starts with an absolute / outside-the-dir prefix.
// Returns the absolute path and the cleaned relative path used as the
// notes_sync primary key.
func (s *Service) safeJoin(rel string) (abs, relClean string, err error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", "", ErrPathRequired
	}
	// Accept an absolute path only when it explicitly resolves under
	// $NOTES_DIR (so a caller pasting back what notes.publish returned
	// keeps working). Any other absolute path is rejected — we don't
	// want "/etc/passwd" silently rewritten to "$NOTES_DIR/etc/passwd".
	if filepath.IsAbs(rel) {
		clean := filepath.Clean(rel)
		rc, rerr := filepath.Rel(s.notesDir, clean)
		if rerr != nil || rc == "." || strings.HasPrefix(rc, "..") || filepath.IsAbs(rc) {
			return "", "", ErrPathEscape
		}
		rel = rc
	}
	if rel == "" {
		return "", "", ErrPathRequired
	}
	cand := filepath.Clean(filepath.Join(s.notesDir, rel))
	rc, rerr := filepath.Rel(s.notesDir, cand)
	if rerr != nil || rc == "." || strings.HasPrefix(rc, "..") || filepath.IsAbs(rc) {
		return "", "", ErrPathEscape
	}
	return cand, rc, nil
}

// PublishResult is what notes.publish returns to the caller.
type PublishResult struct {
	Path       string `json:"path"` // relative to notes dir
	Bytes      int    `json:"bytes"`
	Indexed    bool   `json:"indexed"` // whether mempalace ingested it
	PalaceID   string `json:"palace_id,omitempty"`
	IndexError string `json:"index_error,omitempty"`
}

// Publish writes content to <notesDir>/<path> via the filesystem MCP and
// then ingests the body into mempalace. Indexing is best-effort: a write
// success with an index failure still returns OK so the file persists,
// and the next scanner tick will retry the ingest.
func (s *Service) Publish(ctx context.Context, agentID, path, content, topic string) (*PublishResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(content) == "" {
		return nil, ErrEmptyContent
	}
	abs, rel, err := s.safeJoin(path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir parent: %w", err)
	}
	// Write via the filesystem MCP if registered, otherwise directly to
	// disk. Going through the upstream gives consistent path-policy
	// behaviour with notes.write_file invoked directly; the os fallback
	// keeps the tool useful when the upstream hasn't connected yet
	// (e.g., during the first ~5s after boot while npx is fetching).
	if s.gw != nil && s.gw.HasTool(s.writeTool) {
		args := map[string]any{"path": abs, "content": content}
		res, derr := s.gw.CallInternal(ctx, "notes.publish", s.writeTool, args)
		if derr != nil {
			return nil, fmt.Errorf("write via %s: %w", s.writeTool, derr)
		}
		if res != nil && res.IsError {
			return nil, fmt.Errorf("write via %s: %s", s.writeTool, summariseResult(res))
		}
	} else {
		if err := os.WriteFile(abs, []byte(content), 0o640); err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}
	}

	out := &PublishResult{Path: rel, Bytes: len(content)}

	// Best-effort mempalace ingest.
	palaceID, ierr := s.ingest(ctx, agentID, rel, content, topic)
	if ierr != nil {
		out.IndexError = ierr.Error()
	} else {
		out.Indexed = true
		out.PalaceID = palaceID
	}

	// Record state regardless of index outcome — file is on disk, mtime
	// tracks the write. If the ingest failed, palace_id is empty and the
	// scanner will retry on the next tick (mtime equals last-recorded
	// mtime, BUT palace_id is empty → we re-ingest).
	if info, err := os.Stat(abs); err == nil {
		s.recordSync(ctx, rel, info.ModTime().UnixMilli(), palaceID)
	}
	return out, nil
}

// Sync walks $NOTES_DIR and ingests any markdown file whose mtime
// advanced (or whose previous ingest failed). Returns the number of
// files ingested in this pass. Safe to call concurrently — the second
// caller short-circuits.
func (s *Service) Sync(ctx context.Context) (int, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		return 0, nil
	}
	s.syncing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.syncing = false
		s.mu.Unlock()
	}()

	if s.gw == nil || !s.gw.HasTool(s.diaryTool) {
		return 0, nil // mempalace not registered yet; try next tick
	}

	known, err := s.loadSyncIndex(ctx)
	if err != nil {
		return 0, err
	}

	var ingested int
	walkErr := filepath.WalkDir(s.notesDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			// Skip hidden subtrees (.git, .obsidian, etc).
			base := filepath.Base(path)
			if path != s.notesDir && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isMarkdown(d.Name()) {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(s.notesDir, path)
		if rerr != nil {
			return nil
		}
		mtime := info.ModTime().UnixMilli()
		prev, ok := known[rel]
		if ok && prev.mtime == mtime && prev.palaceID != "" {
			return nil // unchanged + already indexed
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		palaceID, ierr := s.ingest(ctx, "notes-sync", rel, string(body), "")
		if ierr != nil {
			log.Printf("notes-sync: %s: %v", rel, ierr)
			return nil
		}
		s.recordSync(ctx, rel, mtime, palaceID)
		ingested++
		return nil
	})
	if walkErr != nil {
		return ingested, walkErr
	}
	return ingested, nil
}

// StartScanner runs Sync on a ticker until ctx is cancelled. Returns
// immediately when the service is disabled. Errors are logged, never
// propagated — a flaky tick must not crash the gateway.
func (s *Service) StartScanner(ctx context.Context) {
	if !s.Enabled() || s.interval < 0 {
		return
	}
	// First tick after a small delay so the mempalace upstream has time
	// to come up after gateway boot.
	first := time.NewTimer(5 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	if n, err := s.Sync(ctx); err != nil {
		log.Printf("notes-sync: initial: %v", err)
	} else if n > 0 {
		log.Printf("notes-sync: initial: ingested %d file(s)", n)
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sync(ctx); err != nil {
				log.Printf("notes-sync: %v", err)
			} else if n > 0 {
				log.Printf("notes-sync: ingested %d file(s)", n)
			}
		}
	}
}

// ingest forwards one file's body to mempalace.mempalace_diary_write via
// the gateway's internal call path. Returns the palace entry_id when the
// upstream returns one (it does, as JSON in the text content).
func (s *Service) ingest(ctx context.Context, agentID, rel, content, topic string) (string, error) {
	if len(content) > maxIngestBytes {
		// Truncate; mark the truncation so a searcher reading the
		// stored entry knows there's more on disk.
		content = content[:maxIngestBytes] + "\n\n[…truncated by toolyard notes-sync; read the file for the full body]"
	}
	if topic == "" {
		// Topic defaults to the file's parent directory under notes/
		// (e.g. "architecture/decisions.md" → topic "architecture").
		if dir := filepath.Dir(rel); dir != "." {
			topic = dir
		} else {
			topic = "notes"
		}
	}
	args := map[string]any{
		"entry":      "[" + rel + "]\n\n" + content,
		"topic":      topic,
		"wing":       "notes",
		"agent_name": agentID,
	}
	res, err := s.gw.CallInternal(ctx, "notes.publish", s.diaryTool, args)
	if err != nil {
		return "", err
	}
	if res != nil && res.IsError {
		return "", fmt.Errorf("%s", summariseResult(res))
	}
	return extractPalaceID(res), nil
}

// recordSync upserts a notes_sync row. Best-effort.
func (s *Service) recordSync(ctx context.Context, rel string, mtime int64, palaceID string) {
	if s.db == nil {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO notes_sync(path, mtime, palace_id, synced_at)
         VALUES(?, ?, ?, ?)
         ON CONFLICT(path) DO UPDATE SET
            mtime = excluded.mtime,
            palace_id = COALESCE(NULLIF(excluded.palace_id, ''), notes_sync.palace_id),
            synced_at = excluded.synced_at`,
		rel, mtime, nullString(palaceID), now)
}

type syncRow struct {
	mtime    int64
	palaceID string
}

func (s *Service) loadSyncIndex(ctx context.Context) (map[string]syncRow, error) {
	out := map[string]syncRow{}
	if s.db == nil {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, mtime, COALESCE(palace_id,'') FROM notes_sync`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, palace string
		var mtime int64
		if err := rows.Scan(&path, &mtime, &palace); err != nil {
			return nil, err
		}
		out[path] = syncRow{mtime: mtime, palaceID: palace}
	}
	return out, rows.Err()
}

// Status is the lightweight snapshot the dashboard polls.
type Status struct {
	Enabled    bool   `json:"enabled"`
	NotesDir   string `json:"notes_dir"`
	Interval   string `json:"interval"`
	Tracked    int    `json:"tracked"`
	LastSynced int64  `json:"last_synced_ms,omitempty"`
}

// Snapshot returns the dashboard snapshot. Nil-safe.
func (s *Service) Snapshot(ctx context.Context) Status {
	if s == nil {
		return Status{Enabled: false}
	}
	st := Status{
		Enabled:  s.enabled,
		NotesDir: s.notesDir,
		Interval: s.interval.String(),
	}
	if s.db != nil {
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(synced_at),0) FROM notes_sync`).
			Scan(&st.Tracked, &st.LastSynced)
	}
	return st
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	}
	return false
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// summariseResult turns a CallToolResult into a single short string.
func summariseResult(res *mcp.CallToolResult) string {
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

// extractPalaceID best-effort pulls the entry_id out of mempalace's
// JSON-formatted reply text. Returns "" if the reply isn't shaped as
// expected — we tolerate upstream-version drift here rather than
// failing the ingest.
func extractPalaceID(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		t, ok := mcp.AsTextContent(c)
		if !ok {
			continue
		}
		var blob map[string]any
		if err := json.Unmarshal([]byte(t.Text), &blob); err == nil {
			if v, ok := blob["entry_id"].(string); ok {
				return v
			}
		}
	}
	return ""
}

// silence unused-imports check for sql when DB isn't used in a build.
var _ = sql.ErrNoRows

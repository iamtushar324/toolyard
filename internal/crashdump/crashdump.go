// Package crashdump writes panic / fatal context to disk so a restart
// loop leaves breadcrumbs the operator can read later. One file per
// crash: ~/.toolyard/crashes/<unix-nano>.json, mode 0600.
//
// The dump intentionally includes the last ~200 lines of in-process logs
// (from logx.RingSnapshot) so the operator doesn't have to reconstruct
// what was happening from journald alone.
package crashdump

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Dir is the configured crashes directory. Set by Configure() at boot.
var dir string

// Configure sets the crashes directory and ensures it exists with mode 0700.
// Empty path disables crash dumps (useful in tests).
func Configure(d string) error {
	dir = d
	if d == "" {
		return nil
	}
	return os.MkdirAll(d, 0o700)
}

// Dir returns the configured crashes directory ("" if disabled).
func Dir() string { return dir }

// Record writes a crash dump. `where` describes the failure site
// ("http.handler", "goroutine:approval-sweeper", "main"). `panicVal` is the
// recovered value (or nil for fatals). Returns the written path or "".
func Record(where string, panicVal any, extra map[string]string) string {
	if dir == "" {
		return ""
	}
	now := time.Now()
	dump := struct {
		Time       string            `json:"time"`
		Where      string            `json:"where"`
		Panic      string            `json:"panic,omitempty"`
		Goroutines int               `json:"goroutines"`
		GoVersion  string            `json:"go_version"`
		Stack      string            `json:"stack"`
		RecentLogs string            `json:"recent_logs"`
		Extra      map[string]string `json:"extra,omitempty"`
	}{
		Time:       now.UTC().Format(time.RFC3339Nano),
		Where:      where,
		Goroutines: runtime.NumGoroutine(),
		GoVersion:  runtime.Version(),
		Stack:      string(debug.Stack()),
		RecentLogs: logx.RingSnapshot(),
		Extra:      extra,
	}
	if panicVal != nil {
		dump.Panic = fmt.Sprintf("%v", panicVal)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", now.UnixNano()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		// Best-effort: surface to stderr but don't bubble up. We're
		// usually called from a defer recover().
		fmt.Fprintf(os.Stderr, "crashdump: open %s: %v\n", path, err)
		return ""
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	_ = enc.Encode(dump)
	return path
}

// List returns the existing crash dump files sorted newest-first.
func List() ([]Entry, error) {
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out = append(out, Entry{
			Path:    filepath.Join(dir, e.Name()),
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// PruneOlderThan deletes crash dumps older than maxAge. Returns the count
// removed.
func PruneOlderThan(maxAge time.Duration) (int, error) {
	if dir == "" {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge)
	entries, err := List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.ModTime.Before(cutoff) {
			if rerr := os.Remove(e.Path); rerr == nil {
				n++
			}
		}
	}
	return n, nil
}

// Entry is a crash dump file's metadata.
type Entry struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

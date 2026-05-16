package api

import (
	"log/slog"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/crashdump"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Recover wraps an http.Handler so a panic in any handler returns a 500
// to the caller and writes a crash dump instead of taking down the
// process. Goes outside the per-route mux so it covers every endpoint —
// /v1/*, /mcp, /debug/pprof/*, and the static dashboard.
func Recover(next http.Handler) http.Handler {
	lg := logx.For("api.recover")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way to abort a
			// response mid-write; don't treat it as a panic.
			if rec == http.ErrAbortHandler {
				return
			}
			path := crashdump.Record("http.handler", rec, map[string]string{
				"method": r.Method,
				"path":   r.URL.Path,
				"remote": r.RemoteAddr,
			})
			lg.LogAttrs(r.Context(), slog.LevelError, "panic in handler",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", rec),
				slog.String("crash_dump", path),
			)
			// If headers have already been written we can't change the
			// status — best we can do is bail. Otherwise return JSON 500.
			defer func() { _ = recover() }() // swallow "headers already sent"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal error","detail":"see /v1/diagnostics/crashes"}`))
		}()
		next.ServeHTTP(w, r)
	})
}

// diagnostics_crashes implements GET /v1/diagnostics/crashes (list) and
// GET /v1/diagnostics/crashes/<name> (download single dump).
func (s *Server) diagnosticsCrashes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "auth required")
		return
	}
	entries, err := crashdump.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":     crashdump.Dir(),
		"crashes": entries,
	})
}

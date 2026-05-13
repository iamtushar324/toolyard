package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	weblake "github.com/tusharbhardwaj/toolyard/web/lake"
)

// lakeRoutes registers the dashboard-facing API for the personal data lake.
// All routes here are read-only on the underlying lake — write/DDL paths run
// via the MCP tool layer where approval gating applies. The named-query
// endpoints serve SQL bodies stored in web/lake/queries/<tab>/<id>.sql; the
// explorer endpoint accepts ad-hoc SELECTs but funnels them through
// lake.Query (which validates the statement type before executing).
func (s *Server) lakeRoutes(mux *http.ServeMux) {
	if s.lake == nil {
		return // lake disabled; expose no routes rather than 500s
	}
	mux.HandleFunc("/v1/lake/manifest", s.lakeManifest)
	mux.HandleFunc("/v1/lake/run/", s.lakeRunNamed)
	mux.HandleFunc("/v1/lake/exec", s.lakeExec)
	mux.HandleFunc("/v1/lake/tables", s.lakeTables)
	mux.HandleFunc("/v1/lake/describe", s.lakeDescribe)
}

// GET /v1/lake/manifest — returns the embedded manifest.json that the
// dashboard uses to discover tabs/panels.
func (s *Server) lakeManifest(w http.ResponseWriter, r *http.Request) {
	if !s.requireLakeSession(w, r) {
		return
	}
	body, err := fs.ReadFile(weblake.Assets, "manifest.json")
	if err != nil {
		http.Error(w, "manifest missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// GET /v1/lake/run/{tab}/{id} — load queries/<tab>/<id>.sql from the embed
// FS and run it. tab and id are constrained to [a-zA-Z0-9_-]+ to keep
// path-traversal off the table; if either fails validation we 400.
func (s *Server) lakeRunNamed(w http.ResponseWriter, r *http.Request) {
	if !s.requireLakeSession(w, r) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/lake/run/")
	parts := strings.Split(tail, "/")
	if len(parts) != 2 || !isSafeIdent(parts[0]) || !isSafeIdent(parts[1]) {
		http.Error(w, "bad path; expected /v1/lake/run/{tab}/{id}", http.StatusBadRequest)
		return
	}
	relPath := path.Join("queries", parts[0], parts[1]+".sql")
	sqlBytes, err := fs.ReadFile(weblake.Assets, relPath)
	if err != nil {
		http.Error(w, "named query not found", http.StatusNotFound)
		return
	}
	maxRows := lake.DefaultMaxRows
	if v := r.URL.Query().Get("max_rows"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxRows = n
		}
	}
	res, err := s.lake.Query(r.Context(), string(sqlBytes), lake.QueryOpts{MaxRows: maxRows})
	if err != nil {
		writeLakeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeLakeJSON(w, http.StatusOK, res)
}

// POST /v1/lake/exec — explorer tab. Body is {sql, max_rows}. Read-only
// (ValidateSelect inside lake.Query rejects non-SELECT). Capped at the
// lake-level row/byte ceilings.
func (s *Server) lakeExec(w http.ResponseWriter, r *http.Request) {
	if !s.requireLakeSession(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.SQL) == "" {
		http.Error(w, "sql is required", http.StatusBadRequest)
		return
	}
	res, err := s.lake.Query(r.Context(), body.SQL, lake.QueryOpts{MaxRows: body.MaxRows})
	if err != nil {
		writeLakeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeLakeJSON(w, http.StatusOK, res)
}

// GET /v1/lake/tables?schema=raw — proxy to lake.ListTables.
func (s *Server) lakeTables(w http.ResponseWriter, r *http.Request) {
	if !s.requireLakeSession(w, r) {
		return
	}
	tabs, err := s.lake.ListTables(r.Context(), r.URL.Query().Get("schema"))
	if err != nil {
		writeLakeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeLakeJSON(w, http.StatusOK, tabs)
}

// GET /v1/lake/describe?table=mart.bank_accounts — proxy to lake.DescribeTable.
func (s *Server) lakeDescribe(w http.ResponseWriter, r *http.Request) {
	if !s.requireLakeSession(w, r) {
		return
	}
	tbl := r.URL.Query().Get("table")
	if tbl == "" {
		http.Error(w, "table required", http.StatusBadRequest)
		return
	}
	desc, err := s.lake.DescribeTable(r.Context(), tbl)
	if err != nil {
		writeLakeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeLakeJSON(w, http.StatusOK, desc)
}

// requireLakeSession is the standard "must be logged in" gate, named with
// the lake prefix so it doesn't collide if a future package-level
// requireSession ever appears. Accepts either the dashboard session cookie
// (verifySession) or a static `Authorization: Bearer <TOOLYARD_LAKE_TOKEN>`
// header for external read-only consumers like the Grafana Infinity
// datasource. The lake routes are read-only at the engine level
// (lake.Query rejects non-SELECT statements), so a leaked token can't
// mutate state.
func (s *Server) requireLakeSession(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.verifySession(r); ok {
		return true
	}
	if s.lakeAPITokenOK(r) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// lakeAPITokenOK returns true when settings.LakeAPIToken is non-empty and
// the request carries a matching Authorization: Bearer <token>. The
// settings cache is in-memory so the per-request lookup is just a map
// read under an RLock — cheap enough not to need a separate cached copy
// on the server, and reading live means a freshly rotated token takes
// effect immediately without restarting the gateway. Constant-time
// compare so an attacker can't time-leak the secret one byte at a time.
func (s *Server) lakeAPITokenOK(r *http.Request) bool {
	if s.settings == nil {
		return false
	}
	want := s.settings.GetString(settings.LakeAPIToken, "")
	if want == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// WriteGrafanaRuntimeEnv emits a KEY=VALUE env file for the Grafana
// container's docker-compose `env_file:` directive to consume. We write
// atomically (temp + rename) so a partial write can never be observed
// mid-update by a Grafana restart. Mode 0640 so root-via-sudo (the
// typical docker compose runner) can read it while unrelated local
// users cannot. path == "" disables — useful in tests and on first
// boot before the operator has decided where the file should live.
//
// Exported so cmd/gateway can call it during the lake-api-token bootstrap
// without instantiating an api.Server.
func WriteGrafanaRuntimeEnv(filePath, token string) error {
	if filePath == "" {
		return nil
	}
	tmp := filePath + ".tmp"
	body := fmt.Sprintf("# managed by toolyard — do not edit by hand. Rotate via the dashboard.\nTOOLYARD_LAKE_TOKEN=%s\n", token)
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}

// WriteClickhouseRuntimeEnv emits the env file the toolyard-clickhouse
// docker-compose stack reads. Same atomic temp+rename pattern, same
// 0640 mode rationale (root via sudo can read; unrelated local users
// cannot). path == "" disables — useful in tests and on first boot
// before the operator has decided where the file should live.
//
// Exported so cmd/gateway can call it during the clickhouse_password
// bootstrap without instantiating an api.Server.
func WriteClickhouseRuntimeEnv(filePath, password string) error {
	if filePath == "" {
		return nil
	}
	tmp := filePath + ".tmp"
	body := fmt.Sprintf("# managed by toolyard — do not edit by hand. Rotate via the dashboard.\nTOOLYARD_CH_PASSWORD=%s\n", password)
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}

func writeLakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// isSafeIdent allows [a-zA-Z0-9_-] up to 64 chars. Used to constrain URL
// segments (tab id, query id) before they're concatenated into an embed
// path. Kept narrow because "anything that looks like a filename" is not
// the same as "anything safe to feed to fs.ReadFile".
func isSafeIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

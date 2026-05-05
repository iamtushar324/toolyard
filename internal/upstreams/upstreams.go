// Package upstreams persists MCP-server configurations and reconciles them
// with the running gateway: connect-on-add, disconnect-on-remove, retry on
// reconnect, all from the dashboard.
package upstreams

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var (
	ErrInvalid     = errors.New("invalid server config")
	ErrAlreadyHere = errors.New("server name already exists")
	ErrNotFound    = errors.New("server not found")
	ErrReserved    = errors.New("server name is reserved")
)

// Server is one persisted upstream config and its current connection status.
type Server struct {
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	URL        string            `json:"url,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Enabled    bool              `json:"enabled"`
	LastStatus string            `json:"last_status,omitempty"`
	LastError  string            `json:"last_error,omitempty"`
	ToolCount  int               `json:"tool_count"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

// Service owns the upstream_servers table and keeps the live gateway in sync.
type Service struct {
	db *store.DB
	gw *gateway.Gateway

	mu sync.Mutex // serializes connect/disconnect side-effects
}

func New(db *store.DB, gw *gateway.Gateway) *Service {
	return &Service{db: db, gw: gw}
}

func (s *Service) toCfg(srv Server) gateway.UpstreamConfig {
	return gateway.UpstreamConfig{
		Name:      srv.Name,
		Transport: srv.Transport,
		Command:   srv.Command,
		Args:      srv.Args,
		URL:       srv.URL,
		Env:       srv.Env,
	}
}

func validate(srv Server) error {
	if strings.TrimSpace(srv.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalid)
	}
	if strings.ContainsAny(srv.Name, " \t\n.") {
		return fmt.Errorf("%w: name must not contain spaces or dots", ErrInvalid)
	}
	switch srv.Name {
	case "builtin", "fixture", "memory", "tools":
		return ErrReserved
	}
	switch srv.Transport {
	case "stdio":
		if srv.Command == "" {
			return fmt.Errorf("%w: stdio requires command", ErrInvalid)
		}
	case "http", "streamable-http", "":
		if srv.URL == "" {
			return fmt.Errorf("%w: http requires url", ErrInvalid)
		}
		if srv.Transport == "" {
			srv.Transport = "http"
		}
	default:
		return fmt.Errorf("%w: unsupported transport %q", ErrInvalid, srv.Transport)
	}
	return nil
}

// LoadAll reads all enabled servers from the DB and connects them in
// parallel. Connection errors are recorded against last_status / last_error
// so the dashboard can show why something didn't come up.
func (s *Service) LoadAll(ctx context.Context) error {
	servers, err := s.list(ctx)
	if err != nil {
		return err
	}
	for _, srv := range servers {
		if !srv.Enabled {
			continue
		}
		if err := s.connect(ctx, srv); err != nil {
			s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		}
	}
	return nil
}

func (s *Service) list(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), enabled,
            COALESCE(last_status,''), COALESCE(last_error,''), tool_count,
            created_at, updated_at
         FROM upstream_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		var srv Server
		var argsRaw, envRaw string
		var enabled int
		if err := rows.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
			&srv.URL, &envRaw, &enabled, &srv.LastStatus, &srv.LastError,
			&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
			return nil, err
		}
		srv.Enabled = enabled != 0
		if argsRaw != "" {
			_ = json.Unmarshal([]byte(argsRaw), &srv.Args)
		}
		if envRaw != "" {
			_ = json.Unmarshal([]byte(envRaw), &srv.Env)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// List returns the persisted upstream configs.
func (s *Service) List(ctx context.Context) ([]Server, error) { return s.list(ctx) }

// Add inserts a new upstream and tries to connect. On a connection failure the
// row stays around (so the dashboard can show the error and the operator can
// edit + retry); the call returns the connection error for the API layer.
func (s *Service) Add(ctx context.Context, srv Server) (*Server, error) {
	if err := validate(srv); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	srv.CreatedAt = now
	srv.UpdatedAt = now
	srv.Enabled = true

	argsBlob, _ := json.Marshal(srv.Args)
	envBlob, _ := json.Marshal(srv.Env)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_servers(name, transport, command, args_json, url,
            env_json, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?)`,
		srv.Name, srv.Transport, nullStr(srv.Command), string(argsBlob),
		nullStr(srv.URL), string(envBlob), 1, now, now)
	if err != nil {
		// SQLite reports unique constraint as "UNIQUE constraint failed".
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrAlreadyHere
		}
		return nil, err
	}

	if err := s.connect(ctx, srv); err != nil {
		s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		final, _ := s.get(ctx, srv.Name)
		return final, err
	}
	final, _ := s.get(ctx, srv.Name)
	return final, nil
}

// Remove disconnects the upstream and deletes the row.
func (s *Service) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.gw.RemoveUpstream(name); err != nil &&
		!errors.Is(err, gateway.ErrUpstreamNotFound) {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM upstream_servers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Reconnect drops any existing connection and re-tries.
func (s *Service) Reconnect(ctx context.Context, name string) (*Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	_ = s.gw.RemoveUpstream(name)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, name, "", err.Error(), 0)
		final, _ := s.get(ctx, name)
		return final, err
	}
	return s.get(ctx, name)
}

// connect attaches the upstream to the gateway and records status. Caller
// holds s.mu.
func (s *Service) connect(ctx context.Context, srv Server) error {
	if err := s.gw.AddUpstream(ctx, s.toCfg(srv)); err != nil {
		return err
	}
	s.recordStatus(ctx, srv.Name, "ok", "", s.gw.UpstreamToolCount(srv.Name))
	return nil
}

func (s *Service) get(ctx context.Context, name string) (*Server, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), enabled,
            COALESCE(last_status,''), COALESCE(last_error,''), tool_count,
            created_at, updated_at
         FROM upstream_servers WHERE name = ?`, name)
	var srv Server
	var argsRaw, envRaw string
	var enabled int
	if err := row.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
		&srv.URL, &envRaw, &enabled, &srv.LastStatus, &srv.LastError,
		&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	srv.Enabled = enabled != 0
	if argsRaw != "" {
		_ = json.Unmarshal([]byte(argsRaw), &srv.Args)
	}
	if envRaw != "" {
		_ = json.Unmarshal([]byte(envRaw), &srv.Env)
	}
	return &srv, nil
}

func (s *Service) recordStatus(ctx context.Context, name, status, errMsg string, toolCount int) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE upstream_servers SET last_status = ?, last_error = ?, tool_count = ?, updated_at = ?
         WHERE name = ?`,
		nullStr(status), nullStr(errMsg), toolCount, time.Now().UnixMilli(), name)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SortedNames is a small helper for the API layer.
func SortedNames(servers []Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

package mcpauth

// apps.go is the Connected apps view of the grants: list, rename, change
// the tool scope, revoke. Callers (internal/api) check that the person
// owns the app or is an admin; these functions only act.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// App is one connected app: a grant, its client and its agent.
type App struct {
	ID            string            `json:"id"`
	AgentID       string            `json:"agent_id"`
	Nickname      string            `json:"nickname"`
	ClientID      string            `json:"client_id"`
	ClientName    string            `json:"client_name"`
	ClientKind    string            `json:"client_kind"`
	ClientURI     string            `json:"client_uri,omitempty"`
	LogoURI       string            `json:"logo_uri,omitempty"`
	RedirectHosts []string          `json:"redirect_hosts"`
	OwnerID       string            `json:"owner_id"`
	OwnerEmail    string            `json:"owner_email,omitempty"`
	OwnerName     string            `json:"owner_name,omitempty"`
	Scope         access.AgentScope `json:"scope"`
	ScopeSummary  string            `json:"scope_summary"`
	OAuthScope    string            `json:"oauth_scope"`
	Status        string            `json:"status"` // active | revoked | disabled | deleted
	// MayManage is the stored choice; it has an effect only while the
	// owner is an admin (ManageActive).
	MayManage     bool   `json:"may_manage"`
	OwnerIsAdmin  bool   `json:"owner_is_admin"`
	ManageActive  bool   `json:"manage_active"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	LastUsedAt    int64  `json:"last_used_at,omitempty"`
	RevokedAt     int64  `json:"revoked_at,omitempty"`
	RevokedReason string `json:"revoked_reason,omitempty"`
}

const appColumns = `g.id, g.agent_id, COALESCE(a.name, ''), g.client_id, COALESCE(c.client_name, ''), COALESCE(c.kind, ''),
	COALESCE(c.client_uri, ''), COALESCE(c.logo_uri, ''), COALESCE(c.redirect_uris, '[]'),
	g.user_id, COALESCE(u.email, ''), COALESCE(u.display_name, ''), COALESCE(u.username, ''),
	g.scope, COALESCE(a.disabled, 0), g.created_at, g.updated_at, COALESCE(g.last_used_at, 0),
	COALESCE(g.revoked_at, 0), COALESCE(g.revoked_reason, ''), g.may_manage, COALESCE(a.id, ''),
	COALESCE(u.role, ''), COALESCE(u.status, '')`

const appFrom = ` FROM mcp_oauth_grants g
	LEFT JOIN agents a ON a.id = g.agent_id
	LEFT JOIN mcp_oauth_clients c ON c.client_id = g.client_id
	LEFT JOIN users u ON u.id = g.user_id`

// scanApp reads one row of appColumns. The tool scope lives in another
// table and is filled by withScope once the result set is closed, since
// SQLite runs on one connection.
func scanApp(row interface{ Scan(...any) error }) (*App, error) {
	var a App
	var redirects, display, username, agentRow, ownerRole, ownerStatus string
	var disabled, manage int
	if err := row.Scan(&a.ID, &a.AgentID, &a.Nickname, &a.ClientID, &a.ClientName, &a.ClientKind,
		&a.ClientURI, &a.LogoURI, &redirects, &a.OwnerID, &a.OwnerEmail, &display, &username,
		&a.OAuthScope, &disabled, &a.CreatedAt, &a.UpdatedAt, &a.LastUsedAt, &a.RevokedAt, &a.RevokedReason, &manage, &agentRow,
		&ownerRole, &ownerStatus); err != nil {
		return nil, err
	}
	a.MayManage = manage != 0
	a.OwnerIsAdmin = ownerRole == identity.RoleAdmin && ownerStatus == identity.StatusActive
	a.ManageActive = a.MayManage && a.OwnerIsAdmin
	a.OwnerName = display
	if a.OwnerName == "" {
		a.OwnerName = username
	}
	var uris []string
	_ = json.Unmarshal([]byte(redirects), &uris)
	seen := map[string]bool{}
	a.RedirectHosts = []string{}
	for _, u := range uris {
		if h := RedirectHost(u); !seen[h] {
			seen[h] = true
			a.RedirectHosts = append(a.RedirectHosts, h)
		}
	}
	c := Client{ID: a.ClientID, Kind: a.ClientKind, Name: a.ClientName}
	a.ClientName = c.DisplayName()
	switch {
	case agentRow == "":
		a.Status = "deleted" // the agent was deleted; the grant is its tombstone
	case a.RevokedAt > 0:
		a.Status = "revoked"
	case disabled != 0:
		a.Status = "disabled"
	default:
		a.Status = "active"
	}
	return &a, nil
}

// withScope fills the app's tool scope.
func (s *Service) withScope(ctx context.Context, a *App) error {
	sc, err := access.ReadAgentScope(ctx, s.db, a.AgentID)
	if err != nil {
		return err
	}
	a.Scope, a.ScopeSummary = sc, sc.Summary()
	return nil
}

// ListApps returns ownerID's connected apps, or everyone's when ownerID is
// "", newest first.
func (s *Service) ListApps(ctx context.Context, ownerID string) ([]App, error) {
	q := `SELECT ` + appColumns + appFrom
	var args []any
	if ownerID != "" {
		q += ` WHERE g.user_id = ?`
		args = append(args, ownerID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []App{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.withScope(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// App returns one connected app by grant id, or ErrAppNotFound.
func (s *Service) App(ctx context.Context, id string) (*App, error) {
	a, err := scanApp(s.db.QueryRowContext(ctx, `SELECT `+appColumns+appFrom+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.withScope(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// AppByAgent returns the connected app whose agent is agentID, or
// ErrAppNotFound when agentID is an ordinary agent.
func (s *Service) AppByAgent(ctx context.Context, agentID string) (*App, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM mcp_oauth_grants WHERE agent_id = ?`, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.App(ctx, id)
}

// RenameApp sets the app's nickname (its agent's name).
func (s *Service) RenameApp(ctx context.Context, app *App, nickname string, by *identity.User) error {
	nickname, err := CheckNickname(nickname)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET name = ? WHERE id = ?`, nickname, app.AgentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET updated_at = ? WHERE id = ?`, s.now().UnixMilli(), app.ID); err != nil {
		return err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.renamed", agentID: app.AgentID, nickname: nickname,
		owner: appOwner(app), decider: by, clientID: app.ClientID, grantID: app.ID,
		details: map[string]any{"previous_nickname": app.Nickname}}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppScope replaces the app's tool scope.
func (s *Service) SetAppScope(ctx context.Context, app *App, sc access.AgentScope, by *identity.User) (access.AgentScope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return access.AgentScope{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := access.WriteAgentScope(ctx, tx, app.AgentID, sc, by.ID)
	if err != nil {
		return access.AgentScope{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET updated_at = ? WHERE id = ?`, s.now().UnixMilli(), app.ID); err != nil {
		return access.AgentScope{}, err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.scope_changed", agentID: app.AgentID, nickname: app.Nickname,
		owner: appOwner(app), decider: by, clientID: app.ClientID, grantID: app.ID,
		details: map[string]any{"tool_scope": out.Summary(), "previous": app.ScopeSummary}}); err != nil {
		return access.AgentScope{}, err
	}
	if err = tx.Commit(); err != nil {
		return access.AgentScope{}, err
	}
	if s.access != nil {
		s.access.InvalidateAgent(app.AgentID)
	}
	return out, nil
}

// RevokeApp revokes the app: its tokens stop working and its agent is
// disabled. Connecting the client again asks for consent again.
func (s *Service) RevokeApp(ctx context.Context, app *App, by *identity.User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	agentID, err := s.revokeGrantTx(ctx, tx, app.ID, "revoked_by_owner")
	if err != nil {
		return err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.revoked", agentID: app.AgentID, nickname: app.Nickname,
		owner: appOwner(app), decider: by, clientID: app.ClientID, grantID: app.ID,
		details: map[string]any{"by": "dashboard"}}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.afterRevoke(ctx, agentID)
	return nil
}

// ErrManageOwnerOnly: only the app's owner, while an admin, may let it
// manage toolyard.
var ErrManageOwnerOnly = errors.New("mcpauth: only the app's owner, an admin, may let it manage toolyard")

// SetAppManage sets whether the app may manage toolyard (policies, servers,
// shared connections). Turning it on is for the app's owner, while an
// admin; anyone who manages the app may turn it off.
func (s *Service) SetAppManage(ctx context.Context, app *App, manage bool, by *identity.User) error {
	if manage && (by == nil || by.ID != app.OwnerID || by.Role != identity.RoleAdmin) {
		return ErrManageOwnerOnly
	}
	v := 0
	if manage {
		v = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET may_manage = ?, updated_at = ? WHERE id = ?`, v, s.now().UnixMilli(), app.ID); err != nil {
		return err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.manage_changed", agentID: app.AgentID, nickname: app.Nickname,
		owner: appOwner(app), decider: by, clientID: app.ClientID, grantID: app.ID,
		details: map[string]any{"may_manage": manage}}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if s.access != nil {
		s.access.InvalidateAgent(app.AgentID)
	}
	return nil
}

// appOwner is the app's owner as a user, for audit rows.
func appOwner(app *App) *identity.User {
	return &identity.User{ID: app.OwnerID, Email: app.OwnerEmail, DisplayName: app.OwnerName}
}

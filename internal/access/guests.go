package access

// guests.go holds guest invites (TEC-2058): a person outside the Clerk
// organisation signs in only after an admin saved their email in
// guest_invites, and then reaches only the invite's servers and, when the
// invite lists tools, only those tools (plus the always-on groups an agent
// needs to search and ask). The invite is the one source of truth: a
// guest's scope is read from it on every scope load (loadUserScope), so
// an edit, a removal or an expiry takes effect within the scope cache's
// few seconds on every replica, without a copy that could drift. The
// grants in user_server_access are a mirror for the Users page only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
)

// ErrInvalidInvite is returned for a malformed guest invite.
var ErrInvalidInvite = errors.New("invalid guest invite")

// maxInviteNote bounds an invite's note.
const maxInviteNote = 500

// guestNever are the groups a guest never reaches, even if an invite or a
// stored grant names them: the built-in data groups hold data shared by
// the whole organisation, and the skills hub shows the organisation's
// skills (it is always on for members).
var guestNever = map[string]bool{"memory": true, "events": true, "notes": true, "skills": true}

// GuestInvite is one row of guest_invites, as stored and as the REST API
// shows it. Times are Unix milliseconds; ExpiresAt 0 is "never".
type GuestInvite struct {
	Email     string   `json:"email"`
	Servers   []string `json:"servers"`
	Tools     []string `json:"tools"`
	ExpiresAt int64    `json:"expires_at,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt int64    `json:"created_at,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	UpdatedAt int64    `json:"updated_at,omitempty"`
	UpdatedBy string   `json:"updated_by,omitempty"`
}

// Expired reports whether the invite no longer admits anyone at now.
func (inv GuestInvite) Expired(now time.Time) bool {
	return inv.ExpiresAt > 0 && now.UnixMilli() >= inv.ExpiresAt
}

// NormalizeEmail returns email trimmed and in lower case, or
// ErrInvalidInvite. Only plain ASCII addresses are accepted: the match
// against a Clerk address is then a plain byte compare, and no Unicode
// case folding (the Kelvin sign lower-cases to k) can make another
// address match.
func NormalizeEmail(email string) (string, error) {
	e := strings.TrimSpace(email)
	if e == "" || len(e) > 254 {
		return "", fmt.Errorf("%w: email is required (at most 254 characters)", ErrInvalidInvite)
	}
	for i := 0; i < len(e); i++ {
		if c := e[i]; c <= ' ' || c > '~' {
			return "", fmt.Errorf("%w: email %q must be plain ASCII without spaces", ErrInvalidInvite, email)
		}
	}
	e = strings.ToLower(e)
	addr, err := mail.ParseAddress(e)
	local, domain, ok := strings.Cut(e, "@")
	if err != nil || addr.Address != e || addr.Name != "" || !ok || local == "" ||
		strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalidInvite, email)
	}
	return e, nil
}

// Normalize checks inv for a write at now and returns it cleaned: a valid
// email in lower case; one or more servers, each a valid group name that a
// guest may be given (never a built-in data group, an always-on group or a
// retired one); tools as plain full names; an expiry in the future (or
// none); a note of at most 500 characters. Lists are de-duplicated and
// sorted. Whether each server and tool exists is the caller's check: it
// knows the live catalog.
func (inv GuestInvite) Normalize(now time.Time) (GuestInvite, error) {
	out := GuestInvite{ExpiresAt: inv.ExpiresAt, Note: strings.TrimSpace(inv.Note)}
	var err error
	if out.Email, err = NormalizeEmail(inv.Email); err != nil {
		return GuestInvite{}, err
	}
	if len(inv.Servers) == 0 {
		return GuestInvite{}, fmt.Errorf("%w: name at least one server", ErrInvalidInvite)
	}
	if out.Servers, err = cleanServers(inv.Servers); err != nil {
		return GuestInvite{}, fmt.Errorf("%w: %v", ErrInvalidInvite, err)
	}
	for _, g := range out.Servers {
		switch {
		case IsBuiltinGroup(g) || guestNever[g]:
			return GuestInvite{}, fmt.Errorf("%w: %s holds data shared by the whole organisation and is never given to a guest", ErrInvalidInvite, g)
		case alwaysOn[g]:
			return GuestInvite{}, fmt.Errorf("%w: %s is always on and needs no grant", ErrInvalidInvite, g)
		case retiredGroups[g]:
			return GuestInvite{}, fmt.Errorf("%w: %s no longer exists", ErrInvalidInvite, g)
		}
	}
	if len(inv.Tools) > maxScopeEntries {
		return GuestInvite{}, fmt.Errorf("%w: at most %d tools", ErrInvalidInvite, maxScopeEntries)
	}
	seen := map[string]bool{}
	out.Tools = []string{}
	for _, t := range inv.Tools {
		t = strings.TrimSpace(t)
		if !plainToolName(t) {
			return GuestInvite{}, fmt.Errorf("%w: tool %q", ErrInvalidInvite, t)
		}
		if !seen[t] {
			seen[t] = true
			out.Tools = append(out.Tools, t)
		}
	}
	sort.Strings(out.Tools)
	if out.ExpiresAt < 0 || (out.ExpiresAt > 0 && out.ExpiresAt <= now.UnixMilli()) {
		return GuestInvite{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInvite)
	}
	if len(out.Note) > maxInviteNote {
		return GuestInvite{}, fmt.Errorf("%w: note is longer than %d characters", ErrInvalidInvite, maxInviteNote)
	}
	return out, nil
}

const guestInviteColumns = `email, servers, tools, COALESCE(expires_at, 0), note, created_at, COALESCE(created_by, ''), updated_at, COALESCE(updated_by, '')`

func scanGuestInvite(row rowScanner) (*GuestInvite, error) {
	var inv GuestInvite
	var servers, tools string
	if err := row.Scan(&inv.Email, &servers, &tools, &inv.ExpiresAt, &inv.Note,
		&inv.CreatedAt, &inv.CreatedBy, &inv.UpdatedAt, &inv.UpdatedBy); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(servers), &inv.Servers); err != nil {
		return nil, fmt.Errorf("guest_invites %s servers: %w", inv.Email, err)
	}
	if err := json.Unmarshal([]byte(tools), &inv.Tools); err != nil {
		return nil, fmt.Errorf("guest_invites %s tools: %w", inv.Email, err)
	}
	if inv.Servers == nil {
		inv.Servers = []string{}
	}
	if inv.Tools == nil {
		inv.Tools = []string{}
	}
	return &inv, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

// GuestInvite returns the invite for email, expired or not, or nil when
// there is none. An address NormalizeEmail refuses has no invite.
func (s *Service) GuestInvite(ctx context.Context, email string) (*GuestInvite, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return nil, nil
	}
	inv, err := scanGuestInvite(s.db.QueryRowContext(ctx,
		`SELECT `+guestInviteColumns+` FROM guest_invites WHERE email = ?`, e))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return inv, err
}

// ListGuestInvites returns every invite, by email.
func (s *Service) ListGuestInvites(ctx context.Context) ([]GuestInvite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+guestInviteColumns+` FROM guest_invites ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestInvite{}
	for rows.Next() {
		inv, err := scanGuestInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	return out, rows.Err()
}

// PutGuestInvite creates or replaces the invite for inv.Email (normalized
// first; ErrInvalidInvite) and reports whether it was new. A replaced
// invite keeps its created_at and created_by. by is a user id. Every
// cached scope is dropped, so a guest's agents see the change on their
// next call.
func (s *Service) PutGuestInvite(ctx context.Context, inv GuestInvite, by string) (GuestInvite, bool, error) {
	now := s.now()
	out, err := inv.Normalize(now)
	if err != nil {
		return GuestInvite{}, false, err
	}
	servers, err := json.Marshal(out.Servers)
	if err != nil {
		return GuestInvite{}, false, err
	}
	tools, err := json.Marshal(out.Tools)
	if err != nil {
		return GuestInvite{}, false, err
	}
	prev, err := s.GuestInvite(ctx, out.Email)
	if err != nil {
		return GuestInvite{}, false, err
	}
	ms := now.UnixMilli()
	var expires any
	if out.ExpiresAt > 0 {
		expires = out.ExpiresAt
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO guest_invites(email, servers, tools, expires_at, note, created_at, created_by, updated_at, updated_by)
         VALUES(?,?,?,?,?,?,?,?,?)
         ON CONFLICT(email) DO UPDATE SET servers = excluded.servers, tools = excluded.tools,
             expires_at = excluded.expires_at, note = excluded.note,
             updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		out.Email, string(servers), string(tools), expires, out.Note, ms, nullStr(by), ms, nullStr(by)); err != nil {
		return GuestInvite{}, false, err
	}
	s.Invalidate("")
	saved, err := s.GuestInvite(ctx, out.Email)
	if err != nil {
		return GuestInvite{}, false, err
	}
	if saved == nil {
		return GuestInvite{}, false, fmt.Errorf("guest invite for %s vanished after the write", out.Email)
	}
	return *saved, prev == nil, nil
}

// DeleteGuestInvite removes the invite for email and reports whether there
// was one. Every cached scope is dropped: the guest's agents reach nothing
// from their next call (loadUserScope finds no invite).
func (s *Service) DeleteGuestInvite(ctx context.Context, email string) (bool, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM guest_invites WHERE email = ?`, e)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	s.Invalidate("")
	return n > 0, nil
}

// guestScope is a guest's scope, read from the invite for their email:
// Denied when there is none or it has expired, else the invite's servers
// (never a built-in data group or the skills hub) and, when the invite
// lists tools, only those tools of them.
func (s *Service) guestScope(ctx context.Context, email string) (Scope, error) {
	inv, err := s.GuestInvite(ctx, email)
	if err != nil {
		return Scope{}, err
	}
	if inv == nil || inv.Expired(s.now()) {
		return Scope{Denied: true}, nil
	}
	sc := Scope{Guest: true, Groups: make(map[string]bool, len(inv.Servers))}
	for _, g := range inv.Servers {
		if guestNever[g] || IsBuiltinGroup(g) || alwaysOn[g] || retiredGroups[g] {
			continue
		}
		sc.Groups[g] = true
	}
	if len(inv.Tools) > 0 {
		sc.GuestTools = make(map[string]bool, len(inv.Tools))
		for _, t := range inv.Tools {
			sc.GuestTools[t] = true
		}
	}
	return sc, nil
}

// GuestToolList is the scope's guest tool list, sorted; nil when the
// caller is not a guest or the guest may use every tool of their servers.
func (s Scope) GuestToolList() []string {
	if s.GuestTools == nil {
		return nil
	}
	out := make([]string, 0, len(s.GuestTools))
	for t := range s.GuestTools {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

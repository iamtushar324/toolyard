package api

// Skills hub admin routes (TEC-1782, slice C; docs/skills-hub.md "For
// admins"). Owner and admin, through the role guard and requireAdmin; a
// skill contributor reaches the add-source routes and their own sources
// (skillManagerUser, skills_sharing_routes.go), never activate, reject,
// approval or the GitHub token. The webhook is the one public route,
// authenticated by GitHub's signature.
//
//	POST   /v1/skills/sources/inspect                       {url, token?, auth?} -> repository, branches, trusted_owner
//	POST   /v1/skills/sources/preview                       SourceInput -> dry run; nothing is stored
//	GET    /v1/skills/admin/sources                         every source (the agent list is GET /v1/skills/sources)
//	POST   /v1/skills/sources                               SourceInput -> 201 {source, status: "syncing"}; the first sync runs in the background
//	GET    /v1/skills/sources/{id}
//	PATCH  /v1/skills/sources/{id}                          SourceUpdate
//	DELETE /v1/skills/sources/{id}
//	POST   /v1/skills/sources/{id}/sync                     sync now
//	GET    /v1/skills/sources/{id}/versions[?limit=]
//	POST   /v1/skills/sources/{id}/versions/{vid}/activate  rollback, or approve a staged version
//	POST   /v1/skills/sources/{id}/versions/{vid}/reject    {note?}
//	GET    /v1/skills/sources/{id}/skills                   skills with runs and 30-day reads
//	PUT    /v1/skills/sources/{id}/skills/runs              {path, runs}; runs "" clears the override
//	PUT    /v1/skills/sources/{id}/skills/sharing           {path, mode, members, blocked} (skills_sharing_routes.go)
//	POST   /v1/skills/sources/{id}/approval                 admin: approve or reject a contributor's source
//	POST   /v1/skills/sources/{id}/webhook                  GitHub push webhook (public, signed)
//	POST   /v1/skills/sources/{id}/webhook-secret           rotate: the secret once, with the payload URL
//	GET    /v1/skills/admin/github-token                    {set, rate, paused_until}: the default GitHub read token's state
//	PUT    /v1/skills/admin/github-token                    {token}: set (checked with GitHub) or, with "", clear it
//
// A GitHub rate limit answers 429 with retry_after (seconds) and reset_at
// on every route, with a Retry-After header.
//
// A source token is write-only: no response carries it, and an error that
// echoes it is redacted before it leaves. The webhook secret appears only
// in the rotate response. Every write is audited (skills.*).

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

const (
	skillAdminSourcesPrefix = "/v1/skills/sources/"
	// SkillWebhookMaxBytes caps a GitHub webhook delivery to
	// POST /v1/skills/sources/{id}/webhook.
	SkillWebhookMaxBytes int64 = 5 << 20
	// skillAdminBodyMax caps the other skills admin bodies (a source with
	// its excludes and members, a token).
	skillAdminBodyMax int64 = 64 << 10
	// skillWebhookTimeout bounds one webhook's sync; it is detached from
	// the request so GitHub closing the connection does not cut it short.
	skillWebhookTimeout = 2 * time.Minute
	// skillCreateRetryWindow is how long after a create the same create
	// (same person, repository, branch and folder) answers with the
	// source it made instead of a second one with a "-2" slug: a retried
	// request, a double click.
	skillCreateRetryWindow = 2 * time.Minute
	skillAdminHubOff       = "skills hub is off"
)

// skillAdminIDPattern is the shape of source and version IDs in a path.
var skillAdminIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SkillReviewSettler closes the open Inbox review items of a source after
// an admin decided on the Skills page (skillhub.ReviewInbox.Settle).
// versionID "" closes the items of every version.
type SkillReviewSettler interface {
	Settle(ctx context.Context, sourceID, versionID, reason string) error
}

// skillWebhookPath reports whether path is a source's webhook route.
func skillWebhookPath(path string) bool {
	rest, ok := strings.CutPrefix(path, skillAdminSourcesPrefix)
	if !ok {
		return false
	}
	id, sub, ok := strings.Cut(rest, "/")
	return ok && sub == "webhook" && skillAdminIDPattern.MatchString(id)
}

// skillAdminPath reports whether path is one of the skills admin routes
// (for the per-route body cap).
func skillAdminPath(path string) bool {
	return path == "/v1/skills/sources" || strings.HasPrefix(path, skillAdminSourcesPrefix) || path == "/v1/skills/admin/sources" ||
		path == skillGitHubTokenPath || strings.HasPrefix(path, skillContributorsPrefix)
}

// skillGitHubTokenPath is the default GitHub read token's route.
const skillGitHubTokenPath = "/v1/skills/admin/github-token"

// skillsGitHubAccess is the part of the skills hub that holds the default
// GitHub read token (*skillhub.Service). An admin service without it
// answers 404 on the route.
type skillsGitHubAccess interface {
	GitHubAccess(ctx context.Context) (*skillhub.GitHubAccess, error)
	SetGitHubToken(ctx context.Context, by, token string) (*skillhub.GitHubAccess, error)
}

func (s *Server) registerSkillsAdminRoutes(mux *http.ServeMux) {
	// A method pattern: GET /v1/skills/sources is the agents' list
	// (skills_routes.go), and the more specific POST wins for create.
	mux.HandleFunc("POST /v1/skills/sources", s.skillSourceCreate)
	mux.HandleFunc("/v1/skills/sources/", s.skillSourcesItem)
	mux.HandleFunc("/v1/skills/admin/sources", s.skillAdminSources)
	mux.HandleFunc("/v1/skills/admin/github-token", s.skillGitHubToken)
	s.registerSkillsSharingRoutes(mux)
}

// skillGitHubToken reads or sets the default GitHub read token: write-only,
// never in a response, an error or the audit log.
func (s *Server) skillGitHubToken(w http.ResponseWriter, r *http.Request) {
	u, ok := s.skillAdminUser(w, r)
	if !ok {
		return
	}
	ga, ok := s.skillsAdmin.(skillsGitHubAccess)
	if !ok {
		writeError(w, http.StatusNotFound, "a default GitHub token is not available on this server")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		st, err := ga.GitHubAccess(r.Context())
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case http.MethodPut:
		var body struct {
			Token string `json:"token"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, redactSkillSecrets(err.Error(), body.Token))
			return
		}
		st, err := ga.SetGitHubToken(r.Context(), u.ID, body.Token)
		if err != nil {
			writeSkillAdminError(w, err, body.Token)
			return
		}
		event := "skills.github_token_set"
		if strings.TrimSpace(body.Token) == "" {
			event = "skills.github_token_clear"
		}
		s.auditSkillAdmin(r, u, event, "default GitHub read token", "")
		writeJSON(w, http.StatusOK, st)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or PUT")
	}
}

// skillAdminUser answers 401/403 for anyone but an admin, and 404 while the
// hub is off.
func (s *Server) skillAdminUser(w http.ResponseWriter, r *http.Request) (*identity.User, bool) {
	u, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return nil, false
	}
	if s.skillsAdmin == nil {
		writeError(w, http.StatusNotFound, skillAdminHubOff)
		return nil, false
	}
	return u, true
}

// skillAdminSources lists every source for an admin, and a contributor's
// own sources for a skill contributor.
func (s *Server) skillAdminSources(w http.ResponseWriter, r *http.Request) {
	m, ok := s.skillManagerUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	list, err := s.skillsAdmin.ListSources(r.Context())
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	out := []skillhub.Source{}
	for i := range list {
		if m.owns(&list[i]) {
			out = append(out, list[i])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// skillSourceCreate adds a source. A skill contributor's source starts
// pending approval: it is checked with a dry run first (the Inbox question
// to the owner carries its counts and warnings), and agents see nothing of
// it until an admin approves it.
func (s *Server) skillSourceCreate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.skillManagerUser(w, r)
	if !ok {
		return
	}
	u := m.u
	in, ok := s.skillSourceInput(w, r, m)
	if !ok {
		return
	}
	if existing := s.recentSkillSource(r.Context(), u.ID, in); existing != nil && m.owns(existing) {
		writeJSON(w, http.StatusOK, map[string]any{"source": existing, "status": skillSourceStatus(existing), "existing": true})
		return
	}
	var prev *skillhub.Preview
	if !m.admin {
		// An early answer before the dry run; CreateSource enforces the
		// cap itself, under a lock, against parallel creates.
		if s.refuseTooManyPending(w, r, m) {
			return
		}
		var err error
		if prev, err = s.skillsAdmin.Preview(r.Context(), u.ID, in); err != nil {
			writeSkillAdminError(w, err, in.Token)
			return
		}
	}
	src, err := s.skillsAdmin.CreateSource(r.Context(), u.ID, in)
	if err != nil {
		writeSkillAdminError(w, err, in.Token)
		return
	}
	reason := ""
	if !m.admin {
		reason = "skill contributor: waits for an admin's approval"
		s.requestSkillApproval(r.Context(), src, prev, u)
	}
	s.auditSkillAdmin(r, u, "skills.source_create", src.ID+" "+skillhub.RepoLabel(src.Owner, src.Repo, ""), reason)
	// The first sync runs in the background: a large repository can take
	// longer than a request should wait. The dashboard polls the source
	// until its first check is recorded.
	s.skillFirstSync(r.Context(), src.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"source": src, "status": "syncing"})
}

// skillFirstSync syncs a new source on this node in the background. A
// failure is recorded on the source (last_error), and the leader's poll
// tries again.
func (s *Server) skillFirstSync(ctx context.Context, id string) {
	admin := s.skillsAdmin
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skillhub.FirstSyncTimeout)
	goroutines.Go("skills-first-sync", func() {
		defer cancel()
		if _, err := admin.SyncNow(sctx, id); err != nil {
			log.Printf("skills: first sync of %s: %s", id, skillAdminErrorText(err))
		}
	})
}

// recentSkillSource is the source this person created from the same
// repository, branch and folder within skillCreateRetryWindow, or nil: a
// retried create answers with it.
func (s *Server) recentSkillSource(ctx context.Context, uid string, in skillhub.SourceInput) *skillhub.Source {
	ref, err := s.skillsAdmin.ParseURL(in.URL)
	if err != nil {
		return nil
	}
	branch := strings.TrimSpace(in.Branch)
	if branch == "" {
		branch = ref.Branch
	}
	folder := strings.Trim(strings.TrimSpace(in.Path), "/")
	if strings.TrimSpace(in.Path) == "" {
		folder = strings.Trim(ref.Path, "/")
	}
	slug := strings.TrimSpace(in.Slug)
	list, err := s.skillsAdmin.ListSources(ctx)
	if err != nil {
		return nil
	}
	for i := range list {
		src := &list[i]
		switch {
		case src.CreatedBy != uid, time.Since(src.CreatedAt) > skillCreateRetryWindow,
			!strings.EqualFold(src.Owner, ref.Owner), !strings.EqualFold(src.Repo, ref.Repo),
			strings.Trim(src.Path, "/") != folder,
			branch != "" && src.Branch != branch,
			slug != "" && src.Slug != slug:
			continue
		}
		return src
	}
	return nil
}

// skillSourceStatus is the word the dashboard shows for a source.
func skillSourceStatus(src *skillhub.Source) string {
	switch {
	case src.Approval == skillhub.ApprovalRejected:
		return "rejected"
	case !src.Enabled:
		return "paused"
	case src.Approval == skillhub.ApprovalPending:
		return "pending approval"
	case src.LastCheckedAt == nil:
		return "syncing"
	case src.LastError != "":
		return "error"
	case src.PendingVersionID != "":
		return "pending review"
	case src.ActiveVersionID == "":
		return "syncing"
	}
	return "ok"
}

// skillSourcesItem dispatches everything under /v1/skills/sources/.
func (s *Server) skillSourcesItem(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, skillAdminSourcesPrefix), "/")
	if len(parts) == 2 && parts[1] == "webhook" {
		s.skillWebhook(w, r, parts[0])
		return
	}
	m, ok := s.skillManagerUser(w, r)
	if !ok {
		return
	}
	u := m.u
	id := parts[0]
	switch {
	case len(parts) == 1 && id == "inspect":
		s.skillInspect(w, r, m)
		return
	case len(parts) == 1 && id == "preview":
		s.skillPreview(w, r, m)
		return
	}
	if !skillAdminIDPattern.MatchString(id) || (len(parts) == 4 && !skillAdminIDPattern.MatchString(parts[2])) {
		http.NotFound(w, r)
		return
	}
	// Versions are made live or rejected, and contributor sources
	// approved, by admins only.
	if (len(parts) == 4 && parts[1] == "versions") || (len(parts) == 2 && parts[1] == "approval") {
		if !requireSkillAdmin(w, m) {
			return
		}
	}
	if !m.admin {
		if _, ok := s.skillOwnedSource(w, r, m, id); !ok {
			return
		}
	}
	switch {
	case len(parts) == 1:
		s.skillSource(w, r, m, id)
	case len(parts) == 2 && parts[1] == "sync":
		if !skillAdminMethod(w, r, http.MethodPost) {
			return
		}
		res, err := s.skillsAdmin.SyncNow(r.Context(), id)
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		s.auditSkillAdmin(r, u, "skills.source_sync", id+" "+res.SHA, res.Error)
		writeJSON(w, http.StatusOK, res)
	case len(parts) == 2 && parts[1] == "versions":
		if !skillAdminMethod(w, r, http.MethodGet) {
			return
		}
		limit := 20
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = min(n, 100)
		}
		vs, err := s.skillsAdmin.Versions(r.Context(), id, limit)
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		if vs == nil {
			vs = []skillhub.Version{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
	case len(parts) == 4 && parts[1] == "versions" && parts[3] == "activate":
		s.skillActivate(w, r, u, id, parts[2])
	case len(parts) == 4 && parts[1] == "versions" && parts[3] == "reject":
		s.skillReject(w, r, u, id, parts[2])
	case len(parts) == 2 && parts[1] == "skills":
		if !skillAdminMethod(w, r, http.MethodGet) {
			return
		}
		list, err := s.skillsAdmin.Skills(r.Context(), id)
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		if list == nil {
			list = []skillhub.SkillUsage{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"skills": list})
	case len(parts) == 3 && parts[1] == "skills" && parts[2] == "runs":
		s.skillSetRuns(w, r, u, id)
	case len(parts) == 3 && parts[1] == "skills" && parts[2] == "sharing":
		s.skillSetSharing(w, r, m, id)
	case len(parts) == 2 && parts[1] == "approval":
		s.skillDecideApproval(w, r, m, id)
	case len(parts) == 2 && parts[1] == "webhook-secret":
		s.skillRotateSecret(w, r, u, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) skillInspect(w http.ResponseWriter, r *http.Request, m *skillManager) {
	if !skillAdminMethod(w, r, http.MethodPost) {
		return
	}
	var body struct {
		URL   string `json:"url"`
		Token string `json:"token,omitempty"`
		Auth  string `json:"auth,omitempty"` // "github": read with the admin's GitHub connection (slice D)
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, redactSkillSecrets(err.Error(), body.Token))
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required", "code": "invalid"})
		return
	}
	var info *skillhub.RepoInfo
	var err error
	switch {
	case body.Auth == string(skillhub.AuthGitHub):
		info, err = s.skillInspectWithConnection(r, m.u, body.URL)
	case !m.admin && strings.TrimSpace(body.Token) == "":
		// A contributor never reads with the admins' default token.
		sa, ok := s.skillsAdmin.(skillhub.SharingAdmin)
		if !ok {
			writeError(w, http.StatusNotFound, "skill contributors are not available on this server")
			return
		}
		info, err = sa.InspectAnonymous(r.Context(), body.URL)
	default:
		info, err = s.skillsAdmin.Inspect(r.Context(), body.URL, body.Token)
	}
	if err != nil {
		writeSkillAdminError(w, err, body.Token)
		return
	}
	// trusted_owner is the wizard's default mode: auto for a trusted
	// owner, review otherwise (the server decides when mode is unset).
	writeJSON(w, http.StatusOK, struct {
		*skillhub.RepoInfo
		TrustedOwner bool `json:"trusted_owner"`
	}{info, s.skillsAdmin.TrustedOwner(r.Context(), info.Owner)})
}

func (s *Server) skillPreview(w http.ResponseWriter, r *http.Request, m *skillManager) {
	if !skillAdminMethod(w, r, http.MethodPost) {
		return
	}
	in, ok := s.skillSourceInput(w, r, m)
	if !ok {
		return
	}
	prev, err := s.skillsAdmin.Preview(r.Context(), m.u.ID, in)
	if err != nil {
		writeSkillAdminError(w, err, in.Token)
		return
	}
	writeJSON(w, http.StatusOK, prev)
}

// skillSourceInput decodes the source of a preview or a create and marks
// it with who asks. It is the one place that derives this from the
// caller, so no route can forget it: a skill contributor's input gets
// PendingApproval, so the source starts pending approval and every GitHub
// read for it (the dry run included) is anonymous unless the input
// carries the contributor's own token or GitHub connection: never the
// admins' default token. It also refuses what a contributor may not
// choose (contributor_limit), and a person's GitHub connection for an
// operator token. It answers the refusals itself.
func (s *Server) skillSourceInput(w http.ResponseWriter, r *http.Request, m *skillManager) (skillhub.SourceInput, bool) {
	var in skillhub.SourceInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, redactSkillSecrets(err.Error(), in.Token))
		return in, false
	}
	in.PendingApproval = !m.admin
	if s.refuseGitHubConnectionUse(w, r, in.Auth) {
		return in, false
	}
	if !m.admin {
		ref, err := s.skillsAdmin.ParseURL(in.URL)
		if err != nil {
			writeSkillAdminError(w, err)
			return in, false
		}
		if s.refuseContributorCreate(w, r, m, ref.Owner, in.Mode, in.Limits != nil) {
			return in, false
		}
	}
	return in, true
}

func (s *Server) skillSource(w http.ResponseWriter, r *http.Request, m *skillManager, id string) {
	u := m.u
	switch r.Method {
	case http.MethodGet:
		src, err := s.skillsAdmin.GetSource(r.Context(), id)
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, src)
	case http.MethodPatch:
		var in skillhub.SourceUpdate
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, redactSkillSecrets(err.Error(), skillAdminDeref(in.Token)))
			return
		}
		before, err := s.skillsAdmin.GetSource(r.Context(), id)
		if err != nil {
			writeSkillAdminError(w, err)
			return
		}
		if in.Auth != nil && s.refuseGitHubSwitch(w, r, u, before, *in.Auth) {
			return
		}
		if in.Mode != nil && *in.Mode == before.Mode {
			in.Mode = nil // unchanged: not a contributor's choice to check
		}
		if s.refuseContributorUpdate(w, r, m, before, &in) {
			return
		}
		// The checks above read before: the update applies only to that
		// source (409 when it changed since, for example an admin rejected
		// it or changed its mode meanwhile).
		in.IfUpdatedAt = before.UpdatedAt
		src, err := s.skillsAdmin.UpdateSource(r.Context(), u.ID, id, in)
		if err != nil {
			writeSkillAdminError(w, err, skillAdminDeref(in.Token))
			return
		}
		// A change of what is fetched or of how commits go live makes an
		// open review question outdated: close it, so nobody approves a
		// version the new settings did not produce.
		if what := skillReviewChanges(before, src); what != "" {
			s.settleSkillReviews(r.Context(), id, "", "the source's "+what+" changed on the Skills page")
		}
		// A contributor's source that waits for approval: a change of what
		// is fetched makes the open approval question outdated. It is
		// closed and asked again with a new dry run.
		if src.Approval == skillhub.ApprovalPending {
			if what := skillApprovalChanges(before, src, in); what != "" {
				s.reaskSkillApproval(r.Context(), src, "the source's "+what+" changed on the Skills page")
			}
		}
		s.auditSkillAdmin(r, u, "skills.source_update", id, skillUpdateSummary(in))
		if sourceSharingChanged(before, src) {
			s.auditSkillChange(r, u, "skills.source_sharing", id, sourceSharingArgs(before), sourceSharingArgs(src))
		}
		writeJSON(w, http.StatusOK, src)
	case http.MethodDelete:
		if err := s.skillsAdmin.DeleteSource(r.Context(), u.ID, id); err != nil {
			writeSkillAdminError(w, err)
			return
		}
		s.settleDeletedSkillSource(r.Context(), id, "the source was deleted")
		s.auditSkillAdmin(r, u, "skills.source_delete", id, "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE")
	}
}

func (s *Server) skillActivate(w http.ResponseWriter, r *http.Request, u *identity.User, id, vid string) {
	if !skillAdminMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.skillsAdmin.Activate(r.Context(), u.ID, id, vid); err != nil {
		writeSkillAdminError(w, err)
		return
	}
	s.settleSkillReviews(r.Context(), id, vid, "made live on the Skills page by "+u.Label())
	s.auditSkillAdmin(r, u, "skills.version_activate", id+" "+vid, "dashboard")
	body := map[string]any{"ok": true}
	if src, err := s.skillsAdmin.GetSource(r.Context(), id); err == nil {
		body["source"] = src
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) skillReject(w http.ResponseWriter, r *http.Request, u *identity.User, id, vid string) {
	if !skillAdminMethod(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Note string `json:"note,omitempty"`
	}
	if err := decodeOptionalBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	note := strings.TrimSpace(body.Note)
	if len(note) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "note: max 2000 characters", "code": "invalid"})
		return
	}
	if note == "" {
		note = "rejected on the Skills page"
	}
	if err := s.skillsAdmin.Reject(r.Context(), u.ID, id, vid, note); err != nil {
		writeSkillAdminError(w, err)
		return
	}
	s.settleSkillReviews(r.Context(), id, vid, "rejected on the Skills page by "+u.Label())
	s.auditSkillAdmin(r, u, "skills.version_reject", id+" "+vid, note)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) skillSetRuns(w http.ResponseWriter, r *http.Request, u *identity.User, id string) {
	if !skillAdminMethod(w, r, http.MethodPut) {
		return
	}
	var body struct {
		Path string        `json:"path"`
		Runs skillhub.Runs `json:"runs"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Path = strings.Trim(strings.TrimSpace(body.Path), "/")
	switch {
	case body.Path == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required", "code": "invalid"})
		return
	case body.Runs != "" && body.Runs != skillhub.RunsAnywhere && body.Runs != skillhub.RunsHost:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `runs must be "anywhere", "host" or "" (clear the override)`, "code": "invalid"})
		return
	}
	if err := s.skillsAdmin.SetRuns(r.Context(), u.ID, id, body.Path, body.Runs); err != nil {
		writeSkillAdminError(w, err)
		return
	}
	runs := string(body.Runs)
	if runs == "" {
		runs = "cleared"
	}
	s.auditSkillAdmin(r, u, "skills.runs_override", id+" "+body.Path, runs)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) skillRotateSecret(w http.ResponseWriter, r *http.Request, u *identity.User, id string) {
	if !skillAdminMethod(w, r, http.MethodPost) {
		return
	}
	secret, err := s.skillsAdmin.RotateWebhookSecret(r.Context(), u.ID, id)
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	path := skillAdminSourcesPrefix + url.PathEscape(id) + "/webhook"
	if src, err := s.skillsAdmin.GetSource(r.Context(), id); err == nil && src.WebhookURLPath != "" {
		path = src.WebhookURLPath
	}
	s.auditSkillAdmin(r, u, "skills.webhook_secret_rotate", id, "")
	// The one response that carries the secret: never cached.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":       secret,
		"payload_url":  s.skillAdminPublicBase(r) + path,
		"content_type": "application/json",
		"events":       []string{"push"},
		"shown_once":   true,
	})
}

// skillWebhook takes a GitHub push (or ping) delivery. It reads no cookie
// and no session: the X-Hub-Signature-256 HMAC over the raw body, with the
// source's webhook secret, is the credential (Admin.HandleWebhook).
func (s *Server) skillWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.skillsAdmin == nil {
		writeError(w, http.StatusNotFound, skillAdminHubOff)
		return
	}
	if !skillAdminIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, SkillWebhookMaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "the webhook body is larger than 5 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read the webhook body")
		return
	}
	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	sig := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if event == "" {
		writeError(w, http.StatusBadRequest, "X-GitHub-Event header required")
		return
	}
	if sig == "" {
		writeError(w, http.StatusUnauthorized, "X-Hub-Signature-256 header required")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), skillWebhookTimeout)
	defer cancel()
	if err := s.skillsAdmin.HandleWebhook(ctx, id, event, body, sig); err != nil {
		if errors.Is(err, skillhub.ErrBadSignature) {
			writeError(w, http.StatusUnauthorized, "signature does not match")
			return
		}
		writeSkillAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// settleSkillReviews closes the open Inbox review items after a decision
// on the Skills page; a failure there never undoes the decision.
func (s *Server) settleSkillReviews(ctx context.Context, sourceID, versionID, reason string) {
	if s.skillReviews == nil {
		return
	}
	if err := s.skillReviews.Settle(ctx, sourceID, versionID, reason); err != nil {
		log.Printf("skills: close review items of %s: %v", sourceID, err)
	}
}

// skillSourceDeleted is the part of the review settler that also closes the
// agents' change proposals against a deleted source
// (skillhub.ReviewInbox.SourceDeleted).
type skillSourceDeleted interface {
	SourceDeleted(ctx context.Context, sourceID, reason string) error
}

// settleDeletedSkillSource closes every open question about a deleted
// source: its review items and, when the settler can, the change
// proposals against it. The Inbox sweep closes what this misses.
func (s *Server) settleDeletedSkillSource(ctx context.Context, sourceID, reason string) {
	if sd, ok := s.skillReviews.(skillSourceDeleted); ok {
		if err := sd.SourceDeleted(ctx, sourceID, reason); err != nil {
			log.Printf("skills: close the open questions of %s: %v", sourceID, err)
		}
		return
	}
	s.settleSkillReviews(ctx, sourceID, "", reason)
}

func (s *Server) auditSkillAdmin(r *http.Request, u *identity.User, event, summary, reason string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: event, AgentID: "user:" + u.ID, ResultSummary: summary, Reason: reason})
}

// skillAdminPublicBase is toolyard's public origin: PublicURL, else the
// request's own host.
func (s *Server) skillAdminPublicBase(r *http.Request) string {
	if base := strings.TrimRight(s.security.PublicURL, "/"); base != "" {
		return base
	}
	scheme := "http"
	if s.security.IsBehindHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// skillAdminMethod answers 405 unless r uses method.
func skillAdminMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeError(w, http.StatusMethodNotAllowed, method+" only")
		return false
	}
	return true
}

// decodeOptionalBody is decode for a body that may be absent.
func decodeOptionalBody(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decode(r, dst)
}

// writeSkillAdminError maps skillhub errors to HTTP statuses, with a stable
// code for the dashboard. secrets (a token from the request) are redacted
// from the message.
func writeSkillAdminError(w http.ResponseWriter, err error, secrets ...string) {
	if until, ok := skillhub.RateLimitedUntil(err); ok {
		writeSkillRateLimited(w, until)
		return
	}
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, skillhub.ErrDisabled):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": skillAdminHubOff, "code": "skills_hub_off"})
		return
	case errors.Is(err, skillhub.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, skillhub.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid"
	case errors.Is(err, skillhub.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, skillhub.ErrNeedsToken):
		status, code = http.StatusUnprocessableEntity, "needs_token"
	case errors.Is(err, skillhub.ErrLimit):
		status, code = http.StatusUnprocessableEntity, "limit"
	case errors.Is(err, skillhub.ErrBadSignature):
		status, code = http.StatusUnauthorized, "bad_signature"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "timeout"
	}
	msg := redactSkillSecrets(skillAdminErrorText(err), secrets...)
	if status == http.StatusInternalServerError {
		log.Printf("skills admin: %s", msg)
	}
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeSkillRateLimited answers a GitHub rate limit: 429 with when to try
// again, and the way out (a default token).
func writeSkillRateLimited(w http.ResponseWriter, until time.Time) {
	secs := int(time.Until(until).Seconds() + 0.999)
	secs = max(secs, 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       "GitHub rate limit reached until " + until.UTC().Format("15:04 MST") + ": add a GitHub token in Skills, GitHub access",
		"code":        "rate_limited",
		"retry_after": secs,
		"reset_at":    until.UTC().Format(time.RFC3339),
	})
}

// skillAdminErrorText is an error's message without the package prefix.
func skillAdminErrorText(err error) string {
	return strings.TrimPrefix(err.Error(), "skillhub: ")
}

// redactSkillSecrets removes every non-empty secret from msg.
func redactSkillSecrets(msg string, secrets ...string) string {
	for _, sec := range secrets {
		if sec = strings.TrimSpace(sec); len(sec) >= 4 {
			msg = strings.ReplaceAll(msg, sec, "[redacted]")
		}
	}
	return msg
}

// skillReviewChanges names the settings that changed between before and
// after which make an open review outdated (mode, branch, folder, pinned
// commit), or "".
func skillReviewChanges(before, after *skillhub.Source) string {
	var f []string
	if before.Mode != after.Mode {
		f = append(f, "update mode")
	}
	if before.Branch != after.Branch {
		f = append(f, "branch")
	}
	if strings.Trim(before.Path, "/") != strings.Trim(after.Path, "/") {
		f = append(f, "folder")
	}
	if before.PinnedSHA != after.PinnedSHA {
		f = append(f, "pinned commit")
	}
	return strings.Join(f, ", ")
}

// skillUpdateSummary names the fields a PATCH changed, for the audit row
// (never their values: one of them may be a token).
func skillUpdateSummary(in skillhub.SourceUpdate) string {
	var f []string
	add := func(set bool, name string) {
		if set {
			f = append(f, name)
		}
	}
	add(in.Branch != nil, "branch")
	add(in.Path != nil, "path")
	add(in.Exclude != nil, "exclude")
	add(in.Mode != nil, "mode")
	add(in.PinnedSHA != nil, "pinned_sha")
	add(in.Auth != nil, "auth")
	add(in.Token != nil, "token")
	add(in.ClearToken, "clear_token")
	add(in.PollSeconds != nil, "poll_seconds")
	add(in.Enabled != nil, "enabled")
	add(in.Visibility != nil, "visibility")
	add(in.Members != nil, "members")
	add(in.Blocked != nil, "blocked")
	add(in.DefaultRuns != nil, "default_runs")
	add(in.Limits != nil, "limits")
	return "changed: " + strings.Join(f, ", ")
}

func skillAdminDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

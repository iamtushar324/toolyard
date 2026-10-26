package api

// Skills hub sharing controls and skill contributors (TEC-1782, worker F2;
// docs/skills-hub.md "Sharing" and "Contributors").
//
// Who manages skill sources:
//
//   - An admin manages every source and every route.
//   - A skill contributor (a member an admin named in the Contributors
//     panel) uses the Skills page in a limited view: the add-source
//     wizard (inspect, preview, create) and the sources they created
//     (read, edit, sync, pause, delete, runs and sharing, webhook secret).
//     Their new source waits for an admin's approval: it syncs, and agents
//     see nothing of it until an admin approves it in the Inbox or on the
//     source page. They choose review updates, or automatic ones only for
//     a trusted owner; never pinned, never other limits. Other people's
//     sources answer 404; versions are made live, rejected and approved by
//     admins only, and the default GitHub token, the trusted owners and
//     the Contributors panel stay admin-only. A contributor who is removed
//     keeps their sources, which only admins can change then.
//
// Routes (besides the source routes in skills_admin_routes.go):
//
//	GET    /v1/skills/admin/me                       {admin, contributor}: what the signed-in person may do on the Skills page
//	GET    /v1/skills/admin/people                   active people for the sharing pickers (admins and contributors)
//	GET    /v1/skills/admin/contributors             admin: the contributors
//	PUT    /v1/skills/admin/contributors/{user id}   admin: add one
//	DELETE /v1/skills/admin/contributors/{user id}   admin: remove one (their sources stay)
//	PUT    /v1/skills/sources/{id}/skills/sharing    {path, mode, members, blocked}: one skill's sharing; mode "" or "inherit" clears
//	POST   /v1/skills/sources/{id}/approval          admin: {decision: "approve"|"reject", note?} for a contributor's source
//
// Every change is audited with who did it and the setting before and after.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

const (
	skillMePath              = "/v1/skills/admin/me"
	skillPeoplePath          = "/v1/skills/admin/people"
	skillContributorsPath    = "/v1/skills/admin/contributors"
	skillContributorsPrefix  = skillContributorsPath + "/"
	skillAdminOnlyText       = "only an admin can do this"
	skillContributorOnlyText = "only an admin or a skill contributor can manage skill sources"
)

// skillManager is who uses a Skills page route: an admin, or a skill
// contributor limited to their own sources.
type skillManager struct {
	u     *identity.User
	admin bool
}

// owns reports whether the manager may change src. A contributor never
// owns the built-in source (toolyard's own skills), whatever it names as
// its creator.
func (m *skillManager) owns(src *skillhub.Source) bool {
	return m.admin || (src != nil && src.Kind != skillhub.KindBuiltin && src.CreatedBy == m.u.ID)
}

// skillApprovals is the part of the review settler that asks for and
// closes a contributor source's approval (skillhub.ReviewInbox).
type skillApprovals interface {
	RequestApproval(ctx context.Context, src skillhub.Source, prev *skillhub.Preview, contributor string) error
	SettleApproval(ctx context.Context, sourceID, reason string) error
}

func (s *Server) registerSkillsSharingRoutes(mux *http.ServeMux) {
	// Literal paths: the route tests read them from the source.
	mux.HandleFunc("/v1/skills/admin/me", s.skillMe)
	mux.HandleFunc("/v1/skills/admin/people", s.skillPeople)
	mux.HandleFunc("/v1/skills/admin/contributors", s.skillContributors)
	mux.HandleFunc("/v1/skills/admin/contributors/", s.skillContributor)
}

// skillContributorAdmin is the contributors store, or nil when the hub has
// none.
func (s *Server) skillContributorAdmin() skillhub.ContributorsAdmin {
	ca, _ := s.skillsAdmin.(skillhub.ContributorsAdmin)
	return ca
}

// isSkillContributor reports whether uid is a skill contributor; a failed
// read counts as no.
func (s *Server) isSkillContributor(ctx context.Context, uid string) bool {
	ca := s.skillContributorAdmin()
	if ca == nil {
		return false
	}
	ok, err := ca.IsContributor(ctx, uid)
	if err != nil {
		log.Printf("skills: read contributors: %v", err)
		return false
	}
	return ok
}

// skillManagerUser answers 401/403 for anyone but an admin or a skill
// contributor, and 404 while the hub is off.
func (s *Server) skillManagerUser(w http.ResponseWriter, r *http.Request) (*skillManager, bool) {
	u, err := s.requireUserFull(r)
	if err != nil {
		writeAuthError(w, err)
		return nil, false
	}
	if s.skillsAdmin == nil {
		writeError(w, http.StatusNotFound, skillAdminHubOff)
		return nil, false
	}
	if u.Role == identity.RoleAdmin {
		return &skillManager{u: u, admin: true}, true
	}
	if s.isSkillContributor(r.Context(), u.ID) {
		return &skillManager{u: u}, true
	}
	// The role guard's own answer, so a member sees no difference.
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_only", "message": skillContributorOnlyText})
	return nil, false
}

// requireSkillAdmin answers 403 for a contributor on an admin-only route.
func requireSkillAdmin(w http.ResponseWriter, m *skillManager) bool {
	if m.admin {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_only", "message": skillAdminOnlyText})
	return false
}

// skillOwnedSource reads source id for m: a contributor gets 404 for a
// source they did not create (its existence is not theirs to learn).
func (s *Server) skillOwnedSource(w http.ResponseWriter, r *http.Request, m *skillManager, id string) (*skillhub.Source, bool) {
	src, err := s.skillsAdmin.GetSource(r.Context(), id)
	if err != nil {
		writeSkillAdminError(w, err)
		return nil, false
	}
	if !m.owns(src) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found: source " + id, "code": "not_found"})
		return nil, false
	}
	return src, true
}

// maxPendingContributorSources caps the sources one contributor may have
// waiting for approval at once.
const maxPendingContributorSources = 3

// refuseContributor answers 403 contributor_limit with msg.
func refuseContributor(w http.ResponseWriter, msg string) bool {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": msg, "code": "contributor_limit"})
	return true
}

// refuseContributorCreate guards what a contributor may choose for a new
// source: updates by review, or automatic for a trusted owner (an
// admin's list); never pinned; never other limits. owner is the
// repository owner (the engine checks again with the owner GitHub
// answers with).
func (s *Server) refuseContributorCreate(w http.ResponseWriter, r *http.Request, m *skillManager, owner string, mode skillhub.UpdateMode, limits bool) bool {
	switch {
	case m.admin:
		return false
	case limits:
		return refuseContributor(w, "only an admin can change a source's limits")
	case mode == skillhub.ModePinned:
		return refuseContributor(w, "a skill contributor's source updates by review (or automatically for a trusted owner); only an admin can pin one")
	case mode == skillhub.ModeAuto && !s.skillsAdmin.TrustedOwner(r.Context(), owner):
		return refuseContributor(w, "automatic updates are for trusted repository owners only; choose review, so an admin approves each new commit")
	}
	return false
}

// refuseContributorUpdate guards what a contributor may change on their
// own source (in.Mode is nil when unchanged). Only an admin hides or
// shows a source, changes its limits or pinned commit, re-enables a
// rejected source, or changes what a source fetches when its new commits
// go live without review (pinned, or automatic for an owner who is not
// trusted).
func (s *Server) refuseContributorUpdate(w http.ResponseWriter, r *http.Request, m *skillManager, before *skillhub.Source, in *skillhub.SourceUpdate) bool {
	if m.admin {
		return false
	}
	trusted := s.skillsAdmin.TrustedOwner(r.Context(), before.Owner)
	fetch := (in.Branch != nil && strings.TrimSpace(*in.Branch) != before.Branch) ||
		(in.Path != nil && strings.Trim(strings.TrimSpace(*in.Path), "/") != strings.Trim(before.Path, "/")) || in.Exclude != nil
	unreviewed := before.Mode == skillhub.ModePinned || (before.Mode == skillhub.ModeAuto && !trusted)
	switch {
	case in.Hidden != nil:
		return refuseContributor(w, "only an admin can hide or show a source")
	case in.Limits != nil:
		return refuseContributor(w, "only an admin can change a source's limits")
	case in.PinnedSHA != nil && strings.TrimSpace(*in.PinnedSHA) != before.PinnedSHA:
		return refuseContributor(w, "only an admin can change a source's pinned commit")
	case in.Mode != nil && *in.Mode == skillhub.ModePinned:
		return refuseContributor(w, "a skill contributor's source updates by review (or automatically for a trusted owner); only an admin can pin one")
	case in.Mode != nil && *in.Mode == skillhub.ModeAuto && !trusted:
		return refuseContributor(w, "automatic updates are for trusted repository owners only; choose review, so an admin approves each new commit")
	case fetch && unreviewed && in.Mode == nil:
		return refuseContributor(w, "this source's new commits go live without review, so only an admin can change its branch, folder or excludes; switch it to review first")
	case in.Enabled != nil && *in.Enabled && before.Approval == skillhub.ApprovalRejected:
		return refuseContributor(w, "an admin rejected this source; only an admin can turn it on again")
	}
	return false
}

// refuseTooManyPending answers 409 when a contributor already has
// maxPendingContributorSources sources waiting for approval.
func (s *Server) refuseTooManyPending(w http.ResponseWriter, r *http.Request, m *skillManager) bool {
	list, err := s.skillsAdmin.ListSources(r.Context())
	if err != nil {
		writeSkillAdminError(w, err)
		return true
	}
	n := 0
	for i := range list {
		if list[i].CreatedBy == m.u.ID && list[i].Approval == skillhub.ApprovalPending {
			n++
		}
	}
	if n < maxPendingContributorSources {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("you already have %d sources waiting for an admin's approval; wait for a decision, or delete one", n), "code": "conflict"})
	return true
}

// skillApprovalChanges names what changed between before and after that
// makes an open approval question outdated (what is fetched, how, and how
// commits go live), or "".
func skillApprovalChanges(before, after *skillhub.Source, in skillhub.SourceUpdate) string {
	var f []string
	add := func(changed bool, name string) {
		if changed {
			f = append(f, name)
		}
	}
	add(before.Branch != after.Branch, "branch")
	add(strings.Trim(before.Path, "/") != strings.Trim(after.Path, "/"), "folder")
	add(in.Exclude != nil && strings.Join(before.Exclude, "\n") != strings.Join(after.Exclude, "\n"), "excludes")
	add(before.Auth != after.Auth || (in.Token != nil && strings.TrimSpace(*in.Token) != "") || in.ClearToken, "credential")
	add(before.Mode != after.Mode || before.PinnedSHA != after.PinnedSHA, "update mode")
	return strings.Join(f, ", ")
}

// reaskSkillApproval closes a pending source's approval question and asks
// again with a dry run of its new settings (counts unknown when the dry
// run fails).
func (s *Server) reaskSkillApproval(ctx context.Context, src *skillhub.Source, reason string) {
	ap, ok := s.skillReviews.(skillApprovals)
	if !ok {
		return
	}
	if err := ap.SettleApproval(ctx, src.ID, reason); err != nil {
		log.Printf("skills: close the approval question of %s: %v", src.ID, err)
	}
	var prev *skillhub.Preview
	if sa, ok := s.skillsAdmin.(skillhub.SharingAdmin); ok {
		p, err := sa.PreviewSource(ctx, src.ID)
		if err != nil {
			log.Printf("skills: dry run for the approval question of %s: %s", src.ID, skillAdminErrorText(err))
		} else {
			prev = p
		}
	}
	who := src.CreatedBy
	if u, err := s.identity.GetUserByID(ctx, src.CreatedBy); err == nil {
		who = u.Label()
	}
	if err := ap.RequestApproval(ctx, *src, prev, who); err != nil {
		log.Printf("skills: approval question for %s: %v", src.ID, err)
	}
}

// skillMe is GET /v1/skills/admin/me: what the signed-in person may do on
// the Skills page. Any signed-in person may ask; 404 while the hub is off.
func (s *Server) skillMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	u, err := s.requireUserFull(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if s.skillsAdmin == nil {
		writeError(w, http.StatusNotFound, skillAdminHubOff)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":     u.ID,
		"admin":       u.Role == identity.RoleAdmin,
		"contributor": s.isSkillContributor(r.Context(), u.ID),
	})
}

// skillPerson is one person in the sharing pickers.
type skillPerson struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"` // admins only
	Role        string `json:"role"`
}

// skillPeople is GET /v1/skills/admin/people: the active people a source
// or skill can be shared with or blocked from. A contributor gets names
// only (no email).
func (s *Server) skillPeople(w http.ResponseWriter, r *http.Request) {
	m, ok := s.skillManagerUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	list, err := s.identity.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list people")
		return
	}
	out := make([]skillPerson, 0, len(list))
	for _, u := range list {
		if u.Status != identity.StatusActive {
			continue
		}
		p := skillPerson{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role}
		if m.admin {
			p.Email = u.Email
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out})
}

// skillContributors is GET /v1/skills/admin/contributors (admin).
func (s *Server) skillContributors(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.skillAdminUser(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ca := s.skillContributorAdmin()
	if ca == nil {
		writeError(w, http.StatusNotFound, "skill contributors are not available on this server")
		return
	}
	list, err := ca.Contributors(r.Context())
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contributors": list})
}

// skillContributor is PUT or DELETE /v1/skills/admin/contributors/{user
// id} (admin; an operator token needs the owner scope). Only an active
// member can be added; removing keeps the person's sources.
func (s *Server) skillContributor(w http.ResponseWriter, r *http.Request) {
	u, ok := s.skillAdminUser(w, r)
	if !ok {
		return
	}
	uid := strings.TrimPrefix(r.URL.Path, skillContributorsPrefix)
	if !skillAdminIDPattern.MatchString(uid) {
		http.NotFound(w, r)
		return
	}
	ca := s.skillContributorAdmin()
	if ca == nil {
		writeError(w, http.StatusNotFound, "skill contributors are not available on this server")
		return
	}
	var on bool
	switch r.Method {
	case http.MethodPut:
		on = true
		target, err := s.identity.GetUserByID(r.Context(), uid)
		if err != nil || target.Status != identity.StatusActive {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active person with that ID", "code": "not_found"})
			return
		}
	case http.MethodDelete:
	default:
		writeError(w, http.StatusMethodNotAllowed, "PUT or DELETE")
		return
	}
	if err := ca.SetContributor(r.Context(), u.ID, uid, on); err != nil {
		writeSkillAdminError(w, err)
		return
	}
	event := "skills.contributor_add"
	if !on {
		event = "skills.contributor_remove"
	}
	s.auditSkillChange(r, u, event, uid, nil, map[string]any{"user_id": uid, "contributor": on})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": uid, "contributor": on})
}

// skillSetSharing is PUT /v1/skills/sources/{id}/skills/sharing: one
// skill's own sharing ({path, mode, members, blocked}; mode "" or
// "inherit" with nobody blocked clears it).
func (s *Server) skillSetSharing(w http.ResponseWriter, r *http.Request, m *skillManager, id string) {
	if !skillAdminMethod(w, r, http.MethodPut) {
		return
	}
	sa, ok := s.skillsAdmin.(skillhub.SharingAdmin)
	if !ok {
		writeError(w, http.StatusNotFound, "sharing controls are not available on this server")
		return
	}
	var body struct {
		Path    string              `json:"path"`
		Mode    skillhub.Visibility `json:"mode"`
		Members []string            `json:"members"`
		Blocked []string            `json:"blocked"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Path = strings.Trim(strings.TrimSpace(body.Path), "/")
	after := skillhub.SkillSharing{Mode: body.Mode, Members: body.Members, Blocked: body.Blocked}
	before, err := sa.SetSkillSharing(r.Context(), m.u.ID, id, body.Path, after)
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	if after.Mode == "inherit" {
		after.Mode = skillhub.VisibleInherit
	}
	if after.Mode != skillhub.VisibleMembers {
		after.Members = nil
	}
	s.auditSkillChange(r, m.u, "skills.skill_sharing", id+" "+body.Path, sharingArgs(before), sharingArgs(after))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": body.Path, "sharing": after})
}

func sharingArgs(sh skillhub.SkillSharing) map[string]any {
	mode := string(sh.Mode)
	if mode == "" {
		mode = "inherit"
	}
	return map[string]any{"mode": mode, "members": nonNilIDs(sh.Members), "blocked": nonNilIDs(sh.Blocked)}
}

func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// sourceSharingArgs is a source's sharing, for the audit row.
func sourceSharingArgs(src *skillhub.Source) map[string]any {
	return map[string]any{"visibility": string(src.Visibility), "members": nonNilIDs(src.Members), "blocked": nonNilIDs(src.Blocked),
		"hidden": src.Hidden}
}

// sourceSharingChanged reports whether a source's audience, members or
// block list differ.
func sourceSharingChanged(a, b *skillhub.Source) bool {
	x, _ := json.Marshal(sourceSharingArgs(a))
	y, _ := json.Marshal(sourceSharingArgs(b))
	return string(x) != string(y)
}

// skillDecideApproval is POST /v1/skills/sources/{id}/approval (admin):
// approve or reject a contributor's source on the source page. It closes
// the open Inbox question.
func (s *Server) skillDecideApproval(w http.ResponseWriter, r *http.Request, m *skillManager, id string) {
	if !skillAdminMethod(w, r, http.MethodPost) || !requireSkillAdmin(w, m) {
		return
	}
	sa, ok := s.skillsAdmin.(skillhub.SharingAdmin)
	if !ok {
		writeError(w, http.StatusNotFound, "approvals are not available on this server")
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Note     string `json:"note,omitempty"`
		// SHA is the commit of the waiting version the admin saw on the
		// source page: only that version goes live with the approval.
		SHA string `json:"sha,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	note := strings.TrimSpace(body.Note)
	if len(note) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "note: max 2000 characters", "code": "invalid"})
		return
	}
	var approve bool
	switch body.Decision {
	case "approve":
		approve = true
	case "reject":
		if note == "" {
			note = "rejected on the Skills page"
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `decision must be "approve" or "reject"`, "code": "invalid"})
		return
	}
	before, err := s.skillsAdmin.GetSource(r.Context(), id)
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	src, err := sa.DecideApproval(r.Context(), m.u.ID, id, approve, note, strings.TrimSpace(body.SHA))
	if err != nil {
		writeSkillAdminError(w, err)
		return
	}
	verb, event := "approved", "skills.source_approve"
	if !approve {
		verb, event = "rejected", "skills.source_reject"
	}
	if ap, ok := s.skillReviews.(skillApprovals); ok {
		if err := ap.SettleApproval(r.Context(), id, verb+" on the Skills page by "+m.u.Label()); err != nil {
			log.Printf("skills: close the approval question of %s: %v", id, err)
		}
	}
	s.auditSkillChange(r, m.u, event, id, map[string]any{"approval": string(before.Approval), "enabled": before.Enabled},
		map[string]any{"approval": string(src.Approval), "enabled": src.Enabled, "note": note})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "source": src})
}

// requestSkillApproval asks the owner to approve a contributor's new
// source; a failure leaves the source pending, for an admin to approve on
// the Skills page.
func (s *Server) requestSkillApproval(ctx context.Context, src *skillhub.Source, prev *skillhub.Preview, u *identity.User) {
	ap, ok := s.skillReviews.(skillApprovals)
	if !ok {
		return
	}
	if err := ap.RequestApproval(ctx, *src, prev, u.Label()); err != nil {
		log.Printf("skills: approval question for %s: %v", src.ID, err)
	}
}

// auditSkillChange writes a skills audit row with the setting before and
// after (as arguments: never a token).
func (s *Server) auditSkillChange(r *http.Request, u *identity.User, event, summary string, before, after any) {
	if s.audit == nil {
		return
	}
	args, err := json.Marshal(map[string]any{"before": before, "after": after})
	if err != nil {
		args = nil
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: event, AgentID: "user:" + u.ID, ResultSummary: summary, Arguments: args})
}

// errSkillManager is used by the inspect route: the GitHub connection read
// is for the signed-in person only.
var errSkillManager = errors.New("only a signed-in admin or skill contributor can read with their GitHub connection")

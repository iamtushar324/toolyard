package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Skill bundles. skills.bundle (and POST /v1/skills/bundle, which routes
// to it) checks that the caller can see every skill it names and returns a
// short-lived URL; GET /v1/skills/bundle/{ticket} streams the .tar.gz
// (skillhub.Reader.Bundle). The ticket in the URL is the download's only
// credential, so a person can open it in a browser or pass it to curl:
// 256 random bits, stored as its sha256 in skill_bundle_tickets (migration
// 0071) so any replica serves it, redeemable once, for SkillBundleTTL.
// The download is made as the caller that asked for it: the visibility
// check runs again when it is redeemed, against the caller's current
// scope, and the download writes an audit row with each skill's version.

// SkillBundleTTL is how long a bundle URL works.
const SkillBundleTTL = 10 * time.Minute

// SkillBundlePathPrefix is the download route; the ticket follows it.
const SkillBundlePathPrefix = "/v1/skills/bundle/"

// maxLiveBundleTickets bounds the unused, unexpired tickets one caller may
// hold, so a loop cannot fill the table.
const maxLiveBundleTickets = 20

// skillBundleTicketPrefix starts every ticket, so a leaked one is easy to
// recognise.
const skillBundleTicketPrefix = "tyskb_"

var (
	// ErrSkillBundleTicket is a download ticket that is unknown, used or
	// expired. The three answer alike: a ticket is not a probe.
	ErrSkillBundleTicket = errors.New("this skills bundle link is unknown, already used or expired: ask for a new one with skills.bundle")
	// errSkillBundleOff is a bundle request on a server without tickets.
	errSkillBundleOff = errors.New("skill bundles are not available on this server")
	// errTooManyBundles is the per-caller ticket cap.
	errTooManyBundles = fmt.Errorf("%w: too many unused bundle links; use one of them or wait %s", skillhub.ErrLimit, SkillBundleTTL)
)

// SkillBundleTickets stores the single-use download tickets of skill
// bundles in skill_bundle_tickets.
type SkillBundleTickets struct {
	db  *store.DB
	now func() time.Time
}

// NewSkillBundleTickets returns the ticket store on db.
func NewSkillBundleTickets(db *store.DB) *SkillBundleTickets {
	return &SkillBundleTickets{db: db, now: time.Now}
}

func hashBundleTicket(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// issue stores a ticket for callerID to download refs and returns it once.
// Expired rows are swept on the way.
func (t *SkillBundleTickets) issue(ctx context.Context, callerID string, refs []string) (string, time.Time, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	ticket := skillBundleTicketPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	skills, err := json.Marshal(refs)
	if err != nil {
		return "", time.Time{}, err
	}
	now := t.now()
	expires := now.Add(SkillBundleTTL)
	_, _ = t.db.ExecContext(ctx, `DELETE FROM skill_bundle_tickets WHERE expires_at < ?`, now.UnixMilli())
	// The cap and the insert are one statement; on Postgres two concurrent
	// inserts may both pass the count, which overshoots the cap by a few.
	// That bounds spam; it is not a security limit. The SELECT list is
	// cast so Postgres knows the parameter types.
	res, err := t.db.ExecContext(ctx, `
        INSERT INTO skill_bundle_tickets(id_hash, caller_id, skills, created_at, expires_at)
        SELECT CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS BIGINT), CAST(? AS BIGINT)
        WHERE (SELECT count(*) FROM skill_bundle_tickets
               WHERE caller_id = ? AND used_at IS NULL AND expires_at > ?) < ?`,
		hashBundleTicket(ticket), callerID, string(skills), now.UnixMilli(), expires.UnixMilli(),
		callerID, now.UnixMilli(), maxLiveBundleTickets)
	if err != nil {
		return "", time.Time{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", time.Time{}, errTooManyBundles
	}
	return ticket, expires, nil
}

// redeem uses ticket up and returns who asked for the bundle and what it
// holds. The conditional UPDATE is the whole check, so two downloads of
// one link race for one row.
func (t *SkillBundleTickets) redeem(ctx context.Context, ticket string) (callerID string, refs []string, err error) {
	if !validBundleTicket(ticket) {
		return "", nil, ErrSkillBundleTicket
	}
	h := hashBundleTicket(ticket)
	now := t.now().UnixMilli()
	res, err := t.db.ExecContext(ctx,
		`UPDATE skill_bundle_tickets SET used_at = ? WHERE id_hash = ? AND used_at IS NULL AND expires_at > ?`, now, h, now)
	if err != nil {
		return "", nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", nil, ErrSkillBundleTicket
	}
	var skills string
	err = t.db.QueryRowContext(ctx, `SELECT caller_id, skills FROM skill_bundle_tickets WHERE id_hash = ?`, h).Scan(&callerID, &skills)
	if errors.Is(err, sql.ErrNoRows) {
		// Swept between the two statements: it had expired.
		return "", nil, ErrSkillBundleTicket
	}
	if err != nil {
		return "", nil, err
	}
	if err := json.Unmarshal([]byte(skills), &refs); err != nil {
		return "", nil, err
	}
	return callerID, refs, nil
}

// validBundleTicket checks a ticket's shape before any lookup.
func validBundleTicket(ticket string) bool {
	rest, ok := strings.CutPrefix(ticket, skillBundleTicketPrefix)
	if !ok || len(rest) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil
}

// bundleSkill is one skill of a bundle as the answers name it.
type bundleSkill struct {
	URI    string `json:"uri"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// skillBundleAnswer is what skills.bundle answers.
type skillBundleAnswer struct {
	URL       string        `json:"url"`
	ExpiresAt string        `json:"expires_at"`
	Skills    []bundleSkill `json:"skills"`
	Next      string        `json:"next"`
	Note      string        `json:"note,omitempty"`
}

// skillsBundleMaxBytes caps the files one bundle carries, as the Reader
// does (skillhub bundleMaxBytes): checked when the link is issued, so the
// download does not fail on it later.
const skillsBundleMaxBytes = 64 << 20

// resolveBundle checks that v can see every ref and names the version of
// each, in order, without duplicates. Like the archive itself, it refuses
// two different skills with one name (the archive has one folder per
// name: ErrConflict) and more than skillsBundleMaxBytes of files
// (ErrLimit).
func resolveBundle(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, refs []string) ([]bundleSkill, error) {
	out := make([]bundleSkill, 0, len(refs))
	seen := map[string]bool{}
	names := map[string]string{}
	var total int64
	for _, ref := range refs {
		s, err := hub.reader.Get(ctx, v, ref, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		if seen[s.URI] {
			continue
		}
		if prev, dup := names[s.Name]; dup {
			return nil, fmt.Errorf("%w: two chosen skills are named %q (%s and %s); a bundle has one folder per name, so bundle them separately",
				skillhub.ErrConflict, s.Name, prev, s.URI)
		}
		seen[s.URI], names[s.Name] = true, s.URI
		for _, f := range s.Files {
			total += f.Size
		}
		out = append(out, bundleSkill{URI: s.URI, Name: s.Name, Source: s.Source, Repo: s.Repo, Commit: s.Commit})
	}
	if total > skillsBundleMaxBytes {
		return nil, fmt.Errorf("%w: the bundle would hold %d bytes of files; the limit is %d, so bundle fewer skills", skillhub.ErrLimit, total, skillsBundleMaxBytes)
	}
	return out, nil
}

// issueSkillBundle is skills.bundle: it resolves refs for v, stores a
// ticket for the ctx caller and answers with the download URL.
func (g *Gateway) issueSkillBundle(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, refs []string) (*skillBundleAnswer, error) {
	if hub.tickets == nil {
		return nil, errSkillBundleOff
	}
	if len(refs) == 0 || len(refs) > skillsBundleMax {
		return nil, fmt.Errorf("%w: skills: name 1 to %d skills", skillhub.ErrInvalid, skillsBundleMax)
	}
	skills, err := resolveBundle(ctx, hub, v, refs)
	if err != nil {
		return nil, err
	}
	uris := make([]string, len(skills))
	for i, s := range skills {
		uris[i] = s.URI
	}
	ticket, expires, err := hub.tickets.issue(ctx, agentIDFromContext(ctx), uris)
	if err != nil {
		return nil, err
	}
	link := SkillBundlePathPrefix + ticket
	out := &skillBundleAnswer{ExpiresAt: expires.UTC().Format(time.RFC3339), Skills: skills}
	if base := strings.TrimRight(g.publicURL, "/"); base != "" {
		out.URL = base + link
	} else {
		out.URL = link
		out.Note = "This gateway has no -public-url set, so url is a path: prefix it with the toolyard address you connect to."
	}
	out.Next = "Download it once within 10 minutes, then unpack it where your client reads skills: " +
		"curl -sf -o skills.tar.gz " + shellQuote(out.URL) + " && mkdir -p ~/.claude/skills && tar -xzf skills.tar.gz -C ~/.claude/skills. " +
		"The URL is the credential: do not share it."
	return out, nil
}

// SkillBundle is a redeemed download: the skills it holds, as the caller
// that asked for it sees them now.
type SkillBundle struct {
	// Skills are the bundled skills and their versions.
	Skills []bundleSkill
	g      *Gateway
	hub    *skillHubConfig
	caller string
}

// OpenSkillBundle redeems a download ticket for GET
// /v1/skills/bundle/{ticket}: it uses the ticket up, checks again that its
// caller may still use skills.bundle (the same gate as every non-tool
// skills path: blocked or deleted agents, a new deny list entry or deny
// policy refuse it) and can still see every skill, and returns the bundle
// to Write. ErrSkillBundleTicket is an unknown, used or expired ticket; a
// refused caller, or a skill it can no longer see, is skillhub.ErrNotFound;
// skillhub.ErrConflict and skillhub.ErrLimit are a bundle the archive
// cannot hold; skillhub.ErrDisabled means the hub is off.
func (g *Gateway) OpenSkillBundle(ctx context.Context, ticket string) (*SkillBundle, error) {
	hub := g.skillHub.Load()
	if hub == nil {
		return nil, skillhub.ErrDisabled
	}
	if hub.tickets == nil {
		return nil, ErrSkillBundleTicket
	}
	caller, refs, err := hub.tickets.redeem(ctx, ticket)
	if err != nil {
		return nil, err
	}
	b := &SkillBundle{g: g, hub: hub, caller: caller}
	ctx = b.callerContext(ctx)
	if !g.skillsAllowed(ctx, SkillsBundleTool) {
		b.audit(ctx, refs, nil, errSkillsRefused)
		return nil, fmt.Errorf("%w: %w", skillhub.ErrNotFound, errSkillsRefused)
	}
	skills, err := resolveBundle(ctx, hub, g.skillViewer(ctx, hub), refs)
	if err != nil {
		b.audit(ctx, refs, nil, err)
		return nil, err
	}
	b.Skills = skills
	return b, nil
}

// bundleAuditTool is the audit row's tool name for a bundle download.
const bundleAuditTool = "GET " + SkillBundlePathPrefix

// audit writes the download's audit row: every skill and its version in
// the arguments (a list, never cut), a short summary, and the refusal or
// failure when there was one.
func (b *SkillBundle) audit(ctx context.Context, refs []string, skills []bundleSkill, err error) {
	if b.g.audit == nil {
		return
	}
	type entry struct {
		URI  string `json:"uri"`
		Repo string `json:"repo,omitempty"`
	}
	list := make([]entry, 0, max(len(skills), len(refs)))
	for _, s := range skills {
		list = append(list, entry{URI: s.URI, Repo: s.Repo})
	}
	if skills == nil {
		for _, r := range refs {
			list = append(list, entry{URI: r})
		}
	}
	args, _ := json.Marshal(map[string]any{"skills": list})
	decision, summary := "allow", fmt.Sprintf("bundle of %d skills", len(list))
	if len(list) > 0 {
		summary += ": " + list[0].URI + " " + list[0].Repo
		if len(list) > 1 {
			summary += fmt.Sprintf(" and %d more (see arguments)", len(list)-1)
		}
	}
	switch {
	case errors.Is(err, errSkillsRefused):
		decision, summary = "deny", summary+" (refused: access or policy)"
	case err != nil:
		code, _ := skillsErrorText(err, "")
		decision, summary = "error", summary+" ("+code+")"
	}
	_ = b.g.audit.Write(ctx, audit.Event{
		EventType: EventSkillRead, AgentID: b.caller, UpstreamName: skillsUpstream, ToolName: bundleAuditTool,
		Decision: decision, ResultSummary: clipText(summary, 500), Arguments: args,
	})
}

// callerContext is ctx as the caller that asked for the bundle: the
// download request itself carries no credential, so its caller id and the
// raiser that id implies stand in.
func (b *SkillBundle) callerContext(ctx context.Context) context.Context {
	ctx = WithAgentID(ctx, b.caller)
	return actor.WithRaiser(ctx, b.g.resolveRaiser(ctx, b.caller, viaSkillsREST))
}

// Write streams the bundle's .tar.gz into w and audits the download.
func (b *SkillBundle) Write(ctx context.Context, w io.Writer) error {
	ctx = b.callerContext(ctx)
	refs := make([]string, len(b.Skills))
	for i, s := range b.Skills {
		refs[i] = s.URI
	}
	err := b.hub.reader.Bundle(ctx, b.g.skillViewer(ctx, b.hub), refs, w)
	b.audit(ctx, refs, b.Skills, err)
	return err
}

// viaSkillsREST is the raiser's path in for a bundle download.
const viaSkillsREST = "rest"

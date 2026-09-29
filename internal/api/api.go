// Package api exposes the dashboard's REST surface and SSE feeds.
//
// Routes (all under /v1):
//
//	POST /v1/auth/setup           one-time create the local user
//	POST /v1/auth/login           username + password -> session cookie
//	POST /v1/auth/clerk/session   Clerk session JWT -> session cookie
//	GET  /v1/auth/config          which sign-in methods are on (public)
//	POST /v1/auth/logout
//	GET  /v1/auth/me              current user, with role
//
//	GET  /v1/users                admin: every user + grantable groups
//	PATCH /v1/users/{id}          admin: role / status / servers
//	POST /v1/users/{id}/revoke-sessions
//	GET  /v1/me/servers           groups the caller may use
//
//	GET  /v1/agents
//	POST /v1/agents/enroll        creates a code (op uses it on the agent)
//	POST /v1/agents/exchange      agent swaps code for a long-lived token
//
//	GET  /v1/approvals
//	GET  /v1/approvals/{id}
//	POST /v1/approvals/{id}/decide      {"action":"allowed|denied"}
//	POST /v1/approvals/decide-by-token  one-tap approve via signed token
//
//	GET  /v1/audit
//	GET  /v1/audit/stream         SSE
//	GET  /v1/events/stream        SSE - approval/audit fan-out
//
//	GET  /v1/memory               list (?scope=, ?prefix=)
//	POST /v1/memory               {"scope":..., "key":..., "value":...}
//	DELETE /v1/memory             ?scope=&key=
//
//	POST /v1/push/subscribe       browser PushSubscription
//	GET  /v1/push/vapid_key
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/marketplace"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/notes"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/passkey"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/skills"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
	"github.com/tusharbhardwaj/toolyard/internal/usage"
	"github.com/tusharbhardwaj/toolyard/internal/voice"
)

const (
	sessionCookieName = "toolyard_session"
	sessionTTL        = 24 * time.Hour
	enrollCodeTTL     = 15 * time.Minute
)

type Server struct {
	identity  *identity.Service
	approval  *approval.Bus
	audit     *audit.Logger
	memory    *memory.Service
	push      *push.Service
	hub       *realtime.Hub
	gateway   *gateway.Gateway
	upstreams *upstreams.Service
	settings  *settings.Service
	usage     *usage.Service
	metrics   *metrics.Reader
	// metricsRecorder, when set, lets /v1/health surface the
	// async-flusher's dropped-event counter — a quiet warning sign that
	// the metrics buffer is overflowing (often the canary for "the
	// SQLite write path is stalled"), which usually shows up before the
	// dashboard noticeably hangs.
	metricsRecorder          *metrics.Recorder
	autoApproval             *autoapproval.Service
	policy                   *policy.Engine
	oauth                    *oauth.Service
	hooks                    *hooks.Service
	clickhouseRuntimeEnvPath string
	mempalace                *mempalace.Service
	inbox                    *inbox.Service
	snapshots                *inbox.Snapshotter
	passkeys                 *passkey.Service
	guide                    *inbox.Guide
	notes                    *notes.Service
	skills                   *skills.Service
	voice                    *voice.Service
	secrets                  *secrets.Service
	chatTelegram             ChatTelegram
	events                   EventsAPI
	memWebhooks              *memwebhook.Service
	// webhookMaxBytes caps the /v1/memory/webhooks/ingest body. Large by
	// design (n8n meeting transcripts); enforced via MaxBytesReader for a
	// clean 413. Other routes keep their tight per-route caps.
	webhookMaxBytes int64
	sessionKey      []byte
	security        SecurityOptions
	loginLimit      *loginThrottle
	unauthLimit     *loginThrottle
	// access resolves roles and per-user tool-group grants; the Users
	// admin routes write through it and drop its cache after a change.
	access *access.Service
	// clerk is nil when Sign in with Google is off. The publishable key
	// and Frontend API host are copied out so /v1/auth/config and the CSP
	// don't need the client.
	clerk               clerkDirectory
	clerkPublishableKey string
	clerkFrontendAPI    string
	ownerEmail          string
}

type Options struct {
	Identity  *identity.Service
	Approval  *approval.Bus
	Audit     *audit.Logger
	Memory    *memory.Service
	Push      *push.Service
	Hub       *realtime.Hub
	Gateway   *gateway.Gateway
	Upstreams *upstreams.Service
	Settings  *settings.Service
	Usage     *usage.Service
	Metrics   *metrics.Reader
	// MetricsRecorder, when set, exposes the async metrics writer's
	// dropped-event counter on /v1/health. Optional — nil omits the
	// field from the response.
	MetricsRecorder *metrics.Recorder
	AutoApproval    *autoapproval.Service
	Policy          *policy.Engine
	OAuth           *oauth.Service
	// Hooks, when set, enables /v1/hooks/*: bearer-authenticated lifecycle
	// hook ingest for agents plus dashboard browsing/export.
	Hooks *hooks.Service
	// ClickhouseRuntimeEnvPath, when non-empty, is the path on disk
	// where toolyard maintains a TOOLYARD_CH_PASSWORD=... line for the
	// toolyard-clickhouse docker-compose `env_file:` to consume.
	// Updated on initial password bootstrap and on every rotation; the
	// operator follows up with `docker compose restart clickhouse` so
	// the container re-reads the env on next process start.
	ClickhouseRuntimeEnvPath string
	Mempalace                *mempalace.Service
	// Notes, when set, enables /v1/notes/* — markdown workspace +
	// background MemPalace sync. The Service itself nil-checks so an
	// unwired install just returns 503 from the routes.
	Notes *notes.Service
	// Skills, when set, holds the centralised Claude Code skills workspace
	// + background MemPalace sync. No /v1/skills/* HTTP routes in v1 —
	// the field is present so the dashboard can consume snapshots later
	// without another Options refactor.
	Skills *skills.Service
	// Voice, when set, enables /v1/voice/* — the dashboard "live call"
	// panel. Requires GEMINI_API_KEY to be wired into the voice.Service
	// itself; the api layer just gates auth and reverse-checks
	// concurrency.
	Voice *voice.Service
	// Secrets, when set, enables /v1/secrets/* and the convert-env action,
	// and is what masks secret values in /v1/servers responses.
	Secrets *secrets.Service
	// ChatTelegram, when set, enables /v1/chat/telegram/*. nil leaves the
	// routes returning 503 (status still reports from settings).
	ChatTelegram ChatTelegram
	// Events, when set, enables the Events Hub: /v1/ingest, /v1/events*,
	// /v1/event-sources*. nil leaves them returning 503/empty.
	Events EventsAPI
	// MemWebhooks, when set, enables TEC-481: wing-locked memory-ingestion
	// webhooks (/v1/memory/webhooks*, /v1/memory/metrics). nil leaves them
	// returning 503/empty.
	MemWebhooks *memwebhook.Service
	// Inbox, Snapshots and Guide back the owner inbox (/v1/inbox/*) and
	// the agent protocol (/v1/guide). All optional.
	Inbox     *inbox.Service
	Snapshots *inbox.Snapshotter
	Guide     *inbox.Guide
	// Passkeys confirm high-risk approvals (/v1/passkeys/*). Optional.
	Passkeys *passkey.Service
	// WebhookMaxBytes caps the memory-webhook ingest body. 0 falls back to
	// the default (25 MiB).
	WebhookMaxBytes int64
	SessionKey      []byte
	Security        SecurityOptions
	// Access backs the Users admin API (per-user server grants) and
	// /v1/me/servers. Optional; without it members have no grants.
	Access *access.Service
	// Clerk, when set, turns on Sign in with Google through Clerk:
	// /v1/auth/clerk/session accepts Clerk session tokens and
	// /v1/auth/config advertises the publishable key. nil = off.
	Clerk *clerk.Client
	// OwnerEmail: the first Clerk sign-in with this email (case-insensitive)
	// attaches to the existing primary password user instead of creating a
	// new member, so the owner keeps their admin role, agents and passkeys.
	OwnerEmail string
}

func New(ctx context.Context, opts Options) *Server {
	s := &Server{
		identity:                 opts.Identity,
		approval:                 opts.Approval,
		audit:                    opts.Audit,
		memory:                   opts.Memory,
		push:                     opts.Push,
		hub:                      opts.Hub,
		gateway:                  opts.Gateway,
		upstreams:                opts.Upstreams,
		settings:                 opts.Settings,
		usage:                    opts.Usage,
		metrics:                  opts.Metrics,
		metricsRecorder:          opts.MetricsRecorder,
		autoApproval:             opts.AutoApproval,
		policy:                   opts.Policy,
		oauth:                    opts.OAuth,
		hooks:                    opts.Hooks,
		clickhouseRuntimeEnvPath: opts.ClickhouseRuntimeEnvPath,
		mempalace:                opts.Mempalace,
		notes:                    opts.Notes,
		skills:                   opts.Skills,
		voice:                    opts.Voice,
		secrets:                  opts.Secrets,
		chatTelegram:             opts.ChatTelegram,
		events:                   opts.Events,
		memWebhooks:              opts.MemWebhooks,
		inbox:                    opts.Inbox,
		snapshots:                opts.Snapshots,
		passkeys:                 opts.Passkeys,
		guide:                    opts.Guide,
		webhookMaxBytes:          opts.WebhookMaxBytes,
		sessionKey:               opts.SessionKey,
		security:                 opts.Security,
		loginLimit:               newLoginThrottle(5, 15*time.Minute),
		unauthLimit:              newLoginThrottle(0, time.Hour), // max/window passed per-call via AllowN
		access:                   opts.Access,
		ownerEmail:               strings.TrimSpace(opts.OwnerEmail),
	}
	if s.webhookMaxBytes <= 0 {
		s.webhookMaxBytes = DefaultWebhookMaxBytes
	}
	// Guarded assignment: a nil *clerk.Client stored in the interface would
	// be a non-nil interface, and "Clerk is on" is tested with s.clerk != nil.
	if opts.Clerk != nil {
		s.clerk = opts.Clerk
		s.clerkPublishableKey = opts.Clerk.PublishableKey()
		s.clerkFrontendAPI = opts.Clerk.FrontendAPI()
	}
	// Sweep stale throttle buckets periodically. Tied to ctx so the
	// goroutine exits on shutdown instead of leaking (precedent:
	// approval.New).
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.loginLimit.Sweep()
				s.unauthLimit.Sweep()
			}
		}
	}()
	return s
}

// SecurityOpts exposes the configured options for middleware wiring outside
// the api package (cmd/gateway uses this when stitching the mux).
func (s *Server) SecurityOpts() SecurityOptions { return s.security }

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", s.health)
	mux.HandleFunc("/v1/auth/setup", s.authSetup)
	mux.HandleFunc("/v1/auth/login", s.authLogin)
	mux.HandleFunc("/v1/auth/logout", s.authLogout)
	mux.HandleFunc("/v1/auth/me", s.authMe)
	mux.HandleFunc("/v1/auth/config", s.authConfig)
	mux.HandleFunc("/v1/auth/clerk/session", s.authClerkSession)

	mux.HandleFunc("/v1/users", s.usersList)
	mux.HandleFunc("/v1/users/", s.usersItem)
	mux.HandleFunc("/v1/me/servers", s.meServers)

	mux.HandleFunc("/v1/agents", s.agentsCollection)
	mux.HandleFunc("/v1/agents/enroll", s.agentsEnroll)
	mux.HandleFunc("/v1/agents/exchange", s.agentsExchange)
	mux.HandleFunc("/v1/agents/", s.agentsItem)

	mux.HandleFunc("/v1/approvals", s.approvalsList)
	mux.HandleFunc("/v1/approvals/", s.approvalsOne)
	mux.HandleFunc("/v1/approvals/decide-by-token", s.approvalsDecideByToken)
	mux.HandleFunc("/v1/approvals/decide-batch", s.approvalsDecideBatch)
	mux.HandleFunc("/v1/approvals/export", s.approvalsExport)

	mux.HandleFunc("/v1/audit", s.auditList)
	mux.HandleFunc("/v1/audit/export", s.auditExport)
	mux.HandleFunc("/v1/events/stream", s.eventsStream)

	mux.HandleFunc("/v1/hooks/ingest", s.hooksIngest)
	mux.HandleFunc("/v1/hooks/events", s.hooksEvents)
	mux.HandleFunc("/v1/hooks/export", s.hooksExport)

	mux.HandleFunc("/v1/memory", s.memoryHandler)
	mux.HandleFunc("/v1/memory/list", s.memoryList)
	mux.HandleFunc("/v1/memory/export", s.memoryExport)
	mux.HandleFunc("/v1/memory/import", s.memoryImport)

	mux.HandleFunc("/v1/mempalace/ingest", s.mempalaceIngest)
	mux.HandleFunc("/v1/mempalace/status", s.mempalaceStatus)
	mux.HandleFunc("/v1/mempalace/agents", s.mempalaceAgents)

	s.memoryWebhookRoutes(mux)

	mux.HandleFunc("/v1/notes/sync", s.notesSync)
	mux.HandleFunc("/v1/notes/status", s.notesStatus)
	mux.HandleFunc("/v1/notes/publish", s.notesPublish)

	mux.HandleFunc("/v1/push/vapid_key", s.pushVapidKey)
	mux.HandleFunc("/v1/push/subscribe", s.pushSubscribe)
	mux.HandleFunc("/v1/push/test", s.pushTest)
	mux.HandleFunc("/v1/push/diag", s.pushDiag)
	mux.HandleFunc("/v1/push/rotate-vapid", s.pushRotateVapid)
	mux.HandleFunc("/v1/push/jwt-preview", s.pushJWTPreview)

	mux.HandleFunc("/v1/servers", s.serversCollection)
	mux.HandleFunc("/v1/servers/", s.serversItem)
	mux.HandleFunc("/v1/secrets", s.secretsCollection)
	mux.HandleFunc("/v1/secrets/", s.secretsItem)
	mux.HandleFunc("/v1/tools", s.toolsList)
	mux.HandleFunc("/v1/tools/run", s.toolsRun)
	mux.HandleFunc("/v1/marketplace", s.marketplaceList)
	mux.HandleFunc("/v1/settings", s.settingsHandler)
	mux.HandleFunc("/v1/settings/reveal", s.settingsReveal)
	mux.HandleFunc("/v1/settings/rotate", s.settingsRotate)
	mux.HandleFunc("/v1/usage", s.usageHandler)

	mux.HandleFunc("/v1/policies", s.policiesCollection)
	mux.HandleFunc("/v1/policies/", s.policiesItem)

	mux.HandleFunc("/v1/insights/overview", s.insightsOverview)
	mux.HandleFunc("/v1/insights/tools", s.insightsTools)
	mux.HandleFunc("/v1/insights/tools/", s.insightsToolPolicy)
	mux.HandleFunc("/v1/insights/agents", s.insightsAgents)
	mux.HandleFunc("/v1/insights/agents/", s.insightsAgentDetail)
	mux.HandleFunc("/v1/insights/anomalies", s.insightsAnomalies)
	mux.HandleFunc("/v1/insights/anomalies/", s.insightsAnomalyAction)
	mux.HandleFunc("/v1/insights/cost", s.insightsCost)
	mux.HandleFunc("/v1/insights/auto/rules", s.autoRulesCollection)
	mux.HandleFunc("/v1/insights/auto/rules/", s.autoRulesItem)
	mux.HandleFunc("/v1/insights/purge-agent", s.insightsPurgeAgent)
	mux.HandleFunc("/v1/insights/export", s.insightsExport)

	mux.HandleFunc("/v1/diagnostics/crashes", s.diagnosticsCrashes)

	s.chatRoutes(mux)
	s.eventsRoutes(mux)

	s.oauthRoutes(mux)
	s.cliRoutes(mux)
	s.voiceRoutes(mux)
	s.inboxRoutes(mux)
	s.passkeyRoutes(mux)
}

// ---- helpers ----------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// /v1/health doubles as a quick "is the binary stuck?" probe.
	// goroutines + inflight_calls usually move together; if goroutines
	// climbs into the thousands while inflight stays low, leak. If both
	// climb together, upstream pile-up. metrics_dropped > 0 means the
	// async writer's buffer overflowed — typically because SQLite is
	// stalled on a long transaction. upstreams_live vs _max tells the
	// operator whether the LRU pool is at saturation.
	body := map[string]any{
		"ok":                true,
		"service":           "toolyard",
		"version":           "0.1.0",
		"goroutines":        runtime.NumGoroutine(),
		"inflight_calls":    s.gateway.InFlight(),
		"upstreams_live":    s.gateway.LiveUpstreamCount(),
		"upstreams_idle":    s.gateway.SuspendedUpstreamCount(),
		"upstreams_max":     s.gateway.MaxLiveUpstreams(),
		"upstreams_backoff": s.gateway.UpstreamsInBackoff(),
	}
	if s.metricsRecorder != nil {
		body["metrics_dropped"] = s.metricsRecorder.DroppedCount()
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// maxJSONBody caps every control-plane JSON body. The middleware also
// applies http.MaxBytesReader to the raw stream — this is a second belt for
// callers that bypass the middleware (none today, but cheap).
const maxJSONBody = 1 << 20 // 1 MiB

func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	limited := http.MaxBytesReader(nil, r.Body, maxJSONBody)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing garbage so a caller can't smuggle a second JSON value.
	if dec.More() {
		return errors.New("body has trailing JSON tokens")
	}
	return nil
}

// ---- auth & session ---------------------------------------------------------

type sessionClaims struct {
	UserID    string `json:"sub"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	// Persist a session anchor so we can revoke this JWT before its expiry.
	sid, err := s.identity.CreateSession(r.Context(), userID, r.UserAgent(), s.security.ClientIP(r), sessionTTL)
	if err != nil {
		return err
	}
	claims := sessionClaims{
		UserID:    userID,
		SessionID: sid,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL)),
			ID:        sid,
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.sessionKey)
	if err != nil {
		return err
	}
	// Secure flag only when behind real HTTPS; otherwise local dev (plain HTTP)
	// would lose the cookie on every request.
	secure := s.security.IsBehindHTTPS(r)
	sameSite := http.SameSiteLaxMode
	if secure {
		// Strict gives the strongest CSRF posture once we're confident
		// every cross-tab transition still works under HTTPS. Lax in dev.
		sameSite = http.SameSiteStrictMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Expires:  time.Now().Add(sessionTTL),
	})
	return nil
}

func (s *Server) verifySession(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	parsed, err := jwt.ParseWithClaims(cookie.Value, &sessionClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.sessionKey, nil
	})
	if err != nil || !parsed.Valid {
		return "", false
	}
	c, ok := parsed.Claims.(*sessionClaims)
	if !ok {
		return "", false
	}
	// Server-side anchor check: a JWT survives the cookie clear, but a
	// revoked sid stops authenticating immediately. SID is empty for any
	// JWT minted before this change — those are accepted only until they
	// naturally expire (24h grace).
	if c.SessionID != "" && !s.identity.IsSessionValid(r.Context(), c.SessionID) {
		return "", false
	}
	return c.UserID, true
}

// currentSessionID extracts the sid claim if the cookie is present and
// valid. Used by logout so we revoke this JWT specifically.
func (s *Server) currentSessionID(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	parsed, err := jwt.ParseWithClaims(cookie.Value, &sessionClaims{}, func(t *jwt.Token) (any, error) {
		return s.sessionKey, nil
	})
	if err != nil {
		return ""
	}
	if c, ok := parsed.Claims.(*sessionClaims); ok {
		return c.SessionID
	}
	return ""
}

// ctxUserKey carries the resolved session user from RoleGuard to the
// handler so the users row is read once per request.
type ctxKey int

const ctxUserKey ctxKey = iota + 1

var errAdminOnly = errors.New("admin_only")

// sessionUser resolves the session cookie to its user: the RoleGuard's
// per-request cache first, else cookie -> session row -> users row. A
// blocked user never resolves, whatever their cookie says.
func (s *Server) sessionUser(r *http.Request) (*identity.User, bool) {
	if u, ok := r.Context().Value(ctxUserKey).(*identity.User); ok && u != nil {
		return u, true
	}
	id, ok := s.verifySession(r)
	if !ok {
		return nil, false
	}
	u, err := s.identity.GetUserByID(r.Context(), id)
	if err != nil || u.Status != identity.StatusActive {
		return nil, false
	}
	return u, true
}

func (s *Server) requireUser(r *http.Request) (string, error) {
	u, ok := s.sessionUser(r)
	if !ok {
		return "", errors.New("unauthorized")
	}
	return u.ID, nil
}

// requireAdmin is requireUser plus the admin role; it returns the user so
// handlers can record the actor. RoleGuard already keeps members off every
// route that isn't on its allowlist, but admin-only handlers call this too
// so the rule holds without the middleware in front.
func (s *Server) requireAdmin(r *http.Request) (*identity.User, error) {
	u, ok := s.sessionUser(r)
	if !ok {
		return nil, errors.New("unauthorized")
	}
	if u.Role != identity.RoleAdmin {
		return nil, errAdminOnly
	}
	return u, nil
}

// writeAuthError maps requireUser/requireAdmin failures to 401 or 403.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAdminOnly) {
		writeError(w, http.StatusForbidden, "admin_only")
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// One-shot: once any user exists the route closes entirely. This is the
	// sole guard on the bootstrap window — it stays open to any caller
	// (including non-loopback) until the first account is created, then 404s.
	if exists, err := s.identity.HasUser(r.Context()); err == nil && exists {
		http.NotFound(w, r)
		return
	}
	var body struct{ Username, Password string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.identity.CreateUser(r.Context(), body.Username, body.Password)
	if errors.Is(err, identity.ErrUserExists) {
		writeError(w, http.StatusConflict, "user already configured")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.issueSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// Per-IP throttle. We don't let an attacker walk past argon2id at full speed.
	ip := s.security.ClientIP(r)
	if !s.loginLimit.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}
	var body struct{ Username, Password string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.identity.Authenticate(r.Context(), body.Username, body.Password)
	if errors.Is(err, identity.ErrUserBlocked) {
		// Only reachable with the right password, so this reveals nothing
		// to a guesser that the account owner doesn't already know.
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: audit.EventUserLogin, Decision: "denied", Reason: "blocked", ResultSummary: body.Username,
		})
		writeError(w, http.StatusForbidden, "blocked")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// Successful login resets the bucket so later legit requests aren't blocked.
	s.loginLimit.Reset(ip)
	if err := s.issueSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: audit.EventUserLogin, AgentID: "user:" + u.ID, Reason: "password", ResultSummary: u.Username,
	})
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.currentSessionID(r); sid != "" {
		_ = s.identity.RevokeSession(r.Context(), sid)
	}
	secure := s.security.IsBehindHTTPS(r)
	sameSite := http.SameSiteLaxMode
	if secure {
		sameSite = http.SameSiteStrictMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	id, err := s.requireUser(r)
	if err != nil {
		// Distinguish "no user yet" so the dashboard knows to show setup screen.
		ok, herr := s.identity.HasUser(r.Context())
		if herr == nil && !ok {
			writeJSON(w, http.StatusOK, map[string]any{"setup_required": true})
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.identity.GetUserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// ---- agents ----------------------------------------------------------------

// agentsCollection handles GET (list) and POST (create-direct). The POST
// path is the one-step "name -> token" used by the dashboard so the
// operator gets a copy-pasteable agent token without doing the
// enrollment-code dance.
func (s *Server) agentsCollection(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		agents, err := s.identity.ListAgents(r.Context(), uid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]map[string]any, 0, len(agents))
		for _, a := range agents {
			row := map[string]any{"id": a.ID, "name": a.Name}
			if !a.LastSeen.IsZero() {
				row["last_seen"] = a.LastSeen.UnixMilli()
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body struct{ Name string }
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		token, ag, err := s.identity.CreateAgentWithToken(r.Context(), uid, strings.TrimSpace(body.Name))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventAgentEnroll, AgentID: ag.ID, ResultSummary: ag.Name})
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id": ag.ID,
			"name":     ag.Name,
			"token":    token,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) agentsEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct{ Name string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		body.Name = "agent"
	}
	code, _, err := s.identity.CreateEnrollment(r.Context(), uid, body.Name, enrollCodeTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_code": code,
		"expires_in":      int(enrollCodeTTL.Seconds()),
	})
}

// agentsItem handles per-agent actions:
//
//	POST   /v1/agents/{id}/rotate   issue a fresh token, invalidate old hash
//	DELETE /v1/agents/{id}          remove the agent entirely
//
// Owner-scoped via identity.{Rotate,Delete}AgentToken — callers can't reach
// agents owned by other users (relevant the day toolyard goes multi-user).
func (s *Server) agentsItem(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	if id == "" || id == "enroll" || id == "exchange" {
		http.NotFound(w, r)
		return
	}
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	switch {
	case subpath == "rotate" && r.Method == http.MethodPost:
		// Optional grace_seconds: how long the old token keeps working.
		// Omitted → DefaultRotateGrace; explicit 0 → immediate kill.
		grace := identity.DefaultRotateGrace
		var body struct {
			GraceSeconds *int `json:"grace_seconds"`
		}
		_ = decode(r, &body)
		if body.GraceSeconds != nil {
			grace = time.Duration(*body.GraceSeconds) * time.Second
		}
		tok, err := s.identity.RotateAgentToken(r.Context(), uid, id, grace)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "agent.rotate", AgentID: id, ResultSummary: "user:" + uid,
		})
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "token": tok, "grace_seconds": int(grace.Seconds())})
	case (subpath == "disable" || subpath == "enable") && r.Method == http.MethodPost:
		disable := subpath == "disable"
		if err := s.identity.SetAgentDisabled(r.Context(), uid, id, disable); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "agent." + subpath, AgentID: id, ResultSummary: "user:" + uid,
		})
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "disabled": disable})
	case subpath == "" && r.Method == http.MethodDelete:
		if err := s.identity.DeleteAgent(r.Context(), uid, id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "agent.delete", AgentID: id, ResultSummary: "user:" + uid,
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST /rotate, /disable, /enable, or DELETE")
	}
}

func (s *Server) agentsExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct{ Code string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tok, ag, err := s.identity.ExchangeEnrollment(r.Context(), strings.TrimSpace(body.Code))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventAgentEnroll, AgentID: ag.ID, ResultSummary: ag.Name})
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": ag.ID,
		"token":    tok,
	})
}

// ---- approvals -------------------------------------------------------------

func (s *Server) approvalsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.URL.Query().Get("status") {
	case "pending":
		out, err := s.approval.ListPending(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := s.approval.Recent(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) approvalsOne(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/approvals/")
	if id == "" || id == "decide-by-token" || id == "decide-batch" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/decide") {
		s.approvalsDecide(w, r, strings.TrimSuffix(id, "/decide"))
		return
	}
	// Authenticated GET only — the previous ?token= URL fallback was
	// dropped because query strings end up in proxy / browser-history
	// logs, leaking both the approval ID and a key that decides it. The
	// push notification flow uses POST /v1/approvals/decide-by-token
	// which keeps the token in the body.
	if r.Method == http.MethodGet {
		if _, err := s.requireUser(r); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		req, err := s.approval.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, req)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "GET or POST /decide")
}

func (s *Server) approvalsDecide(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct{ Action string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.approval.Decide(r.Context(), id, body.Action, uid)
	if err != nil {
		switch {
		case errors.Is(err, approval.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		case errors.Is(err, approval.ErrNotPending):
			// Already decided/expired/cancelled — 409 with the current row
			// so the dashboard shows the real state instead of a false
			// success toast.
			writeJSON(w, http.StatusConflict, req)
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// approvalsDecideBatch resolves several pending approvals in one shot. The
// dashboard's "Allow all / Deny all" buttons use it. Returns a per-id status
// array so the UI can show which ones flipped vs which were already
// resolved/expired.
func (s *Server) approvalsDecideBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	if body.Action != approval.StatusAllowed && body.Action != approval.StatusDenied {
		writeError(w, http.StatusBadRequest, "action must be allowed or denied")
		return
	}
	type result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(body.IDs))
	for _, id := range body.IDs {
		req, err := s.approval.Decide(r.Context(), id, body.Action, uid)
		switch {
		case err == nil:
			out = append(out, result{ID: id, Status: req.Status})
		case errors.Is(err, approval.ErrNotPending) && req != nil:
			out = append(out, result{ID: id, Status: req.Status, Error: "not pending"})
		default:
			msg := "error"
			if err != nil {
				msg = err.Error()
			}
			out = append(out, result{ID: id, Error: msg})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) approvalsDecideByToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct{ Token, Action string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.approval.DecideByToken(r.Context(), body.Token, body.Action)
	if err != nil {
		switch {
		case errors.Is(err, approval.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		case errors.Is(err, approval.ErrNotPending):
			writeJSON(w, http.StatusConflict, req)
		default:
			// Bad/forged token, bad action, etc.
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ---- audit -----------------------------------------------------------------

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	f := auditFilterFromQuery(r)
	f.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if f.Limit <= 0 {
		f.Limit = 100
	}
	out, err := s.audit.Query(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// eventsStream pushes both approval lifecycle events and audit rows. The
// dashboard subscribes once and demuxes by event type.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.hub.ServeSSE(w, r)
}

// ---- memory ----------------------------------------------------------------

func (s *Server) memoryHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		if k := q.Get("key"); k != "" {
			e, err := s.memory.Get(r.Context(), q.Get("scope"), k)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, e)
			return
		}
		entries, err := s.memory.List(r.Context(), q.Get("scope"), q.Get("prefix"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entries)
	case http.MethodPost:
		var body struct{ Scope, Key, Value string }
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		e, err := s.memory.Set(r.Context(), body.Scope, body.Key, body.Value)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, e)
	case http.MethodDelete:
		if err := s.memory.Delete(r.Context(), q.Get("scope"), q.Get("key")); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, POST, DELETE")
	}
}

func (s *Server) memoryList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	entries, err := s.memory.List(r.Context(), q.Get("scope"), q.Get("prefix"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---- push ------------------------------------------------------------------

func (s *Server) pushVapidKey(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.push.PublicKey()})
}

// pushTest sends a synthetic notification to every push_subscription
// row owned by the current user and returns per-row delivery results so
// the operator can see why notifications might be silently dropped
// (e.g. Apple rejecting `mailto:admin@example.invalid`, an expired
// subscription returning 410 Gone, a VAPID encryption failure, etc.).
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.push == nil {
		writeError(w, http.StatusServiceUnavailable, "push service not wired")
		return
	}
	payload := map[string]any{
		"title": "toolyard test notification",
		"body":  "If you can read this, push works on this device. Tap to dismiss.",
		"url":   "/",
		"tag":   "toolyard-test",
	}
	results, err := s.push.NotifyDetailed(r.Context(), uid, payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delivered, pruned, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Error != "" || r.Status == 0 || r.Status >= 500:
			failed++
		case r.Pruned:
			pruned++
		case r.Status/100 == 2:
			delivered++
		default:
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":   s.push.Subject(),
		"results":   results,
		"delivered": delivered,
		"pruned":    pruned,
		"failed":    failed,
	})
}

// pushDiag returns the configured VAPID subject and the list of stored
// subscriptions for the current user (endpoints + creation time, no
// keys). Lets the operator confirm the dashboard's subscribe call
// actually persisted, which device-IDs are registered, and whether the
// default `.invalid` subject is in use.
func (s *Server) pushDiag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.push == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	subs, err := s.push.ListForUser(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		// Hostname-only — full endpoint contains the device's push channel
		// secret on some platforms; trim to "fcm.googleapis.com" / "*.push.apple.com".
		host := sub.Endpoint
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		out = append(out, map[string]any{
			"id":         sub.ID,
			"host":       host,
			"user_agent": sub.UserAgent,
			"created_at": sub.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":         true,
		"subject":         s.push.Subject(),
		"subject_warning": pushSubjectWarning(s.push.Subject()),
		"subscriptions":   out,
		"public_key_set":  s.push.PublicKey() != "",
	})
}

// pushRotateVapid is the nuclear option for "BadJwtToken" — regenerate
// the VAPID keypair from scratch using the well-tested generator in
// SherClockHolmes/webpush-go (so format / scalar interpretation can't be
// the bug), wipe every existing subscription (they're bound to the old
// public key), and return the new public key. The operator then tells
// every device to Enable push again.
func (s *Server) pushRotateVapid(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.push == nil {
		writeError(w, http.StatusServiceUnavailable, "push service not wired")
		return
	}
	pub, err := s.push.RotateKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "push.rotate_vapid",
		AgentID:       "user:" + uid,
		ResultSummary: "vapid keypair regenerated",
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"new_public_key":      pub,
		"subscriptions_wiped": true,
	})
}

// pushJWTPreview returns the per-subscription JWT diagnostic so the
// operator can see exactly what claims toolyard is signing into the
// VAPID Authorization header.
func (s *Server) pushJWTPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.push == nil {
		writeError(w, http.StatusServiceUnavailable, "push service not wired")
		return
	}
	out, err := s.push.JWTPreview(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// pushSubjectWarning highlights the common operator misconfiguration
// where the VAPID subject is left at the docs default (or a non-mailto
// scheme): Apple silently drops messages addressed from an unroutable
// mailbox and several push services follow suit.
func pushSubjectWarning(subj string) string {
	if subj == "" {
		return "no subject configured"
	}
	if !strings.HasPrefix(subj, "mailto:") && !strings.HasPrefix(subj, "https://") {
		return "subject must be 'mailto:…' or 'https://…'"
	}
	if strings.Contains(subj, "@example.invalid") || strings.Contains(subj, "@example.com") {
		return "subject is the default placeholder; Apple/APNS may drop. Set -push-subject mailto:you@yourdomain.com"
	}
	return ""
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.push == nil {
		writeError(w, http.StatusServiceUnavailable, "push service not wired")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sub, err := s.push.Subscribe(r.Context(), uid, body.Endpoint, body.Keys.P256dh, body.Keys.Auth, r.UserAgent())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sub)
	case http.MethodDelete:
		// Wipe every subscription this user owns. Recovery path when
		// existing subscriptions are bound to a stale VAPID key (or
		// rejected by Apple/Google for any reason) — operator hits
		// "Wipe & re-enroll" in the dashboard, the browser's old sub is
		// unsubscribed client-side, and a fresh row is created on the
		// follow-up Enable-push tap.
		n, err := s.push.DeleteAllForUser(r.Context(), uid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "push.wipe",
			AgentID:       "user:" + uid,
			ResultSummary: fmt.Sprintf("removed %d subscription(s)", n),
		})
		writeJSON(w, http.StatusOK, map[string]any{"removed": n})
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST or DELETE")
	}
}

// ---- servers (upstream MCP) ------------------------------------------------

func (s *Server) serversCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.upstreams.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if out == nil {
			out = []upstreams.Server{}
		}
		// annotate live tool counts so the dashboard does not need a second
		// call, and mask secret/plaintext env+header values so a raw API key
		// never flows back through /v1/servers (the pre-broker leak).
		for i := range out {
			if s.gateway != nil {
				if c := s.gateway.UpstreamToolCount(out[i].Name); c > 0 {
					out[i].ToolCount = c
				}
			}
			out[i] = upstreams.Masked(out[i])
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body upstreams.Server
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		srv, err := s.upstreams.Add(r.Context(), body)
		if err != nil {
			// If the row was created but the connect failed, return 207 with the
			// row body so the dashboard shows the persisted error. Otherwise 400.
			if srv != nil && (errors.Is(err, gateway.ErrUpstreamNotFound) || !errors.Is(err, upstreams.ErrInvalid)) && !errors.Is(err, upstreams.ErrAlreadyHere) && !errors.Is(err, upstreams.ErrReserved) {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"server":  srv,
					"warning": err.Error(),
				})
				return
			}
			status := http.StatusBadRequest
			if errors.Is(err, upstreams.ErrAlreadyHere) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srv)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) serversItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/servers/")
	if tail == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(tail, "/", 2)
	name := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	if strings.HasPrefix(subpath, "oauth") {
		if s.dispatchOAuth(w, r, name, subpath) {
			return
		}
	}
	if subpath == "convert-env" {
		s.serversConvertEnv(w, r, name)
		return
	}
	if subpath == "reconnect" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		srv, err := s.upstreams.Reconnect(r.Context(), name)
		if err != nil {
			if srv != nil {
				writeJSON(w, http.StatusAccepted, map[string]any{"server": srv, "warning": err.Error()})
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srv)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.upstreams.Remove(r.Context(), name); err != nil {
			switch {
			case errors.Is(err, upstreams.ErrNotFound):
				writeError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, upstreams.ErrReserved):
				writeError(w, http.StatusForbidden, "reserved built-in upstream cannot be removed")
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "DELETE")
}

// marketplaceList returns the curated MCP recipes. We require a logged-in
// user so we don't accidentally expose the catalog (small, but still
// confirms the operator's identity before suggesting installs that may want
// secrets).
func (s *Server) marketplaceList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// oauth_redirect_uri rides along so BYO-OAuth install forms can show
	// the exact callback URL to register with the provider (server-computed
	// — respects PublicURL, unlike the browser's location.origin).
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":            marketplace.Catalog(),
		"oauth_redirect_uri": s.oauthRedirectURI(r),
	})
}

func (s *Server) toolsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.gateway == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.gateway.Catalog())
}

// toolsRun lets the dashboard's "workbench" UI try any registered tool.
// Body: {"tool":"<name>", "arguments":{...}}. Calls go through the same
// policy + approval path as a regular MCP call; if a write needs approval
// the response is the deferred CallToolResult payload, and the dashboard's
// Approvals tab will show the pending request.
func (s *Server) toolsRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway not wired")
		return
	}
	var body struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Tool) == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	// Tag the call as coming from the dashboard so audit/approval rows show
	// the actual operator instead of an empty agent_id.
	ctx := gateway.WithAgentID(r.Context(), "dashboard:"+uid)
	res, err := s.gateway.RouteCall(ctx, "dashboard", body.Tool, body.Arguments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Marshal the CallToolResult: we mirror MCP's shape so the UI can
	// handle errors / deferred responses uniformly with what an agent sees.
	out := map[string]any{
		"is_error":           res.IsError,
		"content":            res.Content,
		"structured_content": res.StructuredContent,
	}
	if res.Meta != nil {
		out["_meta"] = res.Meta.AdditionalFields
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- settings -------------------------------------------------------------

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.settings == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.settings.All(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPatch, http.MethodPost:
		var body map[string]any
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Patch(r.Context(), body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Forensic trail: who changed what. Keys recorded; values omitted
		// so we don't end up logging an API-rate fragment as an event row.
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "settings.patch",
			AgentID:       "user:" + uid,
			ResultSummary: strings.Join(keys, ","),
		})
		// If the change affects what tools agents can see, push a
		// notifications/tools/list_changed so connected MCP clients
		// re-fetch instead of relying on their cached tool list.
		if s.gateway != nil {
			for _, k := range []string{"router_only_mode", "surface_mode", "top_n_count", "top_n_personalize_after"} {
				if _, touched := body[k]; touched {
					s.gateway.NotifyToolListChanged()
					break
				}
			}
		}
		out, _ := s.settings.All(r.Context())
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, PATCH")
	}
}

// settingsReveal returns the cleartext value of a single secret-classified
// setting key. The generic GET /v1/settings response masks these values
// behind a `<key>_present` boolean so the dashboard can render status
// without ever pulling cleartext into a normal page response. Reveal is
// the explicit one-shot path for the rotation/copy flow, and every call
// is audit-logged.
//
// POST /v1/settings/reveal {"key":"clickhouse_password"} -> {"value":"..."}
func (s *Server) settingsReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not wired")
		return
	}
	var body struct{ Key string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !settings.IsSecretKey(body.Key) {
		writeError(w, http.StatusBadRequest, "key is not a secret; use GET /v1/settings")
		return
	}
	val, err := s.settings.Reveal(r.Context(), body.Key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "settings.reveal",
		AgentID:       "user:" + uid,
		ResultSummary: body.Key,
	})
	writeJSON(w, http.StatusOK, map[string]string{"key": body.Key, "value": val})
}

// settingsRotate generates a fresh value for a known rotatable secret
// key and returns it once for the operator to copy. Side effect: any
// downstream files (the ClickHouse runtime env that the docker-compose
// stack reads) get re-rendered so the new value reaches its consumers
// without manual file edits.
//
// POST /v1/settings/rotate {"key":"clickhouse_password"} -> {"value":"..."}
func (s *Server) settingsRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not wired")
		return
	}
	var body struct{ Key string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch body.Key {
	case settings.ClickhousePassword:
		pw, err := s.settings.RotateClickhousePassword(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		envWriteWarning := ""
		if err := WriteClickhouseRuntimeEnv(s.clickhouseRuntimeEnvPath, pw); err != nil {
			envWriteWarning = "password rotated but clickhouse runtime env file write failed: " + err.Error()
		}
		summary := body.Key
		if envWriteWarning != "" {
			summary += " (env write warned)"
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "settings.rotate", AgentID: "user:" + uid,
			ResultSummary: summary,
		})
		out := map[string]any{
			"key":   body.Key,
			"value": pw,
			// CH only re-reads the env at process start, so a rotation
			// is incomplete until the container restarts. Tell the UI.
			"requires": "docker compose -f deploy/clickhouse/docker-compose.yaml restart clickhouse",
		}
		if envWriteWarning != "" {
			out["warning"] = envWriteWarning
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusBadRequest, "key is not rotatable")
	}
}

// ---- usage ----------------------------------------------------------------

func (s *Server) usageHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"per_tool": map[string]int64{}, "rows": []any{}})
		return
	}
	agentID := r.URL.Query().Get("agent_id")
	rows, err := s.usage.All(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []usage.Row{}
	}
	perTool, err := s.usage.AggregatePerTool(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"per_tool": perTool,
		"rows":     rows,
	})
}

// ---- mempalace -------------------------------------------------------------

// requireAgent extracts the agent identity from an Authorization: Bearer
// header. The same bearer-token flow used by /mcp. Returns the agent ID and
// true on success; on failure writes a 401 and returns false so the handler
// can return immediately.
func (s *Server) requireAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return "", false
	}
	raw := strings.TrimPrefix(authz, "Bearer ")
	ag, err := s.identity.VerifyAgentToken(r.Context(), raw)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid agent token")
		return "", false
	}
	return ag.ID, true
}

// Tool groups the bearer REST routes stand in for. Derived from the same
// registrations the gateway enforces on (mempalace.* upstream tools, the
// built-in notes.publish), so a grant means the same thing over REST as it
// does over MCP.
var (
	mempalaceGroup = access.GroupOf(mempalace.ToolPrefix, mempalace.DiaryWriteTool)
	notesGroup     = access.GroupOf("builtin", "notes.publish")
)

// agentMayUse reports whether the agent's owner may use group. Without an
// access resolver (single-user installs, tests) every agent may. The scope
// is the owner's: an admin's agents reach everything, a member's agents
// only what an admin granted that member.
func (s *Server) agentMayUse(ctx context.Context, agentID, group string) bool {
	if s.access == nil {
		return true
	}
	return s.access.ScopeFor(ctx, agentID).Allows(group)
}

// writeNotGranted is the bearer-route twin of the gateway's per-caller
// denial: the agent is real, its user just hasn't been granted the group.
func writeNotGranted(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "server not granted")
}

// mempalaceIngest accepts a chat-memory entry from an authenticated agent
// and forwards it to mempalace.diary_write via the gateway's internal call
// path (no human approval — chat capture must not block).
func (s *Server) mempalaceIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.mempalace == nil || s.mempalace.Disabled() {
		writeError(w, http.StatusServiceUnavailable, "mempalace integration is disabled")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	if !s.agentMayUse(r.Context(), agentID, mempalaceGroup) {
		writeNotGranted(w)
		return
	}
	var body struct {
		Entry string `json:"entry"`
		Topic string `json:"topic,omitempty"`
		Wing  string `json:"wing,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.mempalace.Ingest(r.Context(), agentID, body.Entry, body.Topic, body.Wing)
	if err != nil {
		switch {
		case errors.Is(err, mempalace.ErrEntryRequired):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, mempalace.ErrDisabled):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, mempalace.ErrNotReady):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	out := map[string]any{
		"ok":       res.OK,
		"agent_id": agentID,
		"detail":   res.Detail,
	}
	if res.Raw != nil {
		out["raw"] = res.Raw
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, out)
}

// mempalaceStatus returns the integration snapshot for the dashboard card.
// Cookie-authenticated (dashboard user), not the agent path.
func (s *Server) mempalaceStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.mempalace == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.mempalace.Snapshot(r.Context()))
}

// mempalaceAgents returns the top-N agents by ingest count.
func (s *Server) mempalaceAgents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.mempalace == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.mempalace.TopAgents(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []mempalace.AgentRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---- notes ------------------------------------------------------------------

// notesSync triggers a one-shot scan of $NOTES_DIR, ingesting any
// markdown file whose mtime advanced since the last sync. Bearer-authed
// (any enrolled agent can fire it; this is what cron / a `toolyard
// notes sync` CLI hook would hit). Returns count of ingested files.
func (s *Server) notesSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.notes == nil || !s.notes.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "notes service is disabled")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	if !s.agentMayUse(r.Context(), agentID, notesGroup) {
		writeNotGranted(w)
		return
	}
	n, err := s.notes.Sync(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ingested": n})
}

// notesStatus is the dashboard snapshot. Cookie-authenticated.
func (s *Server) notesStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.notes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.notes.Snapshot(r.Context()))
}

// notesPublish is the HTTP twin of the notes.publish MCP tool. Bearer-
// authed; takes the same shape as the tool and forwards through the
// service. Lets non-MCP callers (a phone shortcut, a cron, a third-party
// agent) drop a note + index it in one call.
func (s *Server) notesPublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.notes == nil || !s.notes.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "notes service is disabled")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	if !s.agentMayUse(r.Context(), agentID, notesGroup) {
		writeNotGranted(w)
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Topic   string `json:"topic,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.notes.Publish(r.Context(), agentID, body.Path, body.Content, body.Topic)
	if err != nil {
		switch {
		case errors.Is(err, notes.ErrPathRequired),
			errors.Is(err, notes.ErrEmptyContent),
			errors.Is(err, notes.ErrPathEscape):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, notes.ErrDisabled):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- placeholder for context.Background usage ------------------------------

var _ = context.Background

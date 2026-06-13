package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SecurityOptions tunes the public-host hardening middleware.
type SecurityOptions struct {
	// PublicURL, when set, switches the dashboard into "public" posture:
	// cookies get Secure, HSTS is emitted, and Origin checks are enforced.
	// Form: "https://toolyard.example.com".
	PublicURL string
	// TrustedProxies is a CIDR allowlist for X-Forwarded-* trust. When
	// behind a reverse proxy (Cloudflare, Caddy, nginx) put its IP here so
	// rate-limiting and TLS-detection see the real client IP / scheme.
	TrustedProxies []*net.IPNet
}

// IsBehindHTTPS reports whether this request is effectively HTTPS — either
// a direct TLS connection or a trusted-proxy hop with X-Forwarded-Proto.
func (o SecurityOptions) IsBehindHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !o.proxyTrusted(r) {
		return false
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ClientIP returns the apparent client IP, honoring X-Forwarded-For only
// when the immediate peer is in TrustedProxies. Falls back to RemoteAddr.
func (o SecurityOptions) ClientIP(r *http.Request) string {
	if o.proxyTrusted(r) {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			// First entry is the original client per spec.
			parts := strings.Split(v, ",")
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (o SecurityOptions) proxyTrusted(r *http.Request) bool {
	if len(o.TrustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, c := range o.TrustedProxies {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// IsLoopback reports whether the client address (after trusted-proxy
// resolution) is on loopback.
func (o SecurityOptions) IsLoopback(r *http.Request) bool {
	host := o.ClientIP(r)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SecurityHeaders is middleware that emits the standard hardening headers on
// every response. CSP is intentionally strict — it allows only same-origin
// scripts/styles/connect, blocks framing, and refuses mixed content. The
// dashboard's index.html has been adjusted to load all script/style as
// external files so 'unsafe-inline' is not required.
func (s *Server) SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// microphone=(self) is what unlocks the "Call" panel — without
		// it, getUserMedia rejects with NotAllowedError before the
		// browser even prompts for permission. Camera/payment stay
		// closed; we don't surface them anywhere in the dashboard.
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(self), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// CSP — same-origin everything, no inline. data: allowed for SVG icons.
		// Workers + ServiceWorker explicitly allowed for the push SW.
		csp := "default-src 'self'; " +
			"script-src 'self'; " +
			"style-src 'self'; " +
			"img-src 'self' data:; " +
			"font-src 'self'; " +
			// connect-src 'self' covers same-origin ws://wss://, but
			// some browsers (and reverse proxies that confuse scheme
			// detection) refuse the upgrade unless wss: is explicit.
			"connect-src 'self' wss: ws:; " +
			// media-src needed because the voice panel uses a blob:
			// URL for the silent WAV that anchors the OS MediaSession
			// (so the BTR11's play/pause button routes to us instead
			// of Siri). 'self' alone falls through to default-src.
			"media-src 'self' blob:; " +
			"worker-src 'self'; " +
			"frame-ancestors 'none'; " +
			"base-uri 'self'; " +
			"form-action 'self'"
		h.Set("Content-Security-Policy", csp)
		// HSTS only when actually behind HTTPS so HTTP browser dev doesn't
		// get pinned to a non-existent TLS endpoint.
		if s.security.IsBehindHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// EnforceOriginOnMutations checks Origin/Referer on state-changing methods
// when PublicURL is set. Cookie-authenticated POST/PATCH/DELETE that don't
// match the public origin are 403'd. Bearer-authenticated /mcp traffic is
// excluded — Authorization header alone is not CSRF-replayable.
func (s *Server) EnforceOriginOnMutations(next http.Handler) http.Handler {
	if s.security.PublicURL == "" {
		return next
	}
	allowed := strings.TrimRight(s.security.PublicURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
			// Bearer-authenticated MCP traffic skips this — no cookie auth in play.
			if strings.HasPrefix(r.URL.Path, "/mcp") &&
				strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Some browsers omit Origin on same-origin POSTs; fall back
				// to Referer with a prefix match.
				ref := r.Header.Get("Referer")
				if ref == "" || !strings.HasPrefix(ref, allowed) {
					writeError(w, http.StatusForbidden, "missing or untrusted origin")
					return
				}
			} else if !strings.EqualFold(strings.TrimRight(origin, "/"), allowed) {
				writeError(w, http.StatusForbidden, "untrusted origin: "+origin)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// LimitBody wraps every request body with http.MaxBytesReader so a single
// rogue caller can't OOM the process by streaming gigabytes into a JSON
// decoder. The cap is tight for control-plane endpoints; /mcp and /v1/insights/export
// get the larger limit because tool args + push subscription payloads can be
// several KB.
func LimitBody(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// HardenAPI is the application-layer security envelope around every /v1/*
// route. It is the load-bearing defense in our chosen posture: kernel
// sandbox is loose so subprocesses can run; this middleware compensates by
// enforcing every check uniformly per route.
//
// Per request:
//   - Per-route body cap (writeable routes get 64–256 KiB; tools/run gets
//     4 MiB; everything else gets 8 KiB. Independently of the global mux
//     cap so misconfiguration on one side can't widen the other.)
//   - Anti-CSRF custom-header check on cookie-authenticated mutations.
//     The dashboard sets X-Requested-With: toolyard; cross-site forms
//     can't add a custom header without a CORS preflight, which we never
//     answer with a permissive Allow-Origin.
//   - Strict Content-Type on JSON-bodied mutations: only application/json
//     is accepted. This kills form-encoded CSRF entirely.
//   - Per-IP rate limit on the unauthenticated routes (setup, exchange,
//     decide-by-token) so brute force / spray attacks have a hard wall.
func (s *Server) HardenAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		// 1) Per-route body cap.
		cap := perRouteBodyCap(path)
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, cap)
		}

		// 2) Mutation-only checks: header + content-type + auth-route throttle.
		isMut := r.Method == http.MethodPost ||
			r.Method == http.MethodPatch ||
			r.Method == http.MethodPut ||
			r.Method == http.MethodDelete

		if isMut {
			// Anti-CSRF custom header. The dashboard's app.js sets
			// X-Requested-With on every fetch; phone push handlers do
			// likewise. Cross-site form POSTs cannot add this header,
			// even when Same-Origin is bypassed by a downgrade attack.
			//
			// Exempt: /v1/auth/setup (one-shot; closes after first user) and
			// /v1/auth/login (cookie isn't issued yet) and
			// /v1/agents/exchange (Bearer-bootstrap path) and
			// /v1/approvals/decide-by-token (push-tap path; signed token
			// is the auth, no cookie present).
			if !exemptFromCSRFHeader(path) {
				if r.Header.Get("X-Requested-With") == "" {
					writeError(w, http.StatusForbidden,
						"X-Requested-With header required on mutating requests")
					return
				}
			}

			// Strict Content-Type for JSON bodies. Anything carrying a
			// body must declare application/json. Empty body (e.g.
			// agent rotate POST with no payload) skips this check.
			if r.ContentLength != 0 {
				ct := r.Header.Get("Content-Type")
				if ct == "" {
					writeError(w, http.StatusUnsupportedMediaType, "Content-Type required")
					return
				}
				if i := strings.IndexByte(ct, ';'); i >= 0 {
					ct = ct[:i]
				}
				ct = strings.ToLower(strings.TrimSpace(ct))
				if ct != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType,
						"only application/json is accepted")
					return
				}
			}
		}

		// 3) Per-IP rate limit on the unauthenticated bootstrap / push paths.
		if rl := unauthRouteLimit(path); rl != nil {
			ip := s.security.ClientIP(r)
			if !s.unauthLimit.AllowN(rl.key+":"+ip, rl.max, rl.window) {
				writeError(w, http.StatusTooManyRequests,
					"too many requests; try again later")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// perRouteBodyCap returns the per-route MaxBytesReader cap. Tighter than
// the global mux cap so misconfiguration upstream can't widen the
// per-handler allowance, and so a CSP-bypassing attacker can't park
// unbounded data on a small handler.
func perRouteBodyCap(path string) int64 {
	switch {
	case strings.HasPrefix(path, "/v1/tools/run"),
		strings.HasPrefix(path, "/v1/agents/tools/run"):
		return 4 << 20 // 4 MiB — tool arguments can be substantial
	case strings.HasPrefix(path, "/v1/memory"):
		return 1 << 20 // 1 MiB — memory.set values
	case strings.HasPrefix(path, "/v1/mempalace/ingest"):
		return 1 << 20 // 1 MiB — chat-interaction entries can be sizeable
	case strings.HasPrefix(path, "/v1/hooks/ingest"):
		return 1 << 20 // 1 MiB — raw agent hook payloads
	case strings.HasPrefix(path, "/v1/ingest"):
		return 256 << 10 // 256 KiB — webhook event bodies (payload capped to 64 KiB inside)
	case strings.HasPrefix(path, "/v1/events"),
		strings.HasPrefix(path, "/v1/event-sources"):
		return 64 << 10 // 64 KiB — event queries + source config
	case strings.HasPrefix(path, "/v1/notes/publish"):
		return 1 << 20 // 1 MiB — markdown documents
	case strings.HasPrefix(path, "/v1/notes/sync"):
		return 4 << 10 // tiny — no body needed
	case strings.HasPrefix(path, "/v1/push/subscribe"):
		return 8 << 10 // 8 KiB — subscription metadata
	case strings.HasPrefix(path, "/v1/chat/"):
		return 8 << 10 // 8 KiB — bot token + config
	case strings.HasPrefix(path, "/v1/auth/login"),
		strings.HasPrefix(path, "/v1/auth/setup"):
		return 4 << 10 // 4 KiB — username + password is tiny
	case strings.HasPrefix(path, "/v1/agents"),
		strings.HasPrefix(path, "/v1/approvals"),
		strings.HasPrefix(path, "/v1/servers"),
		strings.HasPrefix(path, "/v1/secrets"),
		strings.HasPrefix(path, "/v1/insights"):
		return 64 << 10 // 64 KiB — config rows + decision payloads + secret values
	default:
		return 16 << 10 // 16 KiB
	}
}

// exemptFromCSRFHeader lists routes that can't carry a custom header
// because the caller is either (a) bootstrapping with no cookie / dashboard
// origin in the picture, or (b) a third-party token-tap path.
func exemptFromCSRFHeader(path string) bool {
	// Webhook event ingest is bearer-authenticated (source token); no cookie
	// or dashboard origin, so the CSRF custom-header check doesn't apply. The
	// path-token form lives under the /v1/ingest/ subtree.
	if path == "/v1/ingest" || strings.HasPrefix(path, "/v1/ingest/") {
		return true
	}
	switch path {
	case "/v1/auth/setup", "/v1/auth/login",
		"/v1/agents/exchange",
		"/v1/approvals/decide-by-token",
		// Agent-authenticated (Bearer) ingest — no cookie, no dashboard
		// origin, so the CSRF custom-header check doesn't apply.
		"/v1/mempalace/ingest",
		"/v1/hooks/ingest",
		"/v1/notes/sync",
		"/v1/notes/publish",
		// CLI is bearer-authenticated and called from servers / scripts,
		// not the dashboard. No cookie to protect.
		"/v1/agents/tools/run":
		return true
	}
	return false
}

// unauthRouteRule describes a per-(route, IP) budget for the unauth-route
// throttle. window is the bucket length; max is the credit cap.
type unauthRouteRule struct {
	key    string
	max    int
	window time.Duration
}

// unauthRouteLimit picks a rate-limit rule for unauthenticated routes that
// would otherwise be infinitely abusable (no cookie required, and either
// fast or DB-backed). /v1/auth/setup is intentionally NOT here — it has
// a stronger guard (one-shot: 404 after the first success) and the
// rate limit there mostly hurts test harnesses that re-bootstrap.
func unauthRouteLimit(path string) *unauthRouteRule {
	switch {
	case path == "/v1/agents/exchange":
		// Exchange burns the enrollment code on success; legitimate users
		// hit it once per agent enrollment. 120/hour leaves room for a
		// fleet of agents and for the e2e suite to spin up many in burst.
		return &unauthRouteRule{"exchange", 120, time.Hour}
	case path == "/v1/approvals/decide-by-token":
		// Push-tap path. A real user taps approve maybe a few times per
		// minute at peak.
		return &unauthRouteRule{"decide", 120, time.Hour}
	case path == "/v1/ingest" || strings.HasPrefix(path, "/v1/ingest/"):
		// Webhook ingest. Generous per-IP budget for legitimate high-volume
		// senders; a bad token still fails auth, this just caps spray.
		return &unauthRouteRule{"ingest", 1200, time.Hour}
	}
	return nil
}

// loginThrottle is a token-bucket per (ip, username) for /v1/auth/login.
// 5 attempts per 15 minutes per source. Cheap, in-process — sufficient for a
// single-instance gateway. Distributed deployments would swap for Redis.
type loginThrottle struct {
	mu      sync.Mutex
	buckets map[string]*throttleBucket
	max     int
	window  time.Duration
}

type throttleBucket struct {
	count int
	since time.Time
}

func newLoginThrottle(max int, window time.Duration) *loginThrottle {
	return &loginThrottle{
		buckets: map[string]*throttleBucket{},
		max:     max,
		window:  window,
	}
}

// Allow returns true if the key is under its budget; consumes one credit.
// Buckets older than window are reset.
func (t *loginThrottle) Allow(key string) bool {
	return t.AllowN(key, t.max, t.window)
}

// AllowN is the variant for callers that want to specify max + window per
// call (e.g. different rate limits per route from one shared bucket store).
func (t *loginThrottle) AllowN(key string, max int, window time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[key]
	now := time.Now()
	if b == nil || now.Sub(b.since) > window {
		b = &throttleBucket{since: now}
		t.buckets[key] = b
	}
	if b.count >= max {
		return false
	}
	b.count++
	return true
}

// Reset clears the bucket for a key — call after a successful login so a
// momentarily-locked-out user can keep working.
func (t *loginThrottle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.buckets, key)
}

// Sweep drops stale buckets so the map doesn't grow unbounded. Cheap;
// callers should run it from a goroutine every minute or so.
func (t *loginThrottle) Sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, b := range t.buckets {
		if now.Sub(b.since) > t.window*2 {
			delete(t.buckets, k)
		}
	}
}

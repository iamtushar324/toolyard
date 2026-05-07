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
	// cookies get Secure, HSTS is emitted, the setup route 404s for
	// non-loopback callers, and Origin checks are enforced. Form:
	// "https://toolyard.example.com".
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
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// CSP — same-origin everything, no inline. data: allowed for SVG icons.
		// Workers + ServiceWorker explicitly allowed for the push SW.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self'; "+
				"img-src 'self' data:; "+
				"font-src 'self'; "+
				"connect-src 'self'; "+
				"worker-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
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
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[key]
	now := time.Now()
	if b == nil || now.Sub(b.since) > t.window {
		b = &throttleBucket{since: now}
		t.buckets[key] = b
	}
	if b.count >= t.max {
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

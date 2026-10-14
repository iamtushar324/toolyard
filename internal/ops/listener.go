package ops

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// minTokenLen is the shortest ops token accepted from -ops-token-file.
const minTokenLen = 16

// ListenerConfig configures the private ops listener.
type ListenerConfig struct {
	// Addrs is the -ops-addr value: one or more comma-separated
	// host:port addresses. Empty turns the listener off.
	Addrs string
	// TokenFile is the -ops-token-file path. When set, every request must
	// carry the token, loopback peers included. Empty means no token: then
	// every address must be loopback or private.
	TokenFile string
	// Handler serves every listener (see Mux).
	Handler http.Handler
	// Logf logs the listening lines and serve errors; nil uses log.Printf.
	Logf func(format string, args ...any)
}

// Mux returns the ops listener's routes: /metrics (metrics) and the
// probes. Every other path is a JSON 404.
func Mux(metrics http.Handler, p *Prober) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	p.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "not_found"})
	})
	return mux
}

// Listen checks cfg, binds every address now (so a bad address or a port
// in use is a startup error) and serves each one on its own http.Server
// until the process exits. It refuses an address that is neither loopback
// nor private when no token is set, and an unreadable, empty or short
// token file. It returns the servers, each with Addr set to the address it
// is bound to; nil when the listener is off.
func Listen(cfg ListenerConfig) ([]*http.Server, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	addrs := ParseAddrs(cfg.Addrs)
	if len(addrs) == 0 {
		if strings.TrimSpace(cfg.TokenFile) != "" {
			return nil, errors.New("ops: -ops-token-file is set but -ops-addr is empty")
		}
		return nil, nil
	}
	if cfg.Handler == nil {
		return nil, errors.New("ops: no handler")
	}
	token, err := ReadToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if err := CheckAddr(a, token != ""); err != nil {
			return nil, err
		}
	}
	handler := RequireToken(token, cfg.Handler)
	listeners := make([]net.Listener, 0, len(addrs))
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, fmt.Errorf("ops: listen %s: %w", a, err)
		}
		listeners = append(listeners, ln)
	}
	servers := make([]*http.Server, 0, len(listeners))
	for _, ln := range listeners {
		srv := &http.Server{
			Addr:              ln.Addr().String(), // the bound address (":0" resolved); informational
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		servers = append(servers, srv)
		addr := srv.Addr
		logf("ops: listening on %s (/metrics, /v1/livez, /v1/readyz; bearer token required: %v)", addr, token != "")
		go func(srv *http.Server, ln net.Listener, addr string) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("ops: serve %s: %v", addr, err)
			}
		}(srv, ln, addr)
	}
	return servers, nil
}

// ParseAddrs splits a comma-separated -ops-addr value, dropping blanks.
func ParseAddrs(csv string) []string {
	var out []string
	for _, a := range strings.Split(csv, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// ReadToken reads the ops token from path, trimming surrounding
// whitespace. An empty path is "no token". The token itself never appears
// in an error.
func ReadToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("ops: read -ops-token-file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("ops: -ops-token-file %s is empty", path)
	}
	if len(tok) < minTokenLen {
		return "", fmt.Errorf("ops: the token in -ops-token-file %s is shorter than %d characters", path, minTokenLen)
	}
	return tok, nil
}

// CheckAddr validates one -ops-addr entry. Without a token the host must
// be loopback or private (see PrivateHost); an empty host or 0.0.0.0
// listens on every interface and counts as public.
func CheckAddr(addr string, haveToken bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ops: -ops-addr %q: %v", addr, err)
	}
	if port == "" {
		return fmt.Errorf("ops: -ops-addr %q has no port", addr)
	}
	if haveToken || PrivateHost(host) {
		return nil
	}
	return fmt.Errorf("ops: -ops-addr %s is not a loopback or private address; set -ops-token-file to serve metrics there", addr)
}

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598): not routed on the public
// internet; overlay networks commonly use it.
var sharedAddressSpace = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// PrivateHost reports whether host is "localhost" or an IP literal that is
// loopback, private (RFC 1918, RFC 4193), link-local or in 100.64.0.0/10.
// Any other name, an empty host and an unspecified address are public: a
// name may resolve anywhere, and the others listen on every interface.
func PrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // IPv6 zone, e.g. fe80::1%eth0
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || sharedAddressSpace.Contains(ip)
}

// RequireToken wraps h so that, when token is set, every request must send
// "Authorization: Bearer <token>", from any peer: a loopback peer is not
// trusted either, because a TCP relay on the same host would make any
// remote caller look local. The comparison is constant-time over SHA-256
// digests, so neither the token nor its length leaks through timing. An
// empty token returns h.
func RequireToken(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="toolyard-ops"`)
			writeJSON(w, r, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

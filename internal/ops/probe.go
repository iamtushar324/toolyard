package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Probe paths. The unprefixed forms are aliases for load balancers and
// orchestrators that expect them; /healthz is liveness.
const (
	LivezPath  = "/v1/livez"
	ReadyzPath = "/v1/readyz"
)

var probePaths = map[string]bool{
	LivezPath:  true,
	ReadyzPath: true,
	"/livez":   true,
	"/readyz":  true,
	"/healthz": true,
}

// IsProbePath reports whether path is one of the probe routes. They are
// public, read-only and need no session (internal/api exempts them from
// the role guard and the API hardening for GET and HEAD).
func IsProbePath(path string) bool { return probePaths[path] }

// Readiness reasons, in the order Ready checks them.
const (
	ReasonDraining = "draining"
	ReasonKeys     = "keys"
	ReasonStarting = "starting"
	ReasonDB       = "db"
)

const (
	// dbPingTimeout bounds one readiness ping of the database.
	dbPingTimeout = time.Second
	// readyCacheTTL is how long one ping result answers every probe, so
	// the public route cannot put load on the database.
	readyCacheTTL = time.Second
)

// Prober answers the liveness and readiness probes for one State and one
// database.
type Prober struct {
	state *State
	ping  func(context.Context) error
	now   func() time.Time

	mu     sync.Mutex
	pinged bool
	pingAt time.Time
	pingOK bool
}

// NewProber returns a Prober over state (nil means Default) that pings the
// database with ping. A nil ping is never ready (reason "db").
func NewProber(state *State, ping func(context.Context) error) *Prober {
	if state == nil {
		state = Default
	}
	return &Prober{state: state, ping: ping, now: time.Now}
}

// Ready reports whether the process may take traffic. reason is "" when
// it may; otherwise it is ReasonDraining, ReasonKeys, ReasonStarting or
// ReasonDB, checked in that order. The flags are read on every call; the
// database is pinged at most once per readyCacheTTL and concurrent callers
// share that result.
func (p *Prober) Ready() (ok bool, reason string) {
	switch {
	case p.state.Draining():
		return false, ReasonDraining
	case !p.state.KeysVerified():
		return false, ReasonKeys
	case !p.state.Started():
		return false, ReasonStarting
	case !p.dbUp():
		return false, ReasonDB
	}
	return true, ""
}

func (p *Prober) dbUp() bool {
	if p.ping == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pinged {
		if age := p.now().Sub(p.pingAt); age >= 0 && age < readyCacheTTL {
			return p.pingOK
		}
	}
	// The request's own context is not used: a caller that hangs up must
	// not cache a failure for everyone else.
	ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
	err := p.ping(ctx)
	cancel()
	p.pinged, p.pingAt, p.pingOK = true, p.now(), err == nil
	return p.pingOK
}

// probeBody is the whole probe response: a status and, when not ready, a
// reason. Nothing internal (errors, versions, counts) is ever included.
type probeBody struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Livez answers GET and HEAD with 200 {"status":"live"}. It reads nothing:
// a process that can run this handler is alive.
func Livez(w http.ResponseWriter, r *http.Request) {
	if !probeMethod(w, r) {
		return
	}
	writeProbe(w, r, http.StatusOK, probeBody{Status: "live"})
}

// Readyz answers GET and HEAD with 200 {"status":"ready"}, or 503
// {"status":"not_ready","reason":"…"} (see Ready).
func (p *Prober) Readyz(w http.ResponseWriter, r *http.Request) {
	if !probeMethod(w, r) {
		return
	}
	if ok, reason := p.Ready(); !ok {
		writeProbe(w, r, http.StatusServiceUnavailable, probeBody{Status: "not_ready", Reason: reason})
		return
	}
	writeProbe(w, r, http.StatusOK, probeBody{Status: "ready"})
}

// Register mounts the probes on mux: /v1/livez, /v1/readyz and the aliases
// /livez, /readyz and /healthz (liveness).
func (p *Prober) Register(mux *http.ServeMux) {
	mux.HandleFunc(LivezPath, Livez)
	mux.HandleFunc(ReadyzPath, p.Readyz)
	mux.HandleFunc("/livez", Livez)
	mux.HandleFunc("/readyz", p.Readyz)
	mux.HandleFunc("/healthz", Livez)
}

func probeMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, r, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	return false
}

func writeProbe(w http.ResponseWriter, r *http.Request, status int, body probeBody) {
	writeJSON(w, r, status, body)
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

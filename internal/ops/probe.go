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
	// dbPingTimeout bounds one readiness ping of the database. The
	// database has a single connection, so a ping waits behind any long
	// query; give it time rather than call a busy database down.
	dbPingTimeout = 3 * time.Second
	// readyCacheTTL is how long one ping result answers every probe, so
	// the public route cannot put load on the database: at most one ping
	// starts per readyCacheTTL.
	readyCacheTTL = time.Second
	// dbFailuresBeforeDown is how many pings in a row must fail before
	// readiness reports "db". One slow or failed ping is not an outage.
	dbFailuresBeforeDown = 2
)

// Prober answers the liveness and readiness probes for one State and one
// database.
//
// The database check never makes a probe wait on a busy database, except
// the very first one: it answers from the latest ping results and, when
// they are older than readyCacheTTL, starts one new ping in the background
// for the next probe to read. The database is "down" when no ping has
// succeeded yet or the last dbFailuresBeforeDown pings all failed.
type Prober struct {
	state *State
	ping  func(context.Context) error
	now   func() time.Time

	mu        sync.Mutex
	pinged    bool          // a ping has completed
	checkedAt time.Time     // when the last ping completed
	lastOK    time.Time     // when a ping last succeeded; zero if never
	failures  int           // failed pings in a row
	inflight  chan struct{} // closed when the running ping completes; nil when none runs
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
// database check is described on Prober.
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
	if !p.pinged {
		// No result yet: the first check waits for its ping.
		done := p.startPingLocked()
		p.mu.Unlock()
		<-done
		p.mu.Lock()
	} else if age := p.now().Sub(p.checkedAt); age < 0 || age >= readyCacheTTL {
		p.startPingLocked() // answered from the last results below
	}
	up := !p.lastOK.IsZero() && p.failures < dbFailuresBeforeDown
	p.mu.Unlock()
	return up
}

// startPingLocked starts a ping unless one is running, and returns the
// channel closed when the running ping completes. p.mu must be held.
func (p *Prober) startPingLocked() <-chan struct{} {
	if p.inflight != nil {
		return p.inflight
	}
	done := make(chan struct{})
	p.inflight = done
	go func() {
		// Not the request's context: a caller that hangs up must not
		// record a failure for everyone else.
		ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
		err := p.ping(ctx)
		cancel()
		p.mu.Lock()
		now := p.now()
		p.pinged, p.checkedAt, p.inflight = true, now, nil
		if err == nil {
			p.lastOK, p.failures = now, 0
		} else {
			p.failures++
		}
		p.mu.Unlock()
		close(done)
	}()
	return done
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

// Package cluster lets several toolyard processes ("nodes") share one
// Postgres database safely (TEC-1630):
//
//   - leader leases, for the background loops that must run on one node at
//     a time (RunLeader);
//   - a small publish/subscribe channel, so a write on one node drops the
//     matching cache or wakes the matching waiter on every node (Publish,
//     Subscribe);
//   - named locks across nodes, for the read-modify-write sections that an
//     in-process mutex guarded while there was one process (Lock, LockTx);
//   - work leases on the rows a node executes (LeaseUntilSQL, RenewLeases).
//
// On Postgres a leader lease is a session advisory lock
// (pg_try_advisory_lock) on a connection of the cluster's own, outside the
// pool; named locks use a second one, lease renewals a third, and messages
// travel by LISTEN/NOTIFY on a fourth, each reopened after a failure. These
// connections ask the server for short TCP keepalives, so a node that
// vanishes loses its locks within about half a minute. On SQLite (one
// process per database), or with Options.SingleNode, the helper is a
// single-node no-op: every lease is granted at once and Publish delivers in
// process.
//
// A nil *Cluster also behaves as a single node, so a service may hold an
// optional one: RunLeader runs fn, Lock takes an in-process lock, Publish
// and Subscribe do nothing.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	// Resync is the payload a subscriber gets on the periodic resync, after
	// the node reconnects to the message channel, and after messages of its
	// topic were dropped: messages may have been missed, so reload what the
	// topic covers. It is never a published payload (it holds a NUL byte,
	// which NOTIFY refuses).
	Resync = "\x00resync"
	// MaxPayload is the largest payload Publish accepts, in bytes.
	MaxPayload = 1024
	// MaxTopic is the longest topic name, in bytes.
	MaxTopic = 64
	// MaxLargePayload is the largest payload PublishLarge accepts, in
	// bytes.
	MaxLargePayload = 1 << 20

	// DefaultRetryInterval is how often a node that does not hold a leader
	// lease tries to take it.
	DefaultRetryInterval = 5 * time.Second
	// DefaultCheckInterval is how often the connections that hold leases
	// and locks are checked; when one fails, everything it held is lost.
	DefaultCheckInterval = 2 * time.Second
	// DefaultResyncInterval is how often subscribers get a Resync call on
	// Postgres.
	DefaultResyncInterval = 30 * time.Second
	// DefaultModeWait is how long New waits for a gateway with the other
	// -cluster setting to leave the database (a short overlap during an
	// update) before it returns ErrModeConflict.
	DefaultModeWait = 60 * time.Second
	// modeRetry is how often New tries the mode lock again while it waits.
	modeRetry = 2 * time.Second

	// channel is the one LISTEN/NOTIFY channel every node uses.
	channel = "toolyard_cluster"
	// queueSize bounds the delivery queue; a full queue drops the message
	// and delivers a Resync to its topic once the queue drains.
	queueSize = 1024
	// queryTimeout bounds each query on a dedicated connection.
	queryTimeout = 10 * time.Second
	// largeKeep is how long a PublishLarge row stays in cluster_messages.
	largeKeep = 5 * time.Minute
	// listenPing is how long the LISTEN connection may stay silent before
	// it is pinged; a ping that fails reopens it.
	listenPing = 30 * time.Second
)

var (
	// ErrPayloadTooLarge is Publish's error for a payload over MaxPayload.
	ErrPayloadTooLarge = errors.New("cluster: payload larger than 1 KiB")
	// ErrBadTopic is Publish's error for an empty topic, a topic over
	// MaxTopic, or one holding a NUL byte.
	ErrBadTopic = errors.New("cluster: a topic is 1 to 64 bytes with no NUL byte")
	// errClosed is the error of a lock or query asked of a closed Cluster.
	errClosed = errors.New("cluster: closed")
	// ErrModeConflict is New's error when another gateway uses the
	// database with the other setting: a single-node gateway
	// (SingleNode, serve -cluster=off) next to any other gateway, or a
	// replica next to a single-node gateway.
	ErrModeConflict = errors.New("cluster: another gateway uses this database with the other -cluster setting (off allows one gateway per database)")
)

// modeKey is the advisory lock a gateway holds for its life to state its
// mode: exclusively when single-node, shared when it is one of several
// replicas, so the two modes never run on one database together.
var modeKey = LockKey("cluster-mode")

// Options tune New. The zero value is ready to use.
type Options struct {
	// InstanceID names this node in lease columns and logs. Every process
	// adds a random suffix of its own (<InstanceID>-<8 hex digits>), so a
	// restarted container, or two replicas given the same name, never
	// share an ID. "" uses the host name.
	InstanceID string
	// SingleNode makes the helper a single-node no-op on Postgres too
	// (toolyard serve -cluster=off): no leader leases in the database, no
	// LISTEN, MultiNode false. For a database that one process uses.
	SingleNode bool
	// RetryInterval is how often RunLeader tries to take a lease it does
	// not hold (with up to 20% jitter), and how long it waits after fn
	// fails. <= 0 means DefaultRetryInterval.
	RetryInterval time.Duration
	// CheckInterval is how often the lease and lock connections are
	// checked. <= 0 means DefaultCheckInterval.
	CheckInterval time.Duration
	// ResyncInterval is how often every subscriber gets a Resync call on
	// Postgres. 0 means DefaultResyncInterval; < 0 turns it off. A single
	// node never resyncs: in-process delivery cannot miss a message.
	ResyncInterval time.Duration
	// ModeWait is how long New tries the mode lock again while another
	// gateway holds it with the other -cluster setting. 0 means
	// DefaultModeWait; < 0 means no retry (New fails at once).
	ModeWait time.Duration
	// Logger receives the cluster's log lines; nil means logx.For("cluster").
	Logger *slog.Logger
}

// Cluster is the helper New returns. Its methods are safe for concurrent
// use, and on a nil *Cluster (see the package comment).
type Cluster struct {
	db   *store.DB
	pg   bool
	id   string
	opts Options
	log  *slog.Logger

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
	draining  atomic.Bool

	// Leader leases (leases.go): held is guarded by leases.mu.
	leases slot
	held   map[int64]*lease

	// Named locks (locks.go): named is guarded by locks.mu.
	locks slot
	named map[int64]*lease
	local keyedMutex

	// Lease renewals and other short writes (work.go).
	work slot

	// mode holds the mode lock (modeKey) on Postgres, single-node or not.
	mode     slot
	modeHeld bool // guarded by mode.mu
	// conflict closes when a gateway with the other -cluster setting took
	// the mode lock after this node lost it (ModeConflict).
	conflict     chan struct{}
	conflictOnce sync.Once

	// Messages (messages.go).
	subMu     sync.RWMutex
	subs      map[string]map[uint64]func(string)
	nextSub   uint64
	queue     chan delivery
	dropMu    sync.Mutex
	dropTopic map[string]bool // topics whose messages were dropped
	dropAll   bool            // a Resync for everyone was dropped
	lastPrune atomic.Int64

	// listenConn is the LISTEN connection, for tests that cut it.
	listenMu   sync.Mutex
	listenConn *pgx.Conn
}

// slot is one dedicated connection, reopened on demand after a failure.
// mu serializes its use (a *pgx.Conn is not safe for concurrent use).
type slot struct {
	purpose string
	mu      sync.Mutex
	conn    *pgx.Conn
	gen     uint64 // bumped whenever conn changes
	dialLog logOnce
	// onDrop runs, mu held, after the connection was dropped: what it held
	// is gone with the session.
	onDrop func()
}

// New starts the cluster helper. On SQLite, with a nil db, or with
// opts.SingleNode, it is a single-node no-op: every leader lease is granted
// and Publish delivers in process. On Postgres it opens its LISTEN
// connection before it returns (bounded by ctx and a few seconds; a
// failure is logged and retried in the background, so a database that is
// briefly unreachable does not stop the start) and opens its other
// connections on first use. ctx bounds the helper's life: it stops when
// ctx ends or Close is called, and either releases every lease and lock it
// holds.
func New(ctx context.Context, db *store.DB, opts Options) (*Cluster, error) {
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = DefaultRetryInterval
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = DefaultCheckInterval
	}
	if opts.ResyncInterval == 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	id := newInstanceID(opts.InstanceID)
	lg := opts.Logger
	if lg == nil {
		lg = logx.For("cluster")
	}
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Cluster{
		db:        db,
		pg:        db != nil && db.Dialect.IsPostgres() && !opts.SingleNode,
		id:        id,
		opts:      opts,
		log:       lg.With("instance", id),
		ctx:       cctx,
		cancel:    cancel,
		leases:    slot{purpose: "leases"},
		locks:     slot{purpose: "locks"},
		work:      slot{purpose: "work"},
		mode:      slot{purpose: "mode"},
		held:      map[int64]*lease{},
		named:     map[int64]*lease{},
		subs:      map[string]map[uint64]func(string){},
		queue:     make(chan delivery, queueSize),
		dropTopic: map[string]bool{},
		conflict:  make(chan struct{}),
	}
	c.leases.onDrop = func() {
		if len(c.held) > 0 && c.ctx.Err() == nil {
			c.log.Warn("cluster: every leader lease of this node is released with its connection", "leases", len(c.held))
		}
		cancelAll(c.held)
	}
	c.locks.onDrop = func() {
		if len(c.named) > 0 && c.ctx.Err() == nil {
			c.log.Warn("cluster: every named lock of this node is released with its connection", "locks", len(c.named))
		}
		cancelAll(c.named)
	}
	c.mode.onDrop = func() { c.modeHeld = false }
	if db != nil && db.Dialect.IsPostgres() {
		// State this gateway's mode before anything else runs.
		if err := c.takeModeAtStart(ctx); err != nil {
			cancel()
			c.mode.mu.Lock()
			if c.mode.conn != nil {
				closeConn(c.mode.conn)
				c.mode.conn = nil
			}
			c.mode.mu.Unlock()
			return nil, err
		}
		c.wg.Add(1)
		go c.keepMode()
	}
	c.wg.Add(1)
	go c.dispatch()
	if c.pg {
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		first, err := c.dialListen(dctx)
		dcancel()
		if err != nil {
			c.log.Warn("cluster: cannot open the message connection yet; retrying in the background", "err", err.Error())
		}
		c.wg.Add(2)
		go c.listen(first)
		go c.checkConns()
		if opts.ResyncInterval > 0 {
			c.wg.Add(1)
			go c.resyncLoop()
		}
	}
	// The helper stops with the caller's context too.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	go func() {
		<-cctx.Done()
		stop()
	}()
	return c, nil
}

// InstanceID names this node; stable for the process lifetime and unique
// to it. A nil Cluster returns a process-wide ID.
func (c *Cluster) InstanceID() string {
	if c == nil {
		return processID()
	}
	return c.id
}

// MultiNode reports whether several nodes may share the database
// (Postgres, unless SingleNode). It is false on SQLite and for a nil
// Cluster, where a code path that only matters across nodes may be
// skipped.
func (c *Cluster) MultiNode() bool { return c != nil && c.pg }

// Close stops the helper: background work ends, the dedicated connections
// close and every lease and lock this node holds is released (their
// contexts are cancelled). Safe to call more than once.
func (c *Cluster) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.cancel()
		c.wg.Wait()
		for _, s := range []*slot{&c.leases, &c.locks, &c.work, &c.mode} {
			s.mu.Lock()
			if s.conn != nil {
				closeConn(s.conn)
				s.conn = nil
			}
			s.gen++
			if s.onDrop != nil {
				s.onDrop()
			}
			s.mu.Unlock()
		}
	})
	return nil
}

// ---- dedicated connections --------------------------------------------------------

// connLocked returns s's connection, opening it when needed. s.mu is held.
func (c *Cluster) connLocked(s *slot) (*pgx.Conn, error) {
	if c.closed.Load() || c.ctx.Err() != nil {
		return nil, errClosed
	}
	if s.conn != nil && !s.conn.IsClosed() {
		return s.conn, nil
	}
	if s.conn != nil {
		// It died since the last check: what it held went with it.
		c.dropLocked(s, "connection closed", errors.New("connection closed"))
	}
	dctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	conn, err := c.db.ConnectPostgres(dctx, c.appName(s.purpose))
	if err != nil {
		s.dialLog.warn(c.log, "cluster: cannot open the "+s.purpose+" connection; waiting for the database", err)
		return nil, err
	}
	s.dialLog.reset()
	s.conn = conn
	s.gen++
	return conn, nil
}

// dropLocked closes s's connection after a failure; what it held is gone.
// s.mu is held.
func (c *Cluster) dropLocked(s *slot, what string, err error) {
	if c.ctx.Err() == nil {
		c.log.Warn("cluster: a dedicated connection failed", "connection", s.purpose, "during", what, "err", err.Error())
	}
	if s.conn != nil {
		closeConn(s.conn)
		s.conn = nil
	}
	s.gen++
	if s.onDrop != nil {
		s.onDrop()
	}
}

// checkConns pings the lease and lock connections while they hold
// something, so a session the server ended (network loss, failover,
// termination) cancels what it held within CheckInterval.
func (c *Cluster) checkConns() {
	defer c.wg.Done()
	t := time.NewTicker(c.opts.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		c.checkSlot(&c.leases, func() int { return len(c.held) })
		c.checkSlot(&c.locks, func() int { return len(c.named) })
	}
}

func (c *Cluster) checkSlot(s *slot, holding func() int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || holding() == 0 {
		return
	}
	qctx, cancel := context.WithTimeout(c.ctx, max(c.opts.CheckInterval, time.Second))
	err := s.conn.Ping(qctx)
	cancel()
	if err != nil && c.ctx.Err() == nil {
		c.dropLocked(s, "check", err)
	}
}

// appName is the application_name of a dedicated connection: its purpose
// and this node's instance ID (pg_stat_activity shows who holds what).
func (c *Cluster) appName(purpose string) string {
	return "toolyard " + purpose + " " + c.id
}

// takeModeLocked takes the mode lock on the mode connection: exclusive
// for a single node, shared for a replica. c.mode.mu is held.
func (c *Cluster) takeModeLocked() error {
	conn, err := c.connLocked(&c.mode)
	if err != nil {
		return err
	}
	q := `SELECT pg_try_advisory_lock_shared($1)`
	if !c.pg {
		q = `SELECT pg_try_advisory_lock($1)`
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	var got bool
	if err := conn.QueryRow(qctx, q, modeKey).Scan(&got); err != nil {
		c.dropLocked(&c.mode, "take the mode lock", err)
		return err
	}
	if !got {
		return ErrModeConflict
	}
	c.modeHeld = true
	return nil
}

// takeModeAtStart takes the mode lock for New. While a gateway with the
// other setting holds it, it tries again every modeRetry for up to
// Options.ModeWait, so a short overlap during an update does not stop the
// start; then it logs who holds the lock and returns ErrModeConflict.
// Other errors (the database is down) return at once.
func (c *Cluster) takeModeAtStart(ctx context.Context) error {
	wait := c.opts.ModeWait
	if wait == 0 {
		wait = DefaultModeWait
	}
	deadline := time.Now().Add(wait)
	warned := false
	for {
		c.mode.mu.Lock()
		err := c.takeModeLocked()
		var holders []string
		if errors.Is(err, ErrModeConflict) {
			holders = c.modeHoldersLocked()
		}
		c.mode.mu.Unlock()
		if !errors.Is(err, ErrModeConflict) {
			return err
		}
		if wait < 0 || !time.Now().Before(deadline) {
			c.log.Error("cluster: another gateway holds this database with the other -cluster setting", "holders", holders, "waited", max(wait, 0).String())
			return err
		}
		if !warned {
			warned = true
			c.log.Warn("cluster: another gateway holds this database with the other -cluster setting; waiting for it to stop", "holders", holders, "wait", wait.String())
		}
		t := time.NewTimer(min(modeRetry, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// modeHoldersLocked names the other sessions that hold the mode lock (their
// application_name and lock mode), for the conflict log line. c.mode.mu is
// held. It returns nil when the query fails.
func (c *Cluster) modeHoldersLocked() []string {
	if c.mode.conn == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	// A bigint advisory key is stored as classid (high 32 bits), objid (low
	// 32 bits), objsubid 1.
	rows, err := c.mode.conn.Query(qctx, `SELECT COALESCE(a.application_name, ''), l.mode
FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory' AND l.granted AND l.pid <> pg_backend_pid()
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND l.classid::bigint = $1 AND l.objid::bigint = $2 AND l.objsubid = 1`,
		int64(uint32(uint64(modeKey)>>32)), int64(uint32(uint64(modeKey))))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app, mode string
		if rows.Scan(&app, &mode) != nil {
			return out
		}
		if app == "" {
			app = "(no application_name)"
		}
		out = append(out, app+" ("+mode+")")
	}
	return out
}

// ModeConflict closes when this node lost its mode lock (its connection
// failed) and a gateway with the other -cluster setting took the lock
// before this node could take it again. The process must stop: the guard
// no longer holds. A nil Cluster returns nil (never ready).
func (c *Cluster) ModeConflict() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.conflict
}

// keepMode checks the mode connection and takes the mode lock again after
// the connection failed, so the guard holds for the process's life. When a
// gateway with the other setting took the lock in between, it closes the
// ModeConflict channel and stops; a database that is down stays a warning
// and is retried.
func (c *Cluster) keepMode() {
	defer c.wg.Done()
	t := time.NewTicker(max(c.opts.CheckInterval, time.Second))
	defer t.Stop()
	var lg logOnce
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		c.mode.mu.Lock()
		if c.modeHeld {
			qctx, cancel := context.WithTimeout(c.ctx, max(c.opts.CheckInterval, time.Second))
			err := c.mode.conn.Ping(qctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.dropLocked(&c.mode, "check the mode lock", err)
			}
		}
		if !c.modeHeld && c.ctx.Err() == nil {
			err := c.takeModeLocked()
			switch {
			case errors.Is(err, ErrModeConflict):
				holders := c.modeHoldersLocked()
				c.mode.mu.Unlock()
				c.log.Error("cluster: a gateway with the other -cluster setting took this database while this node was disconnected; stopping", "holders", holders)
				c.conflictOnce.Do(func() { close(c.conflict) })
				return
			case err != nil:
				lg.warn(c.log, "cluster: cannot take the mode lock again; retrying", err)
			case lg.reset():
				c.log.Info("cluster: mode lock taken again")
			}
		}
		c.mode.mu.Unlock()
	}
}

// ---- helpers --------------------------------------------------------------------

// lease is one leader lease or named lock this node holds.
type lease struct {
	gen    uint64 // slot generation of the connection that holds it
	cancel context.CancelFunc
}

func cancelAll(m map[int64]*lease) {
	for k, l := range m {
		l.cancel()
		delete(m, k)
	}
}

// closeConn closes a dedicated connection, bounded.
func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// sleepCtx waits d or until ctx ends; false means ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter returns d plus up to 20%, so nodes do not retry in step.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(mrand.Int64N(int64(d)/5+1))
}

var (
	processIDOnce sync.Once
	processIDVal  string
)

// processID is the instance ID of a nil Cluster: one per process.
func processID() string {
	processIDOnce.Do(func() { processIDVal = newInstanceID("") })
	return processIDVal
}

// newInstanceID returns <base>-<8 random hex digits>, base being name, or
// the host name when name is empty, cleaned to [A-Za-z0-9._-] and at most
// 63 bytes. The random part is new in every process.
func newInstanceID(name string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base, _ = os.Hostname()
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		}
		return -1
	}, base)
	if len(base) > 63 {
		base = base[:63]
	}
	if base == "" {
		base = "toolyard"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return base + "-" + hex.EncodeToString(b[:])
}

// logOnce logs a repeating failure once until reset.
type logOnce struct{ failing atomic.Bool }

func (l *logOnce) warn(lg *slog.Logger, msg string, err error) {
	if !l.failing.Swap(true) {
		lg.Warn(msg, "err", err.Error())
	}
}

// reset reports whether a failure had been logged.
func (l *logOnce) reset() bool { return l.failing.Swap(false) }

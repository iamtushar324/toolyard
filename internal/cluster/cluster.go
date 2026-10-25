// Package cluster lets several toolyard processes ("nodes") share one
// Postgres database safely (TEC-1630):
//
//   - leader leases, for the background loops that must run on one node at
//     a time (RunLeader);
//   - a small publish/subscribe channel, so a write on one node drops the
//     matching cache or wakes the matching waiter on every node (Publish,
//     Subscribe);
//   - named locks across nodes, for the read-modify-write sections that an
//     in-process mutex guarded while there was one process (Lock, LockTx).
//
// On Postgres a leader lease is a session advisory lock
// (pg_try_advisory_lock) on a connection of the cluster's own, outside the
// pool; messages travel by LISTEN/NOTIFY on a second connection of its
// own, which reconnects after a failure. On SQLite (one process per
// database) the helper is a single-node no-op: every lease is granted at
// once and Publish delivers in process.
//
// A nil *Cluster also behaves as a single node, so a service may hold an
// optional one: RunLeader runs fn, Lock takes an in-process lock, Publish
// and Subscribe do nothing.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
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
	// the node reconnects to the message channel, and after messages were
	// dropped: messages may have been missed, so reload what the topic
	// covers. It is never a published payload (it holds a NUL byte, which
	// NOTIFY refuses).
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
	// DefaultCheckInterval is how often the connection that holds the
	// leases is checked; when it fails, every lease of this node is lost.
	DefaultCheckInterval = 2 * time.Second
	// DefaultResyncInterval is how often subscribers get a Resync call on
	// Postgres.
	DefaultResyncInterval = 30 * time.Second

	// WorkLease is how long a row a node executes (an Inbox execution, a
	// legacy execution claim, a code run, a memory-webhook job) stays
	// claimed without a renewal. Recovery takes a row only once its lease
	// ended.
	WorkLease = 60 * time.Second
	// WorkLeaseRenew is how often a node renews the leases of the work it
	// runs.
	WorkLeaseRenew = 15 * time.Second

	// channel is the one LISTEN/NOTIFY channel every node uses.
	channel = "toolyard_cluster"
	// queueSize bounds the delivery queue; a full queue drops the message
	// and delivers a Resync once the queue drains.
	queueSize = 1024
	// queryTimeout bounds each query on a dedicated connection.
	queryTimeout = 10 * time.Second
	// largeKeep is how long a PublishLarge row stays in cluster_messages.
	largeKeep = 5 * time.Minute
)

var (
	// ErrPayloadTooLarge is Publish's error for a payload over MaxPayload.
	ErrPayloadTooLarge = errors.New("cluster: payload larger than 1 KiB")
	// ErrBadTopic is Publish's error for an empty topic, a topic over
	// MaxTopic, or one holding a NUL byte.
	ErrBadTopic = errors.New("cluster: a topic is 1 to 64 bytes with no NUL byte")
)

// Options tune New. The zero value is ready to use.
type Options struct {
	// InstanceID names this node in lease columns and logs. "" picks
	// <hostname>-<random>, stable for the process lifetime.
	InstanceID string
	// RetryInterval is how often RunLeader tries to take a lease it does
	// not hold (with up to 20% jitter), and how long it waits after fn
	// fails. <= 0 means DefaultRetryInterval.
	RetryInterval time.Duration
	// CheckInterval is how often the lease connection is checked. <= 0
	// means DefaultCheckInterval.
	CheckInterval time.Duration
	// ResyncInterval is how often every subscriber gets a Resync call on
	// Postgres. 0 means DefaultResyncInterval; < 0 turns it off. SQLite
	// never resyncs: in-process delivery cannot miss a message.
	ResyncInterval time.Duration
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

	// Leases. lockMu guards every field below and serializes the use of
	// lockConn (a *pgx.Conn is not safe for concurrent use).
	lockMu   sync.Mutex
	lockConn *pgx.Conn
	lockGen  uint64 // bumped whenever lockConn changes
	held     map[int64]*lease
	closed   bool
	dialLog  logOnce

	// Messages.
	subMu   sync.RWMutex
	subs    map[string]map[uint64]func(string)
	nextSub uint64
	queue   chan delivery
	dropped atomic.Bool
	// lastPrune is when PublishLarge last deleted old cluster_messages
	// rows (Unix milliseconds).
	lastPrune atomic.Int64

	// listenConn is the LISTEN connection, for tests that cut it.
	listenMu   sync.Mutex
	listenConn *pgx.Conn

	// locks is the in-process half of Lock.
	locks keyedMutex
}

// lease is one leader lease this node holds.
type lease struct {
	gen    uint64 // lockGen of the connection that holds it
	cancel context.CancelFunc
}

// delivery is one queued message, or a Resync for every subscriber. A
// message stored by PublishLarge on another node arrives as ref, the
// cluster_messages row to read.
type delivery struct {
	topic   string
	payload string
	ref     string
	resync  bool
}

// wireMsg is a message as NOTIFY carries it: the payload, or the
// cluster_messages row that holds it (Ref).
type wireMsg struct {
	Instance string `json:"n"`
	Topic    string `json:"t"`
	Payload  string `json:"p,omitempty"`
	Ref      string `json:"r,omitempty"`
}

// New starts the cluster helper. On SQLite (or with a nil db) it is a
// single-node no-op: every leader lease is granted and Publish delivers in
// process. On Postgres it opens its LISTEN connection before it returns
// (bounded by ctx and a few seconds; a failure is logged and retried in the
// background, so a database that is briefly unreachable does not stop the
// start) and opens its lease connection on first use. The helper stops
// when ctx ends or Close is called; either releases every lease it holds.
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
	id := strings.TrimSpace(opts.InstanceID)
	if id == "" {
		id = newInstanceID()
	}
	if len(id) > 128 {
		id = id[:128]
	}
	lg := opts.Logger
	if lg == nil {
		lg = logx.For("cluster")
	}
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Cluster{
		db:     db,
		pg:     db != nil && db.Dialect.IsPostgres(),
		id:     id,
		opts:   opts,
		log:    lg.With("instance", id),
		ctx:    cctx,
		cancel: cancel,
		held:   map[int64]*lease{},
		subs:   map[string]map[uint64]func(string){},
		queue:  make(chan delivery, queueSize),
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
		go c.checkLeases()
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

// InstanceID names this node; stable for the process lifetime. A nil
// Cluster returns a process-wide ID.
func (c *Cluster) InstanceID() string {
	if c == nil {
		return processID()
	}
	return c.id
}

// MultiNode reports whether several nodes may share the database
// (Postgres). It is false on SQLite and for a nil Cluster, where a code
// path that only matters across nodes may be skipped.
func (c *Cluster) MultiNode() bool { return c != nil && c.pg }

// Close stops the helper: background work ends, the dedicated connections
// close and every lease this node holds is released (its fn's ctx is
// cancelled). Safe to call more than once.
func (c *Cluster) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.cancel()
		c.wg.Wait()
		c.lockMu.Lock()
		c.closed = true
		for k, l := range c.held {
			l.cancel()
			delete(c.held, k)
		}
		if c.lockConn != nil {
			closeConn(c.lockConn)
			c.lockConn = nil
		}
		c.lockMu.Unlock()
	})
	return nil
}

// ---- leader leases -----------------------------------------------------------

// RunLeader runs fn while this process holds the named lease
// (pg_try_advisory_lock on a dedicated connection on Postgres). fn's ctx is
// cancelled when the lease is lost; RunLeader then retries until ctx ends.
// It blocks until ctx ends or fn returns nil while it still holds the lease
// (the work is finished); an error from fn is logged and fn runs again
// after the retry interval, under a fresh lease. A panic in fn releases the
// lease and propagates (run RunLeader under goroutines.Loop to restart it).
// Within one process, two RunLeader calls with the same name also take
// turns. On SQLite the lease is granted at once.
func (c *Cluster) RunLeader(ctx context.Context, name string, fn func(ctx context.Context) error) {
	if c == nil {
		runUnleased(ctx, name, fn)
		return
	}
	key := LockKey("leader:" + name)
	lg := c.log.With("lease", name)
	wait := time.Duration(0) // the first try is immediate
	for {
		if wait > 0 && !sleepCtx(ctx, jitter(wait)) {
			return
		}
		wait = c.opts.RetryInterval
		lctx, release, ok := c.acquire(ctx, key)
		if !ok {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if c.pg {
			lg.Info("cluster: leader lease taken")
		}
		lost, err := runHeld(lctx, fn, release)
		if ctx.Err() != nil {
			return
		}
		if lost {
			lg.Warn("cluster: leader lease lost; the task stopped and will take the lease again when it can")
			continue
		}
		if err != nil {
			lg.Error("cluster: leader task failed; retrying", "err", err.Error())
			continue
		}
		return
	}
}

// runHeld runs fn under a held lease and releases it, also on a panic.
// lost reports that the lease ended before fn returned.
func runHeld(lctx context.Context, fn func(context.Context) error, release func()) (lost bool, err error) {
	defer release()
	err = fn(lctx)
	return lctx.Err() != nil, err
}

// runUnleased is RunLeader for a nil Cluster: fn runs at once, again after
// an error.
func runUnleased(ctx context.Context, name string, fn func(context.Context) error) {
	for {
		err := fn(ctx)
		if ctx.Err() != nil || err == nil {
			return
		}
		logx.For("cluster").Error("cluster: task failed; retrying", "lease", name, "err", err.Error())
		if !sleepCtx(ctx, jitter(DefaultRetryInterval)) {
			return
		}
	}
}

// acquire takes the lease key if no one holds it. It returns a context
// that ends when the lease is lost or ctx ends, and the func that releases
// the lease.
func (c *Cluster) acquire(ctx context.Context, key int64) (context.Context, func(), bool) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return nil, nil, false
	}
	if _, ok := c.held[key]; ok {
		return nil, nil, false
	}
	if c.pg {
		conn, err := c.lockConnLocked()
		if err != nil {
			return nil, nil, false
		}
		// The cluster's context, not the caller's: a query cancelled
		// half-way closes the connection, and with it every lease.
		qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
		var got bool
		err = conn.QueryRow(qctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got)
		cancel()
		if err != nil {
			c.dropLockConnLocked("take a lease", err)
			return nil, nil, false
		}
		if !got {
			return nil, nil, false
		}
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &lease{gen: c.lockGen, cancel: cancel}
	c.held[key] = l
	return lctx, func() { c.release(key, l) }, true
}

// release gives up one lease. A lease lost with its connection is already
// gone: nothing is sent then.
func (c *Cluster) release(key int64, l *lease) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	l.cancel()
	if c.held[key] != l {
		return
	}
	delete(c.held, key)
	if !c.pg || c.lockConn == nil || l.gen != c.lockGen || c.ctx.Err() != nil {
		// Close closes the connection, which releases the lock.
		return
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	var ok bool
	if err := c.lockConn.QueryRow(qctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&ok); err != nil {
		c.dropLockConnLocked("release a lease", err)
	}
}

// lockConnLocked returns the lease connection, opening it when needed.
// lockMu is held.
func (c *Cluster) lockConnLocked() (*pgx.Conn, error) {
	if c.lockConn != nil && !c.lockConn.IsClosed() {
		return c.lockConn, nil
	}
	if c.lockConn != nil {
		// It died since the last check: its leases went with it.
		c.dropLockConnLocked("lease connection closed", errors.New("connection closed"))
	}
	dctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	conn, err := c.db.ConnectPostgres(dctx)
	if err != nil {
		c.dialLog.warn(c.log, "cluster: cannot open the lease connection; leader tasks wait", err)
		return nil, err
	}
	c.dialLog.reset()
	c.lockConn = conn
	c.lockGen++
	return conn, nil
}

// dropLockConnLocked closes the lease connection after a failure and
// cancels every lease it held: the server released them with the session.
// lockMu is held.
func (c *Cluster) dropLockConnLocked(what string, err error) {
	if c.ctx.Err() == nil {
		c.log.Warn("cluster: the lease connection failed; every lease of this node is released", "during", what, "err", err.Error(), "leases", len(c.held))
	}
	if c.lockConn != nil {
		closeConn(c.lockConn)
		c.lockConn = nil
	}
	c.lockGen++
	for k, l := range c.held {
		l.cancel()
		delete(c.held, k)
	}
}

// checkLeases pings the lease connection while it holds a lease, so a
// session the server ended (network loss, failover, termination) stops the
// leader tasks within CheckInterval.
func (c *Cluster) checkLeases() {
	defer c.wg.Done()
	t := time.NewTicker(c.opts.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		c.lockMu.Lock()
		if c.lockConn != nil && len(c.held) > 0 {
			qctx, cancel := context.WithTimeout(c.ctx, max(c.opts.CheckInterval, time.Second))
			err := c.lockConn.Ping(qctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.dropLockConnLocked("check the leases", err)
			}
		}
		c.lockMu.Unlock()
	}
}

// ---- messages ----------------------------------------------------------------

// Publish sends a small message (payload ≤ 1 KiB) to every node, this one
// included (LISTEN/NOTIFY on Postgres). Subscribe delivers it. Subscribers
// also get a periodic "resync" call so a missed message heals within the
// poll interval. The local delivery is queued before the NOTIFY is sent, so
// it happens even when the database is unreachable (the error is still
// returned). A nil Cluster delivers nothing.
func (c *Cluster) Publish(ctx context.Context, topic, payload string) error {
	if err := checkTopic(topic); err != nil {
		return err
	}
	if len(payload) > MaxPayload {
		return ErrPayloadTooLarge
	}
	if strings.IndexByte(payload, 0) >= 0 {
		return errors.New("cluster: a payload holds no NUL byte")
	}
	if c == nil {
		return nil
	}
	c.enqueue(delivery{topic: topic, payload: payload})
	if !c.pg {
		return nil
	}
	body, err := json.Marshal(wireMsg{Instance: c.id, Topic: topic, Payload: payload})
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `SELECT pg_notify(?, ?)`, channel, string(body))
	return err
}

// PublishLarge is Publish for a payload of up to 1 MiB, such as a
// dashboard live event. A payload over MaxPayload is stored in the
// cluster_messages table for a few minutes and the NOTIFY names its row;
// each other node reads it before it calls its subscribers. This node's
// subscribers get the payload at once, as with Publish.
func (c *Cluster) PublishLarge(ctx context.Context, topic, payload string) error {
	if len(payload) <= MaxPayload {
		return c.Publish(ctx, topic, payload)
	}
	if err := checkTopic(topic); err != nil {
		return err
	}
	if len(payload) > MaxLargePayload {
		return errors.New("cluster: payload larger than 1 MiB")
	}
	if strings.IndexByte(payload, 0) >= 0 {
		return errors.New("cluster: a payload holds no NUL byte")
	}
	if c == nil {
		return nil
	}
	c.enqueue(delivery{topic: topic, payload: payload})
	if !c.pg {
		return nil
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	now := time.Now()
	if _, err := c.db.ExecContext(ctx, `INSERT INTO cluster_messages(id, topic, payload, created_at) VALUES (?, ?, ?, ?)`,
		id, topic, payload, now.UnixMilli()); err != nil {
		return err
	}
	if last := c.lastPrune.Load(); now.UnixMilli()-last > time.Minute.Milliseconds() && c.lastPrune.CompareAndSwap(last, now.UnixMilli()) {
		if _, err := c.db.ExecContext(ctx, `DELETE FROM cluster_messages WHERE created_at < ?`, now.Add(-largeKeep).UnixMilli()); err != nil {
			c.log.Warn("cluster: deleting old large messages failed", "err", err.Error())
		}
	}
	body, err := json.Marshal(wireMsg{Instance: c.id, Topic: topic, Ref: id})
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `SELECT pg_notify(?, ?)`, channel, string(body))
	return err
}

// Subscribe calls fn with the payload of every message published on topic,
// on any node, and with Resync (see there). fn runs on the cluster's one
// delivery goroutine, in order: it must return quickly (start a goroutine
// for slow work) and must not wait for another delivery. The returned
// cancel stops further calls; calling it more than once is safe. A nil
// Cluster never calls fn.
func (c *Cluster) Subscribe(topic string, fn func(payload string)) (cancel func()) {
	if c == nil || fn == nil {
		return func() {}
	}
	c.subMu.Lock()
	c.nextSub++
	id := c.nextSub
	m := c.subs[topic]
	if m == nil {
		m = map[uint64]func(string){}
		c.subs[topic] = m
	}
	m[id] = fn
	c.subMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.subMu.Lock()
			defer c.subMu.Unlock()
			if m := c.subs[topic]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(c.subs, topic)
				}
			}
		})
	}
}

// enqueue queues one delivery without blocking. A full queue drops it and
// asks for a Resync once the queue drains.
func (c *Cluster) enqueue(d delivery) {
	select {
	case c.queue <- d:
	default:
		if !c.dropped.Swap(true) {
			c.log.Warn("cluster: delivery queue full; messages dropped, subscribers will resync")
		}
	}
}

// dispatch delivers queued messages in order.
func (c *Cluster) dispatch() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case d := <-c.queue:
			c.deliver(d)
			if len(c.queue) == 0 && c.dropped.Swap(false) {
				c.deliver(delivery{resync: true})
			}
		}
	}
}

// deliver calls the subscribers of one delivery.
func (c *Cluster) deliver(d delivery) {
	var fns []func(string)
	c.subMu.RLock()
	if d.resync {
		for _, m := range c.subs {
			for _, fn := range m {
				fns = append(fns, fn)
			}
		}
	} else {
		for _, fn := range c.subs[d.topic] {
			fns = append(fns, fn)
		}
	}
	c.subMu.RUnlock()
	if len(fns) == 0 {
		return
	}
	payload := d.payload
	if d.resync {
		payload = Resync
	}
	if d.ref != "" {
		ctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
		err := c.db.QueryRowContext(ctx, `SELECT payload FROM cluster_messages WHERE id = ?`, d.ref).Scan(&payload)
		cancel()
		if err != nil {
			if c.ctx.Err() == nil {
				c.log.Warn("cluster: reading a large message failed; it is dropped", "topic", d.topic, "err", err.Error())
			}
			return
		}
	}
	for _, fn := range fns {
		c.call(d.topic, fn, payload)
	}
}

// call runs one subscriber; a panic is logged, not fatal.
func (c *Cluster) call(topic string, fn func(string), payload string) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("cluster: subscriber panicked", "topic", topic, "panic", r)
		}
	}()
	fn(payload)
}

// dialListen opens a connection that LISTENs on the cluster channel.
func (c *Cluster) dialListen(ctx context.Context) (*pgx.Conn, error) {
	conn, err := c.db.ConnectPostgres(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `LISTEN `+channel); err != nil {
		closeConn(conn)
		return nil, err
	}
	c.listenMu.Lock()
	c.listenConn = conn
	c.listenMu.Unlock()
	return conn, nil
}

// listen receives NOTIFY messages until the helper stops, reconnecting
// with backoff after a failure. Every connection after the first that New
// opened is followed by a Resync: messages sent meanwhile were missed.
func (c *Cluster) listen(conn *pgx.Conn) {
	defer c.wg.Done()
	defer func() {
		if conn != nil {
			closeConn(conn)
		}
	}()
	backoff := time.Second
	var lg logOnce
	for c.ctx.Err() == nil {
		if conn == nil {
			dctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
			var err error
			conn, err = c.dialListen(dctx)
			cancel()
			if err != nil {
				if c.ctx.Err() != nil {
					return
				}
				lg.warn(c.log, "cluster: cannot open the message connection; retrying", err)
				if !sleepCtx(c.ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			if lg.reset() {
				c.log.Info("cluster: message connection restored")
			}
			backoff = time.Second
			c.enqueue(delivery{resync: true})
		}
		n, err := conn.WaitForNotification(c.ctx)
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			lg.warn(c.log, "cluster: the message connection failed; reconnecting", err)
			closeConn(conn)
			conn = nil
			continue
		}
		c.receive(n.Payload)
	}
}

// receive queues one NOTIFY payload. Our own messages were delivered when
// they were published.
func (c *Cluster) receive(raw string) {
	var m wireMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.Topic == "" {
		return
	}
	if m.Instance == c.id {
		return
	}
	c.enqueue(delivery{topic: m.Topic, payload: m.Payload, ref: m.Ref})
}

// resyncLoop sends every subscriber a Resync each ResyncInterval.
func (c *Cluster) resyncLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.opts.ResyncInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.enqueue(delivery{resync: true})
		}
	}
}

// ---- named locks --------------------------------------------------------------

// processLocks is the in-process lock table of a nil Cluster.
var processLocks keyedMutex

// Lock takes the named lock across every node and returns the function
// that releases it (safe to call more than once). It waits until the lock
// is free or ctx ends. The lock is held in process and, on Postgres, as a
// session advisory lock on the cluster's own lease connection, so it holds
// no connection of the pool however long the section runs (a section may
// use the pool freely). Waiting polls pg_try_advisory_lock, at most every
// 100 ms. Should that connection fail while the lock is held, the server
// releases the lock with it: keep each section idempotent or checked in
// the database, as the in-process mutexes it replaces had to be across a
// restart. Use LockTx instead when the section is one transaction.
func (c *Cluster) Lock(ctx context.Context, name string) (unlock func(), err error) {
	locks := &processLocks
	if c != nil {
		locks = &c.locks
	}
	unlockLocal, err := locks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	if !c.MultiNode() {
		return unlockLocal, nil
	}
	key := LockKey("lock:" + name)
	backoff := 5 * time.Millisecond
	for {
		gen, got, err := c.tryNamed(key)
		if err == nil && got {
			var once sync.Once
			return func() {
				once.Do(func() {
					c.unlockNamed(key, gen)
					unlockLocal()
				})
			}, nil
		}
		if errors.Is(err, errClosed) {
			unlockLocal()
			return nil, err
		}
		// Taken elsewhere, or the connection failed (it is opened again on
		// the next try): wait and try again.
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			unlockLocal()
			return nil, ctx.Err()
		case <-t.C:
		}
		backoff = min(backoff*2, 100*time.Millisecond)
	}
}

// errClosed is the error of a lock asked of a closed Cluster.
var errClosed = errors.New("cluster: closed")

// tryNamed tries once to take the session advisory lock key on the lease
// connection; gen names that connection.
func (c *Cluster) tryNamed(key int64) (gen uint64, got bool, err error) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return 0, false, errClosed
	}
	conn, err := c.lockConnLocked()
	if err != nil {
		return 0, false, err
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	if err := conn.QueryRow(qctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		c.dropLockConnLocked("take a lock", err)
		return 0, false, err
	}
	return c.lockGen, got, nil
}

// unlockNamed releases a lock tryNamed took on connection gen. A lock lost
// with its connection is already gone.
func (c *Cluster) unlockNamed(key int64, gen uint64) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.lockConn == nil || gen != c.lockGen || c.ctx.Err() != nil {
		return
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	var ok bool
	if err := c.lockConn.QueryRow(qctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&ok); err != nil {
		c.dropLockConnLocked("release a lock", err)
	}
}

// LockTx takes the named lock for the rest of tx: pg_advisory_xact_lock on
// Postgres, released at commit or rollback. On SQLite it does nothing: the
// database has one connection, so a transaction already excludes every
// other writer.
func LockTx(ctx context.Context, tx store.Querier, d store.Dialect, name string) error {
	if !d.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, LockKey("lock:"+name))
	return err
}

// LockKey maps a lock name to the 64-bit key of a Postgres advisory lock.
func LockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("toolyard:" + name))
	return int64(h.Sum64())
}

// keyedMutex is a table of in-process locks by name whose waits end with
// their context.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	ch   chan struct{}
	refs int
}

func (k *keyedMutex) lock(ctx context.Context, name string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[name]
	if e == nil {
		e = &keyedEntry{ch: make(chan struct{}, 1)}
		k.m[name] = e
	}
	e.refs++
	k.mu.Unlock()
	select {
	case e.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-e.ch
				k.put(name, e)
			})
		}, nil
	case <-ctx.Done():
		k.put(name, e)
		return nil, ctx.Err()
	}
}

func (k *keyedMutex) put(name string, e *keyedEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.m, name)
	}
}

// ---- work leases ------------------------------------------------------------------

// LeaseUntil is the end, in Unix milliseconds, of a work lease taken or
// renewed at now.
func LeaseUntil(now time.Time) int64 { return now.Add(WorkLease).UnixMilli() }

// RenewLeases calls renew every WorkLeaseRenew until ctx ends, logging a
// failure under name. Every node runs one for each kind of work it
// executes, outside RunLeader: each renews only its own rows.
func RenewLeases(ctx context.Context, name string, renew func(ctx context.Context) error) {
	t := time.NewTicker(WorkLeaseRenew)
	defer t.Stop()
	var lg logOnce
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := renew(ctx); err != nil {
			if ctx.Err() == nil {
				lg.warn(logx.For("cluster").With("work", name), "cluster: renewing work leases failed; another node may recover this work once the leases end", err)
			}
			continue
		}
		lg.reset()
	}
}

// ---- helpers --------------------------------------------------------------------

func checkTopic(topic string) error {
	if topic == "" || len(topic) > MaxTopic || strings.IndexByte(topic, 0) >= 0 {
		return ErrBadTopic
	}
	return nil
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
	processIDOnce.Do(func() { processIDVal = newInstanceID() })
	return processIDVal
}

// newInstanceID returns <hostname>-<8 random hex digits>.
func newInstanceID() string {
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		}
		return -1
	}, host)
	if len(host) > 63 {
		host = host[:63]
	}
	if host == "" {
		host = "toolyard"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
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

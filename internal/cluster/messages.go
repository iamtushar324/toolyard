package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// delivery is one queued message, or a Resync. A message stored by
// PublishLarge on another node arrives as ref, the cluster_messages row to
// read. A Resync with no topic goes to every subscriber, one with topics
// only to theirs.
type delivery struct {
	topic   string
	payload string
	ref     string
	resync  bool
	topics  []string
}

// wireMsg is a message as NOTIFY carries it: the payload, or the
// cluster_messages row that holds it (Ref).
type wireMsg struct {
	Instance string `json:"n"`
	Topic    string `json:"t"`
	Payload  string `json:"p,omitempty"`
	Ref      string `json:"r,omitempty"`
}

// Publish sends a small message (payload ≤ 1 KiB) to every node, this one
// included (LISTEN/NOTIFY on Postgres). Subscribe delivers it. Subscribers
// also get a periodic "resync" call so a missed message heals within the
// poll interval. The local delivery is queued before the NOTIFY is sent, so
// it happens even when the database is unreachable (the error is still
// returned). A nil Cluster delivers nothing.
func (c *Cluster) Publish(ctx context.Context, topic, payload string) error {
	if err := checkMessage(topic, payload, MaxPayload); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	c.enqueue(delivery{topic: topic, payload: payload})
	return c.notify(ctx, topic, payload)
}

// PublishLarge is Publish for a payload of up to 1 MiB. A payload over
// MaxPayload is stored in the cluster_messages table for a few minutes and
// the NOTIFY names its row; each other node reads it before it calls its
// subscribers. This node's subscribers get the payload at once.
func (c *Cluster) PublishLarge(ctx context.Context, topic, payload string) error {
	if err := checkMessage(topic, payload, MaxLargePayload); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	c.enqueue(delivery{topic: topic, payload: payload})
	return c.notify(ctx, topic, payload)
}

// PublishOthers is PublishLarge without the local delivery: only the other
// nodes' subscribers get the message (a node that already acted on its own
// event, such as a live dashboard event, needs no echo). On a single node
// it does nothing.
func (c *Cluster) PublishOthers(ctx context.Context, topic, payload string) error {
	if err := checkMessage(topic, payload, MaxLargePayload); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	return c.notify(ctx, topic, payload)
}

// notify sends one message to the other nodes, through cluster_messages
// when it is larger than a NOTIFY carries.
func (c *Cluster) notify(ctx context.Context, topic, payload string) error {
	if !c.pg {
		return nil
	}
	m := wireMsg{Instance: c.id, Topic: topic, Payload: payload}
	if len(payload) > MaxPayload {
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
		m = wireMsg{Instance: c.id, Topic: topic, Ref: id}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `SELECT pg_notify(?, ?)`, channel, string(body))
	return err
}

// Subscribe calls fn with the payload of every message published on topic,
// on any node, and with Resync (see there). fn runs on the cluster's one
// delivery goroutine, in order: it must return quickly (start a goroutine,
// or queue, for slow work) and must not wait for another delivery. The
// returned cancel stops further calls; calling it more than once is safe.
// A nil Cluster never calls fn.
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
// remembers its topic (or everyone, for a dropped Resync), which then gets
// a Resync once the queue drains.
func (c *Cluster) enqueue(d delivery) {
	select {
	case c.queue <- d:
		return
	default:
	}
	c.dropMu.Lock()
	first := !c.dropAll && len(c.dropTopic) == 0
	switch {
	case d.resync && len(d.topics) == 0:
		c.dropAll = true
	case d.resync:
		for _, t := range d.topics {
			c.dropTopic[t] = true
		}
	default:
		c.dropTopic[d.topic] = true
	}
	c.dropMu.Unlock()
	if first {
		c.log.Warn("cluster: delivery queue full; messages dropped, their subscribers will resync")
	}
}

// takeDrops returns the Resync that the dropped messages call for, if any.
func (c *Cluster) takeDrops() (delivery, bool) {
	c.dropMu.Lock()
	defer c.dropMu.Unlock()
	if !c.dropAll && len(c.dropTopic) == 0 {
		return delivery{}, false
	}
	d := delivery{resync: true}
	if !c.dropAll {
		for t := range c.dropTopic {
			d.topics = append(d.topics, t)
		}
	}
	c.dropAll = false
	c.dropTopic = map[string]bool{}
	return d, true
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
			if len(c.queue) == 0 {
				if r, ok := c.takeDrops(); ok {
					c.deliver(r)
				}
			}
		}
	}
}

// deliver calls the subscribers of one delivery.
func (c *Cluster) deliver(d delivery) {
	var fns []func(string)
	var topics []string
	c.subMu.RLock()
	switch {
	case d.resync && len(d.topics) == 0:
		for t, m := range c.subs {
			for _, fn := range m {
				fns = append(fns, fn)
				topics = append(topics, t)
			}
		}
	case d.resync:
		for _, t := range d.topics {
			for _, fn := range c.subs[t] {
				fns = append(fns, fn)
				topics = append(topics, t)
			}
		}
	default:
		for _, fn := range c.subs[d.topic] {
			fns = append(fns, fn)
			topics = append(topics, d.topic)
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
	for i, fn := range fns {
		c.call(topics[i], fn, payload)
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
	conn, err := c.db.ConnectPostgres(ctx, "toolyard-cluster-listen")
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
// with backoff after a failure. A connection silent for listenPing is
// pinged, so a dead socket is noticed even when nothing is published.
// Every connection after the first that New opened is followed by a
// Resync: messages sent meanwhile were missed.
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
		wctx, cancel := context.WithTimeout(c.ctx, listenPing)
		n, err := conn.WaitForNotification(wctx)
		silent := errors.Is(wctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			if silent && !conn.IsClosed() {
				// Quiet, not failed: check that the session still answers.
				pctx, pcancel := context.WithTimeout(c.ctx, 5*time.Second)
				err = conn.Ping(pctx)
				pcancel()
				if err == nil {
					continue
				}
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
// they were published (or were meant for the others only).
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

// checkMessage validates a topic and a payload of at most limit bytes.
func checkMessage(topic, payload string, limit int) error {
	if topic == "" || len(topic) > MaxTopic || strings.IndexByte(topic, 0) >= 0 {
		return ErrBadTopic
	}
	if len(payload) > limit {
		if limit == MaxPayload {
			return ErrPayloadTooLarge
		}
		return errors.New("cluster: payload larger than 1 MiB")
	}
	if strings.IndexByte(payload, 0) >= 0 {
		return errors.New("cluster: a payload holds no NUL byte")
	}
	return nil
}

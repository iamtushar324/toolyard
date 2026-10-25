package upstreams

// High availability (TEC-1630): each replica holds its own connections to
// the upstream servers (the gateway's live pool), built from the shared
// upstream_servers table. A change saved on one replica (add, edit,
// remove, reconnect, a sign-in) is published on the cluster, and every
// other replica reconciles that server with the stored row. The periodic
// cluster resync compares every row with what this replica last connected
// (Service.applied) and heals a missed message. Built-in servers (notes,
// skills, mempalace) are per-replica: each replica attaches its own at
// startup, so they are never reconciled.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// clusterTopic carries upstreamMsg.
const clusterTopic = "upstreams"

// Operations an upstreamMsg asks the other replicas to apply.
const (
	// opSync: reconcile the server with its row (connect, reconnect when
	// the configuration changed, or take it down).
	opSync = "sync"
	// opReconnect: reconnect the server even when its configuration is
	// unchanged (a new shared token, an operator's reconnect).
	opReconnect = "reconnect"
	// opUser: a person signed in to a per_user server.
	opUser = "user"
	// opDropUser: close one person's connection to a per_user server.
	opDropUser = "dropuser"
)

// upstreamMsg is one change, published by the replica that saved it.
type upstreamMsg struct {
	Instance string `json:"n"`
	Op       string `json:"op"`
	Name     string `json:"s"`
	User     string `json:"p,omitempty"`
}

// remoteSync is the service's link to the other replicas: one worker
// applies their changes in order, off the cluster's delivery goroutine
// (a reconnect can take seconds).
type remoteSync struct {
	c       *cluster.Cluster
	mu      sync.Mutex
	pending []upstreamMsg
	resync  bool
	wake    chan struct{}
}

// SetCluster makes the service follow changes saved on other replicas and
// publish its own, until ctx ends. Call it once, at startup, before
// LoadAll. Without a multi-node cluster nothing changes.
func (s *Service) SetCluster(ctx context.Context, c *cluster.Cluster) {
	if !c.MultiNode() {
		return
	}
	r := &remoteSync{c: c, wake: make(chan struct{}, 1)}
	s.remote = r
	s.followAsync(c)
	c.Subscribe(clusterTopic, func(payload string) {
		r.mu.Lock()
		if payload == cluster.Resync {
			r.resync = true
		} else {
			var m upstreamMsg
			if json.Unmarshal([]byte(payload), &m) != nil || m.Instance == c.InstanceID() || m.Name == "" {
				r.mu.Unlock()
				return
			}
			r.pending = append(r.pending, m)
		}
		r.mu.Unlock()
		select {
		case r.wake <- struct{}{}:
		default:
		}
	})
	go s.runRemote(ctx, r)
}

// publish tells the other replicas about a change this replica saved.
func (s *Service) publish(op, name, userID string) {
	r := s.remote
	if r == nil || isReservedBuiltin(name) {
		return
	}
	body, err := json.Marshal(upstreamMsg{Instance: r.c.InstanceID(), Op: op, Name: name, User: userID})
	if err == nil {
		err = r.c.Publish(context.Background(), clusterTopic, string(body))
	}
	if err != nil {
		logx.For("upstreams").Warn("upstreams: telling other nodes about a change failed; they catch up at the next resync", "server", name, "err", err.Error())
	}
}

// runRemote applies the other replicas' changes until ctx ends.
func (s *Service) runRemote(ctx context.Context, r *remoteSync) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		r.mu.Lock()
		msgs, resync := r.pending, r.resync
		r.pending, r.resync = nil, false
		r.mu.Unlock()
		for _, m := range msgs {
			if ctx.Err() != nil {
				return
			}
			s.applyRemote(ctx, m)
		}
		if resync && ctx.Err() == nil {
			s.resync(ctx)
		}
	}
}

// applyRemote applies one change another replica saved, on this replica.
func (s *Service) applyRemote(ctx context.Context, m upstreamMsg) {
	if isReservedBuiltin(m.Name) {
		return
	}
	switch m.Op {
	case opDropUser:
		if m.User != "" {
			s.gw.DropUserConnection(m.Name, m.User)
		}
		return
	case opUser:
		s.reconnectAfterUserAuth(ctx, m.Name)
		return
	}
	srv, err := s.get(ctx, m.Name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		logx.For("upstreams").Warn("upstreams: reading a server another node changed failed; the next resync tries again", "server", m.Name, "err", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if srv == nil || !srv.Enabled || s.stdioRefused(*srv) {
		s.takeDownLocked(m.Name)
		return
	}
	if m.Op != opReconnect && s.applied[m.Name] == configFingerprint(*srv) {
		return
	}
	_ = s.gw.RemoveUpstream(m.Name)
	if err := s.connectWith(ctx, *srv, false); err != nil {
		logx.For("upstreams").Warn("upstreams: connecting a server another node changed failed on this node", "server", m.Name, "err", err.Error())
	}
}

// resync compares every stored server with what this replica last
// connected: it connects servers it has not tried with their current
// configuration and takes down servers that are gone or disabled. A
// server whose connection failed with an unchanged configuration is left
// as it is (an operator reconnect retries it), as on a single node.
func (s *Service) resync(ctx context.Context) {
	servers, err := s.list(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := map[string]bool{}
	for _, srv := range servers {
		if isReservedBuiltin(srv.Name) {
			continue
		}
		stored[srv.Name] = true
		cur, have := s.applied[srv.Name]
		switch want := srv.Enabled && !s.stdioRefused(srv); {
		case !want && have:
			s.takeDownLocked(srv.Name)
		case want && cur != configFingerprint(srv):
			if ctx.Err() != nil {
				return
			}
			_ = s.gw.RemoveUpstream(srv.Name)
			if err := s.connectWith(ctx, srv, false); err != nil {
				logx.For("upstreams").Warn("upstreams: connecting a changed server failed on this node", "server", srv.Name, "err", err.Error())
			}
		}
	}
	for name := range s.applied {
		if !stored[name] && !isReservedBuiltin(name) {
			s.takeDownLocked(name)
		}
	}
}

// lockServer serializes the read-modify-write edits of one server (Update,
// ConvertEnvToSecret) across replicas; on a single node s.mu does.
func (s *Service) lockServer(ctx context.Context, name string) (func(), error) {
	if s.remote == nil {
		return func() {}, nil
	}
	return s.remote.c.Lock(ctx, "upstream:"+name)
}

// takeDownLocked disconnects a server this replica runs. s.mu is held.
func (s *Service) takeDownLocked(name string) {
	if _, ok := s.applied[name]; !ok {
		if err := s.gw.RemoveUpstream(name); errors.Is(err, gateway.ErrUpstreamNotFound) {
			return
		}
	} else {
		_ = s.gw.RemoveUpstream(name)
		delete(s.applied, name)
	}
	s.gw.NotifyToolListChanged()
}

// configFingerprint identifies the configuration a server is connected
// with: everything toCfg reads from the row except its status. An empty
// list or map counts as none, so a row read back matches what was saved.
func configFingerprint(srv Server) string {
	var args []string
	var env, headers map[string]string
	if len(srv.Args) > 0 {
		args = srv.Args
	}
	if len(srv.Env) > 0 {
		env = srv.Env
	}
	if len(srv.Headers) > 0 {
		headers = srv.Headers
	}
	b, _ := json.Marshal([]any{srv.Transport, srv.Command, args, srv.URL, env, headers, srv.Identity, normalizeAuthMode(srv.AuthMode)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

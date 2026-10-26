package skillhub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// Topic is the cluster topic an activation (or any change to what a source
// serves) is published on; the payload is the source ID.
const Topic = "skillhub"

// Goroutine names (metric labels, see internal/goroutines).
const (
	goReload    = "skillhub-reload"
	goUsage     = "skillhub-usage-flush"
	goSyncAsync = "skillhub-sync"
	goPoll      = "skillhub-poll"
)

const (
	// pollTick is how often the leader looks for sources whose check is
	// due.
	pollTick = 15 * time.Second
	// maxBackoff caps the wait after failed syncs.
	maxBackoff = time.Hour
	// syncTimeout bounds one sync (a full download included).
	syncTimeout = 10 * time.Minute
	// usageFlushEvery is how often a node writes its buffered read counts.
	usageFlushEvery = 30 * time.Second
	// refreshEvery is how often a node compares its index with the
	// database, on top of the cluster messages and resyncs.
	refreshEvery = 5 * time.Minute
	// pendingPoll is the shortest check interval of a contributor's source
	// that waits for an admin's approval.
	pendingPoll = time.Hour
	// blobGrace keeps a blob a sync touched this recently out of the
	// unreferenced-blob purge, so a version being written never loses one.
	blobGrace = time.Hour
	// purgeEvery is how often the leader purges unreferenced blobs.
	purgeEvery = time.Hour
)

// Settings reads system settings (internal/settings.Service).
type Settings interface {
	GetString(key, fallback string) string
}

// Options configure a Service.
type Options struct {
	// DB is the toolyard database (required).
	DB *store.DB
	// Cluster carries activations to the other replicas and holds the
	// per-source sync lock. Nil works as a single node.
	Cluster *cluster.Cluster
	// Cipher seals source tokens and webhook secrets (secrets.key). Nil
	// refuses tokens and webhook secrets.
	Cipher *sealbox.Cipher
	// Settings supplies skills_trusted_github_owners. Nil trusts nobody.
	Settings Settings
	// GitHubAPI is the GitHub REST API base URL; default
	// https://api.github.com. Tests point it at a fake server.
	GitHubAPI string
	// HTTPClient makes the GitHub calls; default a client with no overall
	// timeout (each call has its own deadline). Its redirect policy is
	// replaced: toolyard follows only the archive redirect, by hand.
	HTTPClient *http.Client
	// Now is the clock (tests); default time.Now.
	Now func() time.Time
	// Logger; default logx.For("skillhub").
	Logger *slog.Logger
	// BuiltinSkills serves the skills compiled into toolyard as the
	// read-only source "toolyard" (builtin.go). The gateway turns it on;
	// tests leave it off unless they test it.
	BuiltinSkills bool
}

// Service stores, syncs and serves skill sources. It implements Reader and
// Admin. Create it with New, then call Start on every node and run
// RunPoller under a cluster leader lease.
type Service struct {
	db       *store.DB
	cluster  *cluster.Cluster
	cipher   *sealbox.Cipher
	settings Settings
	apiBase  *url.URL
	hc       *http.Client
	now      func() time.Time
	log      *slog.Logger
	search   *toolsearch.Engine
	limits   *rateLimits

	hookMu sync.RWMutex
	review ReviewHook
	tokens TokenSource

	idx   *index
	usage usageBuffer

	// runCtx is the context Start got: background syncs (webhooks) run
	// under it, so they stop with the gateway.
	runMu  sync.Mutex
	runCtx context.Context
	bg     sync.WaitGroup

	// queued holds each source's background sync (see requestSync).
	qmu    sync.Mutex
	queued map[string]*pendingSync

	// wake pokes the poller after a source was created or changed.
	wake chan struct{}
	// lastPublicPause is the rate-limit pause the poller last logged for
	// the sources without their own token (only the poller uses it).
	lastPublicPause time.Time
	// badTokenMu guards badToken, the sealed default token that could not
	// be opened and was logged (once, not at every check).
	badTokenMu sync.Mutex
	badToken   string
	// reloads queues the sources the cluster asked this node to reload.
	reloads reloadQueue
	// bi is the built-in source (builtin.go).
	bi builtinState
}

var (
	_ Reader = (*Service)(nil)
	_ Admin  = (*Service)(nil)
)

// New returns a Service. It starts nothing: call Start.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("skillhub: a database is required")
	}
	base := strings.TrimSpace(opts.GitHubAPI)
	if base == "" {
		base = defaultGitHubAPI
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("skillhub: GitHub API base %q is not an http(s) URL", base)
	}
	hc := &http.Client{}
	if opts.HTTPClient != nil {
		c := *opts.HTTPClient
		hc = &c
	}
	// Redirects are followed by hand (archive), never automatically: an
	// automatic redirect could carry the token to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	lg := opts.Logger
	if lg == nil {
		lg = logx.For("skillhub")
	}
	s := &Service{
		db:       opts.DB,
		cluster:  opts.Cluster,
		cipher:   opts.Cipher,
		settings: opts.Settings,
		apiBase:  u,
		hc:       hc,
		now:      now,
		log:      lg,
		search:   toolsearch.New(toolsearch.Options{}),
		limits:   &rateLimits{until: map[string]time.Time{}, now: now},
		idx:      newIndex(),
		runCtx:   context.Background(),
		queued:   map[string]*pendingSync{},
		wake:     make(chan struct{}, 1),
	}
	s.bi.on = opts.BuiltinSkills
	s.usage.counts = map[usageKey]int64{}
	s.reloads.ids = map[string]bool{}
	s.reloads.wake = make(chan struct{}, 1)
	return s, nil
}

// Start loads the index and starts this node's own work: reloading
// sources the cluster names, a periodic comparison with the database, the
// read-count flush and, with Options.BuiltinSkills, the built-in source
// (installed before the first load; see builtin.go). Everything stops when ctx ends. The returned error
// is the first load's; the node then serves an empty catalog until the
// next refresh succeeds.
func (s *Service) Start(ctx context.Context) error {
	s.runMu.Lock()
	s.runCtx = ctx
	s.runMu.Unlock()
	cancelSub := s.cluster.Subscribe(Topic, func(payload string) {
		if payload == cluster.Resync {
			s.reloads.markAll()
			return
		}
		s.reloads.mark(payload)
	})
	goroutines.Go(goReload, func() {
		defer cancelSub()
		s.runReloads(ctx)
	})
	goroutines.Go(goUsage, func() { s.runUsageFlush(ctx) })
	s.startBuiltin(ctx)
	return s.refreshAll(ctx)
}

// baseCtx is the context background syncs run under.
func (s *Service) baseCtx() context.Context {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runCtx
}

// Wait blocks until the background syncs this node started have finished
// (tests, shutdown).
func (s *Service) Wait() { s.bg.Wait() }

// SetReviewHook sets who hears about staged versions of review-mode
// sources.
func (s *Service) SetReviewHook(h ReviewHook) {
	s.hookMu.Lock()
	s.review = h
	s.hookMu.Unlock()
}

// SetTokenSource lets AuthGitHub sources read with their creator's GitHub
// connection.
func (s *Service) SetTokenSource(ts TokenSource) {
	s.hookMu.Lock()
	s.tokens = ts
	s.hookMu.Unlock()
}

func (s *Service) reviewHook() ReviewHook {
	s.hookMu.RLock()
	defer s.hookMu.RUnlock()
	return s.review
}

func (s *Service) tokenSource() TokenSource {
	s.hookMu.RLock()
	defer s.hookMu.RUnlock()
	return s.tokens
}

// TrustedOwner reports whether owner is in skills_trusted_github_owners
// (comma list, case-insensitive).
func (s *Service) TrustedOwner(_ context.Context, owner string) bool {
	if s.settings == nil || strings.TrimSpace(owner) == "" {
		return false
	}
	for _, o := range strings.Split(s.settings.GetString(settings.SkillsTrustedGitHubOwners, ""), ",") {
		if o = strings.TrimSpace(o); o != "" && strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

// pokePoller wakes this node's poller, if it runs here.
func (s *Service) pokePoller() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// publish tells every node (this one included) that a source changed.
func (s *Service) publish(ctx context.Context, sourceID string) {
	if err := s.cluster.Publish(context.WithoutCancel(ctx), Topic, sourceID); err != nil {
		s.log.Warn("skillhub: telling other nodes about a change failed; they catch up at the next resync",
			"source", sourceID, "err", err.Error())
	}
}

// changed reloads a source on this node and publishes the change.
func (s *Service) changed(ctx context.Context, sourceID string) {
	if err := s.reloadSource(context.WithoutCancel(ctx), sourceID); err != nil {
		s.log.Warn("skillhub: reload after a change failed", "source", sourceID, "err", err.Error())
	}
	s.publish(ctx, sourceID)
}

// newID returns prefix and 24 hex characters.
func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// digest is "sha256:<64 lowercase hex>" of b.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// placeholders is n comma-separated ?.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func msPtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.UnixMilli(*v).UTC()
	return &t
}

// boolInt is 1 or 0: flags are BIGINT on Postgres.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

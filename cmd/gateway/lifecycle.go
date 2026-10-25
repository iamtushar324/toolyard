package main

// TEC-1626 drain: the gateway's one ordered shutdown, and the names of its
// supervised background loops.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/ops"
	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

// Supervised background loops (goroutines.Loop / goroutines.Go). The names
// are a fixed set because each one is a metric label. approval.SweeperLoop,
// inbox.SweeperLoop and inbox.LegacySyncLoop live with their loops, as do
// the lease renewal loops (inbox, approval and coderuns LeaseRenewalLoop).
// With several replicas on one Postgres database (TEC-1630) a loop that
// must run on one replica at a time runs under a cluster leader lease of
// the same name (cluster.go).
const (
	loopAuditFeed      = "audit-feed"
	loopPricing        = "pricing-refresh"
	loopAutoApproval   = "auto-approval-proposer"
	loopSessionPurger  = "session-purger"
	loopAnomaly        = "anomaly-detector"
	loopReasonScorer   = "reason-scorer"
	loopRetention      = "retention-compactor"
	loopOAuthRefresher = "oauth-refresher"
	loopInboxAttention = "inbox-attention"
	loopCallbacks      = "callback-delivery"
	loopNotesScanner   = "notes-scanner"
	loopSkillsSync     = "skills-sync" // skillhub (slice A)
	loopIdleSweeper    = "upstream-idle-sweeper"
	loopCatalogRefresh = "upstream-catalog-refresh"
	goCodemodeProbe    = "codemode-probe"
	// TEC-1630.
	loopWorkRecovery   = "work-recovery"
	loopClerkSync      = "clerk-sync"
	loopKeyRemoval     = "identity-key-removal-retry"
	loopTelegramPoller = "telegram-poller"
	loopChatReconciler = "chat-reconciler"
	loopEventsPoller   = "events-poller"
	loopEventsRetain   = "events-retention"
	loopMemWebhookJobs = "memwebhook-jobs"
)

// Shutdown budget. With the defaults a stop takes at most drainTimeout
// (60s, the drain delay included) + shutdownGrace (2s) + one closeTimeout
// per closer (4 x 2s): 70s, inside the units' TimeoutStopSec=75s.
const (
	defaultDrainDelay   = 3 * time.Second
	defaultDrainTimeout = 60 * time.Second
	// shutdownGrace is how long work cancelled at the drain deadline gets
	// to record its outcome.
	shutdownGrace = 2 * time.Second
	// closeTimeout bounds each close after the drain.
	closeTimeout = 2 * time.Second
)

// shutdownPlan is the ordered shutdown. run performs the numbered steps in
// order; every hook is optional.
type shutdownPlan struct {
	log          *slog.Logger
	drainDelay   time.Duration
	drainTimeout time.Duration // from the start of the drain, delay included
	grace        time.Duration

	// 1. Readiness turns 503.
	markDraining func()
	// 2. New tool calls and executions are refused with a retryable
	// answer, before any grant is redeemed or execution claimed.
	refuseNew func()
	// 3. drainDelay passes, so the load balancer sees not-ready.
	// 4. Streams close and waits end with a retryable answer.
	releaseWaiters func()
	// 5. The HTTP server stops by the drain deadline; forceHTTP closes
	// the connections still open then.
	stopHTTP  func(context.Context) error
	forceHTTP func() error
	// 6. In-flight executions finish. Each wait gets a context that ends
	// at the drain deadline and handles the work still running then; it
	// may use up to grace more for that work to record its outcome. The
	// waits run side by side.
	waitExecutions []func(context.Context)
	// 7. Background loops stop.
	cancelRoot func()
	// 8. Resources close in order, each bounded (closeOnce).
	closers []shutdownCloser
}

// shutdownCloser is one step-8 close.
type shutdownCloser struct {
	name  string
	close func() error
}

// run performs the shutdown. It returns within drainTimeout + grace + the
// closers' own bounds, whatever the hooks do.
func (p *shutdownPlan) run() {
	lg := p.log
	if lg == nil {
		lg = logx.For("shutdown")
	}
	timeout := p.drainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	start := time.Now()
	deadline := start.Add(timeout)
	lg.Info("shutdown: drain started",
		"drain_delay", p.drainDelay.String(), "drain_timeout", timeout.String())

	// 1-2.
	call(p.markDraining)
	call(p.refuseNew)

	// 3.
	if delay := min(p.drainDelay, timeout); delay > 0 {
		time.Sleep(delay)
	}

	// 4.
	call(p.releaseWaiters)

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// 5.
	if p.stopHTTP != nil {
		if err := boundedCall(ctx, p.stopHTTP); err != nil {
			lg.Error("shutdown: HTTP requests still open at the drain deadline; closing their connections", "err", err.Error())
			if p.forceHTTP != nil {
				if err := p.forceHTTP(); err != nil {
					lg.Warn("shutdown: closing HTTP connections failed", "err", err.Error())
				}
			}
		}
	}

	// 6.
	if len(p.waitExecutions) > 0 {
		bound, cancelBound := context.WithDeadline(context.Background(), deadline.Add(p.grace))
		var wg sync.WaitGroup
		for _, wait := range p.waitExecutions {
			if wait == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := boundedCall(bound, func(context.Context) error { wait(ctx); return nil }); err != nil {
					lg.Error("shutdown: an execution wait did not end in time", "err", err.Error())
				}
			}()
		}
		wg.Wait()
		cancelBound()
	}

	// 7.
	call(p.cancelRoot)

	// 8.
	for _, c := range p.closers {
		if c.close == nil {
			continue
		}
		if err := c.close(); err != nil {
			lg.Warn("shutdown: close failed", "resource", c.name, "err", err.Error())
		}
	}
	lg.Info("shutdown: done", "elapsed", time.Since(start).Round(time.Millisecond).String())
}

func call(fn func()) {
	if fn != nil {
		fn()
	}
}

// boundedCall runs fn with ctx and returns by the time ctx ends even if fn
// ignores it (fn then finishes on its own, or with the process). A panic
// in fn becomes an error.
func boundedCall(ctx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeOnce wraps a close so it runs at most once, bounded by timeout: the
// ordered shutdown calls it, and the deferred call that covers an early
// return during startup is then a no-op. A close that ignores its context
// is abandoned at the timeout (the process is exiting).
func closeOnce(name string, timeout time.Duration, fn func(context.Context) error) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err = boundedCall(ctx, fn); err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
		})
		return err
	}
}

// mcpStreamGate ends long-lived MCP streams at shutdown: a GET (the
// 2025-11-25 server-to-client notification stream, stateful mode) and a
// 2026-07-28 subscriptions/listen POST. mcp-go holds either one open until
// its request context ends, and http.Server.Shutdown never cancels a
// request context, so one open stream would hold every stop for the full
// -drain-timeout. Other POSTs (the tool calls) pass straight through.
type mcpStreamGate struct {
	refusing atomic.Bool
	// streams is the parent of every GET stream's context; release
	// cancels it.
	streams context.Context
	cancel  context.CancelFunc
}

func newMCPStreamGate() *mcpStreamGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &mcpStreamGate{streams: ctx, cancel: cancel}
}

// wrap applies the gate to the /mcp handler.
func (g *mcpStreamGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMCPStream(r) {
			next.ServeHTTP(w, r)
			return
		}
		if g.refusing.Load() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "server restarting, reconnect", http.StatusServiceUnavailable)
			return
		}
		// The stream ends when its client goes or when release runs,
		// whichever comes first (AfterFunc also fires at once if release
		// already ran).
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(g.streams, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isMCPStream reports whether r opens a long-lived MCP stream. A
// subscriptions/listen request is told by its Mcp-Method header alone:
// mcp-go refuses a 2026-07-28 request whose Mcp-Method header is missing
// or differs from the method in its body, and the method does not exist
// in older protocol versions.
func isMCPStream(r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	return r.Method == http.MethodPost &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Mcp-Method")), "subscriptions/listen")
}

// refuse answers new streams with 503 and Retry-After (drain step 2).
func (g *mcpStreamGate) refuse() { g.refusing.Store(true) }

// release ends every open stream (drain step 4). New ones stay refused.
func (g *mcpStreamGate) release() {
	g.refusing.Store(true)
	g.cancel()
}

// loopDone records that one iteration of a supervised loop completed.
func loopDone(name string) { opsmetrics.SetLoopLastSuccess(name, time.Now()) }

// markStarted records that startup recovery and upstream loading are done
// (readiness needs it).
func markStarted() { ops.Default.MarkStarted() }

// markDraining turns readiness 503 for the rest of the process.
func markDraining() { ops.Default.SetDraining() }

// waitGrantedCalls waits for granted (Inbox) executions until ctx ends at
// the drain deadline. Those still running then are logged at ERROR by
// grant id and counted outcome_unknown; step 5 closed their HTTP
// connections, which cancels them, and they get grace to record the
// outcome. Any that do not are recorded outcome_unknown by recovery
// (inbox.RecoverExecutions): at the next start on a single node, or by the
// leader of a multi-node database once their lease ends.
func waitGrantedCalls(ctx context.Context, gw *gateway.Gateway, svc *inbox.Service, grace time.Duration) {
	left := gw.WaitGrantedCalls(ctx)
	if left == 0 {
		return
	}
	lg := logx.For("shutdown")
	gctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	ids, err := svc.RunningExecutions(gctx)
	if err != nil {
		lg.Error("shutdown: listing running Inbox executions failed", "err", err.Error())
	}
	for _, id := range ids {
		lg.Error("Inbox execution still running at the drain deadline; outcome unknown", "grant_id", id)
		opsmetrics.IncOutcomeUnknown("inbox", "shutdown_deadline")
	}
	if len(ids) == 0 {
		lg.Error("Inbox executions still running at the drain deadline; outcome unknown", "count", left)
		for i := 0; i < left; i++ {
			opsmetrics.IncOutcomeUnknown("inbox", "shutdown_deadline")
		}
	}
	gw.WaitGrantedCalls(gctx)
}

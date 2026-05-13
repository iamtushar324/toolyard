// Command gateway is the toolyard MCP gateway / dashboard binary.
//
// Subcommands:
//
//	toolyard serve      run the gateway (stdio MCP + HTTP API + dashboard)
//	toolyard exchange   exchange an enrollment code for a long-lived agent token
//	toolyard probe      dry-run a tool against an upstream MCP server
//	toolyard version    print version
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	runtimepprof "runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/api"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
	usagepkg "github.com/tusharbhardwaj/toolyard/internal/usage"
	"github.com/tusharbhardwaj/toolyard/internal/visibility"
	dashboard "github.com/tusharbhardwaj/toolyard/web/dashboard"
	weblake "github.com/tusharbhardwaj/toolyard/web/lake"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "exchange":
		err = runExchange(args)
	case "probe":
		err = runProbe(args)
	case "lake":
		err = runLake(args)
	case "version", "-v", "--version":
		fmt.Println("toolyard", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`toolyard ` + version + `

Commands:
  serve       run gateway + HTTP dashboard (default ports: stdio + :8787)
  exchange    swap an enrollment code for an agent token
  probe       dry-run a tool against an upstream MCP server
  lake        manage the personal data lake (backup | tables)
  version     print version

Run 'toolyard <command> -h' for command flags.`)
}

// ---- serve ------------------------------------------------------------------

func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "directory for SQLite + keys")
	addr := fs.String("addr", ":8787", "HTTP listen address")
	stdio := fs.Bool("stdio", false, "also serve MCP over stdio (for direct agent host wiring)")
	upstreamConfig := fs.String("upstreams", "", "path to JSON file with upstream MCP server configs (optional)")
	pushSubject := fs.String("push-subject", "mailto:admin@example.invalid", "VAPID `sub` claim")
	// Default 0: defer immediately. Agents use tools.poll_approval /
	// tools.wait_for_approval to coordinate. Operators with old MCP
	// clients that don't understand the deferred envelope can set this
	// to e.g. 90s to fall back to the legacy block-and-hold behaviour.
	inLineWait := fs.Duration("in-line-wait", 0, "if non-zero, hold an approval-required call open for up to this long waiting for a human decision before returning the deferred-response envelope. The new default 0s returns the envelope immediately and lets the agent poll via tools.poll_approval or block via tools.wait_for_approval.")
	approvalTTL := fs.Duration("approval-ttl", 3*time.Hour, "how long a pending approval stays decidable before auto-expiring")
	publicURL := fs.String("public-url", "", "public origin (e.g. https://toolyard.example.com). When set, enables HSTS, secure cookies, Origin enforcement, and locks /v1/auth/setup to loopback.")
	trustedProxies := fs.String("trusted-proxy", "", "comma-separated CIDRs to trust for X-Forwarded-* headers (e.g. 127.0.0.1/32,::1/128,10.0.0.0/8)")
	grafanaRuntimeEnvPath := fs.String("grafana-runtime-env", "/var/lib/toolyard/grafana-runtime.env", "path where toolyard maintains a TOOLYARD_LAKE_TOKEN=... line for the Grafana container's docker-compose env_file to consume. Updated on bootstrap and on every rotate. Empty disables the write (useful in tests).")
	clickhouseRuntimeEnvPath := fs.String("clickhouse-runtime-env", "/var/lib/toolyard/clickhouse-runtime.env", "path where toolyard maintains a TOOLYARD_CH_PASSWORD=... line for the toolyard-clickhouse container's docker-compose env_file to consume. Updated on bootstrap and on every rotate. Empty disables the write (useful in tests).")
	requireAuthMCP := fs.Bool("require-auth-on-mcp", false, "reject anonymous /mcp calls (no Authorization header). Auto-enabled when -public-url is set.")
	statelessMCP := fs.Bool("stateless-mcp", false, "skip MCP session-ID tracking. Every request stands alone — no server-initiated notifications, but agents that don't auto-reconnect on session-invalid (e.g., hermes) survive a toolyard restart without manual intervention.")
	noStdioUpstreams := fs.Bool("no-stdio-upstreams", false, "refuse to start any stdio (subprocess) MCP upstream. Use when the dashboard is exposed publicly so a compromised session can't spawn arbitrary commands.")
	envDenylistFlag := fs.String("upstream-env-denylist", "LD_PRELOAD,LD_LIBRARY_PATH,DYLD_INSERT_LIBRARIES,DYLD_LIBRARY_PATH,PATH", "comma-separated env var keys forbidden in upstream stdio configs")
	upstreamCallTimeout := fs.Duration("upstream-call-timeout", 120*time.Second, "per-tool-call deadline applied to every dispatch (built-in, fixture, and external upstreams). 0 disables the cap. Without it, a hung upstream pins a goroutine indefinitely and queues every other caller behind it.")
	mempalaceFlag := fs.String("mempalace", "auto", "MemPalace integration mode: on|off|auto. auto=install via `uv tool install mempalace` if missing and proceed; on=fail boot when install fails; off=skip the integration entirely.")
	_ = fs.Parse(argv)

	// Public mode auto-enables matching safeguards.
	if *publicURL != "" {
		*requireAuthMCP = true
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	// Tighten any pre-existing data dir so other local users can't read keys.
	_ = os.Chmod(*dataDir, 0o700)
	dbPath := filepath.Join(*dataDir, "toolyard.db")
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	log.Printf("toolyard: db at %s", dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idSvc := identity.New(db)
	auditSvc := audit.New(db)
	memSvc := memory.New(db)

	// Personal data lake (ClickHouse). Opened below, once settings are
	// loaded — see the `lake: opened …` log line right after the CH
	// password block. Distinct from toolyard.db: the SQLite control
	// plane is OLTP-shaped, the lake is OLAP-shaped. Failure to open
	// the lake is logged but not fatal — the gateway still serves
	// non-lake tools.
	var lakeSvc *lake.Service

	bus, err := approval.New(ctx, db)
	if err != nil {
		return err
	}
	bus.SetTTL(*approvalTTL)
	log.Printf("toolyard: approval TTL %s (in-line wait %s)", bus.TTL(), *inLineWait)
	go bus.RunSweeper(ctx, 30*time.Second)

	pushSvc, err := push.New(ctx, db, *pushSubject)
	if err != nil {
		return err
	}

	hub := realtime.NewHub()

	// Hook approval lifecycle -> push + realtime fan-out.
	bus.AddNotifierFunc(func(ctx context.Context, req *approval.Request, eventType string) {
		hub.Publish(realtime.Event{Type: "approval", Data: req})
		if eventType == "approval.create" {
			user, err := idSvc.PrimaryUser(ctx)
			if err != nil {
				return
			}
			// Don't put the operator's reason text in the push payload.
			// Web Push payloads transit (and may briefly cache on)
			// third-party push services; keeping the body content-free
			// minimizes what a key compromise could reveal. The dashboard
			// fills in details when the user opens the approval card.
			n := push.Notification{
				Title:    "toolyard approval needed",
				Body:     fmt.Sprintf("%s · %s — tap to review", req.UpstreamName, req.ToolName),
				URL:      "/?approval=" + req.ID,
				Approval: req.ID,
				Tag:      req.ID,
			}
			payload := map[string]any{
				"title":          n.Title,
				"body":           n.Body,
				"url":            n.URL,
				"approval_id":    req.ID,
				"decision_token": req.DecisionToken,
				"tag":            req.ID,
			}
			_ = pushSvc.Notify(ctx, user.ID, payload)
		}
	})
	// Audit log -> realtime feed.
	go func() {
		ch, cancel := auditSvc.Subscribe()
		defer cancel()
		for ev := range ch {
			hub.Publish(realtime.Event{Type: "audit", Data: ev})
		}
	}()

	settingsSvc, err := settings.New(ctx, db)
	if err != nil {
		return err
	}

	// ClickHouse password lives in the settings DB (same pattern as
	// lake_api_token): toolyard is the source of truth, the env file
	// the docker stack reads is a rendered view. Auto-mint on first
	// boot so a fresh install + `docker compose up` Just Works. The
	// rendered file is mode 0600 toolyard:toolyard.
	chPass, chGenerated, err := settingsSvc.EnsureClickhousePassword(ctx)
	if err != nil {
		return fmt.Errorf("ensure clickhouse password: %w", err)
	}
	if chGenerated {
		log.Printf("toolyard: minted a new clickhouse_password; reveal it once from the dashboard's Settings tab")
	}
	if err := api.WriteClickhouseRuntimeEnv(*clickhouseRuntimeEnvPath, chPass); err != nil {
		log.Printf("warning: could not write clickhouse runtime env file at %s: %v", *clickhouseRuntimeEnvPath, err)
	}

	// Open the lake against the local CH stack. Addr is hardcoded to
	// 127.0.0.1:19000 (the host binding from deploy/clickhouse/docker-compose.yaml);
	// CH is loopback-only and the gateway runs on the host, so no
	// further indirection. If CH is down or the password is wrong,
	// log and continue with lake.* tools disabled — the gateway still
	// serves non-lake tools.
	{
		var lakeErr error
		lakeSvc, lakeErr = lake.Open(lake.Config{
			Addr:     "127.0.0.1:19000",
			User:     "default",
			Password: chPass,
		})
		if lakeErr != nil {
			log.Printf("lake: open clickhouse: %v (lake.* tools disabled)", lakeErr)
		} else {
			defer lakeSvc.Close()
			log.Printf("lake: opened clickhouse @ 127.0.0.1:19000")
		}
	}

	usageSvc := usagepkg.New(db)
	vis := visibility.New(settingsSvc, usageSvc)

	metricsRec := metrics.New(db)
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsRec.Close(shCtx)
	}()
	metricsReader := metrics.NewReader(db)

	autoApprover := autoapproval.New(db, metricsReader, settingsSvc)
	bus.SetAutoApprover(autoApprover)
	go autoApprover.RunProposer(ctx, time.Hour)
	go runSessionPurger(ctx, idSvc)

	// Background analytics jobs: anomaly detection, reason quality
	// scoring, retention compaction. All best-effort — failures log and
	// continue.
	zScore := settingsSvc.GetFloat(settings.AnomalyRateZScore, 3)
	go metrics.NewAnomalyDetector(metricsReader, zScore).RunForever(ctx)
	go runReasonScorer(ctx, metricsReader)
	go runRetentionCompactor(ctx, metricsReader, settingsSvc)

	gw := gateway.New(gateway.Options{
		Name:                "toolyard",
		Version:             version,
		Policy:              policy.New(),
		Approval:            bus,
		Audit:               auditSvc,
		Hub:                 hub,
		Memory:              memSvc,
		Lake:                lakeSvc,
		InLineWait:          *inLineWait,
		Visibility:          vis,
		Usage:               usageSvc,
		Metrics:             metricsRec,
		MetricsReader:       metricsReader,
		Surface:             vis,
		UpstreamCallTimeout: *upstreamCallTimeout,
	})
	gw.RegisterBuiltins()
	defer gw.Close()

	// Auto-execute on approve: when the human (or an auto-rule) flips
	// an approval to allowed, the bus invokes Gateway.Execute on a
	// background goroutine, which runs the persisted tool args and
	// writes the result back via bus.SetResult. The agent collects the
	// result through tools.poll_approval / tools.wait_for_approval —
	// no need to re-call the original tool.
	bus.SetExecutor(gw)
	if n, err := bus.SweepUnexecuted(ctx); err != nil {
		log.Printf("auto-execute: startup sweep: %v", err)
	} else if n > 0 {
		log.Printf("auto-execute: startup sweep re-fired %d allowed-but-not-executed approvals", n)
	}

	upstreamSvc := upstreams.New(db, gw)
	upstreamSvc.SetPolicy(upstreams.Policy{
		AllowStdio:  !*noStdioUpstreams,
		EnvDenylist: splitCSV(*envDenylistFlag),
	})
	if *noStdioUpstreams {
		log.Printf("toolyard: stdio upstreams disabled (--no-stdio-upstreams)")
	}

	// Wire the OAuth service. Master key lives in <data-dir>/oauth.key
	// (separate from session.key so leaking the cookie HMAC doesn't expose
	// every upstream's refresh token). The hub adapter forwards the
	// service's small set of event names ("mcp_oauth_done", etc.) onto the
	// dashboard's SSE feed.
	oauthKey, err := oauth.LoadOrCreateKey(*dataDir)
	if err != nil {
		return fmt.Errorf("oauth key: %w", err)
	}
	oauthCipher, err := oauth.NewCipher(oauthKey)
	if err != nil {
		return fmt.Errorf("oauth cipher: %w", err)
	}
	oauthSvc := oauth.New(db, oauthCipher,
		hubEventBus{hub: hub},
		pushSvc,
		identityResolver{id: idSvc},
	)
	upstreamSvc.SetAuth(oauthSvc)
	if err := oauthSvc.PrimeBearers(ctx); err != nil {
		log.Printf("oauth: prime bearers: %v", err)
	}
	go oauthSvc.RunRefresher(ctx)

	if err := upstreamSvc.LoadAll(ctx); err != nil {
		log.Printf("upstreams: load: %v", err)
	}

	// Legacy: also accept a JSON config file. Configs from -upstreams are
	// imported into the DB so the dashboard can manage them afterwards.
	if *upstreamConfig != "" {
		if err := importUpstreams(ctx, upstreamSvc, *upstreamConfig); err != nil {
			log.Printf("import upstreams: %v", err)
		}
	}

	// Lake API token + Grafana origin used to live in env vars; they're
	// now persisted in the settings DB and managed via the dashboard. On
	// first boot we mint a token if there isn't one (so a fresh install
	// just works) and write it to the runtime env file the Grafana
	// docker-compose stack reads. Operators rotate from the UI; the
	// runtime env file gets re-rendered automatically.
	lakeTok, generated, err := settingsSvc.EnsureLakeAPIToken(ctx)
	if err != nil {
		return fmt.Errorf("ensure lake api token: %w", err)
	}
	if generated {
		log.Printf("toolyard: minted a new lake_api_token; reveal it once from the dashboard's Settings tab")
	}
	if err := api.WriteGrafanaRuntimeEnv(*grafanaRuntimeEnvPath, lakeTok); err != nil {
		log.Printf("warning: could not write grafana runtime env file at %s: %v", *grafanaRuntimeEnvPath, err)
	}

	// (CH password is ensured + rendered earlier, right after settings
	// init, because the lake.Open() call also needs the password.)

	// MemPalace: install (if needed), initialize palace dir, and register
	// the built-in stdio upstream so its 29 tools surface as `mempalace.*`.
	// All wired in a soft style so a missing `uv` on the host doesn't break
	// the rest of the gateway.
	mpMode := mempalace.ParseMode(*mempalaceFlag)
	mpSvc := mempalace.New(db, gw, auditSvc, *dataDir, mpMode)
	if !mpSvc.Disabled() {
		if err := mpSvc.EnsureInstalled(ctx); err != nil {
			return fmt.Errorf("mempalace install: %w", err)
		}
		if err := mpSvc.EnsureInitialized(ctx); err != nil {
			log.Printf("mempalace init: %v", err)
		}
		// Only register the upstream if the binary is actually findable;
		// otherwise the upstreams package will try to fork it and fail.
		if _, lookErr := exec.LookPath(mpSvc.Binary()); lookErr == nil {
			if _, err := upstreamSvc.UpsertBuiltin(ctx, upstreams.Server{
				Name:      mempalace.ToolPrefix,
				Transport: "stdio",
				Command:   mpSvc.Binary(),
				Args:      []string{"--palace", mpSvc.PalaceDir()},
				Enabled:   true,
			}); err != nil {
				log.Printf("mempalace upstream: %v", err)
			} else {
				log.Printf("mempalace: connected (palace=%s)", mpSvc.PalaceDir())
			}
		} else if mpMode == mempalace.ModeOn {
			return fmt.Errorf("mempalace: binary %q not on PATH after install (mode=on)", mpSvc.Binary())
		} else {
			log.Printf("mempalace: binary %q not found on PATH; integration inactive (mode=auto)", mpSvc.Binary())
		}
	} else {
		log.Printf("mempalace: disabled (-mempalace=off)")
	}

	secOpts := api.SecurityOptions{
		PublicURL:      *publicURL,
		TrustedProxies: parseCIDRs(*trustedProxies),
	}

	// REST API + dashboard.
	apiSrv := api.New(api.Options{
		Identity:     idSvc,
		Approval:     bus,
		Audit:        auditSvc,
		Memory:       memSvc,
		Push:         pushSvc,
		Hub:          hub,
		Gateway:      gw,
		Upstreams:    upstreamSvc,
		Settings:     settingsSvc,
		Usage:        usageSvc,
		Metrics:         metricsReader,
		MetricsRecorder: metricsRec,
		AutoApproval: autoApprover,
		OAuth:        oauthSvc,
		Lake:                     lakeSvc,
		GrafanaRuntimeEnvPath:    *grafanaRuntimeEnvPath,
		ClickhouseRuntimeEnvPath: *clickhouseRuntimeEnvPath,
		Mempalace:                mpSvc,
		SessionKey:               loadOrCreateSessionKey(*dataDir),
		Security:                 secOpts,
	})

	mux := http.NewServeMux()
	apiSrv.Routes(mux)

	// Streamable HTTP MCP transport at /mcp. Agents use this if they prefer
	// HTTP over stdio. Auth via Authorization: Bearer <agent-token>.
	streamable := server.NewStreamableHTTPServer(gw.MCPServer(),
		server.WithEndpointPath("/mcp"),
		// Stateless mode is opt-in via -stateless-mcp. The default
		// (stateful) lets us push notifications/tools/list_changed to
		// connected agents when servers/settings change; the cost is
		// every toolyard restart invalidates outstanding session IDs and
		// MCP clients that don't auto-reconnect on session-invalid see
		// "Invalid session ID" 404s until they're manually bounced.
		server.WithStateLess(*statelessMCP),
		// In stateless mode there's no session for the server to push
		// notifications onto, so a GET /mcp listening connection has
		// nothing to ever stream — but mcp-go will hold it open
		// indefinitely waiting. Clients like hermes that open a GET
		// listener as part of their handshake then time out before
		// they ever try POST. Telling the server to 405 GETs in
		// stateless mode lets those clients immediately fall back to
		// POST-only operation, which works fine.
		server.WithDisableStreaming(*statelessMCP),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			tok := r.Header.Get("Authorization")
			if strings.HasPrefix(tok, "Bearer ") {
				if ag, err := idSvc.VerifyAgentToken(ctx, strings.TrimPrefix(tok, "Bearer ")); err == nil {
					ctx = gateway.WithAgentID(ctx, ag.ID)
				}
			}
			return ctx
		}),
	)
	// Reject calls that carry a Bearer token we can't verify, so a stale
	// token surfaces as a clear 401 instead of silently falling through to
	// the anonymous bucket. When -require-auth-on-mcp is on (auto-enabled
	// when -public-url is set), we *also* reject anonymous traffic — the
	// dashboard is exposed publicly so we can't trust unauthenticated
	// callers to have any business calling our tools.
	mcpAuthGuard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("Authorization")
			if !strings.HasPrefix(tok, "Bearer ") {
				if *requireAuthMCP {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"toolyard: bearer token required"}}`))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			raw := strings.TrimPrefix(tok, "Bearer ")
			if _, err := idSvc.VerifyAgentToken(r.Context(), raw); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"toolyard: invalid agent token; re-enroll via the dashboard"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	// /mcp gets a larger body limit because tool-call args can be a few MB.
	mux.Handle("/mcp", api.LimitBody(mcpAuthGuard(streamable), 16<<20))
	mux.Handle("/mcp/", api.LimitBody(mcpAuthGuard(streamable), 16<<20))

	// pprof endpoints, gated to loopback. When toolyard hangs, run
	//   curl -s http://127.0.0.1:<port>/debug/pprof/goroutine?debug=2
	// from the host to capture every goroutine's stack — that's how
	// we'll find which call is stuck and on what mutex/syscall.
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", httppprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
	loopbackOnly := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiSrv.SecurityOpts().IsLoopback(r) {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("/debug/pprof/", loopbackOnly(pprofMux))

	// Lake dashboard static assets at /lake/*.
	mux.Handle("/lake/", http.StripPrefix("/lake/", lakeStaticHandler()))

	// Dashboard static assets.
	mux.Handle("/", staticHandler())

	// Compose the public-facing handler:
	//   security headers → origin enforcement → API hardening → body cap → mux
	//
	// HardenAPI is the load-bearing application-layer defense: per-route
	// body caps, anti-CSRF custom-header check on cookie mutations,
	// strict content-type, and per-IP rate limits on the unauthenticated
	// bootstrap routes. It only applies to /v1/* — /mcp keeps its
	// dedicated bearer-token guard above.
	var handler http.Handler = mux
	handler = api.LimitBody(handler, 4<<20) // 4 MiB universal ceiling
	handler = apiSrv.HardenAPI(handler)
	handler = apiSrv.EnforceOriginOnMutations(handler)
	handler = apiSrv.SecurityHeaders(handler)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// SSE responses are open-ended; keep WriteTimeout 0 so the streamer
		// isn't cut off at deadline. The handler enforces its own keep-alive
		// timing.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// SIGUSR1 -> dump every goroutine to stderr. `kill -USR1 <pid>` is
	// how the operator captures a hang without enabling pprof or
	// attaching a debugger; the dump shows up in journalctl alongside
	// the slow-call lines logged by the watchdog.
	dumpSig := make(chan os.Signal, 1)
	signal.Notify(dumpSig, syscall.SIGUSR1)
	go func() {
		for range dumpSig {
			log.Printf("goroutine-dump: count=%d inflight_calls=%d (SIGUSR1; full stacks below)",
				runtime.NumGoroutine(), gw.InFlight())
			_ = runtimepprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		}
	}()

	go func() {
		log.Printf("toolyard: HTTP listening on %s (dashboard + /mcp + /v1/*)", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http serve: %v", err)
			cancel()
		}
	}()

	if *stdio {
		go func() {
			log.Printf("toolyard: also serving MCP over stdio")
			stdioSrv := server.NewStdioServer(gw.MCPServer())
			if err := stdioSrv.Listen(ctx, os.Stdin, os.Stdout); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Printf("stdio serve: %v", err)
			}
		}()
	}

	select {
	case <-stop:
		log.Printf("toolyard: shutting down")
	case <-ctx.Done():
	}
	cancel()
	shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	_ = httpSrv.Shutdown(shutdownCtx)
	return nil
}

// ---- exchange ---------------------------------------------------------------

func runExchange(argv []string) error {
	fs := flag.NewFlagSet("exchange", flag.ExitOnError)
	url := fs.String("url", "http://localhost:8787", "toolyard base URL")
	code := fs.String("code", "", "enrollment code printed by the dashboard")
	_ = fs.Parse(argv)
	if *code == "" {
		return errors.New("-code is required")
	}
	body, _ := json.Marshal(map[string]string{"Code": *code})
	resp, err := http.Post(*url+"/v1/agents/exchange", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return err
	}
	if e, ok := out["error"].(string); ok {
		return errors.New(e)
	}
	fmt.Println("agent_id:", out["agent_id"])
	fmt.Println("token:   ", out["token"])
	fmt.Println()
	fmt.Println("# Add to your MCP-aware agent (e.g. Claude Code):")
	fmt.Printf("#   url: %s/mcp\n#   header: Authorization: Bearer %s\n", *url, out["token"])
	return nil
}

// ---- probe ------------------------------------------------------------------

func runProbe(argv []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to upstream config JSON")
	_ = fs.Parse(argv)
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	body, err := os.ReadFile(*cfgPath)
	if err != nil {
		return err
	}
	var cfgs []gateway.UpstreamConfig
	if err := json.Unmarshal(body, &cfgs); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "toolyard-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	db, err := store.Open(filepath.Join(tmpDir, "toolyard.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	bus, err := approval.New(ctx, db)
	if err != nil {
		return err
	}
	gw := gateway.New(gateway.Options{
		Policy: policy.New(), Approval: bus,
		Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub(),
	})
	defer gw.Close()
	for _, c := range cfgs {
		if err := gw.AddUpstream(ctx, c); err != nil {
			fmt.Printf("upstream %s: ERROR %v\n", c.Name, err)
			continue
		}
		fmt.Printf("upstream %s: connected ok\n", c.Name)
	}
	return nil
}

// ---- helpers ----------------------------------------------------------------

func defaultDataDir() string {
	if d := os.Getenv("TOOLYARD_DATA_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".toolyard")
	}
	return ".toolyard"
}

// importUpstreams reads a JSON file of upstream configs and inserts them via
// the upstreams.Service so they end up persisted and dashboard-manageable. A
// row that already exists by name is left alone — the dashboard is the
// source of truth after first run.
func importUpstreams(ctx context.Context, svc *upstreams.Service, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfgs []gateway.UpstreamConfig
	if err := json.Unmarshal(body, &cfgs); err != nil {
		return err
	}
	for _, c := range cfgs {
		srv := upstreams.Server{
			Name: c.Name, Transport: c.Transport, Command: c.Command,
			Args: c.Args, URL: c.URL, Env: c.Env,
		}
		if _, err := svc.Add(ctx, srv); err != nil {
			if errors.Is(err, upstreams.ErrAlreadyHere) {
				continue
			}
			log.Printf("upstream %s: %v", c.Name, err)
			continue
		}
		log.Printf("upstream %s: imported and connected", c.Name)
	}
	return nil
}

// runReasonScorer runs the reason-quality scorer every 10 minutes, scoring
// rows whose reason_quality is still NULL. Cheap — nothing else to do.
func runReasonScorer(ctx context.Context, r *metrics.Reader) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	// First pass at startup so existing rows get scored.
	if err := r.ScoreReasons(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		log.Printf("reason scorer (initial): %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.ScoreReasons(ctx, time.Now().Add(-24*time.Hour)); err != nil {
				log.Printf("reason scorer: %v", err)
			}
		}
	}
}

// runRetentionCompactor wakes daily and removes call_events older than the
// configured retention window. This keeps the metrics DB from growing
// unboundedly while still leaving plenty of history for analytics.
func runRetentionCompactor(ctx context.Context, r *metrics.Reader, set *settings.Service) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	step := func() {
		days := set.GetInt(settings.MetricsRetentionDays, 90)
		if days <= 0 {
			return
		}
		before := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		if n, err := r.PurgeOlderThan(ctx, before); err != nil {
			log.Printf("retention compactor: %v", err)
		} else if n > 0 {
			log.Printf("retention compactor: purged %d rows older than %s", n, before.Format(time.RFC3339))
		}
	}
	step()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			step()
		}
	}
}

// runSessionPurger drops expired session rows so the table doesn't bloat.
// Runs every 6h with a 24h grace window past expiry — long enough that a
// laptop coming back online from a long sleep can still see its session
// row before deletion (so the user gets a "your session expired" message
// rather than an opaque 401 after a normal token expiry).
func runSessionPurger(ctx context.Context, id *identity.Service) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	step := func() {
		if n, err := id.PurgeExpiredSessions(ctx, 24*time.Hour); err != nil {
			log.Printf("session purger: %v", err)
		} else if n > 0 {
			log.Printf("session purger: removed %d expired session rows", n)
		}
	}
	step()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			step()
		}
	}
}

// parseCIDRs is used to translate the -trusted-proxy CSV flag into
// net.IPNet entries. Bad entries are logged and skipped — better to start
// up with a partial allowlist than refuse to boot on a typo.
func parseCIDRs(csv string) []*net.IPNet {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	var out []*net.IPNet
	for _, raw := range strings.Split(csv, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, c, err := net.ParseCIDR(raw)
		if err != nil {
			log.Printf("trusted-proxy: ignoring invalid CIDR %q: %v", raw, err)
			continue
		}
		out = append(out, c)
	}
	return out
}

// splitCSV is a tiny helper for the env-key denylist flag.
func splitCSV(s string) []string {
	out := []string{}
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			out = append(out, raw)
		}
	}
	return out
}

// loadOrCreateSessionKey persists a 32-byte HMAC key in <data-dir>/session.key
// so JWT-signed dashboard cookies survive process restarts.
func loadOrCreateSessionKey(dataDir string) []byte {
	path := filepath.Join(dataDir, "session.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		out, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err == nil && len(out) == 32 {
			return out
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	_ = os.WriteFile(path, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600)
	return key
}

// staticHandler serves the embedded dashboard. It also fingerprints assets in
// log so we can spot stale-cache bugs.
func staticHandler() http.Handler {
	sub, err := fs.Sub(dashboard.Assets, ".")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	// Bust browser caches across deploys: hash app.js + style.css at
	// startup, then rewrite index.html's <script src="/app.js"> and
	// <link href="/style.css"> to include ?v=<hash>. A new build → new
	// hash → the browser fetches a fresh URL it has never seen, even
	// when the previously-cached asset still has a long-lived freshness
	// window (which Cache-Control:no-cache cannot retroactively shorten).
	appHash := assetHash(sub, "app.js")
	cssHash := assetHash(sub, "style.css")
	swHash := assetHash(sub, "sw.js")
	rewriteIndex := func(body []byte) []byte {
		out := strings.ReplaceAll(string(body), `src="/app.js"`, `src="/app.js?v=`+appHash+`"`)
		out = strings.ReplaceAll(out, `href="/style.css"`, `href="/style.css?v=`+cssHash+`"`)
		return []byte(out)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't let SPA path swallow the API.
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/mcp") {
			http.NotFound(w, r)
			return
		}
		// MCP-over-HTTP clients probe OAuth discovery endpoints first
		// (.well-known/oauth-protected-resource, oauth-authorization-server,
		// /register). We use static bearer-token auth, so these must 404 —
		// returning the SPA's index.html breaks the client with
		// "Failed to parse JSON" because it expects a discovery document.
		if strings.HasPrefix(r.URL.Path, "/.well-known/") ||
			r.URL.Path == "/register" ||
			r.URL.Path == "/authorize" ||
			r.URL.Path == "/token" {
			http.NotFound(w, r)
			return
		}
		// Serve index.html for the root and any unknown path so the SPA can
		// pick up via #hash routing.
		if r.URL.Path == "/" || !assetExists(sub, strings.TrimPrefix(r.URL.Path, "/")) {
			raw, err := fs.ReadFile(dashboard.Assets, "index.html")
			if err != nil {
				http.Error(w, "missing index", http.StatusInternalServerError)
				return
			}
			body := rewriteIndex(raw)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			etag := contentETag(body)
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(body)
			return
		}
		_ = swHash // referenced for completeness; sw.js is loaded once and updates via SW lifecycle
		// Common content types for our few static files. http.FileServer
		// can sometimes mistype CSS as text/plain on systems with sparse
		// /etc/mime.types — be explicit so the browser actually applies it
		// (and the strict Content-Security-Policy doesn't reject it for
		// being the wrong MIME).
		//
		// Cache-Control:no-cache forces the browser to revalidate every
		// time. http.FileServer still emits a Last-Modified / ETag, so a
		// 304 keeps the network cost roughly nil. The big win: a fresh
		// `sudo ./deploy/install.sh` lands a new app.js / style.css and
		// the next page load picks them up without a hard refresh — no
		// more "X-Requested-With required" surprises after a redeploy.
		switch filepath.Ext(r.URL.Path) {
		case ".webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// lakeStaticHandler serves the embedded /lake/ dashboard. Strict-by-default:
// any unknown path falls back to index.html so the SPA can handle hash
// routing, and we set Cache-Control:no-cache so a redeploy lands cleanly.
func lakeStaticHandler() http.Handler {
	sub, err := fs.Sub(weblake.Assets, ".")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip-prefix has already removed /lake/. The empty path is the
		// SPA root; unknown paths fall back to index.html.
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || !assetExists(sub, p) {
			body, err := fs.ReadFile(weblake.Assets, "index.html")
			if err != nil {
				http.Error(w, "missing lake index", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(body)
			return
		}
		switch filepath.Ext(p) {
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		case ".json":
			w.Header().Set("Content-Type", "application/json")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func assetExists(sub fs.FS, name string) bool {
	if name == "" {
		return false
	}
	_, err := fs.Stat(sub, name)
	return err == nil
}

// assetHash returns 8 hex chars of sha256(body) for the embedded asset,
// or "dev" if the file is missing (shouldn't happen, but we don't want
// to panic the binary on a stripped build).
func assetHash(sub fs.FS, name string) string {
	body, err := fs.ReadFile(sub, name)
	if err != nil || len(body) == 0 {
		return "dev"
	}
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:8]
}

func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + base64.RawURLEncoding.EncodeToString(sum[:8]) + `"`
}

// hubEventBus adapts realtime.Hub to oauth.EventBus by translating
// (eventType, data) -> realtime.Event{Type, Data}.
type hubEventBus struct{ hub *realtime.Hub }

func (h hubEventBus) Publish(eventType string, data any) {
	h.hub.Publish(realtime.Event{Type: eventType, Data: data})
}

// identityResolver adapts identity.Service.PrimaryUser → oauth.IdentityResolver.
type identityResolver struct{ id *identity.Service }

func (r identityResolver) PrimaryUserID(ctx context.Context) (string, error) {
	u, err := r.id.PrimaryUser(ctx)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// silence unused-import warnings in case build tags drop something.
var _ = net.Listen

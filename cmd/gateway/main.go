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
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
	usagepkg "github.com/tusharbhardwaj/toolyard/internal/usage"
	"github.com/tusharbhardwaj/toolyard/internal/visibility"
	dashboard "github.com/tusharbhardwaj/toolyard/web/dashboard"
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
	inLineWait := fs.Duration("in-line-wait", 30*time.Second, "max time to block a held call before returning a deferred response")
	approvalTTL := fs.Duration("approval-ttl", 3*time.Hour, "how long a pending approval stays decidable before auto-expiring")
	publicURL := fs.String("public-url", "", "public origin (e.g. https://toolyard.example.com). When set, enables HSTS, secure cookies, Origin enforcement, and locks /v1/auth/setup to loopback.")
	trustedProxies := fs.String("trusted-proxy", "", "comma-separated CIDRs to trust for X-Forwarded-* headers (e.g. 127.0.0.1/32,::1/128,10.0.0.0/8)")
	requireAuthMCP := fs.Bool("require-auth-on-mcp", false, "reject anonymous /mcp calls (no Authorization header). Auto-enabled when -public-url is set.")
	noStdioUpstreams := fs.Bool("no-stdio-upstreams", false, "refuse to start any stdio (subprocess) MCP upstream. Use when the dashboard is exposed publicly so a compromised session can't spawn arbitrary commands.")
	envDenylistFlag := fs.String("upstream-env-denylist", "LD_PRELOAD,LD_LIBRARY_PATH,DYLD_INSERT_LIBRARIES,DYLD_LIBRARY_PATH,PATH", "comma-separated env var keys forbidden in upstream stdio configs")
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
				"title":           n.Title,
				"body":            n.Body,
				"url":             n.URL,
				"approval_id":     req.ID,
				"decision_token":  req.DecisionToken,
				"tag":             req.ID,
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
		Name:       "toolyard",
		Version:    version,
		Policy:     policy.New(),
		Approval:   bus,
		Audit:      auditSvc,
		Hub:        hub,
		Memory:     memSvc,
		InLineWait: *inLineWait,
		Visibility: vis,
		Usage:      usageSvc,
		Metrics:    metricsRec,
		Surface:    vis,
	})
	gw.RegisterBuiltins()
	defer gw.Close()

	upstreamSvc := upstreams.New(db, gw)
	upstreamSvc.SetPolicy(upstreams.Policy{
		AllowStdio:  !*noStdioUpstreams,
		EnvDenylist: splitCSV(*envDenylistFlag),
	})
	if *noStdioUpstreams {
		log.Printf("toolyard: stdio upstreams disabled (--no-stdio-upstreams)")
	}
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
		Metrics:      metricsReader,
		AutoApproval: autoApprover,
		SessionKey:   loadOrCreateSessionKey(*dataDir),
		Security:     secOpts,
	})

	mux := http.NewServeMux()
	apiSrv.Routes(mux)

	// Streamable HTTP MCP transport at /mcp. Agents use this if they prefer
	// HTTP over stdio. Auth via Authorization: Bearer <agent-token>.
	streamable := server.NewStreamableHTTPServer(gw.MCPServer(),
		server.WithEndpointPath("/mcp"),
		server.WithStateLess(false),
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
			body, err := fs.ReadFile(dashboard.Assets, "index.html")
			if err != nil {
				http.Error(w, "missing index", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			etag := contentETag(body)
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(body)
			return
		}
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

func assetExists(sub fs.FS, name string) bool {
	if name == "" {
		return false
	}
	_, err := fs.Stat(sub, name)
	return err == nil
}

func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + base64.RawURLEncoding.EncodeToString(sum[:8]) + `"`
}

// silence unused-import warnings in case build tags drop something.
var _ = net.Listen

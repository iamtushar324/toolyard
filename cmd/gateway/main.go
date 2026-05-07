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
	_ = fs.Parse(argv)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}
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
			body := fmt.Sprintf("%s · %s — %.140s", req.UpstreamName, req.ToolName, req.Reason)
			n := push.Notification{
				Title:    "toolyard approval needed",
				Body:     body,
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
	// the anonymous bucket (where audit/approval rows lose their agent_id
	// and the operator has no way to tell which Claude Code session
	// generated them). Calls with no Authorization header at all are still
	// accepted as anonymous — that's how stdio sessions work.
	mcpAuthGuard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tok := r.Header.Get("Authorization"); strings.HasPrefix(tok, "Bearer ") {
				raw := strings.TrimPrefix(tok, "Bearer ")
				if _, err := idSvc.VerifyAgentToken(r.Context(), raw); err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"toolyard: invalid agent token; re-enroll via the dashboard"}}`))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("/mcp", mcpAuthGuard(streamable))
	mux.Handle("/mcp/", mcpAuthGuard(streamable))

	// Dashboard static assets.
	mux.Handle("/", staticHandler())

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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
		// Common content types for our few static files.
		switch filepath.Ext(r.URL.Path) {
		case ".webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
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

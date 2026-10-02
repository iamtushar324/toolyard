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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	runtimepprof "runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/api"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/auditlink"
	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/chatnotify"
	"github.com/tusharbhardwaj/toolyard/internal/chatnotify/telegram"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/crashdump"
	"github.com/tusharbhardwaj/toolyard/internal/events"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	hookspkg "github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	notespkg "github.com/tusharbhardwaj/toolyard/internal/notes"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/passkey"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	skillspkg "github.com/tusharbhardwaj/toolyard/internal/skills"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
	usagepkg "github.com/tusharbhardwaj/toolyard/internal/usage"
	"github.com/tusharbhardwaj/toolyard/internal/visibility"
	"github.com/tusharbhardwaj/toolyard/internal/voice"
	dashboard "github.com/tusharbhardwaj/toolyard/web/dashboard"
)

// version is stamped by the release build via
// -ldflags "-X main.version=<tag>"; "dev" means a local/untagged build.
var version = "dev"

func main() {
	// The code-mode script worker: the gateway starts this binary again,
	// with a bare environment, to run one agent script in a process of its
	// own (internal/codemode). It runs before loadDotenv so no secret from
	// a .env reaches the process that executes agent code.
	if len(os.Args) > 1 && os.Args[1] == "codemode-worker" {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout, os.Stderr))
	}

	// Auto-load .env from CWD (and, if present, a .env next to the
	// binary) before any os.Getenv lookup runs. Real exported env vars
	// still win — godotenv.Load only fills *unset* keys — so a CI/prod
	// systemd unit can override .env without editing the file.
	loadDotenv()

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
	case "operator-token":
		err = runOperatorToken(args)
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

// loadDotenv pulls keys from a .env in the current working directory and,
// failing that, a .env next to the binary. Real env vars always win — we
// pass the file as the second arg of godotenv.Load which only sets keys
// that aren't already in the environment. Missing files are silently OK;
// this is opt-in by file presence, not a hard dependency.
func loadDotenv() {
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p) // already-set env wins; safe to ignore parse errors
		}
	}
}

// clerkFromEnv builds the Clerk client from TOOLYARD_CLERK_SECRET_KEY,
// TOOLYARD_CLERK_PUBLISHABLE_KEY and TOOLYARD_CLERK_ORGANIZATION_ID. All
// three set: Clerk sign-in is on. None: off. One or two: a startup error,
// as is Clerk without -public-url — a Clerk session token is accepted only
// when its azp claim is the dashboard's own origin. Returns the client (nil
// when off) and the instance's Frontend API host for the login-page CSP.
func clerkFromEnv(publicURL, ownerEmail string, ownerOnly bool) (*clerk.Client, string, error) {
	vars := []struct{ key, val string }{
		{"TOOLYARD_CLERK_SECRET_KEY", strings.TrimSpace(os.Getenv("TOOLYARD_CLERK_SECRET_KEY"))},
		{"TOOLYARD_CLERK_PUBLISHABLE_KEY", strings.TrimSpace(os.Getenv("TOOLYARD_CLERK_PUBLISHABLE_KEY"))},
		{"TOOLYARD_CLERK_ORGANIZATION_ID", strings.TrimSpace(os.Getenv("TOOLYARD_CLERK_ORGANIZATION_ID"))},
	}
	var missing []string
	for _, v := range vars {
		if v.val == "" {
			missing = append(missing, v.key)
		}
	}
	if ownerOnly {
		if strings.TrimSpace(ownerEmail) == "" {
			return nil, "", errors.New("clerk: -clerk-owner-only requires -owner-email")
		}
		missing = nil
		for _, v := range vars[:2] {
			if v.val == "" {
				missing = append(missing, v.key)
			}
		}
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("clerk: personal sign-in requires keys; missing: %s", strings.Join(missing, ", "))
		}
	}
	if len(missing) == len(vars) {
		return nil, "", nil
	}
	if len(missing) > 0 {
		return nil, "", fmt.Errorf("clerk: set all of TOOLYARD_CLERK_SECRET_KEY, TOOLYARD_CLERK_PUBLISHABLE_KEY and TOOLYARD_CLERK_ORGANIZATION_ID, or none of them; missing: %s",
			strings.Join(missing, ", "))
	}
	if publicURL == "" {
		return nil, "", errors.New("clerk: -public-url is required when Clerk sign-in is configured (session tokens are bound to the dashboard origin)")
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, "", fmt.Errorf("clerk: -public-url %q is not an absolute URL", publicURL)
	}
	allowedEmail := ""
	if ownerOnly {
		allowedEmail = ownerEmail
	}
	c, err := clerk.New(clerk.Config{
		SecretKey:          vars[0].val,
		PublishableKey:     vars[1].val,
		OrganizationID:     vars[2].val,
		AllowedGoogleEmail: allowedEmail,
		AuthorizedParties:  []string{u.Scheme + "://" + u.Host},
	})
	if err != nil {
		return nil, "", err
	}
	return c, c.FrontendAPI(), nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func usage() {
	fmt.Println(`toolyard ` + version + `

Commands:
  serve       run gateway + HTTP dashboard (default ports: stdio + :8787)
  exchange    swap an enrollment code for an agent token
  probe       dry-run a tool against an upstream MCP server
  lake        manage the personal data lake (backup | tables)
  operator-token  create | list | revoke CLI operator tokens (local, from -data)
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
	publicURL := fs.String("public-url", "", "public origin (e.g. https://toolyard.example.com). When set, enables HSTS, secure cookies, and Origin enforcement.")
	clerkOwnerOnly := fs.Bool("clerk-owner-only", false, "allow only the verified Google account at -owner-email; disable local password login and organization sync")
	ownerEmail := fs.String("owner-email", "", "email of the local owner account. The first Clerk (Google) sign-in with this address attaches to the existing password admin instead of creating a new member. Case-insensitive.")
	clerkSyncEvery := fs.Duration("clerk-sync-interval", time.Hour, "how often to list the Clerk organisation's members and block users who left (sessions revoked, agents stopped). Only runs when the TOOLYARD_CLERK_* env vars are set.")
	trustedProxies := fs.String("trusted-proxy", "", "comma-separated CIDRs to trust for X-Forwarded-* headers (e.g. 127.0.0.1/32,::1/128,10.0.0.0/8)")
	clickhouseRuntimeEnvPath := fs.String("clickhouse-runtime-env", "/var/lib/toolyard/clickhouse-runtime.env", "path where toolyard maintains a TOOLYARD_CH_PASSWORD=... line for the toolyard-clickhouse container's docker-compose env_file to consume. Updated on bootstrap and on every rotate. Empty disables the write (useful in tests).")
	inboxResetPasskeys := fs.Bool("inbox-reset-passkeys", false, "delete every registered passkey at startup (recovery when the owner has lost all their devices), then continue normally")
	inboxFetchPrivate := fs.Bool("inbox-fetch-private-networks", false, "let the inbox copy attachment links that point at loopback or private-network addresses (e.g. evidence hosted on your LAN). Off by default: agent-chosen URLs could otherwise reach internal services.")
	requireAuthMCP := fs.Bool("require-auth-on-mcp", false, "reject anonymous /mcp calls (no Authorization header). Auto-enabled when -public-url is set.")
	statelessMCP := fs.Bool("stateless-mcp", false, "skip MCP session-ID tracking. Every request stands alone — no server-initiated notifications, but agents that don't auto-reconnect on session-invalid (e.g., hermes) survive a toolyard restart without manual intervention.")
	noStdioUpstreams := fs.Bool("no-stdio-upstreams", false, "refuse to start any stdio (subprocess) MCP upstream. Use when the dashboard is exposed publicly so a compromised session can't spawn arbitrary commands.")
	envDenylistFlag := fs.String("upstream-env-denylist", "LD_PRELOAD,LD_LIBRARY_PATH,DYLD_INSERT_LIBRARIES,DYLD_LIBRARY_PATH,PATH", "comma-separated env var keys forbidden in upstream stdio configs")
	upstreamCallTimeout := fs.Duration("upstream-call-timeout", 120*time.Second, "per-tool-call deadline applied to every dispatch (built-in, fixture, and external upstreams). 0 disables the cap. Without it, a hung upstream pins a goroutine indefinitely and queues every other caller behind it.")
	mempalaceFlag := fs.String("mempalace", "auto", "MemPalace integration mode: on|off|auto. auto=install via `uv tool install mempalace` if missing and proceed; on=fail boot when install fails; off=skip the integration entirely.")
	webhookMaxBytes := fs.Int64("webhook-max-bytes", api.DefaultWebhookMaxBytes, "max body (bytes) accepted by the memory-webhook ingest endpoint (/v1/memory/webhooks/ingest). n8n transcript payloads can be large; bodies above this get a clean 413. Every other route keeps its tight cap.")
	notesFlag := fs.String("notes", "auto", "Notes (markdown workspace) upstream mode: on|off|auto. auto=register the filesystem MCP if npx is on PATH; on=fail boot when npx missing; off=skip the integration entirely.")
	notesDir := fs.String("notes-dir", "", "Directory the notes filesystem upstream exposes (defaults to <data-dir>/notes). Lives under <data-dir> so the existing data-dir backup captures it.")
	notesSyncEvery := fs.Duration("notes-sync-interval", notespkg.DefaultScanEvery, "How often the notes->mempalace background sync walks the notes dir. Use a negative value to disable scanning (notes.publish still works).")
	skillsFlag := fs.String("skills", "auto", "Skills (centralised Claude Code skills workspace) upstream mode: on|off|auto. auto=register the filesystem MCP if npx is on PATH; on=fail boot when npx missing; off=skip the integration entirely.")
	skillsDir := fs.String("skills-dir", "", "Directory the skills filesystem upstream exposes (defaults to <data-dir>/skills). Lives under <data-dir> so the existing data-dir backup captures it.")
	skillsSyncEvery := fs.Duration("skills-sync-interval", skillspkg.DefaultScanEvery, "How often the skills->mempalace background sync walks the skills dir. Use a negative value to disable scanning (skills.publish still works).")
	claudeSkillsDir := fs.String("claude-skills-dir", "", "Default parent directory `skills.install` symlinks/copies into when the caller doesn't pass `target`. Empty means callers must pass `target` explicitly; a typical value is `~/.claude/skills`.")
	voiceFlag := fs.String("voice", "auto", "Voice (live call dashboard panel) mode: on|off|auto. auto=enable iff GEMINI_API_KEY is set in the environment; on=fail boot when the key is missing; off=expose no /v1/voice/* routes.")
	voiceDir := fs.String("voice-dir", "", "Directory holding the voice persona file (soul.md). Defaults to <data-dir>/voice. Seeded on first run with toolyard's embedded default persona.")
	voiceModelDefault := os.Getenv("TOOLYARD_VOICE_MODEL")
	voiceModel := fs.String("voice-model", voiceModelDefault, "Gemini Live model to use for voice calls. Falls back to $TOOLYARD_VOICE_MODEL, then "+voice.DefaultModel+".")
	voiceMaxCall := fs.Duration("voice-max-call", 60*time.Minute, "hard cap on a single voice call's duration; the server hangs up when reached. Bounds the worst-case Gemini Live bill from an abandoned tab.")
	stdioIdleTimeout := fs.Duration("stdio-idle-timeout", 0, "DEPRECATED: alias for -upstream-idle-timeout. Kept for backwards compat.")
	upstreamIdleTimeout := fs.Duration("upstream-idle-timeout", 0, "kill upstream MCP connections idle for this long; transparently re-dial on next call. 0 disables. Recommended: 15m. Covers both stdio (kills subprocess) and http (closes client). Reduces RSS + FDs when no agents are active.")
	upstreamMaxLive := fs.Int("upstream-max-live", 8, "max simultaneously-live upstream connections. When the cap is hit, the least-recently-used upstream is suspended (catalog stays populated, transparently resumed on next call). 0 = unbounded.")
	logLevel := fs.String("log-level", "info", "log level: debug | info | warn | error")
	logFormat := fs.String("log-format", "json", "log format: json | text. Text is friendlier in a terminal; json is what journalctl + jq want.")
	_ = fs.Parse(argv)

	// Initialize structured logging first so every subsequent log line
	// flows through slog. The stdlib `log` package gets re-routed too via
	// logx.Bridge() so existing log.Printf callers don't need to migrate
	// before everything benefits.
	logx.Init(logx.Format(*logFormat), logx.ParseLevel(*logLevel))
	log.SetFlags(0)
	log.SetOutput(logx.Bridge())
	rootLog := logx.For("gateway")

	// Crash dumps go next to the data dir so the same backup that
	// captures toolyard.db captures the crash archive too.
	crashesDir := filepath.Join(*dataDir, "crashes")
	if err := crashdump.Configure(crashesDir); err != nil {
		rootLog.Warn("crashdump configure failed", "dir", crashesDir, "err", err.Error())
	}
	// If a previous run died unexpectedly, surface it on the next start.
	if prev, _ := crashdump.List(); len(prev) > 0 {
		rootLog.Warn("previous crash dumps present",
			"count", len(prev), "latest", prev[0].Path)
	}
	// Auto-prune old crash dumps.
	if n, _ := crashdump.PruneOlderThan(30 * 24 * time.Hour); n > 0 {
		rootLog.Info("pruned old crash dumps", "count", n)
	}

	// Resolve the idle-timeout: new flag wins, old flag is a fallback.
	idleTimeout := *upstreamIdleTimeout
	if idleTimeout == 0 && *stdioIdleTimeout > 0 {
		idleTimeout = *stdioIdleTimeout
		rootLog.Warn("--stdio-idle-timeout is deprecated; use --upstream-idle-timeout")
	}

	// Public mode auto-enables matching safeguards.
	if *publicURL != "" {
		*requireAuthMCP = true
	}

	// Clerk (Google) sign-in: on only when all three TOOLYARD_CLERK_* env
	// vars are set (they come from the systemd EnvironmentFile or .env,
	// never argv). A partial set, or Clerk without -public-url, refuses to
	// start rather than run half-configured.
	clerkClient, clerkFAPI, cerr := clerkFromEnv(*publicURL, *ownerEmail, *clerkOwnerOnly)
	if cerr != nil {
		return cerr
	}
	if clerkClient != nil {
		log.Printf("clerk: sign-in enabled (frontend api %s; owner email %s)", clerkFAPI, orDefault(*ownerEmail, "unset"))
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
	// Roles and per-user tool-group grants. The gateway asks it on every
	// tool list/call; the Users admin API writes through it.
	accessSvc := access.New(db)

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
	// Every approval event (create, decide, expire, cancel, executed)
	// leaves an audit row naming who raised the call and who decided it,
	// whichever path the decision took.
	bus.AddNotifier(auditlink.Notifier(auditSvc))
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
			// The decision token is minted for this recipient, so a tap
			// through it is recorded as their decision.
			payload := map[string]any{
				"title":          n.Title,
				"body":           n.Body,
				"url":            n.URL,
				"approval_id":    req.ID,
				"decision_token": bus.DecisionTokenFor(req.ID, user.ID),
				"tag":            req.ID,
				// Non-secret label (upstream · tool, same as the body) so
				// the service worker can show "✓ Approved — github · …".
				"tool": req.UpstreamName + " · " + req.ToolName,
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

	// ClickHouse password lives in the settings DB: toolyard is the
	// source of truth, the env file the docker stack reads is a rendered
	// view. Auto-mint on first boot so a fresh install + `docker compose
	// up` Just Works. The rendered file is mode 0600 toolyard:toolyard.
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

	// Shared policy engine: the gateway evaluates with it, the API exposes
	// CRUD on the same instance so cache updates are seen immediately.
	policyEngine := policy.New(db)

	// Per-person identity keys ("Beknown keys"). Their own identity.key
	// (blast-radius partitioning, as oauth.key and secrets.key): the raw
	// keys are sealed under it so they can be forwarded to upstreams that
	// name an identity header. The gateway asks this service for the
	// caller's key on every such call; the registry lister and the
	// internal caller are wired below once the upstreams service exists.
	identityKey, err := sealbox.LoadOrCreateKey(*dataDir, "identity.key")
	if err != nil {
		return fmt.Errorf("identity key: %w", err)
	}
	identityCipher, err := sealbox.NewCipher(identityKey)
	if err != nil {
		return fmt.Errorf("identity cipher: %w", err)
	}
	identityKeysSvc := identitykeys.New(db, identityCipher)
	identityKeysSvc.SetAudit(auditSvc)

	gw := gateway.New(gateway.Options{
		Name:                "toolyard",
		Version:             version,
		Policy:              policyEngine,
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
		Access:              accessSvc,
		Identity:            identityKeysSvc,
	})
	gw.RegisterBuiltins()
	defer gw.Close()
	identityKeysSvc.SetCaller(gw)

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

	// Owner inbox + scoped grants. Agents ask for restricted tools with
	// inbox.request; the owner decides on their phone; each allowed tool
	// gets a single-use grant the agent passes as _grant. approval_mode
	// (Settings) decides whether a restricted call without a grant is
	// queued the old way ("execute", default) or coached ("inbox").
	inboxSnaps, err := inbox.NewSnapshotter(db, filepath.Join(*dataDir, "inbox", "blobs"))
	if err != nil {
		return fmt.Errorf("inbox snapshots: %w", err)
	}
	inboxSnaps.AllowPrivateNetworks(*inboxFetchPrivate)
	var inboxJudge inbox.Judge
	if key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); key != "" {
		if j, jerr := inbox.NewGeminiJudge(ctx, key, settingsSvc.GetString(settings.InboxJudgeModel, "")); jerr != nil {
			log.Printf("inbox: judge model unavailable: %v", jerr)
		} else {
			inboxJudge = j
		}
	}
	agentName := func(ctx context.Context, id string) string {
		u, err := idSvc.PrimaryUser(ctx)
		if err != nil {
			return ""
		}
		ags, err := idSvc.ListAgents(ctx, u.ID)
		if err != nil {
			return ""
		}
		for _, a := range ags {
			if a.ID == id {
				return a.Name
			}
		}
		return ""
	}
	var inboxVoice inbox.Voice
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		if v, verr := inbox.NewGeminiVoice(ctx, key, os.Getenv("TOOLYARD_INBOX_VOICE_MODEL"), func() string {
			return settingsSvc.GetString(settings.InboxVoiceName, "")
		}); verr != nil {
			log.Printf("inbox: voice notes unavailable: %v", verr)
		} else {
			inboxVoice = v
		}
	}
	passkeySvc := passkey.New(db)
	if *inboxResetPasskeys {
		n, err := passkeySvc.Reset(ctx)
		if err != nil {
			return fmt.Errorf("reset passkeys: %w", err)
		}
		log.Printf("inbox: removed %d passkey(s) (-inbox-reset-passkeys)", n)
	}
	inboxSvc, err := inbox.New(ctx, inbox.Options{
		Passkeys:          passkeySvc,
		Voice:             inboxVoice,
		VoiceEnabled:      func() bool { return settingsSvc.GetBool(settings.InboxVoiceEnabled) },
		Blobs:             inboxSnaps,
		DB:                db,
		Catalog:           gw,
		Judge:             inboxJudge,
		JudgeEnabled:      func() bool { return settingsSvc.GetBool(settings.InboxJudgeEnabled) },
		Fetcher:           inboxSnaps,
		SnapshotEnabled:   func() bool { return settingsSvc.GetBoolDefault(settings.InboxSnapshotEnabled, true) },
		AllowPrivateMedia: *inboxFetchPrivate,
		Publish:           func(t string, d any) { hub.Publish(realtime.Event{Type: t, Data: d}) },
		AgentName:         agentName,
		Notify: func(ctx context.Context, p inbox.Push) {
			user, err := idSvc.PrimaryUser(ctx)
			if err != nil {
				return
			}
			// Agent-written text is only in p when the owner turned on
			// push details (inbox_push_details); see inbox/attention.go.
			payload := map[string]any{"title": p.Title, "body": p.Body, "url": p.URL, "tag": p.Tag, "kind": "inbox"}
			if p.RequestID != "" {
				payload["request_id"] = p.RequestID
			}
			// The tap token is bound to this recipient when the inbox can
			// mint one, so a tap is recorded as their decision.
			if tok := inboxTapToken(p, user.ID); tok != "" {
				payload["inbox_token"] = tok
			}
			acts := make([]map[string]string, 0, len(p.Actions))
			for _, a := range p.Actions {
				acts = append(acts, map[string]string{"action": a.Action, "title": a.Title})
			}
			payload["actions"] = acts
			_ = pushSvc.Notify(ctx, user.ID, payload)
		},
		Attention: func() inbox.AttentionConfig {
			c := inbox.AttentionConfig{
				NowPerHour: settingsSvc.GetInt(settings.InboxNowPerHour, inbox.DefaultNowPerHour),
				QuietHours: settingsSvc.GetString(settings.InboxQuietHours, ""),
				Details:    settingsSvc.GetBool(settings.InboxPushDetails),
			}
			for _, t := range strings.Split(settingsSvc.GetString(settings.InboxQuietAllow, ""), ",") {
				if t = strings.TrimSpace(t); t != "" {
					c.QuietAllow = append(c.QuietAllow, t)
				}
			}
			// Unset → default slots; set to "" → no digest.
			if v := settingsSvc.GetString(settings.InboxDigestTimes, "\x00"); v != "\x00" {
				c.DigestTimes = []string{}
				for _, t := range strings.Split(v, ",") {
					if t = strings.TrimSpace(t); t != "" {
						c.DigestTimes = append(c.DigestTimes, t)
					}
				}
			}
			if tz := settingsSvc.GetString(settings.InboxTimezone, ""); tz != "" {
				if loc, err := time.LoadLocation(tz); err == nil {
					c.Location = loc
				}
			}
			return c
		},
	})
	if err != nil {
		return fmt.Errorf("inbox: %w", err)
	}
	defer inboxSvc.Close(20 * time.Second)
	if n := inboxSvc.RecheckUnchecked(ctx); n > 0 {
		log.Printf("inbox: re-running background checks for %d request(s)", n)
	}
	inboxGuide := inbox.NewGuide(func() string { return settingsSvc.GetString(settings.InboxHostingNote, "") })
	gw.SetInbox(inboxSvc, inboxGuide, func() string {
		return settingsSvc.GetString(settings.ApprovalMode, settings.ApprovalModeExecute)
	})
	gw.RegisterInboxTools()
	go inboxSvc.RunSweeper(ctx, time.Minute)
	go inboxSvc.RunAttention(ctx, 15*time.Second)
	log.Printf("toolyard: inbox ready (approval_mode=%s, judge=%v)",
		settingsSvc.GetString(settings.ApprovalMode, settings.ApprovalModeExecute), inboxJudge != nil)

	upstreamSvc := upstreams.New(db, gw)
	upstreamSvc.SetPolicy(upstreams.Policy{
		AllowStdio:  !*noStdioUpstreams,
		EnvDenylist: splitCSV(*envDenylistFlag),
	})
	// Registry upstreams: servers whose identity setting has register =
	// true expose the upsert-/delete-bifrost-virtual-key-actor tools.
	identityKeysSvc.SetRegistries(identitykeys.RegistryListerFunc(func(ctx context.Context) ([]string, error) {
		servers, err := upstreamSvc.List(ctx)
		if err != nil {
			return nil, err
		}
		return registryUpstreams(servers), nil
	}))
	// Fingerprints retired by a rotate or revoke whose registry delete
	// failed are retried hourly, each as the admin who retired it.
	identityKeysSvc.StartRemovalRetry(ctx, identitykeys.DefaultRetryInterval)
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

	// Secrets broker. Separate secrets.key (blast-radius partitioning, same
	// rationale as oauth.key vs session.key). Wired into upstreams so
	// secret:// refs in env/headers resolve at dial time, never reaching
	// agents or API responses.
	secretsKey, err := sealbox.LoadOrCreateKey(*dataDir, "secrets.key")
	if err != nil {
		return fmt.Errorf("secrets key: %w", err)
	}
	secretsCipher, err := sealbox.NewCipher(secretsKey)
	if err != nil {
		return fmt.Errorf("secrets cipher: %w", err)
	}
	secretsSvc := secrets.New(db, secretsCipher, auditSvc)
	upstreamSvc.SetSecrets(secretsSvc)

	// Chat-approval notifications (Telegram). The Registry fans approval-bus
	// events out to configured chat channels and reconciles after restarts;
	// the telegram Service is the first Channel. The bot token is sealed with
	// the secrets cipher under a distinct AAD. Long-poll + reconciler run
	// under Supervise so a panic restarts them.
	chatRegistry := chatnotify.NewRegistry(db, bus, func() bool {
		return settingsSvc.GetBoolDefault(settings.ChatIncludeDetails, true)
	})
	telegramSvc := telegram.New(telegram.Options{
		Settings: settingsSvc,
		Cipher:   secretsCipher,
		Decide: func(ctx context.Context, id, action, decidedBy string) (string, bool, error) {
			// The paired Telegram user is the instrument and the only
			// identity recorded: nothing links a Telegram id to a
			// toolyard user, so no person is attributed. See
			// telegramDecider.
			req, derr := bus.DecideAs(ctx, id, action, telegramDecider(decidedBy))
			if derr != nil {
				if errors.Is(derr, approval.ErrNotPending) {
					return "", true, nil
				}
				return "", false, derr
			}
			return req.Status, false, nil
		},
	})
	chatRegistry.Register(telegramSvc)
	bus.AddNotifier(chatRegistry)
	goroutines.Supervise(ctx, "telegram-poller", 30*time.Second, telegramSvc.RunPoller)
	goroutines.Supervise(ctx, "chat-reconciler", 5*time.Minute, func(ctx context.Context) error {
		return chatRegistry.RunReconciler(ctx, 2*time.Minute)
	})

	// Events Hub. SQLite-backed hot store (always available, independent of
	// ClickHouse). Notifier mirrors the approval push notifier: every event
	// fans out on the realtime hub; events from notify-enabled sources also
	// fire a content-minimal push. Pollers, the lake sink (when CH is up),
	// and retention run under Supervise. The events.* MCP tools give every
	// agent type the same NL activity feed.
	evSvc := events.New(db)
	evSvc.AddNotifierFunc(func(ctx context.Context, ev *events.Event, src *events.Source) {
		hub.Publish(realtime.Event{Type: "event", Data: ev})
		if !src.Notify {
			return
		}
		if len(src.NotifyTypes) > 0 && !containsStr(src.NotifyTypes, ev.Type) {
			return
		}
		user, uerr := idSvc.PrimaryUser(ctx)
		if uerr != nil {
			return
		}
		_ = pushSvc.Notify(ctx, user.ID, map[string]any{
			"title": "toolyard event: " + src.Name,
			"body":  ev.Summary,
			"url":   "/?route=events",
			"tag":   "event-" + ev.ID,
		})
	})
	gw.RegisterEventsTools(evSvc)
	goroutines.Supervise(ctx, "events-poller", 60*time.Second, evSvc.RunPollers)
	if lakeSvc != nil {
		goroutines.Supervise(ctx, "events-lake-sink", 60*time.Second, func(ctx context.Context) error {
			return evSvc.RunLakeSink(ctx, lakeSvc)
		})
	}
	hasLake := lakeSvc != nil
	goroutines.Supervise(ctx, "events-retention", time.Hour, func(ctx context.Context) error {
		return evSvc.RunRetention(ctx,
			func() int { return settingsSvc.GetInt(settings.EventsRetentionDays, 90) },
			func() bool { return hasLake })
	})

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
	hooksSvc := hookspkg.New(db, mpSvc)

	// TEC-481: wing-locked memory-ingestion webhooks for n8n. mpSvc satisfies
	// memwebhook.MemIngester directly; even when MemPalace is disabled the
	// service still manages webhooks (ingestion then returns 503).
	memWebhookSvc := memwebhook.New(db, mpSvc, auditSvc)
	// TEC-482: ingest is asynchronous — this worker drains queued jobs and
	// performs the slow MemPalace embed/index/store off the HTTP request path.
	// It exits on ctx cancellation; in-flight jobs resume on next boot.
	goroutines.Supervise(ctx, "memwebhook-jobs", time.Minute, memWebhookSvc.RunWorker)

	// Notes workspace: a markdown scratchpad agents read/write through the
	// official @modelcontextprotocol/server-filesystem MCP. The upstream is
	// scoped to a single directory so agents can only touch the notes dir,
	// never the rest of the toolyard data root. We launch via `npx -y` so
	// re-runs don't need a global npm install — npm's cache handles repeat
	// boots fast.
	notesMode := strings.ToLower(strings.TrimSpace(*notesFlag))
	if notesMode == "" {
		notesMode = "auto"
	}
	resolvedNotesDir := *notesDir
	if resolvedNotesDir == "" {
		resolvedNotesDir = filepath.Join(*dataDir, "notes")
	}
	switch notesMode {
	case "off":
		log.Printf("notes: disabled (-notes=off)")
	case "on", "auto":
		if err := os.MkdirAll(resolvedNotesDir, 0o750); err != nil {
			if notesMode == "on" {
				return fmt.Errorf("notes: mkdir %s: %w", resolvedNotesDir, err)
			}
			log.Printf("notes: mkdir %s: %v", resolvedNotesDir, err)
			break
		}
		npxPath, lookErr := exec.LookPath("npx")
		if lookErr != nil {
			if notesMode == "on" {
				return fmt.Errorf("notes: npx not on PATH (-notes=on); install Node.js or set -notes=off")
			}
			log.Printf("notes: npx not on PATH; integration inactive (-notes=auto)")
			break
		}
		if _, err := upstreamSvc.UpsertBuiltin(ctx, upstreams.Server{
			Name:      "notes",
			Transport: "stdio",
			Command:   npxPath,
			Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", resolvedNotesDir},
			Enabled:   true,
		}); err != nil {
			log.Printf("notes upstream: %v", err)
		} else {
			log.Printf("notes: connected (dir=%s)", resolvedNotesDir)
		}
	default:
		log.Printf("notes: unknown mode %q; treating as auto", notesMode)
	}

	// Notes service: notes.publish built-in tool + background scanner
	// that keeps mempalace's diary in sync with the on-disk notes/ dir.
	// Constructed regardless of the upstream's status — Publish() and
	// Sync() short-circuit when mempalace isn't registered yet, and the
	// scanner retries on every tick. Lives on the same goroutine
	// lifecycle as the gateway (ctx cancellation stops it cleanly).
	var notesSvc *notespkg.Service
	if notesMode != "off" {
		notesSvc = notespkg.New(notespkg.Options{
			DB:       db,
			Gateway:  gw,
			NotesDir: resolvedNotesDir,
			Interval: *notesSyncEvery,
		})
		gw.RegisterNotesPublish(notesAdapter{notesSvc})
		go notesSvc.StartScanner(ctx)
		log.Printf("notes-sync: scanner started (interval=%s, dir=%s)", *notesSyncEvery, resolvedNotesDir)
	}

	// Skills workspace: sibling to notes. Same filesystem-MCP-upstream
	// recipe (zero custom CRUD), same MemPalace ingest pattern, same
	// dataDir-captures-it-all backup story. Two custom built-in tools on
	// top — skills.publish (validate + write + index) and skills.install
	// (symlink/copy into the user's local Claude install).
	skillsMode := strings.ToLower(strings.TrimSpace(*skillsFlag))
	if skillsMode == "" {
		skillsMode = "auto"
	}
	resolvedSkillsDir := *skillsDir
	if resolvedSkillsDir == "" {
		resolvedSkillsDir = filepath.Join(*dataDir, "skills")
	}
	switch skillsMode {
	case "off":
		log.Printf("skills: disabled (-skills=off)")
	case "on", "auto":
		if err := os.MkdirAll(resolvedSkillsDir, 0o750); err != nil {
			if skillsMode == "on" {
				return fmt.Errorf("skills: mkdir %s: %w", resolvedSkillsDir, err)
			}
			log.Printf("skills: mkdir %s: %v", resolvedSkillsDir, err)
			break
		}
		npxPath, lookErr := exec.LookPath("npx")
		if lookErr != nil {
			if skillsMode == "on" {
				return fmt.Errorf("skills: npx not on PATH (-skills=on); install Node.js or set -skills=off")
			}
			log.Printf("skills: npx not on PATH; integration inactive (-skills=auto)")
			break
		}
		if _, err := upstreamSvc.UpsertBuiltin(ctx, upstreams.Server{
			Name:      "skills",
			Transport: "stdio",
			Command:   npxPath,
			Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", resolvedSkillsDir},
			Enabled:   true,
		}); err != nil {
			log.Printf("skills upstream: %v", err)
		} else {
			log.Printf("skills: connected (dir=%s)", resolvedSkillsDir)
		}
	default:
		log.Printf("skills: unknown mode %q; treating as auto", skillsMode)
	}

	// Same shape as notes: construct the Service regardless of the
	// upstream's status. Publish/Install short-circuit if the filesystem
	// MCP isn't registered yet (falls back to direct disk write), and the
	// scanner retries on every tick.
	var skillsSvc *skillspkg.Service
	if skillsMode != "off" {
		skillsSvc = skillspkg.New(skillspkg.Options{
			DB:              db,
			Gateway:         gw,
			SkillsDir:       resolvedSkillsDir,
			Interval:        *skillsSyncEvery,
			ClaudeSkillsDir: *claudeSkillsDir,
		})
		gw.RegisterSkillsBuiltins(skillsAdapter{skillsSvc})
		go skillsSvc.StartScanner(ctx)
		log.Printf("skills-sync: scanner started (interval=%s, dir=%s)", *skillsSyncEvery, resolvedSkillsDir)
	}

	// Voice ("live call" dashboard panel). Mirrors notes/skills: present a
	// Service object regardless of whether the GEMINI_API_KEY is set, so the
	// /v1/voice/* routes can return a clear 503 rather than 404 when the
	// operator hasn't configured the key yet.
	var voiceSvc *voice.Service
	voiceMode := strings.ToLower(strings.TrimSpace(*voiceFlag))
	if voiceMode != "off" {
		apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
		switch voiceMode {
		case "on":
			if apiKey == "" {
				return errors.New("voice: -voice=on requires GEMINI_API_KEY in the environment")
			}
		case "auto", "":
			// fall through; service still constructed even with no key so
			// routes return a clean 503 instead of 404.
		default:
			log.Printf("voice: unknown mode %q; treating as auto", voiceMode)
		}
		resolvedVoiceDir := *voiceDir
		if resolvedVoiceDir == "" {
			resolvedVoiceDir = filepath.Join(*dataDir, "voice")
		}
		voiceSvc = voice.New(voice.Config{
			APIKey:      apiKey,
			Model:       *voiceModel,
			PersonaPath: filepath.Join(resolvedVoiceDir, "soul.md"),
			// The whole gateway catalog: memory, approvals/events meta
			// tools, lake, and every connected upstream. Calls route
			// through the same policy → approval → audit pipeline as any
			// enrolled agent, attributed to "voice:<user>".
			Tools:           gw,
			Hub:             hub,
			MaxCallDuration: *voiceMaxCall,
			Logger:          logx.For("voice"),
		})
		if err := voiceSvc.SeedPersona(); err != nil {
			log.Printf("voice: seed persona at %s: %v (continuing)", resolvedVoiceDir, err)
		}
		if apiKey == "" {
			log.Printf("voice: GEMINI_API_KEY not set — /v1/voice/ws will return 503 until configured")
		} else {
			log.Printf("voice: enabled (persona=%s)", filepath.Join(resolvedVoiceDir, "soul.md"))
		}
	}

	secOpts := api.SecurityOptions{
		PublicURL:        *publicURL,
		TrustedProxies:   parseCIDRs(*trustedProxies),
		ClerkFrontendAPI: clerkFAPI,
	}

	// REST API + dashboard.
	apiSrv := api.New(ctx, api.Options{
		Identity:                 idSvc,
		Approval:                 bus,
		Audit:                    auditSvc,
		Memory:                   memSvc,
		Push:                     pushSvc,
		Hub:                      hub,
		Gateway:                  gw,
		Upstreams:                upstreamSvc,
		Settings:                 settingsSvc,
		Usage:                    usageSvc,
		Metrics:                  metricsReader,
		MetricsRecorder:          metricsRec,
		AutoApproval:             autoApprover,
		Policy:                   policyEngine,
		OAuth:                    oauthSvc,
		Hooks:                    hooksSvc,
		ClickhouseRuntimeEnvPath: *clickhouseRuntimeEnvPath,
		Mempalace:                mpSvc,
		Notes:                    notesSvc,
		Skills:                   skillsSvc,
		Voice:                    voiceSvc,
		Secrets:                  secretsSvc,
		ChatTelegram:             telegramSvc,
		Events:                   evSvc,
		MemWebhooks:              memWebhookSvc,
		Inbox:                    inboxSvc,
		Snapshots:                inboxSnaps,
		Passkeys:                 passkeySvc,
		IdentityKeys:             identityKeysSvc,
		Guide:                    inboxGuide,
		WebhookMaxBytes:          *webhookMaxBytes,
		SessionKey:               loadOrCreateSessionKey(*dataDir),
		Security:                 secOpts,
		Access:                   accessSvc,
		Clerk:                    clerkClient,
		ClerkOwnerOnly:           *clerkOwnerOnly,
		OwnerEmail:               *ownerEmail,
	})

	mux := http.NewServeMux()
	apiSrv.Routes(mux)

	// Streamable HTTP MCP transport at /mcp. Agents use this if they prefer
	// HTTP over stdio. Auth via Authorization: Bearer <agent-token>, or
	// x-bf-vk: <token> from a proxy that strips Authorization (bkt3's
	// Bifrost proxy sends a person's Beknown key that way); see
	// mcpCredential. The token is verified once per request, by the
	// guard, which also works out who raised the call (agent, owner,
	// client, session) for the audit log. -require-auth-on-mcp
	// (auto-enabled with -public-url) rejects anonymous traffic too.
	mcpAuthn := mcpAuth{verify: idSvc.VerifyAgentToken, requireAuth: *requireAuthMCP, sec: secOpts}
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
		// The guard below verified the token once and put the agent and
		// its raiser on the request context; this hands them to every
		// tool call's context. See actors.go.
		server.WithHTTPContextFunc(mcpAuthn.contextFunc),
	)
	// /mcp gets a larger body limit because tool-call args can be a few MB.
	mux.Handle("/mcp", api.LimitBody(mcpAuthn.guard(streamable), 16<<20))
	mux.Handle("/mcp/", api.LimitBody(mcpAuthn.guard(streamable), 16<<20))

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

	// Dashboard static assets.
	mux.Handle("/", staticHandler())

	// Compose the public-facing handler:
	//   security headers → origin enforcement → API hardening → body cap → role guard → mux
	//
	// HardenAPI is the load-bearing application-layer defense: per-route
	// body caps, anti-CSRF custom-header check on cookie mutations,
	// strict content-type, and per-IP rate limits on the unauthenticated
	// bootstrap routes. It only applies to /v1/* — /mcp keeps its
	// dedicated bearer-token guard above.
	//
	// RoleGuard sits innermost: once a request has passed every transport
	// check, a member's session may only reach the short allowlist of
	// member routes; every other /v1 route is admin-only by default.
	var handler http.Handler = apiSrv.RoleGuard(mux)
	// 4 MiB universal ceiling, with one override: the memory-webhook ingest
	// route (TEC-481) intentionally accepts large n8n payloads, so it gets the
	// configured webhook cap. Without the override this outer ceiling would
	// bind below that cap.
	handler = api.LimitBodyByPath(handler, 4<<20, map[string]int64{
		api.MemoryWebhookIngestPath: *webhookMaxBytes,
	})
	handler = apiSrv.HardenAPI(handler)
	handler = apiSrv.EnforceOriginOnMutations(handler)
	handler = apiSrv.SecurityHeaders(handler)
	// Outermost: recover from panics so one bad handler (or middleware)
	// can't kill the gateway. Wrap LAST so it sees every other layer's
	// panic too.
	handler = api.Recover(handler)

	// Hourly offboarding: Clerk-linked users who left the org get blocked
	// (sessions revoked, agents stopped). No-op when Clerk is off; a Clerk
	// outage changes nothing.
	if clerkClient != nil && !*clerkOwnerOnly {
		apiSrv.StartClerkSync(ctx, *clerkSyncEvery)
		log.Printf("clerk: org membership sync every %s", clerkSyncEvery.String())
	}

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

	if idleTimeout > 0 {
		go func() {
			tick := time.NewTicker(idleTimeout / 2)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if n := gw.SweepIdleStdioUpstreams(idleTimeout); n > 0 {
						rootLog.Info("idle-kill swept",
							"count", n, "threshold", idleTimeout.String())
					}
				}
			}
		}()
	}
	gw.SetMaxLiveUpstreams(*upstreamMaxLive)
	rootLog.Info("upstream pool configured",
		"max_live", *upstreamMaxLive,
		"idle_timeout", idleTimeout.String())

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
		Policy: policy.New(db), Approval: bus,
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

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
	loginHash := assetHash(sub, "login.js")
	rewriteIndex := func(body []byte) []byte {
		out := strings.ReplaceAll(string(body), `src="/app.js"`, `src="/app.js?v=`+appHash+`"`)
		out = strings.ReplaceAll(out, `href="/style.css"`, `href="/style.css?v=`+cssHash+`"`)
		return []byte(out)
	}
	rewriteLogin := func(body []byte) []byte {
		out := strings.ReplaceAll(string(body), `src="/login.js"`, `src="/login.js?v=`+loginHash+`"`)
		out = strings.ReplaceAll(out, `href="/style.css"`, `href="/style.css?v=`+cssHash+`"`)
		return []byte(out)
	}
	// serveDocument writes an HTML document with the revalidate-always
	// caching the SPA shell uses: no-cache + a content ETag so reloads are
	// a cheap 304 but a redeploy is picked up immediately.
	serveDocument := func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		etag := contentETag(body)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
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
		// The login page is its own document, not the SPA shell: it alone
		// loads Clerk and gets the Clerk-compatible CSP (see
		// api.SecurityHeaders). Any query string (?signout=1) rides along.
		// A build whose embed lacks login.html answers 404 here rather
		// than falling through to the SPA.
		if r.URL.Path == "/login" || r.URL.Path == "/login.html" {
			raw, err := fs.ReadFile(dashboard.Assets, "login.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			serveDocument(w, r, rewriteLogin(raw))
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
			serveDocument(w, r, rewriteIndex(raw))
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
// mcpCredential is the agent token an /mcp request presents: the
// Authorization bearer when there is one, else the x-bf-vk header (the
// Bifrost virtual-key slot: bkt3's proxy strips Authorization and sends a
// person's Beknown key there). Authorization always wins when both are
// present, so a stale bearer never falls through to a different key. ok
// is false when neither carries a token.
func mcpCredential(r *http.Request) (token string, ok bool) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer "), true
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		// A non-bearer Authorization is not ours: never read x-bf-vk
		// behind it.
		return "", false
	}
	if vk := strings.TrimSpace(r.Header.Get("x-bf-vk")); vk != "" {
		return vk, true
	}
	return "", false
}

// registryUpstreams names the servers where identity key fingerprints are
// registered: those whose identity setting has register = true.
func registryUpstreams(servers []upstreams.Server) []string {
	var out []string
	for _, sv := range servers {
		if sv.Identity != nil && sv.Identity.Register {
			out = append(out, sv.Name)
		}
	}
	return out
}

type identityResolver struct{ id *identity.Service }

func (r identityResolver) PrimaryUserID(ctx context.Context) (string, error) {
	u, err := r.id.PrimaryUser(ctx)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// notesAdapter bridges *notes.Service's typed return to the gateway's
// `(any, error)` NotesPublisher contract — the interface is declared
// without importing internal/notes to avoid a dependency cycle, so we
// flatten the typed value here.
type notesAdapter struct{ s *notespkg.Service }

func (a notesAdapter) Publish(ctx context.Context, agentID, path, content, topic string) (any, error) {
	return a.s.Publish(ctx, agentID, path, content, topic)
}
func (a notesAdapter) NotesDir() string { return a.s.NotesDir() }

// skillsAdapter bridges *skills.Service to the gateway's SkillsPublisher
// contract — flattens the typed return into `(any, error)` so
// internal/gateway doesn't need to import internal/skills.
type skillsAdapter struct{ s *skillspkg.Service }

func (a skillsAdapter) Publish(ctx context.Context, agentID, slug, skillMD, agentsYAML string, scripts map[string]string, topic string) (any, error) {
	return a.s.Publish(ctx, agentID, slug, skillMD, agentsYAML, scripts, topic)
}
func (a skillsAdapter) Install(ctx context.Context, agentID, slug, mode, target string) (any, error) {
	return a.s.Install(ctx, agentID, slug, mode, target)
}
func (a skillsAdapter) List(ctx context.Context, tagFilter []string) (any, error) {
	return a.s.List(ctx, tagFilter)
}
func (a skillsAdapter) ListTags(ctx context.Context) (any, error) {
	return a.s.ListTags(ctx)
}
func (a skillsAdapter) Get(ctx context.Context, slug string, includeFiles bool) (any, error) {
	return a.s.Get(ctx, slug, includeFiles)
}
func (a skillsAdapter) Bundle(ctx context.Context, slugs []string) (any, error) {
	return a.s.Bundle(ctx, slugs)
}
func (a skillsAdapter) SkillsDir() string { return a.s.SkillsDir() }

// silence unused-import warnings in case build tags drop something.
var _ = net.Listen

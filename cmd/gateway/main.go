// Command gateway is the toolyard MCP gateway / dashboard binary.
//
// Subcommands:
//
//	toolyard serve      run the gateway (stdio MCP + HTTP API + dashboard)
//	toolyard exchange   exchange an enrollment code for a long-lived agent token
//	toolyard probe      dry-run a tool against an upstream MCP server
//	toolyard healthcheck  check a liveness or readiness probe (container HEALTHCHECK)
//	toolyard keys-check   read-only check that every master key would load
//	toolyard keys generate  write fresh master keys (stdout, a file, or all four in a directory)
//	toolyard db migrate|copy|verify  apply schema migrations; move SQLite to Postgres
//	toolyard version    print version
package main

import (
	"context"
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
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/api"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/auditlink"
	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
	"github.com/tusharbhardwaj/toolyard/internal/chatnotify"
	"github.com/tusharbhardwaj/toolyard/internal/chatnotify/telegram"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
	"github.com/tusharbhardwaj/toolyard/internal/crashdump"
	"github.com/tusharbhardwaj/toolyard/internal/events"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	hookspkg "github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	notespkg "github.com/tusharbhardwaj/toolyard/internal/notes"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/ops"
	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
	"github.com/tusharbhardwaj/toolyard/internal/passkey"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/pricing"
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
	case "operator-token":
		err = runOperatorToken(args)
	case "healthcheck":
		err = runHealthcheck(args)
	case "keys-check":
		err = runKeysCheck(args)
	case "keys":
		err = runKeys(args)
	case "db":
		err = runDB(args)
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
		if errors.Is(err, keyguard.ErrConfig) {
			// EX_CONFIG: a key or credential is missing, unreadable or
			// wrong. Restarting will not help (RestartPreventExitStatus=78).
			os.Exit(78)
		}
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
// Each variable may instead name a file in <NAME>_FILE (Docker and systemd
// secrets); setting both is an error.
func clerkFromEnv(publicURL, ownerEmail string, ownerOnly bool) (*clerk.Client, string, error) {
	vars := []struct{ key, val string }{
		{key: "TOOLYARD_CLERK_SECRET_KEY"},
		{key: "TOOLYARD_CLERK_PUBLISHABLE_KEY"},
		{key: "TOOLYARD_CLERK_ORGANIZATION_ID"},
	}
	for i := range vars {
		v, err := keyguard.EnvOrFile(vars[i].key)
		if err != nil {
			return nil, "", fmt.Errorf("clerk: %w", err)
		}
		vars[i].val = v
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
              -migrate auto|off: apply pending schema migrations at startup (auto), or apply none and
              refuse to start (exit 78) unless the database is at this binary's level (off)
              -db-connect-timeout 60s: keep trying a Postgres server it cannot reach (0 = one attempt)
  exchange    swap an enrollment code for an agent token
  probe       dry-run a tool against an upstream MCP server
  operator-token  create | list | revoke CLI operator tokens (local, from -data)
  healthcheck check a probe: -url <probe URL> -expect ready|live [-token-file <path>] (exit 0 or 1)
  keys-check  read-only check that every master key would load: -data <dir> [-credentials-dir <dir>] (exit 0 or 78)
  keys        generate -name identity|oauth|secrets|session [-out <file>]: one fresh key to stdout or a new file
              generate -all -dir <dir>: all four as <name>.key (mode 0600; never overwrites a file)
  db          migrate [-dry-run] [-db-connect-timeout 60s]: apply pending schema migrations to the database serve uses
              (exit 0, 78 on a config error)
              copy | verify: move a stopped SQLite installation to Postgres and prove the copy (-from-data <dir>)
  version     print version

Environment:
  Every serve flag can also be set as TOOLYARD_<FLAG>: the flag name upper-cased, with - as _
  (-public-url is TOOLYARD_PUBLIC_URL, -db-max-conns is TOOLYARD_DB_MAX_CONNS, -migrate is
  TOOLYARD_MIGRATE). The command line wins; an empty variable counts as unset. They may also come
  from toolyard.env in the credentials directory, except TOOLYARD_CREDENTIALS_DIR. keys-check,
  operator-token and db migrate read TOOLYARD_DATA (or TOOLYARD_DATA_DIR) and TOOLYARD_CREDENTIALS_DIR,
  keys-check also TOOLYARD_KEYS; healthcheck reads TOOLYARD_HEALTHCHECK_URL, _EXPECT and _TOKEN_FILE.
  -inbox-reset-passkeys is command line only. serve warns about a TOOLYARD_ variable that sets no
  flag and that nothing reads (a misspelt name). Secrets go in files, never in variables.

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
	connectAZPFlag := fs.String("connect-azp", "", "comma-separated bkt3 (T3 Code) browser origins, e.g. https://stagebkt3.dev.beknown.live, whose Clerk session tokens POST /v1/connect/t3 swaps for the person's agent token. https only (http just on loopback). Needs Clerk sign-in; must not include -public-url's origin. Empty disables the endpoint (404).")
	clerkSyncEvery := fs.Duration("clerk-sync-interval", time.Hour, "how often to list the Clerk organisation's members and block users who left (sessions revoked, agents stopped). Only runs when the TOOLYARD_CLERK_* env vars are set.")
	trustedProxies := fs.String("trusted-proxy", "", "comma-separated CIDRs to trust for X-Forwarded-* headers (e.g. 127.0.0.1/32,::1/128,10.0.0.0/8)")
	// TEC-1626 keys and ops listener
	keysFlag := fs.String("keys", string(keyguard.CreateIfFresh), "master keys: create-if-fresh (create a missing key only for a new, empty database) | require (never create; every key must be provided)")
	// TEC-1638: migrations run in the deploy (toolyard db migrate), not at start.
	migrateFlag := fs.String("migrate", string(migrateAuto), "schema migrations at startup: auto (apply pending migrations; on Postgres under an advisory lock) | off (apply none; refuse to start, exit 78, unless the database records every migration this binary knows: run toolyard db migrate first)")
	credentialsDir := fs.String("credentials-dir", os.Getenv("TOOLYARD_CREDENTIALS_DIR"), "directory searched for key files (identity.key, oauth.key, secrets.key, session.key), toolyard.env and database-url after $CREDENTIALS_DIRECTORY and before -data, e.g. /run/secrets. Defaults to $TOOLYARD_CREDENTIALS_DIR.")
	dbConnectTimeout := fs.Duration("db-connect-timeout", defaultDBConnectTimeout, dbConnectTimeoutUsage)
	dbMaxConns := fs.Int("db-max-conns", store.DefaultPostgresMaxConns, "Postgres only: the most connections the database pool opens. Postgres is selected by $TOOLYARD_DATABASE_URL or a database-url file in the credentials directory; otherwise toolyard uses SQLite at <data>/toolyard.db, always with one connection.")
	opsAddr := fs.String("ops-addr", "", "comma-separated listen addresses for the ops listener (/metrics, /v1/livez, /v1/readyz), e.g. 127.0.0.1:9090. Empty = off. A non-private address needs -ops-token-file.")
	opsTokenFile := fs.String("ops-token-file", "", "file holding the bearer token (16+ characters) the ops listener then requires on every request, from every peer, loopback included")
	// Still accepted so service units that pass it keep starting: the flag
	// set exits on an unknown flag. toolyard no longer runs a ClickHouse lake.
	clickhouseRuntimeEnvPath := fs.String("clickhouse-runtime-env", "", "DEPRECATED: ignored. Kept so existing service units keep starting.")
	inboxResetPasskeys := fs.Bool("inbox-reset-passkeys", false, "delete every registered passkey at startup (recovery when the owner has lost all their devices), then continue normally")
	inboxFetchPrivate := fs.Bool("inbox-fetch-private-networks", false, "let the inbox copy attachment links that point at loopback or private-network addresses (e.g. evidence hosted on your LAN). Off by default: agent-chosen URLs could otherwise reach internal services.")
	requireAuthMCP := fs.Bool("require-auth-on-mcp", false, "reject anonymous /mcp calls (no Authorization header). Auto-enabled when -public-url is set.")
	upstreamProtocolFlag := fs.String("upstream-protocol", upstreamProtocolLegacy, "MCP version toward upstream servers: legacy (2025-11-25 for every upstream) | auto (a shared http upstream that offers 2026-07-28 is called statelessly, one POST per call, outside the live pool and the idle sweep; stdio and per_user stay legacy)")
	statelessMCP := fs.Bool("stateless-mcp", false, "skip MCP session-ID tracking. Every request stands alone — no server-initiated notifications, but agents that don't auto-reconnect on session-invalid (e.g., hermes) survive a toolyard restart without manual intervention.")
	noStdioUpstreams := fs.Bool("no-stdio-upstreams", false, "refuse to start any stdio (subprocess) MCP upstream. Use when the dashboard is exposed publicly so a compromised session can't spawn arbitrary commands.")
	envDenylistFlag := fs.String("upstream-env-denylist", "LD_PRELOAD,LD_LIBRARY_PATH,DYLD_INSERT_LIBRARIES,DYLD_LIBRARY_PATH,PATH", "comma-separated env var keys forbidden in upstream stdio configs")
	upstreamCallTimeout := fs.Duration("upstream-call-timeout", 120*time.Second, "per-tool-call deadline applied to every dispatch (built-in, fixture, and external upstreams). 0 disables the cap. Without it, a hung upstream pins a goroutine indefinitely and queues every other caller behind it.")
	// TEC-1626 drain
	drainDelay := fs.Duration("drain-delay", defaultDrainDelay, "on SIGTERM/SIGINT, how long readiness reports 503 (and new tool calls get a retryable refusal) before streams and waits close, so the load balancer stops sending traffic first")
	drainTimeout := fs.Duration("drain-timeout", defaultDrainTimeout, "deadline, from the start of the drain (delay included), for open HTTP requests and in-flight executions to finish. Work still running then is cancelled and recorded outcome_unknown. Keep the unit's TimeoutStopSec above this plus ~10s.")
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
	catalogRefresh := fs.Duration("catalog-refresh", 15*time.Minute, "read the tool list of every live shared upstream again this often and apply what changed (added, changed and removed tools; clients get tools/list_changed). Suspended upstreams are not dialled for it, and the read does not count as use for -upstream-idle-timeout. 0 disables.")
	upstreamIdleTimeout := fs.Duration("upstream-idle-timeout", 0, "kill upstream MCP connections idle for this long; transparently re-dial on next call. 0 disables. Recommended: 15m. Covers both stdio (kills subprocess) and http (closes client). Reduces RSS + FDs when no agents are active.")
	upstreamMaxLive := fs.Int("upstream-max-live", 8, "max simultaneously-live upstream connections. When the cap is hit, the least-recently-used upstream is suspended (catalog stays populated, transparently resumed on next call). 0 = unbounded.")
	perUserMaxLive := fs.Int("per-user-max-live", 64, "max simultaneously-live per-person connections to servers where each person signs in (auth_mode per_user). Their own pool: they never evict a shared server and a shared server never evicts them. LRU-suspended past the cap, resumed on next call. 0 = unbounded.")
	logLevel := fs.String("log-level", "info", "log level: debug | info | warn | error")
	logFormat := fs.String("log-format", "json", "log format: json | text. Text is friendlier in a terminal; json is what journalctl + jq want.")
	// TEC-1630 high availability
	instanceID := fs.String("instance-id", "", "name of this replica in work leases and logs when several replicas share one Postgres database (e.g. the pod name). Empty = <hostname>-<random>, new at every start.")
	_ = fs.Parse(argv)

	// TEC-1638: every flag not given on the command line may come from
	// TOOLYARD_<FLAG> (envconfig.go): the process environment first, then,
	// after toolyard.env in $CREDENTIALS_DIRECTORY or -credentials-dir is
	// loaded (TEC-1626; it fills variables that are still unset), the
	// variables it added, for every flag but -credentials-dir (the file is
	// found through it). Logging is not set up yet: report below.
	envRep, err := loadFlagEnv(fs, processEnv(), credentialsDir)
	if err != nil {
		return err
	}
	migrate, err := parseMigrateMode(*migrateFlag)
	if err != nil {
		return err
	}
	upstreamProtocol, err := parseUpstreamProtocol(*upstreamProtocolFlag)
	if err != nil {
		return err
	}
	if err := checkConnectTimeout(*dbConnectTimeout); err != nil {
		return err
	}
	// A TOOLYARD_ variable that sets no flag and that nothing reads is most
	// likely a misspelt name (TOOLYARD_KEY for TOOLYARD_KEYS): say so. The
	// environment now holds toolyard.env's variables too.
	envRep.warnings = append(envRep.warnings, unknownEnvWarnings(fs, os.Environ())...)

	// Initialize structured logging first so every subsequent log line
	// flows through slog. The stdlib `log` package gets re-routed too via
	// logx.Bridge() so existing log.Printf callers don't need to migrate
	// before everything benefits.
	logx.Init(logx.Format(*logFormat), logx.ParseLevel(*logLevel))
	log.SetFlags(0)
	log.SetOutput(logx.Bridge())
	rootLog := logx.For("gateway")
	if len(envRep.loaded) > 0 {
		log.Printf("toolyard: loaded environment from %s", strings.Join(envRep.loaded, ", "))
	}
	if len(envRep.variables) > 0 {
		// Names only: values are never logged.
		log.Printf("toolyard: flags set from the environment: %s", strings.Join(envRep.variables, ", "))
	}
	for _, w := range envRep.warnings {
		rootLog.Warn(w)
	}

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
	if strings.TrimSpace(*clickhouseRuntimeEnvPath) != "" {
		rootLog.Warn("--clickhouse-runtime-env is deprecated and ignored")
	}

	// Public mode auto-enables matching safeguards.
	if *publicURL != "" {
		*requireAuthMCP = true
	}

	// Clerk (Google) sign-in: on only when all three TOOLYARD_CLERK_* env
	// vars are set (they come from the systemd EnvironmentFile, .env or
	// toolyard.env, loaded above, never argv). A partial set, or Clerk
	// without -public-url, refuses to start rather than run
	// half-configured.
	clerkClient, clerkFAPI, cerr := clerkFromEnv(*publicURL, *ownerEmail, *clerkOwnerOnly)
	if cerr != nil {
		return cerr
	}
	if clerkClient != nil {
		log.Printf("clerk: sign-in enabled (frontend api %s; owner email %s)", clerkFAPI, orDefault(*ownerEmail, "unset"))
	}
	connectOrigins, connErr := connectAZP(*connectAZPFlag, *publicURL, clerkClient != nil)
	if connErr != nil {
		return connErr
	}
	if len(connectOrigins) > 0 {
		log.Printf("connect: POST /v1/connect/t3 accepts Clerk tokens minted for %s", strings.Join(connectOrigins, ", "))
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	// Tighten any pre-existing data dir so other local users can't read keys.
	_ = os.Chmod(*dataDir, 0o700)
	// TEC-1635: $TOOLYARD_DATABASE_URL or a database-url credential
	// selects Postgres; otherwise SQLite at <data>/toolyard.db.
	dbChoice, err := resolveDatabase(*dataDir, *credentialsDir, os.Getenv)
	if err != nil {
		return err
	}
	// TEC-1638: -migrate=off means the deploy ran `toolyard db migrate`:
	// open without migrating and refuse (exit 78) a database behind this
	// binary. Postgres connection failures are retried for
	// -db-connect-timeout (a restart during an Aurora failover).
	db, unknownMigrations, err := openServeDatabase(context.Background(), dbChoice, *dbMaxConns, migrate, *dbConnectTimeout, log.Printf)
	if err != nil {
		return fmt.Errorf("open db (%s): %w", dbChoice.describe(), err)
	}
	// TEC-1626 drain: the ordered shutdown closes the database last
	// (lifecycle.go); this deferred call only acts on an early return.
	closeDB := closeOnce("database", closeTimeout, db.CloseContext)
	defer closeDB()
	log.Printf("toolyard: db %s (selected by %s; -migrate=%s)", dbChoice.describe(), dbChoice.Source, migrate)
	if len(unknownMigrations) > 0 {
		rootLog.Warn("the database records migrations this binary does not know: a newer toolyard migrated it",
			"count", len(unknownMigrations), "migrations", nameList(unknownMigrations, 5))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// TEC-1630: leader leases, cross-node messages and locks. On Postgres
	// several replicas may share the database; on SQLite this is a
	// single-node no-op.
	clusterSvc, err := cluster.New(ctx, db, cluster.Options{InstanceID: *instanceID})
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	closeCluster := closeOnce("cluster", closeTimeout, func(context.Context) error { return clusterSvc.Close() })
	defer closeCluster()
	if clusterSvc.MultiNode() {
		log.Printf("toolyard: cluster instance %s (replicas may share this database)", clusterSvc.InstanceID())
	}

	idSvc := identity.New(db)
	auditSvc := audit.New(db)
	memSvc := memory.New(db)
	// Roles and per-user tool-group grants. The gateway asks it on every
	// tool list/call; the Users admin API writes through it.
	accessSvc := access.New(db)

	bus, err := approval.New(ctx, db)
	if err != nil {
		return err
	}
	bus.SetTTL(*approvalTTL)
	bus.SetCluster(clusterSvc)
	log.Printf("toolyard: approval TTL %s (in-line wait %s)", bus.TTL(), *inLineWait)
	// Every approval event (create, decide, expire, cancel, executed)
	// leaves an audit row naming who raised the call and who decided it,
	// whichever path the decision took.
	bus.AddNotifier(auditlink.Notifier(auditSvc))
	leaderLoop(ctx, clusterSvc, approval.SweeperLoop, time.Minute, func(ctx context.Context) { bus.RunSweeper(ctx, 30*time.Second) })
	// Every replica renews the leases of the legacy executions it runs.
	goroutines.Loop(ctx, approval.LeaseRenewalLoop, time.Minute, bus.RunLeaseRenewal)

	pushSvc, err := push.New(ctx, db, *pushSubject)
	if err != nil {
		return err
	}

	hub := realtime.NewHub()
	registerRealtimeTypes(hub)
	hub.SetCluster(ctx, clusterSvc)

	// Hook approval lifecycle -> push + realtime fan-out.
	bus.AddNotifierFunc(func(ctx context.Context, req *approval.Request, eventType string) {
		hub.Publish(realtime.Event{Type: "approval", Data: req})
		if eventType == "approval.create" {
			user, err := idSvc.PrimaryUser(ctx)
			if uid := req.PersonalOwner(); uid != "" {
				user, err = idSvc.GetUserByID(ctx, uid)
			}
			if err != nil {
				return
			}
			// Don't put the operator's reason text in the push payload.
			// Web Push payloads transit (and may briefly cache on)
			// third-party push services; keeping the body content-free
			// minimizes what a key compromise could reveal. The dashboard
			// fills in details when the user opens the approval card.
			n := push.Notification{
				Title:    "toolyard Inbox decision needed",
				Body:     fmt.Sprintf("%s · %s — tap to review", req.UpstreamName, req.ToolName),
				URL:      "/#inbox/" + req.ID,
				Approval: req.ID,
				Tag:      req.ID,
			}
			// Legacy calls keep their original execution mode, but all
			// permission review happens in Inbox. Do not mint an approval
			// token for a second notification decision interface.
			payload := map[string]any{
				"kind":        "inbox",
				"title":       n.Title,
				"body":        n.Body,
				"url":         n.URL,
				"approval_id": req.ID,
				"tag":         req.ID,
			}
			_ = pushSvc.Notify(ctx, user.ID, payload)
		}
	})
	// Audit log -> realtime feed. Every replica runs it: the audit logger
	// fans out in process, and the hub carries each replica's events to the
	// others' dashboard streams.
	goroutines.Loop(ctx, loopAuditFeed, time.Minute, func(ctx context.Context) {
		ch, unsubscribe := auditSvc.Subscribe()
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				hub.Publish(realtime.Event{Type: "audit", Data: ev})
			}
		}
	})

	settingsSvc, err := settings.New(ctx, db)
	if err != nil {
		return err
	}
	settingsSvc.SetCluster(clusterSvc)

	usageSvc := usagepkg.New(db)
	vis := visibility.New(settingsSvc, usageSvc)

	metricsRec := metrics.New(db)
	closeMetrics := closeOnce("metrics", closeTimeout, metricsRec.Close)
	defer closeMetrics()
	metricsReader := metrics.NewReader(db)
	priceCatalog := pricing.New(filepath.Join(*dataDir, "model-prices.json"))
	// Every replica refreshes its own price catalog: it is held in memory
	// (and a file in -data) per process, and the refresh only reads a
	// public price list.
	goroutines.Loop(ctx, loopPricing, time.Hour, priceCatalog.Run)

	autoApprover := autoapproval.New(db, metricsReader, settingsSvc)
	autoApprover.SetCluster(clusterSvc)
	bus.SetAutoApprover(autoApprover)
	leaderLoop(ctx, clusterSvc, loopAutoApproval, time.Hour, func(ctx context.Context) { autoApprover.RunProposer(ctx, time.Hour) })

	// Background analytics jobs: anomaly detection, reason quality
	// scoring, retention compaction. All best-effort — failures log and
	// continue.
	zScore := settingsSvc.GetFloat(settings.AnomalyRateZScore, 3)
	leaderLoop(ctx, clusterSvc, loopAnomaly, time.Hour, metrics.NewAnomalyDetector(metricsReader, zScore).RunForever)
	leaderLoop(ctx, clusterSvc, loopReasonScorer, time.Hour, func(ctx context.Context) { runReasonScorer(ctx, metricsReader) })
	leaderLoop(ctx, clusterSvc, loopRetention, time.Hour, func(ctx context.Context) { runRetentionCompactor(ctx, metricsReader, settingsSvc) })

	// Shared policy engine: the gateway evaluates with it, the API exposes
	// CRUD on the same instance so cache updates are seen immediately.
	// Unreadable rules stop the start: serving without them would drop
	// every explicit deny.
	policyEngine, err := policy.Load(ctx, db)
	if err != nil {
		return fmt.Errorf("policy: load rules: %w", err)
	}
	// A rule written on any replica reaches this one's cache at once.
	policyEngine.SetCluster(clusterSvc)
	log.Printf("policy: loaded %d rules", len(policyEngine.List()))

	// TEC-1626: every master key loads here, before anything uses one.
	// Each is looked up in $CREDENTIALS_DIRECTORY, -credentials-dir, then
	// -data, and must open its canary in this database. A missing key is
	// created only for a new database (-keys=create-if-fresh); the session
	// key alone may be recreated (everyone signs in again). Any key
	// problem stops the start with exit code 78 (keyguard.ErrConfig).
	keysMode, err := keyguard.ParseMode(*keysFlag)
	if err != nil {
		return err
	}
	masterKeys, err := keyguard.LoadAll(ctx, keyguard.Config{
		DB:             db.DB,
		Dialect:        db.Dialect,
		DataDir:        *dataDir,
		CredentialsDir: *credentialsDir,
		Mode:           keysMode,
	}, keyguard.ServeKeys())
	if err != nil {
		return err
	}
	ops.Default.MarkKeysVerified()
	// Ops listener (-ops-addr): /metrics and the probes on a private
	// address. It is up from here, so a slow start reads "starting".
	probes := ops.NewProber(ops.Default, db.PingContext)
	opsmetrics.SetBuildInfo(version)
	opsmetrics.SetDBPinger(db.PingContext)
	if _, err := ops.Listen(ops.ListenerConfig{
		Addrs:     *opsAddr,
		TokenFile: *opsTokenFile,
		Handler:   ops.Mux(opsmetrics.Handler(), probes),
	}); err != nil {
		return err
	}

	// Per-person identity keys ("Beknown keys"). Their own identity.key
	// (blast-radius partitioning, as oauth.key and secrets.key): the raw
	// keys are sealed under it so they can be forwarded to upstreams that
	// name an identity header. The gateway asks this service for the
	// caller's key on every such call; the registry lister and the
	// internal caller are wired below once the upstreams service exists.
	identityKey := masterKeys[keyguard.IdentityKey]
	identityCipher, err := sealbox.NewCipher(identityKey)
	if err != nil {
		return fmt.Errorf("identity cipher: %w", err)
	}
	identityKeysSvc := identitykeys.New(db, identityCipher)
	identityKeysSvc.SetAudit(auditSvc)

	// Code-mode worker sandbox: TOOLYARD_CODEMODE_SANDBOX=auto (default),
	// off or required. A value it does not know stops the gateway rather
	// than fall back to a weaker mode than the operator asked for.
	sandboxMode, err := codemode.ParseSandboxMode(os.Getenv("TOOLYARD_CODEMODE_SANDBOX"))
	if err != nil {
		return fmt.Errorf("TOOLYARD_CODEMODE_SANDBOX: %w", err)
	}
	gw := gateway.New(gateway.Options{
		Name:                "toolyard",
		Version:             version,
		Policy:              policyEngine,
		Approval:            bus,
		Audit:               auditSvc,
		Hub:                 hub,
		Memory:              memSvc,
		InLineWait:          *inLineWait,
		Visibility:          vis,
		Usage:               usageSvc,
		Metrics:             metricsRec,
		MetricsReader:       metricsReader,
		Surface:             vis,
		UpstreamCallTimeout: *upstreamCallTimeout,
		Access:              accessSvc,
		Identity:            identityKeysSvc,
		Owners:              accessSvc,
		PublicURL:           *publicURL,
		UpstreamProtocol:    upstreamProtocol,
	})
	// The kernel probe for the code-mode sandbox runs now, off the
	// request path.
	gw.SetCodeModeSandbox(sandboxMode)
	gw.SetCluster(clusterSvc)
	goroutines.Go(goCodemodeProbe, func() { codemode.ProbeSandbox() })
	gw.RegisterBuiltins()
	closeGateway := closeOnce("gateway", closeTimeout, func(context.Context) error { return gw.Close() })
	defer closeGateway()
	identityKeysSvc.SetCaller(gw)
	// policies.set by an agent disables a tool's learned auto-approval
	// rule on ask/deny, as the dashboard's policy editor does.
	gw.SetAutoApproval(autoApprover)

	// Auto-execute on approve: when the human (or an auto-rule) flips
	// an approval to allowed, the bus invokes Gateway.Execute on a
	// background goroutine, which runs the persisted tool args and
	// writes the result back via bus.SetResult. The agent collects the
	// result through tools.poll_approval / tools.wait_for_approval —
	// no need to re-call the original tool.
	bus.SetExecutor(gw)

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
	var geminiInboxVoice inbox.Voice
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		if v, verr := inbox.NewGeminiVoice(ctx, key, os.Getenv("TOOLYARD_INBOX_VOICE_MODEL"), func() string {
			return settingsSvc.GetString(settings.InboxVoiceName, "")
		}); verr != nil {
			log.Printf("inbox: voice notes unavailable: %v", verr)
		} else {
			geminiInboxVoice = v
		}
	}
	inboxVoice := &inbox.VoiceSelector{
		Provider: func() string { return settingsSvc.GetString(settings.InboxVoiceProvider, "gemini") },
		Gemini:   geminiInboxVoice,
		ElevenLabs: inbox.NewElevenLabsVoice(
			func() string {
				return settingsSvc.GetString(settings.ElevenLabsAPIKey, os.Getenv("ELEVENLABS_API_KEY"))
			},
			func() string { return settingsSvc.GetString(settings.InboxElevenLabsVoiceID, "") },
			func() string {
				return settingsSvc.GetString(settings.InboxElevenLabsModel, os.Getenv("ELEVENLABS_TTS_MODEL"))
			}),
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
		Cluster:           clusterSvc,
		LegacyDecided:     bus.DispatchCommitted,
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
	// TEC-1626: a legacy sync problem no longer stops the start. Malformed
	// rows are skipped and logged at ERROR, the rest is committed, and
	// the supervised loop below tries again (LegacySyncPass logs and
	// counts every outcome).
	_ = inboxSvc.LegacySyncPass(ctx)
	bus.LegacyDecision = func(ctx context.Context, id, action string, d actor.Decider) (*approval.Request, error) {
		_, err := inboxSvc.DecideLegacy(ctx, id, action, d)
		if err != nil {
			if errors.Is(err, inbox.ErrNotFound) {
				return nil, approval.ErrNotFound
			}
			if errors.Is(err, inbox.ErrNotPending) || errors.Is(err, inbox.ErrConflict) {
				old, _ := bus.Get(ctx, id)
				return old, approval.ErrNotPending
			}
			return nil, err
		}
		return bus.Get(ctx, id)
	}
	// Approval events only signal the one supervised sync loop: no sync
	// on the request path, one pass at a time, signals before the next
	// pass coalesce, and a pass every LegacySyncInterval regardless.
	bus.AddNotifierFunc(func(context.Context, *approval.Request, string) {
		inboxSvc.RequestClusterLegacySync()
	})
	leaderLoop(ctx, clusterSvc, inbox.LegacySyncLoop, time.Minute, func(ctx context.Context) {
		inboxSvc.RunLegacySync(ctx, inbox.LegacySyncInterval)
	})
	// Every replica renews the leases of the Inbox executions it runs.
	goroutines.Loop(ctx, inbox.LeaseRenewalLoop, time.Minute, inboxSvc.RunLeaseRenewal)
	closeInbox := closeOnce("inbox", closeTimeout, func(context.Context) error {
		inboxSvc.Close(closeTimeout * 3 / 4) // logs its own warning first
		return nil
	})
	defer closeInbox()
	if n := inboxSvc.RecheckUnchecked(ctx); n > 0 {
		log.Printf("inbox: re-running background checks for %d request(s)", n)
	}
	inboxGuide := inbox.NewGuide(func() string { return settingsSvc.GetString(settings.InboxHostingNote, "") })
	gw.SetInbox(inboxSvc, inboxGuide, func() string {
		return settingsSvc.GetString(settings.ApprovalMode, settings.ApprovalModeExecute)
	})
	gw.RegisterInboxTools()
	log.Printf("toolyard: inbox ready (approval_mode=%s, judge=%v)",
		settingsSvc.GetString(settings.ApprovalMode, settings.ApprovalModeExecute), inboxJudge != nil)

	// Code mode over REST (TEC-1627): runs outlive their request, so the
	// runs a stopped process left queued or running are settled before the
	// routes serve (a running one is outcome_unknown: a call may have run).
	codeRuns := coderuns.New(db, coderuns.Options{Engine: coderuns.GatewayEngine(gw), Grants: inboxSvc})
	codeRuns.SetCluster(clusterSvc)
	if failed, unknown, err := codeRuns.Recover(ctx); err != nil {
		log.Printf("code runs: recover: %v", err)
	} else if failed+unknown > 0 {
		log.Printf("code runs: %d interrupted run(s) failed, %d marked outcome_unknown", failed, unknown)
	}
	leaderLoop(ctx, clusterSvc, loopSessionPurger, time.Hour, func(ctx context.Context) { runSessionPurger(ctx, idSvc, codeRuns) })
	// Every replica renews the leases of the code runs it runs.
	goroutines.Loop(ctx, coderuns.LeaseRenewalLoop, time.Minute, codeRuns.RunLeaseRenewal)
	// The leader settles the work of replicas that stopped (multi-node only).
	leaderLoop(ctx, clusterSvc, loopWorkRecovery, time.Minute, func(ctx context.Context) {
		runWorkRecovery(ctx, clusterSvc, inboxSvc, bus, codeRuns)
	})

	upstreamSvc := upstreams.New(db, gw)
	// Servers saved on another replica are connected here too.
	upstreamSvc.SetCluster(ctx, clusterSvc)
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
	leaderLoop(ctx, clusterSvc, loopKeyRemoval, time.Hour, func(ctx context.Context) {
		identityKeysSvc.StartRemovalRetry(ctx, identitykeys.DefaultRetryInterval)
		<-ctx.Done()
	})
	if *noStdioUpstreams {
		log.Printf("toolyard: stdio upstreams disabled (--no-stdio-upstreams)")
	}

	// Wire the OAuth service. Master key lives in <data-dir>/oauth.key
	// (separate from session.key so leaking the cookie HMAC doesn't expose
	// every upstream's refresh token). The hub adapter forwards the
	// service's small set of event names ("mcp_oauth_done", etc.) onto the
	// dashboard's SSE feed.
	oauthKey := masterKeys[keyguard.OAuthKey] // loaded and checked above
	oauthCipher, err := oauth.NewCipher(oauthKey)
	if err != nil {
		return fmt.Errorf("oauth cipher: %w", err)
	}
	oauthSvc := oauth.New(db, oauthCipher,
		hubEventBus{hub: hub},
		pushSvc,
		identityResolver{id: idSvc},
	)
	oauthSvc.SetCluster(clusterSvc)
	upstreamSvc.SetAuth(oauthSvc)
	// Per-user sign-in: each person's bearer on their own connection, and
	// a token the upstream or IdP rejects closes only that person's
	// connection.
	upstreamSvc.SetPerUserAuth(oauthSvc)
	oauthSvc.SetUserReauthHook(upstreamSvc.DropUserConnection)
	// servers.list / servers.reconnect for agents (access_tools.go).
	gw.SetServersProvider(serversAdapter{upstreams: upstreamSvc, oauth: oauthSvc})
	// Connect links: an agent whose owner has not signed in to a server
	// gets a one-time link to show them (connections.* tools, refusals).
	gw.SetConnect(oauthSvc, idSvc)
	if err := oauthSvc.PrimeBearers(ctx); err != nil {
		log.Printf("oauth: prime bearers: %v", err)
	}
	// One replica refreshes at a time: two would spend the same rotating
	// refresh token, and the loser's invalid_grant reads as needs_reauth.
	leaderLoop(ctx, clusterSvc, loopOAuthRefresher, time.Minute, oauthSvc.RunRefresher)

	// Secrets broker. Separate secrets.key (blast-radius partitioning, same
	// rationale as oauth.key vs session.key). Wired into upstreams so
	// secret:// refs in env/headers resolve at dial time, never reaching
	// agents or API responses.
	secretsKey := masterKeys[keyguard.SecretsKey] // loaded and checked above
	secretsCipher, err := sealbox.NewCipher(secretsKey)
	if err != nil {
		return fmt.Errorf("secrets cipher: %w", err)
	}
	secretsSvc := secrets.New(db, secretsCipher, auditSvc)
	callbackSvc := callbacks.New(db, secretsCipher)
	inboxSvc.SetCallbacks(callbackSvc)
	leaderLoop(ctx, clusterSvc, inbox.SweeperLoop, time.Minute, func(ctx context.Context) { inboxSvc.RunSweeper(ctx, time.Minute) })
	leaderLoop(ctx, clusterSvc, loopInboxAttention, time.Minute, func(ctx context.Context) { inboxSvc.RunAttention(ctx, 15*time.Second) })
	// Every replica delivers callbacks: each delivery is claimed with a
	// database lease first, so no callback is sent twice.
	goroutines.Loop(ctx, loopCallbacks, time.Minute, callbackSvc.Run)
	federationSvc, err := federation.New(ctx, db, idSvc, secretsCipher)
	if err != nil {
		return fmt.Errorf("federation: %w", err)
	}
	federationSvc.SetCluster(clusterSvc)
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
		Settings:  settingsSvc,
		Cipher:    secretsCipher,
		PublicURL: *publicURL,
	})
	chatRegistry.Register(telegramSvc)
	bus.AddNotifier(chatRegistry)
	// One long-poller per bot: Telegram refuses a second getUpdates.
	leaderSupervise(ctx, clusterSvc, loopTelegramPoller, 30*time.Second, telegramSvc.RunPoller)
	leaderSupervise(ctx, clusterSvc, loopChatReconciler, 5*time.Minute, func(ctx context.Context) error {
		return chatRegistry.RunReconciler(ctx, 2*time.Minute)
	})

	// Events Hub. SQLite-backed store. Notifier mirrors the approval push
	// notifier: every event fans out on the realtime hub; events from
	// notify-enabled sources also fire a content-minimal push. Pollers and
	// retention run under Supervise. The events.* MCP tools give every
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
	leaderSupervise(ctx, clusterSvc, loopEventsPoller, 60*time.Second, evSvc.RunPollers)
	leaderSupervise(ctx, clusterSvc, loopEventsRetain, time.Hour, func(ctx context.Context) error {
		return evSvc.RunRetention(ctx,
			func() int { return settingsSvc.GetInt(settings.EventsRetentionDays, 90) })
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
	// Recovery waits for the persistent upstream catalog and Inbox hooks. A
	// previously claimed write stays unknown and is never dispatched again.
	if n, err := bus.SweepUnexecuted(ctx); err != nil {
		log.Printf("legacy execution: startup recovery: %v", err)
	} else if n > 0 {
		log.Printf("legacy execution: recovered %d unclaimed decisions", n)
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
	memWebhookSvc.SetCluster(clusterSvc)
	// TEC-482: ingest is asynchronous — this worker drains queued jobs and
	// performs the slow MemPalace embed/index/store off the HTTP request path.
	// It exits on ctx cancellation; in-flight jobs resume on next boot. One
	// replica runs it at a time; each job carries that replica's lease.
	leaderSupervise(ctx, clusterSvc, loopMemWebhookJobs, time.Minute, memWebhookSvc.RunWorker)

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
		leaderLoop(ctx, clusterSvc, loopNotesScanner, time.Minute, notesSvc.StartScanner)
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
		leaderLoop(ctx, clusterSvc, loopSkillsScanner, time.Minute, skillsSvc.StartScanner)
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
			// tools, and every connected upstream. Calls route
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
		Federation: federationSvc, Callbacks: callbackSvc,
		BuildVersion: version, Environment: os.Getenv("TOOLYARD_ENVIRONMENT"),
		Identity:        idSvc,
		Approval:        bus,
		Audit:           auditSvc,
		Memory:          memSvc,
		Push:            pushSvc,
		Hub:             hub,
		Gateway:         gw,
		Upstreams:       upstreamSvc,
		Settings:        settingsSvc,
		Usage:           usageSvc,
		Metrics:         metricsReader,
		Pricing:         priceCatalog,
		MetricsRecorder: metricsRec,
		AutoApproval:    autoApprover,
		Policy:          policyEngine,
		OAuth:           oauthSvc,
		Hooks:           hooksSvc,
		Mempalace:       mpSvc,
		Notes:           notesSvc,
		Skills:          skillsSvc,
		Voice:           voiceSvc,
		Secrets:         secretsSvc,
		ChatTelegram:    telegramSvc,
		Events:          evSvc,
		MemWebhooks:     memWebhookSvc,
		Inbox:           inboxSvc,
		Snapshots:       inboxSnaps,
		Passkeys:        passkeySvc,
		IdentityKeys:    identityKeysSvc,
		Guide:           inboxGuide,
		WebhookMaxBytes: *webhookMaxBytes,
		SessionKey:      masterKeys[keyguard.SessionKey], // loaded and checked above
		Probes:          probes,
		Security:        secOpts,
		Access:          accessSvc,
		Clerk:           clerkClient,
		ClerkOwnerOnly:  *clerkOwnerOnly,
		OwnerEmail:      *ownerEmail,
		ConnectAZP:      connectOrigins,
		CodeRuns:        codeRuns,
	})

	// Rate limits and the connect-t3 lock hold across replicas.
	apiSrv.SetCluster(clusterSvc)
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
	mcpAuthn := mcpAuth{verify: federationSvc.VerifyAgent, requireAuth: *requireAuthMCP, sec: secOpts}
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
		// mcp-go refuses a request that arrives on a loopback address with
		// a Host that is not loopback (DNS rebinding protection). Behind a
		// reverse proxy on loopback (deploy/Caddyfile.example) every
		// request carries the public Host, so -public-url turns that check
		// off: public mode always requires a token on /mcp
		// (-require-auth-on-mcp), which a rebinding page cannot send. A
		// local gateway without -public-url keeps the check.
		server.WithDisableLocalhostProtection(*publicURL != ""),
		// The guard below verified the token once and put the agent and
		// its raiser on the request context; this hands them to every
		// tool call's context. See actors.go.
		server.WithHTTPContextFunc(mcpAuthn.contextFunc),
	)
	// /mcp gets a larger body limit because tool-call args can be a few MB.
	// TEC-1626 drain: the stream gate refuses new MCP streams (a GET, or a
	// 2026-07-28 subscriptions/listen POST) once the drain starts and ends
	// open ones with the other waiters (lifecycle.go).
	mcpStreams := newMCPStreamGate()
	mux.Handle("/mcp", mcpStreams.wrap(api.LimitBody(gateway.PreserveToolCallNumbers(mcpAuthn.guard(streamable)), 16<<20)))
	mux.Handle("/mcp/", mcpStreams.wrap(api.LimitBody(gateway.PreserveToolCallNumbers(mcpAuthn.guard(streamable)), 16<<20)))

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
			// The TCP peer itself, never a forwarded address: a container
			// in the trusted docker range could claim 127.0.0.1.
			if !api.DirectLoopback(r) {
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
	// 4 MiB universal ceiling, with two overrides: the memory-webhook ingest
	// route (TEC-481) intentionally accepts large n8n payloads, so it gets the
	// configured webhook cap. Without the override this outer ceiling would
	// bind below that cap.
	handler = api.LimitBodyByPath(handler, 4<<20, map[string]int64{
		api.MemoryWebhookIngestPath: *webhookMaxBytes,
		// Native Inbox uploads (PUT /v1/uploads/{ticket}) carry up to a
		// 50 MB video; the handler caps each body at its ticket's size.
		"/v1/uploads/": inbox.MaxVideoBytes,
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
		every := *clerkSyncEvery
		leaderLoop(ctx, clusterSvc, loopClerkSync, time.Hour, func(ctx context.Context) {
			apiSrv.StartClerkSync(ctx, every)
			<-ctx.Done()
		})
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

	// Per replica by nature: each replica sweeps the idle connections of
	// its own upstream pool.
	if idleTimeout > 0 {
		goroutines.Loop(ctx, loopIdleSweeper, time.Minute, func(ctx context.Context) {
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
					loopDone(loopIdleSweeper)
				}
			}
		})
	}
	// Per replica by nature: each replica's catalog comes from its own
	// upstream connections (held in memory), so each re-reads its own live
	// servers. The refresh only reads tool lists; nothing is written to the
	// database, so running it on every replica is safe.
	if every := *catalogRefresh; every > 0 {
		goroutines.Loop(ctx, loopCatalogRefresh, time.Minute, func(ctx context.Context) {
			tick := time.NewTicker(every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if n := gw.RefreshUpstreamCatalogs(ctx); n > 0 {
						rootLog.Info("upstream catalogs refreshed", "changed", n)
					}
					loopDone(loopCatalogRefresh)
				}
			}
		})
	}
	gw.SetMaxLiveUpstreams(*upstreamMaxLive)
	gw.SetMaxLivePerUser(*perUserMaxLive)
	rootLog.Info("upstream pool configured",
		"max_live", *upstreamMaxLive,
		"per_user_max_live", *perUserMaxLive,
		"idle_timeout", idleTimeout.String())

	// Startup recovery (inbox RecoverExecutions, SweepUnexecuted) and
	// upstream loading are done: readiness may report ready.
	markStarted()
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
		rootLog.Info("shutdown signal received")
	case <-ctx.Done():
		rootLog.Warn("root context ended; shutting down")
	}
	// TEC-1626 drain: one ordered shutdown (lifecycle.go). Executors run on
	// the bus's own context, so nothing in flight is cancelled before the
	// drain deadline.
	(&shutdownPlan{
		log:          logx.For("shutdown"),
		drainDelay:   *drainDelay,
		drainTimeout: *drainTimeout,
		grace:        shutdownGrace,
		markDraining: markDraining,
		refuseNew: func() {
			gw.BeginDrain()
			bus.BeginDrain()
			mcpStreams.refuse()
		},
		releaseWaiters: func() {
			hub.CloseAll()
			mcpStreams.release()
			inboxSvc.ReleaseWaiters()
			gw.ReleaseWaiters()
			bus.ReleaseWaiters()
			codeRuns.ReleaseWaiters()
		},
		stopHTTP:  httpSrv.Shutdown,
		forceHTTP: httpSrv.Close,
		waitExecutions: []func(context.Context){
			func(ctx context.Context) { bus.WaitExecutions(ctx, shutdownGrace) },
			func(ctx context.Context) { waitGrantedCalls(ctx, gw, inboxSvc, shutdownGrace) },
			codeRuns.Wait,
		},
		cancelRoot: cancel,
		closers: []shutdownCloser{
			{name: "inbox", close: closeInbox},
			{name: "metrics", close: closeMetrics},
			{name: "gateway", close: closeGateway},
			// Releases this replica's leader leases (the others take
			// over) before the database closes.
			{name: "cluster", close: closeCluster},
			{name: "database", close: closeDB},
		},
	}).run()
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
	pol, err := policy.Load(ctx, db)
	if err != nil {
		return err
	}
	gw := gateway.New(gateway.Options{
		Policy: pol, Approval: bus,
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

func defaultDataDir() string { return defaultDataDirFrom(os.Getenv) }

// defaultDataDirFrom is the default of -data: $TOOLYARD_DATA_DIR, else
// ~/.toolyard. (TOOLYARD_DATA, the environment layer's variable for -data,
// wins over both.)
func defaultDataDirFrom(getenv func(string) string) string {
	if d := getenv("TOOLYARD_DATA_DIR"); d != "" {
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
	} else {
		loopDone(loopReasonScorer)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.ScoreReasons(ctx, time.Now().Add(-24*time.Hour)); err != nil {
				log.Printf("reason scorer: %v", err)
			} else {
				loopDone(loopReasonScorer)
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
		} else {
			loopDone(loopRetention)
			if n > 0 {
				log.Printf("retention compactor: purged %d rows older than %s", n, before.Format(time.RFC3339))
			}
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

// runSessionPurger drops expired rows so the tables don't bloat. Runs
// hourly: dashboard sessions with a 24h grace window past expiry — long
// enough that a laptop coming back online from a long sleep can still see
// its session row before deletion (so the user gets a "your session
// expired" message rather than an opaque 401 after a normal token
// expiry) — agent session tokens an hour past expiry, and code runs older
// than their 7-day retention (TEC-1627).
func runSessionPurger(ctx context.Context, id *identity.Service, codeRuns *coderuns.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	step := func() {
		ok := true
		if n, err := id.PurgeExpiredSessions(ctx, 24*time.Hour); err != nil {
			ok = false
			log.Printf("session purger: %v", err)
		} else if n > 0 {
			log.Printf("session purger: removed %d expired session rows", n)
		}
		if n, err := id.PurgeExpiredSessionTokens(ctx, time.Hour); err != nil {
			ok = false
			log.Printf("session purger: session tokens: %v", err)
		} else if n > 0 {
			log.Printf("session purger: removed %d expired session tokens", n)
		}
		if n, err := codeRuns.Purge(ctx); err != nil {
			ok = false
			log.Printf("session purger: code runs: %v", err)
		} else if n > 0 {
			log.Printf("session purger: removed %d code runs older than 7 days", n)
		}
		if ok {
			loopDone(loopSessionPurger)
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
	settingsHash := assetHash(sub, "settings.js")
	workspaceHash := assetHash(sub, "workspace.css")
	swHash := assetHash(sub, "sw.js")
	loginHash := assetHash(sub, "login.js")
	rewriteIndex := func(body []byte) []byte {
		out := strings.ReplaceAll(string(body), `src="/app.js"`, `src="/app.js?v=`+appHash+`"`)
		out = strings.ReplaceAll(out, `href="/style.css"`, `href="/style.css?v=`+cssHash+`"`)
		out = strings.ReplaceAll(out, `src="/settings.js"`, `src="/settings.js?v=`+settingsHash+`"`)
		out = strings.ReplaceAll(out, `href="/workspace.css"`, `href="/workspace.css?v=`+workspaceHash+`"`)
		return []byte(out)
	}
	rewriteLogin := func(body []byte) []byte {
		out := strings.ReplaceAll(string(body), `src="/login.js"`, `src="/login.js?v=`+loginHash+`"`)
		out = strings.ReplaceAll(out, `href="/style.css"`, `href="/style.css?v=`+cssHash+`"`)
		out = strings.ReplaceAll(out, `href="/workspace.css"`, `href="/workspace.css?v=`+workspaceHash+`"`)
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
		// Don't let SPA path swallow the API. An unknown /v1 route, a
		// probe path variant and /metrics (served only on the ops
		// listener) get a JSON 404, never the dashboard's HTML.
		if p := r.URL.Path; strings.HasPrefix(p, "/v1/") || ops.IsProbePath(strings.TrimSuffix(p, "/")) ||
			p == "/metrics" || strings.HasPrefix(p, "/metrics/") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}` + "\n"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/mcp") {
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

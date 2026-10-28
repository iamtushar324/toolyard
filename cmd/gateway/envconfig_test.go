package main

import (
	"context"
	"errors"
	"flag"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// mapLookup is an environment holding exactly m.
func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// testFlags is a flag set with one flag of each kind serve uses.
type testFlags struct {
	fs             *flag.FlagSet
	addr           *string
	publicURL      *string
	stdio          *bool
	stateless      *bool
	maxConns       *int
	webhookBytes   *int64
	idle           *time.Duration
	data           *string
	credentialsDir *string
	resetPasskeys  *bool
}

func newTestFlags(t *testing.T, args ...string) *testFlags {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &testFlags{
		fs:             fs,
		addr:           fs.String("addr", ":8787", ""),
		publicURL:      fs.String("public-url", "", ""),
		stdio:          fs.Bool("stdio", false, ""),
		stateless:      fs.Bool("stateless-mcp", false, ""),
		maxConns:       fs.Int("db-max-conns", 10, ""),
		webhookBytes:   fs.Int64("webhook-max-bytes", 1024, ""),
		idle:           fs.Duration("upstream-idle-timeout", 0, ""),
		data:           fs.String("data", "/default/data", ""),
		credentialsDir: fs.String("credentials-dir", "", ""),
		resetPasskeys:  fs.Bool("inbox-reset-passkeys", false, ""),
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestEnvName(t *testing.T) {
	for flagName, want := range map[string]string{
		"public-url":            "TOOLYARD_PUBLIC_URL",
		"db-max-conns":          "TOOLYARD_DB_MAX_CONNS",
		"migrate":               "TOOLYARD_MIGRATE",
		"ops-addr":              "TOOLYARD_OPS_ADDR",
		"upstream-idle-timeout": "TOOLYARD_UPSTREAM_IDLE_TIMEOUT",
	} {
		if got := envName(envPrefix, flagName); got != want {
			t.Errorf("envName(%q) = %q, want %q", flagName, got, want)
		}
	}
	if got := envName(healthcheckEnvPrefix, "token-file"); got != "TOOLYARD_HEALTHCHECK_TOKEN_FILE" {
		t.Errorf("healthcheck token-file = %q", got)
	}
}

// The command line wins over the environment; the environment wins over
// the default; a flag nothing sets keeps its default.
func TestEnvLayerPrecedence(t *testing.T) {
	f := newTestFlags(t, "-addr", ":9999", "-stdio=false")
	env := map[string]string{
		"TOOLYARD_ADDR":                  ":1111", // the command line set -addr
		"TOOLYARD_STDIO":                 "true",  // the command line set -stdio=false
		"TOOLYARD_PUBLIC_URL":            " https://toolyard.example.com ",
		"TOOLYARD_STATELESS_MCP":         "1",
		"TOOLYARD_DB_MAX_CONNS":          "7",
		"TOOLYARD_WEBHOOK_MAX_BYTES":     "4096",
		"TOOLYARD_UPSTREAM_IDLE_TIMEOUT": "15m",
		"TOOLYARD_NOT_A_FLAG":            "ignored",
		"OTHER_PUBLIC_URL":               "ignored",
	}
	layer := newEnvLayer(f.fs, envPrefix)
	if err := layer.apply(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
	if *f.addr != ":9999" || *f.stdio {
		t.Errorf("the command line lost: addr %q, stdio %v", *f.addr, *f.stdio)
	}
	if *f.publicURL != "https://toolyard.example.com" {
		t.Errorf("public-url = %q (surrounding whitespace is trimmed)", *f.publicURL)
	}
	if !*f.stateless || *f.maxConns != 7 || *f.webhookBytes != 4096 || *f.idle != 15*time.Minute {
		t.Errorf("from the environment: stateless %v, max conns %d, webhook bytes %d, idle %s",
			*f.stateless, *f.maxConns, *f.webhookBytes, *f.idle)
	}
	if *f.data != "/default/data" || *f.credentialsDir != "" {
		t.Errorf("unset flags changed: data %q, credentials-dir %q", *f.data, *f.credentialsDir)
	}
	want := "TOOLYARD_DB_MAX_CONNS,TOOLYARD_PUBLIC_URL,TOOLYARD_STATELESS_MCP,TOOLYARD_UPSTREAM_IDLE_TIMEOUT,TOOLYARD_WEBHOOK_MAX_BYTES"
	if got := strings.Join(layer.variables(), ","); got != want {
		t.Errorf("variables = %s, want %s", got, want)
	}
}

func TestEnvLayerBools(t *testing.T) {
	for val, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "t": true, "false": false, "0": false} {
		f := newTestFlags(t)
		if err := newEnvLayer(f.fs, envPrefix).apply(mapLookup(map[string]string{"TOOLYARD_STDIO": val})); err != nil {
			t.Fatalf("TOOLYARD_STDIO=%s: %v", val, err)
		}
		if *f.stdio != want {
			t.Errorf("TOOLYARD_STDIO=%s: stdio %v, want %v", val, *f.stdio, want)
		}
	}
}

// A value the flag refuses is a configuration error (exit 78) that names
// the variable and the flag, and never shows the value.
func TestEnvLayerRefusesInvalidValues(t *testing.T) {
	const secretish = "s3cret-pw-value"
	for key, val := range map[string]string{
		"TOOLYARD_STDIO":                 "yes-" + secretish,
		"TOOLYARD_DB_MAX_CONNS":          secretish,
		"TOOLYARD_WEBHOOK_MAX_BYTES":     "99999999999999999999999",
		"TOOLYARD_UPSTREAM_IDLE_TIMEOUT": "15", // a duration needs a unit
	} {
		t.Run(key, func(t *testing.T) {
			f := newTestFlags(t)
			err := newEnvLayer(f.fs, envPrefix).apply(mapLookup(map[string]string{key: val}))
			if !errors.Is(err, keyguard.ErrConfig) {
				t.Fatalf("%s=%s: %v, want a configuration error", key, val, err)
			}
			flagName := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, envPrefix), "_", "-"))
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "-"+flagName) {
				t.Errorf("error does not name %s and -%s: %v", key, flagName, err)
			}
			if strings.Contains(err.Error(), val) || strings.Contains(err.Error(), secretish) {
				t.Errorf("error shows the value: %v", err)
			}
		})
	}
}

// An empty (or blank) variable counts as unset.
func TestEnvLayerIgnoresEmptyValues(t *testing.T) {
	f := newTestFlags(t)
	env := map[string]string{"TOOLYARD_ADDR": "", "TOOLYARD_DB_MAX_CONNS": "  ", "TOOLYARD_STDIO": ""}
	if err := newEnvLayer(f.fs, envPrefix).apply(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
	if *f.addr != ":8787" || *f.maxConns != 10 || *f.stdio {
		t.Errorf("empty variables changed flags: addr %q, max conns %d, stdio %v", *f.addr, *f.maxConns, *f.stdio)
	}
}

// only limits the layer; -inbox-reset-passkeys is never set from the
// environment.
func TestEnvLayerLimits(t *testing.T) {
	f := newTestFlags(t)
	env := map[string]string{"TOOLYARD_ADDR": ":1", "TOOLYARD_DATA": "/env/data", "TOOLYARD_INBOX_RESET_PASSKEYS": "true"}
	if err := newEnvLayer(f.fs, envPrefix, "data").apply(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
	if *f.addr != ":8787" || *f.data != "/env/data" {
		t.Errorf("only data: addr %q, data %q", *f.addr, *f.data)
	}
	g := newTestFlags(t)
	if err := newEnvLayer(g.fs, envPrefix).apply(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
	if *g.resetPasskeys {
		t.Error("TOOLYARD_INBOX_RESET_PASSKEYS set -inbox-reset-passkeys")
	}
}

// -data: TOOLYARD_DATA, then the older TOOLYARD_DATA_DIR.
func TestEnvLayerDataDirAlias(t *testing.T) {
	f := newTestFlags(t)
	if err := newEnvLayer(f.fs, envPrefix).apply(mapLookup(map[string]string{"TOOLYARD_DATA_DIR": "/legacy"})); err != nil {
		t.Fatal(err)
	}
	if *f.data != "/legacy" {
		t.Errorf("TOOLYARD_DATA_DIR alone: data %q", *f.data)
	}
	g := newTestFlags(t)
	if err := newEnvLayer(g.fs, envPrefix).apply(mapLookup(map[string]string{"TOOLYARD_DATA_DIR": "/legacy", "TOOLYARD_DATA": "/new"})); err != nil {
		t.Fatal(err)
	}
	if *g.data != "/new" {
		t.Errorf("TOOLYARD_DATA and TOOLYARD_DATA_DIR: data %q, want TOOLYARD_DATA", *g.data)
	}
	h := newTestFlags(t, "-data", "/args")
	if err := newEnvLayer(h.fs, envPrefix).apply(mapLookup(map[string]string{"TOOLYARD_DATA_DIR": "/legacy", "TOOLYARD_DATA": "/new"})); err != nil {
		t.Fatal(err)
	}
	if *h.data != "/args" {
		t.Errorf("command line: data %q", *h.data)
	}
}

// fakeEnvFile simulates keyguard.LoadEnvFiles on a map: it adds file's
// variables that are still unset, as godotenv.Load does.
func fakeEnvFile(env, file map[string]string) func(string) ([]string, error) {
	return func(dir string) ([]string, error) {
		for k, v := range file {
			if _, ok := env[k]; !ok {
				env[k] = v
			}
		}
		return []string{filepath.Join(dir, keyguard.EnvFileName)}, nil
	}
}

// Variables from toolyard.env set every flag still unset, except
// -credentials-dir (the file is found through it): a different value there
// is ignored with a warning. The process environment wins over the file.
func TestLoadFlagEnvReadsToolyardEnv(t *testing.T) {
	f := newTestFlags(t)
	env := map[string]string{
		"TOOLYARD_CREDENTIALS_DIR": "/run/secrets",
		"TOOLYARD_PUBLIC_URL":      "https://process.example.com",
	}
	file := map[string]string{
		"TOOLYARD_PUBLIC_URL":      "https://file.example.com",
		"TOOLYARD_DB_MAX_CONNS":    "3",
		"TOOLYARD_DATA":            "/file/data",
		"TOOLYARD_CREDENTIALS_DIR": "/elsewhere",
	}
	rep, err := loadFlagEnv(f.fs, flagEnv{lookup: mapLookup(env), loadFiles: fakeEnvFile(env, file)}, f.credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	if *f.credentialsDir != "/run/secrets" || *f.publicURL != "https://process.example.com" {
		t.Errorf("process environment: credentials-dir %q, public-url %q", *f.credentialsDir, *f.publicURL)
	}
	if *f.maxConns != 3 || *f.data != "/file/data" {
		t.Errorf("from toolyard.env: max conns %d, data %q", *f.maxConns, *f.data)
	}
	if len(rep.loaded) != 1 || len(rep.warnings) != 0 {
		t.Errorf("report: %+v (the file's TOOLYARD_CREDENTIALS_DIR never replaced the process one)", rep)
	}

	// Without TOOLYARD_CREDENTIALS_DIR in the process environment, the
	// file's value would move -credentials-dir: it is ignored, with a
	// warning.
	g := newTestFlags(t, "-credentials-dir", "/cmdline")
	env2 := map[string]string{}
	rep, err = loadFlagEnv(g.fs, flagEnv{lookup: mapLookup(env2), loadFiles: fakeEnvFile(env2, file)}, g.credentialsDir)
	if err != nil || *g.credentialsDir != "/cmdline" || len(rep.warnings) != 0 {
		t.Fatalf("command-line -credentials-dir: %q, %+v, %v", *g.credentialsDir, rep, err)
	}
	h := newTestFlags(t)
	env3 := map[string]string{}
	rep, err = loadFlagEnv(h.fs, flagEnv{lookup: mapLookup(env3), loadFiles: fakeEnvFile(env3, file)}, h.credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	if *h.credentialsDir != "" || len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "TOOLYARD_CREDENTIALS_DIR") {
		t.Fatalf("toolyard.env TOOLYARD_CREDENTIALS_DIR: credentials-dir %q, warnings %q", *h.credentialsDir, rep.warnings)
	}
	if *h.maxConns != 3 {
		t.Errorf("toolyard.env still sets the other flags: max conns %d", *h.maxConns)
	}

	// A bad value in toolyard.env is a configuration error too.
	k := newTestFlags(t)
	env4 := map[string]string{}
	_, err = loadFlagEnv(k.fs, flagEnv{lookup: mapLookup(env4), loadFiles: fakeEnvFile(env4, map[string]string{"TOOLYARD_STDIO": "maybe"})}, k.credentialsDir)
	if !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), "TOOLYARD_STDIO") {
		t.Fatalf("bad value in toolyard.env: %v", err)
	}
}

// The real toolyard.env loader: a variable in the file sets a flag.
func TestLoadFlagEnvWithARealToolyardEnv(t *testing.T) {
	creds := t.TempDir()
	if err := os.WriteFile(filepath.Join(creds, keyguard.EnvFileName), []byte("TOOLYARD_DB_MAX_CONNS=5\nTOOLYARD_STATELESS_MCP=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	// t.Setenv restores each variable when the test ends; unset them now so
	// the file can fill them (godotenv never overrides a set variable).
	for _, k := range []string{"TOOLYARD_DB_MAX_CONNS", "TOOLYARD_STATELESS_MCP", "TOOLYARD_PUBLIC_URL", "TOOLYARD_DATA", "TOOLYARD_DATA_DIR", "TOOLYARD_CREDENTIALS_DIR"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	f := newTestFlags(t, "-credentials-dir", creds)
	rep, err := loadFlagEnv(f.fs, processEnv(), f.credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	if *f.maxConns != 5 || !*f.stateless {
		t.Errorf("from %s: max conns %d, stateless %v", keyguard.EnvFileName, *f.maxConns, *f.stateless)
	}
	if len(rep.loaded) != 1 || rep.loaded[0] != filepath.Join(creds, keyguard.EnvFileName) {
		t.Errorf("loaded %v", rep.loaded)
	}
}

func TestParseMigrateMode(t *testing.T) {
	for in, want := range map[string]migrateMode{"auto": migrateAuto, " AUTO ": migrateAuto, "off": migrateOff, "Off": migrateOff} {
		if got, err := parseMigrateMode(in); err != nil || got != want {
			t.Errorf("parseMigrateMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "on", "false", "never"} {
		if _, err := parseMigrateMode(in); !errors.Is(err, keyguard.ErrConfig) {
			t.Errorf("parseMigrateMode(%q): %v, want a configuration error", in, err)
		}
	}
}

// serve warns about each TOOLYARD_ variable that sets none of its flags and
// that nothing reads (a misspelt name), naming the variable and never its
// value; flag variables, the variables the code reads itself and empty
// ones get no warning.
func TestUnknownEnvWarnings(t *testing.T) {
	f := newTestFlags(t)
	environ := []string{
		"TOOLYARD_KEY=value-one-require",                      // misspelt
		"TOOLYARD_KEY=value-two",                              // same name again: one warning
		"TOOLYARD_ADDR=:1",                                    // a flag
		"TOOLYARD_DATA_DIR=/value/data",                       // the older variable for -data
		"TOOLYARD_DATABASE_URL=postgres://u:s3cret-value@h/d", // read by the code
		"TOOLYARD_DATABASE_URL_FILE=/value/database-url",      // not supported: warn
		"TOOLYARD_CLERK_SECRET_KEY_FILE=/value/clerk",         // read by the code
		"TOOLYARD_CLERK_SECRET=value-clerk",                   // misspelt
		"TOOLYARD_HEALTHCHECK_TOKEN_FILE=/value/ops-token",    // the healthcheck command's
		"TOOLYARD_CODEMODE_SANDBOX=auto",                      // the code-mode sandbox's
		"TOOLYARD_INBOX_RESET_PASSKEYS=true",                  // command line only
		"TOOLYARD_EMPTY=",                                     // empty: unset
		"PATH=/usr/bin",
		"NOT_TOOLYARD_X=1",
		"malformed",
	}
	got := unknownEnvWarnings(f.fs, environ)
	var names []string
	for _, w := range got {
		names = append(names, strings.Fields(w)[0])
		for _, v := range []string{"value-one", "value-two", "s3cret-value", "/value/", "value-clerk", "true"} {
			if strings.Contains(w, v) {
				t.Errorf("warning %q shows a value (%s)", w, v)
			}
		}
	}
	want := "TOOLYARD_CLERK_SECRET,TOOLYARD_DATABASE_URL_FILE,TOOLYARD_INBOX_RESET_PASSKEYS,TOOLYARD_KEY"
	if strings.Join(names, ",") != want {
		t.Fatalf("warned about %v, want %s:\n%s", names, want, strings.Join(got, "\n"))
	}
	for _, w := range got {
		if strings.HasPrefix(w, "TOOLYARD_INBOX_RESET_PASSKEYS") && !strings.Contains(w, "command line only") {
			t.Errorf("command-line-only flag: %q", w)
		}
		if strings.HasPrefix(w, "TOOLYARD_KEY ") && !strings.Contains(w, "serve") {
			t.Errorf("unknown variable: %q does not name the command", w)
		}
	}
	if w := unknownEnvWarnings(f.fs, nil); len(w) != 0 {
		t.Fatalf("no environment: %v", w)
	}
}

// Every TOOLYARD_ name the code spells out as a string literal is one it
// reads itself (knownEnv, knownEnvPrefixes), a prefix (it ends in _) or a
// flag's variable (envAliases), so unknownEnvWarnings never warns about a
// variable the code reads. Flag variables are built from flag names and
// never spelt out. A new os.Getenv("TOOLYARD_...") must be added to knownEnv.
func TestKnownEnvCoversEveryVariableTheCodeReads(t *testing.T) {
	literal := regexp.MustCompile(`"(TOOLYARD_[A-Z0-9_]*)"`)
	var missing []string
	files := 0
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
				name := m[1]
				if strings.HasSuffix(name, "_") || knownEnvVar(name) || envAliases[name] != nil {
					continue
				}
				missing = append(missing, path+": "+name)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("read only %d Go files: wrong directory?", files)
	}
	if len(missing) > 0 {
		t.Fatalf("TOOLYARD_ variables the code reads but knownEnv does not list (unknownEnvWarnings would warn about them):\n%s",
			strings.Join(missing, "\n"))
	}
}

// The -tool-search-* flags (TEC-1807) follow the TOOLYARD_<FLAG> contract:
// each is set by its variable, a bad value is a configuration error, and
// none draws an unknown-variable warning.
func TestToolSearchFlagsFromTheEnvironment(t *testing.T) {
	newFlags := func(args ...string) (*flag.FlagSet, *toolSearchFlags) {
		t.Helper()
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		ts := registerToolSearchFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, ts
	}
	fs, ts := newFlags()
	if on, err := ts.check(); on || err != nil {
		t.Fatalf("defaults: on %v, %v", on, err)
	}
	if *ts.model != "typesafe/jev-1.13" || *ts.keySecret != "OPENROUTER_TOOLSEARCH_KEY" || *ts.timeout != 1500*time.Millisecond || *ts.zdr {
		t.Fatalf("defaults: model %q, key secret %q, timeout %s, zdr %v", *ts.model, *ts.keySecret, *ts.timeout, *ts.zdr)
	}
	env := map[string]string{
		"TOOLYARD_TOOL_SEARCH_RANKER":     "openrouter-decisions",
		"TOOLYARD_TOOL_SEARCH_MODEL":      "typesafe/jev-1.14",
		"TOOLYARD_TOOL_SEARCH_KEY_SECRET": "MY_RANKER_KEY",
		"TOOLYARD_TOOL_SEARCH_TIMEOUT":    "2s",
		"TOOLYARD_TOOL_SEARCH_ZDR":        "true",
	}
	layer := newEnvLayer(fs, envPrefix)
	if err := layer.apply(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
	if *ts.ranker != "openrouter-decisions" || *ts.model != "typesafe/jev-1.14" || *ts.keySecret != "MY_RANKER_KEY" || *ts.timeout != 2*time.Second || !*ts.zdr {
		t.Fatalf("from the environment: %q %q %q %s %v", *ts.ranker, *ts.model, *ts.keySecret, *ts.timeout, *ts.zdr)
	}
	want := "TOOLYARD_TOOL_SEARCH_KEY_SECRET,TOOLYARD_TOOL_SEARCH_MODEL,TOOLYARD_TOOL_SEARCH_RANKER,TOOLYARD_TOOL_SEARCH_TIMEOUT,TOOLYARD_TOOL_SEARCH_ZDR"
	if got := strings.Join(layer.variables(), ","); got != want {
		t.Fatalf("variables = %s, want %s", got, want)
	}
	if on, err := ts.check(); !on || err != nil {
		t.Fatalf("ranker on: %v, %v", on, err)
	}
	var environ []string
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	if w := unknownEnvWarnings(fs, environ); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}

	for _, c := range []struct {
		args     []string
		flagName string
	}{
		{[]string{"-tool-search-ranker=fuzzy"}, "-tool-search-ranker"},
		{[]string{"-tool-search-ranker=openrouter-decisions", "-tool-search-timeout=0s"}, "-tool-search-timeout"},
		{[]string{"-tool-search-ranker=openrouter-decisions", "-tool-search-key-secret=lower"}, "-tool-search-key-secret"},
		{[]string{"-tool-search-ranker=openrouter-decisions", "-tool-search-model= "}, "-tool-search-model"},
	} {
		_, bad := newFlags(c.args...)
		if _, err := bad.check(); !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), c.flagName) {
			t.Errorf("%v: %v, want a configuration error naming %s", c.args, err, c.flagName)
		}
	}
	fresh, _ := newFlags()
	if err := newEnvLayer(fresh, envPrefix).apply(mapLookup(map[string]string{"TOOLYARD_TOOL_SEARCH_TIMEOUT": "15"})); !errors.Is(err, keyguard.ErrConfig) ||
		!strings.Contains(err.Error(), "TOOLYARD_TOOL_SEARCH_TIMEOUT") {
		t.Fatalf("TOOLYARD_TOOL_SEARCH_TIMEOUT=15: %v", err)
	}

	// The engine reads the key at search time: a missing secret is no key,
	// and the search falls back to keyword ranking without a request.
	_, on := newFlags("-tool-search-ranker", "openrouter-decisions")
	reads := 0
	engine := on.engine(func(_ context.Context, name string) (string, error) {
		reads++
		if name != "OPENROUTER_TOOLSEARCH_KEY" {
			t.Errorf("read secret %q", name)
		}
		return "", secrets.ErrNotFound
	})
	tools := []toolsearch.Tool{{Name: "demo.create_issue", Server: "demo", Description: "Create an issue."}}
	for i := 0; i < 3; i++ {
		res, err := engine.Search(context.Background(), tools, toolsearch.Query{Text: "create an issue"})
		if err != nil || res.Fallback != toolsearch.FallbackNoKey || res.RankedBy != toolsearch.RankedKeyword || len(res.Hits) != 1 {
			t.Fatalf("no key: %+v, %v", res, err)
		}
	}
	if reads != 1 {
		t.Fatalf("the secret was read %d times in a minute, want 1", reads)
	}
}

// -mcp-safe-tool-names (TEC-1807) is on by default and follows the
// TOOLYARD_<FLAG> contract: TOOLYARD_MCP_SAFE_TOOL_NAMES=false turns it off
// without a code change, the command line wins, a bad value is a
// configuration error naming the variable, and the variable draws no
// unknown-variable warning.
func TestSafeToolNamesFlagFromTheEnvironment(t *testing.T) {
	newFlags := func(args ...string) (*flag.FlagSet, *bool) {
		t.Helper()
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		on := registerSafeToolNamesFlag(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, on
	}
	if _, on := newFlags(); !*on {
		t.Fatal("-mcp-safe-tool-names is off by default")
	}
	const key = "TOOLYARD_MCP_SAFE_TOOL_NAMES"
	fs, on := newFlags()
	layer := newEnvLayer(fs, envPrefix)
	if err := layer.apply(mapLookup(map[string]string{key: "false"})); err != nil {
		t.Fatal(err)
	}
	if *on || strings.Join(layer.variables(), ",") != key {
		t.Fatalf("%s=false: on %v, variables %v", key, *on, layer.variables())
	}
	if w := unknownEnvWarnings(fs, []string{key + "=false"}); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	cli, cliOn := newFlags("-mcp-safe-tool-names=true")
	if err := newEnvLayer(cli, envPrefix).apply(mapLookup(map[string]string{key: "false"})); err != nil || !*cliOn {
		t.Fatalf("command line true, variable false: on %v, %v", *cliOn, err)
	}
	bad, _ := newFlags()
	if err := newEnvLayer(bad, envPrefix).apply(mapLookup(map[string]string{key: "maybe"})); !errors.Is(err, keyguard.ErrConfig) ||
		!strings.Contains(err.Error(), key) {
		t.Fatalf("%s=maybe: %v", key, err)
	}
}

package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
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

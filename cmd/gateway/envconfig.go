package main

// The environment layer (TEC-1638): every flag of serve, and the location
// flags of the other commands, can be set as TOOLYARD_<FLAG>: the flag name
// upper-cased with - turned into _ (-public-url is TOOLYARD_PUBLIC_URL,
// -db-max-conns is TOOLYARD_DB_MAX_CONNS). Containers and Kubernetes set
// variables more easily than they edit a command line.
//
// Precedence, highest first:
//  1. the command line;
//  2. TOOLYARD_<FLAG> in the process environment (a .env file loadDotenv
//     read counts as the process environment);
//  3. TOOLYARD_<FLAG> from toolyard.env in $CREDENTIALS_DIRECTORY or
//     -credentials-dir (keyguard.LoadEnvFiles, which never overrides a
//     variable that is already set);
//  4. the flag's default, which for a few flags is itself a variable read
//     when the flag is defined (TOOLYARD_DATA_DIR for -data,
//     TOOLYARD_CREDENTIALS_DIR for -credentials-dir, TOOLYARD_VOICE_MODEL
//     for -voice-model).
//
// toolyard.env is found through -credentials-dir, so the layer runs twice:
// once on the process environment, which settles -credentials-dir, then,
// after toolyard.env is loaded, again for every flag still unset. So
// TOOLYARD_<FLAG> inside toolyard.env works for every flag except
// -credentials-dir itself; a TOOLYARD_CREDENTIALS_DIR there that would
// change it is ignored with a warning. -data is not needed to find
// toolyard.env, so TOOLYARD_DATA (or TOOLYARD_DATA_DIR) may come from it.
//
// An empty variable counts as unset. A value the flag refuses is a
// configuration error (exit 78) that names the variable but never shows
// the value. Variables that name no flag are ignored, but serve logs a
// warning for each one the code does not read itself either (a misspelt
// name; unknownEnvWarnings). Secrets stay in files: no flag holds one, only
// paths to them.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
)

// envPrefix is the prefix of the variable that sets a flag.
const envPrefix = "TOOLYARD_"

// envAliases are older variables that also set a flag, after its own
// variable: TOOLYARD_DATA_DIR has set the default of -data since before the
// environment layer.
var envAliases = map[string][]string{
	"TOOLYARD_DATA": {"TOOLYARD_DATA_DIR"},
}

// commandLineOnly are flags no variable sets. -inbox-reset-passkeys deletes
// every passkey at startup: a one-time recovery step must not stay in a
// service's environment and run again on every restart.
var commandLineOnly = map[string]bool{
	"inbox-reset-passkeys": true,
}

// knownEnv are the TOOLYARD_ variables the code reads itself rather than
// through a flag, in this binary (serve and its subcommands, the code-mode
// worker) and in the toolyard admin CLI and bifrost-import, whose variables
// can sit in the same environment. unknownEnvWarnings is quiet about them.
// TestKnownEnvCoversEveryVariableTheCodeReads keeps the list complete.
var knownEnv = map[string]bool{
	databaseURLEnv:                        true, // database.go
	"TOOLYARD_CREDENTIALS_DIR":            true, // default of -credentials-dir
	"TOOLYARD_DATA_DIR":                   true, // default of -data
	"TOOLYARD_VOICE_MODEL":                true, // default of -voice-model
	"TOOLYARD_ENVIRONMENT":                true,
	"TOOLYARD_INBOX_VOICE_MODEL":          true,
	"TOOLYARD_MEMPALACE_BIN":              true, // internal/mempalace
	"TOOLYARD_CLERK_SECRET_KEY":           true, // clerkFromEnv, with the _FILE forms
	"TOOLYARD_CLERK_SECRET_KEY_FILE":      true,
	"TOOLYARD_CLERK_PUBLISHABLE_KEY":      true,
	"TOOLYARD_CLERK_PUBLISHABLE_KEY_FILE": true,
	"TOOLYARD_CLERK_ORGANIZATION_ID":      true,
	"TOOLYARD_CLERK_ORGANIZATION_ID_FILE": true,
	"TOOLYARD_SERVER":                     true, // cmd/toolyard (admin CLI)
	"TOOLYARD_OPERATOR_TOKEN":             true,
	"TOOLYARD_OPERATOR_TOKEN_FILE":        true,
	"TOOLYARD_PASSWORD":                   true, // cmd/bifrost-import
	// deploy/stage/toolyard-stage.service loads it from its EnvironmentFile
	// and expands it into -owner-email.
	"TOOLYARD_STAGE_OWNER_EMAIL": true,
}

// knownEnvPrefixes are families of variables the code reads itself: the
// healthcheck command's, the code-mode sandbox and worker's
// (TOOLYARD_CODEMODE_SANDBOX, internal/codemode) and the test harness's
// (internal/store/storetest).
var knownEnvPrefixes = []string{healthcheckEnvPrefix, "TOOLYARD_CODEMODE_", "TOOLYARD_TEST_"}

func knownEnvVar(key string) bool {
	if knownEnv[key] {
		return true
	}
	for _, p := range knownEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// unknownEnvWarnings returns, sorted, a warning for each TOOLYARD_ variable
// in environ (KEY=value entries, as os.Environ returns them) that sets no
// flag of fs and that the code does not read itself (knownEnv): a misspelt
// name such as TOOLYARD_KEY for TOOLYARD_KEYS would otherwise be ignored
// without a word. A variable for a command-line-only flag gets its own
// warning. A warning names the variable, never its value. An empty
// variable counts as unset and gets none.
func unknownEnvWarnings(fs *flag.FlagSet, environ []string) []string {
	flagVars := map[string]string{} // variable -> flag name
	fs.VisitAll(func(f *flag.Flag) {
		k := envName(envPrefix, f.Name)
		flagVars[k] = f.Name
		for _, alias := range envAliases[k] {
			flagVars[alias] = f.Name
		}
	})
	seen := map[string]bool{}
	var out []string
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, envPrefix) || strings.TrimSpace(val) == "" || seen[key] {
			continue
		}
		seen[key] = true
		if name, isFlag := flagVars[key]; isFlag {
			if commandLineOnly[name] {
				out = append(out, fmt.Sprintf("%s is ignored: -%s is set on the command line only", key, name))
			}
			continue
		}
		if knownEnvVar(key) {
			continue
		}
		out = append(out, fmt.Sprintf("%s is ignored: %s has no flag for it and nothing reads it (a misspelt name?)", key, fs.Name()))
	}
	sort.Strings(out)
	return out
}

// envName returns the variable that sets the flag name under prefix.
func envName(prefix, name string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// flagEnv is the environment a command reads: lookup reads a variable, and
// loadFiles loads toolyard.env from the credentials directories into what
// lookup reads (nil: no file is loaded).
type flagEnv struct {
	lookup    func(string) (string, bool)
	loadFiles func(credentialsDir string) ([]string, error)
}

// processEnv is the real process environment.
func processEnv() flagEnv {
	return flagEnv{lookup: os.LookupEnv, loadFiles: keyguard.LoadEnvFiles}
}

func (e flagEnv) getenv(key string) string {
	if e.lookup == nil {
		return ""
	}
	v, _ := e.lookup(key)
	return v
}

// envLayer fills flags that the command line did not set from variables.
type envLayer struct {
	fs       *flag.FlagSet
	prefix   string
	only     map[string]bool   // nil: every flag
	fromArgs map[string]bool   // set on the command line
	fromEnv  map[string]string // flag name -> the variable that set it
}

// newEnvLayer records the flags the command line set (fs.Visit). Call it
// right after fs.Parse, before anything calls fs.Set. only, when given,
// limits the layer to those flags.
func newEnvLayer(fs *flag.FlagSet, prefix string, only ...string) *envLayer {
	l := &envLayer{fs: fs, prefix: prefix, fromArgs: map[string]bool{}, fromEnv: map[string]string{}}
	if len(only) > 0 {
		l.only = map[string]bool{}
		for _, n := range only {
			l.only[n] = true
		}
	}
	fs.Visit(func(f *flag.Flag) { l.fromArgs[f.Name] = true })
	return l
}

// covers reports whether the layer may set the flag name at all.
func (l *envLayer) covers(name string) bool {
	if commandLineOnly[name] || l.fs.Lookup(name) == nil {
		return false
	}
	return l.only == nil || l.only[name]
}

// value returns the variable that sets the flag name and its trimmed value
// ("" when none is set or every one is empty).
func (l *envLayer) value(lookup func(string) (string, bool), name string) (key, val string) {
	primary := envName(l.prefix, name)
	for _, k := range append([]string{primary}, envAliases[primary]...) {
		if raw, ok := lookup(k); ok {
			if v := strings.TrimSpace(raw); v != "" {
				return k, v
			}
		}
	}
	return primary, ""
}

// apply sets each flag the layer covers that neither the command line nor
// an earlier apply set, from its variable when that is set and not empty.
// skip names flags this pass leaves alone.
func (l *envLayer) apply(lookup func(string) (string, bool), skip ...string) error {
	if lookup == nil {
		return nil
	}
	skipped := map[string]bool{}
	for _, n := range skip {
		skipped[n] = true
	}
	var err error
	l.fs.VisitAll(func(f *flag.Flag) {
		if err != nil || !l.covers(f.Name) || skipped[f.Name] || l.fromArgs[f.Name] || l.fromEnv[f.Name] != "" {
			return
		}
		key, val := l.value(lookup, f.Name)
		if val == "" {
			return
		}
		if serr := l.fs.Set(f.Name, val); serr != nil {
			err = envValueError(key, f.Name, val, serr)
			return
		}
		l.fromEnv[f.Name] = key
	})
	return err
}

// variables returns, sorted, the variables that set a flag.
func (l *envLayer) variables() []string {
	out := make([]string, 0, len(l.fromEnv))
	for _, k := range l.fromEnv {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envValueError is a variable whose value the flag refused. The flag
// package's own messages do not quote the value, but a custom flag's could:
// any copy of it is cut out, since the variable may hold something secret.
// (A value of one to three characters is no secret, and cutting it would
// garble the message: "u" is in "out of range".)
func envValueError(key, flagName, value string, err error) error {
	msg := err.Error()
	if len(value) >= 4 {
		msg = strings.ReplaceAll(msg, value, "<value>")
	}
	return configErrorf("%s: not a valid value for -%s (%s); the value is not shown", key, flagName, msg)
}

// envReport is what loadFlagEnv did, for the caller to log once logging
// is set up.
type envReport struct {
	loaded    []string // toolyard.env files loaded
	variables []string // variables that set a flag (names only)
	warnings  []string
}

// loadFlagEnv applies the environment layer to fs (see the comment at the
// top of this file): the process environment first, then, when
// credentialsDir is given and env loads files, toolyard.env from the
// credentials directories, then the layer again for the flags still unset,
// except -credentials-dir. Call it right after fs.Parse. only, when given,
// limits the layer to those flags.
func loadFlagEnv(fs *flag.FlagSet, env flagEnv, credentialsDir *string, only ...string) (envReport, error) {
	var rep envReport
	layer := newEnvLayer(fs, envPrefix, only...)
	if err := layer.apply(env.lookup); err != nil {
		return rep, err
	}
	if credentialsDir != nil && env.loadFiles != nil && env.lookup != nil {
		loaded, err := env.loadFiles(*credentialsDir)
		rep.loaded = loaded
		if err != nil {
			return rep, err
		}
		if len(loaded) > 0 {
			const pinned = "credentials-dir"
			if layer.covers(pinned) && !layer.fromArgs[pinned] {
				if key, val := layer.value(env.lookup, pinned); val != "" && val != *credentialsDir {
					rep.warnings = append(rep.warnings, fmt.Sprintf(
						"%s from %s is ignored: -credentials-dir is where that file is found, so set it on the command line or in the process environment",
						key, keyguard.EnvFileName))
				}
			}
			if err := layer.apply(env.lookup, pinned); err != nil {
				return rep, err
			}
		}
	}
	rep.variables = layer.variables()
	return rep, nil
}

// configError is a setting that is missing or wrong. It exits 78, like a
// key error, since a restart will not fix it.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

func (e *configError) Is(target error) bool { return target == keyguard.ErrConfig }

func configErrorf(format string, args ...any) error {
	return &configError{msg: fmt.Sprintf(format, args...)}
}

// Package codemode gives toolyard a Bifrost-compatible "code mode": the four
// tools listToolFiles, readToolFile, getToolDocs and executeToolCode, with the
// same names, arguments, stub format and output shapes as the code mode of
// the Bifrost MCP gateway (maximhq/bifrost core/mcp/codemode), so agents and
// skills written against Bifrost work unchanged when their `bifrost` MCP
// server points at toolyard.
//
// Next to Bifrost's Starlark, scripts may be JavaScript (language
// "javascript"): QuickJS-NG compiled to WebAssembly (quickjs/), run by
// wazero's interpreter inside the same worker, with async tool calls
// (await tools.Server.tool({...})), tools.tryCall, a bounded
// tools.parallel and tools.search; see js.go. TypeScript stubs
// (servers/<Server>/<tool>.d.ts) describe the tools to it; see
// stubs_ts.go.
//
// The package knows nothing about the gateway. It sees the caller's tools
// through Caller.Tools, already scoped to what that caller may use, and
// dispatches every nested call through Caller.RouteCall, which is the
// gateway's full pipeline (access, policy, approval, identity forwarding,
// audit, metrics).
//
// A script runs in a child process (WorkerMain, started by the gateway
// binary's codemode-worker subcommand) under a hard memory cap, a step
// limit and a wall clock, with an empty environment and no credentials: it
// only asks the parent, over its stdin and stdout, to make tool calls, and
// the parent makes them. A child that dies, for any reason, costs one
// script its run and nothing else.
//
// On Linux (arm64 and amd64) the worker is also a privilege boundary. It
// starts from a gateway thread that confines itself first, so the child
// inherits three restrictions before its first instruction: no new
// privileges; a Landlock ruleset under which it may open only its own
// binary and ELF interpreter (to execute and read) and the shared
// libraries and /etc/ld.so.cache (to read), so no data directory, /proc,
// /etc or /tmp file, and, as far as the kernel's Landlock ABI goes, no
// TCP bind or connect and no signals or abstract Unix sockets outside
// its own process; and a seccomp filter that refuses socket creation,
// io_uring, ptrace and process_vm access, bpf, perf events, userfaultfd,
// the kernel key store, new namespaces (unshare, setns, clone with a
// namespace flag or CLONE_PARENT, clone3), mounting, resource limits and
// scheduling of other processes (prlimit64 on another pid, setpriority,
// sched_set*, ioprio_set), and changes to file metadata (chmod, chown,
// utimes, extended attributes, truncate). A flaw in starlark-go (or in
// QuickJS or wazero) then reaches the worker's own pipes and memory, not
// the gateway's files, network or processes. See sandbox_linux.go.
//
// Worker.Sandbox picks the mode; the gateway reads it from
// TOOLYARD_CODEMODE_SANDBOX (ParseSandboxMode). SandboxAuto, the default,
// applies what the kernel supports: without Landlock it logs once and
// keeps no_new_privs and the seccomp filter; with neither it runs the
// worker with its resource caps only, as before: same user, same
// filesystem view and network as the gateway, kept in its box by the
// Starlark interpreter (no filesystem, network or load()), the memory,
// CPU, step and output caps, and the bare environment. SandboxRequired
// refuses to run scripts unless both Landlock and the seccomp filter
// apply. SandboxStatus reports which applies.
//
// Known limits:
//   - Path lookup and reading metadata stay possible: a confined worker
//     can still stat, readlink and chdir anywhere, so it can learn which
//     paths exist, not what files hold. Changing metadata is refused by
//     the seccomp filter.
//   - Below Landlock ABI 6 signals are not scoped: a worker can still
//     signal the gateway and other processes of the same user.
//     SandboxStatus then adds "(signals unscoped)"; SandboxRequired still
//     accepts it.
//   - On ABI 6 kernels without Landlock erratum 2, signal scoping also
//     binds the gateway's own confining thread while it starts the
//     worker, so a runtime preemption signal it sends to another gateway
//     thread can be refused; the worst case is a short delay to a garbage
//     collection pause it starts. The one call where a refusal would
//     matter (the pidfd check) runs before confinement.
//   - The sandbox tests' unconfined baseline reads /etc/hostname, which a
//     minimal container may lack.
package codemode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Via is the path recorded on every nested call's audit rows and metric.
const Via = "code_mode"

// Tool is one callable the caller may use, as code mode sees it.
type Tool struct {
	// Server is the key the tool hangs off in code (BkCoreServices.get_client):
	// the upstream name, or a built-in group such as memory. A key that is
	// not a Starlark identifier is bound under a sanitised one.
	Server string
	// Name is the tool's own name on its server, before mangling
	// (get-all-clients). Its Starlark identifier is derived from it.
	Name string
	// Target is the catalog name RouteCall dispatches
	// (BkCoreServices.get-all-clients).
	Target string
	// Description is the tool's own description, without gateway banners.
	Description string
	// Properties and Required are the tool's JSON Schema input, with the
	// gateway's injected fields (_reason and friends) already removed.
	Properties map[string]any
	Required   []string
	// OutputSchema is the tool's declared output JSON Schema, nil when it
	// declares none; the TypeScript stubs type results from it.
	OutputSchema map[string]any
	// ReasonField is the argument a nested call's reason travels in
	// (normally _reason).
	ReasonField string
}

// Caller is what the runtime needs from the gateway.
type Caller interface {
	// Tools lists the tools the ctx caller may use.
	Tools(ctx context.Context) []Tool
	// RouteCall dispatches one nested call through the gateway pipeline.
	RouteCall(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error)
}

// Limits bound one executeToolCode run. They travel to the worker as JSON.
type Limits struct {
	// ScriptTimeout caps the wall clock of a whole script, nested calls
	// included. The caller's own deadline still applies when shorter.
	ScriptTimeout time.Duration `json:"scriptTimeout"`
	// MaxCalls caps how many nested tool calls one script may make.
	MaxCalls int `json:"maxCalls"`
	// MaxOutputBytes caps the print log and the final response text.
	MaxOutputBytes int `json:"maxOutputBytes"`
	// MaxSteps caps Starlark execution steps, so a tight loop stops long
	// before the wall clock does.
	MaxSteps uint64 `json:"maxSteps"`
}

// DefaultLimits are the production limits.
func DefaultLimits() Limits {
	return Limits{
		ScriptTimeout:  5 * time.Minute,
		MaxCalls:       100,
		MaxOutputBytes: 1 << 20,
		MaxSteps:       50_000_000,
	}
}

// Worker describes the process one script runs in.
type Worker struct {
	// Path is the executable; "" means the running one (os.Executable).
	Path string
	// Args make the executable run one script from stdin: the gateway
	// binary's codemode-worker subcommand.
	Args []string
	// Env is the child's whole environment, before the runtime settings the
	// parent adds (memory cap, GOMEMLIMIT, GOMAXPROCS, GOTRACEBACK).
	// Nothing of the parent's environment is inherited.
	Env []string
	// MemoryMiB caps the child's data segment (RLIMIT_DATA); its Go heap
	// target is three quarters of it. An allocation past the cap ends the
	// child, and the script, with a clean error. The Go runtime maps heap
	// in 64 MiB arenas, so the live data a script can hold is about the
	// cap minus 64 MiB; decoding a tool result into Starlark values takes
	// many times the JSON's size, and 256 MiB is what lets a result at the
	// 6 MiB cap decode. Default 256, sized with MaxConcurrent so all
	// workers together (1 GiB) fit a 1.5 GiB service limit with the
	// gateway's own few hundred MiB beside them.
	MemoryMiB int
	// MaxConcurrent caps scripts running at once, MaxPerAgent those of
	// one caller. A run past either cap waits AcquireWait in a fair queue
	// for a slot, then fails as busy. Defaults 4, 2 and 2s.
	MaxConcurrent int
	MaxPerAgent   int
	AcquireWait   time.Duration
	// Sandbox says how the worker is confined beyond its resource caps;
	// the zero value is SandboxAuto. With the sandbox on, Path must be an
	// ELF executable, not a script: the ruleset grants execute on Path and
	// its ELF interpreter only.
	Sandbox SandboxMode
}

// SandboxMode says how a worker is confined beyond its resource caps.
type SandboxMode int

const (
	// SandboxAuto confines the worker with what the kernel supports
	// (Landlock and a seccomp filter on Linux). Without Landlock it logs
	// once and runs the worker with its resource caps only.
	SandboxAuto SandboxMode = iota
	// SandboxOff runs the worker with its resource caps only.
	SandboxOff
	// SandboxRequired refuses to start a script when Landlock is
	// unavailable.
	SandboxRequired
)

func (m SandboxMode) String() string {
	switch m {
	case SandboxAuto:
		return "auto"
	case SandboxOff:
		return "off"
	case SandboxRequired:
		return "required"
	}
	return fmt.Sprintf("SandboxMode(%d)", int(m))
}

// ParseSandboxMode reads a mode name: "auto" (or empty), "off" or
// "required", in any case. An unknown name returns an error, which the
// gateway treats as fatal, and SandboxAuto.
func ParseSandboxMode(s string) (SandboxMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return SandboxAuto, nil
	case "off":
		return SandboxOff, nil
	case "required":
		return SandboxRequired, nil
	}
	return SandboxAuto, fmt.Errorf("unknown code-mode sandbox mode %q (want auto, off or required)", s)
}

// sandboxSupport is the confinement this kernel, under the gateway's own
// seccomp filter (a service manager's, say), lets a worker have.
type sandboxSupport struct {
	// abi is the Landlock ABI version; 0 means Landlock is unavailable.
	abi int
	// reason says why Landlock is unavailable when abi is 0.
	reason string
	// seccomp says whether a seccomp filter can be installed;
	// seccompReason says why not.
	seccomp       bool
	seccompReason string
	// definite says the answer holds for the life of the process: the
	// probe ran to completion, or a seccomp filter killed it. Any other
	// probe failure (a timeout, a failed fork) is probed again at the
	// next worker start.
	definite bool
}

var (
	// lastSandboxMode is the mode of the most recent SetWorker, for
	// SandboxStatus.
	lastSandboxMode atomic.Int32

	// supportMu serialises probes; supportCached holds a definite answer,
	// supportLast the latest answer of any kind.
	supportMu      sync.Mutex
	supportCached  atomic.Pointer[sandboxSupport]
	supportLast    atomic.Pointer[sandboxSupport]
	probeRequested atomic.Bool
	// supportOverride replaces the kernel probe's answer. Tests only.
	supportOverride atomic.Pointer[sandboxSupport]
	// probeSupport is the kernel probe; tests replace it to count probes.
	probeSupport = probeSandboxSupport
)

// sandboxSupported probes the kernel (probeSandboxSupport, per platform),
// waiting for a probe that is running, and caches a definite answer for
// the life of the process. An indefinite one is returned but not kept,
// so the next call probes again.
func sandboxSupported() sandboxSupport {
	if s := supportOverride.Load(); s != nil {
		return *s
	}
	if s := supportCached.Load(); s != nil {
		return *s
	}
	supportMu.Lock()
	defer supportMu.Unlock()
	if s := supportCached.Load(); s != nil {
		return *s
	}
	s := probeSupport()
	supportLast.Store(&s)
	if s.definite {
		supportCached.Store(&s)
	}
	return s
}

// sandboxSupportNow is sandboxSupported without the wait: the latest
// answer, or ok false until the first probe has finished.
func sandboxSupportNow() (s sandboxSupport, ok bool) {
	if p := supportOverride.Load(); p != nil {
		return *p, true
	}
	if p := supportLast.Load(); p != nil {
		return *p, true
	}
	return sandboxSupport{}, false
}

// SandboxStatus describes the confinement scripts get under the most
// recently configured worker: "landlock-abi-<n>+seccomp" ("+seccomp"
// missing when no seccomp filter can be installed, " (signals unscoped)"
// added below ABI 6); "seccomp-only: <reason>" when Landlock is
// unavailable but the filter applies; "none: <reason>" when neither does;
// " (sandbox required: scripts are refused)" appended under
// SandboxRequired when either is missing; or "off". It never waits for
// the kernel probe: until a probe has finished it reports "probing", and
// starts one if nothing has.
func SandboxStatus() string {
	mode := SandboxMode(lastSandboxMode.Load())
	if mode == SandboxOff {
		return "off"
	}
	s, ok := sandboxSupportNow()
	if !ok {
		if probeRequested.CompareAndSwap(false, true) {
			go sandboxSupported()
		}
		return "probing"
	}
	return sandboxStatusText(mode, s)
}

// ProbeSandbox runs the kernel probe unless it has run, logs once what
// confinement workers get, and returns SandboxStatus. The gateway calls
// it in a goroutine at start, so neither a request nor the first script
// waits on the probe.
func ProbeSandbox() string {
	if SandboxMode(lastSandboxMode.Load()) != SandboxOff {
		probeRequested.Store(true)
		logSandboxOnce(sandboxSupported())
	}
	return SandboxStatus()
}

func sandboxStatusText(mode SandboxMode, s sandboxSupport) string {
	if mode == SandboxOff {
		return "off"
	}
	var text string
	switch {
	case s.abi > 0:
		text = fmt.Sprintf("landlock-abi-%d", s.abi)
		if s.seccomp {
			text += "+seccomp"
		}
		if s.abi < 6 {
			text += " (signals unscoped)"
		}
	case s.seccomp:
		text = "seccomp-only: " + s.reason
	default:
		text = "none: " + s.reason
	}
	if mode == SandboxRequired && (s.abi == 0 || !s.seccomp) {
		text += " (sandbox required: scripts are refused)"
	}
	return text
}

// DefaultWorker is the production worker: this binary's codemode-worker
// subcommand.
func DefaultWorker() Worker {
	return Worker{Args: []string{"codemode-worker"}}
}

// withDefaults fills zero fields.
func (w Worker) withDefaults() Worker {
	if w.MemoryMiB <= 0 {
		w.MemoryMiB = 256
	}
	if w.MaxConcurrent <= 0 {
		w.MaxConcurrent = 4
	}
	if w.MaxPerAgent <= 0 {
		w.MaxPerAgent = 2
	}
	if w.AcquireWait <= 0 {
		w.AcquireWait = 2 * time.Second
	}
	return w
}

// Runtime renders stubs and runs scripts against one Caller.
type Runtime struct {
	caller  Caller
	limits  Limits
	worker  Worker
	limiter *limiter
	// inProcess runs scripts in this process instead of a worker. Tests
	// only: it has no memory cap.
	inProcess bool
	// lastPID is the most recent worker's pid, for tests that check it is
	// gone.
	lastPID atomic.Int64
}

// New returns a Runtime with the production worker. Zero fields in limits
// take their defaults.
func New(caller Caller, limits Limits) *Runtime {
	def := DefaultLimits()
	if limits.ScriptTimeout <= 0 {
		limits.ScriptTimeout = def.ScriptTimeout
	}
	if limits.MaxCalls <= 0 {
		limits.MaxCalls = def.MaxCalls
	}
	if limits.MaxOutputBytes <= 0 {
		limits.MaxOutputBytes = def.MaxOutputBytes
	}
	if limits.MaxOutputBytes > maxOutputCap {
		limits.MaxOutputBytes = maxOutputCap
	}
	if limits.MaxSteps == 0 {
		limits.MaxSteps = def.MaxSteps
	}
	r := &Runtime{caller: caller, limits: limits}
	r.SetWorker(DefaultWorker())
	return r
}

// Limits returns the runtime's effective limits.
func (r *Runtime) Limits() Limits { return r.limits }

// SetWorker replaces how scripts are run. Zero fields take their defaults.
func (r *Runtime) SetWorker(w Worker) {
	r.worker = w.withDefaults()
	r.limiter = newLimiter(r.worker.MaxConcurrent, r.worker.MaxPerAgent)
	lastSandboxMode.Store(int32(r.worker.Sandbox))
}

// Worker returns the effective worker settings.
func (r *Runtime) Worker() Worker { return r.worker }

// LimitsText states the enforced limits for a tool description.
func (r *Runtime) LimitsText() string {
	wall := r.limits.ScriptTimeout.String()
	if r.limits.ScriptTimeout >= time.Minute && r.limits.ScriptTimeout%time.Minute == 0 {
		wall = fmt.Sprintf("%d minutes", int(r.limits.ScriptTimeout/time.Minute))
		if r.limits.ScriptTimeout == time.Minute {
			wall = "1 minute"
		}
	}
	return fmt.Sprintf("Limits per run: %s wall clock, %d tool calls, %d MiB of output, %d MiB of memory.",
		wall, r.limits.MaxCalls, r.limits.MaxOutputBytes>>20, r.worker.MemoryMiB)
}

// executablePath is the worker binary.
func (r *Runtime) executablePath() (string, error) {
	if r.worker.Path != "" {
		return r.worker.Path, nil
	}
	return os.Executable()
}

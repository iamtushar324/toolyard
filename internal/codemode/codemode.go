// Package codemode gives toolyard a Bifrost-compatible "code mode": the four
// tools listToolFiles, readToolFile, getToolDocs and executeToolCode, with the
// same names, arguments, stub format and output shapes as the code mode of
// the Bifrost MCP gateway (maximhq/bifrost core/mcp/codemode), so agents and
// skills written against Bifrost work unchanged when their `bifrost` MCP
// server points at toolyard.
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
package codemode

import (
	"context"
	"fmt"
	"os"
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
	// child, and the script, with a clean error. Default 512.
	MemoryMiB int
	// MaxConcurrent caps scripts running at once. A run past the cap waits
	// AcquireWait for a slot, then fails as busy. Defaults 4 and 2s.
	MaxConcurrent int
	AcquireWait   time.Duration
}

// DefaultWorker is the production worker: this binary's codemode-worker
// subcommand.
func DefaultWorker() Worker {
	return Worker{Args: []string{"codemode-worker"}}
}

// withDefaults fills zero fields.
func (w Worker) withDefaults() Worker {
	if w.MemoryMiB <= 0 {
		w.MemoryMiB = 512
	}
	if w.MaxConcurrent <= 0 {
		w.MaxConcurrent = 4
	}
	if w.AcquireWait <= 0 {
		w.AcquireWait = 2 * time.Second
	}
	return w
}

// Runtime renders stubs and runs scripts against one Caller.
type Runtime struct {
	caller Caller
	limits Limits
	worker Worker
	sem    chan struct{}
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
	r.sem = make(chan struct{}, r.worker.MaxConcurrent)
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

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
// audit, metrics). Scripts run in a sandboxed Starlark interpreter: no
// filesystem, no network, no load(), bounded by Limits.
package codemode

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Via is the path recorded on every nested call's audit rows and metric.
const Via = "code_mode"

// Tool is one callable the caller may use, as code mode sees it.
type Tool struct {
	// Server is the key the tool hangs off in code (BkCoreServices.get_client):
	// the upstream name, or a built-in group such as memory.
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

// Limits bound one executeToolCode run.
type Limits struct {
	// ScriptTimeout caps the wall clock of a whole script, nested calls
	// included. The caller's own deadline still applies when shorter.
	ScriptTimeout time.Duration
	// MaxCalls caps how many nested tool calls one script may make.
	MaxCalls int
	// MaxOutputBytes caps the print log and the final response text.
	MaxOutputBytes int
	// MaxSteps caps Starlark execution steps, so a tight loop stops long
	// before the wall clock does.
	MaxSteps uint64
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

// Runtime renders stubs and runs scripts against one Caller.
type Runtime struct {
	caller Caller
	limits Limits
}

// New returns a Runtime. Zero fields in limits take their defaults.
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
	return &Runtime{caller: caller, limits: limits}
}

// Limits returns the runtime's effective limits.
func (r *Runtime) Limits() Limits { return r.limits }

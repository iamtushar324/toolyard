package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Published tool names (TEC-1807). A tool's canonical name is the one the
// catalog, policies, grants, audit rows and code mode use: "inbox.request",
// "<server>.<tool>". Some MCP clients accept only names that match
// ^[a-zA-Z0-9_-]{1,64}$ (OpenAI's tool-name rule), and one name outside it
// can make such a client refuse the whole server. With safe tool names on
// (-mcp-safe-tool-names, Options.SafeToolNames) tools/list publishes every
// tool under a name that matches the rule, and a call may name a tool by
// its published name or by its canonical name. Clients that rewrite names
// themselves (Claude turns "inbox.request" into "inbox_request") arrive at
// the same name either way.
//
// The published name of a canonical name is:
//
//   - the name with every character outside [a-zA-Z0-9_-] replaced by "_"
//     ("inbox.request" -> "inbox_request", "github.get-issue" ->
//     "github_get-issue");
//   - its first 55 characters, "_" and the first 8 hex digits of the
//     SHA-256 of the canonical name, when that is longer than 64
//     characters, or when another tool of the catalog maps to the same
//     name. A canonical name that already matches the rule is never
//     renamed: it is its own published name, so a name that is a
//     canonical name always means that tool.
//
// Names are allocated over the sorted catalog and checked for uniqueness
// after hashing (publishedToolNames): a hashed name that another tool
// already holds takes 16 hex digits, then 8 and a counter, so no
// published name ever means two tools.
//
// The names depend only on the catalog, so every replica with the same
// catalog publishes the same names, and resolving one needs no per-process
// state beyond the catalog (toolNameTable, rebuilt when it changes).

const (
	// maxPublishedNameLen is the longest name the rule allows.
	maxPublishedNameLen = 64
	// publishedHashPrefix is how much of the name a hashed name keeps;
	// with "_" and publishedHashLen hex digits it fills the 64.
	publishedHashPrefix = 55
	publishedHashLen    = 8
)

// PublishedToolName is the name tools/list shows for the canonical tool
// name when safe tool names are on, on its own: every character outside
// [a-zA-Z0-9_-] becomes "_", and a result longer than 64 characters (or
// empty) is hashed. Two canonical names can map to the same result; the
// gateway tells them apart with the whole catalog (toolNameTable), so a
// listed name may be the hashed form even where this returns the short
// one. The built-in tools never collide: their names are what the texts a
// client reads use.
func PublishedToolName(canonical string) string {
	s := sanitizeToolName(canonical)
	if s == "" || len(s) > maxPublishedNameLen {
		return hashedToolName(canonical, s)
	}
	return s
}

// ValidPublishedToolName reports whether name matches ^[a-zA-Z0-9_-]{1,64}$.
func ValidPublishedToolName(name string) bool {
	if name == "" || len(name) > maxPublishedNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !safeToolNameByte(name[i]) {
			return false
		}
	}
	return true
}

func safeToolNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// sanitizeToolName replaces every character outside [a-zA-Z0-9_-] with
// "_", one "_" per character (a multi-byte character is one).
func sanitizeToolName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r < 0x80 && safeToolNameByte(byte(r)) {
			b.WriteByte(byte(r))
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// hashedToolName is the long or colliding form: the first 55 characters
// of sanitized, "_", and the first 8 hex digits of SHA-256(canonical).
func hashedToolName(canonical, sanitized string) string {
	return hashedCandidate(canonical, sanitized, 0)
}

// hashedCandidate is the attempt-th hashed form of canonical, for when the
// earlier ones are taken: "_" and 8 hex digits of SHA-256(canonical)
// (attempt 0), 16 hex digits (attempt 1), then 8 hex digits, "_" and the
// attempt number. The sanitized name fills what is left of the 64
// characters. Every attempt gives a different name, so a search over them
// ends.
func hashedCandidate(canonical, sanitized string, attempt int) string {
	h := sha256.Sum256([]byte(canonical))
	sum := hex.EncodeToString(h[:])
	suffix := "_" + sum[:publishedHashLen]
	switch {
	case attempt == 1:
		suffix = "_" + sum[:2*publishedHashLen]
	case attempt > 1:
		suffix += "_" + strconv.Itoa(attempt)
	}
	prefix := sanitized
	if room := maxPublishedNameLen - len(suffix); len(prefix) > room {
		prefix = prefix[:room]
	}
	return prefix + suffix
}

// publishedToolNames maps every canonical name in names to its published
// name. The answer depends on the set of names only, never on their order,
// and no two names get the same published name:
//
//  1. a canonical name that already matches the rule keeps it;
//  2. a name whose short form (PublishedToolName) no other name has, and
//     that step 1 did not take, gets its short form;
//  3. every other name, in sorted order, gets the first hashed form
//     (hashedCandidate) no name holds yet.
func publishedToolNames(names []string) map[string]string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	out := make(map[string]string, len(sorted))
	taken := make(map[string]bool, len(sorted))
	var rest []string
	for _, n := range sorted {
		if ValidPublishedToolName(n) {
			out[n] = n
			taken[n] = true
			continue
		}
		rest = append(rest, n)
	}
	base := make(map[string]string, len(rest))
	count := make(map[string]int, len(rest))
	for _, n := range rest {
		p := PublishedToolName(n)
		base[n] = p
		count[p]++
	}
	var hashed []string
	for _, n := range rest {
		if p := base[n]; count[p] == 1 && !taken[p] {
			out[n] = p
			taken[p] = true
			continue
		}
		hashed = append(hashed, n)
	}
	for _, n := range hashed {
		sanitized := sanitizeToolName(n)
		for attempt := 0; ; attempt++ {
			if p := hashedCandidate(n, sanitized, attempt); !taken[p] {
				out[n] = p
				taken[p] = true
				break
			}
		}
	}
	return out
}

// toolNameTable is the published names of one catalog generation.
type toolNameTable struct {
	gen uint64
	// published maps every canonical name of the catalog to its
	// published name.
	published map[string]string
	// canonical maps each published name that differs from its canonical
	// name back to that name.
	canonical map[string]string
}

// publish is the published name of a canonical name: from the catalog, or
// PublishedToolName for a name the catalog does not have.
func (t *toolNameTable) publish(canonical string) string {
	if p, ok := t.published[canonical]; ok {
		return p
	}
	return PublishedToolName(canonical)
}

// toolNames is the table for the current catalog. It is built once per
// catalog change (catalogGen) and shared. Never call it holding g.mu.
func (g *Gateway) toolNames() *toolNameTable {
	if t := g.toolNameCache.Load(); t != nil && t.gen == g.catalogGen.Load() {
		return t
	}
	g.mu.RLock()
	gen := g.catalogGen.Load()
	names := make([]string, 0, len(g.tools))
	for n := range g.tools {
		names = append(names, n)
	}
	g.mu.RUnlock()
	published := publishedToolNames(names)
	t := &toolNameTable{gen: gen, published: published, canonical: make(map[string]string)}
	for c, p := range published {
		if p != c {
			t.canonical[p] = c
		}
	}
	// A table built from an older catalog may land after a newer one; the
	// next reader sees the old generation and builds again.
	g.toolNameCache.Store(t)
	return t
}

// catalogChanged marks the catalog as changed so the next toolNames
// rebuilds. Call it holding g.mu for writing, after changing g.tools.
func (g *Gateway) catalogChanged() { g.catalogGen.Add(1) }

// resolveToolName is the canonical name a caller means: name itself when
// a tool has that canonical name, the tool whose published name it is
// otherwise, and name unchanged when it is neither (an unknown tool stays
// unknown, with the name the caller sent). Published names resolve whether
// or not this gateway publishes them, so a client that listed them before
// an operator turned safe names off keeps working until it lists again.
// Never call it holding g.mu.
func (g *Gateway) resolveToolName(name string) string {
	g.mu.RLock()
	_, ok := g.tools[name]
	g.mu.RUnlock()
	if ok || name == "" {
		return name
	}
	if c, ok := g.toolNames().canonical[name]; ok {
		return c
	}
	return name
}

// resolveToolNameFor is resolveToolName limited to the tools callerID may
// use: a published name of a tool outside the caller's access scope (or
// its agent's own limits) stays as supplied, exactly as an unknown name
// would, so an answer never names a tool the caller cannot see. Never call
// it holding g.mu.
func (g *Gateway) resolveToolNameFor(ctx context.Context, callerID, name string) string {
	canonical := g.resolveToolName(name)
	if canonical == name {
		return name
	}
	g.mu.RLock()
	entry, ok := g.tools[canonical]
	g.mu.RUnlock()
	if !ok || !g.allowsEntry(ctx, callerID, entry) {
		return name
	}
	return canonical
}

// CanonicalTool implements inbox.ToolNamer: inbox.request and inbox.check
// accept a published name of a tool the agent may use and keep the
// canonical one, so a grant names the tool the way policies and audit do
// and works for a call by either name. Any other name stays as supplied.
func (g *Gateway) CanonicalTool(ctx context.Context, agentID, name string) string {
	return g.resolveToolNameFor(ctx, agentID, name)
}

// suppliedToolNameKey carries the name a routed call was made with when it
// differs from the canonical one (RouteCall), so a refusal for a tool the
// caller may not use echoes the caller's own name (denyUngranted).
type suppliedToolNameKey struct{}

type suppliedToolName struct{ canonical, supplied string }

func withSuppliedToolName(ctx context.Context, canonical, supplied string) context.Context {
	if canonical == supplied {
		return ctx
	}
	return context.WithValue(ctx, suppliedToolNameKey{}, suppliedToolName{canonical: canonical, supplied: supplied})
}

// callerToolName is the name the caller used for canonical: the supplied
// name RouteCall recorded for this tool, else canonical. Keyed by the
// canonical name, so a nested call to another tool never borrows it.
func callerToolName(ctx context.Context, canonical string) string {
	if s, ok := ctx.Value(suppliedToolNameKey{}).(suppliedToolName); ok && s.canonical == canonical {
		return s.supplied
	}
	return canonical
}

// mcpToolName is how a text the client reads names an MCP-listed tool:
// its published name with safe names on, its canonical name otherwise.
// For the built-in tools the short published name is the listed one.
func (g *Gateway) mcpToolName(canonical string) string {
	return listedToolName(g.safeToolNames, canonical)
}

// listedToolName is mcpToolName without a gateway.
func listedToolName(safe bool, canonical string) string {
	if safe {
		return PublishedToolName(canonical)
	}
	return canonical
}

// publishToolNames is the last tools/list hook with safe names on: after
// the access filter, the small tool list, the visibility provider and the
// task marks, each tool's name becomes its published name. Only the name
// changes, on a copy: the slice an earlier hook returned is never written.
func (g *Gateway) publishToolNames(_ context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
	if res == nil || len(res.Tools) == 0 {
		return
	}
	t := g.toolNames()
	out := make([]mcp.Tool, len(res.Tools))
	copy(out, res.Tools)
	for i := range out {
		out[i].Name = t.publish(out[i].Name)
	}
	res.Tools = out
}

// resolveCallName is the tools/call hook that runs before mcp-go looks the
// tool up: a published name becomes the canonical one, so the call runs
// the registered tool and everything after (policy, grants, approvals,
// audit, jobs) sees the canonical name, exactly as for a call that named
// it. The access request hook, which runs earlier on the raw message,
// resolves the name the same way.
func (g *Gateway) resolveCallName(_ context.Context, _ any, req *mcp.CallToolRequest) {
	if req == nil {
		return
	}
	req.Params.Name = g.resolveToolName(req.Params.Name)
}

// paramHeaderError is a Mcp-Param-* header that disagrees with the body,
// answered as mcp-go answers one: JSON-RPC error HEADER_MISMATCH, which
// the 2026-07-28 transport sends with HTTP 400.
type paramHeaderError struct{ err error }

func (e paramHeaderError) Error() string { return e.err.Error() }

func (e paramHeaderError) Unwrap() error { return e.err }

func (e paramHeaderError) JSONRPCError() mcp.JSONRPCError {
	return mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		Error:   mcp.JSONRPCErrorDetails{Code: mcp.HEADER_MISMATCH, Message: e.err.Error()},
	}
}

// checkParamHeaders checks a 2026-07-28 tools/call's Mcp-Param-* headers
// (SEP-2243) against the tool that runs. mcp-go checks them before
// dispatch, but finds the tool by the name the body sent: a published name
// finds nothing, and its check is skipped. This repeats it with mcp-go's
// own rules (mcp.ValidateParamHeadersLookup) on the body's own bytes where
// PreserveToolCallNumbers kept them, so a call by either name is held to
// the same headers, and a proxy that decides on them sees the parameters
// that run. The Mcp-Name header stays checked by mcp-go against the name
// sent. Requests without the protocol header (stdio, in-process, older
// versions) are not checked, as in mcp-go.
func (g *Gateway) checkParamHeaders(ctx context.Context, entry toolEntry, request mcp.CallToolRequest) error {
	if !server.IsModernRequest(ctx) || request.Header.Get(mcp.HeaderProtocolVersion) == "" {
		return nil
	}
	params := g.callParamsJSON(ctx, entry, request)
	if err := mcp.ValidateParamHeadersLookup(headerLookup(request.Header), &entry.tool, params); err != nil {
		return server.NewHandlerError(paramHeaderError{err: err})
	}
	return nil
}

// callParamsJSON is the call's params as JSON: the request body's own
// bytes when PreserveToolCallNumbers kept them and they are this call,
// else the decoded arguments marshalled again.
func (g *Gateway) callParamsJSON(ctx context.Context, entry toolEntry, request mcp.CallToolRequest) json.RawMessage {
	if raw, ok := rawMCPBody(ctx); ok {
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		var call struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Method == string(mcp.MethodToolsCall) &&
			json.Unmarshal(env.Params, &call) == nil && g.resolveToolName(call.Name) == entry.tool.Name {
			return env.Params
		}
	}
	b, _ := json.Marshal(map[string]any{"arguments": request.Params.Arguments})
	return b
}

// headerLookup reads one header and whether it was sent at all, as mcp-go
// does, so a header sent with an empty value is not taken for a missing
// one.
func headerLookup(h http.Header) func(string) (string, bool) {
	return func(name string) (string, bool) {
		values := h.Values(name)
		if len(values) == 0 {
			return "", false
		}
		return values[0], true
	}
}

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
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
//     renamed for a collision: the other tools are, so a name that is a
//     canonical name always means that tool.
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
	sum := sha256.Sum256([]byte(canonical))
	prefix := sanitized
	if len(prefix) > publishedHashPrefix {
		prefix = prefix[:publishedHashPrefix]
	}
	return prefix + "_" + hex.EncodeToString(sum[:])[:publishedHashLen]
}

// publishedToolNames maps every canonical name in names to its published
// name, with the collision rule applied over all of them.
func publishedToolNames(names []string) map[string]string {
	base := make(map[string]string, len(names))
	groups := make(map[string][]string, len(names))
	for _, n := range names {
		if _, dup := base[n]; dup {
			continue
		}
		p := PublishedToolName(n)
		base[n] = p
		groups[p] = append(groups[p], n)
	}
	out := make(map[string]string, len(base))
	for p, members := range groups {
		if len(members) == 1 {
			out[members[0]] = p
			continue
		}
		for _, n := range members {
			if n == p {
				// Already a valid name: it keeps it, the others move.
				out[n] = p
				continue
			}
			out[n] = hashedToolName(n, sanitizeToolName(n))
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

// CanonicalTool implements inbox.ToolNamer: inbox.request and inbox.check
// accept a published name and keep the canonical one, so a grant names
// the tool the way policies and audit do and works for a call by either
// name.
func (g *Gateway) CanonicalTool(name string) string { return g.resolveToolName(name) }

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

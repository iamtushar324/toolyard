package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// The MCP Skills extension (SEP-2640,
// https://modelcontextprotocol.io/seps/2640-skills-extension). While the
// skills hub is on, toolyard declares
// capabilities.extensions["io.modelcontextprotocol/skills"] =
// {"directoryRead": true} on both protocol versions (the 2025-11-25
// initialize result and the 2026-07-28 server/discover result, which
// mcp-go builds from one capability set), with the resources capability
// the extension requires, and serves each skill file as a resource:
//
//	skill://<source>/<skill path>/SKILL.md     the skill
//	skill://<source>/<skill path>/<file path>  its other files
//	skill://<source>/<skill path>              its folder (inode/directory)
//
// resources/read of a skill:// URI and resources/templates/list (one
// template, skill://{+path}) are mcp-go's, through the template
// registered by RegisterSkillHub, so they work on every transport. mcp-go
// has no route for methods it does not know, so MCPSkills answers the
// other three in front of the HTTP transport, as MCPTasks answers tasks:
//
//   - skills/list: the caller's visible skills, a page at a time, each
//     entry {uri, frontmatter, resources: [{uri, digest, size}]};
//     2026-07-28 results carry the list-caching attributes (private: the
//     listing depends on the caller).
//   - skills/get: one entry by its SKILL.md URI, listed or not.
//   - resources/directory/read: the direct children of a skill folder.
//
// An unknown URI, or one the caller may not see, is JSON-RPC error -32602
// (invalid params) on both versions, as the SEP prescribes. Every read of
// skill content (skills/get, resources/read, resources/directory/read)
// writes an audit row naming the URI and the version (owner/repo@commit).
// skills/list is a catalog listing, like tools/list, and writes none.

// SkillsExtensionID is the extension's capability key.
const SkillsExtensionID = "io.modelcontextprotocol/skills"

// SEP-2640 methods MCPSkills answers.
const (
	MethodSkillsList             = "skills/list"
	MethodSkillsGet              = "skills/get"
	MethodResourcesDirectoryRead = "resources/directory/read"
)

// Audit rows of the non-tool skill reads.
const (
	// EventSkillRead records one read of skill content outside a tool
	// call: tool_name is the method or route, result_summary the URI and
	// owner/repo@commit.
	EventSkillRead = "skill.read"
	// mimeDirectory is a directory resource's MIME type (SEP-2640).
	mimeDirectory = "inode/directory"
	// skillsListCacheTTLMs is the freshness hint on a 2026-07-28
	// skills/list: a new version is announced, best effort, with
	// notifications/resources/list_changed.
	skillsListCacheTTLMs = 60_000
)

// sepResource is one file of a skill entry.
type sepResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// sepSkillEntry is a skills/list and skills/get entry.
type sepSkillEntry struct {
	URI         string         `json:"uri"`
	Frontmatter map[string]any `json:"frontmatter"`
	Resources   []sepResource  `json:"resources"`
}

// sepEntry renders s as a SEP-2640 entry: the frontmatter verbatim (name
// and description from the summary only when the Reader kept none) and
// every file, SKILL.md included.
func sepEntry(s skillhub.Skill) sepSkillEntry {
	fm := s.Frontmatter
	if fm == nil {
		fm = map[string]any{"name": s.Name, "description": s.Description}
	}
	res := make([]sepResource, 0, len(s.Files))
	for _, f := range s.Files {
		res = append(res, sepResource{URI: f.URI, Digest: f.Digest, Size: f.Size})
	}
	return sepSkillEntry{URI: s.URI, Frontmatter: fm, Resources: res}
}

// skillResourceTemplate is the one resource template: every skill file.
func skillResourceTemplate() mcp.ResourceTemplate {
	return mcp.NewResourceTemplate(skillScheme+"{+path}", "Agent Skills",
		mcp.WithTemplateDescription("Files of the Agent Skills toolyard serves (MCP Skills extension): "+
			"skill://<source>/<skill path>/SKILL.md and the files beside it. Find skills with skills/list or the skills.search tool."),
		mcp.WithTemplateMIMEType("text/markdown"))
}

// skillRPCError is a JSON-RPC error a skills handler returns to the client
// as is (server.NewHandlerError).
type skillRPCError struct {
	code int
	msg  string
}

func (e skillRPCError) Error() string { return e.msg }

func (e skillRPCError) JSONRPCError() mcp.JSONRPCError {
	return mcp.NewJSONRPCError(mcp.RequestId{}, e.code, e.msg, nil)
}

// skillRPCErrorFor maps a Reader error on uri to the JSON-RPC error the
// SEP asks for: -32602 for anything the caller cannot have (unknown,
// invisible, not a skill), -32603 when the catalog failed.
func skillRPCErrorFor(err error, uri string) skillRPCError {
	switch {
	case errors.Is(err, skillhub.ErrNotFound), errors.Is(err, skillhub.ErrConflict), errors.Is(err, skillhub.ErrInvalid):
		return skillRPCError{code: mcp.INVALID_PARAMS, msg: "Resource not found: " + clipText(uri, 300)}
	case errors.Is(err, skillhub.ErrDisabled):
		return skillRPCError{code: mcp.INVALID_PARAMS, msg: "the skills hub is off on this server"}
	}
	_, msg := skillsErrorText(err, uri)
	return skillRPCError{code: mcp.INTERNAL_ERROR, msg: msg}
}

// readSkillResource is the skill:// resource template's handler:
// resources/read of one skill file, on any transport.
func (g *Gateway) readSkillResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	uri := req.Params.URI
	hub := g.skillHub.Load()
	if hub == nil {
		return nil, server.NewHandlerError(skillRPCErrorFor(skillhub.ErrDisabled, uri))
	}
	ctx = g.skillReadContext(ctx)
	f, err := hub.reader.Read(ctx, g.skillViewer(ctx, hub), uri, 0, 0)
	if err != nil {
		g.auditSkillRead(ctx, string(mcp.MethodResourcesRead), uri, "", err)
		return nil, server.NewHandlerError(skillRPCErrorFor(err, uri))
	}
	g.auditSkillRead(ctx, string(mcp.MethodResourcesRead), f.URI, f.Repo, nil)
	mimeType := f.Type
	if mimeType == "" {
		mimeType = skillMIMEType(f.URI)
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: f.URI, MIMEType: mimeType, Text: f.Content}}, nil
}

// MCPSkills wraps the /mcp handler with the SEP-2640 methods mcp-go has no
// route for. contextFunc is the transport's HTTPContextFunc (it puts the
// authenticated caller on the context); localhostProtection mirrors the
// transport's DNS rebinding protection, as for MCPTasks. While the skills
// hub is off, and for every other method, it passes the request through
// untouched, so mcp-go answers (an unknown method stays method-not-found).
func (g *Gateway) MCPSkills(next http.Handler, contextFunc func(context.Context, *http.Request) context.Context, localhostProtection bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub := g.skillHub.Load()
		if hub == nil || r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		raw, ok := rawMCPBody(r.Context())
		if !ok {
			var err error
			if raw, err = io.ReadAll(r.Body); err != nil {
				http.Error(w, "invalid MCP body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r = r.WithContext(context.WithValue(r.Context(), rawMCPBodyKey{}, raw))
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &msg) != nil ||
			len(msg.ID) == 0 || string(msg.ID) == "null" || msg.JSONRPC != mcp.JSONRPC_VERSION {
			next.ServeHTTP(w, r)
			return
		}
		switch msg.Method {
		case MethodSkillsList, MethodSkillsGet, MethodResourcesDirectoryRead:
		default:
			next.ServeHTTP(w, r)
			return
		}
		if localhostProtection && rebindingRequest(r) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
			return
		}
		req := taskRequest{id: msg.ID, method: msg.Method, params: msg.Params}
		var p struct {
			Meta json.RawMessage `json:"_meta"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if !checkModernRequest(w, r, &req, p.Meta) {
			return
		}
		ctx := r.Context()
		if contextFunc != nil {
			ctx = contextFunc(ctx, r)
		}
		ctx = g.skillReadContext(withRequestMeta(ctx, p.Meta))
		v := g.skillViewer(ctx, hub)
		switch msg.Method {
		case MethodSkillsList:
			g.skillsListRPC(ctx, w, req, hub, v)
		case MethodSkillsGet:
			g.skillsGetRPC(ctx, w, req, hub, v)
		case MethodResourcesDirectoryRead:
			g.skillsDirectoryRPC(ctx, w, req, hub, v)
		}
	})
}

// skillsListResult is a skills/list answer.
type skillsListResult struct {
	mcp.CacheableResult
	Skills     []sepSkillEntry `json:"skills"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

func (g *Gateway) skillsListRPC(ctx context.Context, w http.ResponseWriter, req taskRequest, hub *skillHubConfig, v skillhub.Viewer) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if len(req.params) > 0 && json.Unmarshal(req.params, &p) != nil {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "invalid skills/list params")
		return
	}
	list, next, err := hub.reader.List(ctx, v, skillhub.ListQuery{Cursor: p.Cursor})
	if err != nil {
		e := skillRPCErrorFor(err, "")
		if errors.Is(err, skillhub.ErrInvalid) {
			e.msg = "invalid cursor"
		}
		writeRPCError(w, req, e.code, e.msg)
		return
	}
	out := skillsListResult{Skills: make([]sepSkillEntry, 0, len(list)), NextCursor: next}
	for _, s := range list {
		out.Skills = append(out.Skills, sepEntry(s))
	}
	if req.modern {
		out.ResultType = mcp.ResultTypeComplete
		out.SetCacheHints(skillsListCacheTTLMs, mcp.CacheScopePrivate)
	}
	writeRPCResult(w, req.id, out)
}

// skillsGetResult is a skills/get answer.
type skillsGetResult struct {
	mcp.Result
	Skill sepSkillEntry `json:"skill"`
}

func (g *Gateway) skillsGetRPC(ctx context.Context, w http.ResponseWriter, req taskRequest, hub *skillHubConfig, v skillhub.Viewer) {
	var p struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(req.params, &p) != nil || p.URI == "" {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "skills/get needs params.uri: the URI of a skill's SKILL.md")
		return
	}
	// params.uri must name a SKILL.md: a bare name or a source/path
	// (which skills.get accepts) is not a skill URI here.
	if !strings.HasPrefix(p.URI, skillScheme) || path.Base(p.URI) != skillFileName {
		g.auditSkillRead(ctx, MethodSkillsGet, p.URI, "", skillhub.ErrNotFound)
		writeRPCError(w, req, mcp.INVALID_PARAMS, "Skill not found: "+clipText(p.URI, 300))
		return
	}
	s, err := hub.reader.Get(ctx, v, p.URI, true)
	if err == nil && s.URI != p.URI {
		err = skillhub.ErrNotFound
	}
	if err != nil {
		g.auditSkillRead(ctx, MethodSkillsGet, p.URI, "", err)
		e := skillRPCErrorFor(err, p.URI)
		if e.code == mcp.INVALID_PARAMS {
			e.msg = "Skill not found: " + clipText(p.URI, 300)
		}
		writeRPCError(w, req, e.code, e.msg)
		return
	}
	g.auditSkillRead(ctx, MethodSkillsGet, s.URI, s.Repo, nil)
	out := skillsGetResult{Skill: sepEntry(*s)}
	if req.modern {
		out.ResultType = mcp.ResultTypeComplete
	}
	writeRPCResult(w, req.id, out)
}

// directoryResult is a resources/directory/read answer.
type directoryResult struct {
	mcp.Result
	Resources []mcp.Resource `json:"resources"`
}

func (g *Gateway) skillsDirectoryRPC(ctx context.Context, w http.ResponseWriter, req taskRequest, hub *skillHubConfig, v skillhub.Viewer) {
	var p struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(req.params, &p) != nil || p.URI == "" {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "resources/directory/read needs params.uri: a directory URI")
		return
	}
	// Only the skill namespace is served as directories; anything else
	// is not a directory resource here.
	if !strings.HasPrefix(p.URI, skillScheme) {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "Resource not found: "+clipText(p.URI, 300))
		return
	}
	entries, err := hub.reader.ListDir(ctx, v, p.URI)
	if err != nil {
		g.auditSkillRead(ctx, MethodResourcesDirectoryRead, p.URI, "", err)
		e := skillRPCErrorFor(err, p.URI)
		writeRPCError(w, req, e.code, e.msg)
		return
	}
	g.auditSkillRead(ctx, MethodResourcesDirectoryRead, p.URI, "", nil)
	out := directoryResult{Resources: make([]mcp.Resource, 0, len(entries))}
	for _, e := range entries {
		r := mcp.Resource{URI: e.URI, Name: e.Name, MIMEType: mimeDirectory}
		if !e.IsDir {
			r.MIMEType = skillMIMEType(e.Name)
			if e.Size > 0 {
				size := e.Size
				r.Size = &size
			}
		}
		out.Resources = append(out.Resources, r)
	}
	if req.modern {
		out.ResultType = mcp.ResultTypeComplete
	}
	writeRPCResult(w, req.id, out)
}

// skillReadContext puts the resolved raiser on ctx for a skill read made
// outside a tool call over MCP, so its audit row names the agent, owner
// and client like a tool call's.
func (g *Gateway) skillReadContext(ctx context.Context) context.Context {
	return actor.WithRaiser(ctx, g.resolveRaiser(ctx, agentIDFromContext(ctx), viaDirect))
}

// auditSkillRead writes the audit row of one skill read outside a tool
// call: what was asked for (method or route), the URI and, when it was
// found, the version (owner/repo@commit). A refused read is recorded too.
func (g *Gateway) auditSkillRead(ctx context.Context, method, uri, repo string, err error) {
	if g.audit == nil {
		return
	}
	summary := uri
	if repo != "" {
		summary += " " + repo
	}
	decision := "allow"
	if err != nil {
		decision = "error"
		code, _ := skillsErrorText(err, uri)
		summary += " (" + code + ")"
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType: EventSkillRead, AgentID: agentIDFromContext(ctx), UpstreamName: skillsUpstream, ToolName: method,
		Decision: decision, ResultSummary: clipText(summary, 500),
	})
}

// skillMIMEType guesses a skill file's media type from its name. Skill
// files are text: anything unknown is text/plain.
func skillMIMEType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".py":
		return "text/x-python"
	case ".sh", ".bash":
		return "text/x-shellscript"
	case ".js", ".mjs", ".cjs":
		return "text/javascript"
	case ".ts":
		return "text/x-typescript"
	case ".html", ".htm":
		return "text/html"
	case ".csv":
		return "text/csv"
	case ".xml":
		return "application/xml"
	case ".toml":
		return "application/toml"
	}
	return "text/plain"
}

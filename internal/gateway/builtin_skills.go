package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// The skills hub's agent surface (docs/design/skills-hub.md, slice B).
// toolyard keeps a synced copy of the Agent Skills in its skill sources
// (internal/skillhub) and serves it to every client: the skills.* tools
// here (MCP, code mode, tools.execute, and REST through RouteCall), the
// MCP Skills extension (SEP-2640, skills_mcp.go) and a bundle download
// (skills_bundle.go). Every answer shows only what the caller may see:
// the Reader filters by the Viewer built from the caller
// (skills_viewer.go), so an invisible skill is simply not found.
//
// The tools live under the synthetic upstream "skills", an always-on
// access group (access.alwaysOn): every active caller has them, code mode
// binds them as toolyard.skills_<name>, and tools.search reports them
// under "skills". The read tools are allowed by policy like the other
// read built-ins; an explicit deny policy on a tool or on the skills
// upstream still switches them off (denyOverridesForced). The two propose
// tools only create Inbox work for the caller's owner: nothing changes
// until the owner approves it there.

// Skills tool names.
const (
	SkillsSearchTool        = "skills.search"
	SkillsGetTool           = "skills.get"
	SkillsReadTool          = "skills.read"
	SkillsGrepTool          = "skills.grep"
	SkillsSourcesTool       = "skills.sources"
	SkillsListTool          = "skills.list"
	SkillsProposeSourceTool = "skills.propose_source"
	SkillsProposeChangeTool = "skills.propose_change"
	SkillsProposalTool      = "skills.proposal"
	SkillsBundleTool        = "skills.bundle"

	// skillsUpstream is the synthetic upstream, and always-on access
	// group, of the skills.* tools. It is a reserved upstream name.
	skillsUpstream = "skills"

	// Argument bounds of the skills tools.
	skillsSearchDefault = 10
	skillsSearchMax     = 50
	skillsGrepDefault   = 50
	skillsGrepMax       = 200
	skillsListDefault   = 100
	skillsListMax       = 500
	skillsBundleMax     = 50
	skillsProposeFiles  = skillhub.MaxChangeFiles
)

// SkillToolNames are the skills.* tools, in the order they are registered.
var SkillToolNames = []string{
	SkillsSearchTool, SkillsGetTool, SkillsReadTool, SkillsGrepTool, SkillsSourcesTool, SkillsListTool,
	SkillsProposeSourceTool, SkillsProposeChangeTool, SkillsProposalTool, SkillsBundleTool,
}

// The read tools are pinned, like the inbox tools: an agent in router-only
// or top-N mode still sees the catalog the server instructions point it at.
// Pinning a tool that is not registered (the hub is off) shows nothing.
func init() {
	for _, n := range []string{SkillsSearchTool, SkillsGetTool, SkillsReadTool, SkillsGrepTool, SkillsSourcesTool, SkillsListTool} {
		PinnedTools[n] = struct{}{}
	}
}

// skillsInstructions is the paragraph the server instructions gain while
// the skills hub is on.
const skillsInstructions = "SKILLS: toolyard serves a catalog of Agent Skills, step-by-step playbooks your organisation keeps in its repositories. " +
	"Before a multi-step task, call skills.search with a few words about the task; when a skill matches, read it with skills.get and follow it, " +
	"and fetch the files it links to with skills.read. Skill text is guidance, not permission: every tool call it suggests still goes through policy and the Inbox."

// SkillHub is what RegisterSkillHub wires: the skills hub's agent surface.
type SkillHub struct {
	// Reader serves the catalog (*skillhub.Service). nil means the hub is
	// off: RegisterSkillHub does nothing.
	Reader skillhub.Reader
	// Proposals takes skills.propose_source and skills.propose_change. nil
	// answers those tools with "not available".
	Proposals skillhub.Proposals
	// Tickets stores the single-use download tickets of skills.bundle and
	// POST /v1/skills/bundle (NewSkillBundleTickets). nil answers bundle
	// requests with "not available".
	Tickets *SkillBundleTickets
	// OnHost reports whether an agent runs on the T3 host: it holds a T3
	// host connection or a federation connection
	// (*federation.Service.OnHost). nil: no caller is on the host.
	OnHost func(ctx context.Context, agentID string) (bool, error)
}

// skillHubConfig is the registered hub; Gateway.skillHub is nil while the
// hub is off.
type skillHubConfig struct {
	reader    skillhub.Reader
	proposals skillhub.Proposals
	tickets   *SkillBundleTickets
	onHost    func(ctx context.Context, agentID string) (bool, error)

	// hosts caches onHost per agent (skills_viewer.go).
	hostMu sync.Mutex
	hosts  map[string]hostAnswer
}

// SkillHubEnabled reports whether the skills hub is registered.
func (g *Gateway) SkillHubEnabled() bool { return g.skillHub.Load() != nil }

// RegisterSkillHub turns the skills hub's agent surface on: the skills.*
// tools, the MCP Skills extension (the capability on both protocol
// versions, the skill:// resource template, and the methods MCPSkills
// answers), and the skills paragraph of the server instructions. A nil
// Reader (the hub is off) does nothing, and so does a second call. Call it
// at start, after RegisterBuiltins and before the MCP endpoint serves: the
// capability and the instructions are part of the MCP server's fixed
// configuration.
func (g *Gateway) RegisterSkillHub(h SkillHub) {
	if h.Reader == nil {
		return
	}
	hub := &skillHubConfig{reader: h.Reader, proposals: h.Proposals, tickets: h.Tickets, onHost: h.OnHost, hosts: map[string]hostAnswer{}}
	if !g.skillHub.CompareAndSwap(nil, hub) {
		return
	}
	for _, e := range g.skillsTools() {
		g.registerEntry(e)
	}
	// tools.search names matching skills now: say so in its description.
	g.mu.RLock()
	search, ok := g.tools[MetaSearchTool]
	g.mu.RUnlock()
	if ok {
		search.tool.Description = metaSearchDescription(true)
		g.registerEntry(search)
	}
	// SEP-2640: the extension (with resources/directory/read) is declared
	// on the answers MCPSkills fronts, where its methods are served
	// (declareSkills); it requires the resources capability, declared for
	// every transport. A source whose active version changes is announced
	// as a resources list change, best effort: under -stateless-mcp no
	// session hears it, and hosts re-list on their own.
	if hooks := g.mcp.GetHooks(); hooks != nil {
		hooks.AddAfterInitialize(g.declareSkillsOnInitialize)
		hooks.AddAfterDiscover(g.declareSkillsOnDiscover)
	}
	server.WithResourceCapabilities(false, true)(g.mcp)
	server.WithInstructions(buildInstructions(g.approval, g.inLineWait) + " " + skillsInstructions)(g.mcp)
	g.mcp.AddResourceTemplate(skillResourceTemplate(), g.readSkillResource)
	h.Reader.Subscribe(func(string) { g.notifySkillsChanged() })
	g.NotifyToolListChanged()
}

// notifySkillsChanged tells connected MCP sessions that the skill
// resources changed.
func (g *Gateway) notifySkillsChanged() {
	if g == nil || g.mcp == nil {
		return
	}
	g.mcp.SendNotificationToAllClients(mcp.MethodNotificationResourcesListChanged, nil)
}

// skillsHandler is one skills tool's body: the registered hub and the
// caller as the hub sees it.
type skillsHandler func(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult

// skillsEntry builds one skills.* entry.
func (g *Gateway) skillsEntry(name, desc string, props map[string]any, required []string, h skillsHandler) toolEntry {
	allow := policy.ActionAllow
	return toolEntry{
		tool:         mcp.Tool{Name: name, Description: descriptionBanner + desc, InputSchema: obj(required, props)},
		upstream:     skillsUpstream,
		originalName: strings.TrimPrefix(name, skillsUpstream+"."),
		reasonField:  ReasonField,
		forcedAction: &allow,
		// Skills are data: an owner may switch them off for an agent with
		// an explicit deny policy, which beats the forced allow.
		denyOverridesForced: true,
		handle: func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			hub := g.skillHub.Load()
			if hub == nil {
				return skillsErrorResult(skillhub.ErrDisabled, ""), nil
			}
			return h(ctx, hub, g.skillViewer(ctx, hub), args), nil
		},
	}
}

func (g *Gateway) skillsTools() []toolEntry {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	integer := func(min, max int, desc string) map[string]any {
		p := map[string]any{"type": "integer", "minimum": min, "description": desc}
		if max > 0 {
			p["maximum"] = max
		}
		return p
	}
	sourceProp := str("Optional: only this source, by its slug (as skills.sources lists it).")
	hostProp := boolean("Also show skills that need the T3 host (runs: host): its paths and tools. Callers on the T3 host see them without it.")
	skillProp := str("The skill: its URI (skill://<source>/<path>/SKILL.md), <source>/<path>, or a bare name when only one skill you can see has it.")
	return []toolEntry{
		g.skillsEntry(SkillsSearchTool,
			"Search the skills catalog: playbooks your organisation keeps in its repositories, each a SKILL.md with supporting files. "+
				"Call it before a multi-step task with a few words about the task. mode keyword (default) ranks names, descriptions and paths; "+
				"regex matches an RE2 pattern against them. Each result names its skill URI, source, repo (owner/repo@commit) and where it runs; "+
				"read a match with skills.get.",
			map[string]any{
				"query":        str("Words that describe the task (keyword), or an RE2 pattern (regex)."),
				"mode":         map[string]any{"type": "string", "enum": []string{"keyword", "regex"}, "description": "keyword (default) or regex."},
				"source":       sourceProp,
				"limit":        integer(1, skillsSearchMax, fmt.Sprintf("Max results (default %d, at most %d).", skillsSearchDefault, skillsSearchMax)),
				"include_host": hostProp,
			}, []string{"query"}, g.handleSkillsSearch),
		g.skillsEntry(SkillsGetTool,
			"Read one skill: its SKILL.md body and frontmatter, the source and the commit it was copied from (repo is owner/repo@commit), "+
				"and the list of its files with their URIs, sizes and sha256 digests (not their content). Relative links in the body resolve "+
				"against root; read those files with skills.read.",
			map[string]any{
				"skill":         skillProp,
				"include_files": boolean("Include the file list (default true)."),
			}, []string{"skill"}, g.handleSkillsGet),
		g.skillsEntry(SkillsReadTool,
			"Read one file of a skill by its URI (from skills.get files[].uri, or a SKILL.md link resolved against the skill's root), "+
				fmt.Sprintf("optionally a range of lines. One call returns at most %d lines and %d KiB: when truncated is true, call again with offset next_offset.",
					skillsReadMaxLines, skillsReadMaxBytes>>10),
			map[string]any{
				"uri":    str("The file URI, e.g. skill://<source>/<path>/references/checklist.md."),
				"offset": integer(0, 0, "First line to return, 0-based (default 0)."),
				"limit":  integer(0, skillsReadMaxLines, fmt.Sprintf("Lines to return (default and most: %d).", skillsReadMaxLines)),
			}, []string{"uri"}, g.handleSkillsRead),
		g.skillsEntry(SkillsGrepTool,
			"Search the contents of the skill files you can see with an RE2 pattern, like grep: each hit names the file URI, the skill, "+
				"the line number and the line. Narrow it with source and a path glob (e.g. **/*.md, scripts/*).",
			map[string]any{
				"pattern":      str("RE2 regular expression matched against each line."),
				"source":       sourceProp,
				"path":         str("Optional glob on the file path inside the skill, e.g. **/*.md or scripts/*."),
				"ignore_case":  boolean("Match case-insensitively."),
				"limit":        integer(1, skillsGrepMax, fmt.Sprintf("Max hits (default %d, at most %d).", skillsGrepDefault, skillsGrepMax)),
				"include_host": hostProp,
			}, []string{"pattern"}, g.handleSkillsGrep),
		g.skillsEntry(SkillsSourcesTool,
			"List the skill sources you can see: each repository and branch toolyard copies skills from, the commit it serves now "+
				"(repo is owner/repo@commit), when it was synced and how many skills it holds.",
			map[string]any{}, nil, g.handleSkillsSources),
		g.skillsEntry(SkillsListTool,
			"List the skills you can see, a page at a time: each entry is the skill's URI, its frontmatter as written, and every file "+
				"with its URI, sha256 digest and size (the MCP skills/list entry), plus its source, repo and commit. Pass next_cursor back "+
				"as cursor for the next page. To find a skill for a task, skills.search is quicker.",
			map[string]any{
				"cursor":       str("The next_cursor of the page before."),
				"limit":        integer(1, skillsListMax, fmt.Sprintf("Skills per page (default %d, at most %d).", skillsListDefault, skillsListMax)),
				"source":       sourceProp,
				"include_host": hostProp,
			}, nil, g.handleSkillsList),
		g.skillsEntry(SkillsProposeSourceTool,
			"Ask your owner to add a GitHub repository as a skill source. This only creates an Inbox item for your owner with the "+
				"repository, branch, folder, a dry run of what it holds and your reason; nothing is added until they approve it. "+
				"Returns the pending proposal.",
			map[string]any{
				"url":    str("The repository: owner/repo, https://github.com/owner/repo, or a .../tree/<branch>/<path> URL."),
				"branch": str("Optional branch (default: the URL's, else the repository's default branch)."),
				"path":   str("Optional folder in the repository that holds the skills (default: the URL's, else the root)."),
				"reason": str("Why your owner should add it, shown in their Inbox. Default: this call's _reason."),
			}, []string{"url"}, g.handleSkillsProposeSource),
		g.skillsEntry(SkillsProposeChangeTool,
			"Propose an edit to one skill. This only creates an Inbox item for the skill source's owner with your title, summary and "+
				"the new file contents; after their approval toolyard opens a pull request on the source repository. Nothing changes "+
				"before that. Returns the pending proposal and the version of the skill it is based on.",
			map[string]any{
				"skill": skillProp,
				"files": map[string]any{
					"type": "object", "additionalProperties": map[string]any{"type": "string"}, "minProperties": 1, "maxProperties": skillsProposeFiles,
					"description": "Path inside the skill folder → its new full content. An empty string deletes the file.",
				},
				"title":   str("Short title of the change (the pull request title)."),
				"summary": str("What changes and why (the pull request body)."),
			}, []string{"skill", "files", "title"}, g.handleSkillsProposeChange),
		g.skillsEntry(SkillsProposalTool,
			"Read one of your skill proposals by its id (from skills.propose_source or skills.propose_change): its status "+
				"(pending, approved, done, rejected, failed), the result (the new source's id, or the pull request) or why it failed. "+
				"Your owner decides in their Inbox: poll this now and then instead of proposing again.",
			map[string]any{
				"id": str("The proposal id, skp_…, as the propose tool returned it."),
			}, []string{"id"}, g.handleSkillsProposal),
		g.skillsEntry(SkillsBundleTool,
			"Package skills as one .tar.gz in the Agent Skills layout (a folder per skill, named after it: two skills with one name "+
				"cannot share a bundle; at most 64 MiB of files), to install them in a local client such as "+
				"Claude Code (~/.claude/skills). Returns a short-lived download URL: it works once, for 10 minutes, without a token.",
			map[string]any{
				"skills": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": skillsBundleMax,
					"description": "The skills to package: URIs, <source>/<path>, or unique bare names.",
				},
			}, []string{"skills"}, g.handleSkillsBundle),
	}
}

// ---- handlers -------------------------------------------------------------

func (g *Gateway) handleSkillsSearch(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	q := skillhub.SearchQuery{
		Text:        strings.TrimSpace(stringArg(args, "query")),
		Mode:        strings.TrimSpace(stringArg(args, "mode")),
		Source:      strings.TrimSpace(stringArg(args, "source")),
		IncludeHost: boolArg(args, "include_host"),
	}
	if q.Text == "" {
		return skillsInvalid("query: give a few words about the task, or an RE2 pattern with mode regex; browse everything with skills.list")
	}
	switch q.Mode {
	case "":
		q.Mode = "keyword"
	case "keyword", "regex":
	default:
		return skillsInvalid(`mode must be "keyword" or "regex"`)
	}
	limit, bad := limitArg(args, skillsSearchDefault, skillsSearchMax)
	if bad != nil {
		return bad
	}
	q.Limit = limit
	hits, err := hub.reader.Search(ctx, v, q)
	if err != nil {
		return skillsErrorResult(err, q.Text)
	}
	if hits == nil {
		hits = []skillhub.SkillSummary{}
	}
	return skillsJSON(map[string]any{"query": q.Text, "mode": q.Mode, "total": len(hits), "skills": hits})
}

// skillView is a skill as skills.get answers it: the Reader's skill, the
// source slug at the top, and the skill's root (the folder relative links
// resolve against).
type skillView struct {
	*skillhub.Skill
	Root string `json:"root"`
}

func (g *Gateway) handleSkillsGet(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	ref := strings.TrimSpace(stringArg(args, "skill"))
	if ref == "" {
		return skillsInvalid("skill: give its URI (skill://…/SKILL.md), <source>/<path>, or its name")
	}
	includeFiles := true
	if b, ok := args["include_files"].(bool); ok {
		includeFiles = b
	}
	s, err := hub.reader.Get(ctx, v, ref, includeFiles)
	if err != nil {
		return skillsErrorResult(err, ref)
	}
	return skillsJSON(skillView{Skill: s, Root: skillRoot(s.URI)})
}

// fileView is a file as skills.read answers it: the Reader's file plus
// its source slug, and where the next part starts when it was truncated.
type fileView struct {
	*skillhub.File
	Source     string `json:"source"`
	NextOffset *int   `json:"next_offset,omitempty"`
}

func (g *Gateway) handleSkillsRead(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	uri := strings.TrimSpace(stringArg(args, "uri"))
	if uri == "" {
		return skillsInvalid("uri: give the file's URI, e.g. a files[].uri from skills.get")
	}
	offset, limit := 0, 0
	if n, ok := asInt(args["offset"]); ok {
		if n < 0 {
			return skillsInvalid("offset must be 0 or more")
		}
		offset = n
	}
	if n, ok := asInt(args["limit"]); ok {
		if n < 0 {
			return skillsInvalid("limit must be 0 or more")
		}
		limit = n
	}
	if limit == 0 || limit > skillsReadMaxLines {
		limit = skillsReadMaxLines
	}
	f, err := hub.reader.Read(ctx, v, uri, offset, limit)
	if err != nil {
		return skillsErrorResult(err, uri)
	}
	capReadBytes(f, skillsReadMaxBytes)
	out := fileView{File: f, Source: skillSourceOf(f.URI)}
	if f.Truncated {
		next := f.Offset + lineCount(f.Content)
		out.NextOffset = &next
	}
	return skillsJSON(out)
}

// One skills.read call returns at most skillsReadMaxLines lines and
// skillsReadMaxBytes bytes, so a large file comes back in parts that fit
// a model's context; resources/read (MCP Skills extension) always returns
// the whole file, whose bytes must match its digest.
const (
	skillsReadMaxLines = 2000
	skillsReadMaxBytes = 256 << 10
)

// capReadBytes cuts f's content after the last whole line within max
// bytes (always keeping its first line) and marks it truncated.
func capReadBytes(f *skillhub.File, max int) {
	if len(f.Content) <= max {
		return
	}
	cut := strings.LastIndexByte(f.Content[:max], '\n')
	if cut < 0 {
		// The first line alone is longer than max: return that line.
		if nl := strings.IndexByte(f.Content, '\n'); nl >= 0 && nl+1 < len(f.Content) {
			f.Content, f.Truncated = f.Content[:nl+1], true
		}
		return
	}
	f.Content, f.Truncated = f.Content[:cut+1], true
}

// grepHitView is one grep hit plus its source slug.
type grepHitView struct {
	skillhub.GrepHit
	Source string `json:"source"`
}

func (g *Gateway) handleSkillsGrep(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	q := skillhub.GrepQuery{
		Pattern:     stringArg(args, "pattern"),
		Source:      strings.TrimSpace(stringArg(args, "source")),
		PathGlob:    strings.TrimSpace(stringArg(args, "path")),
		IgnoreCase:  boolArg(args, "ignore_case"),
		IncludeHost: boolArg(args, "include_host"),
	}
	if strings.TrimSpace(q.Pattern) == "" {
		return skillsInvalid("pattern: give an RE2 regular expression")
	}
	limit, bad := limitArg(args, skillsGrepDefault, skillsGrepMax)
	if bad != nil {
		return bad
	}
	q.Limit = limit
	hits, err := hub.reader.Grep(ctx, v, q)
	if err != nil {
		return skillsErrorResult(err, q.Pattern)
	}
	views := make([]grepHitView, 0, len(hits))
	for _, h := range hits {
		views = append(views, grepHitView{GrepHit: h, Source: skillSourceOf(h.URI)})
	}
	return skillsJSON(map[string]any{"pattern": q.Pattern, "total": len(views), "hits": views})
}

// sourceView is a source as skills.sources answers it: the Reader's
// source plus the source slug and its root URI under the names every
// skills answer uses.
type sourceView struct {
	skillhub.SourceInfo
	Source string `json:"source"`
	URI    string `json:"uri"`
}

func (g *Gateway) handleSkillsSources(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, _ map[string]any) *mcp.CallToolResult {
	list, err := hub.reader.Sources(ctx, v)
	if err != nil {
		return skillsErrorResult(err, "")
	}
	views := make([]sourceView, 0, len(list))
	for _, s := range list {
		views = append(views, sourceView{SourceInfo: s, Source: s.Slug, URI: skillScheme + s.Slug})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Slug < views[j].Slug })
	return skillsJSON(map[string]any{"total": len(views), "sources": views})
}

// listEntryView is one skills.list entry: the MCP skills/list entry plus
// where it comes from.
type listEntryView struct {
	sepSkillEntry
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Source      string        `json:"source"`
	Path        string        `json:"path"`
	Repo        string        `json:"repo"`
	Commit      string        `json:"commit"`
	Runs        skillhub.Runs `json:"runs"`
}

func (g *Gateway) handleSkillsList(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	q := skillhub.ListQuery{
		Cursor:      strings.TrimSpace(stringArg(args, "cursor")),
		Source:      strings.TrimSpace(stringArg(args, "source")),
		IncludeHost: boolArg(args, "include_host"),
	}
	limit, bad := limitArg(args, skillsListDefault, skillsListMax)
	if bad != nil {
		return bad
	}
	q.Limit = limit
	list, next, err := hub.reader.List(ctx, v, q)
	if err != nil {
		return skillsErrorResult(err, "")
	}
	views := make([]listEntryView, 0, len(list))
	for _, s := range list {
		views = append(views, listEntryView{
			sepSkillEntry: sepEntry(s), Name: s.Name, Description: s.Description,
			Source: s.Source, Path: s.Path, Repo: s.Repo, Commit: s.Commit, Runs: s.Runs,
		})
	}
	out := map[string]any{"total": len(views), "skills": views}
	if next != "" {
		out["next_cursor"] = next
	}
	return skillsJSON(out)
}

func (g *Gateway) handleSkillsProposeSource(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.proposals == nil {
		return skillsUnavailable("skill source proposals are not available on this server yet: ask your owner to add the source on toolyard's Skills page")
	}
	if v.AgentID == "" {
		return skillsInvalid("a proposal needs an enrolled agent: connect with an agent token so your owner knows who asks")
	}
	in := skillhub.SourceInput{
		URL:    strings.TrimSpace(stringArg(args, "url")),
		Branch: strings.TrimSpace(stringArg(args, "branch")),
		Path:   strings.TrimSpace(stringArg(args, "path")),
	}
	if in.URL == "" {
		return skillsInvalid("url: give the repository as owner/repo or a github.com URL")
	}
	reason := strings.TrimSpace(stringArg(args, "reason"))
	if reason == "" {
		reason = callReasonFromContext(ctx)
	}
	p, err := hub.proposals.ProposeSource(ctx, v, in, reason)
	if err != nil {
		return skillsErrorResult(err, in.URL)
	}
	return skillsJSON(map[string]any{
		"proposal": p, "url": in.URL, "branch": in.Branch, "path": in.Path,
		"next": "Your owner decides in their Inbox; nothing is added before that. Continue other work; poll the outcome with skills.proposal (id " +
			p.ID + "), and skills.sources lists the source once it is added.",
	})
}

func (g *Gateway) handleSkillsProposal(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.proposals == nil {
		return skillsUnavailable("skill proposals are not available on this server")
	}
	if v.AgentID == "" {
		return skillsInvalid("a proposal belongs to the agent that made it: connect with that agent's token")
	}
	id := strings.TrimSpace(stringArg(args, "id"))
	if id == "" {
		return skillsInvalid("id: give the proposal id (skp_…) the propose tool returned")
	}
	p, err := hub.proposals.GetProposal(ctx, v, id)
	if errors.Is(err, skillhub.ErrNotFound) {
		return skillsError(SkillsErrNotFound, fmt.Sprintf("%q is not one of your proposals", clipText(id, 100)))
	}
	if err != nil {
		return skillsErrorResult(err, id)
	}
	return skillsJSON(map[string]any{"proposal": p, "next": skillsProposalNext(p)})
}

// skillsProposalNext tells the agent what a proposal's status means for it.
func skillsProposalNext(p *skillhub.Proposal) string {
	switch p.Status {
	case "pending":
		return "Your owner has not decided yet. Continue other work and poll again later; do not propose it again."
	case "approved":
		if p.Kind == "change" {
			return "Approved; toolyard is opening the pull request on GitHub (or checking what GitHub did after a lost answer). Poll again in a few minutes."
		}
		return "Your owner approved it; toolyard is applying it. Poll again in a minute."
	case "done":
		if p.Kind == "source" {
			return "Added as source " + p.Result + ". Its first sync runs in the background; skills.sources lists it, and skills.search finds its skills, once it finished."
		}
		if p.Kind == "change" {
			return "The pull request is open: " + p.Result + ". People review and merge it on GitHub; agents see the change once the merged commit is synced."
		}
		return "Done: " + p.Result
	case "rejected":
		return "Your owner did not take it (" + p.Error + "). Do not propose it again unless they ask."
	case "failed":
		return "It could not be applied (" + p.Error + "). Tell your owner; do not retry on your own."
	}
	return ""
}

func (g *Gateway) handleSkillsProposeChange(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.proposals == nil {
		return skillsUnavailable("skill change proposals are not available on this server yet: tell your owner what should change in the skill")
	}
	if v.AgentID == "" {
		return skillsInvalid("a proposal needs an enrolled agent: connect with an agent token so the source's owner knows who asks")
	}
	in := skillhub.ChangeInput{
		Skill:   strings.TrimSpace(stringArg(args, "skill")),
		Title:   strings.TrimSpace(stringArg(args, "title")),
		Summary: strings.TrimSpace(stringArg(args, "summary")),
		Files:   map[string]string{},
	}
	if in.Skill == "" {
		return skillsInvalid("skill: give its URI, <source>/<path>, or its name")
	}
	if in.Title == "" {
		return skillsInvalid("title: give a short title for the change")
	}
	raw, _ := args["files"].(map[string]any)
	for path, content := range raw {
		s, ok := content.(string)
		if !ok {
			return skillsInvalid("files: each value must be the file's new content as a string (an empty string deletes it)")
		}
		in.Files[path] = s
	}
	if len(in.Files) == 0 || len(in.Files) > skillsProposeFiles {
		return skillsInvalid(fmt.Sprintf("files: give 1 to %d files, path inside the skill folder → new content", skillsProposeFiles))
	}
	// The change is against the version the caller can see now: that
	// also proves the skill is visible before anyone is asked.
	s, err := hub.reader.Get(ctx, v, in.Skill, false)
	if err != nil {
		return skillsErrorResult(err, in.Skill)
	}
	in.Skill = s.URI
	p, err := hub.proposals.ProposeChange(ctx, v, in)
	if err != nil {
		return skillsErrorResult(err, in.Skill)
	}
	return skillsJSON(map[string]any{
		"proposal": p,
		"skill":    map[string]any{"uri": s.URI, "name": s.Name, "source": s.Source, "repo": s.Repo, "commit": s.Commit},
		"next":     "The source's owner decides in their Inbox; after approval toolyard opens a pull request. Continue other work; poll the outcome with skills.proposal (id " + p.ID + ").",
	})
}

func (g *Gateway) handleSkillsBundle(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	refs := stringList(args["skills"])
	b, err := g.issueSkillBundle(ctx, hub, v, refs)
	if err != nil {
		return skillsErrorResult(err, strings.Join(refs, ", "))
	}
	return skillsJSON(b)
}

// ---- results ----------------------------------------------------------------

// skillsJSON answers with v as JSON text and as structured content.
func skillsJSON(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return skillsErrorResult(err, "")
	}
	res := mcp.NewToolResultText(string(b))
	res.StructuredContent = v
	return res
}

// Error codes of a skills answer (structured content "error"). The REST
// routes map them to HTTP statuses (internal/api/skills_routes.go).
const (
	SkillsErrNotFound    = "not_found"
	SkillsErrConflict    = "conflict"
	SkillsErrInvalid     = "invalid"
	SkillsErrLimit       = "limit"
	SkillsErrUnavailable = "unavailable"
	SkillsErrInternal    = "internal"
	// SkillsErrRateLimited: GitHub's rate limit; the answer carries
	// retry_after (seconds) and reset_at (RFC 3339).
	SkillsErrRateLimited = "rate_limited"
)

// skillsError is a skills tool's refusal: a message for the model and a
// code for programs.
func skillsError(code, msg string) *mcp.CallToolResult {
	res := mcp.NewToolResultError(msg)
	res.StructuredContent = map[string]any{"error": code, "message": msg}
	return res
}

func skillsInvalid(msg string) *mcp.CallToolResult { return skillsError(SkillsErrInvalid, msg) }

func skillsUnavailable(msg string) *mcp.CallToolResult { return skillsError(SkillsErrUnavailable, msg) }

// skillsErrorResult turns a Reader or Proposals error into the agent's
// next step. ref is what the caller named, for the message.
func skillsErrorResult(err error, ref string) *mcp.CallToolResult {
	code, msg := skillsErrorText(err, ref)
	res := skillsError(code, msg)
	if until, ok := skillhub.RateLimitedUntil(err); ok {
		if m, ok := res.StructuredContent.(map[string]any); ok {
			m["retry_after"] = skillsRetryAfter(until)
			m["reset_at"] = until.UTC().Format(time.RFC3339)
		}
	}
	return res
}

// skillsRetryAfter is the whole seconds until t, at least 1.
func skillsRetryAfter(t time.Time) int {
	secs := int(time.Until(t).Seconds() + 0.999)
	return max(secs, 1)
}

// skillsErrorText is the code and the message for err.
func skillsErrorText(err error, ref string) (code, msg string) {
	switch {
	case errors.Is(err, skillhub.ErrRateLimited):
		until, _ := skillhub.RateLimitedUntil(err)
		return SkillsErrRateLimited, "GitHub's rate limit for toolyard is reached until " + until.UTC().Format("15:04 MST") +
			": try again after that, or ask your owner to add a GitHub token in toolyard (Skills, GitHub access)"
	case errors.Is(err, skillhub.ErrNotFound):
		if ref == "" {
			return SkillsErrNotFound, "not found among the skills you can see: find skills with skills.search or skills.list"
		}
		return SkillsErrNotFound, fmt.Sprintf("%q is not a skill or file you can see: find skills with skills.search or skills.list", clipText(ref, 300))
	case errors.Is(err, skillhub.ErrConflict):
		return SkillsErrConflict, skillsDetail(err)
	case errors.Is(err, skillhub.ErrChangeProposalsUnavailable):
		return SkillsErrUnavailable, "skill change proposals are not available on this server: tell your owner what should change in the skill"
	case errors.Is(err, skillhub.ErrNoGitHubConnection):
		// Before ErrInvalid: a missing GitHub connection is a setup the
		// agent cannot fix, not a bad request.
		return SkillsErrUnavailable, skillsDetail(err)
	case errors.Is(err, skillhub.ErrInvalid):
		return SkillsErrInvalid, skillsDetail(err)
	case errors.Is(err, skillhub.ErrNeedsToken):
		return SkillsErrInvalid, skillsDetail(err)
	case errors.Is(err, skillhub.ErrLimit):
		return SkillsErrLimit, skillsDetail(err)
	case errors.Is(err, skillhub.ErrDisabled):
		return SkillsErrUnavailable, "the skills hub is off on this server"
	case errors.Is(err, errSkillBundleOff):
		return SkillsErrUnavailable, errSkillBundleOff.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return SkillsErrUnavailable, "the skills catalog did not answer in time: call again in a few seconds"
	}
	return SkillsErrInternal, "the skills catalog could not answer (" + sanitizeServerError(err.Error()) + "): call again in a few seconds"
}

// skillsDetail is err's message for an agent: without the "skillhub: "
// package prefix and the sentinel's own words in front of the detail
// ("invalid input: ", "conflict: ", ...), capped.
func skillsDetail(err error) string {
	msg := err.Error()
	for _, sentinel := range []error{skillhub.ErrInvalid, skillhub.ErrConflict, skillhub.ErrLimit, skillhub.ErrNotFound} {
		if rest, ok := strings.CutPrefix(msg, sentinel.Error()+": "); ok {
			msg = rest
			break
		}
		if i := strings.Index(msg, ": "+sentinel.Error()+": "); i >= 0 {
			msg = msg[:i] + ": " + msg[i+len(sentinel.Error())+4:]
			break
		}
	}
	return clipText(strings.TrimPrefix(msg, "skillhub: "), 500)
}

// limitArg reads an optional limit: def when absent, at most max, at least 1.
func limitArg(args map[string]any, def, max int) (int, *mcp.CallToolResult) {
	n, ok := asInt(args["limit"])
	if !ok {
		return def, nil
	}
	if n < 1 {
		return 0, skillsInvalid("limit must be at least 1")
	}
	return min(n, max), nil
}

// ---- skill URIs -------------------------------------------------------------

// skillScheme prefixes every skill URI: skill://<source>/<path>/<file>.
const skillScheme = "skill://"

// skillSourceOf is the source slug of a skill URI ("" when uri is not one).
func skillSourceOf(uri string) string {
	rest, ok := strings.CutPrefix(uri, skillScheme)
	if !ok {
		return ""
	}
	slug, _, _ := strings.Cut(rest, "/")
	return slug
}

// skillRoot is the skill's folder URI: its SKILL.md URI without
// "/SKILL.md" (SEP-2640: no trailing slash).
func skillRoot(skillURI string) string {
	return strings.TrimSuffix(skillURI, "/"+skillFileName)
}

// skillFileName is the file that makes a folder a skill.
const skillFileName = "SKILL.md"

// lineCount is how many lines text holds (a last line without a newline
// counts).
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// skillsCallTimeout bounds the catalog lookups toolyard makes on its own
// (tools.search's skills section).
const skillsCallTimeout = 2 * time.Second

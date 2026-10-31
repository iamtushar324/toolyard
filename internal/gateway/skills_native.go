package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// The native skills registry's agent surface (skillhub/native.go): agents
// write skills into native collections with skills.create and
// skills.update, and read a skill's history with skills.versions (and
// skills.get or skills.read with a revision). Who may write which
// collection, and which writes wait for the collection owner's approval in
// the Inbox, is the registry's rule; these tools only carry the call. A
// write is live for every reader when it answers "live".

// Native skills tool names.
const (
	SkillsCreateTool   = "skills.create"
	SkillsUpdateTool   = "skills.update"
	SkillsVersionsTool = "skills.versions"
	// SkillsCreateCollectionTool creates a native collection (admins'
	// agents only).
	SkillsCreateCollectionTool = "skills.create_collection"

	skillsMaxListArg      = 20
	skillsVersionsMax     = skillhub.MaxRevisionsList
	skillsVersionsDefault = 20
)

// skillsNativeEntries are the native registry's tools.
func (g *Gateway) skillsNativeEntries() []toolEntry {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	list := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": skillsMaxListArg, "description": desc}
	}
	files := func(desc string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "maxProperties": skillsProposeFiles, "description": desc}
	}
	note := str("What changed and why, kept with the revision (default: this call's _reason).")
	create := g.skillsEntry(SkillsCreateTool,
		"Add a skill to a native collection (a skills source kept in toolyard, kind native in skills.sources). Give name, description, "+
			"instructions and optional triggers and tags, or a full SKILL.md in files. Returns the skill's uri and revision; it is live at once, "+
			"unless the change needs the collection owner's approval (status pending_approval, then poll skills.proposal).",
		map[string]any{
			"collection":   str("The native collection's slug."),
			"name":         str("The skill's name: lowercase letters, digits and hyphens; also its folder."),
			"description":  str("What the skill does and when to use it (at most 1024 characters): agents choose skills by it."),
			"instructions": str("The SKILL.md body: the steps, in Markdown."),
			"triggers":     list("When to use it, a phrase each (\"the user asks to roll back a deploy\")."),
			"tags":         list("Short labels for filtering (skills.recommend tags)."),
			"path":         str("Optional folder in the collection (default: the name); its last folder is the name."),
			"files":        files("Optional other files: path in the skill folder → content (e.g. references/checklist.md). Include SKILL.md here instead of name/description/instructions to write it yourself."),
			"note":         note,
		}, []string{"collection"}, g.handleSkillsCreate)
	update := g.skillsEntry(SkillsUpdateTool,
		"Change a native skill: give only what changes (description, instructions, triggers, tags) and/or files (path → full new content; \"\" deletes). "+
			"Pass base_revision (the revision skills.get showed) to refuse the write if someone changed the skill since. Returns the new revision, "+
			"or status pending_approval when the collection owner must approve it.",
		map[string]any{
			"skill":         str("The skill: its URI, <collection>/<path>, or a unique name."),
			"description":   str("New description."),
			"instructions":  str("New SKILL.md body (replaces it)."),
			"triggers":      list("New triggers (replace them; [] removes them)."),
			"tags":          list("New tags (replace them; [] removes them)."),
			"files":         files("Files to write: path in the skill folder → full new content; \"\" deletes the file."),
			"base_revision": map[string]any{"type": "integer", "minimum": 1, "description": "The revision you read; the write fails if the skill moved on."},
			"note":          note,
		}, []string{"skill"}, g.handleSkillsUpdate)
	versions := g.skillsEntry(SkillsVersionsTool,
		"List a native skill's revisions, newest first: who wrote each (person and agent), when, the note, and which is live. "+
			"Read an old revision with skills.get or skills.read and revision.",
		map[string]any{
			"skill": str("The skill: its URI, <collection>/<path>, or a unique name."),
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": skillsVersionsMax,
				"description": fmt.Sprintf("Max revisions (default %d, at most %d).", skillsVersionsDefault, skillsVersionsMax)},
		}, []string{"skill"}, g.handleSkillsVersions)
	versions.tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(true)
	collection := g.skillsEntry(SkillsCreateCollectionTool,
		"Create a native skill collection (a skills source kept in toolyard) to write skills into with skills.create. "+
			"Only an agent whose owner is an admin may; the owner becomes its creator and approves its agents' sensitive writes.",
		map[string]any{
			"slug":         str("The collection's name in every skill URI (skill://<slug>/...): lowercase letters, digits and hyphens."),
			"visibility":   map[string]any{"type": "string", "enum": []string{"everyone", "members", "nobody"}, "description": "Who sees its skills (default everyone)."},
			"members":      list("User IDs who see it, with visibility members."),
			"blocked":      list("User IDs who never see it."),
			"editors":      list("User IDs whose agents may write its skills (besides the creator and admins)."),
			"mode":         map[string]any{"type": "string", "enum": []string{"auto", "review"}, "description": "auto (default): editors' safe writes go live at once; review: every agent write waits for the creator."},
			"default_runs": map[string]any{"type": "string", "enum": []string{"anywhere", "host"}, "description": "Where its skills run by default (default anywhere)."},
		}, []string{"slug"}, g.handleSkillsCreateCollection)
	return []toolEntry{create, update, versions, collection}
}

func (g *Gateway) handleSkillsCreateCollection(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.registry == nil {
		return skillsUnavailable("native skills are not available on this server")
	}
	in := skillhub.SourceInput{
		Slug:        strings.TrimSpace(stringArg(args, "slug")),
		Visibility:  skillhub.Visibility(strings.TrimSpace(stringArg(args, "visibility"))),
		Mode:        skillhub.UpdateMode(strings.TrimSpace(stringArg(args, "mode"))),
		DefaultRuns: skillhub.Runs(strings.TrimSpace(stringArg(args, "default_runs"))),
	}
	if in.Slug == "" {
		return skillsInvalid("slug: give the collection's name, lowercase letters, digits and hyphens")
	}
	for key, dst := range map[string]*[]string{"members": &in.Members, "blocked": &in.Blocked, "editors": &in.Editors} {
		l, bad := skillsListArg(args, key)
		if bad != nil {
			return bad
		}
		if l != nil {
			*dst = *l
		}
	}
	src, err := hub.registry.CreateCollection(ctx, v, in)
	if err != nil {
		return skillsErrorResult(err, in.Slug)
	}
	return skillsJSON(map[string]any{
		"collection": map[string]any{"id": src.ID, "slug": src.Slug, "kind": src.Kind, "mode": src.Mode, "visibility": src.Visibility,
			"members": src.Members, "blocked": src.Blocked, "editors": src.Editors, "default_runs": src.DefaultRuns, "created_by": src.CreatedBy},
		"next": "Write its first skill with skills.create {\"collection\": \"" + src.Slug + "\", ...}.",
	})
}

// skillsFilesArg reads an optional files object: path → content.
func skillsFilesArg(args map[string]any) (map[string]string, *mcp.CallToolResult) {
	out := map[string]string{}
	raw, present := args["files"]
	if !present || raw == nil {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, skillsInvalid("files: give an object, path in the skill folder → content")
	}
	if len(m) > skillsProposeFiles {
		return nil, skillsInvalid(fmt.Sprintf("files: at most %d files", skillsProposeFiles))
	}
	for p, content := range m {
		s, ok := content.(string)
		if !ok {
			return nil, skillsInvalid("files: each value must be the file's content as a string (an empty string deletes it)")
		}
		out[p] = s
	}
	return out, nil
}

// skillsListArg reads an optional list of strings: nil when absent.
func skillsListArg(args map[string]any, key string) (*[]string, *mcp.CallToolResult) {
	raw, present := args[key]
	if !present || raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, skillsInvalid(key + ": give an array of strings")
	}
	if len(arr) > skillsMaxListArg {
		return nil, skillsInvalid(fmt.Sprintf("%s: at most %d", key, skillsMaxListArg))
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, skillsInvalid(key + ": give an array of strings")
		}
		out = append(out, s)
	}
	return &out, nil
}

// skillsStringPtr reads an optional string: nil when absent.
func skillsStringPtr(args map[string]any, key string) *string {
	if s, ok := args[key].(string); ok {
		return &s
	}
	return nil
}

// skillsNote is the note of a write: the note argument, else the call's
// reason.
func skillsNote(ctx context.Context, args map[string]any) string {
	if n := strings.TrimSpace(stringArg(args, "note")); n != "" {
		return n
	}
	return callReasonFromContext(ctx)
}

func (g *Gateway) handleSkillsCreate(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.registry == nil {
		return skillsUnavailable("native skills are not available on this server")
	}
	files, bad := skillsFilesArg(args)
	if bad != nil {
		return bad
	}
	triggers, bad := skillsListArg(args, "triggers")
	if bad != nil {
		return bad
	}
	tags, bad := skillsListArg(args, "tags")
	if bad != nil {
		return bad
	}
	name := strings.TrimSpace(stringArg(args, "name"))
	desc := strings.TrimSpace(stringArg(args, "description"))
	body := stringArg(args, "instructions")
	if _, own := files["SKILL.md"]; own {
		if name != "" || desc != "" || body != "" || triggers != nil || tags != nil {
			return skillsInvalid("give SKILL.md in files, or name, description, instructions, triggers and tags: not both")
		}
	} else {
		if name == "" || desc == "" {
			return skillsInvalid("name and description are required (or give a full SKILL.md in files)")
		}
		d := skillhub.SkillDraft{Name: name, Description: desc, Body: body}
		if triggers != nil {
			d.Triggers = *triggers
		}
		if tags != nil {
			d.Tags = *tags
		}
		md, err := skillhub.ComposeSkillMD(d)
		if err != nil {
			return skillsErrorResult(err, name)
		}
		files["SKILL.md"] = md
	}
	w := skillhub.SkillWrite{
		Collection: strings.TrimSpace(stringArg(args, "collection")),
		Path:       strings.TrimSpace(stringArg(args, "path")),
		Files:      files,
		Note:       skillsNote(ctx, args),
	}
	if w.Path == "" {
		w.Path = name
	}
	res, err := hub.registry.WriteSkill(ctx, v, w)
	if err != nil {
		return skillsErrorResult(err, w.Collection)
	}
	return skillsJSON(skillsWriteView(res))
}

func (g *Gateway) handleSkillsUpdate(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.registry == nil {
		return skillsUnavailable("native skills are not available on this server")
	}
	ref := strings.TrimSpace(stringArg(args, "skill"))
	if ref == "" {
		return skillsInvalid("skill: give its URI, <collection>/<path>, or its name")
	}
	files, bad := skillsFilesArg(args)
	if bad != nil {
		return bad
	}
	triggers, bad := skillsListArg(args, "triggers")
	if bad != nil {
		return bad
	}
	tags, bad := skillsListArg(args, "tags")
	if bad != nil {
		return bad
	}
	patch := skillhub.SkillPatch{Description: skillsStringPtr(args, "description"), Body: skillsStringPtr(args, "instructions"),
		Triggers: triggers, Tags: tags}
	base := 0
	if raw, present := args["base_revision"]; present && raw != nil {
		n, ok := exactInt(raw)
		if !ok || n < 1 {
			return skillsInvalid("base_revision must be a whole number, 1 or more: the revision skills.get showed")
		}
		base = n
	}
	if !patch.IsZero() {
		if _, own := files["SKILL.md"]; own {
			return skillsInvalid("give SKILL.md in files, or description, instructions, triggers and tags: not both")
		}
		// The parts change the SKILL.md the caller sees now; base_revision
		// (or, without it, that same revision) keeps a write in between
		// from being overwritten.
		cur, err := hub.reader.Get(ctx, v, ref, false)
		if err != nil {
			return skillsErrorResult(err, ref)
		}
		if base == 0 {
			base = cur.Revision
		}
		md, err := skillhub.PatchSkillMD(cur.Raw, patch)
		if err != nil {
			return skillsErrorResult(err, ref)
		}
		files["SKILL.md"] = md
		ref = cur.URI
	}
	if len(files) == 0 {
		return skillsInvalid("give what changes: description, instructions, triggers, tags or files")
	}
	res, err := hub.registry.WriteSkill(ctx, v, skillhub.SkillWrite{Skill: ref, Files: files, BaseRevision: base, Note: skillsNote(ctx, args)})
	if err != nil {
		return skillsErrorResult(err, ref)
	}
	return skillsJSON(skillsWriteView(res))
}

// skillsWriteView is a write's answer with what to do next.
func skillsWriteView(res *skillhub.WriteResult) map[string]any {
	out := map[string]any{"result": res}
	switch res.Status {
	case skillhub.WritePending:
		id := ""
		if res.Proposal != nil {
			id = res.Proposal.ID
		}
		out["next"] = "Nothing changed yet: the collection's owner decides in their Inbox. Continue other work; poll skills.proposal (id " + id +
			"), which reports done with the new revision once they approve."
	default:
		out["next"] = "Live now for everyone who can see the collection: skills.get " + res.URI + " reads it."
	}
	return out
}

func (g *Gateway) handleSkillsVersions(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	if hub.registry == nil {
		return skillsUnavailable("native skills are not available on this server")
	}
	ref := strings.TrimSpace(stringArg(args, "skill"))
	if ref == "" {
		return skillsInvalid("skill: give its URI, <collection>/<path>, or its name")
	}
	limit, bad := limitArg(args, skillsVersionsDefault, skillsVersionsMax)
	if bad != nil {
		return bad
	}
	revs, err := hub.registry.SkillRevisions(ctx, v, ref, limit)
	if err != nil {
		return skillsErrorResult(err, ref)
	}
	return skillsJSON(map[string]any{"skill": ref, "total": len(revs), "revisions": revs})
}

// exactInt reads a whole number: an integer type, a float64 with no
// fraction, or a json.Number that is an integer. Anything else (a string,
// 2.5) is not one, so a mistyped argument never turns a check off.
func exactInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) || x > math.MaxInt32 || x < math.MinInt32 {
			return 0, false
		}
		return int(x), true
	case json.Number:
		n, err := x.Int64()
		if err != nil || n > math.MaxInt32 || n < math.MinInt32 {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// skillsRevisionArg reads an optional revision argument: 0 when absent.
func skillsRevisionArg(args map[string]any) (int, *mcp.CallToolResult) {
	raw, present := args["revision"]
	if !present || raw == nil {
		return 0, nil
	}
	n, ok := exactInt(raw)
	if !ok || n < 1 {
		return 0, skillsInvalid("revision must be a whole number, 1 or more (skills.versions lists them)")
	}
	return n, nil
}

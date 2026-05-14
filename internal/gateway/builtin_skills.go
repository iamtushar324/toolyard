package gateway

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// SkillsPublisher is the gateway-side dependency for the skills.publish
// and skills.install built-in tools. Implemented by *internal/skills.Service.
// The interface is declared here so internal/gateway doesn't import
// internal/skills (which itself imports gateway indirectly via Dispatcher).
type SkillsPublisher interface {
	Publish(ctx context.Context, agentID, slug, skillMD, agentsYAML string, scripts map[string]string, topic string) (any, error)
	Install(ctx context.Context, agentID, slug, mode, target string) (any, error)
	List(ctx context.Context, tagFilter []string) (any, error)
	ListTags(ctx context.Context) (any, error)
	Get(ctx context.Context, slug string, includeFiles bool) (any, error)
	Bundle(ctx context.Context, slugs []string) (any, error)
	SkillsDir() string
}

// skillsPublishTool builds the toolEntry for skills.publish. Auto-allowed
// (forcedAction = Allow) so capture doesn't block on a human tap — same
// rationale as notes.publish. The skill goes to the centralised store; if
// the human wants it visible to a local Claude install they call
// skills.install separately, which is approval-gated.
func (g *Gateway) skillsPublishTool(sp SkillsPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "skills.publish",
		Description: descriptionBanner +
			"Write a Claude Code skill into the toolyard skills workspace AND index it into MemPalace in one call. " +
			"`skill_md` is the full SKILL.md file (YAML frontmatter `name`/`description` + markdown body). " +
			"Slug is derived from frontmatter.name unless you pass `slug` explicitly (must match). " +
			"Optional `agents_openai_yaml` writes agents/openai.yaml; optional `scripts` (map of relative path under scripts/ → content) seeds helper scripts. " +
			"Indexing is best-effort: the files are always written; if MemPalace is briefly unavailable the next background sync picks the skill up. " +
			"This DOES NOT make the skill visible to a local Claude install — call skills.install for that.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "skill_md"},
			Properties: addMetaProps(map[string]any{
				"slug": map[string]any{
					"type":        "string",
					"description": "Optional slug override. Must equal kebab-case(frontmatter.name).",
				},
				"skill_md": map[string]any{
					"type":        "string",
					"description": "Full SKILL.md body, frontmatter (between --- markers) followed by markdown.",
				},
				"agents_openai_yaml": map[string]any{
					"type":        "string",
					"description": "Optional contents of agents/openai.yaml.",
				},
				"scripts": map[string]any{
					"type":                 "object",
					"description":          "Optional map of relative path under scripts/ → file content. Keys may not contain `..` segments.",
					"additionalProperties": map[string]any{"type": "string"},
				},
				"topic": map[string]any{
					"type":        "string",
					"description": "Optional MemPalace topic; defaults to `skills`.",
				},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		slug, _ := args["slug"].(string)
		skillMD, _ := args["skill_md"].(string)
		agentsYAML, _ := args["agents_openai_yaml"].(string)
		topic, _ := args["topic"].(string)
		scripts := map[string]string{}
		if raw, ok := args["scripts"].(map[string]any); ok {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					scripts[k] = s
				}
			}
		}
		agentID := AgentIDFromContext(ctx)
		res, err := sp.Publish(ctx, agentID, slug, skillMD, agentsYAML, scripts, topic)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.publish failed", err), nil
		}
		body, _ := json.Marshal(res)
		out := mcp.NewToolResultText(string(body))
		if m, ok := res.(map[string]any); ok {
			out.StructuredContent = m
		}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.publish",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// skillsInstallTool builds the toolEntry for skills.install. **Explicit**
// approval — the name "install" would otherwise read as benign to the
// policy name heuristic, but install writes outside the store (typically
// into ~/.claude/skills) and the human should always see it.
func (g *Gateway) skillsInstallTool(sp SkillsPublisher) toolEntry {
	approve := policy.ActionApprove
	t := mcp.Tool{
		Name: "skills.install",
		Description: descriptionBanner +
			"Make a skill from the toolyard skills workspace visible to a local Claude Code install. " +
			"`slug` is the skill's directory name under the store. " +
			"`mode` is `link` (default; symlink into target) or `copy` (full filesystem copy, safer for repos that pin a version). " +
			"`target` is the parent directory the skill is installed under (default: the gateway's configured ClaudeSkillsDir, typically ~/.claude/skills). " +
			"Idempotent on a symlink that already points at the same source; otherwise refuses to clobber.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "slug"},
			Properties: addMetaProps(map[string]any{
				"slug":   map[string]any{"type": "string"},
				"mode":   map[string]any{"type": "string", "enum": []any{"link", "copy"}},
				"target": map[string]any{"type": "string", "description": "Parent directory to install under. Tildes are expanded."},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		slug, _ := args["slug"].(string)
		mode, _ := args["mode"].(string)
		target, _ := args["target"].(string)
		agentID := AgentIDFromContext(ctx)
		res, err := sp.Install(ctx, agentID, slug, mode, target)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.install failed", err), nil
		}
		body, _ := json.Marshal(res)
		out := mcp.NewToolResultText(string(body))
		if m, ok := res.(map[string]any); ok {
			out.StructuredContent = m
		}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.install",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &approve,
	}
}

// skillsListTagsTool is the entrypoint for catalog browsing — "what
// kinds of skill are in this store?". Returns a tag→count map so the
// agent can pick the most relevant category before pulling descriptions.
func (g *Gateway) skillsListTagsTool(sp SkillsPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "skills.list_tags",
		Description: descriptionBanner +
			"List every tag in the toolyard skills workspace, with the count of skills carrying each. Use this FIRST when looking for a skill — it's the cheapest way to discover what categories exist (e.g. `coding`, `personal-life`, `ai-tooling`). Follow up with skills.list(tags=[...]) to fetch descriptions of the skills under the categories that match.",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Required:   []string{ReasonField},
			Properties: addMetaProps(map[string]any{}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		res, err := sp.ListTags(ctx)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.list_tags failed", err), nil
		}
		body, _ := json.Marshal(map[string]any{"tags": res})
		out := mcp.NewToolResultText(string(body))
		out.StructuredContent = map[string]any{"tags": res}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.list_tags",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// skillsListTool returns slug + name + description + tags for every skill,
// optionally filtered by tag. Step 2 of the catalog-browse flow — read
// descriptions to decide which skill body to actually pull.
func (g *Gateway) skillsListTool(sp SkillsPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "skills.list",
		Description: descriptionBanner +
			"List skills in the toolyard skills workspace with their slug, frontmatter name, description, and tags. Pass `tags` to filter to skills matching ANY of the given tags (post-kebab-case normalisation). Use after skills.list_tags has told you which categories are worth exploring. Output is sorted by slug. To get the raw SKILL.md body for a specific skill, call skills.get.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"tags": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional filter — keep only skills carrying at least one of these tags.",
				},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		tags := []string{}
		if raw, ok := args["tags"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		res, err := sp.List(ctx, tags)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.list failed", err), nil
		}
		body, _ := json.Marshal(map[string]any{"skills": res})
		out := mcp.NewToolResultText(string(body))
		out.StructuredContent = map[string]any{"skills": res}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.list",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// skillsGetTool returns the raw SKILL.md body for one skill, optionally
// with companion files (agents/openai.yaml + scripts/*). Companion files
// are off by default to keep responses small — agents pull them only when
// they actually intend to use the skill.
func (g *Gateway) skillsGetTool(sp SkillsPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "skills.get",
		Description: descriptionBanner +
			"Fetch a skill's raw SKILL.md body by slug. Set `include_files=true` to also inline agents/openai.yaml and every file under scripts/ — useful when you want to copy the skill verbatim or audit it before install. Step 3 of the browse flow (list_tags → list → get).",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "slug"},
			Properties: addMetaProps(map[string]any{
				"slug":          map[string]any{"type": "string"},
				"include_files": map[string]any{"type": "boolean", "description": "When true, inline agents/openai.yaml and scripts/* content. Default false."},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		slug, _ := args["slug"].(string)
		includeFiles, _ := args["include_files"].(bool)
		res, err := sp.Get(ctx, slug, includeFiles)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.get failed", err), nil
		}
		body, _ := json.Marshal(res)
		out := mcp.NewToolResultText(string(body))
		if m, ok := res.(map[string]any); ok {
			out.StructuredContent = m
		}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.get",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// skillsBundleTool returns a base64-encoded gzipped tar of one or more
// skills' directories — the bulk-download endpoint. Empty `slugs` bundles
// the whole store. Decode with `base64 -d | tar -xzf -`. Capped server-side
// so a confused caller can't accidentally pull gigabytes.
func (g *Gateway) skillsBundleTool(sp SkillsPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "skills.bundle",
		Description: descriptionBanner +
			"Download one or more skills as a single base64-encoded `.tar.gz`. Pass `slugs` (array) to bundle specific skills; omit/empty to bundle every skill in the store. Response is `{slugs, tar_gz_b64, bytes, raw_bytes}`. Decode + extract on the receiving side with `base64 -d <<<\"$tar_gz_b64\" | tar -xzf -`. Soft-capped at 16 MiB raw; narrow the slug list if you hit that.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"slugs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Skills to include. Empty or omitted = every skill in the store.",
				},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		slugs := []string{}
		if raw, ok := args["slugs"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					slugs = append(slugs, s)
				}
			}
		}
		res, err := sp.Bundle(ctx, slugs)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("skills.bundle failed", err), nil
		}
		body, _ := json.Marshal(res)
		out := mcp.NewToolResultText(string(body))
		if m, ok := res.(map[string]any); ok {
			out.StructuredContent = m
		}
		return out, nil
	})
	return toolEntry{
		tool:         t,
		upstream:     builtinUpstream,
		originalName: "skills.bundle",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// RegisterSkillsBuiltins wires the skills.* built-ins if sp != nil.
// Safe to call once after RegisterBuiltins.
func (g *Gateway) RegisterSkillsBuiltins(sp SkillsPublisher) {
	if sp == nil {
		return
	}
	g.registerEntry(g.skillsPublishTool(sp))
	g.registerEntry(g.skillsInstallTool(sp))
	g.registerEntry(g.skillsListTagsTool(sp))
	g.registerEntry(g.skillsListTool(sp))
	g.registerEntry(g.skillsGetTool(sp))
	g.registerEntry(g.skillsBundleTool(sp))
}

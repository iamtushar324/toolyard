package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skills.recommend: the caller's skills ranked for a task it describes in
// plain words (skillhub.Service.Recommend). Each answer names the skill's
// URI (load it with skills.get), why it matched and when to use it, and
// says what ranked it (Jev, or keyword with the fallback's reason).

// SkillsRecommendTool ranks skills for a task.
const SkillsRecommendTool = "skills.recommend"

// Argument bounds of skills.recommend.
const (
	skillsRecommendDefault = 5
	skillsRecommendMax     = 20
	skillsRecommendMaxTags = 10
	skillsRecommendMaxTask = 1000
)

// skillsRecommendEntry is the skills.recommend entry.
func (g *Gateway) skillsRecommendEntry() toolEntry {
	e := g.skillsEntry(SkillsRecommendTool,
		"Describe your task in natural language; get the skills that fit it, ranked, with why each matches and when to use it. "+
			"Then load one with skills.get using its uri.",
		map[string]any{
			"task": map[string]any{"type": "string", "maxLength": skillsRecommendMaxTask,
				"description": "What you are about to do, in a sentence or two."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": skillsRecommendMax,
				"description": fmt.Sprintf("Max skills (default %d, at most %d).", skillsRecommendDefault, skillsRecommendMax)},
			"source": map[string]any{"type": "string", "description": "Optional: only this source, by its slug."},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": skillsRecommendMaxTags,
				"description": "Optional: only skills with any of these tags."},
			"include_host": map[string]any{"type": "boolean", "description": "Also rank skills that need the T3 host (runs: host)."},
			"mode": map[string]any{"type": "string", "enum": []string{"auto", "keyword", "semantic"},
				"description": "auto (default): by meaning when the server's ranker is on, else keyword."},
		}, []string{"task"}, g.handleSkillsRecommend)
	// It reads the catalog and changes nothing.
	e.tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(true)
	return e
}

func (g *Gateway) handleSkillsRecommend(ctx context.Context, hub *skillHubConfig, v skillhub.Viewer, args map[string]any) *mcp.CallToolResult {
	q := skillhub.RecommendQuery{
		Task:        strings.TrimSpace(stringArg(args, "task")),
		Source:      strings.TrimSpace(stringArg(args, "source")),
		IncludeHost: boolArg(args, "include_host"),
		Mode:        strings.ToLower(strings.TrimSpace(stringArg(args, "mode"))),
	}
	if q.Task == "" {
		return skillsInvalid("task: describe what you are about to do in a sentence or two")
	}
	switch q.Mode {
	case "", "auto", "keyword", "semantic":
	default:
		return skillsInvalid(`mode must be "auto", "keyword" or "semantic"`)
	}
	limit, bad := limitArg(args, skillsRecommendDefault, skillsRecommendMax)
	if bad != nil {
		return bad
	}
	q.Limit = limit
	tags, ok := skillsTagsArg(args["tags"])
	if !ok {
		return skillsInvalid("tags: give an array of strings")
	}
	if len(tags) > skillsRecommendMaxTags {
		return skillsInvalid(fmt.Sprintf("tags: at most %d", skillsRecommendMaxTags))
	}
	q.Tags = tags
	res, err := hub.reader.Recommend(ctx, v, q)
	if err != nil {
		return skillsErrorResult(err, "")
	}
	if res.Skills == nil {
		res.Skills = []skillhub.Recommendation{}
	}
	return skillsJSON(res)
}

// skillsTagsArg reads the optional tags argument: absent, or an array of
// strings (empty ones dropped). ok is false for anything else.
func skillsTagsArg(v any) (tags []string, ok bool) {
	switch x := v.(type) {
	case nil:
		return nil, true
	case []string:
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				tags = append(tags, s)
			}
		}
		return tags, true
	case []any:
		for _, item := range x {
			s, isStr := item.(string)
			if !isStr {
				return nil, false
			}
			if s = strings.TrimSpace(s); s != "" {
				tags = append(tags, s)
			}
		}
		return tags, true
	}
	return nil, false
}

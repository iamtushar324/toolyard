package gateway

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// NotesPublisher is the gateway-side dependency for the notes.publish
// built-in tool. Implemented by *internal/notes.Service. The interface is
// declared here so internal/gateway doesn't import internal/notes
// (which itself imports gateway indirectly through Dispatcher).
type NotesPublisher interface {
	Publish(ctx context.Context, agentID, path, content, topic string) (any, error)
	NotesDir() string
}

// notesPublishTool builds the toolEntry for the notes.publish built-in.
// Auto-allowed (forcedAction = Allow) so capture doesn't block on a
// human tap — same reasoning as /v1/mempalace/ingest's internal-call
// path. Audit + metrics still capture the call.
func (g *Gateway) notesPublishTool(np NotesPublisher) toolEntry {
	allow := policy.ActionAllow
	t := mcp.Tool{
		Name: "notes.publish",
		Description: descriptionBanner +
			"Write a markdown note to the toolyard notes workspace AND index it into MemPalace in one call. " +
			"`path` is relative to the notes directory (e.g. `decisions/graphql.md`). " +
			"Content is the full file body. Indexing is best-effort: the file is always written; if MemPalace is briefly unavailable the next background sync picks the file up. Use this whenever you create or update an agent-authored note so search stays current.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "path", "content"},
			Properties: addMetaProps(map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path relative to the notes directory."},
				"content": map[string]any{"type": "string", "description": "Full markdown body of the note."},
				"topic":   map[string]any{"type": "string", "description": "Optional MemPalace topic; defaults to the parent directory name."},
			}),
		},
	}
	handler := directHandler(func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		topic, _ := args["topic"].(string)
		agentID := AgentIDFromContext(ctx)
		res, err := np.Publish(ctx, agentID, path, content, topic)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("notes.publish failed", err), nil
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
		originalName: "notes.publish",
		reasonField:  ReasonField,
		handle:       handler,
		forcedAction: &allow,
	}
}

// RegisterNotesPublish wires the notes.publish built-in if np != nil.
// Safe to call once after RegisterBuiltins.
func (g *Gateway) RegisterNotesPublish(np NotesPublisher) {
	if np == nil {
		return
	}
	g.registerEntry(g.notesPublishTool(np))
}

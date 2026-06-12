package voice

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/genai"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// ToolBackend is the slice of the gateway a voice call needs: the catalog
// (declared to Gemini as callable functions) and the routed call path, so
// a voice tool call goes through the exact policy → approval → audit
// pipeline every enrolled agent does. *gateway.Gateway satisfies it.
type ToolBackend interface {
	Catalog() []gateway.CatalogEntry
	RouteCall(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error)
}

// voiceVia is the dispatcher name recorded in the audit log for calls made
// from a live voice session.
const voiceVia = "voice"

// buildGenaiTools renders the gateway catalog as Gemini function
// declarations. The schemas are the post-wrap schemas — `_reason` included
// — so Gemini supplies the reason field the policy pipeline requires, the
// same way a text agent would. Tools whose names Gemini would reject
// (>64 chars) are skipped with a log line; nothing is silently dropped.
func buildGenaiTools(catalog []gateway.CatalogEntry, log *slog.Logger) []*genai.Tool {
	decls := make([]*genai.FunctionDeclaration, 0, len(catalog))
	for _, e := range catalog {
		if len(e.Name) > 64 {
			log.Warn("voice: skipping tool with name too long for Gemini", "tool", e.Name)
			continue
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name:        e.Name,
			Description: e.Description,
			Parameters:  mapToGenaiSchema(e.InputSchema),
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// mapToGenaiSchema converts a JSON-Schema-shaped map (the catalog's
// input_schema) into the genai representation. Unknown/unsupported keys
// are ignored — Gemini only needs type/description/enum/required and the
// nested properties/items.
func mapToGenaiSchema(m map[string]any) *genai.Schema {
	if len(m) == 0 {
		return nil
	}
	out := &genai.Schema{}
	if t, ok := m["type"].(string); ok {
		out.Type = toGenaiType(t)
	}
	if d, ok := m["description"].(string); ok {
		out.Description = d
	}
	switch req := m["required"].(type) {
	case []string:
		out.Required = append([]string(nil), req...)
	case []any:
		for _, r := range req {
			if s, ok := r.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	}
	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			if s, ok := e.(string); ok {
				out.Enum = append(out.Enum, s)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		out.Items = mapToGenaiSchema(items)
	}
	if props, ok := m["properties"].(map[string]any); ok && len(props) > 0 {
		out.Properties = make(map[string]*genai.Schema, len(props))
		for k, v := range props {
			if pm, ok := v.(map[string]any); ok {
				out.Properties[k] = mapToGenaiSchema(pm)
			}
		}
	}
	// Gemini rejects a typeless object schema with properties; backfill.
	// The zero value "" is distinct from the explicit TypeUnspecified
	// constant — treat both as unset.
	if (out.Type == "" || out.Type == genai.TypeUnspecified) && len(out.Properties) > 0 {
		out.Type = genai.TypeObject
	}
	return out
}

func toGenaiType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	default:
		return genai.TypeUnspecified
	}
}

// dispatchTool routes one Gemini tool call through the gateway. The agent
// identity is already on ctx (set in runCall). The result — success,
// policy denial, or a deferred-approval envelope — comes back as text for
// Gemini to speak from.
func dispatchTool(ctx context.Context, backend ToolBackend, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := backend.RouteCall(ctx, voiceVia, name, args)
	if err != nil {
		return "", err
	}
	text := resultText(res)
	if res != nil && res.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

// resultText flattens a CallToolResult's text contents into one string.
func resultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok && tc.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

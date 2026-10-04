package gateway

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/previewassertion"
)

// PreviewCaller is the private control boundary. Preflight commits the verified
// immutable enrollment before approval; Call rechecks it before every POST.
type PreviewCaller interface {
	Preflight(context.Context, string, string, map[string]any) error
	RequireApprovalBinding(context.Context, string, int64) error
	Call(context.Context, string, string, map[string]any) (json.RawMessage, error)
}

// RegisterBKSPreviewTools adds a fixed local catalog through the normal policy,
// reason, access, approval, audit and metrics route. It makes no network request.
// A nil client keeps the feature disabled and its tools absent.
func (g *Gateway) RegisterBKSPreviewTools(client PreviewCaller) {
	if client == nil {
		return
	}
	for _, definition := range previewassertion.Descriptors() {
		definition := definition
		properties := definition.Schema["properties"].(map[string]any)
		required := append([]string{ReasonField}, definition.Schema["required"].([]string)...)
		read, destructive, idempotent, openWorld := definition.ReadOnly, definition.Destructive, true, false
		tool := mcp.Tool{
			Name: definition.Name, Description: descriptionBanner + definition.Description,
			InputSchema: mcp.ToolInputSchema{
				Type: "object", Properties: addMetaProps(properties), Required: required, AdditionalProperties: false,
			},
			Annotations: mcp.ToolAnnotation{
				ReadOnlyHint: &read, DestructiveHint: &destructive, IdempotentHint: &idempotent, OpenWorldHint: &openWorld,
			},
		}
		g.registerEntry(toolEntry{
			tool: tool, upstream: "bks_preview", originalName: definition.Operation,
			reasonField: ReasonField, denyInternal: true,
			preflight: func(ctx context.Context, args map[string]any) error {
				return client.Preflight(ctx, AgentIDFromContext(ctx), definition.Operation, args)
			},
			approvedPreflight: func(ctx context.Context, req *approval.Request) error {
				return client.RequireApprovalBinding(ctx, req.AgentID, req.CreatedAt)
			},
			handle: func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				result, err := client.Call(ctx, AgentIDFromContext(ctx), definition.Operation, args)
				if err != nil {
					return mcp.NewToolResultError(previewassertion.SafeError(err)), nil
				}
				var response mcp.CallToolResult
				if json.Unmarshal(result, &response) != nil {
					return mcp.NewToolResultError("private preview response invalid"), nil
				}
				return &response, nil
			},
		})
	}
}

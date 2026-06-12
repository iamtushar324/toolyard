package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/events"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// eventsUpstream is the synthetic upstream we tag events.* tools with.
const eventsUpstream = "events"

// EventsProvider is the gateway-side dependency for the events.* built-in
// tools. Implemented by *internal/events.Service. Declared here (importing the
// events package, which does not import gateway — no cycle) so the tool
// handlers can pass typed filters and receive typed events.
type EventsProvider interface {
	Brief(ctx context.Context, since int64) (string, error)
	Query(ctx context.Context, f events.Filter) ([]events.Event, error)
	Get(ctx context.Context, id string) (*events.Event, error)
	Ack(ctx context.Context, ids []string, actor string) (int, error)
	EnsureAgentSource(ctx context.Context, agentName string) (*events.Source, error)
	Ingest(ctx context.Context, src *events.Source, in events.IngestInput) (*events.Event, bool, error)
}

// RegisterEventsTools wires events.brief/query/get/ack/publish if ep != nil.
// Safe to call once after RegisterBuiltins (NotesPublisher pattern).
func (g *Gateway) RegisterEventsTools(ep EventsProvider) {
	if ep == nil {
		return
	}
	for _, e := range g.eventsTools(ep) {
		g.registerEntry(e)
	}
}

func (g *Gateway) eventsTools(ep EventsProvider) []toolEntry {
	out := []toolEntry{}

	out = append(out, eventsEntry(
		"events.brief",
		"THE common-language entry point. Call this at session start to learn what happened across all agents and external systems while you were away. Returns a markdown digest of unacked events grouped by source and type. Ack what you handle so the brief stays focused. Optional `since_minutes` limits the window.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"since_minutes": map[string]any{"type": "integer", "description": "Only include events from the last N minutes."},
			}),
		},
		handleEventsBrief(ep),
	))

	out = append(out, eventsEntry(
		"events.query",
		"Search the events feed. Returns a text-first list of event summaries (most recent first). Filter by `source`, `type`, `since_minutes`, free-text `q`, or `unacked_only`. Use events.get for the full payload of a specific event.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"source":        map[string]any{"type": "string", "description": "Source name filter."},
				"type":          map[string]any{"type": "string"},
				"since_minutes": map[string]any{"type": "integer"},
				"q":             map[string]any{"type": "string", "description": "Substring match on summary/title/type."},
				"unacked_only":  map[string]any{"type": "boolean"},
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "Default 50, max 500."},
			}),
		},
		handleEventsQuery(ep),
	))

	out = append(out, eventsEntry(
		"events.get",
		"Return one event by id, including its full JSON payload.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "id"},
			Properties: addMetaProps(map[string]any{
				"id": map[string]any{"type": "string"},
			}),
		},
		handleEventsGet(ep),
	))

	out = append(out, eventsEntry(
		"events.ack",
		"Acknowledge events you've handled so they drop out of the brief. Pass a single `id` or a list of `ids`. Low-risk bookkeeping — no approval needed.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"id":  map[string]any{"type": "string"},
				"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}),
		},
		handleEventsAck(ep),
	))

	out = append(out, eventsEntry(
		"events.publish",
		"Emit a natural-language event into the shared cross-agent activity feed. Use this to tell other agents (Claude Code, Codex, Hermes, OpenClaw) what you did or noticed. `summary` is a plain-English sentence; `type` is a short category; `payload` is optional structured data. The event is attributed to an auto-created source named after you.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "type", "summary"},
			Properties: addMetaProps(map[string]any{
				"type":    map[string]any{"type": "string", "description": "Short category, e.g. 'deploy', 'finding', 'handoff'."},
				"summary": map[string]any{"type": "string", "description": "Natural-language sentence describing what happened."},
				"title":   map[string]any{"type": "string"},
				"payload": map[string]any{"type": "object", "description": "Optional structured data."},
			}),
		},
		handleEventsPublish(ep),
	))

	return out
}

func eventsEntry(name, desc string, schema mcp.ToolInputSchema, h directHandler) toolEntry {
	allow := policy.ActionAllow
	tool := mcp.Tool{
		Name:        name,
		Description: descriptionBanner + desc,
		InputSchema: schema,
	}
	return toolEntry{
		tool:         tool,
		upstream:     eventsUpstream,
		originalName: strings.TrimPrefix(name, "events."),
		reasonField:  ReasonField,
		handle:       h,
		forcedAction: &allow,
	}
}

// ---- handlers ---------------------------------------------------------------

func sinceFromMinutes(args map[string]any) int64 {
	if m, ok := asInt(args["since_minutes"]); ok && m > 0 {
		return time.Now().Add(-time.Duration(m) * time.Minute).UnixMilli()
	}
	return 0
}

func handleEventsBrief(ep EventsProvider) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		md, err := ep.Brief(ctx, sinceFromMinutes(args))
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.brief", err), nil
		}
		return mcp.NewToolResultText(md), nil
	}
}

func handleEventsQuery(ep EventsProvider) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		f := events.Filter{
			SourceName:  stringArg(args, "source"),
			Type:        stringArg(args, "type"),
			Q:           stringArg(args, "q"),
			Since:       sinceFromMinutes(args),
			UnackedOnly: boolArg(args, "unacked_only"),
		}
		if n, ok := asInt(args["limit"]); ok {
			f.Limit = n
		}
		evs, err := ep.Query(ctx, f)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.query", err), nil
		}
		if len(evs) == 0 {
			return mcp.NewToolResultText("No matching events."), nil
		}
		var b strings.Builder
		for _, ev := range evs {
			ack := ""
			if ev.AckedAt == 0 {
				ack = " [unacked]"
			}
			fmt.Fprintf(&b, "%s · %s · %s%s\n  %s\n", ev.ID, ev.SourceName, ev.Type, ack, ev.Summary)
		}
		return mcp.NewToolResultText(strings.TrimRight(b.String(), "\n")), nil
	}
}

func handleEventsGet(ep EventsProvider) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id := stringArg(args, "id")
		if id == "" {
			return mcp.NewToolResultError("id is required"), nil
		}
		ev, err := ep.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.get", err), nil
		}
		body, _ := json.Marshal(ev)
		return mcp.NewToolResultText(string(body)), nil
	}
}

func handleEventsAck(ep EventsProvider) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		var ids []string
		if id := stringArg(args, "id"); id != "" {
			ids = append(ids, id)
		}
		if raw, ok := args["ids"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					ids = append(ids, s)
				}
			}
		}
		if len(ids) == 0 {
			return mcp.NewToolResultError("provide `id` or `ids`"), nil
		}
		actor := "agent:" + AgentIDFromContext(ctx)
		n, err := ep.Ack(ctx, ids, actor)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.ack", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Acked %d event(s).", n)), nil
	}
}

func handleEventsPublish(ep EventsProvider) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		typ := stringArg(args, "type")
		summary := stringArg(args, "summary")
		if typ == "" || summary == "" {
			return mcp.NewToolResultError("type and summary are required"), nil
		}
		agentName := AgentIDFromContext(ctx)
		src, err := ep.EnsureAgentSource(ctx, agentName)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.publish source", err), nil
		}
		in := events.IngestInput{Type: typ, Summary: summary, Title: stringArg(args, "title")}
		if payload, ok := args["payload"].(map[string]any); ok {
			pb, _ := json.Marshal(payload)
			in.Payload = pb
		}
		ev, _, err := ep.Ingest(ctx, src, in)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("events.publish", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Published %s (%s).", ev.ID, ev.Type)), nil
	}
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

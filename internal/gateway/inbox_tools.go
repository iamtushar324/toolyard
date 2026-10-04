package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/docs"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// Synthetic upstreams for the inbox and session tools.
const (
	inboxUpstream   = "inbox"
	sessionUpstream = "session"

	// ApprovalModeInbox makes restricted calls return a coaching result
	// instead of queueing an approval.
	ApprovalModeInbox   = "inbox"
	ApprovalModeExecute = "execute"

	// EventCallCoached is the audit event for a restricted call that was
	// answered with permission_required.
	EventCallCoached = "call.coached"
)

// SetInbox wires the inbox. mode is read on every restricted call so the
// owner can switch modes without a restart.
func (g *Gateway) SetInbox(svc *inbox.Service, guide *inbox.Guide, mode func() string) {
	g.inbox = svc
	g.guide = guide
	g.approvalMode = mode
}

func (g *Gateway) inboxMode() bool {
	return g.inbox != nil && g.approvalMode != nil && g.approvalMode() == ApprovalModeInbox
}

// Access implements inbox.Catalog: how would a call to tool be treated?
// A tool outside agentID's access scope reads as unknown, the same as a
// tool that doesn't exist, so inbox.check, request validation and the
// nearby-restricted hints never name a server the agent wasn't granted.
func (g *Gateway) Access(ctx context.Context, agentID, tool string, args map[string]any) (string, string) {
	g.mu.RLock()
	entry, ok := g.tools[tool]
	g.mu.RUnlock()
	if !ok || !g.allowsEntry(ctx, agentID, entry) {
		return inbox.AccessUnknown, ""
	}
	var action policy.Action
	if entry.forcedAction != nil {
		action = *entry.forcedAction
	} else {
		action = g.policy.Eval(policy.Request{AgentID: agentID, UpstreamName: entry.upstream, ToolName: entry.tool.Name, Arguments: args}).Action
	}
	if entry.requireHuman && action != policy.ActionDeny {
		action = policy.ActionApprove
	}
	switch action {
	case policy.ActionAllow:
		return inbox.AccessOpen, entry.upstream
	case policy.ActionDeny:
		return inbox.AccessDenied, entry.upstream
	default:
		return inbox.AccessRestricted, entry.upstream
	}
}

// nearbyRestricted lists other restricted tools on the same upstream, so
// the agent thinks about bundling them into one request.
func (g *Gateway) nearbyRestricted(ctx context.Context, agentID string, entry toolEntry) []string {
	g.mu.RLock()
	var names []string
	for name, e := range g.tools {
		if e.upstream == entry.upstream && name != entry.tool.Name {
			names = append(names, name)
		}
	}
	g.mu.RUnlock()
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if a, _ := g.Access(ctx, agentID, n, nil); a == inbox.AccessRestricted {
			out = append(out, n)
			if len(out) == 8 {
				break
			}
		}
	}
	return out
}

// draftFor pre-fills an inbox.request for one call: the tool and its exact
// arguments are filled in; everything the owner needs to hear from the
// agent is left as a placeholder.
func draftFor(tool string, args map[string]any) map[string]any {
	params := map[string]any{}
	for k, c := range inbox.ParamsFromArgs(args) {
		params[k] = c
	}
	return map[string]any{
		"title":   "<≤60 chars: verb + object + target>",
		"summary": "<≤200 chars: one line for the inbox card>",
		"message": "<first person: what you want to do and why>",
		"facts": map[string]any{
			"why_now":          "<why this can't wait>",
			"if_it_goes_wrong": "<what breaks, concretely>",
			"undo":             "<how you'd undo it, or say it can't be undone>",
		},
		"audio":   map[string]any{"script": "<≤75 words, first person, no IDs or URLs>"},
		"urgency": "soon",
		"tools": []any{map[string]any{
			"tool":     tool,
			"required": true,
			"summary":  "<in plain words, what this call will do>",
			"params":   params,
		}},
		"attachments": []any{},
	}
}

// coach answers a restricted call made without a grant. Nothing runs and
// nothing reaches the owner.
func (g *Gateway) coach(ctx context.Context, entry toolEntry, args map[string]any, agentID, reason string, ev *metrics.Event) *mcp.CallToolResult {
	nearby := g.nearbyRestricted(ctx, agentID, entry)
	argsJSON, _ := json.Marshal(args)
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    EventCallCoached,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Decision:     "permission_required",
		Reason:       reason,
		Arguments:    argsJSON,
	})
	ev.Outcome = metrics.OutcomeDenied
	ev.ErrorClass = "permission_required"

	msg := fmt.Sprintf("%s is restricted. Nothing ran and nothing was sent to your owner. "+
		"Ask with inbox.request, starting from the draft below: fill in the message, the voice-note script and the evidence, "+
		"and add every other restricted tool your task needs so your owner decides once.", entry.tool.Name)
	body := map[string]any{
		"status":                 "permission_required",
		"executed":               false,
		"tool":                   entry.tool.Name,
		"message":                msg,
		"draft":                  draftFor(entry.tool.Name, args),
		"also_restricted_nearby": nearby,
		"tips": []string{
			"Put every restricted tool your task needs in ONE request. Mark each required or optional.",
			"Attach evidence: PR links, test results as a table, the diff that matters, screenshots or a short recording (as https links).",
			"Send it with dry_run: true first and fix every problem it lists.",
			"Don't retry this call without a grant; it will return this again.",
		},
		"guide": `inbox.guide({"topic": "requests"})`,
	}
	text := msg + "\n\nNext: call inbox.guide() if you haven't, then inbox.request({... , \"dry_run\": true}) with the draft in structured content."
	if len(nearby) > 0 {
		text += "\nOther restricted tools here: " + strings.Join(nearby, ", ") + "."
	}
	res := mcp.NewToolResultError(text)
	res.StructuredContent = body
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.permission_required": true}}
	return res
}

// redeemAndDispatch runs a restricted call under a grant.
func (g *Gateway) redeemAndDispatch(ctx context.Context, entry toolEntry, args map[string]any, agentID, reason, token string, ev *metrics.Event) (*mcp.CallToolResult, error) {
	if agentID == "" {
		ev.Outcome = metrics.OutcomeDenied
		return mcp.NewToolResultError("grants only work with an enrolled agent token; this connection is anonymous"), nil
	}
	gr, err := g.inbox.Redeem(ctx, token, agentID, entry.tool.Name, args)
	if err != nil {
		var re *inbox.RedeemError
		if !errors.As(err, &re) {
			ev.Outcome = metrics.OutcomeError
			return mcp.NewToolResultErrorFromErr("grant check failed", err), nil
		}
		argsJSON, _ := json.Marshal(args)
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     "grant_invalid",
			Reason:       reason,
			Arguments:    argsJSON,
		})
		ev.Outcome = metrics.OutcomeDenied
		ev.ErrorClass = "grant_invalid"
		return grantInvalidResponse(entry, args, re), nil
	}
	// The grant is the instrument; the owner who issued it is the decider,
	// named by the email and name Redeem read off their decision.
	decider := gr.Decider()
	allowed := audit.Event{
		EventType:    audit.EventCallAllowed,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Decision:     "grant",
		Reason:       reason,
		ApprovalID:   gr.ID,
	}
	allowed.SetDecider(decider)
	_ = g.audit.Write(ctx, allowed)
	ev.ApprovalOutcome = metrics.ApprovalApproved
	ev.ApprovalVia = "grant"
	ev.ApprovalDecider = decider.Legacy()
	ev.ApprovalID = gr.ID
	if gr.CreatedAt > 0 {
		ev.ApprovalLatencyMs = int(time.Now().UnixMilli() - gr.CreatedAt)
	}
	return g.dispatch(ctx, entry, args, agentID, reason, gr.ID, ev)
}

func grantInvalidResponse(entry toolEntry, args map[string]any, re *inbox.RedeemError) *mcp.CallToolResult {
	body := map[string]any{
		"status":   "grant_invalid",
		"executed": false,
		"tool":     entry.tool.Name,
		"reason":   re.Error(),
	}
	next := "Nothing ran. "
	switch {
	case errors.Is(re, inbox.ErrGrantScope):
		next += "Call again within the grant's parameters, or send a new inbox.request for the call you actually need (draft below) and say what changed."
		body["draft"] = draftFor(entry.tool.Name, args)
	case errors.Is(re, inbox.ErrGrantUsed), errors.Is(re, inbox.ErrGrantExpired):
		next += "Grants work once and expire. If you still need this, send a new inbox.request (draft below) and say it's a re-request."
		body["draft"] = draftFor(entry.tool.Name, args)
	case errors.Is(re, inbox.ErrGrantRevoked):
		next += "Your owner revoked it. Stop, and report where you stopped with inbox.post."
	default:
		next += "Use the exact token inbox.wait or inbox.status gave you, for the tool it was issued for."
	}
	if re.Grant != nil {
		body["grant_id"] = re.Grant.ID
		body["grant_tool"] = re.Grant.Tool
		body["grant_expires_at"] = re.Grant.ExpiresAt
	}
	body["next"] = next
	res := mcp.NewToolResultError(fmt.Sprintf("%s: %s. %s", entry.tool.Name, re.Error(), next))
	res.StructuredContent = body
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.grant_invalid": true}}
	return res
}

// ---- inbox.* and session.* tools ------------------------------------------------

// RegisterInboxTools adds the inbox.* and session.* tools and the guide
// resources. Call after SetInbox.
func (g *Gateway) RegisterInboxTools() {
	if g.inbox == nil {
		return
	}
	for _, e := range g.inboxTools() {
		g.registerEntry(e)
	}
	if g.guide != nil {
		full, _ := g.guide.Topic("all")
		g.mcp.AddResource(
			mcp.NewResource("toolyard://guide", "toolyard agent protocol",
				mcp.WithResourceDescription("How to ask your owner for permission through the toolyard inbox."),
				mcp.WithMIMEType("text/markdown")),
			func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: "toolyard://guide", MIMEType: "text/markdown", Text: full}}, nil
			})
		g.mcp.AddResource(
			mcp.NewResource("toolyard://skill/toolyard-inbox", "toolyard-inbox skill",
				mcp.WithResourceDescription("SKILL.md for Claude Code: install under ~/.claude/skills/toolyard-inbox/."),
				mcp.WithMIMEType("text/markdown")),
			func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: "toolyard://skill/toolyard-inbox", MIMEType: "text/markdown", Text: docs.InboxSkill}}, nil
			})
	}
}

// InboxToolNames are always visible to agents.
var InboxToolNames = []string{
	"inbox.guide", "inbox.check", "inbox.request", "inbox.ask", "inbox.post",
	"inbox.status", "inbox.wait", "inbox.cancel", "session.start", "session.update",
}

func init() {
	for _, n := range InboxToolNames {
		PinnedTools[n] = struct{}{}
	}
}

func inboxEntry(name, upstream, desc string, schema mcp.ToolInputSchema, h directHandler) toolEntry {
	allow := policy.ActionAllow
	return toolEntry{
		tool:          mcp.Tool{Name: name, Description: desc, InputSchema: schema},
		upstream:      upstream,
		originalName:  strings.TrimPrefix(name, upstream+"."),
		reasonField:   ReasonField,
		handle:        h,
		forcedAction:  &allow,
		noCallTimeout: name == "inbox.wait",
	}
}

func obj(required []string, props map[string]any) mcp.ToolInputSchema {
	return mcp.ToolInputSchema{Type: "object", Required: append([]string{ReasonField}, required...), Properties: addMetaProps(props)}
}

var (
	schemaFacts = map[string]any{
		"type":     "object",
		"required": []string{"why_now", "if_it_goes_wrong", "undo"},
		"properties": map[string]any{
			"why_now":          map[string]any{"type": "string"},
			"if_it_goes_wrong": map[string]any{"type": "string"},
			"undo":             map[string]any{"type": "string"},
		},
	}
	schemaAudio = map[string]any{
		"type":        "object",
		"required":    []string{"script"},
		"description": "The voice note your owner hears first. At most 75 words, first person, written to be heard: no IDs, hashes or URLs.",
		"properties":  map[string]any{"script": map[string]any{"type": "string"}},
	}
	schemaUrgency     = map[string]any{"type": "string", "enum": []string{"now", "soon", "digest", "fyi"}}
	schemaAttachments = map[string]any{
		"type":     "array",
		"maxItems": inbox.MaxAttachments,
		"description": "Evidence, shown before the decision. Send data where you can: markdown{body}, table{title,columns,rows}, " +
			"chart{title,chart:line|bar,unit,x,series:[{name,values}]}, diff{file,patch}, code{file,language,body}, log{title,body}, " +
			"link{label,url}. Media as https links toolyard copies when you send: image{url,alt}, video{url,poster_url} (MP4), file{url,name}. " +
			"Each takes an optional first-person caption.",
		"items": map[string]any{
			"type":     "object",
			"required": []string{"type"},
			"properties": map[string]any{
				"type": map[string]any{"type": "string", "enum": []string{"markdown", "table", "chart", "diff", "code", "log", "image", "video", "file", "link"}},
			},
			"additionalProperties": true,
		},
	}
	commonProps = func() map[string]any {
		return map[string]any{
			"title":       map[string]any{"type": "string", "maxLength": inbox.MaxTitle, "description": "Verb + object + target, ≤60 characters."},
			"summary":     map[string]any{"type": "string", "maxLength": inbox.MaxSummary, "description": "One line for the inbox card, ≤200 characters."},
			"message":     map[string]any{"type": "string", "description": "First person: what you want and why."},
			"audio":       schemaAudio,
			"urgency":     schemaUrgency,
			"attachments": schemaAttachments,
			"session_id":  map[string]any{"type": "string", "description": "From session.start (optional)."},
			"dry_run":     map[string]any{"type": "boolean", "description": "Check the request and see its flags without sending it."},
		}
	}
)

func (g *Gateway) inboxTools() []toolEntry {
	svc := g.inbox
	requestProps := commonProps()
	requestProps["facts"] = schemaFacts
	requestProps["ttl_seconds"] = map[string]any{"type": "integer", "minimum": inbox.MinTTL, "maximum": inbox.MaxTTL, "description": "How long grants last once approved. Default 1800."}
	requestProps["tools"] = map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": inbox.MaxTools,
		"items": map[string]any{
			"type":     "object",
			"required": []string{"tool", "required", "summary", "params"},
			"properties": map[string]any{
				"tool":     map[string]any{"type": "string", "description": "Catalog name, e.g. deploy.run."},
				"required": map[string]any{"type": "boolean", "description": "Without this tool the task fails."},
				"summary":  map[string]any{"type": "string", "description": "Your plain-words description of what the call does."},
				"params": map[string]any{
					"type": "object",
					"description": "Every argument you'll pass. A value (or {\"eq\": v}) is exact; also {\"in\": [...]}, {\"prefix\": s}, {\"gte\": n, \"lte\": n}, " +
						"{\"limit\": \"what it will be\", \"pattern\": \"regex\"} for values not known yet, {\"any\": true}. Arguments you don't list aren't covered.",
					"additionalProperties": true,
				},
				"after": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tools in this request that run first."},
			},
		},
	}
	askProps := commonProps()
	askProps["kind"] = map[string]any{"type": "string", "enum": []string{"question", "blocker"}, "description": "question (default) or blocker when you're stuck."}
	askProps["options"] = map[string]any{
		"type": "array", "minItems": inbox.MinOptions, "maxItems": inbox.MaxOptions,
		"items": map[string]any{"type": "object", "required": []string{"label"}, "properties": map[string]any{
			"label": map[string]any{"type": "string"}, "detail": map[string]any{"type": "string"},
		}},
	}
	askProps["schema_version"] = map[string]any{"type": "integer", "enum": []int{1, 2}}
	askProps["client_request_id"] = map[string]any{"type": "string", "maxLength": 128, "description": "Stable retry key, unique per agent and question."}
	askProps["prompt"] = map[string]any{"type": "string", "maxLength": 1000}
	askProps["context"] = map[string]any{"type": "string", "maxLength": 3000}
	askProps["blocking"] = map[string]any{"type": "boolean"}
	askProps["task"] = map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"}}}
	askProps["question"] = map[string]any{"type": "object", "required": []string{"type"}, "properties": map[string]any{
		"type":           map[string]any{"type": "string", "enum": []string{"free_text", "single_choice", "multiple_choice"}},
		"min_selections": map[string]any{"type": "integer", "minimum": 0}, "max_selections": map[string]any{"type": "integer", "minimum": 0},
		"options": map[string]any{"type": "array", "maxItems": 12, "items": map[string]any{"type": "object", "required": []string{"id", "label"}, "properties": map[string]any{
			"id": map[string]any{"type": "string"}, "label": map[string]any{"type": "string"}, "detail": map[string]any{"type": "string"}, "exclusive": map[string]any{"type": "boolean"}, "recommended": map[string]any{"type": "boolean"},
		}}}}}
	postProps := commonProps()
	delete(postProps, "dry_run")
	postProps["request_id"] = map[string]any{"type": "string", "description": "The request this update closes (optional)."}
	idsProp := map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": map[string]any{"type": "string"}}

	submit := func(kind string) directHandler {
		return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			agentID, errRes := requireAgentID(ctx)
			if errRes != nil {
				return errRes, nil
			}
			sub, err := inbox.DecodeSubmission(args)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			switch kind {
			case inbox.KindQuestion:
				if sub.Kind != inbox.KindBlocker {
					sub.Kind = inbox.KindQuestion
				}
			case inbox.KindUpdate:
				sub.Kind, sub.DryRun = inbox.KindUpdate, false
			default:
				sub.Kind = inbox.KindAccess
			}
			res, err := svc.Submit(ctx, agentID, sub)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return submitResult(res), nil
		}
	}

	return []toolEntry{
		inboxEntry("inbox.guide", inboxUpstream,
			"How to ask your owner for permission: the rules, the request format, and worked examples. Call it once before your first request. "+
				"No topic returns an overview and the topic list (restricted, coaching, requests, tools, voice, attachments, urgency, dry-run, grants, updates, never, examples, hosting, all).",
			obj(nil, map[string]any{"topic": map[string]any{"type": "string"}}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				if g.guide == nil {
					return mcp.NewToolResultError("the guide isn't available on this gateway"), nil
				}
				topic, _ := args["topic"].(string)
				text, ok := g.guide.Topic(topic)
				if !ok {
					return mcp.NewToolResultErrorf("unknown topic %q; topics: %s", topic, strings.Join(g.guide.Topics(), ", ")), nil
				}
				return mcp.NewToolResultText(text), nil
			}),
		inboxEntry("inbox.check", inboxUpstream,
			"Before a task: for each call you plan, is it open (call it now), granted (a live grant covers it), restricted (put it in your inbox.request), denied, or unknown?",
			obj([]string{"calls"}, map[string]any{"calls": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 50,
				"items": map[string]any{"type": "object", "required": []string{"tool"}, "properties": map[string]any{
					"tool": map[string]any{"type": "string"}, "args": map[string]any{"type": "object", "additionalProperties": true},
				}},
			}}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				var in struct {
					Calls []inbox.CheckCall `json:"calls"`
				}
				if err := remarshal(args, &in); err != nil || len(in.Calls) == 0 {
					return mcp.NewToolResultError("calls: give a list of {tool, args}"), nil
				}
				res, err := svc.Check(ctx, agentID, in.Calls)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("check failed", err), nil
				}
				return inboxJSON(map[string]any{"calls": res}), nil
			}),
		inboxEntry("inbox.request", inboxUpstream,
			"Ask your owner for permission to use restricted tools: one request per task, with every tool it needs. "+
				"Write it as yourself: a first-person message, facts (why_now, if_it_goes_wrong, undo), a ≤75-word voice-note script, and evidence attachments. "+
				"Run it with dry_run: true first. Returns request_id; then keep working and use inbox.wait. Read inbox.guide for the format.",
			obj([]string{"title", "summary", "message", "facts", "urgency", "tools"}, requestProps),
			submit(inbox.KindAccess)),
		inboxEntry("inbox.ask", inboxUpstream,
			"Ask a question. Prefer schema_version:2 with prompt and question.type (free_text, single_choice, multiple_choice). Options need stable IDs. Your owner can always write a custom answer. Audio is optional. Legacy title/summary/message/options remain supported.",
			obj(nil, askProps),
			submit(inbox.KindQuestion)),
		inboxEntry("inbox.post", inboxUpstream,
			"Tell your owner what happened, e.g. when a task you asked permission for is done. First person, with a short voice note and evidence. Set request_id to close that request's loop.",
			obj([]string{"title", "summary", "message"}, postProps),
			submit(inbox.KindUpdate)),
		inboxEntry("inbox.status", inboxUpstream,
			"Check your requests without blocking. Approved requests include a grant token per allowed tool, shown once: keep it in memory and pass it as _grant.",
			obj([]string{"ids"}, map[string]any{"ids": idsProp}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				ids := stringList(args["ids"])
				if len(ids) == 0 {
					return mcp.NewToolResultError("ids: give at least one request_id"), nil
				}
				views, err := svc.Status(ctx, agentID, ids)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("status failed", err), nil
				}
				return inboxJSON(map[string]any{"requests": views}), nil
			}),
		inboxEntry("inbox.wait", inboxUpstream,
			"Block until your owner decides (mode any: first decision; all: every one) or timeout_seconds passes (max 300). Use it when you have nothing else to do. Same result as inbox.status.",
			obj([]string{"ids"}, map[string]any{
				"ids":             idsProp,
				"mode":            map[string]any{"type": "string", "enum": []string{"any", "all"}},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": int(inbox.MaxWait.Seconds())},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				ids := stringList(args["ids"])
				if len(ids) == 0 {
					return mcp.NewToolResultError("ids: give at least one request_id"), nil
				}
				mode, _ := args["mode"].(string)
				if mode != "all" {
					mode = "any"
				}
				timeout := 60 * time.Second
				if f, ok := args["timeout_seconds"].(float64); ok && f > 0 {
					timeout = time.Duration(f) * time.Second
				}
				views, err := svc.Wait(ctx, agentID, ids, mode, timeout)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("wait failed", err), nil
				}
				return inboxJSON(map[string]any{"requests": views}), nil
			}),
		inboxEntry("inbox.cancel", inboxUpstream,
			"Withdraw one of your pending requests when you no longer need it.",
			obj([]string{"request_id"}, map[string]any{"request_id": map[string]any{"type": "string"}}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				id, _ := args["request_id"].(string)
				r, err := svc.Cancel(ctx, agentID, id)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return inboxJSON(map[string]any{"request_id": r.ID, "status": r.Status}), nil
			}),
		inboxEntry("session.start", sessionUpstream,
			"Name the piece of work you're doing, so your owner can see it among their other agents. Pass the returned session_id on your inbox requests.",
			obj([]string{"title"}, map[string]any{
				"title":  map[string]any{"type": "string", "description": "What you're working on, as a person would say it."},
				"repo":   map[string]any{"type": "string"},
				"branch": map[string]any{"type": "string"},
				"host":   map[string]any{"type": "string", "description": "Where you're running, e.g. laptop, cloud, ci."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				title, _ := args["title"].(string)
				repo, _ := args["repo"].(string)
				branch, _ := args["branch"].(string)
				host, _ := args["host"].(string)
				ss, err := svc.StartSession(ctx, agentID, title, repo, branch, host)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return inboxJSON(ss), nil
			}),
		inboxEntry("session.update", sessionUpstream,
			"Report your session's state: working, waiting_on_owner (you have open requests but other work to do), blocked_on_owner, idle or done. Also counts as a heartbeat.",
			obj([]string{"session_id", "status"}, map[string]any{
				"session_id": map[string]any{"type": "string"},
				"status":     map[string]any{"type": "string", "enum": []string{"working", "waiting_on_owner", "blocked_on_owner", "idle", "done"}},
				"note":       map[string]any{"type": "string"},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				id, _ := args["session_id"].(string)
				status, _ := args["status"].(string)
				note, _ := args["note"].(string)
				ss, err := svc.UpdateSession(ctx, agentID, id, status, note)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return inboxJSON(ss), nil
			}),
	}
}

func requireAgentID(ctx context.Context) (string, *mcp.CallToolResult) {
	id := agentIDFromContext(ctx)
	if id == "" {
		return "", mcp.NewToolResultError("the inbox needs an enrolled agent: connect with an agent token (Authorization: Bearer …) so requests and grants can be tied to you")
	}
	return id, nil
}

func submitResult(res *inbox.SubmitResult) *mcp.CallToolResult {
	var b strings.Builder
	switch {
	case res.DryRun && res.OK:
		b.WriteString("Dry run passed. Nothing was sent. Review the flags (they're what your owner will see), then send it without dry_run.")
	case res.DryRun:
		fmt.Fprintf(&b, "Dry run found %d problem(s). Nothing was sent. Fix them and try again.", len(res.Problems))
	case res.OK:
		fmt.Fprintf(&b, "Sent to your owner as %s. Keep working on anything that doesn't depend on it; use inbox.wait when you run out.", res.RequestID)
	default:
		fmt.Fprintf(&b, "Not sent: %d problem(s). Fix them and send again (dry_run: true helps).", len(res.Problems))
	}
	for _, p := range res.Problems {
		fmt.Fprintf(&b, "\n- %s: %s", p.Path, p.Message)
	}
	out := mcp.NewToolResultText(b.String())
	if !res.OK {
		out.IsError = !res.DryRun
	}
	out.StructuredContent = res
	return out
}

func inboxJSON(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("encode", err)
	}
	res := mcp.NewToolResultText(string(b))
	res.StructuredContent = v
	return res
}

func remarshal(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func stringList(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Package auditlink writes an audit row for every approval-bus event, so
// the audit log records who raised each held call and who decided it,
// on every decision path the bus owns (dashboard, push tap, Telegram,
// auto-rule, agent cancel, expiry) and when the approved call ran.
//
// Wire it once at startup: bus.AddNotifier(auditlink.Notifier(auditLog)).
package auditlink

import (
	"context"
	"log"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
)

// Notifier returns an approval.Notifier that mirrors bus events into
// the audit log.
func Notifier(log *audit.Logger) approval.Notifier {
	return &notifier{log: log}
}

type notifier struct {
	log *audit.Logger
}

// OnApproval implements approval.Notifier. The bus fans out on the
// caller's context, which for a dashboard click is the HTTP request's;
// the write uses a context that outlives it but keeps its values (the
// Raiser, if any).
func (n *notifier) OnApproval(ctx context.Context, req *approval.Request, eventType string) {
	if n.log == nil || req == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	// An auto-approved request never fans out approval.create: the bus
	// decides it before the create fan-out so the push notifier stays
	// quiet. Its decide event is the first the log hears of it, so the
	// create row is written here, first.
	if eventType == "approval.decide" && req.DecidedVia == actor.ViaAutoRule {
		n.write(ctx, req, "approval.create")
	}
	n.write(ctx, req, eventType)
}

func (n *notifier) write(ctx context.Context, req *approval.Request, eventType string) {
	ev, ok := EventFor(req, eventType)
	if !ok {
		return
	}
	if err := n.log.Write(ctx, ev); err != nil {
		log.Printf("auditlink: %s %s: %v", eventType, req.ID, err)
	}
}

// EventFor maps a bus event to the audit row it leaves. ok is false for
// an event type the audit log does not record. Every row carries the
// request's Raiser, tool, upstream and approval id; decision rows carry
// the decider and Decision = the request's status.
func EventFor(req *approval.Request, eventType string) (ev audit.Event, ok bool) {
	ev = audit.Event{
		AgentID:      req.AgentID,
		UpstreamName: req.UpstreamName,
		ToolName:     req.ToolName,
		ApprovalID:   req.ID,
	}
	if req.RaisedBy != nil {
		ev.Raiser = *req.RaisedBy
	}
	switch eventType {
	case "approval.create":
		ev.EventType = audit.EventApprovalCreate
		ev.Reason = req.Reason
		// Stamp the approval's own creation time so the row sorts before
		// its decision even when both are written together.
		ev.TS = req.CreatedAt
	case "approval.decide":
		ev.EventType = audit.EventApprovalDecide
		ev.Decision = req.Status
		ev.SetDecider(req.Decider())
	case "approval.expire":
		ev.EventType = audit.EventApprovalExpire
		ev.Decision = approval.StatusExpired
		ev.SetDecider(actor.Decider{Via: actor.ViaExpiry})
	case "approval.cancel":
		ev.EventType = audit.EventApprovalCancel
		ev.Decision = approval.StatusCancelled
		ev.SetDecider(req.Decider())
	case "approval.executed":
		ev.EventType = audit.EventApprovalExecuted
		ev.ResultSummary = executionSummary(req)
		ev.SetDecider(req.Decider())
	default:
		return audit.Event{}, false
	}
	return ev, true
}

func executionSummary(req *approval.Request) string {
	switch {
	case req.ResultError != "":
		return "execution failed: " + req.ResultError
	case req.ResultIsError:
		return "tool returned an error"
	default:
		return "executed"
	}
}

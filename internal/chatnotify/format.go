package chatnotify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
)

// maxArgsChars bounds how much of the (already redaction-free) argument JSON
// we put into a chat message. Chat surfaces are not an audit log; the full
// payload lives in the dashboard.
const maxArgsChars = 500

// statusLabel maps an approval status to a human chat label + emoji.
func statusLabel(status string) string {
	switch status {
	case approval.StatusPending:
		return "⏳ Pending"
	case approval.StatusAllowed:
		return "✅ Allowed"
	case approval.StatusDenied:
		return "⛔ Denied"
	case approval.StatusExpired:
		return "⌛ Expired"
	case approval.StatusCancelled:
		return "🚫 Cancelled"
	default:
		return status
	}
}

// render produces the channel-agnostic message body for an approval in any
// state. When includeDetails is false only the headline (status, upstream·tool,
// agent) is shown — useful when chat history is shared more widely than the
// dashboard. The format is plain text with light markdown that every channel
// renders acceptably.
func render(req *approval.Request, includeDetails bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s · %s\n", statusLabel(req.Status), req.UpstreamName, req.ToolName)
	if req.AgentID != "" {
		fmt.Fprintf(&b, "agent: %s\n", req.AgentID)
	}
	if includeDetails {
		if req.Reason != "" {
			fmt.Fprintf(&b, "reason: %s\n", oneLine(req.Reason, 240))
		}
		if len(req.Arguments) > 0 {
			fmt.Fprintf(&b, "args: %s\n", argsPreview(req.Arguments))
		}
	}
	if req.Status == approval.StatusPending && req.ExpiresAt > 0 {
		left := time.Until(time.UnixMilli(req.ExpiresAt)).Round(time.Minute)
		if left > 0 {
			fmt.Fprintf(&b, "expires in %s\n", left)
		}
	}
	if req.Status != approval.StatusPending {
		if who := deciderLabel(req.Decider()); who != "" {
			fmt.Fprintf(&b, "decided by %s\n", who)
		}
	}
	// Append the executed result line when present.
	if req.ResultExecutedAt > 0 {
		if req.ResultError != "" {
			fmt.Fprintf(&b, "result: error — %s\n", oneLine(req.ResultError, 200))
		} else if req.ResultIsError {
			b.WriteString("result: tool returned an error\n")
		} else {
			b.WriteString("result: ✓ executed\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// deciderLabel says who decided and how, for a chat line: the person
// (name, else email, else user id) and the route when it was not a plain
// dashboard click; the instrument alone when no person is recorded.
// Empty for an expired request, which nobody decided.
func deciderLabel(d actor.Decider) string {
	who := d.Name
	if who == "" {
		who = d.Email
	}
	if who == "" {
		who = d.UserID
	}
	var how string
	switch d.Via {
	case actor.ViaExpiry:
		return ""
	case "", actor.ViaDashboard:
		if who == "" {
			return d.Legacy()
		}
	case actor.ViaDashboardBatch:
		how = "dashboard, batch"
	case actor.ViaPushToken:
		how = "notification tap"
	case actor.ViaPasskey:
		how = "passkey"
	case actor.ViaTelegram:
		how = "Telegram"
		if who == "" && d.Ref != "" {
			how += " user " + d.Ref
		}
	case actor.ViaAutoRule:
		how = "auto-approval rule " + d.Ref
	case actor.ViaPolicy:
		how = "policy " + d.Ref
	case actor.ViaInboxGrant:
		how = "inbox grant " + d.Ref
	case actor.ViaAgentCancel:
		how = "agent " + d.Ref + " (cancelled)"
	default:
		how = d.Via
		if d.Ref != "" {
			how += " " + d.Ref
		}
	}
	switch {
	case who != "" && how != "":
		return who + " via " + how
	case who != "":
		return who
	}
	return strings.TrimSpace(how)
}

func argsPreview(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "(unserializable)"
	}
	return oneLine(string(raw), maxArgsChars)
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

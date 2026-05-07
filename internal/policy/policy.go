// Package policy decides whether a tool call is allowed, must be approved, or
// denied. v0.1 ships a hardcoded rule: writes need approval, reads pass.
package policy

import "strings"

type Action string

const (
	ActionAllow   Action = "allow"
	ActionApprove Action = "approve"
	ActionDeny    Action = "deny"
)

type Decision struct {
	Action  Action
	Reason  string // human-readable rationale for the decision
	RuleID  string
}

type Request struct {
	AgentID        string
	UpstreamName   string
	ToolName       string
	IntentCategory string // optional v0.1
	Arguments      map[string]any
	UserReason     string
}

// Engine is the policy evaluator. v0.1 has no rules table; it uses heuristics.
type Engine struct{}

func New() *Engine { return &Engine{} }

// Eval returns the decision for req. The hardcoded rule:
//   - meta-tools on the synthetic "tools" upstream pass through (their inner
//     call is policy-evaluated independently)
//   - explicit category "read" -> allow
//   - explicit category "write" / "destructive" / "external_communication" /
//     "financial" / "privileged_admin" -> approve
//   - otherwise infer from tool name (read-ish prefixes pass; everything else
//     gets approval).
func (e *Engine) Eval(req Request) Decision {
	if req.UpstreamName == "tools" {
		return Decision{Action: ActionAllow, Reason: "meta-tool routes inner call", RuleID: "v0.1-meta-tool"}
	}
	switch req.IntentCategory {
	case "read":
		return Decision{Action: ActionAllow, Reason: "category=read", RuleID: "v0.1-category"}
	case "write", "destructive", "external_communication", "financial", "privileged_admin":
		return Decision{
			Action: ActionApprove,
			Reason: "category=" + req.IntentCategory + " requires human approval",
			RuleID: "v0.1-category",
		}
	}
	if IsReadOnlyName(req.ToolName) {
		return Decision{Action: ActionAllow, Reason: "tool name looks read-only", RuleID: "v0.1-name-heuristic"}
	}
	return Decision{
		Action: ActionApprove,
		Reason: "default: writes require approval",
		RuleID: "v0.1-default-write",
	}
}

// readVerbs are the leading verb-words that mark a tool as read-only when
// matched against the first segment of a tool name (split on . _ /).
var readVerbs = map[string]bool{
	"get": true, "list": true, "search": true, "find": true, "read": true,
	"show": true, "describe": true, "fetch": true, "view": true,
	"lookup": true, "query": true, "head": true, "ls": true,
}

// IsReadOnlyName is a name-only heuristic: returns true when the tool name's
// first verb-segment matches one of the known read verbs. Exported so the
// gateway can populate its is_write metric column without re-running policy
// eval.
func IsReadOnlyName(name string) bool {
	low := strings.ToLower(name)
	// Strip an upstream prefix ("github.", "linear/", ...). We deliberately
	// do not strip on `_` because plenty of raw tool names look like
	// "read_file" with no upstream prefix.
	if i := strings.IndexAny(low, "./"); i >= 0 {
		low = low[i+1:]
	}
	// First word of the snake/dash-separated remainder.
	first := low
	for i := 0; i < len(low); i++ {
		if low[i] == '_' || low[i] == '-' {
			first = low[:i]
			break
		}
	}
	return readVerbs[first]
}

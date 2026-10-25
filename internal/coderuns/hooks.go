package coderuns

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// The gateway's argument names for an Inbox grant token and a legacy
// approval resume.
const (
	grantArg      = "_grant"
	approvalIDArg = "_approval_id"
)

// hookWriteTimeout bounds one call-record write from a hook.
const hookWriteTimeout = 5 * time.Second

// resumeState is what a resume knows before the script runs again.
type resumeState struct {
	// heldSeq is the held call's place: calls before it are replayed.
	heldSeq int
	// approvalID resumes a deferred legacy approval at the held call.
	approvalID string
	// recorded are the call records of the run so far, by seq.
	recorded map[int]Call
	// grants are the request's unused grant tokens, in request order.
	grants []grantToken
}

// grantToken is one Inbox grant for one call of a tool.
type grantToken struct {
	tool  string // the gateway target it covers
	token string
	// params are the parameters the grant allows; with matchParams set a
	// call takes the grant only when its arguments are within them, the
	// way the gateway checks the grant.
	params      map[string]inbox.Constraint
	matchParams bool
	used        bool
}

// gatewayMetaArgs are the arguments the gateway strips, besides the tool's
// own reason field, before it checks a call against a grant
// (internal/gateway schema_wrap.go extractReasonOpt).
var gatewayMetaArgs = []string{"_intent_category", "_approval_id", "_grant", "_session_id"}

// grantArgs is a call's arguments as the gateway matches them to a grant:
// without the tool's reason field (reasonField) and the meta arguments. A
// tool with its own _reason keeps it, because its reason field is then
// __toolyard_reason.
func grantArgs(args map[string]any, reasonField string) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	if reasonField != "" {
		delete(out, reasonField)
	}
	for _, k := range gatewayMetaArgs {
		delete(out, k)
	}
	return out
}

// rerunOnResume are read-only Inbox reads that a resume sends again
// instead of replaying: their answers can carry live grant tokens, which
// the call record never stores.
var rerunOnResume = map[string]bool{"toolyard.inbox_status": true, "toolyard.inbox_wait": true}

// grantTokenRE matches an Inbox grant token: tyg_<base64url id>.<base64url
// Ed25519 signature> (86 characters; 40 or more is enough to tell a token
// from a name that only starts with tyg_).
var grantTokenRE = regexp.MustCompile(`tyg_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{40,}`)

// redactedGrant replaces a grant token in stored text.
const redactedGrant = "tyg_[redacted]"

// redactGrants removes grant tokens from text the run stores: call
// records, the result, logs and the held call. A token is single-use and
// agent-bound, but nothing keeps one at rest in plain text.
func redactGrants(s string) string {
	if !strings.Contains(s, "tyg_") {
		return s
	}
	return grantTokenRE.ReplaceAllString(s, redactedGrant)
}

// seenCall is what the hooks learnt about one call of this execution.
type seenCall struct {
	argsHash   string
	replayText string
}

// hooks are a run's codemode.CallHooks: they record every call the
// gateway answered, and on a resume they replay the calls before the held
// one from the record and add the grants (or the legacy approval id) to
// the held call and the calls after it.
type hooks struct {
	s      *Service
	runID  string
	resume *resumeState

	mu   sync.Mutex
	seen map[int]*seenCall
}

func newHooks(s *Service, runID string, rs *resumeState) *hooks {
	return &hooks{s: s, runID: runID, resume: rs, seen: map[int]*seenCall{}}
}

// callSeen returns a copy of what the hooks saw for seq.
func (h *hooks) callSeen(seq int) seenCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sc, ok := h.seen[seq]; ok {
		return *sc
	}
	return seenCall{}
}

func (h *hooks) BeforeCall(_ context.Context, c *codemode.PendingCall) (*mcp.CallToolResult, bool, error) {
	hash := argsHash(c.ScriptArgs)
	sc := &seenCall{argsHash: hash}
	h.mu.Lock()
	h.seen[c.Seq] = sc
	h.mu.Unlock()

	rs := h.resume
	if rs == nil {
		return nil, false, nil
	}
	rec, recorded := rs.recorded[c.Seq]
	if c.Seq == rs.heldSeq {
		if recorded && (rec.Tool != c.Tool || rec.ArgsHash != hash) {
			return nil, false, diverged("call %d is %s with these arguments, but the held call was %s; the script took another path than before, so nothing more ran", c.Seq, c.Tool, recordName(rec))
		}
		if tok := h.takeGrant(c); tok != "" {
			setArg(c, grantArg, tok)
		} else if rs.approvalID != "" {
			setArg(c, approvalIDArg, rs.approvalID)
		}
		return nil, false, nil
	}
	if !recorded {
		if c.Seq < rs.heldSeq {
			return nil, false, diverged("call %d (%s) has no record in this run, so the script took another path than before; nothing more ran", c.Seq, c.Tool)
		}
		// A call past every call the run made before: new, so it is sent,
		// with a grant when one of the request's covers it.
		if tok := h.takeGrant(c); tok != "" {
			setArg(c, grantArg, tok)
		}
		return nil, false, nil
	}
	// A recorded call other than the held one, wherever it sits: with
	// tools.parallel, a call left un-awaited, or a held call the script
	// caught, calls numbered after the held one may have run too. The
	// record decides for each of them, so nothing that ran is sent again.
	switch {
	case rec.Tool != c.Tool || rec.ArgsHash != hash:
		return nil, false, diverged("call %d is %s with these arguments, but the run's record has %s; the script took another path than before, so nothing more ran", c.Seq, c.Tool, recordName(rec))
	case rec.Status == codemode.StatusOutcomeUnknown:
		return nil, false, &codemode.CallError{Kind: codemode.ErrorOutcomeUnknown, Message: fmt.Sprintf(
			"call %d (%s) has an unknown outcome in this run's record, so it is not repeated: check whether it took effect, then start a new run", c.Seq, rec.Tool)}
	case rec.Status == codemode.StatusSucceeded:
		if rerunOnResume[rec.Tool] {
			// A read-only Inbox read: sent again, never replayed.
			return nil, false, nil
		}
		if strings.Contains(rec.replayText, redactedGrant) {
			return nil, false, diverged("call %d (%s) answered with an Inbox grant token, which the record never keeps, so it cannot be replayed with the same value; nothing more ran. Start a new run", c.Seq, c.Tool)
		}
		h.mu.Lock()
		sc.replayText = rec.replayText
		h.mu.Unlock()
		return mcp.NewToolResultText(rec.replayText), true, nil
	}
	// permission_required or deferred (a held call the script caught, with
	// try_call or tools.tryCall), or failed: the call did not take effect,
	// so it is sent again, with a grant when one of the request's covers
	// it (else the gateway holds it again and the script sees that).
	if tok := h.takeGrant(c); tok != "" {
		setArg(c, grantArg, tok)
	}
	return nil, false, nil
}

func (h *hooks) AfterCall(ctx context.Context, c codemode.PendingCall, out codemode.CallOutcome) {
	h.mu.Lock()
	sc, ok := h.seen[c.Seq]
	if !ok {
		sc = &seenCall{argsHash: argsHash(c.ScriptArgs)}
		h.seen[c.Seq] = sc
	}
	if out.Record.Status == codemode.StatusSucceeded && !rerunOnResume[c.Tool] {
		sc.replayText = redactGrants(out.Text)
	}
	rec := Call{Seq: c.Seq, Tool: c.Tool, Target: c.Target, ArgsHash: sc.argsHash, Status: out.Record.Status,
		StartedAt: out.Record.StartedAt, FinishedAt: out.Record.FinishedAt, replayText: sc.replayText}
	h.mu.Unlock()
	// Written now, not only when the run ends, so a resume and a reader
	// see every call the gateway answered even if the process stops.
	wctx, cancel := context.WithTimeout(ctx, hookWriteTimeout)
	defer cancel()
	if err := upsertCall(wctx, h.s.db, h.runID, rec); err != nil {
		h.s.log.Warn("code run: could not record a call", "run_id", h.runID, "seq", c.Seq, "err", err.Error())
	}
}

// takeGrant returns an unused grant token that covers the call (same
// gateway target and, when the grant's parameters are known, arguments
// within them), and marks it used. A call no grant covers goes without
// one, so the gateway holds it again and the run stays resumable.
func (h *hooks) takeGrant(c *codemode.PendingCall) string {
	if h.resume == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var args map[string]any
	for i := range h.resume.grants {
		g := &h.resume.grants[i]
		if g.used || g.tool != c.Target {
			continue
		}
		if g.matchParams {
			if args == nil {
				args = grantArgs(c.ScriptArgs, c.ReasonField)
			}
			if ok, _ := inbox.MatchArgs(g.params, args); !ok {
				continue
			}
		}
		g.used = true
		return g.token
	}
	return ""
}

// setArg adds one gateway argument to a call.
func setArg(c *codemode.PendingCall, key, value string) {
	if c.Args == nil {
		c.Args = map[string]any{}
	}
	c.Args[key] = value
}

func diverged(format string, a ...any) error {
	return &codemode.CallError{Kind: codemode.ErrorScriptDiverged, Message: fmt.Sprintf(format, a...)}
}

func recordName(c Call) string {
	if c.Tool == "" {
		return "another call"
	}
	return c.Tool + " with its own arguments"
}

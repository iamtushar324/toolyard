package coderuns

import (
	"context"
	"encoding/json"
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
	// heldSeq is the held call's place.
	heldSeq int
	// approvalID resumes the held call's deferred legacy approval.
	approvalID string
	// recorded are the call records of the run so far, by seq: every call
	// any earlier execution made (a later execution that did not reach a
	// call leaves its record in place).
	recorded map[int]Call
	// mark is the highest call number the run ever reached (the held call
	// included). A call at or below it with no record means the script
	// took another path; one above it is new.
	mark int
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

// webhookSecretRE matches a Standard Webhooks secret (callbacks.register
// and callbacks.update return a generated one once).
var webhookSecretRE = regexp.MustCompile(`whsec_[A-Za-z0-9+/=_-]{16,}`)

// redactedSecret replaces a webhook secret in stored text.
const redactedSecret = "whsec_[redacted]"

// skillBundleRE matches a skills bundle download ticket (skills.bundle
// returns a URL whose only credential it is: "tyskb_" and 43 base64url
// characters).
var skillBundleRE = regexp.MustCompile(`tyskb_[A-Za-z0-9_-]{43}`)

// redactedBundle replaces a skills bundle ticket in stored text.
const redactedBundle = "tyskb_[redacted]"

// redactGrants removes grant tokens, webhook secrets and skills bundle
// tickets from text the run stores: call records, the result, logs and the
// held call. Nothing keeps any of them at rest in plain text.
func redactGrants(s string) string {
	if strings.Contains(s, "whsec_") {
		s = webhookSecretRE.ReplaceAllString(s, redactedSecret)
	}
	if strings.Contains(s, "tyskb_") {
		s = skillBundleRE.ReplaceAllString(s, redactedBundle)
	}
	if !strings.Contains(s, "tyg_") {
		return s
	}
	return grantTokenRE.ReplaceAllString(s, redactedGrant)
}

// holdCap caps the structured content kept with a held call's record
// (the Inbox draft, the approval id); a bigger one is not kept, and a
// replayed hold then carries only the gateway's text.
const holdCap = 1 << 20

// seenCall is what the hooks learnt about one call of this execution.
type seenCall struct {
	argsHash   string
	replayText string
	// errKind and errText are a failed call's error kind and text; errText
	// is a held call's text too.
	errKind, errText string
	// approvalID is a deferred call's legacy approval id.
	approvalID string
	// hold is a held call's structured content (at most holdCap).
	hold string
}

// hooks are a run's codemode.CallHooks: they record every call the
// gateway answered, and on a resume they answer each call from the run's
// record or let it go to the gateway (with the request's grant, or a
// deferred call's own approval id).
//
// On a resume the calls run again from the top, with the same numbers as
// before as long as the script takes the same path. Each call is checked
// against the record at its number. A call that goes to the gateway waits
// until every call numbered before it (up to the mark) has passed that
// check, and is refused if one of them did not: a script that took
// another path (an Inbox read that answered differently, say) shifts the
// numbers, and no call on that path is sent, even one that started
// beside the calls that diverged (tools.parallel).
type hooks struct {
	s      *Service
	runID  string
	resume *resumeState

	mu   sync.Mutex
	seen map[int]*seenCall
	// checked are the call numbers, at or below the mark, whose check is
	// done in this execution; through is the highest n with 1..n all
	// checked; firstRefused is the lowest refused one (0 for none).
	checked      map[int]bool
	through      int
	firstRefused int
	// checkedCh is closed and replaced whenever a check is done.
	checkedCh chan struct{}
}

func newHooks(s *Service, runID string, rs *resumeState) *hooks {
	return &hooks{s: s, runID: runID, resume: rs, seen: map[int]*seenCall{}, checked: map[int]bool{}, checkedCh: make(chan struct{})}
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

func (h *hooks) BeforeCall(ctx context.Context, c *codemode.PendingCall) (*mcp.CallToolResult, bool, error) {
	hash := argsHash(c.ScriptArgs)
	sc := &seenCall{argsHash: hash}
	h.mu.Lock()
	h.seen[c.Seq] = sc
	h.mu.Unlock()

	if h.resume == nil {
		return nil, false, nil
	}
	res, send, err := h.decide(c, hash, sc)
	h.checkDone(c.Seq, err != nil)
	switch {
	case err != nil:
		return nil, false, err
	case !send:
		return res, true, nil
	}
	if err := h.waitForEarlierChecks(ctx, c); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// decide checks one call of a resumed script against the run's record:
// an answer from the record (send false), or the call goes to the
// gateway (send true, with a grant or an approval id added to c), or an
// error that stops the script.
func (h *hooks) decide(c *codemode.PendingCall, hash string, sc *seenCall) (res *mcp.CallToolResult, send bool, err error) {
	rs := h.resume
	rec, recorded := rs.recorded[c.Seq]
	if recorded && (rec.Tool != c.Tool || rec.ArgsHash != hash) {
		if c.Seq == rs.heldSeq {
			return nil, false, diverged("call %d is %s with these arguments, but the held call was %s; the script took another path than before, so nothing more ran", c.Seq, c.Tool, recordName(rec))
		}
		return nil, false, diverged("call %d is %s with these arguments, but the run's record has %s; the script took another path than before, so nothing more ran", c.Seq, c.Tool, recordName(rec))
	}
	if c.Seq == rs.heldSeq {
		if rs.approvalID != "" {
			// A deferred legacy approval: resumed with its own id, never a
			// grant, which would run the tool a second time beside the
			// approval's own execution.
			setArg(c, approvalIDArg, rs.approvalID)
		} else if tok := h.takeGrant(c); tok != "" {
			setArg(c, grantArg, tok)
		}
		return nil, true, nil
	}
	if !recorded {
		if c.Seq <= rs.mark {
			return nil, false, diverged("call %d (%s) has no record in this run, though the run reached call %d before; the script took another path than before, so nothing more ran", c.Seq, c.Tool, rs.mark)
		}
		// Past every call the run ever made: new, so it is sent (once the
		// calls before it passed their checks), with a grant when one of
		// the request's covers it.
		if tok := h.takeGrant(c); tok != "" {
			setArg(c, grantArg, tok)
		}
		return nil, true, nil
	}
	// A recorded call other than the held one, wherever it sits: with
	// tools.parallel, a call left un-awaited, or a held call the script
	// caught, calls numbered after the held one may have run too. The
	// record decides for each of them, so nothing that ran is sent again.
	switch rec.Status {
	case codemode.StatusOutcomeUnknown:
		return nil, false, &codemode.CallError{Kind: codemode.ErrorOutcomeUnknown, Message: fmt.Sprintf(
			"call %d (%s) has an unknown outcome in this run's record, so it is not repeated: check whether it took effect, then start a new run", c.Seq, rec.Tool)}
	case codemode.StatusSucceeded:
		if rerunOnResume[rec.Tool] {
			// A read-only Inbox read: sent again, never replayed.
			return nil, true, nil
		}
		if strings.Contains(rec.replayText, redactedGrant) {
			return nil, false, diverged("call %d (%s) answered with an Inbox grant token, which the record never keeps, so it cannot be replayed with the same value; nothing more ran. Start a new run", c.Seq, c.Tool)
		}
		if strings.Contains(rec.replayText, redactedSecret) {
			return nil, false, diverged("call %d (%s) answered with a webhook secret, which the record never keeps, so it cannot be replayed with the same value; nothing more ran. Start a new run", c.Seq, c.Tool)
		}
		if strings.Contains(rec.replayText, redactedBundle) {
			return nil, false, diverged("call %d (%s) answered with a single-use skills bundle link, which the record never keeps, so it cannot be replayed with the same value; nothing more ran. Start a new run", c.Seq, c.Tool)
		}
		h.mu.Lock()
		sc.replayText = rec.replayText
		h.mu.Unlock()
		return mcp.NewToolResultText(rec.replayText), false, nil
	case codemode.StatusFailed:
		if !rec.sent {
			// It never reached the gateway (the script or its clock stopped
			// first): nothing ran, so it goes now.
			if tok := h.takeGrant(c); tok != "" {
				setArg(c, grantArg, tok)
			}
			return nil, true, nil
		}
		if rec.errorKind == codemode.ErrorGrantInvalid {
			// The gateway refused the call's grant before it ran: like a
			// hold, it goes again with a grant of this request that covers
			// it, and else the script gets the same refusal.
			if tok := h.takeGrant(c); tok != "" {
				setArg(c, grantArg, tok)
				return nil, true, nil
			}
		}
		// The gateway answered it with an error, and an upstream error may
		// come after a write took effect: the script gets the same failure
		// again, never a second attempt.
		text := rec.errorText
		if text == "" {
			text = fmt.Sprintf("call %d (%s) failed in an earlier execution of this run (its error text was not kept)", c.Seq, rec.Tool)
		}
		return codemode.ReplayFailure(rec.errorKind, text), false, nil
	case codemode.StatusPermissionRequired:
		// Nothing ran. It goes again only with a grant that covers it;
		// else the script gets the same hold, so it takes the same path as
		// before (a call sent without a grant could now run under a
		// changed policy, behind the script's back).
		if tok := h.takeGrant(c); tok != "" {
			setArg(c, grantArg, tok)
			return nil, true, nil
		}
		return codemode.ReplayHold(codemode.StatusPermissionRequired, h.holdText(c, rec), json.RawMessage(rec.hold)), false, nil
	case codemode.StatusDeferred:
		// A legacy approval: asked again with its own id, which answers its
		// state or result and never makes a second approval.
		if rec.approvalID != "" {
			setArg(c, approvalIDArg, rec.approvalID)
			return nil, true, nil
		}
		return codemode.ReplayHold(codemode.StatusDeferred, h.holdText(c, rec), json.RawMessage(rec.hold)), false, nil
	}
	return nil, false, diverged("call %d (%s) has a record with status %q, which a resume cannot continue; start a new run", c.Seq, c.Tool, rec.Status)
}

// holdText is a held call's recorded text, or a stand-in.
func (h *hooks) holdText(c *codemode.PendingCall, rec Call) string {
	if rec.errorText != "" {
		return rec.errorText
	}
	return fmt.Sprintf("%s is held for approval (call %d of this run, as before)", c.Tool, c.Seq)
}

// checkDone records that call seq passed (or failed) its check in this
// execution, and wakes the calls waiting on it. Only calls at or below
// the mark count.
func (h *hooks) checkDone(seq int, refused bool) {
	if seq > h.resume.mark {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if refused && (h.firstRefused == 0 || seq < h.firstRefused) {
		h.firstRefused = seq
	}
	if h.checked[seq] {
		return
	}
	h.checked[seq] = true
	for h.checked[h.through+1] {
		h.through++
	}
	close(h.checkedCh)
	h.checkedCh = make(chan struct{})
}

// waitForEarlierChecks holds a call that is about to be sent until every
// call numbered before it, up to the mark, passed its check in this
// execution; it refuses the call when one did not. The calls before it
// all have their numbers already (numbers go out in order), so each
// reaches its check unless the script stops first, which ends ctx.
func (h *hooks) waitForEarlierChecks(ctx context.Context, c *codemode.PendingCall) error {
	need := min(c.Seq-1, h.resume.mark)
	if need <= 0 {
		return nil
	}
	for {
		h.mu.Lock()
		through, refused, ch := h.through, h.firstRefused, h.checkedCh
		h.mu.Unlock()
		if refused != 0 && refused <= need {
			return diverged("call %d (%s) was not sent: call %d of this resumed run did not match the run's record, so the script took another path than before and nothing on it is sent", c.Seq, c.Tool, refused)
		}
		if through >= need {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return &codemode.CallError{Kind: codemode.ErrorTimeout, Message: fmt.Sprintf(
				"call %d (%s) was not sent: the script stopped while it waited for the calls before it to be checked against the run's record", c.Seq, c.Tool)}
		}
	}
}

func (h *hooks) AfterCall(ctx context.Context, c codemode.PendingCall, out codemode.CallOutcome) {
	h.mu.Lock()
	sc, ok := h.seen[c.Seq]
	if !ok {
		sc = &seenCall{argsHash: argsHash(c.ScriptArgs)}
		h.seen[c.Seq] = sc
	}
	switch out.Record.Status {
	case codemode.StatusSucceeded:
		if !rerunOnResume[c.Tool] {
			sc.replayText = redactGrants(out.Text)
		}
	case codemode.StatusFailed:
		sc.errKind, sc.errText = out.Kind, storedText(out.FailureText)
	case codemode.StatusPermissionRequired, codemode.StatusDeferred:
		sc.errText = storedText(out.FailureText)
		if out.Held != nil {
			sc.approvalID = out.Held.ApprovalID
			if len(out.Held.Structured) <= holdCap {
				sc.hold = redactGrants(cleanText(string(out.Held.Structured)))
			}
		}
	}
	rec := Call{Seq: c.Seq, Tool: c.Tool, Target: c.Target, ArgsHash: sc.argsHash, Status: out.Record.Status,
		StartedAt: out.Record.StartedAt, FinishedAt: out.Record.FinishedAt}
	rec.setSeen(*sc, true)
	h.mu.Unlock()
	// Written now, not only when the run ends, so a resume and a reader
	// see every call the gateway answered even if the process stops.
	wctx, cancel := context.WithTimeout(ctx, hookWriteTimeout)
	defer cancel()
	if err := upsertCall(wctx, h.s.db, h.runID, rec); err != nil {
		h.s.log.Warn("code run: could not record a call", "run_id", h.runID, "seq", c.Seq, "err", err.Error())
	}
}

// storedText is a failure or hold text as a record keeps it: storable,
// and without grant tokens or webhook secrets.
func storedText(s string) string { return redactGrants(cleanText(s)) }

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

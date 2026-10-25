package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
)

// callbackRefArg is the reserved argument with which one call names the
// receiver (callbacks.register) that hears about it: approval.decided and
// approval.executed for a legacy deferred approval. Like _reason it is
// stripped before the tool sees it.
const callbackRefArg = "_callback_ref"

type callbackRefKey struct{}

// withCallbackRef carries a call's _callback_ref to holdAndWait.
func withCallbackRef(ctx context.Context, args map[string]any) context.Context {
	ref := strings.TrimSpace(stringArg(args, callbackRefArg))
	if ref == "" {
		return ctx
	}
	return context.WithValue(ctx, callbackRefKey{}, ref)
}

// callbackRefFrom is the call's _callback_ref, or "".
func callbackRefFrom(ctx context.Context) string {
	ref, _ := ctx.Value(callbackRefKey{}).(string)
	return ref
}

// CallbackToolNames are the callbacks.* tools with which an agent manages
// the webhook receivers toolyard calls (docs/guidelines/inbox-federation.md).
// They are always visible, like the inbox tools.
var CallbackToolNames = []string{"callbacks.register", "callbacks.list", "callbacks.update", "callbacks.set_default", "callbacks.test"}

func init() {
	for _, n := range CallbackToolNames {
		PinnedTools[n] = struct{}{}
	}
}

// CallbackPrivacy reports whether an agent may register a private
// destination: its federated T3 environment's fixed session-webhook URL
// (federation.Service.PrivateReceiverAllowed). The REST route asks the same.
type CallbackPrivacy func(ctx context.Context, agentID, environmentID, destination string) bool

// RegisterCallbackTools adds the callbacks.* tools. Each acts only on the
// calling agent's own receivers, so they are always allowed; they sit in
// the inbox access group, which every caller has. private may be nil (no
// private destinations).
func (g *Gateway) RegisterCallbackTools(svc *callbacks.Service, private CallbackPrivacy) {
	if svc == nil {
		return
	}
	for _, e := range callbackTools(svc, private) {
		g.registerEntry(e)
	}
}

// receiverView is a receiver as the callbacks.* tools show it.
func receiverView(r *callbacks.Receiver) map[string]any {
	out := map[string]any{
		"callback_ref":       r.ID,
		"destination":        r.Destination,
		"transport":          r.Transport,
		"status":             r.Status,
		"revision":           r.Revision,
		"client_receiver_id": r.ClientReceiverID,
		"default":            r.Default,
	}
	if r.EnvironmentID != "" {
		out["environment_id"] = r.EnvironmentID
	}
	return out
}

// callbackError turns a callbacks error into the agent's next step.
func callbackError(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, callbacks.ErrConflict):
		return mcp.NewToolResultError("conflict: the receiver changed since you read it, or this client_receiver_id is registered with other settings. Read callbacks.list and retry with its current revision, or use another client_receiver_id.")
	case errors.Is(err, callbacks.ErrReceiver):
		return mcp.NewToolResultError("no active receiver of yours has this callback_ref (or your connection is revoked). List yours with callbacks.list.")
	}
	return mcp.NewToolResultError(err.Error())
}

const callbackSecretNote = "Shown once: store it now to verify the webhook-signature header. Toolyard never shows it again; rotate with callbacks.update if you lose it."

func callbackTools(svc *callbacks.Service, private CallbackPrivacy) []toolEntry {
	refProp := map[string]any{"type": "string", "description": "The receiver's callback_ref (cb_…) from callbacks.register or callbacks.list."}
	return []toolEntry{
		inboxEntry("callbacks.register", inboxUpstream,
			"Register a webhook receiver: an https URL (port 443) toolyard POSTs signed events to. Give your own Standard Webhooks secret (whsec_…), "+
				"or omit it and toolyard generates one and returns it once. Pass the returned callback_ref on inbox.request / inbox.ask (callback_ref or callback_refs), "+
				"as _callback_ref on a call that may wait for approval, or make it your default with callbacks.set_default (make_default: true). "+
				"Events: inbox.decision (an item is fully answered, with verdicts and answers), inbox.expired, inbox.cancelled, execution.completed, "+
				"approval.decided, approval.executed, ping. A repeated identical registration returns the same callback_ref. Destinations are fixed; register a new one to change it.",
			obj([]string{"destination"}, map[string]any{
				"destination":        map[string]any{"type": "string", "description": "https URL toolyard POSTs events to (public, port 443, no credentials or fragment)."},
				"secret":             map[string]any{"type": "string", "description": "Optional Standard Webhooks secret: whsec_ and 24 to 64 random bytes in Base64. Omitted: toolyard generates one and returns it once."},
				"client_receiver_id": map[string]any{"type": "string", "maxLength": 120, "description": "Your stable key for this receiver, so a retry returns the same callback_ref. Default: derived from the destination."},
				"environment_id":     map[string]any{"type": "string", "description": "Only for a federated T3 environment registering its own session-webhook URL."},
				"make_default":       map[string]any{"type": "boolean", "description": "Also make it your default receiver (callbacks.set_default)."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				dest := strings.TrimSpace(stringArg(args, "destination"))
				secret := strings.TrimSpace(stringArg(args, "secret"))
				clientID := strings.TrimSpace(stringArg(args, "client_receiver_id"))
				env := strings.TrimSpace(stringArg(args, "environment_id"))
				if dest == "" {
					return mcp.NewToolResultError("destination: give the https URL toolyard should POST events to"), nil
				}
				if clientID == "" {
					sum := sha256.Sum256([]byte(dest))
					clientID = "mcp:" + hex.EncodeToString(sum[:12])
				}
				privateOK := private != nil && private(ctx, agentID, env, dest)
				// A claimed environment must be the verified connection.
				if env != "" && !privateOK {
					return mcp.NewToolResultError("environment_id: the destination isn't this environment's session webhook (environment_destination_mismatch)"), nil
				}
				generated := false
				if secret == "" {
					// A retry of a registration whose generated secret was
					// shown once gets the receiver back, without the secret.
					if prev, err := svc.ByClientID(ctx, agentID, clientID); err == nil {
						if prev.Destination != dest || prev.EnvironmentID != env || prev.Status != "active" || prev.Transport != "push" {
							return callbackError(callbacks.ErrConflict), nil
						}
						out := receiverView(prev)
						out["secret_returned"] = false
						out["next"] = "Already registered. Its secret was shown once when toolyard generated it; if you lost it, rotate with callbacks.update {action: \"rotate\"}."
						return inboxJSON(out), nil
					}
					s, err := callbacks.GenerateSecret()
					if err != nil {
						return mcp.NewToolResultErrorFromErr("generate secret", err), nil
					}
					secret, generated = s, true
				}
				r, err := svc.Register(ctx, agentID, env, clientID, dest, secret, privateOK)
				if err != nil {
					return callbackError(err), nil
				}
				if boolArg(args, "make_default") {
					if err := svc.SetDefault(ctx, agentID, r.ID); err != nil {
						return callbackError(err), nil
					}
					r.Default = true
				}
				out := receiverView(r)
				if generated {
					out["secret"] = secret
					out["secret_note"] = callbackSecretNote
				}
				out["next"] = "Pass callback_ref on inbox.request / inbox.ask (callback_ref or callback_refs), or make it your default with callbacks.set_default. Check delivery with callbacks.test."
				return inboxJSON(out), nil
			}),
		inboxEntry("callbacks.list", inboxUpstream,
			"List your webhook receivers: callback_ref, destination, status, revision and which one is your default. "+
				"Give callback_ref to also see that receiver's recent deliveries (event id, type, status, attempts).",
			obj(nil, map[string]any{
				"callback_ref":    refProp,
				"include_removed": map[string]any{"type": "boolean", "description": "Also list removed receivers."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				list, err := svc.List(ctx, agentID, boolArg(args, "include_removed"))
				if err != nil {
					return mcp.NewToolResultErrorFromErr("list receivers", err), nil
				}
				views := make([]map[string]any, 0, len(list))
				def := ""
				for i := range list {
					views = append(views, receiverView(&list[i]))
					if list[i].Default {
						def = list[i].ID
					}
				}
				out := map[string]any{"receivers": views, "default": def}
				if ref := strings.TrimSpace(stringArg(args, "callback_ref")); ref != "" {
					history, err := svc.ReceiverHistory(ctx, agentID, ref)
					if err != nil {
						return callbackError(err), nil
					}
					out["deliveries"] = history
				}
				return inboxJSON(out), nil
			}),
		inboxEntry("callbacks.update", inboxUpstream,
			"Change one of your receivers: disable (stop delivery), rotate (new secret; give yours or toolyard generates one and returns it once) or remove. "+
				"Give expected_revision from callbacks.list; a repeated identical change is answered as before. Pending events follow a rotated secret; disabling or removing fails them.",
			obj([]string{"callback_ref", "action", "expected_revision"}, map[string]any{
				"callback_ref":      refProp,
				"action":            map[string]any{"type": "string", "enum": []string{"disable", "rotate", "remove"}},
				"expected_revision": map[string]any{"type": "integer", "minimum": 1, "description": "The receiver's current revision (callbacks.list)."},
				"secret":            map[string]any{"type": "string", "description": "rotate: the new whsec_ secret. Omitted: toolyard generates one and returns it once."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				ref := strings.TrimSpace(stringArg(args, "callback_ref"))
				action := stringArg(args, "action")
				revision, ok := asInt(args["expected_revision"])
				if ref == "" || !ok {
					return mcp.NewToolResultError("give callback_ref and expected_revision (callbacks.list)"), nil
				}
				secret := strings.TrimSpace(stringArg(args, "secret"))
				generated := false
				if action == "rotate" && secret == "" {
					s, err := callbacks.GenerateSecret()
					if err != nil {
						return mcp.NewToolResultErrorFromErr("generate secret", err), nil
					}
					secret, generated = s, true
				}
				r, err := svc.Change(ctx, agentID, ref, action, secret, revision)
				if err != nil {
					return callbackError(err), nil
				}
				out := receiverView(r)
				if generated {
					out["secret"] = secret
					out["secret_note"] = callbackSecretNote
				}
				return inboxJSON(out), nil
			}),
		inboxEntry("callbacks.set_default", inboxUpstream,
			"Make one of your receivers your default: it follows every inbox.request / inbox.ask (and every call that waits for a legacy approval) that names no callback_ref. "+
				"Give an empty callback_ref to clear the default.",
			obj([]string{"callback_ref"}, map[string]any{"callback_ref": refProp}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				ref := strings.TrimSpace(stringArg(args, "callback_ref"))
				if err := svc.SetDefault(ctx, agentID, ref); err != nil {
					return callbackError(err), nil
				}
				if ref == "" {
					return inboxJSON(map[string]any{"default": "", "next": "No default receiver: only items that name a callback_ref get callbacks."}), nil
				}
				return inboxJSON(map[string]any{"default": ref, "next": "Items and legacy approval calls that name no callback_ref now report to this receiver."}), nil
			}),
		inboxEntry("callbacks.test", inboxUpstream,
			"Send a signed ping event to one of your receivers, to check that it verifies the signature and answers 2xx. Read the delivery with callbacks.list {callback_ref}.",
			obj([]string{"callback_ref"}, map[string]any{"callback_ref": refProp}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				agentID, errRes := requireAgentID(ctx)
				if errRes != nil {
					return errRes, nil
				}
				id, err := svc.Test(ctx, agentID, strings.TrimSpace(stringArg(args, "callback_ref")))
				if err != nil {
					return callbackError(err), nil
				}
				return inboxJSON(map[string]any{"event_id": id, "type": callbacks.EventPing,
					"next": "Queued. Delivery starts within seconds and retries on failure; read its attempts with callbacks.list {callback_ref}."}), nil
			}),
	}
}

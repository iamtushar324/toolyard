package chatnotify

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
)

func TestDeciderLabel(t *testing.T) {
	cases := []struct {
		name string
		d    actor.Decider
		want string
	}{
		{"dashboard, by name", actor.Decider{UserID: "u_1", Email: "ada@beknown.work", Name: "Ada Lovelace", Via: actor.ViaDashboard}, "Ada Lovelace"},
		{"dashboard, by email", actor.Decider{UserID: "u_1", Email: "ada@beknown.work", Via: actor.ViaDashboard}, "ada@beknown.work"},
		{"dashboard, id only (legacy row)", actor.Decider{UserID: "u_1", Via: actor.ViaDashboard}, "u_1"},
		{"batch", actor.Decider{Name: "Ada", Via: actor.ViaDashboardBatch, Ref: "batch_1"}, "Ada via dashboard, batch"},
		{"push token, bound", actor.Decider{Name: "Ada", Via: actor.ViaPushToken}, "Ada via notification tap"},
		{"push token, anonymous", actor.Decider{Via: actor.ViaPushToken}, "notification tap"},
		{"passkey", actor.Decider{Name: "Ada", Via: actor.ViaPasskey, Ref: "cred_1"}, "Ada via passkey"},
		{"telegram, mapped to a person", actor.Decider{Name: "Ada", Via: actor.ViaTelegram, Ref: "12345"}, "Ada via Telegram"},
		{"telegram, unmapped", actor.Decider{Via: actor.ViaTelegram, Ref: "12345"}, "Telegram user 12345"},
		{"auto rule", actor.Decider{Via: actor.ViaAutoRule, Ref: "ar_9"}, "auto-approval rule ar_9"},
		{"auto rule with creator", actor.Decider{UserID: "u_1", Via: actor.ViaAutoRule, Ref: "ar_9"}, "u_1 via auto-approval rule ar_9"},
		{"policy", actor.Decider{Via: actor.ViaPolicy, Ref: "v0.1-default-write"}, "policy v0.1-default-write"},
		{"inbox grant", actor.Decider{Via: actor.ViaInboxGrant, Ref: "gr_1"}, "inbox grant gr_1"},
		{"agent cancel", actor.Decider{Via: actor.ViaAgentCancel, Ref: "ag_1"}, "agent ag_1 (cancelled)"},
		{"expiry", actor.Decider{Via: actor.ViaExpiry}, ""},
		{"nothing recorded", actor.Decider{}, ""},
	}
	for _, c := range cases {
		if got := deciderLabel(c.d); got != c.want {
			t.Errorf("%s: deciderLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

// The chat line names the recorded decider, not the raw decided_by
// column; legacy rows still read sensibly and an expired request has no
// decider line.
func TestRenderDecidedBy(t *testing.T) {
	person := &approval.Request{Status: approval.StatusAllowed, UpstreamName: "github", ToolName: "create_issue",
		DecidedBy: "u_1", DecidedVia: actor.ViaDashboard, DeciderEmail: "ada@beknown.work", DeciderName: "Ada Lovelace"}
	if out := render(person, false); !strings.Contains(out, "decided by Ada Lovelace") {
		t.Errorf("person: %q", out)
	}
	legacy := &approval.Request{Status: approval.StatusDenied, UpstreamName: "github", ToolName: "create_issue", DecidedBy: "telegram:12345"}
	if out := render(legacy, false); !strings.Contains(out, "decided by Telegram user 12345") {
		t.Errorf("legacy telegram: %q", out)
	}
	expired := &approval.Request{Status: approval.StatusExpired, UpstreamName: "github", ToolName: "create_issue"}
	if out := render(expired, false); strings.Contains(out, "decided by") {
		t.Errorf("expired: %q", out)
	}
	pending := &approval.Request{Status: approval.StatusPending, UpstreamName: "github", ToolName: "create_issue", DecidedBy: "u_1"}
	if out := render(pending, false); strings.Contains(out, "decided by") {
		t.Errorf("pending: %q", out)
	}
}

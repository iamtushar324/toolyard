package actor

import (
	"context"
	"strings"
	"testing"
)

func TestLegacy(t *testing.T) {
	cases := map[Decider]string{
		// Person instruments: the user id when known.
		{UserID: "u_1", Via: ViaDashboard}:                 "u_1",
		{UserID: "u_2", Via: ViaPushToken}:                 "u_2",
		{UserID: "u_3", Via: ViaPasskey, Ref: "cred_1"}:    "u_3",
		{UserID: "u_4", Via: ViaTelegram, Ref: "12345"}:    "u_4",
		{UserID: "u_5", Via: ViaInboxGrant, Ref: "gr_1"}:   "u_5",
		{UserID: "u_6", Via: ViaDashboardBatch, Ref: "b1"}: "u_6",
		{Via: ViaTelegram, Ref: "12345"}:                   "telegram:12345",
		{Via: ViaPushToken}:                                "push_token",
		// Machine instruments: never a user id, even when one is set.
		{Via: ViaAutoRule, Ref: "ar_9"}:                      "rule:ar_9",
		{UserID: "u_creator", Via: ViaAutoRule, Ref: "ar_9"}: "rule:ar_9",
		{Via: ViaAutoRule}:                                   "auto_rule",
		{Via: ViaPolicy, Ref: "pol_1"}:                       "policy:pol_1",
		{UserID: "u_x", Via: ViaAgentCancel, Ref: "ag_x"}:    "agent_cancel:ag_x",
		{Via: ViaAgentCancel, Ref: "ag_x"}:                   "agent_cancel:ag_x",
		{Via: ViaExpiry}:                                     "expiry",
	}
	for d, want := range cases {
		if got := d.Legacy(); got != want {
			t.Errorf("%+v.Legacy() = %q, want %q", d, got, want)
		}
	}
}

func TestMergeFillsOnlyEmpty(t *testing.T) {
	r := Raiser{CallerID: "ag_1", Via: "tools.execute"}
	got := r.Merge(Raiser{CallerID: "ag_2", AgentName: "bot", Via: "direct",
		ClientSessionID: "thr_1", ClientSessionClaimed: true})
	if got.CallerID != "ag_1" || got.Via != "tools.execute" {
		t.Fatalf("Merge overwrote set fields: %+v", got)
	}
	if got.AgentName != "bot" || got.ClientSessionID != "thr_1" || !got.ClientSessionClaimed {
		t.Fatalf("Merge did not fill empty fields: %+v", got)
	}
}

func TestRaiserContext(t *testing.T) {
	if _, ok := RaiserFrom(context.Background()); ok {
		t.Fatal("empty ctx reported a raiser")
	}
	ctx := WithRaiser(context.Background(), Raiser{CallerID: "ag_1"})
	if r, ok := RaiserFrom(ctx); !ok || r.CallerID != "ag_1" {
		t.Fatalf("RaiserFrom = %+v, %v", r, ok)
	}
}

func TestClean(t *testing.T) {
	if got := Clean("  thr\x00_1\n "); got != "thr_1" {
		t.Fatalf("Clean = %q", got)
	}
	if got := Clean(strings.Repeat("é", 300)); len([]rune(got)) != MaxLen {
		t.Fatalf("Clean did not cap: %d runes", len([]rune(got)))
	}
}

package actor

import (
	"context"
	"strings"
	"testing"
)

func TestLegacy(t *testing.T) {
	cases := map[Decider]string{
		{UserID: "u_1", Via: ViaDashboard}:     "u_1",
		{Via: ViaAutoRule, Ref: "ar_9"}:        "auto_rule:ar_9",
		{Via: ViaTelegram, Ref: "12345"}:       "telegram:12345",
		{Via: ViaExpiry}:                       "expiry",
		{UserID: "u_2", Via: ViaPushToken}:     "u_2",
		{Via: ViaAgentCancel, Ref: "ag_x"}:     "agent_cancel:ag_x",
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

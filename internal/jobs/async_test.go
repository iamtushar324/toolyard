package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestAsyncSettingRules(t *testing.T) {
	s := AsyncSetting{Mode: AsyncAfter, AfterMs: 2000, Tools: map[string]AsyncRule{
		"create_report": {Mode: AsyncAlways},
		"get_thing":     {Mode: AsyncOff},
	}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if r := s.RuleFor("create_report"); r.Mode != AsyncAlways || !r.Enabled() {
		t.Fatalf("tool rule = %+v", r)
	}
	if r := s.RuleFor("get_thing"); r.Mode != AsyncOff || r.Enabled() {
		t.Fatalf("tool off = %+v", r)
	}
	if r := s.RuleFor("other"); r.Mode != AsyncAfter || r.AfterMs != 2000 {
		t.Fatalf("server rule = %+v", r)
	}
	if !s.AnyEnabled() || s.IsZero() {
		t.Fatal("setting reads as off")
	}
	var zero AsyncSetting
	if !zero.IsZero() || zero.AnyEnabled() || zero.RuleFor("x").Mode != AsyncOff {
		t.Fatalf("zero setting = %+v", zero.RuleFor("x"))
	}
	n := AsyncSetting{Mode: AsyncAlways, AfterMs: 50, Tools: map[string]AsyncRule{"t": {}}}.Normalized()
	if n.AfterMs != 0 || n.Tools["t"].Mode != AsyncOff {
		t.Fatalf("Normalized = %+v", n)
	}
}

func TestAsyncSettingValidation(t *testing.T) {
	for name, s := range map[string]AsyncSetting{
		"unknown mode":     {Mode: "sometimes"},
		"after without ms": {Mode: AsyncAfter},
		"after too long":   {Mode: AsyncAfter, AfterMs: MaxAsyncAfterMs + 1},
		"bad tool mode":    {Tools: map[string]AsyncRule{"t": {Mode: "x"}}},
		"tool after short": {Tools: map[string]AsyncRule{"t": {Mode: AsyncAfter, AfterMs: 1}}},
		"blank tool name":  {Tools: map[string]AsyncRule{" ": {Mode: AsyncAlways}}},
		"long tool name":   {Tools: map[string]AsyncRule{strings.Repeat("x", 201): {Mode: AsyncAlways}}},
	} {
		if err := s.Validate(); !errors.Is(err, ErrInvalidAsync) {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
	tools := map[string]AsyncRule{}
	for i := 0; i <= MaxAsyncTools; i++ {
		tools[strings.Repeat("t", 1)+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = AsyncRule{Mode: AsyncAlways}
	}
	if err := (AsyncSetting{Tools: tools}).Validate(); !errors.Is(err, ErrInvalidAsync) {
		t.Errorf("too many tools: %v", err)
	}
}

func TestCallbackEvent(t *testing.T) {
	ev, d := CallbackEvent(Job{ID: "job_1", Tool: "alpha.x", Upstream: "alpha", Status: StatusSucceeded, ResultBytes: 10, CallbackRef: "cb_1", FinishedAt: 5})
	if ev != EventJobCompleted || d.JobID != "job_1" || d.ResultRef != "/v1/jobs/job_1/result" || d.CallbackRef != "cb_1" {
		t.Fatalf("succeeded = %s %+v", ev, d)
	}
	for _, st := range []string{StatusFailed, StatusCancelled, StatusOutcomeUnknown} {
		if ev, d := CallbackEvent(Job{ID: "job_2", Status: st, Error: "why"}); ev != EventJobFailed || d.ResultRef != "" || d.Error != "why" {
			t.Fatalf("%s = %s %+v", st, ev, d)
		}
	}
	if _, d := CallbackEvent(Job{ID: "job_3", Status: StatusOutcomeUnknown}); d.MayHaveRun == nil || !*d.MayHaveRun {
		t.Fatalf("outcome_unknown without may_have_run: %+v", d)
	}
	if _, d := CallbackEvent(Job{ID: "job_4", Status: StatusCancelled}); d.MayHaveRun == nil || *d.MayHaveRun {
		t.Fatalf("a job cancelled before it started may have run: %+v", d)
	}
	if _, d := CallbackEvent(Job{ID: "job_5", Status: StatusFailed}); d.MayHaveRun != nil {
		t.Fatalf("failed job with may_have_run: %+v", d)
	}
}

func TestEmitterNotifier(t *testing.T) {
	type call struct{ agent, ref, ev string }
	var got []call
	n := EmitterNotifier(func(_ context.Context, _ *sql.Tx, agentID, ref, eventType string, data any) error {
		if d, ok := data.(CallbackData); !ok || d.JobID == "" {
			t.Fatalf("data = %#v", data)
		}
		got = append(got, call{agentID, ref, eventType})
		return nil
	})
	ctx := context.Background()
	_ = n.JobFinished(ctx, nil, Job{ID: "job_1", AgentID: "ag_1", Status: StatusSucceeded})
	_ = n.JobFinished(ctx, nil, Job{ID: "job_2", AgentID: "ag_1", CallbackRef: "cb_1", Status: StatusOutcomeUnknown})
	_ = n.JobFinished(ctx, nil, Job{ID: "job_3", Status: StatusSucceeded}) // anonymous, no ref: nothing
	want := []call{{"ag_1", "", EventJobCompleted}, {"ag_1", "cb_1", EventJobFailed}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

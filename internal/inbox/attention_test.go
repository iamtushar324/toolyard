package inbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type pushLog struct {
	mu    sync.Mutex
	items []Push
}

func (l *pushLog) add(_ context.Context, p Push) {
	l.mu.Lock()
	l.items = append(l.items, p)
	l.mu.Unlock()
}
func (l *pushLog) take() []Push {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.items
	l.items = nil
	return out
}

func newAttnEnv(t *testing.T, cfg AttentionConfig) (*testEnv, *pushLog) {
	e := newEnv(t)
	log := &pushLog{}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.DigestTimes == nil {
		cfg.DigestTimes = []string{}
	}
	e.svc.opts.Notify = log.add
	e.svc.opts.Attention = func() AttentionConfig { return cfg }
	return e, log
}

func questionSubmission(urgency string) *Submission {
	return &Submission{Kind: KindQuestion, Title: "Retire the v1 endpoint?", Summary: "Two callers left.", Message: "m",
		Audio: Audio{Script: "Should I retire it now?"}, Urgency: urgency,
		Options: []Option{{Label: "Retire it now"}, {Label: "Wait a week"}}}
}

func mustSubmit(t *testing.T, e *testEnv, agent string, s *Submission) (*SubmitResult, *Request) {
	t.Helper()
	res, err := e.svc.Submit(context.Background(), agent, s)
	if err != nil || !res.OK {
		t.Fatalf("submit: %v %+v", err, res)
	}
	e.svc.Flush()
	r, err := e.svc.Get(context.Background(), res.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return res, r
}

func TestClockParsing(t *testing.T) {
	if m, ok := ParseClock("07:05"); !ok || m != 425 {
		t.Fatalf("07:05 → %d %v", m, ok)
	}
	for _, bad := range []string{"", "7", "24:00", "12:60", "12:5", "ab:cd"} {
		if _, ok := ParseClock(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, _, ok := ParseQuietHours("22:00-22:00"); ok {
		t.Error("empty range accepted")
	}
	c := AttentionConfig{QuietHours: "22:00-07:00", Location: time.UTC}
	in, end := quietWindow(c, time.Date(2026, 9, 28, 23, 30, 0, 0, time.UTC))
	if !in || !end.Equal(time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("23:30 → %v %v", in, end)
	}
	in, end = quietWindow(c, time.Date(2026, 9, 29, 6, 59, 0, 0, time.UTC))
	if !in || !end.Equal(time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("06:59 → %v %v", in, end)
	}
	if in, _ := quietWindow(c, time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC)); in {
		t.Fatal("07:00 should be outside quiet hours")
	}
	d := AttentionConfig{DigestTimes: []string{"09:30", "18:30"}, Location: time.UTC}
	slot, ok := digestSlot(d, time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC))
	if !ok || !slot.Equal(time.Date(2026, 9, 28, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("slot before first time of day → %v", slot)
	}
}

func TestNowPushesAtOnceAndBudgetDowngrades(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{NowPerHour: 2})
	for i := 0; i < 2; i++ {
		mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
		if got := log.take(); len(got) != 1 || got[0].Reason != "new" {
			t.Fatalf("now request %d: want one immediate push, got %+v", i, got)
		}
	}
	res, r := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	if r.Urgency != UrgencySoon || r.RequestedUrgency != UrgencyNow || r.Downgraded == "" {
		t.Fatalf("third now request not downgraded: %+v", r)
	}
	if len(res.Warnings) == 0 || res.Warnings[len(res.Warnings)-1].Path != "urgency" {
		t.Fatalf("agent wasn't told: %+v", res.Warnings)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("downgraded request pushed at once: %+v", got)
	}
	// Another agent has its own budget.
	mustSubmit(t, e, "ag_2", questionSubmission(UrgencyNow))
	if got := log.take(); len(got) != 1 {
		t.Fatalf("other agent's now request: %+v", got)
	}
	e.advance(GroupWindow + time.Second)
	e.svc.Tick(context.Background())
	if got := log.take(); len(got) != 1 || got[0].RequestID != r.ID {
		t.Fatalf("downgraded request should push after the window: %+v", got)
	}
	// An hour later the budget is back.
	e.advance(time.Hour)
	_, r4 := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	if r4.Urgency != UrgencyNow {
		t.Fatal("budget didn't refill")
	}
}

func TestSoonGroupsPerSession(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{})
	ctx := context.Background()
	ss, _ := e.svc.StartSession(ctx, "ag_1", "Ship billing v2", "", "", "")
	for i := 0; i < 3; i++ {
		q := questionSubmission(UrgencySoon)
		q.SessionID = ss.ID
		mustSubmit(t, e, "ag_1", q)
	}
	mustSubmit(t, e, "ag_2", questionSubmission(UrgencySoon)) // no session: grouped per agent
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("soon pushed before the grouping window: %+v", got)
	}
	e.advance(GroupWindow)
	e.svc.Tick(ctx)
	got := log.take()
	if len(got) != 2 {
		t.Fatalf("want one grouped push + one single, got %+v", got)
	}
	var grouped *Push
	for i := range got {
		if got[i].Reason == "grouped" {
			grouped = &got[i]
		}
	}
	if grouped == nil || grouped.Tag != ss.ID || !strings.HasPrefix(grouped.Title, "3 requests") {
		t.Fatalf("grouped push: %+v", got)
	}
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("pushed twice: %+v", got)
	}
}

func TestQuietHoursHoldAndAllowList(t *testing.T) {
	cfg := AttentionConfig{QuietHours: "17:00-19:00"} // the test clock starts at 18:00 UTC
	e, log := newAttnEnv(t, cfg)
	ctx := context.Background()
	s := deploySubmission()
	s.Urgency = UrgencyNow
	_, r := mustSubmit(t, e, "ag_1", s)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("pushed during quiet hours: %+v", got)
	}
	e.advance(59 * time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("pushed during quiet hours: %+v", got)
	}
	e.advance(time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 1 || got[0].RequestID != r.ID {
		t.Fatalf("held push not sent at the end of quiet hours: %+v", got)
	}

	cfg.QuietHours = "19:00-21:00"
	cfg.QuietAllow = []string{"db.migrate", "deploy.*", "flags.set"}
	e2, log2 := newAttnEnv(t, cfg)
	e2.advance(time.Hour) // 19:00
	s = deploySubmission()
	s.Urgency = UrgencyNow
	mustSubmit(t, e2, "ag_1", s)
	if got := log2.take(); len(got) != 1 {
		t.Fatalf("allow-listed now request should break through: %+v", got)
	}
	s = deploySubmission()
	s.Urgency = UrgencyNow
	s.Tools = append(s.Tools, SubmissionTool{
		CallID: "notify_team", Tool: "slack.post_message", Required: false,
		Summary: "Tell the engineering team that billing v2 is deployed.",
		Target:  "Slack channel #eng", Operation: "write",
		ExpectedEffects: "The engineering team sees the billing v2 deployment notice.",
		AffectedScope:   "One deployment notice in Slack channel #eng.",
		MaterialRisks:   "An incorrect notice can confuse the engineering team.",
		Undo:            "Delete the deployment notice from Slack channel #eng.",
		Params:          map[string]any{"channel": "#eng"},
	})
	mustSubmit(t, e2, "ag_1", s)
	if got := log2.take(); len(got) != 0 {
		t.Fatalf("a tool off the allow list broke through: %+v", got)
	}
}

func TestDigestOncePerSlot(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{DigestTimes: []string{"18:30"}})
	ctx := context.Background()
	mustSubmit(t, e, "ag_1", questionSubmission(UrgencyDigest))
	mustSubmit(t, e, "ag_2", &Submission{Kind: KindUpdate, Title: "Billing v2 shipped", Summary: "s", Message: "m",
		Audio: Audio{Script: "Shipped."}, Urgency: UrgencyDigest})
	mustSubmit(t, e, "ag_2", &Submission{Kind: KindUpdate, Title: "FYI", Summary: "s", Message: "m",
		Audio: Audio{Script: "FYI."}, Urgency: UrgencyFYI})
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("digest items pushed on their own: %+v", got)
	}
	e.advance(31 * time.Minute)
	e.svc.Tick(ctx)
	got := log.take()
	if len(got) != 1 || got[0].Reason != "digest" || !strings.Contains(got[0].Body, "1 question and 1 update from 2 agents") {
		t.Fatalf("digest: %+v", got)
	}
	e.svc.Tick(ctx)
	e.advance(10 * time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("digest sent twice: %+v", got)
	}
	// Next day's slot: the same items aren't repeated.
	e.advance(24 * time.Hour)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("already-digested items sent again: %+v", got)
	}
}

func TestOneReminderWhenBlocked(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{})
	ctx := context.Background()
	ss, _ := e.svc.StartSession(ctx, "ag_1", "Ship billing v2", "", "", "")
	s := deploySubmission()
	s.SessionID = ss.ID
	_, r := mustSubmit(t, e, "ag_1", s)
	e.advance(GroupWindow)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 1 {
		t.Fatalf("first push: %+v", got)
	}
	e.advance(20 * time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("reminded while the agent wasn't blocked: %+v", got)
	}
	if _, err := e.svc.UpdateSession(ctx, "ag_1", ss.ID, SessionBlockedOnOwner, ""); err != nil {
		t.Fatal(err)
	}
	e.svc.Tick(ctx)
	got := log.take()
	if len(got) != 1 || got[0].Reason != "reminder" || !strings.HasPrefix(got[0].Title, "Still waiting") {
		t.Fatalf("reminder: %+v", got)
	}
	e.advance(2 * time.Hour)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("second reminder: %+v", got)
	}
	if r2, _ := e.svc.Get(ctx, r.ID); r2.RemindedAt == 0 {
		t.Fatal("reminded_at not recorded")
	}
}

func TestSnoozeComesBack(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{})
	ctx := context.Background()
	_, r := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	log.take()
	if _, err := e.decide(ctx, r.ID, Decision{Action: "snooze", SnoozeMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	e.advance(59 * time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("pushed while snoozed: %+v", got)
	}
	e.advance(time.Minute)
	e.svc.Tick(ctx)
	got := log.take()
	if len(got) != 1 || got[0].Reason != "snooze_end" {
		t.Fatalf("snooze end: %+v", got)
	}
	// Re-snoozing makes the earlier wake-up a no-op.
	if _, err := e.decide(ctx, r.ID, Decision{Action: "snooze", SnoozeMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.decide(ctx, r.ID, Decision{Action: "snooze", SnoozeMinutes: 120}); err != nil {
		t.Fatal(err)
	}
	e.advance(31 * time.Minute)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("stale snooze wake-up fired: %+v", got)
	}
	// Decided while snoozed: nothing comes back.
	if _, err := e.decide(ctx, r.ID, Decision{Action: "answer", Option: new(int)}); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour)
	e.svc.Tick(ctx)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("decided request came back: %+v", got)
	}
}

func TestPushPrivacyAndActions(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{})
	s := deploySubmission()
	s.Urgency = UrgencyNow
	_, r := mustSubmit(t, e, "ag_1", s)
	p := log.take()[0]
	for _, secret := range []string{r.Title, r.Summary, "billing"} {
		if strings.Contains(p.Title+p.Body, secret) {
			t.Fatalf("push without details leaked %q: %+v", secret, p)
		}
	}
	for _, a := range p.Actions {
		if a.Action == "approve" || a.Action == "allow" {
			t.Fatal("a notification must never approve")
		}
	}
	if !strings.Contains(p.Body, "High risk") {
		t.Fatalf("high-risk request not marked: %q", p.Body)
	}

	// With details on, the watch gets the title, summary and scope line.
	e.svc.opts.Attention = func() AttentionConfig {
		return AttentionConfig{Details: true, Location: time.UTC, DigestTimes: []string{}}
	}
	_, q := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	p = log.take()[0]
	if p.Title != q.Title || len(p.Actions) != 2 || p.Actions[1].Title != "Wait a week" {
		t.Fatalf("detailed question push: %+v", p)
	}
	if line := ScopeLine(r); !strings.Contains(line, "db.migrate env=prod") || !strings.Contains(line, "+2 tools") {
		t.Fatalf("scope line: %q", line)
	}
}

func TestDecideByTap(t *testing.T) {
	e, log := newAttnEnv(t, AttentionConfig{Details: true})
	ctx := context.Background()
	s := deploySubmission()
	s.Urgency = UrgencyNow
	_, r := mustSubmit(t, e, "ag_1", s)
	p := log.take()[0]

	for _, bad := range []string{"approve", "allow", "opt0"} {
		if _, err := e.svc.DecideByTap(ctx, p.TapToken, bad); !errors.Is(err, ErrTapToken) {
			t.Fatalf("action %q accepted: %v", bad, err)
		}
	}
	tampered := p.TapToken[:len(p.TapToken)-3] + "AAA"
	if _, err := e.svc.DecideByTap(ctx, tampered, TapDeny); !errors.Is(err, ErrTapToken) {
		t.Fatalf("tampered token accepted: %v", err)
	}
	if got, err := e.svc.DecideByTap(ctx, p.TapToken, TapSnooze); err != nil || got.SnoozedUntil == 0 || got.Status != StatusPending {
		t.Fatalf("snooze by tap: %v %+v", err, got)
	}
	if got, err := e.svc.DecideByTap(ctx, p.TapToken, TapDeny); err != nil || got.Status != StatusDenied || got.DecidedBy != "notification" {
		t.Fatalf("deny by tap: %v %+v", err, got)
	}
	if _, err := e.svc.DecideByTap(ctx, p.TapToken, TapDeny); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second tap: %v", err)
	}
	grants, _ := e.svc.ListGrants(ctx, "", r.AgentID, 10)
	if len(grants) != 0 {
		t.Fatal("a tap issued grants")
	}

	_, q := mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	p = log.take()[0]
	if got, err := e.svc.DecideByTap(ctx, p.TapToken, "opt1"); err != nil || got.Answer != "Wait a week" || got.ID != q.ID {
		t.Fatalf("answer by tap: %v %+v", err, got)
	}

	mustSubmit(t, e, "ag_1", questionSubmission(UrgencyNow))
	p = log.take()[0]
	e.advance(RequestTTL + time.Minute)
	if _, err := e.svc.DecideByTap(ctx, p.TapToken, "opt0"); !errors.Is(err, ErrTapToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/docs"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

func f64(v float64) *float64 { return &v }

// everyQuestionType is one question of every schema version 3 type.
func everyQuestionType() []QuestionSpec {
	opts := []Option{{ID: "stage", Label: "Stage"}, {ID: "prod", Label: "Production"}, {ID: "none", Label: "Neither", Exclusive: true}}
	rank := []Option{{ID: "speed", Label: "Speed"}, {ID: "cost", Label: "Cost"}, {ID: "safety", Label: "Safety"}}
	return []QuestionSpec{
		{ID: "notes", Type: QuestionFreeText, Prompt: "Anything I should know?", Multiline: true, MaxLength: 500},
		{ID: "env", Type: QuestionSingleChoice, Prompt: "Where should it run first?", Options: opts[:2], Recommended: "stage"},
		{ID: "checks", Type: QuestionMultipleChoice, Prompt: "Which checks must pass?", Options: opts, MinSelections: 1, AllowOther: true},
		{ID: "notify", Type: QuestionYesNo, Prompt: "Shall I tell the team?", Default: true},
		{ID: "budget", Type: QuestionNumber, Prompt: "How many dollars may I spend?", Min: f64(0), Max: f64(500), Step: f64(25), Unit: "USD"},
		{ID: "start", Type: QuestionDate, Prompt: "From which day?"},
		{ID: "window", Type: QuestionDateTime, Prompt: "When does the change window open?"},
		{ID: "confidence", Type: QuestionScale, Prompt: "How confident are you in the plan?", MinLabel: "Not at all", MaxLabel: "Fully"},
		{ID: "priorities", Type: QuestionRanking, Prompt: "Order these by importance.", Options: rank},
	}
}

func richSubmission() *Submission {
	sub := deploySubmission()
	sub.SchemaVersion = SchemaVersionRich
	sub.Questions = everyQuestionType()
	sub.Message = "## Plan\n\nPR **#218** is approved.\n\n| check | result |\n|---|---|\n| unit | 212 passed |"
	return sub
}

func submitRich(t *testing.T, e *testEnv) *Request {
	t.Helper()
	res, err := e.svc.Submit(context.Background(), "ag_1", richSubmission())
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	if res.Preview == nil || res.Preview.Questions != 9 || res.Preview.Tools != 3 {
		t.Fatalf("preview: %+v", res.Preview)
	}
	e.svc.Flush()
	r, err := e.svc.Get(context.Background(), res.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func completeAnswers() map[string]Answer {
	return map[string]Answer{
		"notes":      {Value: "Keep the flag off."},
		"env":        {Value: "stage"},
		"checks":     {Value: []any{"stage", "prod"}, Text: "Also run the smoke test."},
		"notify":     {Value: false},
		"budget":     {Value: 125.0},
		"start":      {Value: "2026-11-02"},
		"window":     {Value: "2026-11-02T09:30:00+05:30"},
		"confidence": {Value: 4.0},
		"priorities": {Value: []any{"safety", "speed", "cost"}},
	}
}

func richDecision(r *Request, id string, answers map[string]Answer) Decision {
	verdicts := map[string]CallVerdict{}
	for i, t := range r.Tools {
		v := VerdictAccepted
		if i == 2 {
			v = VerdictRejected
		}
		verdicts[t.CallID] = CallVerdict{Verdict: v}
	}
	return Decision{Action: "submit", RequestRevision: r.Revision, SubmissionID: id, Verdicts: verdicts, Answers: answers,
		Decider: actor.Decider{UserID: "u_owner", Via: actor.ViaDashboard}}
}

// A version 3 item with three tool calls and one question of every type
// validates, and its questions are stored normalized.
func TestRichItemValidatesEveryQuestionType(t *testing.T) {
	e := newEnv(t)
	r := submitRich(t, e)
	if r.SchemaVersion != SchemaVersionRich || len(r.Tools) != 3 || len(r.Questions) != 9 {
		t.Fatalf("stored item: version %d, %d tools, %d questions", r.SchemaVersion, len(r.Tools), len(r.Questions))
	}
	byID := map[string]QuestionSpec{}
	for _, q := range r.Questions {
		byID[q.ID] = q
		if q.Required == nil || !*q.Required {
			t.Errorf("%s: required must default to true", q.ID)
		}
	}
	if byID["confidence"].Scale != 5 || byID["checks"].MaxSelections != 3 || byID["env"].MaxSelections != 1 {
		t.Fatalf("limits not filled in: %+v", byID)
	}
	if byID["notify"].Default != true || byID["env"].Recommended != "stage" {
		t.Fatalf("default and recommended: %+v %+v", byID["notify"].Default, byID["env"].Recommended)
	}
}

func TestRichItemRefusesBadQuestions(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name  string
		edit  func(*Submission)
		paths []string
	}{
		{"needs version 3", func(s *Submission) { s.SchemaVersion = 0 }, []string{"questions"}},
		{"bad id", func(s *Submission) { s.Questions[0].ID = "has space" }, []string{"questions[0].id"}},
		{"duplicate id", func(s *Submission) { s.Questions[1].ID = s.Questions[0].ID }, []string{"questions[1].id"}},
		{"unknown type", func(s *Submission) { s.Questions[0].Type = "slider" }, []string{"questions[0].type"}},
		{"one option", func(s *Submission) {
			s.Questions[1].Options = s.Questions[1].Options[:1]
			s.Questions[1].Recommended = nil
		}, []string{"questions[1].options"}},
		{"options on yes_no", func(s *Submission) { s.Questions[3].Options = []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}} }, []string{"questions[3].options"}},
		{"scale too long", func(s *Submission) { s.Questions[7].Scale = 11 }, []string{"questions[7].scale"}},
		{"min above max", func(s *Submission) { s.Questions[4].Min = f64(600) }, []string{"questions[4].min"}},
		{"unknown default", func(s *Submission) { s.Questions[1].Default = "qa" }, []string{"questions[1].default"}},
		{"default off step", func(s *Submission) { s.Questions[4].Default = 30.0 }, []string{"questions[4].default"}},
		{"missing prompt", func(s *Submission) { s.Questions[5].Prompt = " " }, []string{"questions[5].prompt"}},
		{"too many", func(s *Submission) {
			for len(s.Questions) <= MaxQuestions {
				q := s.Questions[0]
				q.ID = "extra_" + strings.Repeat("x", len(s.Questions))
				s.Questions = append(s.Questions, q)
			}
		}, []string{"questions"}},
	}
	for _, c := range cases {
		sub := richSubmission()
		c.edit(sub)
		res, err := e.svc.Submit(context.Background(), "ag_1", sub)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK {
			t.Fatalf("%s: accepted", c.name)
		}
		for _, want := range c.paths {
			found := false
			for _, p := range res.Problems {
				found = found || p.Path == want
			}
			if !found {
				t.Errorf("%s: no problem at %s: %+v", c.name, want, res.Problems)
			}
		}
	}
}

// One decision answers every call and every question; bad answers are
// refused and change nothing.
func TestRichDecisionRecordsVerdictsAndAnswers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := submitRich(t, e)
	bad := map[string]func(map[string]Answer){
		"number above max":   func(a map[string]Answer) { a["budget"] = Answer{Value: 750.0} },
		"number off step":    func(a map[string]Answer) { a["budget"] = Answer{Value: 130.0} },
		"unknown option":     func(a map[string]Answer) { a["env"] = Answer{Value: "qa"} },
		"exclusive with":     func(a map[string]Answer) { a["checks"] = Answer{Value: []any{"none", "stage"}} },
		"incomplete ranking": func(a map[string]Answer) { a["priorities"] = Answer{Value: []any{"safety", "speed"}} },
		"repeated ranking":   func(a map[string]Answer) { a["priorities"] = Answer{Value: []any{"safety", "safety", "cost"}} },
		"missing required":   func(a map[string]Answer) { delete(a, "start") },
		"empty required":     func(a map[string]Answer) { a["notes"] = Answer{Value: "   "} },
		"bad date":           func(a map[string]Answer) { a["start"] = Answer{Value: "02/11/2026"} },
		"bad datetime":       func(a map[string]Answer) { a["window"] = Answer{Value: "2026-11-02 09:30"} },
		"scale out of range": func(a map[string]Answer) { a["confidence"] = Answer{Value: 6.0} },
		"yes_no as string":   func(a map[string]Answer) { a["notify"] = Answer{Value: "yes"} },
		"unknown question":   func(a map[string]Answer) { a["extra"] = Answer{Value: "x"} },
		"too long text":      func(a map[string]Answer) { a["notes"] = Answer{Value: strings.Repeat("x", 501)} },
	}
	for name, edit := range bad {
		answers := completeAnswers()
		edit(answers)
		if _, err := e.svc.Decide(ctx, r.ID, richDecision(r, "bad_"+name, answers)); !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("%s: want ErrBadAnswer, got %v", name, err)
		}
	}
	if cur, _ := e.svc.Get(ctx, r.ID); cur.Status != StatusPending || cur.Answers != nil {
		t.Fatalf("refused answers changed the item: %+v", cur)
	}

	got, err := e.svc.Decide(ctx, r.ID, richDecision(r, "complete", completeAnswers()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusApproved || got.Tools[0].Verdict != VerdictAccepted || got.Tools[2].Verdict != VerdictRejected {
		t.Fatalf("verdicts: %+v", got.Tools)
	}
	if len(got.Answers) != 9 {
		t.Fatalf("answers: %+v", got.Answers)
	}
	if v := got.Answers["priorities"]; strings.Join(v.Labels, ",") != "Safety,Speed,Cost" {
		t.Fatalf("ranking labels: %+v", v)
	}
	if v := got.Answers["notify"]; v.Value != false || v.Labels[0] != "No" {
		t.Fatalf("yes_no: %+v", v)
	}
	if !strings.Contains(got.Answer, "How many dollars may I spend?: 125 USD") {
		t.Fatalf("answer summary: %q", got.Answer)
	}

	views, err := e.svc.Status(ctx, "ag_1", []string{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(views[0])
	var view struct {
		Answers map[string]struct {
			Value  any      `json:"value"`
			Text   string   `json:"text"`
			Labels []string `json:"labels"`
		} `json:"answers"`
		Next string `json:"next"`
	}
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	if view.Answers["budget"].Value != 125.0 || view.Answers["checks"].Text != "Also run the smoke test." || view.Answers["env"].Value != "stage" {
		t.Fatalf("inbox.status answers: %s", b)
	}
	if !strings.Contains(view.Next, "answers") {
		t.Fatalf("next step should mention the answers: %q", view.Next)
	}

	// The same submission is a replay; other content under it conflicts.
	replay, err := e.svc.Decide(ctx, r.ID, richDecision(r, "complete", completeAnswers()))
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: %v", err)
	}
	other := completeAnswers()
	other["notify"] = Answer{Value: true}
	if _, err := e.svc.Decide(ctx, r.ID, richDecision(r, "complete", other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed answers under the same submission: %v", err)
	}
}

// Optional questions may stay unanswered; a custom answer stands in for a
// selection when allow_other is set.
func TestRichOptionalAndCustomAnswers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sub := richSubmission()
	no := false
	for i := range sub.Questions {
		sub.Questions[i].Required = &no
	}
	res, err := e.svc.Submit(ctx, "ag_1", sub)
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	r, _ := e.svc.Get(ctx, res.RequestID)
	got, err := e.svc.Decide(ctx, r.ID, richDecision(r, "custom", map[string]Answer{"checks": {Text: "Only the load test."}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Answers) != 1 || got.Answers["checks"].Text != "Only the load test." || got.Answers["checks"].Value != nil {
		t.Fatalf("answers: %+v", got.Answers)
	}
}

// A question item may carry several questions; one answer set closes it.
func TestRichQuestionItem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var s Submission
	if err := json.Unmarshal([]byte(`{"schema_version":3,"kind":"question","title":"Release choices","summary":"Two choices block the release.","message":"I need both answers before I tag.",
		"questions":[{"id":"tag","type":"yes_no","prompt":"Tag v2.1 today?"},{"id":"order","type":"ranking","prompt":"Order the fixes.","options":[{"id":"a","label":"Login"},{"id":"b","label":"Search"}]}]}`), &s); err != nil {
		t.Fatal(err)
	}
	s.Kind = KindQuestion
	res, err := e.svc.Submit(ctx, "ag_1", &s)
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	e.svc.Flush()
	var d Decision
	if err := json.Unmarshal([]byte(`{"action":"answer","request_revision":1,"submission_id":"ans_1","answers":{"tag":{"value":true},"order":{"value":["b","a"]}}}`), &d); err != nil {
		t.Fatal(err)
	}
	partial := d
	partial.Answers = map[string]Answer{"tag": {Value: true}}
	if _, err := e.svc.Decide(ctx, res.RequestID, partial); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("a partial answer set must be refused: %v", err)
	}
	got, err := e.svc.Decide(ctx, res.RequestID, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAnswered || got.Answers["order"].Labels[0] != "Search" || !strings.Contains(got.Answer, "Tag v2.1 today?: Yes") {
		t.Fatalf("answered item: %+v", got)
	}
	again, err := e.svc.Decide(ctx, res.RequestID, d)
	if err != nil || !again.Replayed {
		t.Fatalf("answer replay: %v", err)
	}
	stale := d
	stale.SubmissionID = "ans_2"
	if _, err := e.svc.Decide(ctx, res.RequestID, stale); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second answer set: %v", err)
	}
	views, _ := e.svc.Status(ctx, "ag_1", []string{res.RequestID})
	if views[0].Answers["tag"].Value != true || !strings.Contains(views[0].Next, "answers") {
		t.Fatalf("status: %+v", views[0])
	}
}

// The single version 2 question still works with version 3 and becomes
// questions[0] with the ID "answer"; a question item needs a question.
func TestRichSingleQuestionIsMapped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := &Submission{SchemaVersion: SchemaVersionRich, Kind: KindQuestion, Prompt: "Which region?",
		Question: &Question{Type: QuestionSingleChoice, Options: []Option{{ID: "eu", Label: "Europe"}, {ID: "us", Label: "United States"}}}}
	res, err := e.svc.Submit(ctx, "ag_1", s)
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	r, _ := e.svc.Get(ctx, res.RequestID)
	if len(r.Questions) != 1 || r.Questions[0].ID != "answer" || !r.Questions[0].AllowOther || r.Question != nil || r.Title != "Which region?" {
		t.Fatalf("mapped question: %+v %+v", r.Questions, r)
	}
	got, err := e.decide(ctx, r.ID, Decision{Action: "answer", RequestRevision: r.Revision, SubmissionID: "s1", Answers: map[string]Answer{"answer": {Text: "Asia, please."}}})
	if err != nil || got.Answers["answer"].Text != "Asia, please." {
		t.Fatalf("custom answer: %+v %v", got, err)
	}
	empty := &Submission{SchemaVersion: SchemaVersionRich, Kind: KindQuestion, Title: "Nothing to ask", Summary: "No question.", Message: "None here."}
	res, err = e.svc.Submit(ctx, "ag_1", empty)
	if err != nil || res.OK {
		t.Fatalf("a question item without questions: %+v %v", res, err)
	}
}

// The decision callback receives the answers with the verdicts.
func TestRichDecisionCallbackCarriesAnswers(t *testing.T) {
	e := newEnv(t)
	installTestCallbacks(t, e)
	ctx := context.Background()
	sub := richSubmission()
	sub.CallbackRef = "cb_one"
	res, err := e.svc.Submit(ctx, "ag_1", sub)
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	r, _ := e.svc.Get(ctx, res.RequestID)
	if _, err := e.svc.Decide(ctx, r.ID, richDecision(r, "with_answers", completeAnswers())); err != nil {
		t.Fatal(err)
	}
	var doc string
	if err := e.svc.db.QueryRow(`SELECT doc FROM batch_test_events`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	var event Request
	if err := json.Unmarshal([]byte(doc), &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Answers) != 9 || len(event.Tools) != 3 {
		t.Fatalf("callback without answers: %s", doc)
	}
}

func TestRichChartsValidate(t *testing.T) {
	ok := []Attachment{
		{Type: "chart", Title: "Errors by service", Chart: "pie", X: []any{"api", "web"}, Series: []Series{{Name: "errors", Values: []float64{3, 1}}}},
		{Type: "chart", Title: "Latency vs load", Chart: "scatter", X: []any{json.Number("1"), 2.5, 4}, Series: []Series{{Name: "p95", Values: []float64{120, 180, 260}}}},
		{Type: "chart", Title: "Deploys", Chart: "stacked_bar", X: []any{"Mon", "Tue"}, Series: []Series{{Name: "ok", Values: []float64{4, 5}}, {Name: "failed", Values: []float64{1, 0}}}},
		{Type: "chart", Title: "Traffic", Chart: "area", Series: []Series{{Name: "rps", Values: []float64{1, -2, 3}}}},
		{Type: "chart", Title: "Old line", Chart: "line", Series: []Series{{Name: "rps", Values: []float64{1, 2}}}},
	}
	bad := map[string]Attachment{
		"pie two series":    {Type: "chart", Title: "x", Chart: "pie", X: []any{"a"}, Series: []Series{{Values: []float64{1}}, {Values: []float64{2}}}},
		"pie no labels":     {Type: "chart", Title: "x", Chart: "pie", Series: []Series{{Values: []float64{1, 2}}}},
		"pie zero total":    {Type: "chart", Title: "x", Chart: "pie", X: []any{"a"}, Series: []Series{{Values: []float64{0}}}},
		"stacked negative":  {Type: "chart", Title: "x", Chart: "stacked_bar", Series: []Series{{Values: []float64{1, -1}}}},
		"scatter string x":  {Type: "chart", Title: "x", Chart: "scatter", X: []any{"a"}, Series: []Series{{Values: []float64{1}}}},
		"scatter without x": {Type: "chart", Title: "x", Chart: "scatter", Series: []Series{{Values: []float64{1}}}},
		"unknown kind":      {Type: "chart", Title: "x", Chart: "radar", Series: []Series{{Values: []float64{1}}}},
	}
	check := func(a Attachment) []Problem {
		s := &Submission{Kind: KindUpdate, Title: "Weekly numbers", Summary: "The numbers of the week.", Message: "Here are the numbers.", Urgency: UrgencyFYI, Attachments: []Attachment{a}}
		_, probs, _ := Validate(context.Background(), testCatalog, "ag_1", s)
		return probs
	}
	for _, a := range ok {
		if probs := check(a); len(probs) > 0 {
			t.Errorf("%s chart refused: %+v", a.Chart, probs)
		}
	}
	for name, a := range bad {
		if probs := check(a); len(probs) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRichLimitsAndMarkdownMessage(t *testing.T) {
	sub := richSubmission()
	sub.Message = strings.Repeat("Long plan line. ", 1200) // 19,200 characters
	for len(sub.Tools) < MaxTools {
		tl := sub.Tools[0]
		tl.CallID = "extra_" + strings.Repeat("y", len(sub.Tools))
		sub.Tools = append(sub.Tools, tl)
	}
	_, probs, _ := Validate(context.Background(), testCatalog, "ag_1", sub)
	if len(probs) > 0 {
		t.Fatalf("50 tools and a 19,200-character message must validate: %+v", probs)
	}
	sub.Tools = append(sub.Tools, sub.Tools[0])
	sub.Tools[len(sub.Tools)-1].CallID = "one_too_many"
	sub.Message = strings.Repeat("x", MaxMessage+1)
	_, probs, _ = Validate(context.Background(), testCatalog, "ag_1", sub)
	paths := map[string]bool{}
	for _, p := range probs {
		paths[p.Path] = true
	}
	if !paths["tools"] || !paths["message"] {
		t.Fatalf("limits: %+v", probs)
	}
	// An image that isn't the agent's own upload is a warning, not an error.
	sub = richSubmission()
	sub.Message += "\n\n![graph](https://example.com/graph.png)"
	_, probs, warns := Validate(context.Background(), testCatalog, "ag_1", sub)
	if len(probs) > 0 || len(warns) == 0 || warns[len(warns)-1].Path != "message" {
		t.Fatalf("external image: problems %+v warnings %+v", probs, warns)
	}
}

// A Markdown image that cites the agent's own upload is resolved when the
// item is sent; another agent's upload is refused.
func TestRichInlineImagesResolveOwnUploads(t *testing.T) {
	u := newUploadEnv(t, false)
	ctx := context.Background()
	data := testPNG(31)
	ref := u.upload(t, "ag_1", pngSpec(data), data)
	post := uploadPost()
	post.Message = "The page after the fix:\n\n![checkout](" + ref + ")"
	res, err := u.svc.Submit(ctx, "ag_2", post)
	if err != nil || res.OK || res.Problems[0].Path != "message" {
		t.Fatalf("another agent's upload: %+v %v", res, err)
	}
	res, err = u.svc.Submit(ctx, "ag_1", post)
	if err != nil || !res.OK {
		t.Fatalf("own upload: %+v %v", res, err)
	}
	r, _ := u.svc.Get(ctx, res.RequestID)
	if len(r.InlineImages) != 1 || r.InlineImages[0].Blob != hexSum(data) || r.InlineImages[0].Ref != ref {
		t.Fatalf("inline images: %+v", r.InlineImages)
	}
	if !u.svc.BlobBelongsToAgent(ctx, "ag_1", hexSum(data)) {
		t.Fatal("the owner must be able to load the inline image")
	}
}

// The schema version 3 example in the agent protocol is a valid request.
func TestProtocolRichExampleValidates(t *testing.T) {
	g := newGuide(docs.AgentProtocol, nil)
	text, ok := g.Topic("questions")
	if !ok {
		t.Fatal("questions topic missing")
	}
	m := regexp.MustCompile("(?s)```json\\ninbox\\.request\\((.*?)\\)\\n```").FindStringSubmatch(text)
	if m == nil {
		t.Fatal("no inbox.request example in the questions topic")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(m[1]), &args); err != nil {
		t.Fatalf("example isn't valid JSON: %v", err)
	}
	sub, err := DecodeSubmission(args)
	if err != nil {
		t.Fatal(err)
	}
	sub.Kind = KindAccess
	_, probs, _ := Validate(context.Background(), testCatalog, "a", sub)
	if len(probs) > 0 {
		t.Fatalf("protocol example: %+v", probs)
	}
	if _, ok := g.Topic("callbacks"); !ok {
		t.Fatal("callbacks topic missing")
	}
}

// execCallbacks records the execution.completed hook.
type execCallbacks struct {
	runs []string
	fail bool
}

func (c *execCallbacks) RegisterTx(context.Context, *sql.Tx, *Request, string) error { return nil }
func (c *execCallbacks) DecisionTx(context.Context, *sql.Tx, *Request) error         { return nil }

func (c *execCallbacks) ExecutionTx(ctx context.Context, tx *sql.Tx, e *Execution, agentID, tool string) error {
	if c.fail {
		return errors.New("outbox unavailable")
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM inbox_executions WHERE grant_id=?`, e.GrantID).Scan(&state); err != nil {
		return err
	}
	c.runs = append(c.runs, agentID+"|"+tool+"|"+e.CallID+"|"+e.State+"|"+state)
	return nil
}

// A granted call's outcome reaches the callbacks hook inside the
// transaction that records it; a failed hook records nothing.
func TestFinishExecutionAnnouncesTheOutcome(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cb := &execCallbacks{}
	e.svc.SetCallbacks(cb)
	r := submitBatch(t, e)
	if _, err := e.svc.Decide(ctx, r.ID, batchDecision(r, "approved", VerdictAccepted, VerdictRejected)); err != nil {
		t.Fatal(err)
	}
	views, _ := e.svc.Status(ctx, "ag_1", []string{r.ID})
	g, err := e.svc.Redeem(ctx, views[0].Tools[0].Grant, "ag_1", "db.migrate", map[string]any{"env": "stage", "migration": "0042"})
	if err != nil {
		t.Fatal(err)
	}
	cb.fail = true
	if err := e.svc.FinishExecution(ctx, g, ExecutionUnknown, nil, "transport failed after dispatch"); err == nil {
		t.Fatal("an outcome committed without its event")
	}
	if ex, _ := e.svc.Execution(ctx, g.ID, "ag_1"); ex.State != ExecutionRunning {
		t.Fatalf("a failed event left the execution %s", ex.State)
	}
	cb.fail = false
	if err := e.svc.FinishExecution(ctx, g, ExecutionUnknown, nil, "transport failed after dispatch"); err != nil {
		t.Fatal(err)
	}
	if len(cb.runs) != 1 || cb.runs[0] != "ag_1|db.migrate|file_alpha|outcome_unknown|outcome_unknown" {
		t.Fatalf("execution events: %v", cb.runs)
	}
}

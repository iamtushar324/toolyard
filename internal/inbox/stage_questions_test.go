package inbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStageExpiredDecisionRefusedBeforeSweep(t *testing.T) {
	e := newEnv(t)
	r := submitDeploy(t, e, "ag_stage")
	e.advance(RequestTTL + time.Second)
	if _, err := e.svc.Decide(context.Background(), r.ID, Decision{Action: "approve", Allow: []bool{true, true, false}}); err == nil {
		t.Fatal("expired request issued grants before the sweeper ran")
	}
}

func TestStageStructuredQuestionRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var s Submission
	err := json.Unmarshal([]byte(`{"schema_version":2,"client_request_id":"stage-q1","kind":"question","prompt":"Which checks must run?","context":"Choose any checks or write your own answer.","question":{"type":"multiple_choice","min_selections":1,"max_selections":2,"options":[{"id":"mobile","label":"Mobile"},{"id":"keys","label":"Keyboard"},{"id":"none","label":"None","exclusive":true}]}}`), &s)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := e.svc.Submit(ctx, "ag_stage", &s)
	if err != nil || !sub.OK {
		t.Fatalf("submit: %+v %v", sub, err)
	}
	again, err := e.svc.Submit(ctx, "ag_stage", &s)
	if err != nil || again.RequestID != sub.RequestID {
		t.Fatalf("submission retry duplicated request: %+v %v", again, err)
	}
	for _, bad := range []string{`{"selected_option_ids":["unknown"]}`, `{"selected_option_ids":["mobile","mobile"]}`, `{"selected_option_ids":["none","mobile"]}`, `{"text":"   "}`} {
		var d Decision
		_ = json.Unmarshal([]byte(`{"action":"answer","request_revision":1,"submission_id":"attempt","response":`+bad+`}`), &d)
		if _, err = e.svc.Decide(ctx, sub.RequestID, d); err == nil {
			t.Fatalf("accepted invalid answer %s", bad)
		}
	}
	var d Decision
	_ = json.Unmarshal([]byte(`{"action":"answer","request_revision":1,"submission_id":"attempt","response":{"selected_option_ids":["mobile","keys"],"text":"  Keep this exact text.\n"}}`), &d)
	r, err := e.svc.Decide(ctx, sub.RequestID, d)
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.Activity)
	r, err = e.svc.Decide(ctx, sub.RequestID, d)
	if err != nil || len(r.Activity) != before {
		t.Fatalf("answer retry failed: %v", err)
	}
	v, err := e.svc.Status(ctx, "ag_stage", []string{sub.RequestID})
	b, _ := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"text":"  Keep this exact text.\n"`) || !strings.Contains(v[0].Answer, "Keyboard") {
		t.Fatalf("agent lost answer: %s %v", b, err)
	}
	d.By = "another person"
	if _, err = e.svc.Decide(ctx, sub.RequestID, d); err == nil {
		t.Fatal("different actor replayed answer")
	}
}

func TestStageFreeTextOnlyQuestion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var s Submission
	_ = json.Unmarshal([]byte(`{"schema_version":2,"kind":"question","prompt":"What would improve this page?","question":{"type":"free_text"}}`), &s)
	r, err := e.svc.Submit(ctx, "ag_stage", &s)
	if err != nil || !r.OK {
		t.Fatalf("submit: %+v %v", r, err)
	}
	var d Decision
	_ = json.Unmarshal([]byte(`{"action":"answer","request_revision":1,"submission_id":"text1","response":{"text":"Make the list easier to scan."}}`), &d)
	got, err := e.svc.Decide(ctx, r.RequestID, d)
	if err != nil || got.Answer != "Make the list easier to scan." {
		t.Fatalf("answer: %+v %v", got, err)
	}
}

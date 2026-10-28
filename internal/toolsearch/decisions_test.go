package toolsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// seenRequest is a Decisions request as the fake API decodes it.
type seenRequest struct {
	Auth  string `json:"-"`
	Model string `json:"model"`
	State struct {
		Task  string `json:"task"`
		Tools []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Server      string   `json:"server"`
			Description string   `json:"description"`
			Parameters  []string `json:"parameters"`
		} `json:"tools"`
		Servers []struct {
			Name     string   `json:"name"`
			Tools    int      `json:"tools"`
			Examples []string `json:"examples"`
		} `json:"servers"`
	} `json:"state"`
	Questions map[string]struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria"`
	} `json:"questions"`
	Provider *struct {
		ZDR bool `json:"zdr"`
	} `json:"provider"`
	raw map[string]any
}

// fakeAPI is the Decisions API on an httptest server: it records each
// request and answers with reply.
type fakeAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	seen  []seenRequest
	reply func(n int, req seenRequest) (int, string)
}

func newFakeAPI(t *testing.T, reply func(n int, req seenRequest) (int, string)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req seenRequest
		if r.Method != http.MethodPost || json.Unmarshal(body, &req) != nil || json.Unmarshal(body, &req.raw) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Auth = r.Header.Get("Authorization")
		f.mu.Lock()
		f.seen = append(f.seen, req)
		n := len(f.seen)
		f.mu.Unlock()
		status, out := f.reply(n, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// answersFor answers every question of req: each tool's noul is p(name);
// the server choice is server (or any offered one) with probability sp.
// edit, when set, changes the answers before they are sent.
func answersFor(req seenRequest, p func(string) float64, server string, sp float64, edit func(map[string]any)) string {
	answers := map[string]any{}
	for _, tl := range req.State.Tools {
		answers[tl.ID] = map[string]any{"type": "noul", "noul": p(tl.Name)}
	}
	if q, ok := req.Questions[serverQuestion]; ok {
		choice := server
		if _, offered := q.Criteria[choice]; !offered {
			for k := range q.Criteria {
				choice = k
				break
			}
		}
		answers[serverQuestion] = map[string]any{"type": "choice", "choice": choice, "confidence": sp, "probabilities": map[string]any{choice: sp}}
	}
	if edit != nil {
		edit(answers)
	}
	b, _ := json.Marshal(map[string]any{"id": "dec_1", "model": req.Model, "provider": "test", "answers": answers,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "cost": 0.0002}})
	return string(b)
}

func judge(p func(string) float64, server string, sp float64) func(int, seenRequest) (int, string) {
	return func(_ int, req seenRequest) (int, string) {
		return http.StatusOK, answersFor(req, p, server, sp, nil)
	}
}

// only says yes (p) to one tool and a clear no to every other.
func only(name string, p float64) func(string) float64 {
	return func(n string) float64 {
		if n == name {
			return p
		}
		return 0.05
	}
}

// observed records what a ranker's Observe hears.
type observed struct {
	mu       sync.Mutex
	outcomes []string
	cost     float64
}

func (o *observed) observe(outcome string, cost float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, outcome)
	o.cost += cost
}

func (o *observed) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.outcomes...)
}

const testKey = "sk-test-key-123"

func newTestRanker(api *fakeAPI, o *observed, edit ...func(*DecisionsOptions)) *DecisionsRanker {
	opts := DecisionsOptions{
		Endpoint: api.srv.URL,
		Key:      func(context.Context) (string, error) { return testKey, nil },
		Client:   api.srv.Client(),
		Observe:  o.observe,
	}
	for _, e := range edit {
		e(&opts)
	}
	return NewDecisionsRanker(opts)
}

// TestDecisionsRankerRanksByProbability: one request with a noul per
// keyword candidate and a choice among servers; Jev's probabilities order
// the result, every other keyword match follows unscored; the request
// carries the key in its header only and nothing but names, servers, cut
// descriptions and parameter names.
func TestDecisionsRankerRanksByProbability(t *testing.T) {
	const query = "send a message to a slack channel"
	api := newFakeAPI(t, judge(only("HeyReach.send_message", 0.92), "HeyReach", 0.8))
	o := &observed{}
	e := New(Options{Semantic: newTestRanker(api, o, func(op *DecisionsOptions) { op.ZDR = true })})
	tools := fixture()
	for i := range tools {
		if tools[i].Name == "Slack.send_message" {
			tools[i].Description += " " + strings.Repeat("A long description of what the tool does. ", 20)
		}
	}
	kw := search(t, New(Options{}), tools, Query{Text: query, Mode: ModeKeyword, Limit: MaxLimit})
	res := search(t, e, tools, Query{Text: query, Limit: MaxLimit})
	if res.Mode != ModeSemantic || res.RankedBy != RankedJev || res.Fallback != "" {
		t.Fatalf("result %s %s %q", res.Mode, res.RankedBy, res.Fallback)
	}
	if res.Hits[0].Tool.Name != "HeyReach.send_message" || res.Hits[0].Score != 0.92 {
		t.Fatalf("first hit %s %v; keyword had %v", res.Hits[0].Tool.Name, res.Hits[0].Score, names(kw.Hits))
	}
	if kw.Hits[0].Tool.Name == "HeyReach.send_message" {
		t.Fatal("the fixture should make keyword rank another tool first")
	}
	// Every keyword match is still there; the unjudged ones carry no score.
	judged := min(len(kw.Hits), decisionsCandidates)
	if res.Matched != kw.Matched {
		t.Fatalf("matched %d, keyword matched %d", res.Matched, kw.Matched)
	}
	got := names(res.Hits)
	want := names(kw.Hits)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the result lost keyword matches:\n%v\n%v", got, want)
	}
	for i := judged; i < len(res.Hits); i++ {
		if res.Hits[i].Score != 0 || res.Hits[i].Tool.Name != kw.Hits[i].Tool.Name {
			t.Fatalf("unjudged hit %d: %s %v, want %s unscored", i, res.Hits[i].Tool.Name, res.Hits[i].Score, kw.Hits[i].Tool.Name)
		}
	}

	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1 (a candidate was likely)", len(reqs))
	}
	req := reqs[0]
	if req.Auth != "Bearer "+testKey || req.Model != DefaultDecisionsModel || req.Provider == nil || !req.Provider.ZDR || req.State.Task != query {
		t.Fatalf("request auth %q model %q provider %+v task %q", req.Auth, req.Model, req.Provider, req.State.Task)
	}
	if b, _ := json.Marshal(req.raw); strings.Contains(string(b), testKey) {
		t.Fatal("the key is in the request body")
	}
	if len(req.State.Tools) != judged {
		t.Fatalf("%d candidates, want %d", len(req.State.Tools), judged)
	}
	cut := false
	for i, tl := range req.State.Tools {
		cut = cut || strings.HasSuffix(tl.Description, "…")
		if tl.ID != fmt.Sprintf("t%d", i+1) || tl.Name != kw.Hits[i].Tool.Name || tl.Server != kw.Hits[i].Tool.Server || len(tl.Description) > decisionsDescLen+len("…") {
			t.Fatalf("candidate %d: %+v", i, tl)
		}
		q := req.Questions[tl.ID]
		if q.Type != "noul" || q.Criteria["true"] == "" || q.Criteria["false"] == "" || !strings.Contains(q.Instructions, tl.ID) {
			t.Fatalf("question %s: %+v", tl.ID, q)
		}
	}
	if !cut {
		t.Fatal("a long description was not cut")
	}
	allowed := map[string]bool{"id": true, "name": true, "server": true, "description": true, "parameters": true}
	for _, tl := range req.raw["state"].(map[string]any)["tools"].([]any) {
		for k := range tl.(map[string]any) {
			if !allowed[k] {
				t.Fatalf("a candidate carries %q", k)
			}
		}
	}
	for k := range req.raw["state"].(map[string]any) {
		if k != "task" && k != "tools" && k != "servers" {
			t.Fatalf("the state carries %q", k)
		}
	}
	sq := req.Questions[serverQuestion]
	if sq.Type != "choice" || len(sq.Criteria) != len(req.State.Servers) || len(sq.Criteria) < 2 {
		t.Fatalf("server question %+v for %d servers", sq, len(req.State.Servers))
	}
	for _, s := range req.State.Servers {
		if sq.Criteria[s.Name] == "" || s.Tools == 0 || len(s.Examples) == 0 || len(s.Examples) > decisionsServerExamples {
			t.Fatalf("server %+v", s)
		}
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{"ok"}) || math.Abs(o.cost-0.0002) > 1e-12 {
		t.Fatalf("observed %v cost %v", got, o.cost)
	}
}

// TestDecisionsRankerAsksTheChosenServerSecond: no keyword candidate is
// likely but a server is, so a second request asks about that server's
// other tools; an unlikely server asks nothing more.
func TestDecisionsRankerAsksTheChosenServerSecond(t *testing.T) {
	const query = "tell the team in chat that the deploy finished"
	tools := fixture()
	api := newFakeAPI(t, judge(only("Slack.send_message", 0.95), "Slack", 0.9))
	o := &observed{}
	res := search(t, New(Options{Semantic: newTestRanker(api, o)}), tools, Query{Text: query})
	if res.RankedBy != RankedJev || len(res.Hits) == 0 || res.Hits[0].Tool.Name != "Slack.send_message" || res.Hits[0].Score != 0.95 {
		t.Fatalf("result %s %v", res.RankedBy, names(res.Hits))
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	first := map[string]bool{}
	for _, tl := range reqs[0].State.Tools {
		first[tl.Name] = true
		if tl.Server == "Slack" {
			t.Fatalf("the fixture should give keyword no Slack candidate: %s", tl.Name)
		}
	}
	if len(first) == 0 {
		t.Fatal("the fixture should give keyword some candidates")
	}
	second := reqs[1]
	if _, ok := second.Questions[serverQuestion]; ok || len(second.State.Servers) != 0 || len(second.State.Tools) != 5 {
		t.Fatalf("second request: %d tools, servers %v", len(second.State.Tools), second.State.Servers)
	}
	for _, tl := range second.State.Tools {
		if tl.Server != "Slack" || first[tl.Name] {
			t.Fatalf("second request asks about %s", tl.Name)
		}
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{"ok", "ok"}) {
		t.Fatalf("observed %v", got)
	}

	unsure := newFakeAPI(t, judge(only("Slack.send_message", 0.95), "Slack", 0.3))
	search(t, New(Options{Semantic: newTestRanker(unsure, &observed{})}), tools, Query{Text: query})
	if n := len(unsure.requests()); n != 1 {
		t.Fatalf("an unlikely server: %d requests, want 1", n)
	}
}

// TestDecisionsRankerFallsBack: a timeout, an HTTP error and a missing key
// each rank by keyword and say why; nothing the provider sent reaches the
// result or the error.
func TestDecisionsRankerFallsBack(t *testing.T) {
	const query = "create a linear issue"
	tools := fixture()
	slow := newFakeAPI(t, func(int, seenRequest) (int, string) {
		time.Sleep(300 * time.Millisecond)
		return http.StatusOK, `{"answers":{}}`
	})
	o := &observed{}
	res := search(t, New(Options{Semantic: newTestRanker(slow, o, func(op *DecisionsOptions) { op.Timeout = 50 * time.Millisecond })}), tools, Query{Text: query})
	if res.Mode != ModeKeyword || res.RankedBy != RankedKeyword || res.Fallback != FallbackTimeout || res.Hits[0].Tool.Name != "Linear.create_issue" {
		t.Fatalf("timeout: %s %s %q %v", res.Mode, res.RankedBy, res.Fallback, names(res.Hits))
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{"timeout"}) {
		t.Fatalf("timeout observed %v", got)
	}

	const secretText = "provider-says-something-private"
	denied := newFakeAPI(t, func(int, seenRequest) (int, string) {
		return http.StatusUnauthorized, `{"error":{"code":401,"message":"` + secretText + `"}}`
	})
	o = &observed{}
	r := newTestRanker(denied, o)
	res = search(t, New(Options{Semantic: r}), tools, Query{Text: query})
	if res.Fallback != FallbackError || res.RankedBy != RankedKeyword || strings.Contains(fmt.Sprintf("%+v", res), secretText) {
		t.Fatalf("401: %q %+v", res.Fallback, res)
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{"http_401"}) {
		t.Fatalf("401 observed %v", got)
	}
	if _, err := r.Rank(context.Background(), tools, query); err == nil || strings.Contains(err.Error(), secretText) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("401 error: %v", err)
	}

	never := newFakeAPI(t, judge(only("x", 1), "", 1))
	for name, key := range map[string]func(context.Context) (string, error){
		"empty":    func(context.Context) (string, error) { return " ", nil },
		"ErrNoKey": func(context.Context) (string, error) { return "", ErrNoKey },
		"nil":      nil,
	} {
		o := &observed{}
		res := search(t, New(Options{Semantic: newTestRanker(never, o, func(op *DecisionsOptions) { op.Key = key })}), tools, Query{Text: query})
		if res.Fallback != FallbackNoKey || res.Hits[0].Tool.Name != "Linear.create_issue" {
			t.Errorf("key %s: %q %v", name, res.Fallback, names(res.Hits))
		}
		if got := o.list(); !reflect.DeepEqual(got, []string{FallbackNoKey}) {
			t.Errorf("key %s observed %v", name, got)
		}
	}
	o = &observed{}
	broken := func(context.Context) (string, error) { return "", errors.New("database is down: " + testKey) }
	res = search(t, New(Options{Semantic: newTestRanker(never, o, func(op *DecisionsOptions) { op.Key = broken })}), tools, Query{Text: query})
	if res.Fallback != FallbackError || !reflect.DeepEqual(o.list(), []string{"key_error"}) {
		t.Fatalf("key error: %q %v", res.Fallback, o.list())
	}
	if n := len(never.requests()); n != 0 {
		t.Fatalf("%d requests without a key", n)
	}
}

// TestDecisionsRankerBudgetCoversTheKeyRead (review r1-4): a slow secret
// store counts against the budget, so the search falls back with timeout
// instead of waiting for the key.
func TestDecisionsRankerBudgetCoversTheKeyRead(t *testing.T) {
	never := newFakeAPI(t, judge(only("x", 1), "", 1))
	o := &observed{}
	slowKey := func(ctx context.Context) (string, error) {
		select {
		case <-time.After(2 * time.Second):
			return testKey, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	start := time.Now()
	res := search(t, New(Options{Semantic: newTestRanker(never, o, func(op *DecisionsOptions) {
		op.Key, op.Timeout = slowKey, 50*time.Millisecond
	})}), fixture(), Query{Text: "create a linear issue"})
	if res.Fallback != FallbackTimeout || res.RankedBy != RankedKeyword || time.Since(start) > time.Second {
		t.Fatalf("slow key: %q %s after %s", res.Fallback, res.RankedBy, time.Since(start))
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{FallbackTimeout}) || len(never.requests()) != 0 {
		t.Fatalf("slow key observed %v, %d requests", got, len(never.requests()))
	}
}

// TestDecisionsRankerKeepsTheFirstRoundWhenTheSecondFails (review r1-3):
// the first request already ranked every keyword candidate, so a failed
// second request (the chosen server's other tools) keeps that ranking
// (ranked_by jev) rather than throwing it away; it is not cached, so the
// next search asks again.
func TestDecisionsRankerKeepsTheFirstRoundWhenTheSecondFails(t *testing.T) {
	const query = "tell the team in chat that the deploy finished"
	first := judge(func(string) float64 { return 0.2 }, "Slack", 0.9)
	api := newFakeAPI(t, func(n int, req seenRequest) (int, string) {
		if n%2 == 1 { // each search's first request
			return first(n, req)
		}
		return http.StatusBadGateway, `{"error":{"code":502,"message":"upstream"}}`
	})
	o := &observed{}
	e := New(Options{Semantic: newTestRanker(api, o)})
	res := search(t, e, fixture(), Query{Text: query})
	if res.RankedBy != RankedJev || res.Fallback != "" || len(res.Hits) == 0 || res.Hits[0].Score != 0.2 {
		t.Fatalf("second round failed: %s %q %v", res.RankedBy, res.Fallback, names(res.Hits))
	}
	if got := o.list(); !reflect.DeepEqual(got, []string{"ok", "http_502"}) {
		t.Fatalf("observed %v", got)
	}
	search(t, e, fixture(), Query{Text: query})
	if n := len(api.requests()); n != 4 {
		t.Fatalf("%d requests: an incomplete ranking must not be cached", n)
	}
}

// TestDecisionsRankerRefusesMalformedAnswers: an answer that misses a
// question, has a probability outside [0, 1], the wrong type, a server
// not offered, is not JSON or is too large fails the ranking.
func TestDecisionsRankerRefusesMalformedAnswers(t *testing.T) {
	tools := fixture()
	ok := only("Linear.create_issue", 0.9)
	edits := map[string]func(map[string]any){
		"missing id": func(a map[string]any) { delete(a, "t1") },
		"noul > 1":   func(a map[string]any) { a["t1"] = map[string]any{"type": "noul", "noul": 1.5} },
		"noul < 0":   func(a map[string]any) { a["t2"] = map[string]any{"type": "noul", "noul": -0.1} },
		"no noul":    func(a map[string]any) { a["t1"] = map[string]any{"type": "noul"} },
		"wrong type": func(a map[string]any) { a["t1"] = map[string]any{"type": "choice", "choice": "true"} },
		"server not offered": func(a map[string]any) {
			a[serverQuestion] = map[string]any{"type": "choice", "choice": "Nowhere", "confidence": 0.9, "probabilities": map[string]any{"Nowhere": 0.9}}
		},
		"server probability > 1": func(a map[string]any) {
			a[serverQuestion] = map[string]any{"type": "choice", "choice": "Linear", "probabilities": map[string]any{"Linear": 2}}
		},
		"server without probability": func(a map[string]any) {
			a[serverQuestion] = map[string]any{"type": "choice", "choice": "Linear"}
		},
		"no server answer": func(a map[string]any) { delete(a, serverQuestion) },
	}
	for name, edit := range edits {
		api := newFakeAPI(t, func(_ int, req seenRequest) (int, string) {
			return http.StatusOK, answersFor(req, ok, "Linear", 0.9, edit)
		})
		o := &observed{}
		res := search(t, New(Options{Semantic: newTestRanker(api, o)}), tools, Query{Text: "create a linear issue"})
		if res.Fallback != FallbackError || res.RankedBy != RankedKeyword || !reflect.DeepEqual(o.list(), []string{"bad_response"}) {
			t.Errorf("%s: %q %s %v", name, res.Fallback, res.RankedBy, o.list())
		}
	}
	for name, body := range map[string]string{
		"not JSON":  "<html>busy</html>",
		"too large": `{"answers":{},"pad":"` + strings.Repeat("x", decisionsMaxBody) + `"}`,
	} {
		api := newFakeAPI(t, func(int, seenRequest) (int, string) { return http.StatusOK, body })
		o := &observed{}
		res := search(t, New(Options{Semantic: newTestRanker(api, o)}), tools, Query{Text: "create a linear issue"})
		if res.Fallback != FallbackError || !reflect.DeepEqual(o.list(), []string{"bad_response"}) {
			t.Errorf("%s: %q %v", name, res.Fallback, o.list())
		}
	}
}

// TestDecisionsRankerCachesAnswers: the same query over the same catalog
// is answered from the cache for 10 minutes and still reads as Jev's; a
// changed catalog, an expired entry or an incomplete answer asks again.
func TestDecisionsRankerCachesAnswers(t *testing.T) {
	const query = "create a linear issue"
	tools := fixture()
	api := newFakeAPI(t, judge(only("Linear.create_issue", 0.9), "Linear", 0.9))
	e := New(Options{Semantic: newTestRanker(api, &observed{})})
	now := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return now }
	first := search(t, e, tools, Query{Text: query})
	second := search(t, e, fixture(), Query{Text: query})
	if n := len(api.requests()); n != 1 {
		t.Fatalf("%d requests for the same query and catalog, want 1", n)
	}
	if second.RankedBy != RankedJev || second.Fallback != "" || !reflect.DeepEqual(names(first.Hits), names(second.Hits)) || second.Hits[0].Score != 0.9 {
		t.Fatalf("cache hit: %s %q %v", second.RankedBy, second.Fallback, names(second.Hits))
	}
	search(t, e, tools, Query{Text: query + " now"})
	if n := len(api.requests()); n != 2 {
		t.Fatalf("another query: %d requests, want 2", n)
	}
	changed := fixture()
	changed[0].Description = "Something else entirely."
	search(t, e, changed, Query{Text: query})
	if n := len(api.requests()); n != 3 {
		t.Fatalf("a changed catalog: %d requests, want 3", n)
	}
	now = now.Add(rankCacheTTL)
	search(t, e, tools, Query{Text: query})
	if n := len(api.requests()); n != 4 {
		t.Fatalf("an expired entry: %d requests, want 4", n)
	}

	// The second round fails: the first round's scores rank this search,
	// and are not cached.
	flaky := newFakeAPI(t, func(n int, req seenRequest) (int, string) {
		if n%2 == 0 {
			return http.StatusInternalServerError, `{"error":{"code":500,"message":"x"}}`
		}
		return http.StatusOK, answersFor(req, only("Slack.send_message", 0.95), "Slack", 0.9, nil)
	})
	o := &observed{}
	e = New(Options{Semantic: newTestRanker(flaky, o)})
	res := search(t, e, tools, Query{Text: "tell the team in chat that the deploy finished"})
	if res.RankedBy != RankedJev || !reflect.DeepEqual(o.list(), []string{"ok", "http_500"}) {
		t.Fatalf("a failed second round: %s %v", res.RankedBy, o.list())
	}
	search(t, e, tools, Query{Text: "tell the team in chat that the deploy finished"})
	if n := len(flaky.requests()); n != 4 {
		t.Fatalf("an incomplete answer was cached: %d requests, want 4", n)
	}

	// The cache stays bounded.
	for i := 0; i < rankCacheSize+10; i++ {
		e.storeScores(fmt.Sprintf("k%d", i), map[string]float64{})
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ranks) > rankCacheSize || len(e.rankOrder) > rankCacheSize {
		t.Fatalf("cache holds %d entries and %d references", len(e.ranks), len(e.rankOrder))
	}
}

// TestDecisionsRankerCapsServerChoices: at most 25 servers are offered,
// the best summed keyword scores first, and at most 30 tools asked about.
func TestDecisionsRankerCapsServerChoices(t *testing.T) {
	var tools []Tool
	for i := 0; i < 40; i++ {
		srv := fmt.Sprintf("srv%02d", i)
		desc := "Do a thing."
		if i == 39 {
			desc = "Do a thing, the thing, every thing."
		}
		tools = append(tools, Tool{Name: srv + ".do_thing", Server: srv, Description: desc})
	}
	api := newFakeAPI(t, judge(only("srv39.do_thing", 0.9), "srv39", 0.9))
	res := search(t, New(Options{Semantic: newTestRanker(api, &observed{})}), tools, Query{Text: "thing"})
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	req := reqs[0]
	if len(req.State.Tools) != decisionsCandidates || len(req.State.Servers) != decisionsServerChoices || len(req.Questions[serverQuestion].Criteria) != decisionsServerChoices {
		t.Fatalf("%d tools, %d servers, %d choices", len(req.State.Tools), len(req.State.Servers), len(req.Questions[serverQuestion].Criteria))
	}
	if req.State.Servers[0].Name != "srv39" || req.State.Servers[0].Examples[0] != "do_thing" {
		t.Fatalf("the best keyword server comes first: %+v", req.State.Servers[0])
	}
	if res.Hits[0].Tool.Name != "srv39.do_thing" || len(res.Hits) != 40 {
		t.Fatalf("result %v", names(res.Hits))
	}
}

// TestDecisionsRankerRank: the SemanticProvider method ranks alone.
func TestDecisionsRankerRank(t *testing.T) {
	api := newFakeAPI(t, judge(only("GitHub.create_issue", 0.8), "GitHub", 0.9))
	hits, err := newTestRanker(api, &observed{}).Rank(context.Background(), fixture(), "create an issue")
	if err != nil || len(hits) == 0 || hits[0].Tool.Name != "GitHub.create_issue" || hits[0].Score != 0.8 {
		t.Fatalf("Rank: %v %v", err, names(hits))
	}
}

// TestCachedKey: a resolved key, set or not, is reused for the ttl; an
// error is not cached.
func TestCachedKey(t *testing.T) {
	calls := 0
	val, fail := "", false
	resolve := func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("down")
		}
		return val, nil
	}
	now := time.Unix(0, 0)
	key := cachedKey(resolve, time.Minute, func() time.Time { return now })
	ctx := context.Background()
	if v, err := key(ctx); v != "" || err != nil || calls != 1 {
		t.Fatalf("first: %q %v %d", v, err, calls)
	}
	val = "sk-new"
	if v, _ := key(ctx); v != "" || calls != 1 {
		t.Fatalf("within the ttl: %q %d", v, calls)
	}
	now = now.Add(time.Minute)
	if v, _ := key(ctx); v != "sk-new" || calls != 2 {
		t.Fatalf("after the ttl: %q %d", v, calls)
	}
	now = now.Add(time.Minute)
	fail = true
	if _, err := key(ctx); err == nil {
		t.Fatal("an error should pass through")
	}
	fail = false
	if v, _ := key(ctx); v != "sk-new" || calls != 4 {
		t.Fatalf("an error is not cached: %q %d", v, calls)
	}
}

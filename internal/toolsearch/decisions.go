package toolsearch

// DecisionsRanker: a SemanticProvider that asks a classification model,
// through the OpenRouter Decisions API, which of the caller's tools do what
// a task asks. Jev (typesafe/jev-1.13) is the default model. One search
// makes one request, sometimes two:
//
//  1. keyword search picks up to 30 candidates; the request asks one yes/no
//     question (noul) per candidate and, when the caller's tools span
//     several servers, one choice among up to 25 of them (the 25 with the
//     best summed keyword score);
//  2. when the chosen server is likely (p >= 0.5), no candidate is (every
//     noul < 0.5) and that server has tools that were not candidates, a
//     second request asks about up to 30 of them, if at least 400 ms of the
//     search's budget is left.
//
// The model sees the task text and, per tool, its name, server, its
// description cut to 300 bytes and its parameter names; per server, its
// name, tool count and a few tool names. Never arguments, results or
// secrets. Answers are validated strictly. An error, a timeout or a missing
// key fails the ranking, and the engine falls back to keyword; nothing the
// provider says reaches a result or a log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDecisionsEndpoint is the OpenRouter Decisions API.
	DefaultDecisionsEndpoint = "https://openrouter.ai/api/alpha/decisions"
	// DefaultDecisionsModel is Jev.
	DefaultDecisionsModel = "typesafe/jev-1.13"
	// DefaultDecisionsTimeout is one search's whole budget, both requests
	// included.
	DefaultDecisionsTimeout = 1500 * time.Millisecond

	// decisionsCandidates caps the tools one request asks about.
	decisionsCandidates = 30
	// decisionsServerChoices caps the servers the choice offers.
	decisionsServerChoices = 25
	// decisionsServerExamples is how many tool names a server shows.
	decisionsServerExamples = 4
	// decisionsMaxParams caps the parameter names a tool shows.
	decisionsMaxParams = 20
	// decisionsDescLen caps the description a tool shows, in bytes.
	decisionsDescLen = 300
	// decisionsSecondRoundMin is the budget a second request needs left.
	decisionsSecondRoundMin = 400 * time.Millisecond
	// decisionsMaxBody caps an answer, in bytes.
	decisionsMaxBody = 1 << 20
	// decisionsLikely is the probability from which an answer counts as
	// yes.
	decisionsLikely = 0.5
	// serverQuestion is the id of the choice among servers.
	serverQuestion = "server"
)

// ErrNoKey is a ranker with no API key: the secret is not set, and the
// search falls back to keyword.
var ErrNoKey = errors.New("toolsearch: the ranker has no API key")

// rankError is a failed request. outcome is a fixed word ("timeout",
// "http_<status>", "bad_response", "key_error", "error") and msg is ours:
// neither carries anything the provider sent.
type rankError struct {
	outcome string
	msg     string
}

func (e *rankError) Error() string { return "toolsearch: decisions: " + e.msg }

// DecisionsOptions configure a DecisionsRanker.
type DecisionsOptions struct {
	// Endpoint is the Decisions API URL; "" is DefaultDecisionsEndpoint.
	Endpoint string
	// Model is the model to ask; "" is DefaultDecisionsModel.
	Model string
	// Key returns the API key at search time. "" or ErrNoKey means none is
	// set; a nil Key is never set.
	Key func(context.Context) (string, error)
	// Timeout is one search's whole budget; 0 is DefaultDecisionsTimeout.
	Timeout time.Duration
	// ZDR asks OpenRouter to route only to providers that keep no data
	// (provider.zdr).
	ZDR bool
	// Client sends the requests; nil is a plain http.Client (the budget
	// is the context's deadline).
	Client *http.Client
	// Observe, when set, hears every attempt's outcome ("ok", "no_key",
	// "key_error", "timeout", "http_<status>", "bad_response" or "error")
	// and the cost the API reported for it (0 when none). It must be cheap
	// and never block.
	Observe func(outcome string, cost float64)
}

// DecisionsRanker ranks tools with the OpenRouter Decisions API. It is safe
// for concurrent use.
type DecisionsRanker struct {
	endpoint string
	model    string
	key      func(context.Context) (string, error)
	timeout  time.Duration
	zdr      bool
	client   *http.Client
	observe  func(string, float64)
}

// NewDecisionsRanker returns a DecisionsRanker.
func NewDecisionsRanker(opts DecisionsOptions) *DecisionsRanker {
	r := &DecisionsRanker{endpoint: opts.Endpoint, model: opts.Model, key: opts.Key, timeout: opts.Timeout,
		zdr: opts.ZDR, client: opts.Client, observe: opts.Observe}
	if r.endpoint == "" {
		r.endpoint = DefaultDecisionsEndpoint
	}
	if r.model == "" {
		r.model = DefaultDecisionsModel
	}
	if r.timeout <= 0 {
		r.timeout = DefaultDecisionsTimeout
	}
	if r.client == nil {
		r.client = &http.Client{}
	}
	return r
}

// Rank implements SemanticProvider: the tools the model judged, most
// likely first, then every other keyword match. An Engine calls score
// instead, with its cached keyword index.
func (r *DecisionsRanker) Rank(ctx context.Context, tools []Tool, query string) ([]Hit, error) {
	keyword := buildIndex(tools).search(query)
	scores, err := r.score(ctx, tools, query, keyword)
	if err != nil {
		return nil, err
	}
	return merge(tools, scores, keyword), nil
}

// score asks the model about the keyword candidates (and the servers),
// then, when that found nothing likely, about the likely server's other
// tools. Every round it asks must answer: a second round that fails or has
// no time left fails the ranking, and the search falls back to keyword
// ranking (fallback timeout or error), so ranked_by jev always means Jev
// judged every tool it set out to judge.
func (r *DecisionsRanker) score(ctx context.Context, tools []Tool, query string, keyword []Hit) (map[string]float64, error) {
	// The budget covers reading the key too: a slow secret store must not
	// hold the search past it.
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	key, err := r.apiKey(ctx)
	if err != nil {
		var re *rankError
		if errors.As(err, &re) {
			r.note(re.outcome, 0)
		} else {
			r.note(FallbackNoKey, 0)
		}
		return nil, err
	}

	cands := make([]Tool, 0, min(len(keyword), decisionsCandidates))
	for _, h := range keyword {
		if len(cands) == decisionsCandidates {
			break
		}
		cands = append(cands, h.Tool)
	}
	servers := summarizeServers(tools, keyword)
	var choices []serverSummary
	chosen, chosenP := "", 0.0
	switch {
	case len(servers) == 1:
		chosen, chosenP = servers[0].name, 1
	case len(servers) > 1:
		choices = servers[:min(len(servers), decisionsServerChoices)]
	}
	scores := map[string]float64{}
	if len(cands) > 0 || len(choices) > 0 {
		ans, err := r.ask(ctx, key, query, cands, choices)
		if err != nil {
			return nil, err
		}
		for i, t := range cands {
			scores[t.Name] = ans.nouls[i]
		}
		if len(choices) > 0 {
			chosen, chosenP = ans.choice, ans.choiceP
		}
	}
	best := 0.0
	for _, p := range scores {
		best = math.Max(best, p)
	}
	if chosen == "" || chosenP < decisionsLikely || best >= decisionsLikely {
		return scores, nil
	}
	more := serverTools(tools, keyword, chosen, scores)
	if len(more) == 0 {
		return scores, nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < decisionsSecondRoundMin {
		r.note(FallbackTimeout, 0)
		return nil, &rankError{outcome: FallbackTimeout, msg: "no time left for the server's tools"}
	}
	ans, err := r.ask(ctx, key, query, more, nil)
	if err != nil {
		return nil, err
	}
	for i, t := range more {
		scores[t.Name] = ans.nouls[i]
	}
	return scores, nil
}

func (r *DecisionsRanker) note(outcome string, cost float64) {
	if r.observe != nil {
		r.observe(outcome, cost)
	}
}

// apiKey is the key to send, ErrNoKey when none is set.
func (r *DecisionsRanker) apiKey(ctx context.Context) (string, error) {
	if r.key == nil {
		return "", ErrNoKey
	}
	k, err := r.key(ctx)
	switch {
	case errors.Is(err, ErrNoKey):
		return "", ErrNoKey
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)):
		return "", &rankError{outcome: FallbackTimeout, msg: "the API key was not read within the budget"}
	case err != nil:
		return "", &rankError{outcome: "key_error", msg: "the API key could not be read"}
	}
	if k = strings.TrimSpace(k); k == "" {
		return "", ErrNoKey
	}
	return k, nil
}

// serverSummary is one server as the model sees it.
type serverSummary struct {
	name     string
	desc     string // the server's description, "" when none
	count    int
	score    float64 // summed keyword score of its tools
	examples []string
}

// summarizeServers is every server of tools, the best summed keyword score
// first (then the most tools, then by name), each with up to
// decisionsServerExamples tool names: its best keyword matches, then by
// name.
func summarizeServers(tools []Tool, keyword []Hit) []serverSummary {
	idx := map[string]int{}
	var list []serverSummary
	names := map[string][]string{}
	for _, t := range tools {
		i, ok := idx[t.Server]
		if !ok {
			i = len(list)
			idx[t.Server] = i
			list = append(list, serverSummary{name: t.Server})
		}
		if list[i].desc == "" {
			list[i].desc = t.ServerDescription
		}
		list[i].count++
		names[t.Server] = append(names[t.Server], t.Name)
	}
	shown := map[string]bool{}
	for _, h := range keyword {
		i, ok := idx[h.Tool.Server]
		if !ok {
			continue
		}
		list[i].score += h.Score
		if len(list[i].examples) < decisionsServerExamples {
			list[i].examples = append(list[i].examples, toolShortName(h.Tool.Name))
			shown[h.Tool.Name] = true
		}
	}
	for i := range list {
		if len(list[i].examples) >= decisionsServerExamples {
			continue
		}
		ns := names[list[i].name]
		sort.Strings(ns)
		for _, n := range ns {
			if len(list[i].examples) == decisionsServerExamples {
				break
			}
			if !shown[n] {
				list[i].examples = append(list[i].examples, toolShortName(n))
			}
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		if list[a].score != list[b].score {
			return list[a].score > list[b].score
		}
		if list[a].count != list[b].count {
			return list[a].count > list[b].count
		}
		return list[a].name < list[b].name
	})
	return list
}

// serverTools is up to decisionsCandidates tools of server that have no
// score yet: its keyword matches in keyword order, then the rest by name.
func serverTools(tools []Tool, keyword []Hit, server string, scored map[string]float64) []Tool {
	var out []Tool
	taken := map[string]bool{}
	for _, h := range keyword {
		if len(out) == decisionsCandidates {
			return out
		}
		if _, ok := scored[h.Tool.Name]; ok || h.Tool.Server != server {
			continue
		}
		out = append(out, h.Tool)
		taken[h.Tool.Name] = true
	}
	var rest []Tool
	for _, t := range tools {
		if _, ok := scored[t.Name]; ok || t.Server != server || taken[t.Name] {
			continue
		}
		rest = append(rest, t)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	for _, t := range rest {
		if len(out) == decisionsCandidates {
			break
		}
		out = append(out, t)
	}
	return out
}

// toolShortName is a catalog name without its server prefix.
func toolShortName(name string) string {
	if s := shortName(name); s != "" {
		return s
	}
	return name
}

// paramNames are a schema's top-level parameter names, sorted, at most
// decisionsMaxParams.
func paramNames(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for name := range props {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > decisionsMaxParams {
		out = out[:decisionsMaxParams]
	}
	return out
}

// The request and answer of the Decisions API.
type (
	decisionsRequest struct {
		Model     string                       `json:"model"`
		State     decisionsState               `json:"state"`
		Questions map[string]decisionsQuestion `json:"questions"`
		Provider  *decisionsProvider           `json:"provider,omitempty"`
	}
	decisionsProvider struct {
		ZDR bool `json:"zdr"`
	}
	decisionsState struct {
		Task    string            `json:"task"`
		Tools   []decisionsTool   `json:"tools,omitempty"`
		Servers []decisionsServer `json:"servers,omitempty"`
	}
	decisionsTool struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Server      string   `json:"server"`
		Description string   `json:"description,omitempty"`
		Parameters  []string `json:"parameters,omitempty"`
	}
	decisionsServer struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Tools       int      `json:"tools"`
		Examples    []string `json:"examples,omitempty"`
	}
	decisionsQuestion struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria"`
	}
	decisionsResponse struct {
		Answers map[string]decisionsAnswer `json:"answers"`
		Usage   struct {
			Cost *float64 `json:"cost"`
		} `json:"usage"`
	}
	decisionsAnswer struct {
		Type          string             `json:"type"`
		Noul          *float64           `json:"noul"`
		Choice        string             `json:"choice"`
		Confidence    *float64           `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	}
)

// toolQuestion asks whether the tool with id does what the task asks.
func toolQuestion(id string) decisionsQuestion {
	return decisionsQuestion{
		Type:         "noul",
		Instructions: "Tool " + id + " is listed under tools in the state. Does calling " + id + " do what the task asks?",
		Criteria: map[string]string{
			"true":  "Calling " + id + " performs the action the task asks for, or returns the information it needs, on the system the task means.",
			"false": id + " does something else, works on another system or kind of object, or only shares words with the task.",
		},
	}
}

// answers are one request's validated answers: a probability per tool
// asked about, in order, and the chosen server with its probability.
type answers struct {
	nouls   []float64
	choice  string
	choiceP float64
}

// ask sends one request: a noul per tool and, with choices, the server
// choice. It notes the outcome once.
func (r *DecisionsRanker) ask(ctx context.Context, key, task string, tools []Tool, choices []serverSummary) (*answers, error) {
	st := decisionsState{Task: task}
	questions := make(map[string]decisionsQuestion, len(tools)+1)
	ids := make([]string, len(tools))
	for i, t := range tools {
		id := "t" + strconv.Itoa(i+1)
		ids[i] = id
		st.Tools = append(st.Tools, decisionsTool{ID: id, Name: t.Name, Server: t.Server,
			Description: clip(t.Description, decisionsDescLen), Parameters: paramNames(t.InputSchema)})
		questions[id] = toolQuestion(id)
	}
	if len(choices) > 0 {
		criteria := make(map[string]string, len(choices))
		for _, s := range choices {
			st.Servers = append(st.Servers, decisionsServer{Name: s.name, Description: clip(s.desc, decisionsDescLen), Tools: s.count, Examples: s.examples})
			criteria[s.name] = "Choose " + s.name + " when the task is done with that server's tools."
		}
		questions[serverQuestion] = decisionsQuestion{
			Type:         "choice",
			Instructions: "The servers are listed under servers in the state. Which server's tools fit the task best?",
			Criteria:     criteria,
		}
	}
	body := decisionsRequest{Model: r.model, State: st, Questions: questions}
	if r.zdr {
		body.Provider = &decisionsProvider{ZDR: true}
	}
	resp, cost, err := r.post(ctx, key, body)
	if err != nil {
		var re *rankError
		if !errors.As(err, &re) {
			re = &rankError{outcome: "error", msg: "the request failed"}
			err = re
		}
		r.note(re.outcome, cost)
		return nil, err
	}
	out, err := validate(resp, ids, questions[serverQuestion].Criteria)
	if err != nil {
		r.note("bad_response", cost)
		return nil, err
	}
	r.note("ok", cost)
	return out, nil
}

// post sends body and decodes a 200 answer, bounded in size. The cost is
// the API's, 0 when it reported none.
func (r *DecisionsRanker) post(ctx context.Context, key string, body decisionsRequest) (*decisionsResponse, float64, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, &rankError{outcome: "error", msg: "the request could not be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, &rankError{outcome: "error", msg: "the request could not be built"}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, 0, transportError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, decisionsMaxBody+1))
	if err != nil {
		return nil, 0, transportError(ctx, err)
	}
	if res.StatusCode != http.StatusOK {
		// The body is the provider's words: never read into an error.
		return nil, 0, &rankError{outcome: "http_" + strconv.Itoa(res.StatusCode), msg: "HTTP " + strconv.Itoa(res.StatusCode)}
	}
	if len(data) > decisionsMaxBody {
		return nil, 0, &rankError{outcome: "bad_response", msg: "the answer is larger than 1 MiB"}
	}
	var out decisionsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, &rankError{outcome: "bad_response", msg: "the answer is not the expected JSON"}
	}
	cost := 0.0
	if c := out.Usage.Cost; c != nil && *c >= 0 && !math.IsInf(*c, 0) && !math.IsNaN(*c) {
		cost = *c
	}
	return &out, cost, nil
}

// transportError is a failed send or read: a timeout when the budget ran
// out or the network timed out, else an error.
func transportError(ctx context.Context, err error) error {
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &rankError{outcome: FallbackTimeout, msg: "no answer within the budget"}
	}
	return &rankError{outcome: "error", msg: "the request failed"}
}

// validate checks that resp answers every question asked: each tool id with
// a noul in [0, 1], and, when servers were offered (choices), a choice
// among them with its probability in [0, 1].
func validate(resp *decisionsResponse, ids []string, choices map[string]string) (*answers, error) {
	bad := func(msg string) error { return &rankError{outcome: "bad_response", msg: msg} }
	out := &answers{nouls: make([]float64, len(ids))}
	for i, id := range ids {
		a, ok := resp.Answers[id]
		if !ok {
			return nil, bad("no answer for " + id)
		}
		if a.Type != "noul" || a.Noul == nil || !unitInterval(*a.Noul) {
			return nil, bad("the answer for " + id + " is not a probability")
		}
		out.nouls[i] = *a.Noul
	}
	if len(choices) == 0 {
		return out, nil
	}
	a, ok := resp.Answers[serverQuestion]
	if !ok {
		return nil, bad("no answer for the server choice")
	}
	if _, offered := choices[a.Choice]; a.Type != "choice" || !offered {
		return nil, bad("the server choice names no server offered")
	}
	for _, p := range a.Probabilities {
		if !unitInterval(p) {
			return nil, bad("a server probability is outside [0, 1]")
		}
	}
	if a.Confidence != nil && !unitInterval(*a.Confidence) {
		return nil, bad("the server confidence is outside [0, 1]")
	}
	p, ok := a.Probabilities[a.Choice]
	if !ok {
		if a.Confidence == nil {
			return nil, bad("the server choice has no probability")
		}
		p = *a.Confidence
	}
	out.choice, out.choiceP = a.Choice, p
	return out, nil
}

func unitInterval(p float64) bool { return !math.IsNaN(p) && p >= 0 && p <= 1 }

// CachedKey returns a DecisionsOptions.Key that calls resolve at most once
// per ttl, so a key set (or removed) after startup is used within ttl
// without a secret read, and its audit trail, on every search. A key that
// is not set ("") is cached too; an error is not.
func CachedKey(resolve func(context.Context) (string, error), ttl time.Duration) func(context.Context) (string, error) {
	return cachedKey(resolve, ttl, time.Now)
}

func cachedKey(resolve func(context.Context) (string, error), ttl time.Duration, now func() time.Time) func(context.Context) (string, error) {
	var (
		mu   sync.Mutex
		val  string
		at   time.Time
		have bool
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		if have {
			if age := now().Sub(at); age >= 0 && age < ttl {
				v := val
				mu.Unlock()
				return v, nil
			}
		}
		mu.Unlock()
		v, err := resolve(ctx)
		if err != nil {
			return "", err
		}
		mu.Lock()
		val, at, have = v, now(), true
		mu.Unlock()
		return v, nil
	}
}

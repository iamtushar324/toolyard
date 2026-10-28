// Package toolsearch ranks a caller's tool catalog for a query. One Engine
// serves the MCP tool tools.search, the REST route GET /v1/code/search and
// code-mode scripts (tools.search in JavaScript, toolyard.tools_search in
// Starlark), so every surface ranks the same way:
//
//   - auto (the default): semantic when the engine has a SemanticProvider,
//     else keyword;
//   - semantic: delegated to the SemanticProvider (DecisionsRanker asks
//     Jev, a classification model, through the OpenRouter Decisions API),
//     refused with ErrSemanticOff where none is configured. When the
//     provider fails, times out or has no key, the search falls back to
//     keyword and says why (Result.Fallback);
//   - keyword: BM25F over the tool name (split at dots, underscores, dashes
//     and camelCase), its description, its parameter names and its
//     parameter descriptions, each field with its own weight;
//   - regex: an RE2 pattern of at most 200 characters, case-insensitive
//     unless the pattern says otherwise, over the same fields.
//
// The engine never decides what a caller may see: the caller passes the
// tools it may use (Gateway.CatalogFor's scope), and the engine ranks those.
// Keyword indexes and a provider's scores are cached by a hash of the tools
// they were built from, so a changed catalog builds a new index (and asks
// the provider again) and other nodes need no invalidation message (the
// caches are content-addressed).
package toolsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Tool is one tool as search sees it.
type Tool struct {
	// Name is the catalog name the gateway dispatches
	// ("BkCoreServices.get_client", "inbox.request").
	Name string
	// Server is the upstream or built-in group the tool belongs to.
	Server string
	// Description is the tool's own description, without gateway banners.
	Description string
	// InputSchema is the tool's input JSON Schema ("type", "properties",
	// "required"), without the gateway's injected fields.
	InputSchema map[string]any
	// OutputSchema is the tool's declared output schema, nil when none.
	OutputSchema map[string]any
	// CodeName is the name a code-mode script calls the tool by
	// ("BkCoreServices.get_client", "toolyard.inbox_request"), "" when it
	// has none.
	CodeName string
	// ReadOnly is the tool's readOnlyHint annotation: true when the
	// catalog says the tool changes nothing, so a request may ask for it
	// with operation "read".
	ReadOnly bool
}

// Mode picks how a query matches.
type Mode string

const (
	ModeKeyword  Mode = "keyword"
	ModeRegex    Mode = "regex"
	ModeSemantic Mode = "semantic"
	ModeAuto     Mode = "auto"
)

// ParseMode reads a mode name; "" is auto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeAuto, nil
	case ModeKeyword, ModeRegex, ModeSemantic, ModeAuto:
		return m, nil
	}
	return "", fmt.Errorf("unknown search mode %q: use keyword, regex, semantic or auto", s)
}

// Detail picks how much of each tool a result carries.
type Detail string

const (
	// DetailName is the catalog name alone.
	DetailName Detail = "name"
	// DetailSummary adds the server, a short description, the code-mode
	// name and the score.
	DetailSummary Detail = "summary"
	// DetailFull adds the whole description and the input and output
	// schemas.
	DetailFull Detail = "full"
)

// ParseDetail reads a detail level; "" is summary.
func ParseDetail(s string) (Detail, error) {
	switch d := Detail(strings.ToLower(strings.TrimSpace(s))); d {
	case "":
		return DetailSummary, nil
	case DetailName, DetailSummary, DetailFull:
		return d, nil
	}
	return "", fmt.Errorf("unknown detail %q: use name, summary or full", s)
}

const (
	// DefaultLimit and MaxLimit bound how many results a search returns.
	DefaultLimit = 50
	MaxLimit     = 200
	// MaxPatternLen caps a regex pattern, in characters.
	MaxPatternLen = 200
	// MaxQueryLen caps a keyword query, in characters.
	MaxQueryLen = 1000
	// summaryLen caps the description a summary result carries, in bytes.
	summaryLen = 300
	// indexCacheSize is how many keyword indexes (one per distinct
	// catalog) the engine keeps.
	indexCacheSize = 32
	// MaxSearchesPerCaller caps the regex searches one caller
	// (Query.Caller) runs at once; keyword searches, which the cached index
	// answers, are not capped. One more waits up to searchWait for one of
	// them to finish (a script's tools.parallel of a few searches still
	// works), then is refused.
	MaxSearchesPerCaller = 2
	searchWait           = 2 * time.Second
	// rankCacheSize and rankCacheTTL bound the cache of a provider's
	// scores: one entry per query and catalog, kept 10 minutes.
	rankCacheSize = 1024
	rankCacheTTL  = 10 * time.Minute
)

// RankedBy values: what ordered a result.
const (
	// RankedJev is the semantic provider's ranking (Jev).
	RankedJev = "jev"
	// RankedKeyword is BM25F.
	RankedKeyword = "keyword"
	// RankedRegex is a regex search.
	RankedRegex = "regex"
	// RankedName is an empty query's list, sorted by name.
	RankedName = "name"
)

// Fallback values: why an auto or semantic search ranked by keyword.
const (
	// FallbackOff: no semantic provider is configured (mode auto).
	FallbackOff = "off"
	// FallbackNoKey: the provider has no API key.
	FallbackNoKey = "no_key"
	// FallbackTimeout: the provider did not answer within its budget.
	FallbackTimeout = "timeout"
	// FallbackError: the provider failed or answered something invalid.
	FallbackError = "error"
	// FallbackEmpty: the caller has no tools to rank.
	FallbackEmpty = "empty"
	// FallbackBusy: the caller already runs MaxSearchesPerCaller searches;
	// a search that would pay the provider does not queue behind them.
	FallbackBusy = "busy"
)

// ErrSemanticOff answers a semantic search on a server without a
// SemanticProvider.
var ErrSemanticOff = errors.New("semantic search is not configured on this server: use mode \"keyword\" (ranked words) or \"regex\" (a pattern)")

// fallbackReason is the Fallback value for a provider's error.
func fallbackReason(err error) string {
	var re *rankError
	switch {
	case errors.Is(err, ErrNoKey):
		return FallbackNoKey
	case errors.Is(err, context.DeadlineExceeded):
		return FallbackTimeout
	case errors.As(err, &re) && re.outcome == FallbackTimeout:
		return FallbackTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return FallbackTimeout
	}
	return FallbackError
}

// QueryError is a query the engine refuses (a bad pattern, a bad limit).
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return e.Message }

// Query is one search.
type Query struct {
	// Text is the words (keyword, semantic) or the pattern (regex). Empty
	// matches every tool, sorted by name.
	Text string
	Mode Mode
	// Limit caps the results; 0 is DefaultLimit, more than MaxLimit is
	// MaxLimit.
	Limit int
	// Server keeps only the tools of one server ("" keeps all).
	Server string
	// Caller names who searches (an agent id), for the per-caller cap on
	// regex searches at once; "" is not capped.
	Caller string
}

// Hit is one ranked tool.
type Hit struct {
	Tool  Tool
	Score float64
}

// Result is what a search found.
type Result struct {
	// Mode is the mode that ran: semantic when the provider ranked, keyword
	// for auto without one and for a fallback.
	Mode Mode
	// RankedBy says what ordered the hits: RankedJev, RankedKeyword,
	// RankedRegex or RankedName.
	RankedBy string
	// Fallback says why an auto or semantic search ranked by keyword
	// (FallbackOff, FallbackNoKey, FallbackTimeout, FallbackError,
	// FallbackEmpty); "" when it did not fall back.
	Fallback string
	// Matched counts the tools that matched, before the limit.
	Matched int
	Hits    []Hit
}

// SemanticProvider ranks tools by meaning. A server that wants semantic
// search plugs one in (DecisionsRanker, an embeddings model); the engine
// hands it the caller's tools and the query and returns its ranking as it
// is, or the keyword ranking when it fails.
type SemanticProvider interface {
	Rank(ctx context.Context, tools []Tool, query string) ([]Hit, error)
}

// scorer is a SemanticProvider that judges the engine's keyword ranking
// rather than ranking alone (DecisionsRanker): it returns a score for each
// tool it judged, by name, and the engine orders, caches and falls back.
type scorer interface {
	score(ctx context.Context, tools []Tool, query string, keyword []Hit) (scores map[string]float64, err error)
}

// Options configure an Engine.
type Options struct {
	// Semantic serves modes semantic and auto; nil refuses semantic with
	// ErrSemanticOff and makes auto keyword.
	Semantic SemanticProvider
}

// Engine runs searches. It is safe for concurrent use.
type Engine struct {
	semantic SemanticProvider

	mu      sync.Mutex
	indexes map[string]*index
	order   []string // index keys, oldest first
	// ranks caches a scorer's answers by rankKey; rankOrder is their keys,
	// oldest first (an entry stored again leaves a stale reference, which
	// seq tells apart).
	ranks     map[string]rankEntry
	rankOrder []rankRef
	rankSeq   uint64
	// now is the clock of the rank cache (tests).
	now func() time.Time
	// running holds each caller's search slots while it has searches
	// running or waiting.
	running map[string]*callerSlots
	// wait is how long a search past the per-caller cap waits (tests).
	wait time.Duration
}

// rankEntry is one cached answer of a scorer.
type rankEntry struct {
	scores map[string]float64
	at     time.Time
	seq    uint64
}

type rankRef struct {
	key string
	seq uint64
}

// callerSlots are one caller's search slots and how many searches use or
// wait for them.
type callerSlots struct {
	slots chan struct{}
	users int
}

// New returns an Engine.
func New(opts Options) *Engine {
	return &Engine{semantic: opts.Semantic, indexes: map[string]*index{}, ranks: map[string]rankEntry{},
		now: time.Now, running: map[string]*callerSlots{}, wait: searchWait}
}

// SemanticEnabled reports whether mode semantic can run.
func (e *Engine) SemanticEnabled() bool { return e != nil && e.semantic != nil }

// Search ranks tools for q.
func (e *Engine) Search(ctx context.Context, tools []Tool, q Query) (*Result, error) {
	mode := q.Mode
	if mode == "" {
		mode = ModeAuto
	}
	limit := q.Limit
	switch {
	case limit < 0:
		return nil, &QueryError{Message: "limit must be at least 1"}
	case limit == 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}
	if q.Server != "" {
		kept := make([]Tool, 0, len(tools))
		for _, t := range tools {
			if t.Server == q.Server {
				kept = append(kept, t)
			}
		}
		tools = kept
	}
	text := strings.TrimSpace(q.Text)
	if mode != ModeRegex {
		if n := utf8.RuneCountInString(text); n > MaxQueryLen {
			return nil, &QueryError{Message: fmt.Sprintf("the query is %d characters; a search takes at most %d", n, MaxQueryLen)}
		}
	}
	var hits []Hit
	var err error
	rankedBy, fallback := RankedKeyword, ""
	switch {
	case mode == ModeSemantic && e.semantic == nil:
		return nil, ErrSemanticOff
	case text == "":
		hits, rankedBy = byName(tools), RankedName
		if mode == ModeAuto {
			mode = ModeKeyword
		}
	case mode == ModeRegex:
		rankedBy = RankedRegex
		if q.Caller != "" {
			// Only a regex search scans the catalog's text; cap how many one
			// caller runs at once.
			release, eerr := e.enter(ctx, q.Caller)
			if eerr != nil {
				return nil, eerr
			}
			defer release()
		}
		hits, err = regexSearch(ctx, tools, q.Text)
	case mode == ModeKeyword:
		hits = e.index(tools).search(text)
	case e.semantic == nil: // auto without a provider
		mode, fallback = ModeKeyword, FallbackOff
		hits = e.index(tools).search(text)
	default: // auto or semantic, with a provider
		if release, ok := e.tryEnter(q.Caller); ok {
			hits, fallback = e.rank(ctx, tools, text)
			release()
		} else {
			hits, fallback = e.index(tools).search(text), FallbackBusy
		}
		if fallback == "" {
			mode, rankedBy = ModeSemantic, RankedJev
		} else {
			mode = ModeKeyword
		}
	}
	if err != nil {
		return nil, err
	}
	res := &Result{Mode: mode, RankedBy: rankedBy, Fallback: fallback, Matched: len(hits), Hits: hits}
	if len(res.Hits) > limit {
		res.Hits = res.Hits[:limit]
	}
	return res, nil
}

// rank is a semantic search: the provider's ranking, or, when it fails,
// times out, has no key or has nothing to rank, the keyword ranking and
// why (a Fallback value). A scorer's answers are cached by query and
// catalog for rankCacheTTL.
func (e *Engine) rank(ctx context.Context, tools []Tool, text string) ([]Hit, string) {
	catalog := catalogKey(tools)
	keyword := e.indexFor(catalog, tools).search(text)
	if len(tools) == 0 {
		return keyword, FallbackEmpty
	}
	s, ok := e.semantic.(scorer)
	if !ok {
		hits, err := e.semantic.Rank(ctx, tools, text)
		if err != nil {
			return keyword, fallbackReason(err)
		}
		return hits, ""
	}
	key := rankKey(text, catalog)
	if scores, ok := e.cachedScores(key); ok {
		return merge(tools, scores, keyword), ""
	}
	scores, err := s.score(ctx, tools, text, keyword)
	if err != nil {
		return keyword, fallbackReason(err)
	}
	e.storeScores(key, scores)
	return merge(tools, scores, keyword), ""
}

// rankKey is the rank cache key of a query over a catalog (catalogKey).
func rankKey(text, catalog string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s\x00%s", len(text), text, catalog)
	return hex.EncodeToString(h.Sum(nil))
}

// cachedScores returns the scores cached under key, unless they are older
// than rankCacheTTL.
func (e *Engine) cachedScores(key string) (map[string]float64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.ranks[key]
	if !ok {
		return nil, false
	}
	if age := e.now().Sub(ent.at); age < 0 || age >= rankCacheTTL {
		delete(e.ranks, key)
		return nil, false
	}
	return ent.scores, true
}

// storeScores caches scores under key, dropping the oldest entries past
// rankCacheSize.
func (e *Engine) storeScores(key string, scores map[string]float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rankSeq++
	e.ranks[key] = rankEntry{scores: scores, at: e.now(), seq: e.rankSeq}
	e.rankOrder = append(e.rankOrder, rankRef{key: key, seq: e.rankSeq})
	for len(e.rankOrder) > rankCacheSize {
		old := e.rankOrder[0]
		e.rankOrder = e.rankOrder[1:]
		if ent, ok := e.ranks[old.key]; ok && ent.seq == old.seq {
			delete(e.ranks, old.key)
		}
	}
}

// merge orders the tools a scorer judged by their scores (ties by keyword
// score, then name), then every other keyword match in keyword order and
// without a score: the result still holds every keyword match, and a score
// is always the scorer's.
func merge(tools []Tool, scores map[string]float64, keyword []Hit) []Hit {
	kw := make(map[string]float64, len(keyword))
	for _, h := range keyword {
		kw[h.Tool.Name] = h.Score
	}
	out := make([]Hit, 0, len(scores)+len(keyword))
	for _, t := range tools {
		if p, ok := scores[t.Name]; ok {
			out = append(out, Hit{Tool: t, Score: p})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if kw[a.Tool.Name] != kw[b.Tool.Name] {
			return kw[a.Tool.Name] > kw[b.Tool.Name]
		}
		return a.Tool.Name < b.Tool.Name
	})
	for _, h := range keyword {
		if _, ok := scores[h.Tool.Name]; !ok {
			out = append(out, Hit{Tool: h.Tool})
		}
	}
	return out
}

// enter takes one of caller's search slots, waiting up to e.wait when the
// caller already runs MaxSearchesPerCaller; the returned func gives it
// back.
func (e *Engine) enter(ctx context.Context, caller string) (func(), error) {
	e.mu.Lock()
	cs := e.running[caller]
	if cs == nil {
		cs = &callerSlots{slots: make(chan struct{}, MaxSearchesPerCaller)}
		e.running[caller] = cs
	}
	cs.users++
	wait := e.wait
	e.mu.Unlock()
	leave := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if cs.users--; cs.users == 0 {
			delete(e.running, caller)
		}
	}
	select {
	case cs.slots <- struct{}{}:
		return func() { <-cs.slots; leave() }, nil
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case cs.slots <- struct{}{}:
		return func() { <-cs.slots; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	case <-t.C:
		leave()
		return nil, &QueryError{Message: fmt.Sprintf("this caller already runs %d searches at once, the limit, and none finished within %s; search again in a moment", MaxSearchesPerCaller, wait)}
	}
}

// tryEnter takes one of caller's search slots without waiting: a search
// that would pay the semantic provider ranks by keyword instead of queueing
// behind the caller's others, so one script cannot start many paid searches
// at once. ok is false when every slot is taken; an empty caller always
// gets in.
func (e *Engine) tryEnter(caller string) (func(), bool) {
	if caller == "" {
		return func() {}, true
	}
	e.mu.Lock()
	cs := e.running[caller]
	if cs == nil {
		cs = &callerSlots{slots: make(chan struct{}, MaxSearchesPerCaller)}
		e.running[caller] = cs
	}
	cs.users++
	e.mu.Unlock()
	leave := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if cs.users--; cs.users == 0 {
			delete(e.running, caller)
		}
	}
	select {
	case cs.slots <- struct{}{}:
		return func() { <-cs.slots; leave() }, true
	default:
		leave()
		return nil, false
	}
}

// byName is every tool, sorted by name, unscored.
func byName(tools []Tool) []Hit {
	hits := make([]Hit, len(tools))
	for i, t := range tools {
		hits[i] = Hit{Tool: t}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Tool.Name < hits[j].Tool.Name })
	return hits
}

// sortHits orders by score, best first, then by name.
func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Tool.Name < hits[j].Tool.Name
	})
}

// index returns the keyword index of tools, building it when this catalog
// has none cached.
func (e *Engine) index(tools []Tool) *index { return e.indexFor(catalogKey(tools), tools) }

// indexFor is index with the catalog's key already computed.
func (e *Engine) indexFor(key string, tools []Tool) *index {
	e.mu.Lock()
	ix, ok := e.indexes[key]
	e.mu.Unlock()
	if ok {
		return ix
	}
	ix = buildIndex(tools)
	e.mu.Lock()
	defer e.mu.Unlock()
	if have, ok := e.indexes[key]; ok {
		return have
	}
	e.indexes[key] = ix
	e.order = append(e.order, key)
	for len(e.order) > indexCacheSize {
		delete(e.indexes, e.order[0])
		e.order = e.order[1:]
	}
	return ix
}

// catalogKey hashes everything an index is built from.
func catalogKey(tools []Tool) string {
	names := make([]int, len(tools))
	for i := range names {
		names[i] = i
	}
	sort.Slice(names, func(a, b int) bool { return tools[names[a]].Name < tools[names[b]].Name })
	h := sha256.New()
	for _, i := range names {
		t := tools[i]
		schema, _ := json.Marshal(t.InputSchema)
		output, _ := json.Marshal(t.OutputSchema)
		fmt.Fprintf(h, "%d:%s\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s\x00%t\x00",
			len(t.Name), t.Name, len(t.Server), t.Server, len(t.Description), t.Description,
			len(t.CodeName), t.CodeName, len(schema), schema, len(output), output, t.ReadOnly)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Entry is one result as the surfaces return it. Name and Upstream keep
// the keys tools.search has always used.
type Entry struct {
	Name string `json:"name"`
	// MCPName is the name the gateway's tools/list shows for the tool when
	// it publishes OpenAI-safe names ("inbox_request" for
	// "inbox.request"); the gateway fills it in, Render leaves it empty.
	MCPName     string  `json:"mcp_name,omitempty"`
	Upstream    string  `json:"upstream,omitempty"`
	Description string  `json:"description,omitempty"`
	Code        string  `json:"code,omitempty"`
	Score       float64 `json:"score,omitempty"`
	// Run is what a call with no arguments would get now ("now",
	// "needs_approval" or "blocked"); the gateway fills it in, Render
	// leaves it empty.
	Run string `json:"run,omitempty"`
	// ReadOnly is the tool's readOnlyHint: only a read-only tool may be
	// requested with operation "read". Left out at detail name.
	ReadOnly     *bool          `json:"read_only,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
}

// Render shapes hits at detail d.
func Render(hits []Hit, d Detail) []Entry {
	out := make([]Entry, len(hits))
	for i, h := range hits {
		e := Entry{Name: h.Tool.Name}
		if d != DetailName {
			readOnly := h.Tool.ReadOnly
			e.ReadOnly = &readOnly
		}
		switch d {
		case DetailName:
		case DetailFull:
			e.Upstream, e.Description, e.Code = h.Tool.Server, strings.TrimSpace(h.Tool.Description), h.Tool.CodeName
			e.Score = round(h.Score)
			e.InputSchema, e.OutputSchema = h.Tool.InputSchema, h.Tool.OutputSchema
		default: // summary
			e.Upstream, e.Description, e.Code = h.Tool.Server, summarize(h.Tool.Description), h.Tool.CodeName
			e.Score = round(h.Score)
		}
		out[i] = e
	}
	return out
}

func round(f float64) float64 { return math.Round(f*1e4) / 1e4 }

// summarize is a description's first paragraph, cut at summaryLen bytes on
// a word boundary.
func summarize(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.Index(desc, "\n\n"); i >= 0 {
		desc = desc[:i]
	}
	return clip(desc, summaryLen)
}

// clip is s with its whitespace collapsed, cut at n bytes on a word
// boundary (or inside a long word, never inside a character) with an
// ellipsis.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut < n/2 {
		cut = n
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

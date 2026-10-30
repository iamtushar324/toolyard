package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// Recommendations (skills.recommend): the viewer's skills ranked for a task
// described in plain words. The candidates are exactly what Search would
// rank (the same sharing rule and where-it-runs filter), narrowed by source
// and tags. Each skill is ranked on its name, path, description, when to
// use it (metadata.triggers, metadata.when-to-use, else the description's
// "use when" sentences) and its tags (metadata.tags), by the engine
// SetRecommendEngine gave: keyword BM25F, or hybrid when the server's
// tool-search ranker is on (keyword candidates, which Jev judges). A ranker
// that fails falls back to keyword and says why (RecommendResult.Fallback).
// The rationale is built from data only, never from what a provider says.

// Recommendation limits.
const (
	recommendDefaultLimit = 5
	recommendMaxLimit     = 20
	// recommendMaxTask caps the task, in characters.
	recommendMaxTask = 1000
	// recommendMaxTags caps the tags a query filters by.
	recommendMaxTags = 10
	// recommendDescLen caps the description an answer carries, in bytes.
	recommendDescLen = 240
	// recommendShownTriggers and recommendTriggerLen cap the triggers an
	// answer carries; recommendRankTriggers caps those ranked on.
	recommendShownTriggers = 3
	recommendRankTriggers  = 10
	recommendTriggerLen    = 160
	// recommendShownTags caps the tags an answer carries.
	recommendShownTags = 8
	// recommendRationaleLen caps a rationale, in bytes.
	recommendRationaleLen = 200
	// recommendRationaleWords caps the matched words a rationale names.
	recommendRationaleWords = 3
)

// RecommendQuery asks for the skills that fit a task.
type RecommendQuery struct {
	Task  string `json:"task"`
	Limit int    `json:"limit,omitempty"` // default 5, at most 20
	// Source keeps one source's skills, by slug ("" keeps all).
	Source string `json:"source,omitempty"`
	// Tags keeps the skills with any of these tags (metadata.tags,
	// case-insensitive); none keeps all.
	Tags        []string `json:"tags,omitempty"`
	IncludeHost bool     `json:"include_host,omitempty"`
	// Mode is "" or "auto" (by meaning when a ranker is configured, else
	// keyword), "keyword", or "semantic" (refused without a ranker).
	Mode string `json:"mode,omitempty"`
}

// Recommendation is one skill that fits a task: enough to choose it, and
// the URI to load it with (skills.get).
type Recommendation struct {
	URI         string   `json:"uri,omitempty"` // skill://<slug>/<path>/SKILL.md
	Name        string   `json:"name,omitempty"`
	Source      string   `json:"source,omitempty"`
	Path        string   `json:"path,omitempty"`
	Description string   `json:"description,omitempty"` // clipped
	Triggers    []string `json:"triggers,omitempty"`    // when to use it, at most 3
	Tags        []string `json:"tags,omitempty"`
	Runs        Runs     `json:"runs,omitempty"`
	Score       float64  `json:"score,omitempty"`
	// Revision is a native skill's live revision (native.go).
	Revision int `json:"revision,omitempty"`
	// Rationale says, in one short sentence, why the skill was ranked:
	// which words of the task it shares, where, and Jev's rating.
	Rationale string `json:"rationale,omitempty"`
}

// RecommendResult is a ranked answer for a task.
type RecommendResult struct {
	Task string `json:"task"`
	// RankedBy is what ordered the skills: "jev" or "keyword"
	// (toolsearch.RankedJev, RankedKeyword).
	RankedBy string `json:"ranked_by"`
	// Fallback says why an auto or semantic ranking ranked by keyword
	// (toolsearch.Fallback*); "" when it did not fall back.
	Fallback string `json:"fallback,omitempty"`
	// Matched counts the skills that matched, before the limit.
	Matched int              `json:"matched"`
	Skills  []Recommendation `json:"skills"`
}

// recommendSlot holds the engine recommendations rank with; empty means
// the Service's keyword-only engine. It is safe to set after Start.
type recommendSlot struct {
	engine atomic.Pointer[toolsearch.Engine]
}

// SetRecommendEngine sets the engine skills.recommend ranks with: the
// gateway passes the tools.search engine when its ranker is on, so the
// hybrid ranking, its caches and its per-caller cap are shared. nil keeps
// (or restores) keyword ranking.
func (s *Service) SetRecommendEngine(e *toolsearch.Engine) { s.recommend.engine.Store(e) }

// recommendEngine is the engine recommendations rank with.
func (s *Service) recommendEngine() *toolsearch.Engine {
	if e := s.recommend.engine.Load(); e != nil {
		return e
	}
	return s.search
}

// recommendCandidate is one skill a recommendation may name.
type recommendCandidate struct {
	si    *srcIndex
	e     *skillEntry
	hints skillHints
}

// Recommend ranks the viewer's skills for a task.
func (s *Service) Recommend(ctx context.Context, v Viewer, q RecommendQuery) (*RecommendResult, error) {
	task := strings.Join(strings.Fields(q.Task), " ")
	if task == "" {
		return nil, fmt.Errorf("%w: describe the task in a sentence or two", ErrInvalid)
	}
	if n := utf8.RuneCountInString(task); n > recommendMaxTask {
		return nil, fmt.Errorf("%w: the task is %d characters; describe it in at most %d", ErrInvalid, n, recommendMaxTask)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = recommendDefaultLimit
	case limit > recommendMaxLimit:
		limit = recommendMaxLimit
	}
	if len(q.Tags) > recommendMaxTags {
		return nil, fmt.Errorf("%w: at most %d tags", ErrInvalid, recommendMaxTags)
	}
	eng := s.recommendEngine()
	mode := toolsearch.ModeAuto
	switch strings.ToLower(strings.TrimSpace(q.Mode)) {
	case "", "auto":
	case "keyword":
		mode = toolsearch.ModeKeyword
	case "semantic":
		if !eng.SemanticEnabled() {
			return nil, fmt.Errorf("%w: semantic ranking is not configured on this server: use mode auto or keyword", ErrInvalid)
		}
		mode = toolsearch.ModeSemantic
	default:
		return nil, fmt.Errorf("%w: recommend mode %q: use auto, keyword or semantic", ErrInvalid, q.Mode)
	}
	want := map[string]bool{}
	for _, t := range q.Tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			want[t] = true
		}
	}

	refs := map[string]recommendCandidate{}
	var tools []toolsearch.Tool
	for _, si := range s.visibleSources(v, strings.TrimSpace(q.Source)) {
		for _, e := range si.skills {
			if !showRuns(v, q.IncludeHost, e.runs) || !si.skillVisible(v, e.path) {
				continue
			}
			h := hintsOf(e)
			if len(want) > 0 && !h.hasTag(want) {
				continue
			}
			name := recommendToolName(si, e)
			refs[name] = recommendCandidate{si: si, e: e, hints: h}
			tools = append(tools, toolsearch.Tool{Name: name, Server: si.src.Slug, Description: h.rankText(e.description), CodeName: e.name})
		}
	}
	caller := v.AgentID
	if caller == "" {
		caller = v.UserID
	}
	res, err := eng.Search(ctx, tools, toolsearch.Query{Text: task, Mode: mode, Limit: limit, Caller: caller})
	if err != nil {
		var qe *toolsearch.QueryError
		switch {
		case errors.As(err, &qe):
			return nil, fmt.Errorf("%w: %s", ErrInvalid, qe.Message)
		case errors.Is(err, toolsearch.ErrSemanticOff):
			return nil, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
		}
		return nil, err
	}
	out := &RecommendResult{Task: task, RankedBy: res.RankedBy, Fallback: res.Fallback, Matched: res.Matched,
		Skills: make([]Recommendation, 0, len(res.Hits))}
	words := recTokens(task)
	for _, hit := range res.Hits {
		c, ok := refs[hit.Tool.Name]
		if !ok {
			continue
		}
		// A hybrid ranking scores only what Jev judged: the keyword
		// matches it did not judge follow without a score.
		judged := res.RankedBy == toolsearch.RankedJev && hit.Score > 0
		out.Skills = append(out.Skills, Recommendation{
			URI: c.si.skillURI(c.e.path), Name: c.e.name, Source: c.si.src.Slug, Path: c.e.path,
			Description: recClip(c.e.description, recommendDescLen),
			Triggers:    firstStrings(c.hints.triggers, recommendShownTriggers),
			Tags:        firstStrings(c.hints.tags, recommendShownTags),
			Runs:        c.e.runs,
			Revision:    c.e.revision,
			Score:       math.Round(hit.Score*1000) / 1000,
			Rationale:   recRationale(words, c.e, c.hints, res.RankedBy, hit.Score, judged),
		})
	}
	return out, nil
}

// recommendToolName is a skill's name as the engine sees it: slug/path, as
// Search names it (a skill at the source folder itself: slug/name, or
// slug/ when a skill folder has that name).
func recommendToolName(si *srcIndex, e *skillEntry) string {
	if e.path != "" {
		return si.src.Slug + "/" + e.path
	}
	if si.byPath[e.name] != nil {
		return si.src.Slug + "/"
	}
	return si.src.Slug + "/" + e.name
}

// skillHints is what a skill's frontmatter says about when to use it.
type skillHints struct {
	// triggers say when to use the skill, at most recommendRankTriggers,
	// each clipped to recommendTriggerLen bytes.
	triggers []string
	// fromMeta: the triggers come from the metadata, not the description
	// (whose text is ranked anyway).
	fromMeta bool
	tags     []string
}

// hintsOf reads a skill's triggers and tags from its stored frontmatter.
func hintsOf(e *skillEntry) skillHints {
	var fm map[string]any
	if len(e.frontmatter) > 0 {
		_ = json.Unmarshal(e.frontmatter, &fm)
	}
	meta, _ := fm["metadata"].(map[string]any)
	var h skillHints
	h.triggers, h.fromMeta = triggersFrom(meta, e.description)
	h.tags = tagsFrom(meta)
	return h
}

// hasTag reports whether the skill has any of want (lowercased tags).
func (h skillHints) hasTag(want map[string]bool) bool {
	for _, t := range h.tags {
		if want[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

// rankText is the text a skill is ranked on besides its name: the
// description, when to use it and its tags, so keyword ranking and Jev
// both see them. Triggers taken from the description are not repeated.
func (h skillHints) rankText(desc string) string {
	var b strings.Builder
	b.WriteString(desc)
	if h.fromMeta && len(h.triggers) > 0 {
		b.WriteString(" When to use: ")
		b.WriteString(strings.Join(h.triggers, "; "))
	}
	if len(h.tags) > 0 {
		b.WriteString(" Tags: ")
		b.WriteString(strings.Join(h.tags, ", "))
	}
	return b.String()
}

// triggerKeys are the metadata keys that say when to use a skill.
var triggerKeys = []string{"triggers", "when-to-use", "when_to_use"}

// triggerCues mark a description sentence that says when to use a skill.
var triggerCues = []string{
	"use when", "use this when", "use whenever", "use for", "use it when", "trigger", "when the user", "when you",
}

// triggersFrom is when to use a skill: metadata.triggers (a list, or text
// split at ";" and line breaks), metadata.when-to-use or when_to_use;
// without those, the description's sentences that say when (triggerCues).
// fromMeta reports the first case.
func triggersFrom(meta map[string]any, desc string) (out []string, fromMeta bool) {
	for _, k := range triggerKeys {
		out = append(out, listValue(meta[k], ";\n")...)
	}
	if len(out) > 0 {
		fromMeta = true
	} else {
		for _, s := range sentences(desc) {
			low := strings.ToLower(s)
			for _, cue := range triggerCues {
				if strings.Contains(low, cue) {
					out = append(out, s)
					break
				}
			}
		}
	}
	return clipList(out, recommendRankTriggers, recommendTriggerLen), fromMeta
}

// tagsFrom is metadata.tags: a list, or text split at commas, semicolons
// and line breaks; each tag once (case-insensitive), as written.
func tagsFrom(meta map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range listValue(meta["tags"], ",;\n") {
		if k := strings.ToLower(t); !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

// listValue is a frontmatter value as a list of trimmed, non-empty
// strings: a list's string items, or a string split at any of seps.
func listValue(v any, seps string) []string {
	var out []string
	add := func(s string) {
		if s = strings.Join(strings.Fields(s), " "); s != "" {
			out = append(out, s)
		}
	}
	switch x := v.(type) {
	case string:
		for _, part := range strings.FieldsFunc(x, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
			add(part)
		}
	case []any:
		for _, item := range x {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	}
	return out
}

// sentences splits text at line breaks and at ".", "!" or "?" before a
// space or the end.
func sentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		end := -1
		switch c := text[i]; {
		case c == '\n' || c == '\r':
			end = i
		case c == '.' || c == '!' || c == '?':
			if i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\t' || text[i+1] == '\n' || text[i+1] == '\r' {
				end = i + 1
			}
		}
		if end < 0 {
			continue
		}
		if s := strings.TrimSpace(text[start:end]); s != "" {
			out = append(out, s)
		}
		start = end
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// clipList is each distinct item of list (case-insensitive) clipped to n
// bytes, at most max of them.
func clipList(list []string, max, n int) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		if len(out) == max {
			break
		}
		if k := strings.ToLower(s); !seen[k] {
			seen[k] = true
			out = append(out, recClip(s, n))
		}
	}
	return out
}

// firstStrings is at most n items of list.
func firstStrings(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// recClip is s with its whitespace collapsed, cut at n bytes (with the
// ellipsis) on a word boundary, or inside a long word, never inside a
// character.
func recClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	room := n - len(ellipsis)
	if room <= 0 {
		return ellipsis
	}
	cut := strings.LastIndexByte(s[:room+1], ' ')
	if cut < room/2 {
		cut = room
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return strings.TrimSpace(s[:cut]) + ellipsis
}

// recStopWords carry no meaning in a rationale.
var recStopWords = map[string]bool{
	"a": true, "about": true, "after": true, "all": true, "also": true, "an": true, "and": true, "any": true,
	"are": true, "as": true, "at": true, "be": true, "before": true, "by": true, "can": true, "could": true,
	"do": true, "does": true, "for": true, "from": true, "get": true, "has": true, "have": true, "help": true,
	"how": true, "i": true, "if": true, "in": true, "into": true, "is": true, "it": true, "its": true,
	"just": true, "let": true, "like": true, "make": true, "me": true, "my": true, "need": true, "new": true,
	"no": true, "not": true, "of": true, "on": true, "or": true, "our": true, "out": true, "please": true,
	"should": true, "so": true, "some": true, "that": true, "the": true, "their": true, "them": true,
	"then": true, "there": true, "these": true, "this": true, "those": true, "to": true, "up": true,
	"us": true, "use": true, "used": true, "using": true, "via": true, "want": true, "was": true, "we": true,
	"what": true, "when": true, "where": true, "which": true, "while": true, "who": true, "why": true,
	"will": true, "with": true, "would": true, "you": true, "your": true,
}

// recTokens are text's words for a rationale: lowercased, split at
// everything but letters and digits, stop words and one-letter words
// dropped, each once.
func recTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(w) < 2 || recStopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// recWordMatch reports whether a task word matches a field word: equal, or
// one is a prefix of the other sharing at least four letters (deploy,
// deployment).
func recWordMatch(q, f string) bool {
	if q == f {
		return true
	}
	short, long := q, f
	if len(short) > len(long) {
		short, long = long, short
	}
	return utf8.RuneCountInString(short) >= 4 && strings.HasPrefix(long, short)
}

// recField is one part of a skill a rationale names.
type recField struct {
	label string
	words []string
}

// recRationale says why a skill was ranked, from data only: the task's
// words it shares and where, and Jev's rating when Jev judged it.
func recRationale(task []string, e *skillEntry, h skillHints, rankedBy string, score float64, judged bool) string {
	fields := []recField{
		{"name", recTokens(e.name + " " + e.path)},
		{"triggers", recTokens(strings.Join(h.triggers, " "))},
		{"tags", recTokens(strings.Join(h.tags, " "))},
		{"description", recTokens(e.description)},
	}
	matched := map[string]bool{}
	var labels []string
	for _, f := range fields {
		hit := false
		for _, q := range task {
			for _, w := range f.words {
				if recWordMatch(q, w) {
					hit, matched[q] = true, true
					break
				}
			}
		}
		if hit {
			labels = append(labels, f.label)
		}
	}
	var quoted []string
	for _, q := range task {
		if matched[q] && len(quoted) < recommendRationaleWords {
			quoted = append(quoted, strconv.Quote(q))
		}
	}
	jev := func(withTask bool) string {
		p := strconv.FormatFloat(score, 'f', 2, 64)
		verdict := "relevant"
		if score < 0.5 {
			verdict = "a weak match"
		}
		if withTask && score >= 0.5 {
			verdict += " to the task"
		} else if withTask {
			verdict += " for the task"
		}
		return "Jev rates it " + verdict + " (" + p + ")"
	}
	var out string
	switch {
	case len(quoted) == 0 && judged:
		out = jev(true) + "; no shared words"
	case len(quoted) == 0:
		out = "matches forms of the task's words (keyword rank)"
	default:
		out = "matches " + strings.Join(quoted, ", ") + " in its " + joinLabels(labels)
		if judged {
			out += "; " + jev(false)
		} else if rankedBy != toolsearch.RankedJev {
			out += " (keyword rank)"
		} else {
			out += " (keyword match; Jev did not rate it)"
		}
	}
	return recClip(out, recommendRationaleLen)
}

// joinLabels is "a", "a and b" or "a, b and c".
func joinLabels(labels []string) string {
	switch n := len(labels); n {
	case 0:
		return ""
	case 1:
		return labels[0]
	default:
		return strings.Join(labels[:n-1], ", ") + " and " + labels[n-1]
	}
}

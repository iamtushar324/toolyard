package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// The retrieval-quality evaluation of skills.recommend: a corpus of
// generic skills (testdata/recommend-eval/<name>/SKILL.md, some
// near-duplicates of others) and labelled tasks (queries.json; paraphrase
// marks a task that shares few words with its skill). The keyword ranking
// must stay above conservative floors; scripts/skills-recommend-eval.py
// measures the same tasks on a live server, Jev included.

const recommendEvalDir = "testdata/recommend-eval"

type evalQuery struct {
	Task       string   `json:"task"`
	Relevant   []string `json:"relevant"`
	Paraphrase bool     `json:"paraphrase,omitempty"`
}

// evalScore sums recall@1, recall@3 and MRR@5 over tasks. recall@k is the
// share of a task's relevant skills in the top k, out of at most k.
type evalScore struct {
	n             int
	r1, r3, mrr   float64
	missedAtThree []string
}

func (s *evalScore) add(task string, got, relevant []string) {
	rel := map[string]bool{}
	for _, r := range relevant {
		rel[r] = true
	}
	recall := func(k int) float64 {
		hits := 0
		for i := 0; i < k && i < len(got); i++ {
			if rel[got[i]] {
				hits++
			}
		}
		return float64(hits) / float64(min(len(relevant), k))
	}
	s.n++
	s.r1 += recall(1)
	r3 := recall(3)
	s.r3 += r3
	if r3 == 0 {
		s.missedAtThree = append(s.missedAtThree, task)
	}
	for i := 0; i < 5 && i < len(got); i++ {
		if rel[got[i]] {
			s.mrr += 1 / float64(i+1)
			break
		}
	}
}

func (s *evalScore) mean() (r1, r3, mrr float64) {
	if s.n == 0 {
		return 0, 0, 0
	}
	n := float64(s.n)
	return s.r1 / n, s.r3 / n, s.mrr / n
}

func (s *evalScore) String() string {
	r1, r3, mrr := s.mean()
	return fmt.Sprintf("n=%d recall@1=%.3f recall@3=%.3f mrr=%.3f", s.n, r1, r3, mrr)
}

// recommendEvalCorpus is the corpus as a repository tree under skills/.
func recommendEvalCorpus(t *testing.T) (map[string]string, []string) {
	t.Helper()
	entries, err := os.ReadDir(recommendEvalDir)
	if err != nil {
		t.Fatal(err)
	}
	tree := map[string]string{}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(recommendEvalDir, e.Name(), skillFile))
		if err != nil {
			t.Fatal(err)
		}
		tree["skills/"+e.Name()+"/"+skillFile] = string(b)
		names = append(names, e.Name())
	}
	return tree, names
}

func recommendEvalQueries(t *testing.T) []evalQuery {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(recommendEvalDir, "queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Queries []evalQuery `json:"queries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Queries
}

// recommendEvalService is a hub serving the corpus as the source "lib",
// loaded through the normal sync and index path.
func recommendEvalService(t *testing.T) *Service {
	t.Helper()
	tree, names := recommendEvalCorpus(t)
	if len(names) < 30 || len(names) > 40 {
		t.Fatalf("the corpus has %d skills; keep 30 to 40", len(names))
	}
	gh := newFakeGitHub(t, "acme", "evals")
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	_, res := mustCreate(t, svc, SourceInput{URL: "acme/evals", Path: "skills", Slug: "lib", Mode: ModeAuto})
	if !res.Activated {
		t.Fatalf("the corpus did not go live: %+v", res)
	}
	listed, _, err := svc.List(context.Background(), memberViewer, ListQuery{Source: "lib", Limit: 100})
	if err != nil || len(listed) != len(names) {
		t.Fatalf("served %d of %d corpus skills (%v): check their frontmatter", len(listed), len(names), err)
	}
	return svc
}

func TestRecommendEvalKeyword(t *testing.T) {
	svc := recommendEvalService(t)
	_, names := recommendEvalCorpus(t)
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	queries := recommendEvalQueries(t)
	if len(queries) < 40 {
		t.Fatalf("%d labelled tasks; keep at least 40", len(queries))
	}
	ctx := context.Background()
	var all, direct, para evalScore
	for _, q := range queries {
		for _, r := range q.Relevant {
			if !known[r] {
				t.Fatalf("%q: relevant skill %q is not in the corpus", q.Task, r)
			}
		}
		res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: q.Task, Mode: "keyword", Limit: 5})
		if err != nil {
			t.Fatalf("%q: %v", q.Task, err)
		}
		if res.RankedBy != toolsearch.RankedKeyword || res.Fallback != "" || len(res.Skills) > 5 {
			t.Fatalf("%q: ranked_by %q fallback %q, %d skills", q.Task, res.RankedBy, res.Fallback, len(res.Skills))
		}
		var got []string
		for _, s := range res.Skills {
			got = append(got, s.Name)
			if !strings.HasPrefix(s.URI, "skill://lib/") || s.Rationale == "" || len(s.Rationale) > recommendRationaleLen ||
				len(s.Description) > recommendDescLen || len(s.Triggers) > recommendShownTriggers {
				t.Fatalf("%q: recommendation %+v", q.Task, s)
			}
		}
		all.add(q.Task, got, q.Relevant)
		if q.Paraphrase {
			para.add(q.Task, got, q.Relevant)
		} else {
			direct.add(q.Task, got, q.Relevant)
		}
	}
	t.Logf("recommend-eval keyword: %s", &all)
	t.Logf("recommend-eval keyword direct: %s", &direct)
	t.Logf("recommend-eval keyword paraphrase: %s", &para)
	if len(para.missedAtThree) > 0 {
		t.Logf("recommend-eval keyword: paraphrases missed in the top 3: %q", para.missedAtThree)
	}
	if para.n < 10 {
		t.Errorf("%d paraphrased tasks; keep at least 10", para.n)
	}
	// Conservative floors: a pure keyword ranking finds the skill a task
	// names in its own words; paraphrases are measured, not held to one.
	_, allR3, _ := all.mean()
	_, dirR3, dirMRR := direct.mean()
	if allR3 < 0.6 || dirR3 < 0.85 || dirMRR < 0.75 {
		t.Errorf("keyword retrieval fell below its floors (all recall@3 >= 0.6, direct recall@3 >= 0.85, direct mrr >= 0.75); missed in the top 3: %q",
			append(direct.missedAtThree, para.missedAtThree...))
	}
}

// fakeRanker is a plain SemanticProvider: it ranks the skills named in
// order, with scores, or fails with err.
type fakeRanker struct {
	mu     sync.Mutex
	order  []string
	scores []float64
	err    error
	calls  int
	seen   int // tools in the last call
}

func (f *fakeRanker) Rank(_ context.Context, tools []toolsearch.Tool, _ string) ([]toolsearch.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = len(tools)
	if f.err != nil {
		return nil, f.err
	}
	byName := map[string]toolsearch.Tool{}
	for _, t := range tools {
		byName[t.CodeName] = t
	}
	var hits []toolsearch.Hit
	for i, n := range f.order {
		if t, ok := byName[n]; ok {
			hits = append(hits, toolsearch.Hit{Tool: t, Score: f.scores[i]})
		}
	}
	return hits, nil
}

// With a semantic provider the provider's order is the answer (ranked_by
// jev) and the rationale names its rating; a failing provider falls back
// to keyword and says why; without one, semantic is refused.
func TestRecommendSemanticAndFallback(t *testing.T) {
	svc := recommendEvalService(t)
	ctx := context.Background()
	r := &fakeRanker{order: []string{"meeting-notes", "translate-docs"}, scores: []float64{0.91, 0.83}}
	svc.SetRecommendEngine(toolsearch.New(toolsearch.Options{Semantic: r}))

	for _, mode := range []string{"", "auto", "semantic"} {
		res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "convert the user guide to spanish", Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		if res.RankedBy != toolsearch.RankedJev || res.Fallback != "" || len(res.Skills) != 2 {
			t.Fatalf("mode %q: %+v", mode, res)
		}
		first, second := res.Skills[0], res.Skills[1]
		if first.Name != "meeting-notes" || first.Score != 0.91 || second.Name != "translate-docs" || second.Score != 0.83 {
			t.Fatalf("mode %q: order %+v", mode, res.Skills)
		}
		// meeting-notes says when to use it in its description ("Use this
		// when the user shares..."), which shares "user" with the task.
		if !strings.Contains(first.Rationale, `"user"`) || !strings.HasSuffix(first.Rationale, "; Jev rates it relevant (0.91)") {
			t.Fatalf("rationale %q", first.Rationale)
		}
		if second.Rationale != "Jev rates it relevant to the task (0.83); no shared words" {
			t.Fatalf("rationale %q", second.Rationale)
		}
	}
	r.mu.Lock()
	seen := r.seen
	r.mu.Unlock()
	if _, names := recommendEvalCorpus(t); seen != len(names) {
		t.Fatalf("the provider saw %d skills, want every visible one (%d)", seen, len(names))
	}
	// The tag filter narrows what the provider is shown.
	if res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "convert the user guide", Tags: []string{"documents"}}); err != nil ||
		len(res.Skills) != 1 || res.Skills[0].Name != "translate-docs" {
		t.Fatalf("tag filter with a provider: %+v %v", res, err)
	}

	// A failing provider: keyword ranking, and why.
	r.mu.Lock()
	r.err = errors.New("provider down")
	r.mu.Unlock()
	res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "fill in the PDF form"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RankedBy != toolsearch.RankedKeyword || res.Fallback != toolsearch.FallbackError || len(res.Skills) == 0 ||
		res.Skills[0].Name != "pdf-fill-form" || !strings.HasSuffix(res.Skills[0].Rationale, "(keyword rank)") {
		t.Fatalf("fallback: %+v", res)
	}
	// Mode keyword never asks the provider.
	r.mu.Lock()
	before := r.calls
	r.mu.Unlock()
	if res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "fill in the PDF form", Mode: "keyword"}); err != nil || res.Fallback != "" {
		t.Fatalf("keyword with a provider: %+v %v", res, err)
	}
	r.mu.Lock()
	after := r.calls
	r.mu.Unlock()
	if after != before {
		t.Fatal("mode keyword asked the provider")
	}

	// nil restores keyword-only ranking: semantic is refused.
	svc.SetRecommendEngine(nil)
	if _, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "fill in the PDF form", Mode: "semantic"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("semantic without a ranker: %v", err)
	}
	if res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "fill in the PDF form"}); err != nil || res.Fallback != toolsearch.FallbackOff {
		t.Fatalf("auto without a ranker: %+v %v", res, err)
	}
}

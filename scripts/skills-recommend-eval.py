#!/usr/bin/env python3
"""Measure skills.recommend retrieval quality on a running toolyard.

Asks GET /v1/skills/recommend for every labelled task in
internal/skillhub/testdata/recommend-eval/queries.json, once per mode, and
prints recall@1, recall@3 and MRR@5 per mode, for all tasks and separately
for the direct and the paraphrased ones, plus what ranked the answers
(ranked_by) and why a ranking fell back to keyword (fallback). The unit test
TestRecommendEvalKeyword measures the keyword ranking offline; this script
measures Jev (mode auto or semantic) on a server whose tool-search ranker is
on.

The labels name the skills of the corpus in the same folder: serve that
corpus as a source first (for example push the folders to a repository and
add it on the Skills page) and pass its slug as SOURCE, so the rest of the
catalog does not count.

Run it with the standard library only:

  BASE=https://toolyard.example TOKEN_FILE=~/.toolyard/agent-token SOURCE=evals \\
    python3 -I scripts/skills-recommend-eval.py

Inputs (environment; the token is read from a file, never from argv):
  BASE        toolyard URL (required)
  TOKEN_FILE  file holding an agent token or a session token (required)
  SOURCE      only this source's skills (recommended; default: all)
  MODES       comma-separated modes (default keyword,auto)
  LIMIT       skills per answer (default 5)
  QUERIES     queries.json path (default: the corpus beside the unit test)

The bearer never appears in the output. Exit status 1 when any call failed.
"""

import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_QUERIES = os.path.join(HERE, "..", "internal", "skillhub", "testdata", "recommend-eval", "queries.json")
REASON = "measure skills.recommend retrieval quality with the labelled eval tasks"


def env(name, default=None, required=False):
    value = os.environ.get(name, "").strip()
    if not value:
        if required:
            sys.exit(f"{name} is required (see the docstring)")
        return default
    return value


class Score:
    """Sums recall@1, recall@3 and MRR@5; recall@k is the share of a task's
    relevant skills in the top k, out of at most k."""

    def __init__(self):
        self.n = 0
        self.r1 = 0.0
        self.r3 = 0.0
        self.mrr = 0.0

    def add(self, got, relevant):
        rel = set(relevant)

        def recall(k):
            return len(rel & set(got[:k])) / min(len(rel), k)

        self.n += 1
        self.r1 += recall(1)
        self.r3 += recall(3)
        for i, name in enumerate(got[:5]):
            if name in rel:
                self.mrr += 1 / (i + 1)
                break

    def line(self):
        if self.n == 0:
            return "n=0"
        n = self.n
        return f"n={n} recall@1={self.r1 / n:.3f} recall@3={self.r3 / n:.3f} mrr={self.mrr / n:.3f}"


def recommend(base, token, task, mode, limit, source):
    params = {"task": task, "mode": mode, "limit": str(limit), "reason": REASON}
    if source:
        params["source"] = source
    url = base.rstrip("/") + "/v1/skills/recommend?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def main():
    base = env("BASE", required=True)
    token_file = os.path.expanduser(env("TOKEN_FILE", required=True))
    with open(token_file, encoding="utf-8") as f:
        token = f.read().strip()
    if not token:
        sys.exit("TOKEN_FILE is empty")
    source = env("SOURCE")
    modes = [m.strip() for m in env("MODES", "keyword,auto").split(",") if m.strip()]
    limit = int(env("LIMIT", "5"))
    with open(env("QUERIES", DEFAULT_QUERIES), encoding="utf-8") as f:
        queries = json.load(f)["queries"]

    failed = 0
    for mode in modes:
        scores = {"all": Score(), "direct": Score(), "paraphrase": Score()}
        ranked_by, fallback = {}, {}
        for q in queries:
            try:
                res = recommend(base, token, q["task"], mode, limit, source)
            except urllib.error.HTTPError as e:
                failed += 1
                body = e.read(300).decode("utf-8", "replace")
                print(f"  {mode}: {q['task']!r}: HTTP {e.code} {body}", file=sys.stderr)
                continue
            except (urllib.error.URLError, TimeoutError, ValueError) as e:
                failed += 1
                print(f"  {mode}: {q['task']!r}: {type(e).__name__}: {e}", file=sys.stderr)
                continue
            got = [s.get("name", "") for s in res.get("skills", [])]
            scores["all"].add(got, q["relevant"])
            scores["paraphrase" if q.get("paraphrase") else "direct"].add(got, q["relevant"])
            rb = res.get("ranked_by", "?")
            ranked_by[rb] = ranked_by.get(rb, 0) + 1
            if res.get("fallback"):
                fallback[res["fallback"]] = fallback.get(res["fallback"], 0) + 1
        print(f"recommend-eval {mode}: {scores['all'].line()}")
        print(f"recommend-eval {mode} direct: {scores['direct'].line()}")
        print(f"recommend-eval {mode} paraphrase: {scores['paraphrase'].line()}")
        counts = " ".join(f"{k}={v}" for k, v in sorted(ranked_by.items()))
        falls = " ".join(f"{k}={v}" for k, v in sorted(fallback.items())) or "none"
        print(f"recommend-eval {mode} ranked_by: {counts} fallback: {falls}")
    if failed:
        print(f"{failed} calls failed", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()

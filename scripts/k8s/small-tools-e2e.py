#!/usr/bin/env python3
"""End-to-end check of the five-tool list and tool search on a running deployment.

Runs against the Kubernetes HA test deployment (two replicas, Postgres) or any
toolyard. It signs in as the first admin (credentials from the
`toolyard-admin` Secret through kubectl, or ADMIN_FILE), makes an agent key and
checks:

  - tools/list shows exactly the five tools by default, every tool after the
    agent is switched to the full list, and the five again after switching back,
    every listed name OpenAI-safe (^[a-zA-Z0-9_-]{1,64}$: tools_search for
    tools.search, -mcp-safe-tool-names, the default);
  - tools.search finds a tool that is not listed, says how it ranked
    (ranked_by, fallback), and gives every hit a run status that agrees with
    policies.explain;
  - a tool that is not listed still runs: directly (tools/call) by its
    canonical name (access.whoami) and by its published name (access_whoami),
    and from a code-mode script that found it with tools.search;
  - one inbox.request carries several calls and a question as one Inbox item,
    and keeps the canonical tool name for a call that named the published one
    (the request is cancelled at the end).

Inputs (environment):
  BASE             toolyard URL of the deployment under test (required)
  KUBECONFIG/NAMESPACE  for the admin Secret
  ADMIN_FILE       JSON {"username", "password"} instead of the Secret
  EXPECT_RANKED_BY jev or keyword: the ranker the run expects (default: report only)
  REPORT           path of the JSON report (default /tmp/small-tools-e2e.json)

Secrets never appear in argv or output: tokens stay in memory, and the report
holds no token.
"""

import base64
import json
import os
import re
import subprocess
import sys
import time
import uuid

import requests

BASE = os.environ.get("BASE", "").rstrip("/")
if not BASE:
    sys.exit("small-tools-e2e: set BASE to the toolyard URL of the deployment under test")
NS = os.environ.get("NAMESPACE", "toolyard-test")
EXPECT = os.environ.get("EXPECT_RANKED_BY", "")
REPORT = os.environ.get("REPORT", "/tmp/small-tools-e2e.json")
REASON = "five-tool list and tool search end-to-end test on the test deployment (TEC-1807)"
# The five tools as tools/list shows them: OpenAI-safe published names.
FIVE = {"tools_search", "getToolDocs", "executeToolCode", "inbox_request", "inbox_status"}
SAFE_NAME = re.compile(r"^[a-zA-Z0-9_-]{1,64}$")
RUN_FOR_ACCESS = {"allow": "now", "ask": "needs_approval", "deny": "blocked"}

results = []


def check(name, ok, detail=""):
    results.append({"check": name, "ok": bool(ok), "detail": str(detail)[:800]})
    print(("PASS " if ok else "FAIL ") + name + (": " + str(detail)[:400] if detail else ""), flush=True)
    return ok


def step(name, fn):
    try:
        return fn()
    except Exception as e:  # noqa: BLE001 - one failed step must not stop the run
        check(name, False, "%s: %s" % (type(e).__name__, e))
        return None


def kubectl(*args):
    return subprocess.run(["kubectl", "-n", NS, *args], capture_output=True, text=True, timeout=60)


def admin_creds():
    path = os.environ.get("ADMIN_FILE")
    if path:
        return json.load(open(path))
    out = {}
    for k in ("username", "password"):
        p = kubectl("get", "secret", "toolyard-admin", "-o", "jsonpath={.data.%s}" % k)
        out[k] = base64.b64decode(p.stdout).decode()
    return out


class Admin:
    def __init__(self):
        self.s = requests.Session()
        self.s.headers.update({"Origin": BASE, "X-Requested-With": "small-tools-e2e"})
        r = self.s.post(BASE + "/v1/auth/login", json=admin_creds(), timeout=30)
        r.raise_for_status()

    def j(self, method, path, **kw):
        r = self.s.request(method, BASE + path, timeout=kw.pop("timeout", 60), **kw)
        if r.status_code >= 400:
            raise RuntimeError("%s %s -> %s %s" % (method, path, r.status_code, r.text[:300]))
        return r.json() if r.content else {}


class Agent:
    def __init__(self, token):
        self.h = {"Authorization": "Bearer " + token}
        self.sid = None
        self.n = 0

    def rpc(self, method, params=None):
        self.n += 1
        h = dict(self.h, **{"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
                            "MCP-Protocol-Version": "2025-11-25"})
        if self.sid:
            h["Mcp-Session-Id"] = self.sid
        body = {"jsonrpc": "2.0", "id": self.n, "method": method, "params": params or {}}
        r = requests.post(BASE + "/mcp", headers=h, json=body, timeout=120)
        if r.headers.get("Mcp-Session-Id"):
            self.sid = r.headers["Mcp-Session-Id"]
        text = r.text
        if "text/event-stream" in r.headers.get("Content-Type", ""):
            data = [ln[5:].strip() for ln in text.splitlines() if ln.startswith("data:")]
            text = data[-1] if data else "{}"
        return json.loads(text) if text.strip() else {}

    def init(self):
        res = self.rpc("initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                                      "clientInfo": {"name": "small-tools-e2e", "version": "1"}})
        self.rpc("notifications/initialized")
        return res

    def list_names(self):
        names, cursor = [], None
        while True:
            res = self.rpc("tools/list", {"cursor": cursor} if cursor else {})
            out = res.get("result", {})
            names += [t["name"] for t in out.get("tools", [])]
            cursor = out.get("nextCursor")
            if not cursor:
                return names

    def tool(self, name, args):
        args = dict(args)
        args.setdefault("_reason", REASON)
        res = self.rpc("tools/call", {"name": name, "arguments": args})
        if "error" in res:
            raise RuntimeError("%s: %s" % (name, res["error"]))
        out = res.get("result", {})
        if out.get("isError"):
            raise RuntimeError("%s isError: %s" % (name, json.dumps(out)[:400]))
        sc = out.get("structuredContent")
        if sc is not None:
            return sc
        txt = "".join(c.get("text", "") for c in out.get("content", []) if c.get("type") == "text")
        try:
            return json.loads(txt)
        except ValueError:
            return {"text": txt}


def set_tool_list(admin, agent_id, value):
    cur = admin.j("GET", "/v1/me/agent-scopes", params={"agent_id": agent_id}).get("scope") or {}
    cur["tool_list"] = value
    return admin.j("POST", "/v1/me/agent-scopes", json={"agent_id": agent_id, "scope": cur}).get("scope") or {}


def main():
    t0 = time.time()
    h = requests.get(BASE + "/v1/health", timeout=20).json()
    check("health ok", h.get("ok"), h.get("version"))

    admin = Admin()
    ag = admin.j("POST", "/v1/agents", json={"name": "small-tools-e2e-" + uuid.uuid4().hex[:6]})
    agent = Agent(ag["token"])
    agent_id = ag.get("agent_id") or ag.get("id")
    init = agent.init()
    instructions = init.get("result", {}).get("instructions", "")
    report = {"base": BASE, "version": h.get("version")}

    # --- Done means 1: the five-tool list and the full-list switch --------
    def lists():
        small = agent.list_names()
        check("default agent lists exactly the five tools", set(small) == FIVE and len(small) == 5, sorted(small))
        sc = set_tool_list(admin, agent_id, "full")
        full = agent.list_names()
        check("an agent switched to the full list lists every tool it may use",
              sc.get("tool_list") == "full" and len(full) > 5 and FIVE <= set(full),
              {"tool_list": sc.get("tool_list"), "count": len(full)})
        bad = [n for n in small + full if not SAFE_NAME.match(n)]
        check("every listed name matches ^[a-zA-Z0-9_-]{1,64}$ and none repeats",
              not bad and len(set(full)) == len(full), bad[:10] or "%d names" % len(full))
        sc = set_tool_list(admin, agent_id, "small")
        again = agent.list_names()
        check("switching back shows the five again", sc.get("tool_list") in ("small", "", None) and set(again) == FIVE,
              {"tool_list": sc.get("tool_list"), "count": len(again)})
        report["full_count"] = len(full)
    step("tool lists", lists)

    # --- Done means 3 (text): permission and batching guidance -------------
    def guidance():
        low = instructions.lower()
        check("instructions name inbox_request as the way to get permission and ask to batch",
              "inbox_request" in instructions and "batch" in low and "tools_search" in instructions, instructions[:300])
        check("instructions say the dotted names work too", "dotted names work" in low, instructions[:300])
        report["instructions"] = instructions
    step("instructions", guidance)

    # --- Done means 2 and 3: search, ranking and run status ----------------
    def search():
        res = agent.tool("tools_search", {"query": "who am I and what may I use", "limit": 8})
        hits = res.get("results", [])
        names = [x.get("name") for x in hits]
        report["search"] = {k: res.get(k) for k in ("mode", "ranked_by", "fallback", "matched")}
        report["search"]["top"] = [(x.get("name"), x.get("run"), x.get("score")) for x in hits]
        check("search finds access.whoami, which the list does not show", "access.whoami" in names, names)
        mcp_names = {x.get("name"): x.get("mcp_name") for x in hits}
        check("every hit gives its listed name (mcp_name)",
              hits and all(SAFE_NAME.match(v or "") for v in mcp_names.values()) and mcp_names.get("access.whoami") == "access_whoami",
              mcp_names)
        check("search says how it ranked", res.get("ranked_by") in ("jev", "keyword", "regex", "name"),
              report["search"])
        if EXPECT:
            check("search ranked with %s" % EXPECT, res.get("ranked_by") == EXPECT, report["search"])
        runs = [x.get("run") for x in hits]
        check("every hit has a run status", hits and all(r in ("now", "needs_approval", "blocked") for r in runs), runs)
        mism = []
        for x in hits:
            ex = agent.tool("policies.explain", {"tool": x["name"]})
            acc = ex.get("access") or ex.get("effective_access") or (ex.get("effective") or {}).get("access")
            if RUN_FOR_ACCESS.get(acc) != x.get("run"):
                mism.append((x["name"], x.get("run"), acc))
        check("run status agrees with policies.explain", not mism, mism or "%d hits" % len(hits))
    step("search", search)

    # --- Done means 3: run status for ask and deny policies ---------------
    def policy_statuses():
        made = []
        try:
            for target, action in (("memory.set", "ask"), ("Context7.query-docs", "deny")):
                p = admin.j("POST", "/v1/policies", json={"scope": "tool", "target": target, "action": action,
                                                          "note": "small-tools-e2e (TEC-1807), removed at the end of the run"})
                made.append(p.get("id"))
            want = {"memory.set": "needs_approval", "Context7.query-docs": "blocked"}
            got, mism = {}, []
            for name in want:
                res = agent.tool("tools_search", {"query": name.replace(".", " ").replace("-", " "), "limit": 10})
                hit = next((x for x in res.get("results", []) if x.get("name") == name), None)
                got[name] = hit and hit.get("run")
                ex = agent.tool("policies.explain", {"tool": name})
                acc = ex.get("access") or ex.get("effective_access") or (ex.get("effective") or {}).get("access")
                if got[name] != want[name] or RUN_FOR_ACCESS.get(acc) != got[name]:
                    mism.append((name, got[name], acc))
            report["policy_statuses"] = got
            check("an ask policy shows needs_approval and a deny policy shows blocked, as policies.explain says",
                  not mism, mism or got)
        finally:
            for pid in made:
                if pid:
                    admin.j("DELETE", "/v1/policies/%s" % pid)
    step("run status under policies", policy_statuses)

    # --- Done means 4: a tool that is not listed still runs ----------------
    def hidden_calls():
        direct = agent.tool("access.whoami", {})
        check("a tool that is not listed runs through tools/call by its canonical name", bool(direct.get("caller_id")), list(direct)[:6])
        published = agent.tool("access_whoami", {})
        check("the same tool runs by its published name", published.get("caller_id") == direct.get("caller_id"),
              list(published)[:6])
        script = """
const found = await tools.search("who am I", { limit: 3 });
const hit = found.find(x => x.name === "access.whoami") || found[0];
const me = await tools.call(hit.code || hit.name, {});
return { top: found.map(x => [x.name, x.run]), caller: me.caller_id || null };
"""
        out = agent.tool("executeToolCode", {"language": "javascript", "code": script})
        report["code_mode"] = out
        val = out.get("result", out)
        text = out.get("text", "")
        if "Return value:" in text:
            # The text answer: print output, then "Return value: <JSON>",
            # then the environment notes.
            val = text.split("Return value:", 1)[1].split("\n\nEnvironment:", 1)[0]
        if isinstance(val, str):
            try:
                val = json.loads(val)
            except ValueError:
                val = {}
        check("a code-mode script finds the tool with tools.search and runs it",
              isinstance(val, dict) and bool(val.get("caller")), json.dumps(out)[:400])
    step("hidden tools", hidden_calls)

    # --- Done means 3: one batched Inbox item ------------------------------
    def batch():
        key = "small-tools-e2e-" + uuid.uuid4().hex[:6]
        call = {"target": "toolyard test memory", "operation": "write", "expected_effects": "One test memory entry exists.",
                "affected_scope": "One memory key on the test deployment", "material_risks": "None: test data only.",
                "undo": "Delete the memory key.", "tool": "memory.set", "required": False}
        args = {
            "task": {"objective": "Check that one Inbox request carries several calls and a question (TEC-1807 test)."},
            "title": "Test: two memory writes and one question",
            "summary": "A test request; it is cancelled at the end of the run.",
            "message": "This is an automated test of batched permission requests. Nothing needs a decision.",
            "facts": {"why_now": "End-to-end test of the five-tool list.", "if_it_goes_wrong": "Test data only.",
                      "undo": "The test cancels the request."},
            "urgency": "fyi",
            "schema_version": 3,
            "tools": [dict(call, call_id="write_a", summary="Write test key A.",
                           params={"scope": "test", "key": key + "-a", "value": "a"}),
                      # The published name: the Inbox keeps the canonical one.
                      dict(call, call_id="write_b", tool="memory_set", summary="Write test key B.",
                           params={"scope": "test", "key": key + "-b", "value": "b"})],
            "questions": [{"id": "q1", "type": "single_choice", "prompt": "Keep the test entries?",
                           "options": [{"id": "no", "label": "No"}, {"id": "yes", "label": "Yes"}]}],
        }
        res = agent.tool("inbox_request", args)
        rid = res.get("request_id")
        report["inbox_request"] = {k: res.get(k) for k in ("ok", "request_id", "problems", "preview")}
        if not check("one inbox.request with two calls and a question is accepted", res.get("ok") and rid, report["inbox_request"]):
            return
        pv = res.get("preview") or {}
        check("the one request is one Inbox card with both calls and the question",
              pv.get("tools") == 2 and pv.get("questions") == 1, pv)
        st = agent.tool("inbox_status", {"ids": [rid]})
        reqs = st.get("requests", [])
        tools_n = len(reqs[0].get("tools", [])) if reqs else 0
        check("inbox.status shows one item with both calls", len(reqs) == 1 and tools_n == 2,
              {"items": len(reqs), "calls": tools_n})
        kept = [x.get("tool") for x in (reqs[0].get("tools", []) if reqs else [])]
        check("the request keeps the canonical name for a call that named memory_set", kept == ["memory.set", "memory.set"], kept)
        report["inbox_request_id"] = rid
        if os.environ.get("KEEP_INBOX") != "1":
            agent.tool("inbox.cancel", {"request_id": rid})
    step("batched inbox request", batch)

    if agent_id:
        try:
            admin.j("DELETE", "/v1/agents/%s" % agent_id)
        except Exception:  # noqa: BLE001 - cleanup only
            pass

    passed = sum(1 for r in results if r["ok"])
    print("\n%d/%d checks passed in %.0f s" % (passed, len(results), time.time() - t0))
    report["results"] = results
    json.dump(report, open(REPORT, "w"), indent=1)
    os.chmod(REPORT, 0o600)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())

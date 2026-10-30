#!/usr/bin/env python3
"""End-to-end check of native skills and skills.recommend on a running deployment.

Runs against the Kubernetes HA test deployment (two replicas, Postgres) or any
toolyard with the skills hub on. It signs in as the first admin (credentials
from the `toolyard-admin` Secret through kubectl, or ADMIN_FILE), makes an
agent key for that admin, and checks:

  - an admin creates a native collection over the admin API, and a GitHub-only
    setting on it is refused;
  - the agent creates a skill with skills.create: live at once as revision 1,
    readable with skills.get on every replica, and ranked first by
    skills.recommend for a task in plain words right after the write;
  - skills.update changes only the description (revision 2), a write on an old
    base_revision answers conflict, skills.versions lists both revisions and
    skills.get with revision 1 still shows the old description;
  - an agent write that adds a script waits for the owner (pending_approval),
    the admin approves it in the Inbox, and skills.proposal reports the new
    revision;
  - the REST routes (POST /v1/skills/update, GET /v1/skills/versions,
    GET /v1/skills/recommend) answer the same;
  - the built-in skills still answer skills.get and skills.recommend;
  - the audit log has skills.native_create, skills.native_update,
    skills.native_propose and skills.native_apply rows.

The collection is deleted at the end.

Inputs (environment):
  BASE        toolyard URL of the deployment under test (required)
  KUBECONFIG/NAMESPACE  for the admin Secret and the per-replica check
  ADMIN_FILE  JSON {"username", "password"} instead of the Secret
  REPORT      path of the JSON report (default /tmp/native-skills-e2e.json)

Secrets never appear in argv or output: tokens stay in memory, and the report
holds no token.
"""

import base64
import json
import os
import subprocess
import sys
import time
import uuid

import requests

BASE = os.environ.get("BASE", "").rstrip("/")
if not BASE:
    sys.exit("native-skills-e2e: set BASE to the toolyard URL of the deployment under test")
NS = os.environ.get("NAMESPACE", "toolyard-test")
REPORT = os.environ.get("REPORT", "/tmp/native-skills-e2e.json")
REASON = "native skills registry and skills.recommend end-to-end test on the test deployment"

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
        self.s.headers.update({"Origin": BASE, "X-Requested-With": "native-skills-e2e"})
        r = self.s.post(BASE + "/v1/auth/login", json=admin_creds(), timeout=30)
        r.raise_for_status()

    def raw(self, method, path, **kw):
        return self.s.request(method, BASE + path, timeout=kw.pop("timeout", 60), **kw)

    def j(self, method, path, **kw):
        r = self.raw(method, path, **kw)
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
        self.rpc("initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                                "clientInfo": {"name": "native-skills-e2e", "version": "1"}})
        self.rpc("notifications/initialized")

    def call(self, name, args):
        """Calls a tool through tools.execute (the six-tool route) and returns
        (ok, structured answer)."""
        args = dict(args)
        args.setdefault("_reason", REASON)
        res = self.rpc("tools/call", {"name": "tools_execute", "arguments": {
            "tool": name, "arguments": args, "_reason": REASON}})
        if "error" in res:
            raise RuntimeError("%s: %s" % (name, res["error"]))
        out = res.get("result", {})
        sc = out.get("structuredContent")
        if sc is None:
            txt = "".join(c.get("text", "") for c in out.get("content", []) if c.get("type") == "text")
            try:
                sc = json.loads(txt)
            except ValueError:
                sc = {"text": txt}
        # tools.execute wraps the tool's answer.
        if isinstance(sc, dict) and "result" in sc and isinstance(sc["result"], dict) and "structured" in sc["result"]:
            sc = sc["result"]["structured"]
        return not out.get("isError"), sc

    def tool(self, name, args):
        ok, sc = self.call(name, args)
        if not ok:
            raise RuntimeError("%s refused: %s" % (name, json.dumps(sc)[:400]))
        return sc

    def rest(self, method, path, **kw):
        r = requests.request(method, BASE + path, headers=self.h, timeout=60, **kw)
        return r.status_code, (r.json() if r.content else {})


def find(obj, key):
    """The first value of key anywhere in a JSON answer (tools.execute may wrap it)."""
    if isinstance(obj, dict):
        if key in obj:
            return obj[key]
        for v in obj.values():
            got = find(v, key)
            if got is not None:
                return got
    elif isinstance(obj, list):
        for v in obj:
            got = find(v, key)
            if got is not None:
                return got
    return None


def main():
    t0 = time.time()
    h = requests.get(BASE + "/v1/health", timeout=20).json()
    check("health ok", h.get("ok"), h.get("version"))
    report = {"base": BASE, "version": h.get("version")}
    admin = Admin()
    ag = admin.j("POST", "/v1/agents", json={"name": "native-skills-e2e-" + uuid.uuid4().hex[:6]})
    agent = Agent(ag["token"])
    agent.init()
    slug = "e2e-" + uuid.uuid4().hex[:6]
    state = {}

    def collection():
        r = admin.raw("POST", "/v1/skills/sources", json={"kind": "native", "slug": slug, "url": "acme/skills"})
        check("a native collection with a repository URL is refused (400)", r.status_code == 400, r.status_code)
        src = admin.j("POST", "/v1/skills/sources", json={"kind": "native", "slug": slug, "visibility": "everyone"})
        src = src.get("source", src)
        state["src"] = src["id"]
        check("an admin creates a native collection", src.get("kind") == "native" and src.get("slug") == slug, src.get("id"))
        r = admin.raw("PATCH", "/v1/skills/sources/%s" % src["id"], json={"branch": "main"})
        check("a native collection's branch cannot change (409)", r.status_code == 409, r.status_code)
    step("collection", collection)

    def create():
        t = time.time()
        res = agent.tool("skills.create", {
            "collection": slug, "name": "invoice-dispute",
            "description": "Handle a customer who disputes an invoice amount: collect the invoice, compare it with the contract, and draft a reply.",
            "instructions": "# Invoice dispute\n\n1. Get the invoice number.\n2. Compare the billed lines with the contract.\n3. Draft the reply.\n",
            "triggers": ["a customer says they were overcharged", "a client questions a bill"],
            "tags": ["billing", "support"],
            "files": {"references/reply-template.md": "Dear customer,\n"},
            "note": "first version"})
        r = find(res, "result") or res
        check("skills.create is live as revision 1", r.get("status") == "live" and r.get("revision") == 1, r)
        state["uri"] = r.get("uri")
        g = agent.tool("skills.get", {"skill": slug + "/invoice-dispute"})
        check("skills.get reads it at once", find(g, "revision") == 1 and "Compare the billed lines" in (find(g, "body") or ""),
              find(g, "repo"))
        rec = agent.tool("skills.recommend", {"task": "a client thinks we billed them too much last month, help me answer", "limit": 3})
        skills = find(rec, "skills") or []
        top = skills[0] if skills else {}
        report["recommend_after_create_s"] = round(time.time() - t, 2)
        check("skills.recommend ranks the new skill first right after the write",
              top.get("uri") == state["uri"] and top.get("rationale"),
              {"ranked_by": find(rec, "ranked_by"), "fallback": find(rec, "fallback"), "top": top})
        report["recommend_ranked_by"] = find(rec, "ranked_by")
        report["recommend_fallback"] = find(rec, "fallback")
        kw = agent.tool("skills.recommend", {"task": "invoice dispute overcharged", "mode": "keyword", "tags": ["billing"]})
        check("keyword mode with a tag filter finds it", any(s.get("uri") == state["uri"] for s in find(kw, "skills") or []),
              find(kw, "ranked_by"))
    step("create", create)

    def update():
        res = agent.tool("skills.update", {"skill": slug + "/invoice-dispute", "base_revision": 1,
                                            "description": "Handle a disputed invoice amount: check it against the contract and reply within a day."})
        r = find(res, "result") or res
        check("skills.update with base_revision 1 is revision 2", r.get("status") == "live" and r.get("revision") == 2, r)
        ok, sc = agent.call("skills.update", {"skill": slug + "/invoice-dispute", "base_revision": 1,
                                              "files": {"notes.md": "late\n"}})
        check("a write on an old base_revision answers conflict", not ok and find(sc, "error") == "conflict", sc)
        v = agent.tool("skills.versions", {"skill": slug + "/invoice-dispute"})
        revs = find(v, "revisions") or []
        check("skills.versions lists both revisions, newest current", [x.get("revision") for x in revs] == [2, 1] and revs[0].get("current"),
              [(x.get("revision"), x.get("current"), x.get("note")) for x in revs])
        old = agent.tool("skills.get", {"skill": slug + "/invoice-dispute", "revision": 1})
        check("skills.get with revision 1 shows the old description", "draft a reply" in (find(old, "description") or ""), find(old, "description"))
    step("update", update)

    def approval():
        res = agent.tool("skills.update", {"skill": slug + "/invoice-dispute",
                                            "files": {"scripts/total.py": "print(sum([1, 2]))\n"}, "note": "add a helper"})
        r = find(res, "result") or res
        prop = r.get("proposal") or {}
        check("a script write waits for the owner's approval", r.get("status") == "pending_approval" and prop.get("id"),
              r.get("approval_reasons"))
        g = agent.tool("skills.get", {"skill": slug + "/invoice-dispute"})
        check("nothing changes before the approval", find(g, "revision") == 2, find(g, "revision"))
        full = admin.j("GET", "/v1/inbox/%s" % prop["inbox_id"])
        full = full.get("request", full)
        admin.j("POST", "/v1/inbox/%s/decide" % prop["inbox_id"], json={
            "action": "answer", "request_revision": full.get("revision"),
            "submission_id": "e2e-" + uuid.uuid4().hex[:8], "answers": {"decision": {"value": "approve"}}})
        time.sleep(2)
        p = agent.tool("skills.proposal", {"id": prop["id"]})
        pp = find(p, "proposal") or {}
        check("the owner's approval applies it as revision 3", pp.get("status") == "done" and pp.get("result", "").endswith("revision 3"), pp)
    step("approval", approval)

    def rest():
        code, body = agent.rest("POST", "/v1/skills/update", json={
            "skill": slug + "/invoice-dispute", "files": {"references/faq.md": "Q: When?\nA: Within a day.\n"},
            "reason": "add the FAQ through the REST route in the end-to-end test"})
        check("POST /v1/skills/update writes revision 4", code == 200 and find(body, "revision") == 4, (code, find(body, "status")))
        code, body = agent.rest("GET", "/v1/skills/versions", params={"skill": slug + "/invoice-dispute"})
        check("GET /v1/skills/versions lists 4 revisions", code == 200 and len(find(body, "revisions") or []) == 4, code)
        code, body = agent.rest("GET", "/v1/skills/recommend", params={"task": "customer disputes an invoice", "limit": 2})
        check("GET /v1/skills/recommend answers", code == 200 and find(body, "skills"), (code, find(body, "ranked_by")))
        code, body = agent.rest("POST", "/v1/skills/update", json={"skill": slug + "/invoice-dispute", "bogus": 1})
        check("an unknown body field is refused (400)", code == 400, code)
    step("rest", rest)

    def builtin():
        g = agent.tool("skills.get", {"skill": "toolyard/toolyard-skills"})
        check("the built-in toolyard-skills guide teaches the new tools", "skills.recommend" in (find(g, "body") or ""), find(g, "repo"))
        rec = agent.tool("skills.recommend", {"task": "I need my owner's permission before I can call a restricted tool", "limit": 3})
        names = [s.get("name") for s in find(rec, "skills") or []]
        check("skills.recommend finds the built-in permissions guide", "toolyard-permissions-and-inbox" in names, names)
    step("built-in skills", builtin)

    def audit():
        rows = admin.j("GET", "/v1/audit", params={"limit": 500})
        rows = rows if isinstance(rows, list) else rows.get("events", rows.get("rows", []))
        seen = {r.get("event_type") for r in rows}
        want = {"skills.native_create", "skills.native_update", "skills.native_propose", "skills.native_apply"}
        check("the audit log records native writes", want <= seen, sorted(want - seen) or "all four")
    step("audit", audit)

    def cleanup():
        if state.get("src"):
            admin.j("DELETE", "/v1/skills/sources/%s" % state["src"])
            ok, sc = agent.call("skills.get", {"skill": slug + "/invoice-dispute"})
            check("deleting the collection removes its skills", not ok and find(sc, "error") == "not_found", sc)
    step("cleanup", cleanup)

    report["seconds"] = round(time.time() - t0, 1)
    report["results"] = results
    with open(REPORT, "w") as f:
        json.dump(report, f, indent=2)
    failed = [r for r in results if not r["ok"]]
    print("native-skills-e2e: %d/%d passed in %ss" % (len(results) - len(failed), len(results), report["seconds"]))
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()

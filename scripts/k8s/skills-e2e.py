#!/usr/bin/env python3
"""End-to-end check of the toolyard skills hub on a running deployment.

Runs against the Kubernetes HA test deployment (two replicas, Postgres) or any
toolyard with the hub on. It signs in as the first admin (credentials from the
`toolyard-admin` Secret through kubectl, or ADMIN_FILE), makes an agent key,
adds skill sources, and checks every surface: admin REST, MCP tools, the MCP
Skills extension (SEP-2640), code mode (Starlark and JavaScript), agent REST,
bundles, Auto and Review updates (Inbox approval), rollback, agent source
proposals, the webhook, and that both replicas serve the same version.

Inputs (environment):
  BASE                 toolyard URL of the deployment under test (required)
  KUBECONFIG/NAMESPACE  for the admin Secret and the per-replica checks
  ADMIN_FILE           JSON {"username", "password"} instead of the Secret
  FIXTURE_REPO         owner/repo of the fixture branch (default iamtushar324/toolyard)
  FIXTURE_BRANCH       default t3/skills-fixture (folder "skills"): an orphan
                       branch of this repository that holds the test skills;
                       keep it, the checks depend on it
  TRUSTED_OWNERS       skills_trusted_github_owners for the run (default: the
                       fixture repository's owner)
  FIXTURE_WORKTREE     a local checkout of FIXTURE_BRANCH; when set, the Auto
                       and Review update checks push commits to it
  MEMBER_FILE          JSON {"username", "password"} of a second, non-admin
                       account; turns on the sharing, contributor and owner
                       transfer checks
  PRIVATE_URL          optional private repository URL (e.g. a bk-docs folder)
  PRIVATE_TOKEN_FILE   file with a read-only token for PRIVATE_URL
  REPORT               path of the JSON report (default /tmp/skills-e2e.json)

Secrets never appear in argv or output: tokens stay in memory or in 0600
files, and the report redacts them.
"""

import base64
import hashlib
import hmac
import io
import json
import os
import socket
import subprocess
import sys
import tarfile
import time
import uuid

import requests

BASE = os.environ.get("BASE", "").rstrip("/")
if not BASE:
    sys.exit("skills-e2e: set BASE to the toolyard URL of the deployment under test")
NS = os.environ.get("NAMESPACE", "toolyard-test")
FIXTURE_REPO = os.environ.get("FIXTURE_REPO", "iamtushar324/toolyard")
FIXTURE_BRANCH = os.environ.get("FIXTURE_BRANCH", "t3/skills-fixture")
FIXTURE_WT = os.environ.get("FIXTURE_WORKTREE", "")
REPORT = os.environ.get("REPORT", "/tmp/skills-e2e.json")
REASON = "skills hub end-to-end test on the test deployment (TEC-1782)"

results = []


def check(name, ok, detail=""):
    results.append({"check": name, "ok": bool(ok), "detail": str(detail)[:600]})
    print(("PASS " if ok else "FAIL ") + name + (": " + str(detail)[:300] if detail else ""), flush=True)
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
        self.s.headers.update({"Origin": BASE, "X-Requested-With": "skills-e2e"})
        r = self.s.post(BASE + "/v1/auth/login", json=admin_creds(), timeout=30)
        r.raise_for_status()

    def req(self, method, path, **kw):
        r = self.s.request(method, BASE + path, timeout=kw.pop("timeout", 120), **kw)
        return r

    def j(self, method, path, **kw):
        r = self.req(method, path, **kw)
        if r.status_code >= 400:
            raise RuntimeError("%s %s -> %s %s" % (method, path, r.status_code, r.text[:300]))
        return r.json() if r.content else {}


class Agent:
    def __init__(self, token):
        self.token = token
        self.h = {"Authorization": "Bearer " + token}
        self.sid = None
        self.n = 0

    def rpc(self, method, params=None, proto="2025-11-25"):
        self.n += 1
        h = dict(self.h, **{"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
                            "MCP-Protocol-Version": proto})
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
                                      "clientInfo": {"name": "skills-e2e", "version": "1"}})
        self.rpc("notifications/initialized")
        return res

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

    def rest(self, path, **params):
        params.setdefault("reason", REASON)
        r = requests.get(BASE + path, headers=self.h, params=params, timeout=60)
        return r


def git(*args):
    p = subprocess.run(["git", "-C", FIXTURE_WT, *args], capture_output=True, text=True, timeout=120)
    if p.returncode != 0:
        raise RuntimeError("git %s: %s" % (" ".join(args), p.stderr[-300:]))
    return p.stdout.strip()


def fixture_commit(desc):
    """Change release-notes' description on the fixture branch and push."""
    path = os.path.join(FIXTURE_WT, "skills/release-notes/SKILL.md")
    text = open(path).read().splitlines()
    for i, ln in enumerate(text):
        if ln.startswith("description:"):
            text[i] = "description: " + desc
    open(path, "w").write("\n".join(text) + "\n")
    git("add", "skills/release-notes/SKILL.md")
    git("commit", "-q", "-m", "skills hub e2e: " + desc[:60] + "\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>")
    git("push", "-q", "origin", "HEAD:" + FIXTURE_BRANCH)
    return git("rev-parse", "HEAD")


def admin_sources(admin):
    lst = admin.j("GET", "/v1/skills/admin/sources")
    return (lst.get("sources", lst) if isinstance(lst, dict) else lst) or []


def find_source(admin, slug):
    for s in admin_sources(admin):
        if s.get("slug") == slug:
            return s
    return None


def sync_until(admin, sid, sha, timeout=100):
    """Sync until the source sees sha: GitHub may serve a public branch head
    up to 60 s old (Cache-Control: public, max-age=60)."""
    deadline = time.time() + timeout
    while True:
        r = sync(admin, sid)
        if r.get("sha") == sha or time.time() > deadline:
            return r
        time.sleep(5)


def ensure_source(admin, slug, body):
    s = find_source(admin, slug)
    if s:
        return s, False
    body = dict(body, slug=slug)
    s = admin.j("POST", "/v1/skills/sources", json=body)
    return s.get("source", s), True


def sync(admin, sid):
    return admin.j("POST", "/v1/skills/sources/%s/sync" % sid, timeout=600)


def pod_names():
    """The gateway pods that serve: ready, and not on their way out after a
    rollout (a terminating pod still reports phase Running)."""
    p = kubectl("get", "pods", "-l", "app.kubernetes.io/name=toolyard", "-o", "json")
    names = []
    for pod in json.loads(p.stdout or "{}").get("items", []):
        if pod["metadata"].get("deletionTimestamp"):
            continue
        conds = {c["type"]: c["status"] for c in pod.get("status", {}).get("conditions", [])}
        if conds.get("Ready") == "True":
            names.append(pod["metadata"]["name"])
    return names


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def main():
    t0 = time.time()
    h = requests.get(BASE + "/v1/health", timeout=20).json()
    check("health ok", h.get("ok"), h.get("version"))

    admin = Admin()
    check("admin signs in", True)

    # Our own GitHub owners sync in Auto mode; anthropics stays untrusted.
    admin.j("PATCH", "/v1/settings", json={"skills_trusted_github_owners": os.environ.get(
        "TRUSTED_OWNERS", FIXTURE_REPO.split("/")[0])})
    # A source left by an earlier run's proposal check.
    for leftover in admin_sources(admin):
        if leftover.get("path") == "skills/incident-triage":
            admin.req("DELETE", "/v1/skills/sources/%s" % leftover["id"])

    # An agent key owned by the admin (a fresh one each run, deleted at the end).
    name = "skills-e2e-" + uuid.uuid4().hex[:6]
    ag = admin.j("POST", "/v1/agents", json={"name": name})
    agent = Agent(ag["token"])
    agent_id = ag.get("agent_id") or ag.get("id")

    # --- Admin: inspect and preview the fixture -------------------------
    fx_url = "https://github.com/%s/tree/%s/skills" % (FIXTURE_REPO, FIXTURE_BRANCH)

    def inspect():
        info = admin.j("POST", "/v1/skills/sources/inspect", json={"url": fx_url})
        ok = info.get("branch") == FIXTURE_BRANCH and info.get("path") == "skills" and FIXTURE_BRANCH in info.get("branches", [])
        check("inspect parses /tree/<branch>/<folder> and lists branches", ok,
              {k: info.get(k) for k in ("owner", "repo", "branch", "path", "default_branch", "private")})
    step("inspect", inspect)

    def preview():
        pv = admin.j("POST", "/v1/skills/sources/preview", json={"url": fx_url})
        codes = sorted({w.get("code") for w in pv.get("warnings", [])})
        check("preview finds 4 skills and a name_mismatch warning", pv.get("skill_count") == 4 and "name_mismatch" in codes,
              {"skills": pv.get("skill_count"), "codes": codes, "sha": pv.get("sha", "")[:12]})
    step("preview", preview)

    # --- Admin: add the fixture (auto) and anthropics/skills (review) ----
    fx, created = ensure_source(admin, "fixture", {"url": fx_url, "mode": "auto", "visibility": "everyone",
                                                   "default_runs": "anywhere"})
    if not created and fx.get("mode") != "auto":
        admin.j("PATCH", "/v1/skills/sources/%s" % fx["id"], json={"mode": "auto"})
    sr = sync(admin, fx["id"])
    fx = admin.j("GET", "/v1/skills/sources/%s" % fx["id"])
    fx = fx.get("source", fx)
    check("fixture source syncs and is active", bool(fx.get("active_sha")), {"sha": fx.get("active_sha", "")[:12],
                                                                          "skills": fx.get("skill_count"), "sync": sr})
    fx_sha = fx.get("active_sha", "")

    def anthropic():
        an, _ = ensure_source(admin, "anthropic", {"url": "anthropics/skills", "visibility": "everyone"})
        check("untrusted owner defaults to review mode", an.get("mode") == "review", an.get("mode"))
        r = sync(admin, an["id"])
        an = admin.j("GET", "/v1/skills/sources/%s" % an["id"])
        an = an.get("source", an)
        if an.get("pending_version_id"):
            admin.j("POST", "/v1/skills/sources/%s/versions/%s/activate" % (an["id"], an["pending_version_id"]))
            an = admin.j("GET", "/v1/skills/sources/%s" % an["id"])
            an = an.get("source", an)
        check("anthropics/skills is live after approval", bool(an.get("active_sha")) and an.get("skill_count", 0) >= 10,
              {"skills": an.get("skill_count"), "sha": an.get("active_sha", "")[:12], "sync": r})
    step("anthropics/skills", anthropic)

    if os.environ.get("PRIVATE_URL") and os.environ.get("PRIVATE_TOKEN_FILE"):
        def private():
            tok = open(os.environ["PRIVATE_TOKEN_FILE"]).read().strip()
            info = admin.j("POST", "/v1/skills/sources/inspect", json={"url": os.environ["PRIVATE_URL"]})
            check("private repository without a token says needs_token", info.get("needs_token") is True)
            pv, _ = ensure_source(admin, "bk-docs", {"url": os.environ["PRIVATE_URL"], "token": tok, "mode": "auto",
                                                     "visibility": "everyone", "default_runs": "host"})
            body = json.dumps(pv)
            check("the token never comes back", tok not in body and "token\": \"" not in body)
            r = sync(admin, pv["id"])
            pv = admin.j("GET", "/v1/skills/sources/%s" % pv["id"])
            pv = pv.get("source", pv)
            check("private source syncs", bool(pv.get("active_sha")), {"skills": pv.get("skill_count"), "sync": r})
        step("private source", private)

    # --- MCP: capability, tools ---------------------------------------
    init = agent.init()
    caps = init.get("result", {}).get("capabilities", {})
    check("initialize declares io.modelcontextprotocol/skills", "io.modelcontextprotocol/skills" in caps.get("extensions", {}),
          caps.get("extensions"))

    def mcp_tools():
        res = agent.tool("skills.search", {"query": "release notes", "source": "fixture"})
        hits = res.get("skills", res.get("results", []))
        top = hits[0] if hits else {}
        check("skills.search finds release-notes with source and commit",
              top.get("name") == "release-notes" and top.get("commit") == fx_sha and top.get("repo", "").startswith(FIXTURE_REPO),
              {k: top.get(k) for k in ("name", "uri", "repo", "commit")})
        g = agent.tool("skills.get", {"skill": "fixture/release-notes"})
        files = g.get("files", [])
        style = [f for f in files if f.get("path") == "references/STYLE.md"]
        check("skills.get returns body, frontmatter and files with sha256 digests",
              "Release notes" in g.get("body", "") and style and style[0].get("digest", "").startswith("sha256:"),
              {"files": [f.get("path") for f in files]})
        rd = agent.tool("skills.read", {"uri": style[0]["uri"]})
        digest = "sha256:" + hashlib.sha256(rd.get("content", "").encode()).hexdigest()
        check("skills.read content matches its digest", digest == style[0]["digest"], digest[:20])
        gr = agent.tool("skills.grep", {"pattern": "on-call", "source": "fixture", "ignore_case": True})
        hits = gr.get("hits", gr.get("matches", []))
        check("skills.grep finds the nested skill text", any("page-oncall" in (x.get("uri", "")) for x in hits), len(hits))
        nest = agent.tool("skills.get", {"skill": "fixture/incident-triage/skills/page-oncall"})
        check("a nested skill is its own entry", nest.get("name") == "page-oncall", nest.get("uri"))
        hs = agent.tool("skills.search", {"query": "browser screenshot", "source": "fixture"})
        names = [x.get("name") for x in hs.get("skills", hs.get("results", []))]
        check("host-only skill hidden from a non-host agent", "host-browser" not in names, names)
        hs2 = agent.tool("skills.search", {"query": "browser screenshot", "source": "fixture", "include_host": True})
        names2 = [x.get("name") for x in hs2.get("skills", hs2.get("results", []))]
        check("include_host shows it", "host-browser" in names2, names2)
        src = agent.tool("skills.sources", {})
        slugs = [x.get("slug") for x in src.get("sources", [])]
        check("skills.sources lists the sources", "fixture" in slugs, slugs)
        ts = agent.tool("tools.search", {"query": "incident triage"})
        check("tools.search names matching skills", any(x.get("name") == "incident-triage" for x in ts.get("skills", [])),
              [x.get("name") for x in ts.get("skills", [])])
    step("MCP tools", mcp_tools)

    def sep2640():
        lst = agent.rpc("skills/list", {})
        entries = lst.get("result", {}).get("skills", [])
        fxe = [e for e in entries if e.get("uri", "").startswith("skill://fixture/")]
        check("skills/list returns entries with frontmatter and resources",
              fxe and all("frontmatter" in e and isinstance(e.get("resources"), list) for e in fxe), len(fxe))
        uri = "skill://fixture/release-notes/SKILL.md"
        got = agent.rpc("skills/get", {"uri": uri}).get("result", {}).get("skill", {})
        check("skills/get returns the entry", got.get("uri") == uri and got.get("frontmatter", {}).get("name") == "release-notes")
        res = agent.rpc("resources/read", {"uri": uri}).get("result", {}).get("contents", [{}])[0]
        dg = [r for r in got.get("resources", []) if r.get("uri") == uri]
        ok = dg and "sha256:" + hashlib.sha256(res.get("text", "").encode()).hexdigest() == dg[0].get("digest")
        check("resources/read of skill:// matches the listed digest", ok, res.get("mimeType"))
        d = agent.rpc("resources/directory/read", {"uri": "skill://fixture/release-notes"}).get("result", {})
        check("resources/directory/read lists the folder", any("references" in json.dumps(x) for x in d.get("resources", d.get("entries", [])) or [d]), json.dumps(d)[:200])
        bad = agent.rpc("skills/get", {"uri": "skill://fixture/nope/SKILL.md"})
        check("an unknown skill URI is -32602", bad.get("error", {}).get("code") == -32602, bad.get("error"))
    step("SEP-2640", sep2640)

    def codemode():
        r = agent.tool("executeToolCode", {"code": 'result = toolyard.skills_search(query="incident triage", source="fixture")'})
        check("Starlark code mode calls skills_search", "incident-triage" in json.dumps(r), json.dumps(r)[:200])
        r = agent.tool("executeToolCode", {"language": "javascript",
                                          "code": 'const r = await tools.toolyard.skills_get({skill: "fixture/release-notes"}); return r.name;'})
        check("JavaScript code mode calls skills_get", "release-notes" in json.dumps(r), json.dumps(r)[:200])
    step("code mode", codemode)

    def rest():
        r = agent.rest("/v1/skills/search", query="release notes", source="fixture")
        check("REST search", r.status_code == 200 and "release-notes" in r.text, r.status_code)
        r = agent.rest("/v1/skills/get", skill="fixture/release-notes")
        check("REST get carries the commit", r.status_code == 200 and fx_sha in r.text, r.status_code)
        r = requests.get(BASE + "/v1/skills/search", params={"query": "x"}, timeout=20)
        check("REST without a token is refused", r.status_code in (401, 403), r.status_code)
    step("agent REST", rest)

    def bundle():
        b = agent.tool("skills.bundle", {"skills": ["fixture/release-notes", "fixture/incident-triage"]})
        url = b.get("url") or b.get("download_url")
        r = requests.get(url, timeout=60)
        names = tarfile.open(fileobj=io.BytesIO(r.content), mode="r:gz").getnames() if r.ok else []
        check("bundle downloads a tar.gz in the Agent Skills layout", any(n.endswith("release-notes/SKILL.md") for n in names), names[:8])
        r2 = requests.get(url, timeout=30)
        check("a bundle URL works once", r2.status_code in (404, 410), r2.status_code)
    step("bundle", bundle)

    # --- HA: both replicas serve the same commit ----------------------
    def per_replica(expect_sha, label):
        shas = {}
        for pod in pod_names():
            port = free_port()
            pf = subprocess.Popen(["kubectl", "-n", NS, "port-forward", "pod/" + pod, "%d:8787" % port],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(40):
                    try:
                        socket.create_connection(("127.0.0.1", port), 1).close()
                        break
                    except OSError:
                        time.sleep(0.25)
                deadline = time.time() + 20
                while True:
                    try:
                        r = requests.get("http://127.0.0.1:%d/v1/skills/get" % port, headers=agent.h,
                                         params={"skill": "fixture/release-notes", "reason": REASON}, timeout=20)
                        sha = r.json().get("commit") if r.ok else "HTTP %s" % r.status_code
                    except requests.ConnectionError as e:  # port-forward not ready yet
                        sha = "connection: %s" % type(e).__name__
                    if sha == expect_sha or time.time() > deadline:
                        break
                    time.sleep(1)
                shas[pod] = sha
            finally:
                pf.terminate()
        check("both replicas serve %s (%s)" % (expect_sha[:12], label), len(shas) >= 2 and all(v == expect_sha for v in shas.values()),
              {k[-5:]: (v or "")[:12] for k, v in shas.items()})
    step("replicas", lambda: per_replica(fx_sha, "after first sync"))

    # --- Auto update, Review update with an Inbox approval, rollback ----
    if FIXTURE_WT:
        def auto_update():
            new = fixture_commit("Write release notes from merged pull requests (auto update %s)." % time.strftime("%H:%M:%S"))
            # A signed push webhook names the exact commit; then make sure.
            sec = admin.j("POST", "/v1/skills/sources/%s/webhook-secret" % fx["id"]).get("secret", "")
            body = json.dumps({"ref": "refs/heads/" + FIXTURE_BRANCH, "after": new}).encode()
            sig = "sha256=" + hmac.new(sec.encode(), body, hashlib.sha256).hexdigest()
            t_push = time.time()
            requests.post(BASE + "/v1/skills/sources/%s/webhook" % fx["id"], data=body, timeout=20, headers={
                "Content-Type": "application/json", "X-GitHub-Event": "push", "X-Hub-Signature-256": sig})
            seen = None
            while time.time() - t_push < 30:
                seen = agent.tool("skills.get", {"skill": "fixture/release-notes"}).get("commit")
                if seen == new:
                    break
                time.sleep(1)
            check("webhook push reaches agents within 30 s", seen == new, "%.0f s" % (time.time() - t_push))
            sync_until(admin, fx["id"], new)
            g = agent.tool("skills.get", {"skill": "fixture/release-notes"})
            check("Auto mode: a new commit reaches agents", g.get("commit") == new and "auto update" in g.get("frontmatter", {}).get("description", ""),
                  g.get("commit", "")[:12])
            per_replica(new, "after auto update")
            return new
        auto_sha = step("auto update", auto_update)

        def review_update():
            admin.j("PATCH", "/v1/skills/sources/%s" % fx["id"], json={"mode": "review"})
            new = fixture_commit("Write release notes from merged pull requests (review update %s)." % time.strftime("%H:%M:%S"))
            r = sync_until(admin, fx["id"], new)
            g = agent.tool("skills.get", {"skill": "fixture/release-notes"})
            check("Review mode: the new commit waits", g.get("commit") == auto_sha and r.get("staged"), {"served": g.get("commit", "")[:12], "sync": r})
            items = admin.j("GET", "/v1/inbox").get("requests", [])
            item = next((i for i in items if "live" in json.dumps(i).lower() and new[:7] in json.dumps(i)), None)
            check("Review mode: an Inbox question waits for the creator", item is not None, (item or {}).get("id"))
            full = admin.j("GET", "/v1/inbox/%s" % item["id"])
            full = full.get("request", full)
            admin.j("POST", "/v1/inbox/%s/decide" % item["id"], json={
                "action": "answer", "request_revision": full.get("revision"),
                "submission_id": "e2e-" + uuid.uuid4().hex[:8], "answers": {"decision": {"value": "approve"}}})
            time.sleep(2)
            g = agent.tool("skills.get", {"skill": "fixture/release-notes"})
            check("Review mode: the Inbox approval makes it live", g.get("commit") == new, g.get("commit", "")[:12])
            per_replica(new, "after review approval")
            return new
        step("review update", review_update)

        def rollback():
            vs = admin.j("GET", "/v1/skills/sources/%s/versions" % fx["id"])
            vs = vs.get("versions", vs)
            prev = next((v for v in vs if v.get("sha") == auto_sha), None)
            admin.j("POST", "/v1/skills/sources/%s/versions/%s/activate" % (fx["id"], prev["id"]))
            g = agent.tool("skills.get", {"skill": "fixture/release-notes"})
            check("rollback serves the older commit", g.get("commit") == auto_sha, g.get("commit", "")[:12])
            admin.j("PATCH", "/v1/skills/sources/%s" % fx["id"], json={"mode": "auto"})
        step("rollback", rollback)

    # --- Webhook ------------------------------------------------------------
    def webhook():
        sec = admin.j("POST", "/v1/skills/sources/%s/webhook-secret" % fx["id"])
        secret = sec.get("secret", "")
        body = json.dumps({"ref": "refs/heads/" + FIXTURE_BRANCH, "after": "0" * 40}).encode()
        sig = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
        url = BASE + "/v1/skills/sources/%s/webhook" % fx["id"]
        r = requests.post(url, data=body, headers={"Content-Type": "application/json", "X-GitHub-Event": "push",
                                                   "X-Hub-Signature-256": sig}, timeout=20)
        check("webhook with a good signature is accepted", r.status_code == 202, r.status_code)
        r = requests.post(url, data=body, headers={"Content-Type": "application/json", "X-GitHub-Event": "push",
                                                   "X-Hub-Signature-256": "sha256=" + "0" * 64}, timeout=20)
        check("webhook with a bad signature is 401", r.status_code == 401, r.status_code)
    step("webhook", webhook)

    # --- Agent proposes a source; the owner approves in the Inbox -------
    def propose():
        p = agent.tool("skills.propose_source", {"url": "https://github.com/%s/tree/%s/skills/incident-triage" % (FIXTURE_REPO, FIXTURE_BRANCH),
                                                 "reason": "The incident-triage skill should be its own source for the on-call agents."})
        pid = p.get("id") or p.get("proposal", {}).get("id")
        check("skills.propose_source creates a pending proposal", p.get("status", p.get("proposal", {}).get("status")) == "pending", pid)
        items = admin.j("GET", "/v1/inbox").get("requests", [])
        item = next((i for i in items if pid and pid in json.dumps(i)), None) or next(
            (i for i in items if "incident-triage" in json.dumps(i) and i.get("status") in ("pending", "open", None)), None)
        full = admin.j("GET", "/v1/inbox/%s" % item["id"])
        full = full.get("request", full)
        admin.j("POST", "/v1/inbox/%s/decide" % item["id"], json={
            "action": "answer", "request_revision": full.get("revision"),
            "submission_id": "e2e-" + uuid.uuid4().hex[:8], "answers": {"decision": {"value": "approve"}}})
        # The approval creates the source (Auto: its owner is trusted) and its
        # first sync runs; agents see it once a version is live.
        new_src, seen = None, []
        deadline = time.time() + 120
        while time.time() < deadline:
            new_src = next((x for x in admin_sources(admin) if x.get("path") == "skills/incident-triage"), None)
            st = agent.tool("skills.sources", {})
            seen = [x.get("slug") for x in st.get("sources", [])]
            if new_src and new_src.get("slug") in seen:
                break
            time.sleep(3)
        check("the approved proposal adds a live source in Auto mode",
              new_src is not None and new_src.get("mode") == "auto" and new_src.get("slug") in seen,
              {"source": (new_src or {}).get("slug"), "mode": (new_src or {}).get("mode"), "agent sees": seen})
        if new_src:
            admin.req("DELETE", "/v1/skills/sources/%s" % new_src["id"])
    step("proposal", propose)

    # --- Built-in toolyard skills --------------------------------------------
    def builtin():
        src = agent.tool("skills.sources", {})
        slugs = [x.get("slug") for x in src.get("sources", [])]
        check("the built-in source 'toolyard' is served", "toolyard" in slugs, slugs)
        g = agent.tool("skills.get", {"skill": "toolyard/toolyard-basics"})
        check("skills.get toolyard/toolyard-basics returns the guide", g.get("name") == "toolyard-basics" and len(g.get("body", "")) > 200,
              {"files": [f.get("path") for f in g.get("files", [])][:6]})
        s = agent.tool("skills.search", {"query": "permission inbox grant", "source": "toolyard"})
        names = [x.get("name") for x in s.get("skills", [])]
        check("a built-in skill teaches permissions and the Inbox", any("inbox" in (n or "") or "permission" in (n or "") for n in names), names)
        bi = find_source(admin, "toolyard")
        r = admin.req("DELETE", "/v1/skills/sources/%s" % bi["id"]) if bi else None
        check("the built-in source cannot be deleted", r is not None and r.status_code in (400, 403, 409), getattr(r, "status_code", None))
    step("built-in skills", builtin)

    # --- Owner, sharing and contributors (needs a member account) ------------
    member_file = os.environ.get("MEMBER_FILE")
    if member_file:
        creds = json.load(open(member_file))
        msess = requests.Session()
        msess.headers.update({"Origin": BASE, "X-Requested-With": "skills-e2e"})
        msess.post(BASE + "/v1/auth/login", json=creds, timeout=30).raise_for_status()
        me_member = msess.get(BASE + "/v1/auth/me", timeout=20).json()
        member_id = me_member.get("id")
        me_admin = admin.j("GET", "/v1/auth/me")
        check("the first admin is the owner", me_admin.get("is_owner") is True, {"admin": me_admin.get("username")})
        mag = msess.post(BASE + "/v1/agents", json={"name": "skills-e2e-member-" + uuid.uuid4().hex[:6]}, timeout=30).json()
        magent = Agent(mag["token"])
        magent.init()
        mag_id = mag.get("agent_id") or mag.get("id")
        fx_id = fx["id"]

        def visible(ag, ref):
            try:
                return ag.tool("skills.get", {"skill": ref}).get("name") is not None
            except RuntimeError:
                return False

        def sharing():
            put = lambda body: admin.j("PUT", "/v1/skills/sources/%s/skills/sharing" % fx_id, json=body)
            check("a member's agent sees an everyone skill", visible(magent, "fixture/release-notes"))
            put({"path": "release-notes", "mode": "nobody"})
            time.sleep(2)
            check("skill shared with nobody: hidden from the member's agent", not visible(magent, "fixture/release-notes"))
            check("skill shared with nobody: hidden from every agent, the admin's too", not visible(agent, "fixture/release-notes"))
            rows = admin.j("GET", "/v1/skills/sources/%s/skills" % fx_id)
            rows = rows.get("skills", rows) if isinstance(rows, dict) else rows
            check("the admin's Skills page still lists it", any(x.get("path") == "release-notes" for x in rows or []))
            hits = magent.tool("skills.search", {"query": "release notes", "source": "fixture"}).get("skills", [])
            check("skill shared with nobody: not in the member's search", all(h.get("name") != "release-notes" for h in hits))
            listed = [e.get("uri", "") for e in magent.rpc("skills/list", {}).get("result", {}).get("skills", [])]
            check("skill shared with nobody: not in the member's skills/list", all("release-notes" not in u for u in listed))
            put({"path": "release-notes", "mode": "members", "members": [member_id]})
            time.sleep(2)
            check("skill shared with chosen members: the member sees it", visible(magent, "fixture/release-notes"))
            put({"path": "release-notes", "mode": "members", "members": [member_id], "blocked": [member_id]})
            time.sleep(2)
            check("a blocked member does not see the skill", not visible(magent, "fixture/release-notes"))
            put({"path": "release-notes", "mode": ""})
            time.sleep(2)
            check("inherit again: the member sees the skill", visible(magent, "fixture/release-notes"))
            admin.j("PATCH", "/v1/skills/sources/%s" % fx_id, json={"visibility": "nobody"})
            time.sleep(2)
            slugs = [x.get("slug") for x in magent.tool("skills.sources", {}).get("sources", [])]
            check("source shared with nobody: hidden from the member", "fixture" not in slugs, slugs)
            admin.j("PATCH", "/v1/skills/sources/%s" % fx_id, json={"visibility": "everyone"})
        step("sharing", sharing)

        def contributors():
            url = "https://github.com/%s/tree/%s/skills/incident-triage" % (FIXTURE_REPO, FIXTURE_BRANCH)
            r = msess.post(BASE + "/v1/skills/sources", json={"url": url, "mode": "review"}, timeout=60)
            check("a member who is not a contributor cannot add a source", r.status_code in (401, 403, 404), r.status_code)
            admin.j("PUT", "/v1/skills/admin/contributors/%s" % member_id)
            r = msess.post(BASE + "/v1/skills/sources", json={"url": url, "mode": "auto", "slug": "e2e-contrib"}, timeout=120)
            body = r.json() if r.content else {}
            src = body.get("source", body)
            check("a contributor can add a source; it waits for approval", r.status_code in (200, 201) and src.get("approval") == "pending",
                  {"status": r.status_code, "approval": src.get("approval")})
            cid = src.get("id")
            r = msess.get(BASE + "/v1/skills/sources/%s" % fx_id, timeout=20)
            check("a contributor cannot read another person's source settings", r.status_code in (403, 404), r.status_code)
            deadline = time.time() + 90
            while time.time() < deadline:
                s = admin.j("GET", "/v1/skills/sources/%s" % cid)
                s = s.get("source", s)
                if s.get("pending_version_id") or s.get("active_sha"):
                    break
                time.sleep(3)
            slugs = [x.get("slug") for x in agent.tool("skills.sources", {}).get("sources", [])]
            check("a pending contributor source is hidden from agents", "e2e-contrib" not in slugs, slugs)
            s = admin.j("GET", "/v1/skills/sources/%s" % cid)
            s = s.get("source", s)
            vs = admin.j("GET", "/v1/skills/sources/%s/versions" % cid)
            vs = vs.get("versions", vs) if isinstance(vs, dict) else vs
            pend = next((v for v in vs or [] if v.get("id") == s.get("pending_version_id")), {})
            check("a contributor's source in Auto mode waits: nothing is live before approval",
                  bool(pend.get("sha")) and not s.get("active_version_id"), {"pending": pend.get("sha"), "active": s.get("active_sha")})
            r = admin.req("POST", "/v1/skills/sources/%s/approval" % cid, json={"decision": "approve", "sha": "0" * 40})
            check("an approval for another commit is refused", r.status_code == 409, r.status_code)
            admin.j("POST", "/v1/skills/sources/%s/approval" % cid, json={"decision": "approve", "sha": pend.get("sha", "")})
            s = admin.j("GET", "/v1/skills/sources/%s" % cid)
            s = s.get("source", s)
            if s.get("pending_version_id"):
                admin.j("POST", "/v1/skills/sources/%s/versions/%s/activate" % (cid, s["pending_version_id"]))
            time.sleep(2)
            slugs = [x.get("slug") for x in agent.tool("skills.sources", {}).get("sources", [])]
            check("after approval the contributor source is served", "e2e-contrib" in slugs, slugs)
            admin.req("DELETE", "/v1/skills/sources/%s" % cid)
            admin.req("DELETE", "/v1/skills/admin/contributors/%s" % member_id)
        step("contributors", contributors)

        def owner_transfer():
            r = msess.patch(BASE + "/v1/users/%s" % me_admin.get("id"), json={"role": "member"}, timeout=20)
            check("a member cannot change roles", r.status_code in (401, 403), r.status_code)
            r = admin.req("POST", "/v1/users/%s/make-owner" % member_id, json={"confirm": creds["username"]})
            check("the owner can transfer ownership", r.status_code == 200 and msess.get(BASE + "/v1/auth/me", timeout=20).json().get("is_owner") is True,
                  r.status_code)
            r = msess.post(BASE + "/v1/users/%s/make-owner" % me_admin.get("id"), json={"confirm": me_admin.get("username")}, timeout=20)
            check("the new owner can transfer it back", r.status_code == 200 and admin.j("GET", "/v1/auth/me").get("is_owner") is True, r.status_code)
            # A transfer makes the new owner an admin; set the test account back
            # to member so the next run starts from the same state.
            r = admin.req("PATCH", "/v1/users/%s" % member_id, json={"role": "member"})
            check("the owner can set the former owner back to member", r.status_code == 200 and msess.get(BASE + "/v1/auth/me", timeout=20).json().get("role") == "member",
                  r.status_code)
        step("owner transfer", owner_transfer)

        if mag_id:
            msess.delete(BASE + "/v1/agents/%s" % mag_id, timeout=20)
        msess.post(BASE + "/v1/auth/logout", timeout=20)

    # Clean up the agent key.
    if agent_id:
        admin.req("DELETE", "/v1/agents/%s" % agent_id)

    passed = sum(1 for r in results if r["ok"])
    print("\n%d/%d checks passed in %.0f s" % (passed, len(results), time.time() - t0))
    json.dump({"base": BASE, "version": h.get("version"), "results": results}, open(REPORT, "w"), indent=1)
    os.chmod(REPORT, 0o600)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())

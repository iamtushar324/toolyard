#!/usr/bin/env python3
"""End-to-end check of the OAuth "finish in another browser" flow (TEC-1808).

Runs against the Kubernetes HA test deployment or any toolyard. Two cookie
jars stand in for two browser profiles:

  A  the profile where the MCP client waits (it never signs in to toolyard)
  B  the profile where the person is signed in to toolyard (the first admin,
     from the `toolyard-admin` Secret through kubectl, or ADMIN_FILE)

It registers a public client (dynamic registration, PKCE), then checks:

  - /oauth/authorize stops on a page of ours in A (no redirect), with
    "Continue in this browser" and a copyable consent link;
  - B opens the copied consent link, allows the app, and lands on a return
    page with a one-time return link (no redirect to the client);
  - A opens the return link (GET changes nothing), presses Continue (POST
    with the page's nonce), and only then gets a 303 to the client's
    redirect URI with code, state and iss;
  - the code works once at the token endpoint with the PKCE verifier, and
    the access token lists the five tools over MCP;
  - a second use of the return link, and a POST without the nonce cookie,
    are refused.

Inputs (environment):
  BASE        toolyard URL of the deployment under test (required)
  KUBECONFIG/NAMESPACE  for the admin Secret
  ADMIN_FILE  JSON {"username", "password"} instead of the Secret
  REPORT      path of the JSON report (default /tmp/oauth-handoff-e2e.json)

Secrets never appear in argv or output.
"""

import base64
import hashlib
import html
import json
import os
import re
import secrets
import subprocess
import sys
import time
from urllib.parse import parse_qs, urlencode, urlparse

import requests

BASE = os.environ.get("BASE", "").rstrip("/")
if not BASE:
    sys.exit("oauth-handoff-e2e: set BASE to the toolyard URL of the deployment under test")
NS = os.environ.get("NAMESPACE", "toolyard-test")
REPORT = os.environ.get("REPORT", "/tmp/oauth-handoff-e2e.json")
REDIRECT = "http://127.0.0.1:43123/callback"
FIVE = {"tools.search", "getToolDocs", "executeToolCode", "inbox.request", "inbox.status"}

results = []


def check(name, ok, detail=""):
    results.append({"check": name, "ok": bool(ok), "detail": str(detail)[:600]})
    print(("PASS " if ok else "FAIL ") + name + (": " + str(detail)[:300] if detail else ""), flush=True)
    return ok


def admin_creds():
    path = os.environ.get("ADMIN_FILE")
    if path:
        return json.load(open(path))
    out = {}
    for k in ("username", "password"):
        p = subprocess.run(["kubectl", "-n", NS, "get", "secret", "toolyard-admin", "-o", "jsonpath={.data.%s}" % k],
                           capture_output=True, text=True, timeout=60)
        out[k] = base64.b64decode(p.stdout).decode()
    return out


def post_form(sess, url, data, cookies_ok=True):
    h = {"Content-Type": "application/x-www-form-urlencoded", "Origin": BASE, "Sec-Fetch-Site": "same-origin"}
    return sess.post(url, data=urlencode(data, doseq=True), headers=h, allow_redirects=False, timeout=30)


def nonce_of(body):
    m = re.search(r'name="nonce" value="([^"]+)"', body)
    return html.unescape(m.group(1)) if m else None


def main():
    t0 = time.time()
    a, b = requests.Session(), requests.Session()

    reg = requests.post(BASE + "/oauth/register", json={
        "redirect_uris": [REDIRECT], "token_endpoint_auth_method": "none", "client_name": "Handoff test client",
        "grant_types": ["authorization_code", "refresh_token"], "response_types": ["code"]}, timeout=30)
    client_id = reg.json().get("client_id")
    check("a public client registers", reg.status_code == 201 and client_id, reg.status_code)

    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    q = {"response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT, "state": "st-123",
         "code_challenge": challenge, "code_challenge_method": "S256", "resource": BASE + "/mcp", "scope": "mcp"}

    # --- A: authorize stops on our page ----------------------------------
    stop = a.get(BASE + "/oauth/authorize?" + urlencode(q), allow_redirects=False, timeout=30)
    m = re.search(r'value="(' + re.escape(BASE) + r'/oauth/consent\?request=[^"]+)"', stop.text)
    consent_url = html.unescape(m.group(1)) if m else None
    check("authorize stops on a page with Continue and a copyable consent link (profile A)",
          stop.status_code == 200 and "Location" not in stop.headers and "Continue in this browser" in stop.text and consent_url,
          {"status": stop.status_code, "link": bool(consent_url)})
    if not consent_url:
        return finish(t0)

    # --- B: sign in, open the copied link, allow ---------------------------
    b.headers.update({"Origin": BASE, "X-Requested-With": "oauth-handoff-e2e"})
    lr = b.post(BASE + "/v1/auth/login", json=admin_creds(), timeout=30)
    check("profile B signs in to toolyard", lr.status_code == 200, lr.status_code)
    page = b.get(consent_url, timeout=30)
    nonce = nonce_of(page.text)
    check("profile B sees the consent page for the copied link", page.status_code == 200 and nonce, page.status_code)
    done = post_form(b, consent_url, {"nonce": nonce, "verdict": "allow", "nickname": "Handoff test", "scope_mode": "all"})
    ret_url = done.headers.get("Location", "")
    if ret_url.startswith("/"):
        ret_url = BASE + ret_url
    check("allow answers with our return page, not the client", done.status_code == 303 and "/oauth/return/" in ret_url
          and not ret_url.startswith(REDIRECT), {"status": done.status_code, "location": ret_url.split("/oauth/return/")[0]})
    shown = b.get(ret_url, timeout=30)
    m = re.search(r'value="(' + re.escape(BASE) + r'/oauth/return/[^"]+)"', shown.text)
    check("profile B sees a one-time return link to copy", shown.status_code == 200 and m and "Copy return link" in shown.text,
          shown.status_code)
    copied = html.unescape(m.group(1)) if m else ret_url

    # --- A: open the return link and continue --------------------------------
    peek = a.get(copied, timeout=30)
    rnonce = nonce_of(peek.text)
    check("the return link opens in profile A without a toolyard session, and GET changes nothing",
          peek.status_code == 200 and rnonce and "Location" not in peek.headers, peek.status_code)
    no_cookie = post_form(requests.Session(), copied, {"nonce": rnonce})
    check("a POST without the page's nonce cookie is refused", no_cookie.status_code in (400, 403), no_cookie.status_code)
    go = post_form(a, copied, {"nonce": rnonce})
    loc = go.headers.get("Location", "")
    qs = parse_qs(urlparse(loc).query)
    check("Continue in profile A returns to the client with code, state and iss", go.status_code == 303 and loc.startswith(REDIRECT)
          and qs.get("state") == ["st-123"] and qs.get("code") and qs.get("iss") == [BASE],
          {"status": go.status_code, "to_client": loc.startswith(REDIRECT)})
    replay = post_form(a, copied, {"nonce": rnonce})
    reopen = a.get(copied, timeout=30)
    check("the return link works once: a replay is refused and reopening it says already used",
          replay.status_code in (403, 410) and reopen.status_code == 410 and "already used" in reopen.text.lower(),
          {"replay": replay.status_code, "reopen": reopen.status_code})

    # --- the client: token and five tools ---------------------------------------
    code = (qs.get("code") or [""])[0]
    tok = requests.post(BASE + "/oauth/token", data={"grant_type": "authorization_code", "code": code, "redirect_uri": REDIRECT,
                                                     "code_verifier": verifier, "client_id": client_id, "resource": BASE + "/mcp"}, timeout=30)
    at = tok.json().get("access_token") if tok.status_code == 200 else None
    check("the code works at the token endpoint with the PKCE verifier", bool(at), tok.status_code)
    if at:
        h = {"Authorization": "Bearer " + at, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
             "MCP-Protocol-Version": "2025-11-25"}
        def rpc(method, params, sid=None):
            hh = dict(h, **({"Mcp-Session-Id": sid} if sid else {}))
            r = requests.post(BASE + "/mcp", headers=hh, json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, timeout=60)
            text = r.text
            if "text/event-stream" in r.headers.get("Content-Type", ""):
                data = [ln[5:].strip() for ln in text.splitlines() if ln.startswith("data:")]
                text = data[-1] if data else "{}"
            return r.headers.get("Mcp-Session-Id"), (json.loads(text) if text.strip() else {})
        sid, _ = rpc("initialize", {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "handoff-e2e", "version": "1"}})
        _, lst = rpc("tools/list", {}, sid)
        names = {t["name"] for t in lst.get("result", {}).get("tools", [])}
        check("the connected client lists the five tools", names == FIVE, sorted(names))
    return finish(t0)


def finish(t0):
    passed = sum(1 for r in results if r["ok"])
    print("\n%d/%d checks passed in %.0f s" % (passed, len(results), time.time() - t0))
    json.dump({"base": BASE, "results": results}, open(REPORT, "w"), indent=1)
    os.chmod(REPORT, 0o600)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""End-to-end check of the OAuth "finish in another browser" flow (TEC-1808).

Runs against the Kubernetes HA test deployment or any toolyard. Two cookie
jars stand in for two browser profiles:

  A  the profile where the MCP client waits (it never signs in to toolyard)
  B  the profile where the person is signed in to toolyard (the first admin,
     from the `toolyard-admin` Secret through kubectl, or ADMIN_FILE)

It registers a public client (dynamic registration, PKCE), then checks two
authorizations.

The waiting page (the window where the client started continues by itself):

  - /oauth/authorize stops on a page of ours in A (no redirect), with
    "Continue in this browser", a copyable consent link, a confirmation code
    (XXX-XXX) and a waiter secret in the page body only (never in the copied
    link); A keeps its cookies;
  - A's page asks POST /oauth/wait: pending; a wrong waiter gets the answer
    of an unknown request;
  - B opens the copied consent link: it asks for the code; B's Allow
    without the code is refused; B types A's code and allows; A's page then
    sees approved;
  - A's Continue (POST /oauth/wait/continue with the page's nonce) gets a
    303 to the client's redirect URI with code, state and iss; without the
    nonce cookie it is refused;
  - the code works at the token endpoint with the PKCE verifier, and the
    return link B got for the same approval is refused as already used.

The return link (a separate authorization):

  - B types A's code, allows the app and lands on a return page with a
    one-time return link (no redirect to the client);
  - A opens the return link (GET changes nothing), presses Continue (POST
    with the page's nonce), and only then gets a 303 to the client's
    redirect URI with code, state and iss;
  - the code works once at the token endpoint with the PKCE verifier, and
    the access token lists the five tools over MCP;
  - a second use of the return link, and a POST without the nonce cookie,
    are refused; A's waiting page of that request reports used and its
    Continue is refused.

Inputs (environment):
  BASE        toolyard URL of the deployment under test (required)
  KUBECONFIG/NAMESPACE  for the admin Secret
  ADMIN_FILE  JSON {"username", "password"} instead of the Secret
  REPORT      path of the JSON report (default /tmp/oauth-handoff-e2e.json)

Secrets (password, waiter, confirmation code, nonces, codes, tokens) never
appear in argv or output.
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
    return field_of(body, "nonce")


def field_of(body, name):
    m = re.search(r'name="%s" value="([^"]+)"' % re.escape(name), body)
    return html.unescape(m.group(1)) if m else None


def authorize(sess, client_id, state):
    """Starts an authorization in sess (profile A) and returns the PKCE
    verifier, the stop page and the copyable consent link on it."""
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    q = {"response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT, "state": state,
         "code_challenge": challenge, "code_challenge_method": "S256", "resource": BASE + "/mcp", "scope": "mcp"}
    stop = sess.get(BASE + "/oauth/authorize?" + urlencode(q), allow_redirects=False, timeout=30)
    m = re.search(r'value="(' + re.escape(BASE) + r'/oauth/consent\?request=[^"]+)"', stop.text)
    return verifier, stop, (html.unescape(m.group(1)) if m else None)


def code_of(body):
    m = re.search(r'class="oauth-code">([2-9A-HJKMNP-Z]{3}-[2-9A-HJKMNP-Z]{3})</strong>', body)
    return m.group(1) if m else None


def allow_in_b(b, consent_url, nickname, code, refused_first=False):
    """B opens the copied consent link and allows the app with the code A's
    stop page shows (refused_first: first checks that Allow without the
    code is refused); returns the absolute return link the consent answered
    with."""
    page = b.get(consent_url, timeout=30)
    nonce = nonce_of(page.text)
    check("profile B sees the consent page for the copied link, which asks for the code",
          page.status_code == 200 and nonce and 'name="code"' in page.text, page.status_code)
    if refused_first:
        bad = post_form(b, consent_url, {"nonce": nonce, "verdict": "allow", "nickname": nickname, "scope_mode": "all"})
        check("B's Allow without the code is refused", bad.status_code == 400 and "That code is not right" in bad.text
              and "Location" not in bad.headers, bad.status_code)
        page = b.get(consent_url, timeout=30)
        nonce = nonce_of(page.text)
    done = post_form(b, consent_url, {"nonce": nonce, "verdict": "allow", "nickname": nickname, "scope_mode": "all", "code": code or ""})
    ret_url = done.headers.get("Location", "")
    if ret_url.startswith("/"):
        ret_url = BASE + ret_url
    check("allow answers with our return page, not the client", done.status_code == 303 and "/oauth/return/" in ret_url
          and not ret_url.startswith(REDIRECT), {"status": done.status_code, "location": ret_url.split("/oauth/return/")[0]})
    return ret_url


def poll(sess, rid, waiter):
    """Asks /oauth/wait as the stop page's script does: (HTTP status, status)."""
    r = sess.post(BASE + "/oauth/wait", json={"request": rid, "waiter": waiter},
                  headers={"Origin": BASE, "Sec-Fetch-Site": "same-origin"}, timeout=30)
    try:
        return r.status_code, r.json().get("status")
    except ValueError:
        return r.status_code, None


def exchange(client_id, code, verifier):
    tok = requests.post(BASE + "/oauth/token", data={"grant_type": "authorization_code", "code": code, "redirect_uri": REDIRECT,
                                                     "code_verifier": verifier, "client_id": client_id, "resource": BASE + "/mcp"}, timeout=30)
    return tok.status_code, (tok.json().get("access_token") if tok.status_code == 200 else None)


def to_client(go, state):
    loc = go.headers.get("Location", "")
    qs = parse_qs(urlparse(loc).query)
    ok = go.status_code == 303 and loc.startswith(REDIRECT) and qs.get("state") == [state] and qs.get("code") and qs.get("iss") == [BASE]
    return ok, (qs.get("code") or [""])[0], {"status": go.status_code, "to_client": loc.startswith(REDIRECT)}


def main():
    t0 = time.time()
    a, b = requests.Session(), requests.Session()

    reg = requests.post(BASE + "/oauth/register", json={
        "redirect_uris": [REDIRECT], "token_endpoint_auth_method": "none", "client_name": "Handoff test client",
        "grant_types": ["authorization_code", "refresh_token"], "response_types": ["code"]}, timeout=30)
    client_id = reg.json().get("client_id")
    check("a public client registers", reg.status_code == 201 and client_id, reg.status_code)

    b.headers.update({"Origin": BASE, "X-Requested-With": "oauth-handoff-e2e"})
    lr = b.post(BASE + "/v1/auth/login", json=admin_creds(), timeout=30)
    check("profile B signs in to toolyard", lr.status_code == 200, lr.status_code)

    # === 1. The waiting page: A's window continues by itself ==============
    wverifier, wstop, wconsent = authorize(a, client_id, "st-wait")
    check("authorize stops on a page with Continue and a copyable consent link (profile A)",
          wstop.status_code == 200 and "Location" not in wstop.headers and "Continue in this browser" in wstop.text and wconsent,
          {"status": wstop.status_code, "link": bool(wconsent)})
    if not wconsent:
        return finish(t0)
    rid, waiter, wnonce = field_of(wstop.text, "request"), field_of(wstop.text, "waiter"), nonce_of(wstop.text)
    wcode = code_of(wstop.text)
    has_cookie = any("toolyard_oauth_wait_" in c.name for c in a.cookies)
    in_cookies = wcode and any(wcode.replace("-", "") in (c.value or "") for c in a.cookies)
    check("the stop page waits and shows a code, with its secrets in the page body only (not in the copied link or a cookie)",
          rid and waiter and wnonce and wcode and has_cookie and waiter not in wconsent and wstop.text.count(waiter) == 1
          and wcode.replace("-", "") not in wconsent and not in_cookies
          and "Keep this window open" in wstop.text and "/oauth/wait.js" in wstop.text,
          {"request": bool(rid), "waiter": bool(waiter), "code": bool(wcode), "nonce_cookie": has_cookie})
    if not (rid and waiter and wnonce):
        return finish(t0)
    st = poll(a, rid, waiter)
    check("A's waiting page reports pending before the answer", st == (200, "pending"), st)
    wrong, unknown = poll(requests.Session(), rid, "tywt_" + "A" * 43), poll(requests.Session(), "A" * 43, waiter)
    check("a wrong waiter gets the same answer as an unknown request", wrong == unknown == (200, "expired"),
          {"wrong": wrong, "unknown": unknown})

    wret = allow_in_b(b, wconsent, "Handoff wait test", wcode, refused_first=True)
    shown = b.get(wret, timeout=30)
    check("B's return page says the waiting window is ready", shown.status_code == 200 and "continue there: it is ready" in shown.text,
          shown.status_code)
    deadline, st = time.time() + 30, None
    while time.time() < deadline:
        st = poll(a, rid, waiter)
        if st[1] != "pending":
            break
        time.sleep(2)
    check("A's waiting page sees the approval from B", st == (200, "approved"), st)
    cont = {"request": rid, "waiter": waiter, "nonce": wnonce}
    no_cookie = post_form(requests.Session(), BASE + "/oauth/wait/continue", cont)
    check("Continue without the stop page's nonce cookie is refused", no_cookie.status_code == 403, no_cookie.status_code)
    ok, wcode, detail = to_client(post_form(a, BASE + "/oauth/wait/continue", cont), "st-wait")
    check("Continue on A's waiting page returns to the client with code, state and iss", ok, detail)
    status, wat = exchange(client_id, wcode, wverifier)
    check("the waiting page's code works at the token endpoint with the PKCE verifier", bool(wat), status)
    reopen = a.get(wret, timeout=30)
    check("the return link of the same approval is then refused as already used",
          reopen.status_code == 410 and "already used" in reopen.text.lower(), reopen.status_code)
    st = poll(a, rid, waiter)
    check("A's waiting page then reports used", st == (200, "used"), st)

    # === 2. The return link (a separate authorization) ====================
    verifier, stop, consent_url = authorize(a, client_id, "st-123")
    check("a second authorization stops on our page (profile A)",
          stop.status_code == 200 and "Location" not in stop.headers and consent_url, stop.status_code)
    if not consent_url:
        return finish(t0)
    rid2, waiter2, wnonce2 = field_of(stop.text, "request"), field_of(stop.text, "waiter"), nonce_of(stop.text)
    ret_url = allow_in_b(b, consent_url, "Handoff test", code_of(stop.text))
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
    ok, code, detail = to_client(post_form(a, copied, {"nonce": rnonce}), "st-123")
    check("Continue in profile A returns to the client with code, state and iss", ok, detail)
    replay = post_form(a, copied, {"nonce": rnonce})
    reopen = a.get(copied, timeout=30)
    check("the return link works once: a replay is refused and reopening it says already used",
          replay.status_code in (403, 410) and reopen.status_code == 410 and "already used" in reopen.text.lower(),
          {"replay": replay.status_code, "reopen": reopen.status_code})
    st = poll(a, rid2, waiter2) if rid2 and waiter2 else None
    late = post_form(a, BASE + "/oauth/wait/continue", {"request": rid2 or "", "waiter": waiter2 or "", "nonce": wnonce2 or ""})
    check("after the return link, A's waiting page of that request reports used and its Continue is refused",
          st == (200, "used") and late.status_code == 410 and "Location" not in late.headers, {"poll": st, "continue": late.status_code})

    # --- the client: token and five tools ---------------------------------------
    status, at = exchange(client_id, code, verifier)
    check("the code works at the token endpoint with the PKCE verifier", bool(at), status)
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

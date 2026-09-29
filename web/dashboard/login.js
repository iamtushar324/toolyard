// toolyard /login: "Sign in with Google" through Clerk. Vanilla, no build
// step, nothing inline (the page CSP allows Clerk's Frontend API origin for
// scripts and fetches, and nothing inline for scripts).
//
// Flow: GET /v1/auth/config -> load Clerk's UI and clerk-js bundles from the
// instance's Frontend API -> Clerk.load(). If the browser already holds a
// Clerk session, trade its JWT for a toolyard session cookie
// (POST /v1/auth/clerk/session) and open the dashboard; otherwise mount
// Clerk's <SignIn/> and do the same once it signs the user in.
//
// Script-tag load per https://clerk.com/docs/js-frontend/getting-started/quickstart
// (the "<script>" tab). clerk-js 6 ships its prebuilt components separately
// as @clerk/ui, so both bundles load and the UI constructor goes to load().

(() => {
  // Clerk majors matched to bkt3, which uses the same Clerk instance:
  // @clerk/clerk-js 6.x, with its components in @clerk/ui 1.x.
  const CLERK_JS_MAJOR = 6;
  const CLERK_UI_MAJOR = 1;
  const PASSWORD_URL = '/?password=1';

  // Where Clerk returns after sign-in, sign-up and sign-out. Without an
  // explicit URL Clerk falls back to the instance's dashboard setting, which
  // for the Beknown instance is another app, so the exchange below would
  // never run. Path only: <SignIn/> keeps its steps in the hash, and the
  // query may carry ?signout=1 or Clerk handshake parameters.
  const here = location.origin + location.pathname;
  // The dashboard's Logout sends Google users here with ?signout=1 so the
  // Clerk session ends too; otherwise the next visit signs straight back in.
  const signingOut = new URLSearchParams(location.search).get('signout') === '1';
  const SCRIPT_TIMEOUT_MS = 20000;

  const body = document.getElementById('login-body');
  const passwordAlt = document.getElementById('login-alt');
  let clerk = null;
  let mountNode = null;
  // Set once an exchange starts and cleared only by an explicit retry, so a
  // later Clerk emission (token refresh, org switch) can't re-run it behind
  // an error panel.
  let exchanging = false;

  // el mirrors the dashboard's DOM helper in app.js.
  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const k in attrs) {
      if (k === 'class') node.className = attrs[k];
      else if (k === 'on') for (const ev in attrs.on) node.addEventListener(ev, attrs.on[ev]);
      else if (k in node) {
        try { node[k] = attrs[k]; } catch { node.setAttribute(k, attrs[k]); }
      } else node.setAttribute(k, attrs[k]);
    }
    for (const c of children.flat()) {
      if (c == null || c === false) continue;
      node.appendChild(c.nodeType ? c : document.createTextNode(String(c)));
    }
    return node;
  }

  function unmountSignIn() {
    if (mountNode && clerk) {
      try { clerk.unmountSignIn(mountNode); } catch (_) {}
    }
    mountNode = null;
  }

  function setBody(node) {
    unmountSignIn();
    body.replaceChildren(node);
  }

  function status(text) {
    return el('p', { class: 'meta login-status' }, text);
  }

  // panel is the card for every non-Clerk state: a title, explanatory
  // lines and action buttons.
  function panel(title, lines, actions) {
    return el('div', { class: 'card login-panel' },
      el('h2', {}, title),
      lines.map((l) => el('p', { class: 'meta' }, l)),
      actions && actions.length ? el('div', { class: 'row login-actions' }, actions) : null,
    );
  }

  function actionButton(label, onClick, primary) {
    return el('button', {
      class: primary ? 'primary' : '',
      on: { click: async (e) => { e.currentTarget.disabled = true; await onClick(); } },
    }, label);
  }

  const retryButton = () => actionButton('Retry', () => location.reload(), true);

  // ---- states ---------------------------------------------------------------

  function hidePasswordLink() {
    if (passwordAlt) passwordAlt.hidden = true;
  }

  function showNotSetUp() {
    hidePasswordLink(); // the panel's own button is the way out
    setBody(panel("Google sign-in isn't set up here", [
      "This toolyard doesn't have Clerk configured, so there's no Google sign-in.",
      'Sign in with your toolyard username and password instead.',
    ], [
      el('a', { class: 'btn primary', href: PASSWORD_URL }, 'Use password instead'),
    ]));
  }

  function showSignIn() {
    unmountSignIn();
    mountNode = el('div', { class: 'clerk-mount' });
    body.replaceChildren(mountNode);
    try {
      clerk.mountSignIn(mountNode, {
        forceRedirectUrl: here,
        signUpForceRedirectUrl: here,
      });
    } catch (e) {
      mountNode = null;
      clerk = null; // nothing to sign out of that the retry can't redo
      showFailed(`Couldn't show Google sign-in. ${(e && e.message) || ''}`.trim());
    }
  }

  function showDenied() {
    setBody(panel('Access denied', [
      'This account is not a member of the Beknown workspace.',
      'Ask an admin to invite you, or sign in with a Beknown workspace account.',
    ], [
      actionButton('Try a different account', signOutAndRestart, true),
    ]));
  }

  function showBlocked() {
    setBody(panel('Access blocked', [
      'Your toolyard access has been blocked. Ask an admin.',
    ], [
      actionButton('Try a different account', signOutAndRestart),
    ]));
  }

  function showFailed(message) {
    setBody(panel('Sign-in failed', [message || 'Please try again.'], [
      clerk ? actionButton('Sign out and retry', signOutAndRestart, true) : retryButton(),
    ]));
  }

  async function signOutAndRestart() {
    try { await clerk.signOut({ redirectUrl: here }); } catch (_) {}
    // signOut usually navigates to `here`; if it didn't, carry on in place.
    exchanging = false;
    showSignIn();
  }

  // ---- exchange -------------------------------------------------------------

  function failureText(httpStatus, code) {
    if (code === 'invalid_token') return "toolyard couldn't verify your Clerk session. Sign out and try again.";
    if (code === 'clerk_unavailable') return "toolyard couldn't reach Clerk to check your account. Try again in a minute.";
    return code || `toolyard answered HTTP ${httpStatus}.`;
  }

  async function exchange() {
    if (exchanging) return;
    exchanging = true;
    setBody(status('Signing you in…'));

    let token = null;
    try {
      token = clerk.session ? await clerk.session.getToken() : null;
    } catch (e) {
      showFailed((e && e.message) || 'Could not read a Clerk session token.');
      return;
    }
    if (!token) { showFailed('Could not read a Clerk session token.'); return; }

    let r;
    let res = null;
    try {
      r = await fetch('/v1/auth/clerk/session', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'toolyard' },
        body: JSON.stringify({ token }),
      });
      try { res = await r.json(); } catch (_) {}
    } catch (_) {
      showFailed("Couldn't reach toolyard. Check your connection and try again.");
      return;
    }
    if (r.ok) { location.replace('/'); return; }
    const code = res && res.error;
    if (r.status === 403 && code === 'not_org_member') { showDenied(); return; }
    if (r.status === 403 && code === 'blocked') { showBlocked(); return; }
    if (r.status === 404) { showNotSetUp(); return; }
    showFailed(failureText(r.status, code));
  }

  // ---- boot -----------------------------------------------------------------

  function loadScript(src, attrs) {
    return new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = src;
      s.crossOrigin = 'anonymous';
      // Dynamic scripts default to async; keep insertion order (UI first).
      s.async = false;
      for (const k in attrs || {}) s.setAttribute(k, attrs[k]);
      const timer = setTimeout(() => reject(new Error('timed out loading ' + src)), SCRIPT_TIMEOUT_MS);
      s.onload = () => { clearTimeout(timer); resolve(); };
      s.onerror = () => { clearTimeout(timer); reject(new Error('failed to load ' + src)); };
      document.head.appendChild(s);
    });
  }

  async function start() {
    let cfg;
    try {
      const r = await fetch('/v1/auth/config', {
        credentials: 'same-origin',
        headers: { 'X-Requested-With': 'toolyard' },
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      cfg = await r.json();
    } catch (e) {
      setBody(panel("Couldn't load sign-in", [
        `toolyard didn't return its sign-in settings (${e.message}).`,
      ], [retryButton()]));
      return;
    }

    if (cfg && cfg.password_login === false) hidePasswordLink();
    const conf = cfg && cfg.clerk;
    if (!conf || !conf.publishable_key || !conf.frontend_api) { showNotSetUp(); return; }
    // The Frontend API is a bare host (clerk.example.com); refuse anything
    // else rather than build a script URL out of it.
    const host = String(conf.frontend_api).replace(/^https?:\/\//, '').replace(/\/+$/, '');
    if (!/^[a-z0-9.-]+(:\d+)?$/i.test(host)) { showFailed('The Clerk Frontend API setting is not a host name.'); return; }

    try {
      await Promise.all([
        loadScript(`https://${host}/npm/@clerk/ui@${CLERK_UI_MAJOR}/dist/ui.browser.js`),
        loadScript(`https://${host}/npm/@clerk/clerk-js@${CLERK_JS_MAJOR}/dist/clerk.browser.js`,
          { 'data-clerk-publishable-key': conf.publishable_key }),
      ]);
      clerk = window.Clerk;
      // The bundle exposes an instance when it finds the key attribute;
      // construct one if it only left the class behind.
      if (typeof clerk === 'function') clerk = new clerk(conf.publishable_key);
      if (!clerk || typeof clerk.load !== 'function') throw new Error('Clerk did not initialise.');
      if (!window.__internal_ClerkUICtor) throw new Error("Clerk's sign-in components did not load.");
      await clerk.load({ ui: { ClerkUI: window.__internal_ClerkUICtor } });
    } catch (e) {
      clerk = null;
      showFailed(`Couldn't load Google sign-in from ${host}. ${(e && e.message) || ''}`.trim());
      return;
    }

    if (signingOut && clerk.user) {
      setBody(status('Signing you out…'));
      // Usually navigates to `here` (a fresh /login); otherwise carry on.
      try { await clerk.signOut({ redirectUrl: here }); } catch (_) {}
    }

    // A sign-in that completes inside <SignIn/> without a page load lands
    // here; so does one that Clerk finishes after its OAuth redirect.
    clerk.addListener(({ user }) => { if (user) exchange(); }, { skipInitialEmit: true });

    if (clerk.user) exchange();
    else showSignIn();
  }

  start().catch((e) => showFailed((e && e.message) || 'Sign-in failed.'));
})();

// Landing-page interactions: nav scroll state, scroll reveals, copy
// buttons, install tabs, the typed one-liner, and the approval-card demo.
(function () {
  'use strict';

  var reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  // ---- nav scroll state -------------------------------------------------
  var nav = document.getElementById('nav');
  function onScroll() {
    nav.classList.toggle('scrolled', window.scrollY > 24);
  }
  window.addEventListener('scroll', onScroll, { passive: true });
  onScroll();

  // ---- scroll reveals -----------------------------------------------------
  var revealEls = document.querySelectorAll('.reveal');
  if (reduceMotion || !('IntersectionObserver' in window)) {
    revealEls.forEach(function (el) { el.classList.add('in'); });
  } else {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add('in');
          io.unobserve(e.target);
        }
      });
    }, { threshold: 0.12, rootMargin: '0px 0px -8% 0px' });
    revealEls.forEach(function (el) { io.observe(el); });
  }

  // ---- copy buttons ---------------------------------------------------------
  document.querySelectorAll('.copy-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var text = btn.getAttribute('data-copy-text');
      if (!text) {
        var sel = btn.getAttribute('data-copy');
        var el = sel && document.querySelector(sel);
        text = el ? el.textContent : '';
      }
      if (!text) return;
      navigator.clipboard.writeText(text).then(function () {
        btn.classList.add('copied');
        var prev = btn.innerHTML;
        if (btn.textContent.trim() === 'copy') btn.textContent = 'copied';
        setTimeout(function () {
          btn.classList.remove('copied');
          if (btn.textContent.trim() === 'copied') btn.innerHTML = prev;
        }, 1600);
      });
    });
  });

  // ---- install tabs -----------------------------------------------------------
  var tabs = document.querySelectorAll('.terminal-tabs [role="tab"]');
  var panels = document.querySelectorAll('.terminal-body');
  tabs.forEach(function (tab) {
    tab.addEventListener('click', function () {
      tabs.forEach(function (t) { t.setAttribute('aria-selected', String(t === tab)); });
      panels.forEach(function (p) {
        var active = p.getAttribute('data-panel') === tab.getAttribute('data-tab');
        p.hidden = !active;
        p.setAttribute('aria-hidden', String(!active));
      });
    });
  });

  // ---- typed one-liner -----------------------------------------------------------
  var typedTarget = document.getElementById('typed-cmd');
  var typedCursor = document.getElementById('typed-cursor');
  var typedOut = document.getElementById('typed-out');
  var CMD = 'curl -fsSL https://raw.githubusercontent.com/iamtushar324/toolyard/main/install.sh | bash';

  function showFullCommand() {
    typedTarget.textContent = CMD;
    typedOut.hidden = false;
    typedCursor.style.display = 'none';
  }

  if (reduceMotion || !('IntersectionObserver' in window)) {
    showFullCommand();
  } else {
    var typedStarted = false;
    var typeIO = new IntersectionObserver(function (entries) {
      if (!entries[0].isIntersecting || typedStarted) return;
      typedStarted = true;
      typeIO.disconnect();
      var i = 0;
      (function tick() {
        if (i <= CMD.length) {
          typedTarget.textContent = CMD.slice(0, i);
          i += 1 + (Math.random() < 0.3 ? 1 : 0); // uneven, human-ish
          setTimeout(tick, 14 + Math.random() * 26);
        } else {
          setTimeout(function () {
            typedOut.hidden = false;
            typedCursor.style.display = 'none';
          }, 350);
        }
      })();
    }, { threshold: 0.4 });
    typeIO.observe(typedTarget.closest('.terminal'));
  }

  // ---- approval-card demo -----------------------------------------------------------
  // Pressing Allow/Deny plays the decision, then the card resets to
  // pending after a beat — an endless little demo loop.
  var card = document.getElementById('approval-demo');
  if (card) {
    var statusEl = card.querySelector('.approval-status');
    var resetTimer = null;
    function decide(kind) {
      card.classList.remove('allowed', 'denied');
      card.classList.add(kind === 'allow' ? 'allowed' : 'denied');
      statusEl.innerHTML = kind === 'allow'
        ? '<span class="dot dot-green"></span><em>allowed · executing</em>'
        : '<span class="dot dot-red"></span><em>denied</em>';
      clearTimeout(resetTimer);
      resetTimer = setTimeout(function () {
        card.classList.remove('allowed', 'denied');
        statusEl.innerHTML = '<span class="dot dot-amber"></span><em>pending</em>';
      }, 2400);
    }
    card.querySelector('.btn-allow').addEventListener('click', function () { decide('allow'); });
    card.querySelector('.btn-deny').addEventListener('click', function () { decide('deny'); });
  }

  // ---- footer year ------------------------------------------------------------------
  var year = document.getElementById('footer-year');
  if (year) year.textContent = '© ' + new Date().getFullYear();
})();

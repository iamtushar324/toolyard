/* toolyard lake — manifest-driven dashboard.
 *
 * Architecture:
 *   1. fetch /v1/lake/manifest → build tab buttons.
 *   2. on tab activate, render its panels.
 *   3. each panel kind has a renderer in PANEL_RENDERERS.
 *   4. data comes from /v1/lake/run/{tab}/{query} or /v1/lake/exec.
 *
 * Adding a new chart kind: add a renderer to PANEL_RENDERERS keyed on
 * panel.type. Adding a new chart: drop a SQL file under queries/<tab>/
 * and add an entry in manifest.json. No JS change required.
 */

(function () {
  'use strict';

  const main = document.getElementById('lake-main');
  const tabsEl = document.getElementById('lake-tabs');
  let manifest = null;
  let activeTabId = null;

  // ---------- bootstrap ----------------------------------------------------
  fetch('/v1/lake/manifest', { credentials: 'include' })
    .then(r => {
      if (!r.ok) throw new Error('manifest http ' + r.status);
      return r.json();
    })
    .then(m => {
      manifest = m;
      renderTabs();
      const initial = (location.hash || '').replace(/^#\//, '') || (m.tabs[0] && m.tabs[0].id);
      activate(initial);
    })
    .catch(err => {
      main.innerHTML = '<div class="lake-error">Failed to load manifest: ' +
        escapeHtml(err.message) + '. Are you logged in to the gateway?</div>';
    });

  window.addEventListener('hashchange', () => {
    const id = (location.hash || '').replace(/^#\//, '');
    if (id) activate(id);
  });

  // ---------- tabs ---------------------------------------------------------
  function renderTabs() {
    tabsEl.innerHTML = '';
    manifest.tabs.forEach(t => {
      const btn = document.createElement('button');
      btn.textContent = t.label || t.id;
      btn.dataset.tab = t.id;
      btn.addEventListener('click', () => {
        location.hash = '#/' + t.id;
        activate(t.id);
      });
      tabsEl.appendChild(btn);
    });
  }

  function activate(tabId) {
    activeTabId = tabId;
    [...tabsEl.children].forEach(btn => {
      btn.classList.toggle('active', btn.dataset.tab === tabId);
    });
    const tab = manifest.tabs.find(t => t.id === tabId);
    if (!tab) {
      main.innerHTML = '<div class="lake-error">Unknown tab: ' + escapeHtml(tabId) + '</div>';
      return;
    }
    renderTab(tab);
  }

  // ---------- tab renderer -------------------------------------------------
  function renderTab(tab) {
    const grid = document.createElement('div');
    grid.className = 'lake-grid';
    main.innerHTML = '';
    main.appendChild(grid);

    tab.panels.forEach(panel => {
      const cell = document.createElement('section');
      cell.className = 'lake-panel size-' + (panel.size || 'medium');
      if (panel.type === 'kpi') cell.classList.add('kpi');
      const title = document.createElement('div');
      title.className = 'lake-panel-title';
      title.innerHTML = '<span>' + escapeHtml(panel.title || panel.id) + '</span>' +
        '<span class="lake-panel-meta"></span>';
      const body = document.createElement('div');
      body.className = 'lake-panel-body';
      cell.appendChild(title);
      cell.appendChild(body);
      grid.appendChild(cell);
      const renderer = PANEL_RENDERERS[panel.type] || renderUnknown;
      renderer(panel, body, tab);
    });
  }

  // ---------- panel renderers ---------------------------------------------
  const PANEL_RENDERERS = {
    table: renderTable,
    line_chart: renderLineChart,
    bar_chart: renderBarChart,
    pie_chart: renderPieChart,
    kpi: renderKPI,
    sql_editor: renderSqlEditor,
  };

  function renderUnknown(panel, body) {
    body.innerHTML = '<div class="lake-error">Unknown panel type: ' +
      escapeHtml(panel.type) + '</div>';
  }

  function renderTable(panel, body, tab) {
    body.textContent = 'Loading…';
    runNamedQuery(tab.id, queryId(panel.query)).then(res => {
      if (!res.rows || res.rows.length === 0) {
        body.innerHTML = '<div class="lake-error">No rows.</div>';
        return;
      }
      const tbl = document.createElement('table');
      tbl.className = 'lake-table';
      const thead = document.createElement('thead');
      const trh = document.createElement('tr');
      res.columns.forEach(c => {
        const th = document.createElement('th');
        th.textContent = c.name;
        trh.appendChild(th);
      });
      thead.appendChild(trh);
      tbl.appendChild(thead);
      const tbody = document.createElement('tbody');
      res.rows.forEach(row => {
        const tr = document.createElement('tr');
        row.forEach(v => {
          const td = document.createElement('td');
          td.textContent = formatValue(v);
          tr.appendChild(td);
        });
        tbody.appendChild(tr);
      });
      tbl.appendChild(tbody);
      body.innerHTML = '';
      body.appendChild(tbl);
      if (res.truncated) {
        const note = document.createElement('div');
        note.className = 'lake-explorer-meta';
        note.textContent = 'Truncated at ' + res.row_count + ' rows.';
        body.appendChild(note);
      }
    }).catch(err => showPanelError(body, err));
  }

  function renderLineChart(panel, body, tab) {
    body.textContent = 'Loading…';
    runNamedQuery(tab.id, queryId(panel.query)).then(res => {
      const xKey = panel.x;
      const yKeys = Array.isArray(panel.y) ? panel.y : [panel.y];
      const xCol = res.columns.findIndex(c => c.name === xKey);
      if (xCol < 0) throw new Error('column ' + xKey + ' not in result');
      const xs = res.rows.map(r => String(r[xCol]));
      const series = yKeys.map(y => {
        const yi = res.columns.findIndex(c => c.name === y);
        return {
          name: y,
          type: 'line',
          smooth: true,
          data: res.rows.map(r => yi < 0 ? null : r[yi]),
        };
      });
      body.innerHTML = '';
      const chart = window.echarts ? echarts.init(body, null, { renderer: 'canvas' }) : null;
      if (!chart) { body.textContent = 'ECharts vendor missing'; return; }
      chart.setOption({
        backgroundColor: 'transparent',
        textStyle: { color: '#e8eaed' },
        tooltip: { trigger: 'axis' },
        legend: { data: yKeys, textStyle: { color: '#e8eaed' } },
        grid: { left: 40, right: 20, top: 30, bottom: 40 },
        xAxis: { type: 'category', data: xs, axisLine: { lineStyle: { color: '#8a93a3' } } },
        yAxis: { type: 'value', axisLine: { lineStyle: { color: '#8a93a3' } } },
        series,
      });
      window.addEventListener('resize', () => chart.resize());
    }).catch(err => showPanelError(body, err));
  }

  function renderBarChart(panel, body, tab) {
    // Same as line chart but type:bar.
    body.textContent = 'Loading…';
    runNamedQuery(tab.id, queryId(panel.query)).then(res => {
      const xCol = res.columns.findIndex(c => c.name === panel.x);
      const yKeys = Array.isArray(panel.y) ? panel.y : [panel.y];
      const xs = res.rows.map(r => String(r[xCol]));
      const series = yKeys.map(y => {
        const yi = res.columns.findIndex(c => c.name === y);
        return { name: y, type: 'bar', stack: panel.stacked ? 'total' : null,
          data: res.rows.map(r => yi < 0 ? null : r[yi]) };
      });
      body.innerHTML = '';
      const chart = echarts.init(body);
      chart.setOption({
        backgroundColor: 'transparent', textStyle: { color: '#e8eaed' },
        tooltip: { trigger: 'axis' },
        legend: { data: yKeys, textStyle: { color: '#e8eaed' } },
        grid: { left: 40, right: 20, top: 30, bottom: 40 },
        xAxis: { type: 'category', data: xs },
        yAxis: { type: 'value' },
        series,
      });
      window.addEventListener('resize', () => chart.resize());
    }).catch(err => showPanelError(body, err));
  }

  function renderPieChart(panel, body, tab) {
    body.textContent = 'Loading…';
    runNamedQuery(tab.id, queryId(panel.query)).then(res => {
      const labelCol = res.columns.findIndex(c => c.name === panel.label);
      const valueCol = res.columns.findIndex(c => c.name === panel.value);
      const data = res.rows.map(r => ({ name: String(r[labelCol]), value: r[valueCol] }));
      body.innerHTML = '';
      const chart = echarts.init(body);
      chart.setOption({
        backgroundColor: 'transparent', textStyle: { color: '#e8eaed' },
        tooltip: { trigger: 'item' },
        series: [{ type: 'pie', radius: '70%', data }],
      });
      window.addEventListener('resize', () => chart.resize());
    }).catch(err => showPanelError(body, err));
  }

  function renderKPI(panel, body, tab) {
    body.innerHTML = '';
    const v = document.createElement('div'); v.className = 'lake-kpi-value'; v.textContent = '…';
    body.appendChild(v);
    const d = document.createElement('div'); d.className = 'lake-kpi-delta';
    body.appendChild(d);
    runNamedQuery(tab.id, queryId(panel.query)).then(res => {
      const valCol = res.columns.findIndex(c => c.name === panel.value);
      const row = res.rows[0] || [];
      const raw = row[valCol];
      // The manifest can override; otherwise we infer from the column name.
      const fmt = panel.format || inferFormat(panel.value);
      v.textContent = formatWithKind(raw, fmt);
    }).catch(err => { v.textContent = 'error'; d.textContent = String(err); });
  }

  function renderSqlEditor(panel, body) {
    body.innerHTML = '';
    body.classList.add('lake-explorer');
    const ta = document.createElement('textarea');
    ta.placeholder = 'SELECT 1; -- read-only. SELECT/WITH/SHOW/PRAGMA only.';
    body.appendChild(ta);
    const run = document.createElement('button');
    run.textContent = 'Run';
    body.appendChild(run);
    const out = document.createElement('div'); out.className = 'lake-explorer-result';
    body.appendChild(out);
    run.addEventListener('click', async () => {
      out.innerHTML = 'Running…';
      try {
        const res = await fetch('/v1/lake/exec', {
          method: 'POST', credentials: 'include',
          headers: {'Content-Type':'application/json','X-Toolyard-Source':'lake-ui'},
          body: JSON.stringify({ sql: ta.value, max_rows: 500 }),
        });
        if (!res.ok) {
          const text = await res.text();
          out.innerHTML = '<div class="lake-error">' + escapeHtml(text) + '</div>';
          return;
        }
        const data = await res.json();
        if (data.error) {
          out.innerHTML = '<div class="lake-error">' + escapeHtml(data.error) + '</div>';
          return;
        }
        renderExplorerResult(out, data);
      } catch (err) {
        out.innerHTML = '<div class="lake-error">' + escapeHtml(String(err)) + '</div>';
      }
    });
  }

  function renderExplorerResult(container, res) {
    container.innerHTML = '';
    const meta = document.createElement('div'); meta.className = 'lake-explorer-meta';
    meta.textContent = res.row_count + ' rows · ' + res.elapsed_ms + ' ms' +
      (res.truncated ? ' · TRUNCATED' : '');
    container.appendChild(meta);
    if (!res.rows || res.rows.length === 0) return;
    const tbl = document.createElement('table'); tbl.className = 'lake-table';
    const thead = document.createElement('thead'); const trh = document.createElement('tr');
    res.columns.forEach(c => {
      const th = document.createElement('th'); th.textContent = c.name; trh.appendChild(th);
    });
    thead.appendChild(trh); tbl.appendChild(thead);
    const tbody = document.createElement('tbody');
    res.rows.forEach(row => {
      const tr = document.createElement('tr');
      row.forEach(v => {
        const td = document.createElement('td'); td.textContent = formatValue(v); tr.appendChild(td);
      });
      tbody.appendChild(tr);
    });
    tbl.appendChild(tbody); container.appendChild(tbl);
  }

  // ---------- helpers ------------------------------------------------------

  // queryId pulls 'subscriptions' out of 'finance/subscriptions.sql'.
  function queryId(qPath) {
    return qPath.split('/').pop().replace(/\.sql$/, '');
  }

  function runNamedQuery(tab, id) {
    return fetch('/v1/lake/run/' + encodeURIComponent(tab) + '/' + encodeURIComponent(id),
      { credentials: 'include' })
      .then(async r => {
        if (!r.ok) {
          const t = await r.text();
          throw new Error(t || ('http ' + r.status));
        }
        return r.json();
      });
  }

  function showPanelError(body, err) {
    body.innerHTML = '<div class="lake-error">' + escapeHtml(String(err && err.message || err)) + '</div>';
  }

  function formatValue(v) {
    if (v == null) return '';
    if (typeof v === 'number') return v.toLocaleString();
    if (v instanceof Date) return v.toISOString();
    return String(v);
  }

  // inferFormat picks a sensible formatter from a column name. Hits the
  // common personal-finance column names so KPIs render naturally without
  // every panel having to specify "format": "currency_inr". Heuristic, not
  // exhaustive — manifest entries with an explicit `format` win.
  function inferFormat(name) {
    if (!name) return 'plain';
    const n = String(name).toLowerCase();
    if (/(balance|worth|asset|liabilit|amount|paid|billed|value|pnl|outstanding)/.test(n)) {
      return 'currency_inr';
    }
    if (/(count|n_|num_|qty)/.test(n)) return 'count';
    return 'plain';
  }

  function formatWithKind(v, kind) {
    if (v == null) return '';
    switch (kind) {
      case 'currency_inr': {
        if (typeof v !== 'number') v = Number(v);
        if (!isFinite(v)) return '—';
        // Indian numbering style is part of en-IN; INR symbol via currency.
        try {
          return new Intl.NumberFormat('en-IN', {
            style: 'currency', currency: 'INR',
            maximumFractionDigits: 0,
          }).format(v);
        } catch {
          return '₹' + Math.round(v).toLocaleString();
        }
      }
      case 'count':
        return Number(v).toLocaleString();
      default:
        return formatValue(v);
    }
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
    }[c]));
  }
})();

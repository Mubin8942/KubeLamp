'use strict';
/* KubeLamp — shared helpers, charts, tables, dialogs. Vanilla JS, no build step. */
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const main = $('#main');
const S = { clusters: [], kc: null, timers: [], abort: null, sort: {}, view: null, gen: 0, pvc: {} };

// ------------------------------------------------------------------ helpers
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const enc = encodeURIComponent;
// KubeLamp may be served under a sub-path by a reverse proxy (https://host/kubelamp/):
// every request is relative to the page's own folder, never to the site root
const BASE = location.pathname.replace(/[^/]*$/, '');
const apiURL = path => BASE + 'api/' + path;
// the session ended (expired, signed out elsewhere, password reset): sign in again
function authLost(r, d) {
  if (typeof Auth === 'undefined') return;
  if (r.status === 401 && d.login) Auth.signIn('Your session ended. Sign in again.').then(() => location.reload());
  else if (r.status === 403 && d.mustChange) location.reload();
}
const httpErr = (r, d) => (authLost(r, d), new Error(d.error || `${r.status} ${r.statusText || 'error'}${r.headers.get('content-type')?.includes('json') ? '' : ` from ${new URL(r.url).pathname} — is the reverse proxy forwarding ${BASE}api/ to KubeLamp?`}`));
async function api(path) {
  const r = await fetch(apiURL(path), { signal: S.abort?.signal });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw httpErr(r, d);
  return d;
}
function fmtCPU(c) {
  if (c == null || isNaN(c)) return '—';
  if (c === 0) return '0';
  if (c < 1) return Math.round(c * 1000) + 'm';
  return (c < 10 ? c.toFixed(2) : c.toFixed(1)).replace(/\.?0+$/, '');
}
function fmtMem(b) {
  if (b == null || isNaN(b)) return '—';
  if (b === 0) return '0';
  const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (b < 10 && i > 0 ? +b.toFixed(1) : Math.round(b)) + ' ' + u[i];
}
function fmtNum(n) { return n >= 10000 ? (n / 1000).toFixed(n >= 100000 ? 0 : 1) + 'K' : n.toLocaleString(); }
function age(iso) {
  if (!iso) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return 'now';
  if (s < 120) return Math.round(s) + 's';
  if (s < 7200) return Math.round(s / 60) + 'm';
  if (s < 172800) return Math.round(s / 3600) + 'h';
  return Math.round(s / 86400) + 'd';
}
const pct = (a, b) => b > 0 ? a / b * 100 : 0;
const hhmm = t => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const hhmmss = t => new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
const ctxPath = c => '#/c/' + enc(c);
function every(ms, fn) { const id = setInterval(fn, ms); S.timers.push(id); }
function css(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

function statusClass(st) {
  if (['Running', 'Succeeded', 'Completed', 'healthy', 'Ready'].includes(st)) return 'good';
  if (['Pending', 'ContainerCreating', 'PodInitializing', 'progressing', 'Terminating', 'scaled-down'].includes(st)) return 'warning';
  return 'critical';
}
const stLabel = (st, cls) => `<span class="st ${cls || statusClass(st)}">${esc(st)}</span>`;

function meter(used, alloc, req) {
  const p = Math.min(100, pct(used, alloc));
  const cls = p >= 90 ? 'critical' : p >= 75 ? 'warning' : '';
  const rq = req != null && alloc > 0 ? `<b style="left:calc(${Math.min(100, pct(req, alloc))}% - 1px)" title="requested"></b>` : '';
  return `<div class="meter"><i class="${cls}" style="width:${p}%"></i>${rq}</div>`;
}
function mcell(used, alloc, req, fmt, hasUsage = true) {
  const right = alloc > 0 ? `${pct(used, alloc).toFixed(0)}%` : '';
  return `<div class="mcell">${meter(hasUsage ? used : 0, alloc, req)}
    <div class="nums"><span>${hasUsage ? fmt(used) : '—'}${alloc > 0 ? ' / ' + fmt(alloc) : ''}</span><span>${hasUsage ? right : ''}</span></div></div>`;
}
function spark(h, key = 'cpu') {
  const v = h?.[key] || [];
  if (v.length < 2) return '<span class="muted">—</span>';
  const w = 90, ht = 24, max = Math.max(...v) || 1, min = Math.min(...v);
  const span = max - min || max || 1;
  const pts = v.map((y, i) => [i / (v.length - 1) * (w - 4) + 1, ht - 3 - (y - min) / span * (ht - 6)]);
  const d = 'M' + pts.map(p => p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' L');
  const e = pts[pts.length - 1];
  return `<svg class="spark" viewBox="0 0 ${w} ${ht}"><path d="${d}"/><circle class="end" cx="${e[0]}" cy="${e[1]}" r="2.5"/></svg>`;
}

// ------------------------------------------------------------------ tooltip
const tip = $('#tip');
function showTip(html, x, y) {
  tip.innerHTML = html; tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let left = x + 14, top = y + 14;
  if (left + r.width > innerWidth - 8) left = x - r.width - 14;
  if (top + r.height > innerHeight - 8) top = y - r.height - 14;
  tip.style.left = left + 'px'; tip.style.top = top + 'px';
}
const hideTip = () => { tip.hidden = true; };

// ------------------------------------------------------------------ line chart
// One measure per chart (no dual axis). Usage is the series; requests / limits /
// allocatable are labelled reference lines.
// Clean tick steps: decimal for CPU cores, binary (Mi/Gi) for bytes.
function niceScale(max, bytes) {
  if (!(max > 0)) max = bytes ? 1 << 30 : 1;
  let steps;
  if (bytes) { steps = []; for (let e = 20; e <= 50; e++) steps.push(2 ** e); }
  else { steps = []; for (let e = -3; e <= 4; e++) for (const m of [1, 2, 2.5, 5]) steps.push(m * 10 ** e); }
  const step = steps.find(st => max / st <= 4) || steps[steps.length - 1];
  return { top: Math.ceil(max / step) * step, step };
}
function lineChart(el, { t, series, refs = [], fmt }) {
  const W = el.clientWidth || 600, H = el.clientHeight || 220;
  const m = { l: 50, r: 16, t: 10, b: 22 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;
  const valid = t.length >= 2;
  // Scale to the data (and requests), not to capacity, so movement stays visible.
  // Far-away references (allocatable, limits) are labelled at the top instead.
  let dmax = 0;
  series.forEach(s => s.data.forEach(v => { if (v > dmax) dmax = v; }));
  refs.filter(r => !r.soft).forEach(r => { if (r.value > dmax) dmax = r.value; });
  refs.filter(r => r.soft && r.value > 0 && r.value <= dmax * 2).forEach(r => { dmax = Math.max(dmax, r.value); });
  const { top: ymax, step } = niceScale(dmax * 1.12, fmt === fmtMem);
  const x0 = valid ? t[0] : 0, x1 = valid ? t[t.length - 1] : 1;
  const X = v => m.l + (v - x0) / ((x1 - x0) || 1) * iw;
  const Y = v => m.t + ih - v / ymax * ih;
  let g = '';
  for (let v = 0; v <= ymax + step / 2; v += step) {
    const y = Y(v), i = v;
    g += `<line class="${i === 0 ? 'base' : 'gridl'}" x1="${m.l}" x2="${W - m.r}" y1="${y}" y2="${y}"/>`;
    g += `<text class="tick" x="${m.l - 8}" y="${y + 4}" text-anchor="end">${esc(fmt(v))}</text>`;
  }
  if (!valid) {
    el.innerHTML = `<svg>${g}<text class="collect" x="${m.l + iw / 2}" y="${m.t + ih / 2}" text-anchor="middle">Collecting data — collecting history</text></svg>`;
    return;
  }
  // time labels: fewer when the history is short, and never the same minute twice
  const n = Math.max(1, Math.min(5, Math.floor((x1 - x0) / 60)));
  let lastLbl = '';
  for (let i = 0; i <= n; i++) {
    const tv = x0 + (x1 - x0) * i / n, lbl = hhmm(tv);
    if (lbl === lastLbl) continue;
    lastLbl = lbl;
    g += `<text class="tick" x="${X(tv)}" y="${H - 5}" text-anchor="${i === 0 ? 'start' : i === n ? 'end' : 'middle'}">${lbl}</text>`;
  }
  let above = 0;
  const placed = [];
  refs.slice().sort((a, b) => b.value - a.value).forEach(r => {
    if (!(r.value > 0)) return;
    if (r.value > ymax) {
      g += `<text class="reflbl" x="${W - m.r - 4}" y="${m.t + 12 + above++ * 14}" text-anchor="end">↑ ${esc(r.name)} ${esc(fmt(r.value))}</text>`;
      return;
    }
    const y = Y(r.value);
    // keep labels of nearby reference lines from overlapping: flip below the line
    let ly = y - 5;
    if (placed.some(p => Math.abs(p - ly) < 13)) ly = y + 13;
    placed.push(ly);
    g += `<line class="ref" x1="${m.l}" x2="${W - m.r}" y1="${y}" y2="${y}"/>`;
    g += `<text class="reflbl" x="${W - m.r - 4}" y="${ly}" text-anchor="end">${esc(r.name)} ${esc(fmt(r.value))}</text>`;
  });
  series.forEach(s => {
    const pts = s.data.map((v, i) => `${X(t[i]).toFixed(1)},${Y(v).toFixed(1)}`);
    if (s.area) g += `<path d="M${X(t[0])},${Y(0)} L${pts.join(' L')} L${X(t[t.length - 1])},${Y(0)} Z" fill="${s.color}" fill-opacity=".10"/>`;
    g += `<path d="M${pts.join(' L')}" fill="none" stroke="${s.color}" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`;
    const li = s.data.length - 1;
    g += `<circle cx="${X(t[li])}" cy="${Y(s.data[li])}" r="4" fill="${s.color}" stroke="${css('--surface')}" stroke-width="2"/>`;
  });
  g += `<line class="xhair" y1="${m.t}" y2="${m.t + ih}" visibility="hidden"/><g class="hdots"></g>`;
  g += `<rect class="hit" x="${m.l}" y="${m.t}" width="${iw}" height="${ih}" fill="transparent"/>`;
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${g}</svg>`;
  const svg = $('svg', el), xh = $('.xhair', el), dots = $('.hdots', el);
  $('.hit', el).addEventListener('pointermove', e => {
    const r = svg.getBoundingClientRect();
    const px = (e.clientX - r.left) / r.width * W;
    const tv = x0 + (px - m.l) / iw * (x1 - x0);
    let i = 0, best = Infinity;
    t.forEach((v, k) => { const d = Math.abs(v - tv); if (d < best) { best = d; i = k; } });
    const x = X(t[i]);
    xh.setAttribute('x1', x); xh.setAttribute('x2', x); xh.setAttribute('visibility', 'visible');
    dots.innerHTML = series.map(s => `<circle cx="${x}" cy="${Y(s.data[i])}" r="4" fill="${s.color}" stroke="${css('--surface')}" stroke-width="2"/>`).join('');
    showTip(`<div class="tt">${hhmmss(t[i])}</div>` +
      series.map(s => `<div class="row"><i style="background:${s.color}"></i><strong>${esc(fmt(s.data[i]))}</strong><span>${esc(s.name)}</span></div>`).join('') +
      refs.filter(r => r.value > 0).map(r => `<div class="row"><i style="background:${css('--ink-2')};height:1px"></i><strong>${esc(fmt(r.value))}</strong><span>${esc(r.name)}</span></div>`).join(''),
      e.clientX, e.clientY);
  });
  $('.hit', el).addEventListener('pointerleave', () => { xh.setAttribute('visibility', 'hidden'); dots.innerHTML = ''; hideTip(); });
}
function chartCard(id, title, caption, refs) {
  const keys = [`<span><i style="background:var(--accent)"></i>Usage</span>`]
    .concat(refs.filter(r => r.value > 0).map(r => `<span><i class="refkey"></i>${esc(r.name)}</span>`));
  return `<div class="card"><div class="card-head"><h3>${esc(title)}</h3><span class="cap">${esc(caption)}</span></div>
    <div class="legend">${keys.join('')}</div><div class="chart" id="${id}"></div></div>`;
}
function drawUsageCharts(prefix, h, cpuRefs, memRefs) {
  const draw = () => {
    const a = $('#' + prefix + 'Cpu'), b = $('#' + prefix + 'Mem');
    if (a) lineChart(a, { t: h.t, series: [{ name: 'Usage', data: h.cpu, color: css('--accent'), area: true }], refs: cpuRefs, fmt: fmtCPU });
    if (b) lineChart(b, { t: h.t, series: [{ name: 'Usage', data: h.mem, color: css('--accent'), area: true }], refs: memRefs, fmt: fmtMem });
  };
  draw();
  S.redraw = draw;
}
window.addEventListener('resize', () => S.redraw && S.redraw());

// horizontal bars (single series, value at tip, tooltip on hover)
function hbars(rows, valueKey, fmt, limit = 10) {
  const list = rows.slice().sort((a, b) => valueKey(b) - valueKey(a)).slice(0, limit).filter(r => valueKey(r) > 0);
  if (!list.length) return '<div class="muted">No usage data</div>';
  const max = valueKey(list[0]) || 1;
  return `<div class="hbars">${list.map((r, i) => `<div class="hbar" data-i="${i}"><span class="lbl">${esc(r.name)}</span>
    <div class="bar"><i style="width:${valueKey(r) / max * 100}%"></i></div><span class="val">${esc(fmt(valueKey(r)))}</span></div>`).join('')}</div>`;
}
function bindHbarTips(container, rows, valueKey, kind) {
  const list = rows.slice().sort((a, b) => valueKey(b) - valueKey(a)).slice(0, 10).filter(r => valueKey(r) > 0);
  $$('.hbar', container).forEach(el => {
    const r = list[+el.dataset.i];
    el.addEventListener('pointermove', e => showTip(`<div class="tt">${esc(r.name)} · ${r.pods} pods</div>
      <div class="row"><strong>${kind === 'cpu' ? fmtCPU(r.cpu.used) : fmtMem(r.mem.used)}</strong><span>used</span></div>
      <div class="row"><strong>${kind === 'cpu' ? fmtCPU(r.cpu.req) : fmtMem(r.mem.req)}</strong><span>requested</span></div>
      <div class="row"><strong>${kind === 'cpu' ? fmtCPU(r.cpu.lim) : fmtMem(r.mem.lim)}</strong><span>limits</span></div>`, e.clientX, e.clientY));
    el.addEventListener('pointerleave', hideTip);
  });
}

// ------------------------------------------------------------------ sortable table
function table(id, cols, rows, opts = {}) {
  const st = S.sort[id] || opts.sort || {};
  if (st.key) {
    const col = cols.find(c => c.key === st.key);
    if (col) {
      const val = col.sort || (r => r[col.key]);
      rows = rows.slice().sort((a, b) => {
        const x = val(a), y = val(b);
        const c = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y));
        return st.dir === 'desc' ? -c : c;
      });
    }
  }
  const limit = opts.limit || 2000;
  const head = cols.map(c => `<th class="${c.num ? 'num ' : ''}${c.nosort ? '' : 'sort'}" data-k="${c.key}">${esc(c.label)}${st.key === c.key ? `<span class="arr"> ${st.dir === 'desc' ? '↓' : '↑'}</span>` : ''}</th>`).join('');
  const body = rows.slice(0, limit).map((r, i) => `<tr class="${opts.href ? 'link' : ''}" data-i="${i}">${cols.map(c => `<td class="${c.cls || ''}${c.num ? ' num' : ''}">${c.render ? c.render(r, i) : esc(r[c.key])}</td>`).join('')}</tr>`).join('');
  const more = rows.length > limit ? `<div class="muted" style="padding:8px 10px">Showing ${limit} of ${rows.length} — narrow with the filters.</div>` : '';
  const html = `<div class="tbl-wrap"><table class="t" data-tid="${id}"><thead><tr>${head}</tr></thead><tbody>${body || `<tr><td colspan="${cols.length}" class="muted">Nothing here</td></tr>`}</tbody></table>${more}</div>`;
  return { html, rows: rows.slice(0, limit), bind(container, rerender) {
    const tblEl = $(`table[data-tid="${id}"]`, container);
    if (!tblEl) return;
    $$('th.sort', tblEl).forEach(th => th.addEventListener('click', () => {
      const k = th.dataset.k, cur = S.sort[id] || opts.sort || {};
      S.sort[id] = { key: k, dir: cur.key === k && cur.dir === 'asc' ? 'desc' : cur.key === k ? 'asc' : (cols.find(c => c.key === k).num ? 'desc' : 'asc') };
      rerender();
    }));
    if (opts.href) $$('tbody tr.link', tblEl).forEach(tr => tr.addEventListener('click', () => { location.hash = opts.href(this.rows[+tr.dataset.i]); }));
  } };
}


// ------------------------------------------------------------------ writes, dialogs, toasts, popovers
async function send(method, path, body) {
  const r = await fetch(apiURL(path), { method, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw httpErr(r, d);
  return d;
}

function toast(msg, kind = '') {
  const t = document.createElement('div');
  t.className = 'toast ' + kind;
  t.textContent = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), kind === 'err' ? 8000 : 3500);
}

// modal({ title, body (HTML), actions: [{ label, kind, value }] }) → resolves with the chosen value (or null)
function modal({ title, body = '', actions = [], wide = false, init }) {
  return new Promise(resolve => {
    const d = document.createElement('dialog');
    d.innerHTML = `<form class="dlg ${wide ? 'wide' : ''}" method="dialog"><h3>${esc(title)}</h3>${body}
      <div class="err-line" data-err></div>
      <div class="actions">${actions.map((a, i) => `<button class="btn ${a.kind || ''}" value="${i}" ${a.submit ? 'type="submit"' : 'type="button"'}>${esc(a.label)}</button>`).join('')}</div></form>`;
    document.body.appendChild(d);
    const done = v => { d.close(); d.remove(); resolve(v); };
    const form = $('form', d);
    const run = async a => {
      if (!a.run) return done(a.value ?? null);
      const btns = $$('button', d); btns.forEach(b => b.disabled = true);
      try { done(await a.run(d)); } catch (e) { $('[data-err]', d).textContent = e.message; btns.forEach(b => b.disabled = false); }
    };
    $$('.actions button', d).forEach((b, i) => b.addEventListener('click', e => { e.preventDefault(); run(actions[i]); }));
    form.addEventListener('submit', e => { e.preventDefault(); const a = actions.find(x => x.submit); if (a) run(a); });
    d.addEventListener('cancel', e => { e.preventDefault(); done(null); });
    d.showModal();
    if (init) init(d);
    ($('input, textarea', d) || $('button.primary, button.solid', d))?.focus();
  });
}

function confirmDanger(title, message, label = 'Delete') {
  return modal({ title, body: `<p>${message}</p>`, actions: [{ label: 'Cancel', value: false }, { label, kind: 'danger solid', value: true, submit: true }] });
}

// popover anchored to an element; items: [{ label, sub, icon, on, danger, run }] or 'sep'
const pop = $('#pop');
let popAnchor = null;
function closePop() { pop.hidden = true; popAnchor = null; }
function openPop(anchor, items, { search = false, placeholder = 'Filter…', side = '', title = '' } = {}) {
  if (popAnchor === anchor) { closePop(); return; }
  popAnchor = anchor;
  const render = (q = '') => {
    const list = items.filter(it => it === 'sep' || it.head || !q || (it.label + ' ' + (it.sub || '')).toLowerCase().includes(q));
    $('.opts', pop).innerHTML = list.map((it, i) => it === 'sep' ? '<hr>' : it.head ? `<div class="pop-head">${esc(it.head)}</div>` :
      `<button class="opt ${it.on ? 'on' : ''} ${it.danger ? 'danger' : ''}" data-i="${items.indexOf(it)}" type="button">${it.icon || ''}<span>${esc(it.label)}</span>${it.sub ? `<span class="sub">${esc(it.sub)}</span>` : ''}</button>`).join('')
      || '<div class="muted" style="padding:8px 10px">No matches</div>';
    $$('.opt', pop).forEach(b => b.addEventListener('click', () => { const it = items[+b.dataset.i]; closePop(); it.run && it.run(); }));
  };
  pop.innerHTML = (title ? `<div class="pop-title">${esc(title)}</div>` : '') + (search ? `<input type="search" placeholder="${esc(placeholder)}" spellcheck="false">` : '') + '<div class="opts"></div>';
  render();
  pop.hidden = false;
  const r = anchor.getBoundingClientRect(), pr = pop.getBoundingClientRect();
  if (side === 'right') { // flyout next to the collapsed sidebar
    pop.style.top = Math.max(8, Math.min(r.top - 4, innerHeight - pr.height - 8)) + 'px';
    const sr = $('#side').getBoundingClientRect();
    pop.style.left = Math.max(r.right + 8, sr.right + 6) + 'px';
  } else {
    pop.style.top = Math.min(r.bottom + 6, innerHeight - pr.height - 8) + 'px';
    pop.style.left = Math.max(8, Math.min(r.left, innerWidth - pr.width - 8)) + 'px';
    if (r.right > innerWidth - 240) pop.style.left = Math.max(8, r.right - pr.width) + 'px';
  }
  if (search) {
    const inp = $('input', pop);
    inp.focus();
    inp.addEventListener('input', () => render(inp.value.toLowerCase()));
    inp.addEventListener('keydown', e => {
      if (e.key === 'Enter') { const first = $('.opt', pop); first && first.click(); }
      if (e.key === 'Escape') closePop();
    });
  }
}
document.addEventListener('mousedown', e => { if (!pop.hidden && !pop.contains(e.target) && !(popAnchor && popAnchor.contains(e.target))) closePop(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape' && !pop.hidden) closePop(); });

const ICON = {
  logs: '<svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h10"/></svg>',
  term: '<svg viewBox="0 0 24 24"><rect x="3" y="4.5" width="18" height="15" rx="2.5"/><path d="M7 10l3 2.5L7 15M12.5 15H17"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M13.5 6.5l4 4"/></svg>',
  del: '<svg viewBox="0 0 24 24"><path d="M5 7h14M10 7V4.5h4V7M7 7l1 13h8l1-13"/></svg>',
  scale: '<svg viewBox="0 0 24 24"><path d="M4 12h16M12 4v16"/></svg>',
  restart: '<svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7"/></svg>',
  eye: '<svg viewBox="0 0 24 24"><path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5.5A1.5 1.5 0 0 0 14.5 4h-9A1.5 1.5 0 0 0 4 5.5v9A1.5 1.5 0 0 0 5.5 16H8"/></svg>',
  more: '<svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="1.4"/><circle cx="12" cy="12" r="1.4"/><circle cx="19" cy="12" r="1.4"/></svg>',
  pause: '<svg viewBox="0 0 24 24"><path d="M8 5v14M16 5v14"/></svg>',
  play: '<svg viewBox="0 0 24 24"><path d="M7 5l12 7-12 7z"/></svg>',
};


// ------------------------------------------------------------------ volume usage (PVC) helpers
const fmtBytes = b => {
  const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti', 'Pi']; let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1).replace(/\.0$/, '')) + u[i];
};
const PVC_WARN = 80, PVC_CRIT = 90;
function pvcCell(u) {
  if (!u) return '<span class="muted">checking…</span>';
  if (u.state === 'ok') {
    const p = Math.min(100, u.pct), cls = p >= PVC_CRIT ? 'critical' : p >= PVC_WARN ? 'warning' : '';
    const tip = `${u.pod}/${u.container}:${u.path}${u.approx ? ' (subPath: whole volume shown)' : ''} · read ${age(u.at)} ago${u.cached ? ' (cached)' : ''}`;
    return `<div class="mcell" title="${esc(tip)}"><div class="meter"><i class="${cls}" style="width:${p}%"></i></div>
      <div class="nums"><span>${u.approx ? '~' : ''}${fmtBytes(u.used)} / ${fmtBytes(u.size)}</span><span class="${cls ? 'pct-' + cls : ''}">${p.toFixed(0)}%</span></div></div>`;
  }
  const m = { unmounted: ['not mounted by a running pod', ''], unbound: ['not bound', ''], disabled: ['needs exec (read-only mode)', ''], error: ['can’t read usage', u.err] }[u.state] || ['—', ''];
  return `<span class="muted" title="${esc(m[1])}">${m[0]}</span>`;
}
const pvcKey = (ctx, ns, name) => `${ctx}/${ns}/${name}`;
async function loadPvcUsage(ctx, ns, { refresh = false, name = '' } = {}) {
  const d = await api(`pvcusage?ctx=${enc(ctx)}&ns=${enc(ns)}${name ? '&name=' + enc(name) : ''}${refresh ? '&refresh=1' : ''}`);
  d.pvcs.forEach(u => { S.pvc[pvcKey(ctx, u.namespace, u.name)] = u; });
  return d;
}

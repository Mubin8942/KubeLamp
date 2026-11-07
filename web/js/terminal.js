'use strict';
/* Terminal drawer: local shells (kubectl preset to the open cluster) and pod exec
   sessions as tabs. Sessions survive page navigation. */

const Term = (() => {
  const drawer = $('#term'), body = $('#termBody'), tabsEl = $('#termTabs');
  const tabs = [];
  let active = null, seq = 0;
  let hlOn = (() => { try { return localStorage.getItem('kd.term.hl') !== '0'; } catch { return true; } })();
  const theme = {
    background: '#0b1016', foreground: '#e6edf3', cursor: '#8fb3ff', cursorAccent: '#0b1016',
    selectionBackground: 'rgba(143,179,255,.28)',
    black: '#1b2430', red: '#ef6b6b', green: '#4cc38a', yellow: '#e5b84a', blue: '#6ea0ff', magenta: '#c792ea', cyan: '#56c8d8', white: '#d5dde7',
    brightBlack: '#5c6b7d', brightRed: '#ff8a8a', brightGreen: '#6fdca4', brightYellow: '#f2cc6b', brightBlue: '#8fb3ff', brightMagenta: '#dcb0f5', brightCyan: '#7fdde9', brightWhite: '#ffffff',
  };
  const wsURL = path => (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/' + path;

  function show(open = true) {
    drawer.hidden = !open;
    $('#termToggle').classList.toggle('on', open);
    try { localStorage.setItem('kd.term.open', open ? '1' : ''); } catch {}
    if (open) {
      if (!tabs.length) newShell();
      requestAnimationFrame(fitActive);
    }
  }
  const toggle = () => show(drawer.hidden);

  function renderTabs() {
    tabsEl.innerHTML = tabs.map(t => `<button class="term-tab ${t.kind} ${t === active ? 'on' : ''} ${t.dead ? 'dead' : ''}" data-id="${t.id}" role="tab" title="${esc(t.tip)}">
      <svg class="ico" viewBox="0 0 24 24">${t.kind === 'exec' ? '<rect x="4" y="4" width="16" height="16" rx="3"/><path d="M8.5 10l2.5 2-2.5 2M13 14.5h3"/>' : '<path d="M5 8l4 4-4 4M11 17h8"/>'}</svg>
      <span>${esc(t.title)}</span><span class="x" data-close="${t.id}" title="Close"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg></span></button>`).join('');
    $$('.term-tab', tabsEl).forEach(b => b.addEventListener('click', e => {
      const id = +b.dataset.id;
      if (e.target.closest('[data-close]')) close(id); else select(id);
    }));
    if (!tabs.length) body.innerHTML = `<div class="term-empty"><div>No sessions.<br><button class="btn" style="margin-top:10px" id="termStart">${ICON.term}Open a shell</button></div></div>`;
    $('#termStart')?.addEventListener('click', () => newShell());
  }

  function select(id) {
    active = tabs.find(t => t.id === id) || null;
    tabs.forEach(t => { t.pane.hidden = t !== active; });
    renderTabs();
    if (active) requestAnimationFrame(() => { fitActive(); active.term.focus(); });
  }

  function close(id) {
    const i = tabs.findIndex(t => t.id === id);
    if (i < 0) return;
    const t = tabs[i];
    try { t.ws && t.ws.close(); } catch {}
    t.term.dispose(); t.pane.remove();
    tabs.splice(i, 1);
    if (active === t) active = tabs[Math.max(0, i - 1)] || null;
    if (active) select(active.id); else renderTabs();
  }

  function fitActive() {
    if (!active || drawer.hidden) return;
    try { active.fit.fit(); } catch {}
    sendSize(active);
  }
  function sendSize(t) {
    if (t.ws && t.ws.readyState === 1) t.ws.send(JSON.stringify({ t: 'r', cols: t.term.cols, rows: t.term.rows }));
  }

  function open({ title, tip, kind, path }) {
    body.querySelector('.term-empty')?.remove();
    const pane = document.createElement('div');
    pane.className = 'term-pane';
    body.appendChild(pane);
    const term = new Terminal({ fontFamily: '"Plex Mono", ui-monospace, Menlo, Consolas, monospace', fontSize: 13, lineHeight: 1.2,
      cursorBlink: true, theme, scrollback: 5000, allowProposedApi: false });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(pane);
    const t = { id: ++seq, title, tip, kind, path, term, fit, pane, ws: null, dead: false, hl: TermHL.create(), dec: new TextDecoder(), hlTimer: 0 };
    tabs.push(t);
    connect(t);
    term.onData(d => { if (t.ws && t.ws.readyState === 1) t.ws.send(JSON.stringify({ t: 'i', d })); else if (t.dead && d === '\r') { t.dead = false; renderTabs(); connect(t); } });
    show(true);
    select(t.id);
    return t;
  }

  function connect(t) {
    const ws = new WebSocket(wsURL(t.path));
    ws.binaryType = 'arraybuffer';
    t.ws = ws;
    t.hl.reset(); t.dec = new TextDecoder();
    ws.onopen = () => { fitActive(); sendSize(t); };
    ws.onmessage = ev => {
      if (typeof ev.data === 'string') {
        try { const m = JSON.parse(ev.data); if (m.t === 'exit') { t.dead = true; t.term.write(`\r\n\x1b[2m[${m.msg || 'session ended'} — press Enter to reconnect]\x1b[0m\r\n`); renderTabs(); } } catch {}
        return;
      }
      if (!hlOn) { t.term.write(new Uint8Array(ev.data)); return; }
      clearTimeout(t.hlTimer);
      t.term.write(t.hl.write(t.dec.decode(ev.data, { stream: true })));
      // a word cut at the end of a chunk is held for the next one; show it soon if nothing follows
      if (t.hl.hasPending()) t.hlTimer = setTimeout(() => t.term.write(t.hl.flush()), 15);
    };
    ws.onclose = () => {
      if (!t.dead) { t.dead = true; t.term.write('\r\n\x1b[2m[disconnected — press Enter to reconnect]\x1b[0m\r\n'); renderTabs(); }
    };
  }

  function newShell(ctx = parseRoute().ctx) {
    if (S.info && !S.info.shell) {
      toast(S.info.readOnly ? 'The shell is disabled in read-only mode.' : 'The shell is only available when KubeLamp listens on localhost.', 'err');
      return;
    }
    const n = tabs.filter(t => t.kind === 'shell').length + 1;
    return open({ kind: 'shell', title: ctx ? `${shortName(ctx)}${n > 1 ? ' ' + n : ''}` : `shell ${n}`,
      tip: ctx ? `Local shell · kubectl uses context ${ctx}` : 'Local shell', path: 'shell?ctx=' + enc(ctx || '') });
  }

  function openExec(ctx, ns, pod, container) {
    const short = pod.length > 28 ? '…' + pod.slice(-26) : pod;
    return open({ kind: 'exec', title: short + (container ? ` · ${container}` : ''), tip: `${ctx} · ${ns}/${pod}${container ? ' [' + container + ']' : ''}`,
      path: `exec?ctx=${enc(ctx)}&ns=${enc(ns)}&pod=${enc(pod)}${container ? '&container=' + enc(container) : ''}` });
  }

  // resizing the drawer by dragging its top edge
  let drag = null;
  $('#termGrip').addEventListener('pointerdown', e => { drag = { y: e.clientY, h: drawer.offsetHeight }; e.target.setPointerCapture(e.pointerId); });
  $('#termGrip').addEventListener('pointermove', e => {
    if (!drag) return;
    const h = Math.max(140, Math.min(innerHeight - 160, drag.h + (drag.y - e.clientY)));
    drawer.style.height = h + 'px';
    fitActive();
  });
  $('#termGrip').addEventListener('pointerup', () => { if (drag) { try { localStorage.setItem('kd.term.h', drawer.offsetHeight); } catch {} } drag = null; });
  try { const h = +localStorage.getItem('kd.term.h'); if (h) drawer.style.height = h + 'px'; } catch {}
  window.addEventListener('resize', fitActive);
  $('#termToggle').addEventListener('click', toggle);
  $('#termHide').addEventListener('click', () => show(false));
  $('#termNew').addEventListener('click', () => newShell());
  const hlBtn = $('#termHL');
  const drawHL = () => {
    hlBtn.classList.toggle('on', hlOn);
    hlBtn.title = `Highlighting ${hlOn ? 'on' : 'off'}: colours IPs, URLs, MAC addresses, timestamps and words like error, warning, success in the output. Click to turn ${hlOn ? 'off' : 'on'}.`;
  };
  hlBtn.addEventListener('click', () => {
    hlOn = !hlOn;
    try { localStorage.setItem('kd.term.hl', hlOn ? '1' : '0'); } catch {}
    tabs.forEach(t => { clearTimeout(t.hlTimer); if (t.hl.hasPending()) t.term.write(t.hl.flush()); t.hl.reset(); t.dec = new TextDecoder(); });
    drawHL();
    toast(`Terminal highlighting ${hlOn ? 'on' : 'off'} (applies to new output)`);
  });
  drawHL();
  document.addEventListener('keydown', e => { if (e.ctrlKey && (e.key === '`' || e.code === 'Backquote')) { e.preventDefault(); toggle(); } });
  renderTabs();
  return { show, toggle, newShell, openExec };
})();

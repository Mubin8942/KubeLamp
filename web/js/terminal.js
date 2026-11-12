'use strict';
/* Terminal drawer: local shells (kubectl preset to the open cluster) and pod exec sessions.
   Sessions survive page navigation. Views: tabs (one at a time) or split (all sessions in a
   grid). Broadcast sends what you type to every session that has broadcast ticked, Termius style.
   Fullscreen makes the drawer fill the whole window. */

const Term = (() => {
  const drawer = $('#term'), body = $('#termBody'), tabsEl = $('#termTabs');
  const tabs = [];
  let active = null, seq = 0;
  const pref = (k, d) => { try { const v = localStorage.getItem(k); return v === null ? d : v === '1'; } catch { return d; } };
  const save = (k, v) => { try { localStorage.setItem(k, v ? '1' : '0'); } catch {} };
  let hlOn = pref('kd.term.hl', true);
  let split = pref('kd.term.split', false);
  let full = false, broadcast = false;
  const theme = {
    background: '#0b1016', foreground: '#e6edf3', cursor: '#8fb3ff', cursorAccent: '#0b1016',
    selectionBackground: 'rgba(143,179,255,.28)',
    black: '#1b2430', red: '#ef6b6b', green: '#4cc38a', yellow: '#e5b84a', blue: '#6ea0ff', magenta: '#c792ea', cyan: '#56c8d8', white: '#d5dde7',
    brightBlack: '#5c6b7d', brightRed: '#ff8a8a', brightGreen: '#6fdca4', brightYellow: '#f2cc6b', brightBlue: '#8fb3ff', brightMagenta: '#dcb0f5', brightCyan: '#7fdde9', brightWhite: '#ffffff',
  };
  const wsURL = path => (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + apiURL(path);
  const tabIcon = t => `<svg class="ico" viewBox="0 0 24 24">${t.kind === 'exec' ? '<rect x="4" y="4" width="16" height="16" rx="3"/><path d="M8.5 10l2.5 2-2.5 2M13 14.5h3"/>' : '<path d="M5 8l4 4-4 4M11 17h8"/>'}</svg>`;

  // hidden with sessions still open: a floating bar at the bottom brings them back
  function updateMin() {
    const min = $('#termMin');
    min.hidden = !drawer.hidden || !tabs.length;
    if (min.hidden) return;
    const live = tabs.filter(t => !t.dead).length;
    $('#termMinTxt').textContent = tabs.length === 1 ? `Terminal · ${tabs[0].title}` : `Terminal · ${tabs.length} sessions${live < tabs.length ? ` (${tabs.length - live} closed)` : ''}`;
  }
  function show(open = true) {
    drawer.hidden = !open;
    updateMin();
    $('#termToggle').classList.toggle('on', open);
    try { localStorage.setItem('kd.term.open', open ? '1' : ''); } catch {}
    if (!open && full) setFull(false);
    if (open) {
      if (!tabs.length) newShell();
      requestAnimationFrame(fitAll);
    }
  }
  const toggle = () => show(drawer.hidden);

  // ---- layout
  const targets = () => tabs.filter(t => t.bc && !t.dead && t.ws && t.ws.readyState === 1);
  function renderTabs() {
    updateMin();
    tabsEl.innerHTML = tabs.map(t => `<button class="term-tab ${t.kind} ${t === active ? 'on' : ''} ${t.dead ? 'dead' : ''} ${broadcast && t.bc ? 'bc' : ''}" data-id="${t.id}" role="tab" title="${esc(t.tip)}">
      ${tabIcon(t)}<span>${esc(t.title)}</span><span class="x" data-close="${t.id}" title="Close"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg></span></button>`).join('');
    $$('.term-tab', tabsEl).forEach(b => b.addEventListener('click', e => {
      const id = +b.dataset.id;
      if (e.target.closest('[data-close]')) close(id); else select(id);
    }));
    body.querySelector('.term-empty')?.remove();
    if (!tabs.length) body.insertAdjacentHTML('beforeend', `<div class="term-empty"><div>No sessions.<br><button class="btn" style="margin-top:10px" id="termStart">${ICON.term}Open a shell</button></div></div>`);
    $('#termStart')?.addEventListener('click', () => newShell());
    drawState();
  }
  function drawState() {
    const n = targets().length;
    drawer.classList.toggle('split', split && tabs.length > 0);
    drawer.classList.toggle('full', full);
    drawer.classList.toggle('broadcasting', broadcast);
    $('#termSplit').classList.toggle('on', split);
    $('#termFull').classList.toggle('on', full);
    $('#termBc').classList.toggle('on', broadcast);
    $('#termFull').title = full ? 'Exit fullscreen (Ctrl+Shift+F)' : 'Fullscreen terminal (Ctrl+Shift+F)';
    $('#termSplit').title = split ? 'Show one session at a time (tabs)' : 'Show all sessions side by side (Ctrl+Shift+S)';
    $('#termBc').title = broadcast ? 'Stop broadcasting (Ctrl+Shift+B)' : 'Broadcast: send what you type to several sessions at once (Ctrl+Shift+B)';
    const bar = $('#termBcBar');
    bar.hidden = !broadcast;
    if (broadcast) {
      bar.innerHTML = `<b>Broadcasting</b> to ${n} of ${tabs.length} session${tabs.length === 1 ? '' : 's'}: everything you type goes to each ticked session.
        <button type="button" data-bc="all">Tick all</button><button type="button" data-bc="none">Untick all</button><button type="button" data-bc="off">Stop</button>`;
      $$('[data-bc]', bar).forEach(b => b.onclick = () => {
        if (b.dataset.bc === 'off') return setBroadcast(false);
        tabs.forEach(t => { t.bc = b.dataset.bc === 'all'; });
        drawCells(); renderTabs();
      });
    }
    // grid shape
    if (split) {
      const c = tabs.length <= 1 ? 1 : tabs.length <= 4 ? 2 : tabs.length <= 9 ? 3 : 4;
      body.style.gridTemplateColumns = `repeat(${c}, minmax(0, 1fr))`;
      body.style.gridTemplateRows = `repeat(${Math.ceil(tabs.length / c) || 1}, minmax(0, 1fr))`;
    } else { body.style.gridTemplateColumns = body.style.gridTemplateRows = ''; }
    drawCells();
  }
  function drawCells() {
    for (const t of tabs) {
      t.cell.hidden = !split && t !== active;
      t.cell.classList.toggle('on', t === active);
      t.cell.classList.toggle('bc', broadcast && t.bc);
      const html = `${tabIcon(t)}<span class="term-ct" title="${esc(t.tip)}">${esc(t.title)}</span>${t.dead ? '<span class="term-dead">disconnected</span>' : ''}
        <span class="grow"></span>
        ${broadcast ? `<label class="term-bcchk" title="Receive broadcast input"><input type="checkbox" ${t.bc ? 'checked' : ''}> broadcast</label>` : ''}
        <span class="x" title="Close"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg></span>`;
      if (t.headHTML !== html) { t.head.innerHTML = html; t.headHTML = html; } // only redraw when it changes, so clicks land
    }
  }

  function select(id) {
    active = tabs.find(t => t.id === id) || null;
    renderTabs();
    if (active) requestAnimationFrame(() => { fitAll(); active.term.focus(); });
  }

  function close(id) {
    const i = tabs.findIndex(t => t.id === id);
    if (i < 0) return;
    const t = tabs[i];
    try { t.ws && t.ws.close(); } catch {}
    t.term.dispose(); t.cell.remove();
    tabs.splice(i, 1);
    if (active === t) active = tabs[Math.max(0, i - 1)] || null;
    if (active) select(active.id); else renderTabs();
    requestAnimationFrame(fitAll);
  }

  function fitAll() {
    if (drawer.hidden) return;
    for (const t of tabs) {
      if (t.cell.hidden) continue;
      try { t.fit.fit(); } catch {}
      sendSize(t);
    }
  }
  function sendSize(t) {
    if (t.ws && t.ws.readyState === 1 && (t.cols !== t.term.cols || t.rows !== t.term.rows || t.sizeDirty)) {
      t.cols = t.term.cols; t.rows = t.term.rows; t.sizeDirty = false;
      t.ws.send(JSON.stringify({ t: 'r', cols: t.term.cols, rows: t.term.rows }));
    }
  }

  // ---- modes
  function setFull(on) {
    full = on;
    if (on && drawer.hidden) show(true);
    drawState();
    requestAnimationFrame(() => { fitAll(); active && active.term.focus(); });
  }
  function setSplit(on) {
    split = on; save('kd.term.split', on);
    drawState();
    requestAnimationFrame(() => { fitAll(); active && active.term.focus(); });
  }
  function setBroadcast(on) {
    broadcast = on;
    if (on && !tabs.some(t => t.bc)) tabs.forEach(t => { t.bc = true; });
    if (on && !split && tabs.length > 1) setSplit(true);
    renderTabs();
    active && active.term.focus();
  }

  // keyboard shortcuts work while the terminal has focus (Ctrl+Shift+letter is not used by shells)
  function shortcut(e) {
    if (e.type !== 'keydown' || !e.ctrlKey || !e.shiftKey || e.altKey || e.metaKey) return false;
    const k = e.key.toLowerCase();
    if (k === 'f') { setFull(!full); return true; }
    if (k === 's') { setSplit(!split); return true; }
    if (k === 'b') { setBroadcast(!broadcast); return true; }
    return false;
  }

  function open({ title, tip, kind, path, focus = true }) {
    const cell = document.createElement('div');
    cell.className = 'term-cell';
    const head = document.createElement('div');
    head.className = 'term-ph';
    const pane = document.createElement('div');
    pane.className = 'term-pane';
    cell.append(head, pane);
    body.appendChild(cell);
    const term = new Terminal({ fontFamily: '"Plex Mono", ui-monospace, Menlo, Consolas, monospace', fontSize: 13, lineHeight: 1.2,
      cursorBlink: true, theme, scrollback: 5000, allowProposedApi: false });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(pane);
    const t = { id: ++seq, title, tip, kind, path, term, fit, cell, head, pane, ws: null, dead: false, bc: true,
      hl: TermHL.create(), dec: new TextDecoder(), hlTimer: 0 };
    tabs.push(t);
    connect(t);
    term.attachCustomKeyEventHandler(e => {
      if (shortcut(e)) { e.preventDefault(); return false; }
      return true;
    });
    term.onData(d => {
      if (t.dead) { if (d === '\r') { t.dead = false; renderTabs(); connect(t); } return; }
      const to = broadcast && t.bc ? targets() : [t];
      const msg = JSON.stringify({ t: 'i', d });
      for (const x of to) if (x.ws && x.ws.readyState === 1) x.ws.send(msg);
    });
    head.addEventListener('click', e => { if (e.target.closest('.x')) { e.stopPropagation(); close(t.id); } });
    head.addEventListener('change', e => { if (e.target.matches('input[type=checkbox]')) { t.bc = e.target.checked; renderTabs(); } });
    cell.addEventListener('mousedown', () => { if (active !== t) { active = t; renderTabs(); } });
    term.textarea?.addEventListener('focus', () => { if (active !== t) { active = t; renderTabs(); } });
    show(true);
    if (focus) select(t.id); else renderTabs();
    requestAnimationFrame(fitAll);
    return t;
  }

  function connect(t) {
    const ws = new WebSocket(wsURL(t.path));
    ws.binaryType = 'arraybuffer';
    t.ws = ws;
    t.hl.reset(); t.dec = new TextDecoder(); t.sizeDirty = true;
    ws.onopen = () => { try { t.fit.fit(); } catch {} sendSize(t); drawState(); };
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
      toast(S.info.readOnly ? 'The shell is disabled in read-only mode.' : !S.info.admin ? 'The local shell runs on the KubeLamp server, so it is for admins only. Use “Exec” on a pod instead.' : 'The shell is only available when KubeLamp listens on localhost.', 'err');
      return;
    }
    const n = tabs.filter(t => t.kind === 'shell').length + 1;
    return open({ kind: 'shell', title: ctx ? `${shortName(ctx)}${n > 1 ? ' ' + n : ''}` : `shell ${n}`,
      tip: ctx ? `Local shell · kubectl uses context ${ctx}` : 'Local shell', path: 'shell?ctx=' + enc(ctx || '') });
  }

  function openExec(ctx, ns, pod, container, opts = {}) {
    const short = pod.length > 28 ? '…' + pod.slice(-26) : pod;
    return open({ kind: 'exec', title: short + (container ? ` · ${container}` : ''), tip: `${ctx} · ${ns}/${pod}${container ? ' [' + container + ']' : ''}`,
      path: `exec?ctx=${enc(ctx)}&ns=${enc(ns)}&pod=${enc(pod)}${container ? '&container=' + enc(container) : ''}`, focus: opts.focus !== false });
  }

  // one exec session per pod, side by side, broadcasting to all of them
  const MULTI_MAX = 12;
  function openMulti(ctx, ns, pods, container) {
    const list = pods.slice(0, MULTI_MAX);
    if (pods.length > MULTI_MAX) toast(`Opened the first ${MULTI_MAX} of ${pods.length} pods`, 'err');
    let first = null;
    list.forEach((p, i) => { const t = openExec(ctx, ns, p, container, { focus: i === 0 }); first = first || t; });
    const opened = new Set(tabs.slice(-list.length));
    tabs.forEach(t => { t.bc = opened.has(t); });
    setSplit(true);
    setBroadcast(true);
    if (first) select(first.id);
  }

  // resizing the drawer by dragging its top edge
  let drag = null;
  $('#termGrip').addEventListener('pointerdown', e => { if (full) return; drag = { y: e.clientY, h: drawer.offsetHeight }; e.target.setPointerCapture(e.pointerId); });
  $('#termGrip').addEventListener('pointermove', e => {
    if (!drag) return;
    const h = Math.max(140, Math.min(innerHeight - 160, drag.h + (drag.y - e.clientY)));
    drawer.style.height = h + 'px';
    fitAll();
  });
  $('#termGrip').addEventListener('pointerup', () => { if (drag) { try { localStorage.setItem('kd.term.h', drawer.offsetHeight); } catch {} } drag = null; });
  try { const h = +localStorage.getItem('kd.term.h'); if (h) drawer.style.height = h + 'px'; } catch {}
  new ResizeObserver(() => requestAnimationFrame(fitAll)).observe(body);
  window.addEventListener('resize', fitAll);
  $('#termToggle').addEventListener('click', toggle);
  $('#termHide').addEventListener('click', () => show(false));
  $('#termMin').addEventListener('click', () => show(true));
  $('#termNew').addEventListener('click', () => newShell());
  $('#termFull').addEventListener('click', () => setFull(!full));
  $('#termSplit').addEventListener('click', () => setSplit(!split));
  $('#termBc').addEventListener('click', () => setBroadcast(!broadcast));
  document.addEventListener('keydown', e => {
    if (e.ctrlKey && (e.key === '`' || e.code === 'Backquote')) { e.preventDefault(); toggle(); return; }
    if (!drawer.hidden && !e.target.closest?.('.xterm') && shortcut(e)) e.preventDefault();
  });
  const hlBtn = $('#termHL');
  const drawHL = () => {
    hlBtn.classList.toggle('on', hlOn);
    hlBtn.title = `Highlighting ${hlOn ? 'on' : 'off'}: colours IPs, URLs, MAC addresses, timestamps and words like error, warning, success in the output. Click to turn ${hlOn ? 'off' : 'on'}.`;
  };
  hlBtn.addEventListener('click', () => {
    hlOn = !hlOn;
    save('kd.term.hl', hlOn);
    tabs.forEach(t => { clearTimeout(t.hlTimer); if (t.hl.hasPending()) t.term.write(t.hl.flush()); t.hl.reset(); t.dec = new TextDecoder(); });
    drawHL();
    toast(`Terminal highlighting ${hlOn ? 'on' : 'off'} (applies to new output)`);
  });
  drawHL();
  renderTabs();
  return { show, toggle, newShell, openExec, openMulti, setFull, setSplit, setBroadcast, _tabs: tabs };
})();

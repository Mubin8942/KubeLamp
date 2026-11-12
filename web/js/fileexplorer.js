'use strict';
/* Tools → Files → File Explorer: browse a container's folders and read any file its user can read. */

const FX_SIZES = [[16384, '16 KiB'], [65536, '64 KiB'], [262144, '256 KiB'], [524288, '512 KiB']];
const FX_MAX_LINES = 20000;

async function viewFileExplorer(route) {
  const ctx = route.ctx;
  const key = 'kl.explorer.' + ctx;
  const roOnly = !!(S.info && S.info.readOnly);
  const saved = (() => { try { return JSON.parse(localStorage.getItem(key) || '{}'); } catch { return {}; } })();
  const F = Object.assign({ ns: S.ns || '', pod: '', container: '', path: '/', hidden: true, wrap: true, nums: true, bytes: 65536 }, saved);
  let nss = [], pods = [], podInfo = null, list = null, filter = '', user = '';
  let V = null; // the open file: { path, entry, mode, data, err, loading, find, only }
  try { nss = await api('namespaces?ctx=' + enc(ctx)); } catch {}
  if (!F.ns || !nss.some(n => n.name === F.ns)) F.ns = (S.ns && nss.some(n => n.name === S.ns) ? S.ns : (nss.find(n => n.pods) || nss[0] || {}).name) || '';
  const persist = () => { try { localStorage.setItem(key, JSON.stringify(F)); } catch {} };
  const qs = (o = {}) => new URLSearchParams(Object.assign({ ctx, ns: F.ns, pod: F.pod, container: F.container }, o)).toString();
  const ctrMounts = () => ((podInfo && podInfo.containers.find(c => c.name === F.container)) || { mounts: [] }).mounts;

  main.innerHTML = `
    <div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/tools">Tools</a></div>
      <h1>File Explorer</h1>
      <div class="sub"><span>Browse a container's folders and read any file its user is allowed to read. Read-only: nothing is changed in the pod.</span></div></div></div>
    ${roOnly ? `<div class="banner err"><b>Read-only mode.</b><span>This tool runs commands inside pods, so it is disabled.</span></div>` : ''}
    <div class="card ct-form">
      <div class="ct-row">
        <label class="ct-f"><span>Namespace</span><select id="fxNs">${nss.map(n => `<option value="${esc(n.name)}" ${n.name === F.ns ? 'selected' : ''}>${esc(n.name)}${n.pods ? ` (${n.pods})` : ''}</option>`).join('')}</select></label>
        <label class="ct-f"><span>Pod</span><select id="fxPod" style="min-width:300px"></select></label>
        <label class="ct-f"><span>Container</span><select id="fxCtr"></select></label>
        <div class="ct-f"><span>Runs as</span><span class="mono" id="fxUser" title="Files are read with this user's permissions">-</span></div>
      </div>
    </div>
    <div id="fxBody"></div>`;
  const ns$ = $('#fxNs'), pod$ = $('#fxPod'), ctr$ = $('#fxCtr'), body$ = $('#fxBody');

  async function loadPods() {
    pods = [];
    try { pods = (await api('pods?ctx=' + enc(ctx) + '&ns=' + enc(F.ns))).pods.filter(p => p.phase === 'Running'); } catch {}
    if (!pods.some(p => p.name === F.pod)) { F.pod = (pods[0] || {}).name || ''; F.container = ''; }
    pod$.innerHTML = pods.length ? pods.map(p => `<option value="${esc(p.name)}" ${p.name === F.pod ? 'selected' : ''}>${esc(p.name)}</option>`).join('') : '<option value="">No running pods</option>';
    await loadPod();
  }
  async function loadPod() {
    podInfo = null; list = null; V = null; user = '';
    $('#fxUser').textContent = '-';
    if (!F.pod || roOnly) { ctr$.innerHTML = ''; draw(); return; }
    try { podInfo = await ctSend('GET', 'tools/files/pod?' + qs()); } catch (e) { body$.innerHTML = `<div class="banner err"><b>Cannot read the pod.</b><span>${esc(e.message)}</span></div>`; return; }
    const cs = podInfo.containers;
    if (!cs.some(c => c.name === F.container && c.running)) F.container = (cs.find(c => c.running) || cs[0] || {}).name || '';
    ctr$.innerHTML = cs.map(c => `<option value="${esc(c.name)}" ${c.name === F.container ? 'selected' : ''} ${c.running ? '' : 'disabled'}>${esc(c.name)}${c.running ? '' : ' (not running)'}</option>`).join('');
    persist();
    await go(F.path || '/');
  }

  let seq = 0;
  async function go(p) {
    const my = ++seq;
    p = (p || '/').trim() || '/';
    F.path = p; persist();
    list = null; draw();
    try {
      const d = await ctSend('GET', 'tools/files/list?' + qs({ path: p }));
      if (my !== seq) return;
      list = d; F.path = d.path; user = (d.user || user || '').replace(/\s+groups=.*/, '');
      persist();
    } catch (e) { if (my !== seq) return; list = { path: p, error: e.message, entries: [] }; }
    $('#fxUser').textContent = user || '-';
    draw();
  }

  async function openFile(entry) {
    const path = ftJoin(F.path, entry.n);
    V = { path, entry, mode: V && V.path === path ? V.mode : 'head', data: null, err: '', loading: true, find: V && V.path === path ? V.find : '', only: V ? V.only : false };
    drawViewer();
    const my = V;
    try {
      const d = await ctSend('GET', 'tools/files/view?' + qs({ path, mode: V.mode, bytes: F.bytes }));
      if (V !== my) return;
      V.data = d;
    } catch (e) { if (V !== my) return; V.err = e.message; }
    V.loading = false;
    drawViewer();
  }

  function draw() {
    if (roOnly || !F.pod) { body$.innerHTML = F.pod || roOnly ? '' : '<div class="card muted">No running pods in this namespace.</div>'; return; }
    const mounts = ctrMounts();
    const q = filter.trim().toLowerCase();
    const ents = (list?.entries || []).filter(e => (F.hidden || !e.n.startsWith('.')) && (!q || e.n.toLowerCase().includes(q)));
    const segs = F.path.split('/').filter(Boolean);
    const crumbs = `<a href="#" data-go="/">/</a>` + segs.map((s, i) => `<a href="#" data-go="/${esc(segs.slice(0, i + 1).join('/'))}">${esc(s)}</a>`).join('<span class="muted">/</span>');
    let rows;
    if (!list) rows = '<div class="muted fx-pad">Loading…</div>';
    else if (list.error) rows = `<div class="ct-prev err" style="margin:8px">${esc(list.error)}</div>`;
    else if (!ents.length) rows = `<div class="muted fx-pad">${q ? 'Nothing matches the filter.' : 'This folder is empty.'}</div>`;
    else rows = ents.map(e => {
      const i = list.entries.indexOf(e);
      const icon = e.dir ? '📁' : e.k === 'l' ? '🔗' : '📄';
      const on = V && V.path === ftJoin(F.path, e.n);
      return `<button class="fx-row ${on ? 'on' : ''}" data-i="${i}" type="button" title="${esc(e.n)}${e.mode ? ' · ' + esc(e.mode) + ' ' + esc(e.u || '') : ''}"><span class="fx-ic">${icon}</span><span class="fx-n">${esc(e.n)}</span><span class="fx-s">${e.file ? fmtBytes(e.s) : ''}</span></button>`;
    }).join('');
    body$.innerHTML = `
      <div class="fx-grid">
        <div class="card fx-left">
          <div class="ft-bar"><button class="btn sm" id="fxUp" title="Parent folder">↑</button><div class="fx-crumbs" id="fxCrumbs">${crumbs}</div><button class="btn sm" id="fxRef" title="Reload">↻</button></div>
          <input type="text" id="fxPath" class="mono fx-path" spellcheck="false" value="${esc(F.path)}" title="Type a path and press Enter">
          <div class="ft-meta">${ftVolBadge(mounts, F.path)}</div>
          <div class="ft-meta fx-mounts">${mounts.map(m => `<button class="chip ft-chip" data-go="${esc(m.path)}" title="${esc(m.kind)}${m.readOnly ? ' (read-only)' : ''}">${esc(m.path)}</button>`).join('')}</div>
          <div class="ft-bar" style="margin:8px 0"><input type="search" id="fxFilter" placeholder="Filter…" value="${esc(filter)}" style="flex:1;min-width:0">
            <label class="chk" title="Show files starting with a dot"><input type="checkbox" id="fxHidden" ${F.hidden ? 'checked' : ''}> hidden</label></div>
          <div class="muted fx-count">${list && !list.error ? `${ents.length} of ${list.entries.length}${list.more ? '+' : ''} items${list.more ? ` (first ${list.max} shown)` : ''}` : ''}</div>
          <div class="fx-list">${rows}</div>
        </div>
        <div class="card fx-right" id="fxView"></div>
      </div>`;
    $$('[data-go]', body$).forEach(a => a.onclick = ev => { ev.preventDefault(); go(a.dataset.go); });
    $$('.fx-row', body$).forEach(b => b.onclick = () => { const e = list.entries[+b.dataset.i]; if (e.dir) { V = null; go(ftJoin(F.path, e.n)); } else openFile(e); });
    $('#fxUp').onclick = () => { V = null; go(ftParent(F.path)); };
    $('#fxRef').onclick = () => go(F.path);
    $('#fxPath').onkeydown = e => { if (e.key === 'Enter') { V = null; go(e.target.value); } };
    $('#fxFilter').oninput = e => { filter = e.target.value; const pos = e.target.selectionStart; draw(); const f = $('#fxFilter'); f.focus(); f.setSelectionRange(pos, pos); };
    $('#fxHidden').onchange = e => { F.hidden = e.target.checked; persist(); draw(); };
    drawViewer();
  }

  // ---- viewer
  const hl = (line, q) => {
    if (!q) return esc(line);
    const lo = line.toLowerCase(), ql = q.toLowerCase();
    let out = '', i = 0;
    for (;;) {
      const j = lo.indexOf(ql, i);
      if (j < 0) { out += esc(line.slice(i)); break; }
      out += esc(line.slice(i, j)) + '<mark>' + esc(line.slice(j, j + q.length)) + '</mark>';
      i = j + q.length;
    }
    return out;
  };
  function drawViewer() {
    const el = $('#fxView');
    if (!el) return;
    if (!V) {
      el.innerHTML = `<div class="fx-empty muted"><div style="font-size:30px">📄</div>Select a file on the left to read it.<br><small>You can read any file the container's user ${user ? `(<span class="mono">${esc(user)}</span>) ` : ''}has permission for. At most 512 KiB is read at a time.</small></div>`;
      return;
    }
    const e = V.entry, d = V.data;
    el.innerHTML = `
      <div class="fx-vhead"><div><div class="mono fx-vpath">${esc(V.path)}</div>
        <div class="muted fx-vmeta">${e.file ? fmtBytes(e.s) : ''}${e.m ? ' · ' + esc(ftWhen(e.m)) : ''}${e.mode ? ' · ' + esc(e.mode) : ''}${e.u ? ' · ' + esc(e.u) : ''}</div></div>
        <button class="btn sm" id="fxClose" title="Close">✕</button></div>
      <div class="ft-bar fx-tools">
        <div class="seg" id="fxMode"><button type="button" data-m="head" class="${V.mode === 'head' ? 'on' : ''}">Start</button><button type="button" data-m="tail" class="${V.mode === 'tail' ? 'on' : ''}">End</button></div>
        <select id="fxBytes" data-label="Read" title="How much to read">${FX_SIZES.map(([n, l]) => `<option value="${n}" ${n === F.bytes ? 'selected' : ''}>${l}</option>`).join('')}</select>
        <input type="search" id="fxFind" placeholder="Find in text…" value="${esc(V.find)}" style="width:170px">
        <label class="chk"><input type="checkbox" id="fxOnly" ${V.only ? 'checked' : ''}> only matching lines</label>
        <label class="chk"><input type="checkbox" id="fxWrap" ${F.wrap ? 'checked' : ''}> wrap</label>
        <label class="chk" title="Not shown for the end of a file, where the real line numbers are unknown"><input type="checkbox" id="fxNums" ${F.nums ? 'checked' : ''}> line numbers</label>
        <span style="flex:1"></span>
        <button class="btn sm" id="fxCopy" ${d && d.text ? '' : 'disabled'}>Copy</button>
        <button class="btn sm" id="fxXfer" title="Open this folder in File Transfer to download the whole file">Download…</button>
      </div>
      <div id="fxNote"></div>
      <div id="fxText" class="fx-text"></div>`;
    $('#fxClose').onclick = () => { V = null; draw(); };
    $$('#fxMode button').forEach(b => b.onclick = () => { if (V.mode !== b.dataset.m) { V.mode = b.dataset.m; openFile(V.entry); } });
    $('#fxBytes').onchange = ev => { F.bytes = +ev.target.value; persist(); openFile(V.entry); };
    $('#fxFind').oninput = ev => { V.find = ev.target.value; drawText(); };
    $('#fxOnly').onchange = ev => { V.only = ev.target.checked; drawText(); };
    $('#fxWrap').onchange = ev => { F.wrap = ev.target.checked; persist(); drawText(); };
    $('#fxNums').onchange = ev => { F.nums = ev.target.checked; persist(); drawText(); };
    $('#fxCopy').onclick = async () => { try { await navigator.clipboard.writeText(V.data.text); toast('Copied the shown text'); } catch { toast('Could not copy', 'err'); } };
    $('#fxXfer').onclick = () => {
      try { localStorage.setItem('kl.files.' + ctx, JSON.stringify({ ns: F.ns, pod: F.pod, container: F.container, path: F.path })); } catch {}
      location.hash = toolsHref(ctx, 'files');
    };
    drawText();
  }
  function drawText() {
    const box = $('#fxText'), note = $('#fxNote');
    if (!box || !V) return;
    box.className = 'fx-text' + (F.wrap ? ' wrap' : '');
    if (V.loading) { box.innerHTML = '<div class="muted fx-pad">Reading…</div>'; note.innerHTML = ''; return; }
    if (V.err) { note.innerHTML = ''; box.innerHTML = `<div class="ct-prev err" style="margin:10px">${esc(V.err)}</div>`; return; }
    const d = V.data;
    const part = V.mode === 'tail' ? 'last' : 'first';
    const notes = [];
    if (d.truncated) notes.push(`Showing the ${part} ${fmtBytes(d.shown)}${d.size > d.shown ? ' of ' + fmtBytes(d.size) : ''}. ${V.mode === 'head' ? 'Choose “End” for the last part, or a larger size (up to 512 KiB).' : 'Choose “Start” for the beginning, or a larger size (up to 512 KiB).'} Use Download… for the whole file.`);
    if (!d.shown) notes.push('The file is empty.');
    note.innerHTML = notes.map(n => `<div class="fx-note">${esc(n)}</div>`).join('');
    if (d.binary) {
      note.innerHTML += `<div class="fx-note warn">This looks like a binary file. Showing a hex preview of the first bytes; use Download… to get it.</div>`;
      box.className = 'fx-text';
      box.innerHTML = `<pre class="fx-hex">${esc(d.hex)}</pre>`;
      return;
    }
    let lines = d.text.split('\n');
    if (lines.length && lines[lines.length - 1] === '') lines.pop();
    const q = V.find;
    let shown = lines.map((t, i) => ({ t, n: i + 1 }));
    let matches = 0;
    if (q) { const ql = q.toLowerCase(); shown.forEach(l => { if (l.t.toLowerCase().includes(ql)) matches++; }); if (V.only) shown = shown.filter(l => l.t.toLowerCase().includes(ql)); }
    const over = shown.length > FX_MAX_LINES;
    if (over) shown = shown.slice(0, FX_MAX_LINES);
    box.innerHTML = shown.length ? shown.map(l => `<div class="fx-line">${F.nums && V.mode !== 'tail' ? `<span class="fx-ln">${l.n}</span>` : ''}<span class="fx-lt">${hl(l.t, q) || ' '}</span></div>`).join('') + (over ? `<div class="fx-note">Only the first ${FX_MAX_LINES} lines are drawn; use the filter or a smaller size.</div>` : '')
      : `<div class="muted fx-pad">${q ? 'No line contains that text.' : ''}</div>`;
    if (q) note.innerHTML += `<div class="fx-note">${matches} line${matches === 1 ? '' : 's'} match${matches === 1 ? 'es' : ''} in the text shown.</div>`;
  }

  ns$.onchange = () => { F.ns = ns$.value; F.pod = ''; F.container = ''; F.path = '/'; persist(); loadPods(); };
  pod$.onchange = () => { F.pod = pod$.value; F.container = ''; F.path = '/'; persist(); loadPod(); };
  ctr$.onchange = () => { F.container = ctr$.value; F.path = '/'; persist(); loadPod(); };
  await loadPods();
}

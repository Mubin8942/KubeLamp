'use strict';
/* Tools → Pod File Transfer: browse, download and upload files inside running containers. */

const FT_QUICK = [1, 5, 10, 25, 50, 100, 256, 512, 1024, 2048];
const ftJoin = (dir, name) => (dir === '/' ? '' : dir.replace(/\/+$/, '')) + '/' + name;
const ftParent = p => { const i = p.replace(/\/+$/, '').lastIndexOf('/'); return i <= 0 ? '/' : p.slice(0, i); };
const ftWhen = s => s ? new Date(s * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';

function ftMountOf(mounts, p) {
  let best = null;
  for (const m of mounts) if ((m.path === '/' || p === m.path || p.startsWith(m.path.replace(/\/+$/, '') + '/')) && (!best || m.path.length > best.path.length)) best = m;
  return best;
}
function ftVolBadge(mounts, p) {
  const m = ftMountOf(mounts, p);
  if (!m) return `<span class="ft-vol warn" title="Files here live in the container's own writable layer: they count against the node's ephemeral storage and disappear when the container restarts.">Container filesystem · not persistent</span>`;
  const ro = m.readOnly ? ' · read-only' : '';
  const t = { pvc: `PVC ${m.claim || m.detail || ''}`, emptyDir: 'emptyDir', configMap: 'ConfigMap', secret: 'Secret', projected: 'Projected volume', hostPath: 'hostPath', nfs: 'NFS', cephfs: 'CephFS' }[m.kind] || m.kind;
  return `<span class="ft-vol ${m.readOnly ? 'warn' : 'good'}" title="${esc(m.detail || '')}">${esc(t)}${m.subPath ? ' · subPath ' + esc(m.subPath) : ''}${ro}</span>`;
}

function ftXhr(url, file, onProgress, hold) {
  return new Promise(resolve => {
    const x = new XMLHttpRequest();
    hold.abort = () => x.abort();
    x.open('POST', url);
    x.setRequestHeader('Content-Type', 'application/octet-stream');
    x.upload.onprogress = e => e.lengthComputable && onProgress(e.loaded / e.total);
    x.onload = () => { let d = {}; try { d = JSON.parse(x.responseText); } catch { d = { error: x.responseText || x.statusText }; } resolve({ status: x.status, data: d }); };
    x.onerror = () => resolve({ status: 0, data: { error: 'network error: is KubeLamp still running?' } });
    x.onabort = () => resolve({ status: 0, data: { error: 'cancelled', code: 'cancelled' } });
    x.send(file);
  });
}

async function viewFileTransfer(route) {
  const ctx = route.ctx;
  const key = 'kl.files.' + ctx;
  const roOnly = !!(S.info && S.info.readOnly);
  const saved = (() => { try { return JSON.parse(localStorage.getItem(key) || '{}'); } catch { return {}; } })();
  const F = Object.assign({ ns: S.ns || '', pod: '', container: '', path: '/' }, saved);
  let nss = [], pods = [], podInfo = null, list = null, alts = [], viaIdx = -1, filter = '';
  let lim = { maxDownload: 256 << 20, maxUpload: 100 << 20, rate: 20 << 20 };
  try { lim = await ctSend('GET', 'tools/files/info'); } catch {}
  try { nss = await api('namespaces?ctx=' + enc(ctx)); } catch {}
  if (!F.ns || !nss.some(n => n.name === F.ns)) F.ns = (S.ns && nss.some(n => n.name === S.ns) ? S.ns : (nss.find(n => n.pods) || nss[0] || {}).name) || '';
  const persist = () => { try { localStorage.setItem(key, JSON.stringify({ ns: F.ns, pod: F.pod, container: F.container, path: F.path })); } catch {} };
  const qs = (o = {}) => new URLSearchParams(Object.assign({ ctx, ns: F.ns, pod: F.pod, container: F.container }, o)).toString();

  main.innerHTML = `
    <div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/tools">Tools</a></div>
      <h1>File Transfer</h1>
      <div class="sub"><span>Browse a container's files, download logs and assets, upload files. Works through the pod exec API; no agent is installed in the pod.</span></div></div></div>
    ${roOnly ? `<div class="banner err"><b>Read-only mode.</b><span>This tool runs commands inside pods, so it is disabled.</span></div>` : ''}
    <div class="card ct-form">
      <div class="ct-row">
        <label class="ct-f"><span>Namespace</span><select id="ftNs">${nss.map(n => `<option value="${esc(n.name)}" ${n.name === F.ns ? 'selected' : ''}>${esc(n.name)}${n.pods ? ` (${n.pods})` : ''}</option>`).join('')}</select></label>
        <label class="ct-f"><span>Pod</span><select id="ftPod" style="min-width:300px"></select></label>
        <label class="ct-f"><span>Container</span><select id="ftCtr"></select></label>
      </div>
      <div class="muted ft-limits">Download up to <b>${fmtBytes(lim.maxDownload)}</b> per file or folder, upload up to <b>${fmtBytes(lim.maxUpload)}</b>, at most <b>${fmtBytes(lim.rate)}/s</b> per transfer and 2 transfers at a time, so a copy cannot overload the pod, the node or the API server.</div>
    </div>
    <div id="ftBody"></div>`;

  const ns$ = $('#ftNs'), pod$ = $('#ftPod'), ctr$ = $('#ftCtr'), body$ = $('#ftBody');

  const drawPods = () => {
    pod$.innerHTML = pods.length ? pods.map(p => `<option value="${esc(p.name)}" ${p.name === F.pod ? 'selected' : ''}>${esc(p.name)}</option>`).join('') : '<option value="">No running pods</option>';
  };
  async function loadPods() {
    pods = [];
    try { pods = (await api('pods?ctx=' + enc(ctx) + '&ns=' + enc(F.ns))).pods.filter(p => p.phase === 'Running'); } catch {}
    if (!pods.some(p => p.name === F.pod)) { F.pod = (pods[0] || {}).name || ''; F.container = ''; }
    drawPods();
    await loadPod();
  }
  async function loadPod() {
    podInfo = null; list = null; alts = []; viaIdx = -1;
    if (!F.pod || roOnly) { ctr$.innerHTML = ''; drawBody(); return; }
    try { podInfo = await ctSend('GET', 'tools/files/pod?' + qs()); } catch (e) { body$.innerHTML = `<div class="banner err"><b>Cannot read the pod.</b><span>${esc(e.message)}</span></div>`; return; }
    const cs = podInfo.containers;
    if (!cs.some(c => c.name === F.container && c.running)) F.container = (cs.find(c => c.running) || cs[0] || {}).name || '';
    ctr$.innerHTML = cs.map(c => `<option value="${esc(c.name)}" ${c.name === F.container ? 'selected' : ''} ${c.running ? '' : 'disabled'}>${esc(c.name)}${c.running ? '' : ' (not running)'}</option>`).join('');
    const mounts = ctrMounts();
    if (!F.path || F.path === '/') F.path = mounts.find(m => m.kind === 'pvc')?.path || mounts[0]?.path || '/';
    persist();
    await go(F.path);
  }
  const ctrMounts = () => ((podInfo && podInfo.containers.find(c => c.name === F.container)) || { mounts: [] }).mounts;
  const volBadge = p => ftVolBadge(ctrMounts(), p);

  // ---- directory listing
  let seq = 0;
  async function go(p) {
    const my = ++seq;
    p = (p || '/').trim() || '/';
    F.path = p; persist();
    list = null; drawBody();
    try {
      const d = await ctSend('GET', 'tools/files/list?' + qs({ path: p }));
      if (my !== seq) return;
      list = d;
      F.path = d.path; persist();
    } catch (e) { if (my !== seq) return; list = { path: p, error: e.message, entries: [] }; }
    drawBody();
    loadAlts(my);
  }
  async function loadAlts(my) {
    const prev = alts[viaIdx]?.pod;
    alts = [];
    try { alts = (await ctSend('GET', 'tools/files/targets?' + qs({ path: F.path }))).alternatives || []; } catch {}
    if (my !== seq) return;
    viaIdx = alts.findIndex(a => a.pod === prev && prev);
    drawVia();
  }
  function drawVia() {
    const el = $('#ftVia');
    if (!el) return;
    el.hidden = !alts.length;
    el.innerHTML = alts.length ? `<span>Upload through</span><select id="ftViaSel"><option value="-1">this container (${esc(F.container)})</option>${alts.map((a, i) => `<option value="${i}" ${i === viaIdx ? 'selected' : ''}>${esc(a.pod)} / ${esc(a.container)} → ${esc(a.path)}</option>`).join('')}</select>
      <span class="muted" title="These containers mount the same volume. Use one (for example your ncdu pod) when the application's own container cannot write.">same volume</span>` : '';
    $('#ftViaSel')?.addEventListener('change', e => { viaIdx = +e.target.value; });
  }

  function drawBody() {
    if (roOnly || !F.pod) {
      body$.innerHTML = F.pod || roOnly ? '' : '<div class="card muted">No running pods in this namespace.</div>';
      return;
    }
    const mounts = ctrMounts();
    const q = filter.trim().toLowerCase();
    const ents = (list?.entries || []).filter(e => !q || e.n.toLowerCase().includes(q));
    let rows = '';
    if (!list) rows = '<tr><td colspan="5" class="muted">Loading…</td></tr>';
    else if (list.error) rows = `<tr><td colspan="5"><div class="ct-prev err">${esc(list.error)}</div></td></tr>`;
    else if (!ents.length) rows = `<tr><td colspan="5" class="muted">${q ? 'Nothing matches the filter.' : 'This folder is empty.'}</td></tr>`;
    else rows = ents.map(e => {
      const i = list.entries.indexOf(e);
      const icon = e.dir ? '📁' : e.k === 'l' ? '🔗' : '📄';
      const name = e.dir ? `<a href="#" data-open="${i}" class="ft-name">${esc(e.n)}${e.k === 'l' ? ' <span class="muted">(link)</span>' : ''}</a>` : `<span class="ft-name">${esc(e.n)}</span>`;
      const big = e.file && e.s > lim.maxDownload;
      return `<tr><td class="ft-ic">${icon}</td><td>${name}</td><td class="num">${e.file ? fmtBytes(e.s) : ''}</td><td class="muted">${esc(ftWhen(e.m))}</td>
        <td class="ft-act">${e.dir || e.file ? `<button class="btn sm" data-dl="${i}" title="${big ? 'Larger than the download limit: you can download the last part' : e.dir ? 'Download the folder as a tar archive' : 'Download'}">${e.dir ? 'Download folder' : big ? 'Download tail…' : 'Download'}</button>` : ''}</td></tr>`;
    }).join('');
    body$.innerHTML = `
      <div class="card ft-card" id="ftDrop">
        <div class="ft-bar">
          <button class="btn" id="ftUp" title="Parent folder">↑ Up</button>
          <input type="text" id="ftPath" class="mono" spellcheck="false" value="${esc(F.path)}">
          <button class="btn" id="ftGo">Go</button>
          <button class="btn" id="ftRef" title="Reload this folder">↻</button>
        </div>
        <div class="ft-meta">${volBadge(F.path)}
          <span class="ft-mounts">${mounts.map(m => `<button class="chip ft-chip" data-path="${esc(m.path)}" title="${esc(m.kind)}${m.claim ? ' ' + esc(m.claim) : ''}${m.readOnly ? ' (read-only)' : ''}">${esc(m.path)}</button>`).join('')}</span></div>
        <div class="ft-bar" style="margin-top:10px"><input type="search" id="ftFilter" placeholder="Filter this folder…" value="${esc(filter)}" style="max-width:260px">
          <span class="muted">${list && !list.error ? `${list.entries.length}${list.more ? '+' : ''} item${list.entries.length === 1 ? '' : 's'}${list.more ? ` (first ${list.max} shown)` : ''}` : ''}</span></div>
        <div class="ct-scroll" style="margin-top:8px"><table class="ct-matrix ft-table"><thead><tr><th></th><th>Name</th><th class="num">Size</th><th>Modified</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>
      </div>
      <div class="card ft-up">
        <div class="ft-uphead"><b>Upload to <span class="mono">${esc(F.path)}</span></b>
          <div class="ft-via" id="ftVia" hidden></div></div>
        <div class="ft-drop"><input type="file" id="ftFiles" multiple hidden><button class="btn primary" id="ftPick">Choose files…</button>
          <span class="muted">or drop them on this page. Up to ${fmtBytes(lim.maxUpload)} each. Files are written to a hidden temporary name, checked, then moved into place.</span></div>
        <div id="ftQueue"></div>
      </div>`;
    drawVia();
    drawQueue();
    bindBody();
  }

  function bindBody() {
    $('#ftGo').onclick = () => go($('#ftPath').value);
    $('#ftPath').onkeydown = e => { if (e.key === 'Enter') go(e.target.value); };
    $('#ftUp').onclick = () => go(ftParent(F.path));
    $('#ftRef').onclick = () => go(F.path);
    $('#ftFilter').oninput = e => { filter = e.target.value; const pos = e.target.selectionStart; drawBody(); const f = $('#ftFilter'); f.focus(); f.setSelectionRange(pos, pos); };
    $$('.ft-chip', body$).forEach(b => b.onclick = () => go(b.dataset.path));
    $$('[data-open]', body$).forEach(a => a.onclick = ev => { ev.preventDefault(); go(ftJoin(F.path, list.entries[+a.dataset.open].n)); });
    $$('[data-dl]', body$).forEach(b => b.onclick = () => download(list.entries[+b.dataset.dl]));
    $('#ftPick').onclick = () => $('#ftFiles').click();
    $('#ftFiles').onchange = e => { enqueue([...e.target.files]); e.target.value = ''; };
  }

  // ---- download
  function startDownload(params) {
    const a = document.createElement('a');
    a.href = apiURL('tools/files/download?') + qs(params);
    a.download = '';
    document.body.appendChild(a); a.click(); a.remove();
  }
  async function download(e) {
    const p = ftJoin(F.path, e.n);
    toast(e.dir ? 'Measuring the folder…' : 'Checking the file…');
    let st;
    try { st = await ctSend('GET', 'tools/files/stat?' + qs({ path: p })); } catch (err) { return toast(err.message, 'err'); }
    if (st.missing) return toast('That file no longer exists.', 'err');
    if (e.dir) {
      if (!st.ok) return toast(`Cannot download this folder: ${st.why}.`, 'err');
      const r = await modal({
        title: 'Download folder', body: `<p><b class="mono">${esc(e.n)}</b> is about <b>${fmtBytes(st.size)}</b> on disk. It is streamed as a tar archive straight from the pod, nothing is written in the pod.</p>
          ${st.gzip ? `<label class="chk"><input type="checkbox" id="ftGz" checked> Compress in the pod (smaller transfer, uses some CPU there)</label>` : '<p class="muted">gzip is not available in this container, so the archive is not compressed.</p>'}`,
        actions: [{ label: 'Cancel', value: null }, { label: 'Download', kind: 'primary', submit: true, run: d => ({ gz: !!$('#ftGz', d)?.checked }) }],
      });
      if (r) startDownload({ path: p, gz: r.gz ? '1' : '0' });
      return;
    }
    if (st.ok) { startDownload({ path: p }); return; }
    if (!st.readable && st.file === false) return toast(st.why, 'err');
    if (st.file && st.tail && st.size > st.limit) {
      const opts = FT_QUICK.filter(m => m * 1048576 <= st.limit && m * 1048576 < st.size);
      if (!opts.length) return toast(`${st.why}.`, 'err');
      const r = await modal({
        title: 'File is over the download limit',
        body: `<p><b class="mono">${esc(e.n)}</b> is <b>${fmtBytes(st.size)}</b>; the limit is ${fmtBytes(st.limit)}. For a growing log you usually want the end of it:</p>
          <label class="ct-inl">Download the last <select id="ftTail">${opts.map((m, i) => `<option value="${m}" ${i === Math.min(opts.length - 1, 3) ? 'selected' : ''}>${m >= 1024 ? (m / 1024).toFixed(m % 1024 ? 1 : 0) + ' GiB' : m + ' MiB'}</option>`).join('')}</select></label>`,
        actions: [{ label: 'Cancel', value: null }, { label: 'Download', kind: 'primary', submit: true, run: d => ({ tail: $('#ftTail', d).value }) }],
      });
      if (r) startDownload({ path: p, tail: r.tail });
      return;
    }
    toast(`Cannot download: ${st.why}.`, 'err');
  }

  // ---- upload queue
  const Q = { items: [], busy: false };
  function enqueue(files) {
    for (const f of files) {
      const it = { f, dir: F.path, state: 'wait', pct: 0, msg: '', hold: {} };
      if (f.size > lim.maxUpload) { it.state = 'err'; it.msg = `${fmtBytes(f.size)} is over the ${fmtBytes(lim.maxUpload)} upload limit`; }
      Q.items.push(it);
    }
    drawQueue(); pump();
  }
  function drawQueue() {
    const el = $('#ftQueue');
    if (!el) return;
    el.innerHTML = Q.items.length ? `<div class="ft-q">${Q.items.map((it, i) => `
      <div class="ft-qrow ${it.state}"><span class="mono ft-qn" title="${esc(it.dir)}">${esc(it.f.name)}</span><span class="muted">${fmtBytes(it.f.size)}</span>
        <span class="ct-bar ft-qbar"><i style="width:${Math.round(it.pct * 100)}%"></i></span>
        <span class="ft-qs">${it.state === 'wait' ? 'Waiting' : it.state === 'run' ? Math.round(it.pct * 100) + '%' : it.state === 'ok' ? '✓ ' + esc(it.msg) : '✕ ' + esc(it.msg)}</span>
        ${it.state === 'run' ? `<button class="btn sm" data-qc="${i}">Cancel</button>` : ''}</div>`).join('')}
      <div><button class="btn sm" id="ftQClear">Clear list</button></div></div>` : '';
    $$('[data-qc]', el).forEach(b => b.onclick = () => Q.items[+b.dataset.qc].hold.abort?.());
    $('#ftQClear')?.addEventListener('click', () => { Q.items = Q.items.filter(i => i.state === 'run' || i.state === 'wait'); drawQueue(); });
  }
  async function pump() {
    if (Q.busy) return;
    Q.busy = true;
    for (;;) {
      const it = Q.items.find(i => i.state === 'wait');
      if (!it) break;
      it.state = 'run'; drawQueue();
      await uploadOne(it);
      drawQueue();
    }
    Q.busy = false;
    if (Q.items.some(i => i.state === 'ok')) go(F.path);
  }
  async function uploadOne(it) {
    const opts = { overwrite: false, allowLayer: false };
    const via = viaIdx >= 0 ? alts[viaIdx] : null;
    let t = via ? { pod: via.pod, container: via.container, dir: via.path } : { pod: F.pod, container: F.container, dir: it.dir };
    const fail = m => { it.state = 'err'; it.msg = m; };
    for (let tries = 0; tries < 6; tries++) {
      const u = apiURL('tools/files/upload?') + qs({ pod: t.pod, container: t.container, dir: t.dir, name: it.f.name, size: it.f.size, overwrite: opts.overwrite ? '1' : '0', allowLayer: opts.allowLayer ? '1' : '0' });
      it.pct = 0;
      const r = await ftXhr(u, it.f, p => { it.pct = p; drawQueue(); }, it.hold);
      const d = r.data || {};
      if (r.status === 200 && d.ok) {
        it.state = 'ok'; it.pct = 1;
        it.msg = `${d.path}${t.pod !== F.pod ? ' (via ' + t.pod + ')' : ''} · ${d.verified === 'sha256' ? 'sha256 verified' : 'size verified'}`;
        return;
      }
      if (d.code === 'cancelled') return fail('cancelled');
      if (d.code === 'exists') {
        const ok = await modal({ title: 'Replace existing file?', body: `<p><b class="mono">${esc(it.f.name)}</b> already exists in <span class="mono">${esc(t.dir)}</span>. Replacing it cannot be undone.</p>`,
          actions: [{ label: 'Skip', value: false }, { label: 'Replace', kind: 'danger solid', value: true, submit: true }] });
        if (!ok) return fail('skipped: file exists');
        opts.overwrite = true; continue;
      }
      const options = (d.alternatives || []).filter(a => a.pod !== t.pod || a.container !== t.container);
      if (['layer', 'readonly', 'notwritable', 'nospace'].includes(d.code)) {
        const acts = [{ label: 'Skip', value: null }];
        options.slice(0, 3).forEach(a => acts.push({ label: `Upload via ${a.pod}`, value: { alt: a } }));
        if (d.code === 'layer') acts.push({ label: 'Upload here anyway', kind: 'danger solid', value: { layer: true } });
        const r2 = await modal({ title: d.code === 'layer' ? 'Not a persistent volume' : d.code === 'nospace' ? 'Not enough free space' : 'Cannot write here',
          body: `<p>${esc(d.error)}.</p>${options.length ? `<p class="muted">Other containers mounting the same volume: ${options.map(a => `<span class="mono">${esc(a.pod)} / ${esc(a.container)}</span> (${esc(a.path)})`).join(', ')}.</p>` : ''}`,
          actions: acts.map(a => Object.assign({}, a, { submit: false })) });
        if (!r2) return fail(d.error || 'skipped');
        if (r2.alt) { t = { pod: r2.alt.pod, container: r2.alt.container, dir: r2.alt.path }; continue; }
        if (r2.layer) { opts.allowLayer = true; continue; }
      }
      return fail(d.error || `upload failed (${r.status})`);
    }
    fail('too many retries');
  }

  // drag and drop anywhere on the page
  const onDrag = e => { if (e.dataTransfer && [...e.dataTransfer.types].includes('Files')) e.preventDefault(); };
  const onDrop = e => {
    if (!e.dataTransfer?.files?.length || !$('#ftFiles')) return;
    e.preventDefault();
    if (!F.pod || roOnly) return;
    enqueue([...e.dataTransfer.files]);
  };
  main.ondragover = onDrag;
  main.ondrop = onDrop;

  ns$.onchange = () => { F.ns = ns$.value; F.pod = ''; F.container = ''; F.path = '/'; persist(); loadPods(); };
  pod$.onchange = () => { F.pod = pod$.value; F.container = ''; F.path = '/'; persist(); loadPod(); };
  ctr$.onchange = () => { F.container = ctr$.value; F.path = '/'; persist(); const m = ctrMounts(); F.path = m.find(x => x.kind === 'pvc')?.path || m[0]?.path || '/'; go(F.path); };
  await loadPods();
}

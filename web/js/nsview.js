'use strict';
/* Namespace → Overview: a dashboard for one namespace, with a list of namespaces on the left to
   switch between them. Data: /api/nsdash (one namespace) and /api/namespaces (the list). */

S.nsRail = S.nsRail || { q: '', scroll: 0 };

const nsHref = (ctx, ns) => `${ctxPath(ctx)}/nsview/${enc(ns)}`;
// open another page filtered to this namespace (sets the namespace picker)
function gotoWithNs(ctx, ns, hash) {
  S.ns = ns;
  try { localStorage.setItem('kd.ns.' + ctx, ns); } catch {}
  renderTop(parseRoute());
  location.hash = hash;
}

async function viewNsOverview(route) {
  const ctx = route.ctx;
  const lastKey = 'kd.nsview.' + ctx;
  let nss = [];
  try { nss = await api('namespaces?ctx=' + enc(ctx)); } catch {}
  nss.sort((a, b) => a.name.localeCompare(b.name));
  let ns = route.args[0];
  if (!ns) {
    let last = '';
    try { last = localStorage.getItem(lastKey) || ''; } catch {}
    ns = [S.ns, last].find(n => n && nss.some(x => x.name === n)) || (nss.find(n => n.pods) || nss[0] || {}).name;
    if (!ns) { main.innerHTML = '<div class="empty-state"><h2>No namespaces</h2><p>Nothing is readable in this cluster with your credentials.</p></div>'; return; }
    history.replaceState(null, '', nsHref(ctx, ns));
    route = parseRoute();
  }
  try { localStorage.setItem(lastKey, ns); } catch {}

  main.innerHTML = `<div class="nsv">
      <aside class="card nsv-rail"><div class="nsv-rhead"><b>Namespaces</b><span class="muted">${nss.length}</span></div>
        <input type="search" id="nsvFind" placeholder="Find a namespace" spellcheck="false" value="${esc(S.nsRail.q)}">
        <div class="nsv-list" id="nsvList"></div></aside>
      <section class="nsv-main" id="nsvMain"><div class="muted">Loading…</div></section></div>`;

  // ---- the namespace list
  const drawList = () => {
    const q = S.nsRail.q.trim().toLowerCase();
    const list = nss.filter(n => !q || n.name.toLowerCase().includes(q));
    $('#nsvList').innerHTML = list.map(n => {
      const st = n.problem ? 'critical' : n.pods ? 'ok' : 'idle';
      return `<a class="nsv-item ${n.name === ns ? 'on' : ''}" href="${nsHref(ctx, n.name)}" title="${esc(n.name)} · ${n.pods} pods${n.problem ? ' · ' + n.problem + ' need attention' : ''}">
        <i class="nsv-dot ${st}"></i><span class="n">${esc(n.name)}</span><span class="c">${n.problem ? `<b>${n.problem}</b>/` : ''}${n.pods}</span></a>`;
    }).join('') || '<div class="muted" style="padding:10px">No match</div>';
  };
  drawList();
  const listEl = $('#nsvList');
  const cur = $('.nsv-item.on', listEl);
  if (S.nsRail.scroll) listEl.scrollTop = S.nsRail.scroll;
  if (cur && (cur.offsetTop < listEl.scrollTop || cur.offsetTop > listEl.scrollTop + listEl.clientHeight - 30)) cur.scrollIntoView({ block: 'center' });
  listEl.addEventListener('scroll', () => { S.nsRail.scroll = listEl.scrollTop; });
  $('#nsvFind').addEventListener('input', e => { S.nsRail.q = e.target.value; drawList(); });
  $('#nsvFind').addEventListener('keydown', e => {
    if (e.key === 'Enter') { const first = $('.nsv-item', listEl); if (first) location.hash = first.getAttribute('href'); }
  });

  // ---- the dashboard
  const box = $('#nsvMain');
  let checkedPvc = false;
  const render = async () => {
    const [d, sum] = await Promise.all([api(`nsdash?ctx=${enc(ctx)}&ns=${enc(ns)}`), api('clusters').then(l => l.find(c => c.name === ctx))]);
    if (parseRoute().section !== 'nsview' || (parseRoute().args[0] || ns) !== ns) return;
    const s = d.summary, cpuCap = s.cpu.quota || 0, memCap = s.mem.quota || 0;
    const running = d.phases.Running || 0, pending = d.phases.Pending || 0;
    const totalPods = Object.values(d.phases).reduce((a, b) => a + b, 0);
    const wlBad = d.workloads.filter(w => w.health !== 'healthy');
    const tile = (label, val, foot, extra = '') => `<div class="card tile"><div class="label">${label}</div><div class="value">${val}</div><div class="foot">${foot}</div>${extra}</div>`;
    const res = (r, fmt, cap) => cap
      ? `<div class="value">${d.metrics ? pct(r.used, cap).toFixed(0) + '%' : '—'}</div><div class="foot">${d.metrics ? fmt(r.used) : '—'} of ${fmt(cap)} quota · req ${pct(r.req, cap).toFixed(0)}%</div><div class="meter-wrap">${meter(r.used, cap, r.req)}</div>`
      : `<div class="value">${d.metrics ? fmt(r.used) : '—'}</div><div class="foot">requests ${r.req ? fmt(r.req) : 'none'} · limits ${r.lim ? fmt(r.lim) : 'none'}</div>${r.req ? `<div class="meter-wrap">${meter(r.used, r.req * 1.5, r.req)}</div>` : ''}`;
    const refsCpu = [{ name: 'Requests', value: s.cpu.req }, { name: 'Quota', value: cpuCap, soft: true }];
    const refsMem = [{ name: 'Requests', value: s.mem.req }, { name: 'Quota', value: memCap, soft: true }];

    const wlT = table('nsWl', [
      { key: 'name', label: 'Workload', cls: 'name', render: w => `${esc(w.name)}<span class="sub">${esc(w.kind)}</span>` },
      { key: 'health', label: 'Health', render: w => healthLabel(w.health) },
      { key: 'ready', label: 'Ready', num: true, sort: w => w.ready / (w.desired || 1), render: w => `${w.ready}/${w.desired}` },
      { key: 'cpu', label: 'CPU used / req', num: true, sort: w => w.cpu.used, render: w => d.metrics ? `${fmtCPU(w.cpu.used)} <span class="muted">/ ${w.cpu.req ? fmtCPU(w.cpu.req) : 'none'}</span>` : `<span class="muted">req ${w.cpu.req ? fmtCPU(w.cpu.req) : 'none'}</span>` },
      { key: 'mem', label: 'Memory used / req', num: true, sort: w => w.mem.used, render: w => d.metrics ? `${fmtMem(w.mem.used)} <span class="muted">/ ${w.mem.req ? fmtMem(w.mem.req) : 'none'}</span>` : `<span class="muted">req ${w.mem.req ? fmtMem(w.mem.req) : 'none'}</span>` },
      { key: 'restarts', label: 'Restarts', num: true },
      { key: 'spark', label: 'CPU 10m', nosort: true, render: w => spark(w.spark) },
    ], d.workloads, { href: w => `${ctxPath(ctx)}/wl/${enc(w.kind)}/${enc(w.namespace)}/${enc(w.name)}` });
    const podCols = kind => [
      { key: 'name', label: 'Pod', cls: 'name', render: p => `${esc(p.name)}<span class="sub">${esc(p.node || '')}</span>` },
      { key: 'used', label: kind === 'cpu' ? 'CPU' : 'Memory', num: true, render: p => kind === 'cpu' ? fmtCPU(p.cpu.used) : fmtMem(p.mem.used) },
      { key: 'req', label: 'of request', num: true, render: p => { const u = kind === 'cpu' ? p.cpu : p.mem; return u.req ? pct(u.used, u.req).toFixed(0) + '%' : '<span class="muted">none</span>'; } },
    ];
    const podHref = p => `${ctxPath(ctx)}/pod/${enc(p.namespace)}/${enc(p.name)}`;
    const topC = table('nsTopC', podCols('cpu'), d.topCPU, { href: podHref });
    const topM = table('nsTopM', podCols('mem'), d.topMem, { href: podHref });
    const probT = table('nsProb', [
      { key: 'name', label: 'Pod', cls: 'name', render: p => `${esc(p.name)}<span class="sub">${esc(p.owner || '')}</span>` },
      { key: 'status', label: 'Status', render: p => stLabel(p.status) },
      { key: 'restarts', label: 'Restarts', num: true },
      { key: 'created', label: 'Age', num: true, render: p => age(p.created) },
    ], d.problems, { href: podHref });

    const items = d.health.items || [];
    const itemHref = it => it.kind === 'pod' ? `${ctxPath(ctx)}/pod/${enc(ns)}/${enc(it.name)}`
      : it.kind === 'workload' ? `${ctxPath(ctx)}/wl/${enc(it.ref)}/${enc(ns)}/${enc(it.name)}`
      : it.kind === 'pvc' ? objHref(ctx, '', 'persistentvolumeclaims', ns, it.name) : `${ctxPath(ctx)}/events`;
    const labels = Object.entries(d.labels || {}).filter(([k]) => k !== 'kubernetes.io/metadata.name');
    const counts = d.counts.map(c => {
      const v = c.count === null || c.count === undefined ? `<span class="muted" title="${esc(c.err || '')}">—</span>` : c.count;
      return `<a class="nsv-count" href="#" data-list="${esc(c.group)}|${esc(c.resource)}" title="${esc(c.err ? c.label + ': ' + c.err : 'Open ' + c.label + ' in ' + ns)}"><b>${v}</b><span>${esc(c.label)}</span></a>`;
    }).join('');
    const quotas = d.quotas.map(q => `<div class="nsv-quota"><div class="cap" style="margin-bottom:4px">${esc(q.name)}</div>${q.items.map(i => `
        <div class="nsv-qrow"><span class="mono">${esc(i.resource)}</span><span class="nums">${esc(i.used || '—')} / ${esc(i.hard)}</span>
        ${i.pct >= 0 ? `<div class="meter"><i class="${i.pct >= 90 ? 'critical' : i.pct >= 80 ? 'warning' : ''}" style="width:${Math.min(100, i.pct)}%"></i></div><span class="nums ${i.pct >= 90 ? 'pct-critical' : i.pct >= 80 ? 'pct-warning' : ''}">${i.pct.toFixed(0)}%</span>` : '<span></span><span></span>'}</div>`).join('')}</div>`).join('');
    const pvcRows = d.pvcs.map(p => `<tr class="link" data-pvc="${esc(p.name)}"><td class="name">${esc(p.name)}<span class="sub">${esc(p.storageClass || 'default class')}</span></td>
        <td>${stLabel(p.phase, p.phase === 'Bound' ? 'good' : 'warning')}</td><td class="num">${p.requested ? fmtBytes(p.requested) : '—'}</td>
        <td style="min-width:180px">${p.phase !== 'Bound' ? '<span class="muted">not bound</span>' : pvcCell(p.usage || S.pvc[pvcKey(ctx, ns, p.name)] || (d.exec ? null : { state: 'disabled' }))}</td></tr>`).join('');

    box.innerHTML = `
      <div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/namespaces">Namespaces</a></div>
        <h1><span class="kind">Namespace</span>${esc(ns)}</h1>
        <div class="sub">${stLabel(d.phase, d.phase === 'Active' ? 'good' : 'warning')}${d.created ? `<span>age ${age(d.created)}</span>` : ''}<span>${totalPods} pod${totalPods === 1 ? '' : 's'} · ${d.containers} containers</span>
          <span class="muted" id="updated">updated ${new Date(d.at).toLocaleTimeString()}</span></div>
        ${labels.length ? `<div class="chips nsv-labels">${labels.slice(0, 8).map(([k, v]) => `<span class="chip">${esc(k)}${v ? '=' + esc(v) : ''}</span>`).join('')}${labels.length > 8 ? `<span class="muted">+${labels.length - 8}</span>` : ''}</div>` : ''}</div>
        <div class="right">
          <button class="btn" data-go="pods">Pods</button><button class="btn" data-go="workloads">Workloads</button><button class="btn" data-go="events">Events</button>
          <a class="btn ghost" href="${objHref(ctx, '', 'namespaces', '', ns, 'yaml')}" title="YAML">${ICON.edit}</a></div></div>
      ${sum ? certBanner(sum) : ''}
      ${items.length ? `<div class="card nscard ${d.health.sev}" style="margin-bottom:14px"><div class="card-head"><h3>Needs attention</h3>${stLabel(d.health.sev === 'critical' ? 'Critical' : 'Warning', d.health.sev)}<span class="cap">${items.length} item${items.length === 1 ? '' : 's'}</span></div>
          <ul class="attn">${items.slice(0, 12).map(it => `<li class="${it.sev}"><span class="ai"><i class="sdot"></i></span><a href="${itemHref(it)}">${esc(it.name)}</a><span class="muted">${esc(it.detail)}</span></li>`).join('')}</ul>
          ${items.length > 12 ? `<div class="muted" style="margin-top:6px">+${items.length - 12} more on <a href="${ctxPath(ctx)}/namespaces">Namespace health</a></div>` : ''}</div>`
        : `<div class="card empty-ok" style="margin-bottom:14px"><b>Healthy.</b><span class="muted"> No failing pods, unhealthy workloads, full volumes or recent warnings in this namespace.</span></div>`}
      <div class="tiles">
        ${tile('Pods running', `${running}<small>/ ${totalPods}</small>`, s.problem ? stLabel(s.problem + (s.problem > 1 ? ' need' : ' needs') + ' attention', 'critical') : pending ? stLabel(pending + ' pending', 'warning') : stLabel('no problems', 'good'))}
        ${tile('Workloads healthy', `${d.workloads.length - wlBad.length}<small>/ ${d.workloads.length}</small>`, wlBad.length ? stLabel(wlBad.map(w => w.name).slice(0, 2).join(', ') + (wlBad.length > 2 ? '…' : ''), 'warning') : 'Deployments, StatefulSets, DaemonSets')}
        <div class="card tile"><div class="label">CPU used</div>${res(s.cpu, fmtCPU, cpuCap)}</div>
        <div class="card tile"><div class="label">Memory used</div>${res(s.mem, fmtMem, memCap)}</div>
        ${tile('Restarts', d.restarts, 'all containers, since they started')}
        ${tile('Warning events · 1h', d.warnings1h, `<a href="#" data-go="events">view events →</a>`)}
      </div>
      <div class="grid g2">
        ${chartCard('nsdCpu', 'CPU', 'cores · last hour', refsCpu)}
        ${chartCard('nsdMem', 'Memory', 'bytes · last hour', refsMem)}
      </div>
      <div class="card mt"><div class="card-head"><h3>Workloads</h3><span class="cap">${d.workloads.length}</span></div><div id="nsWlBox">${wlT.html}</div></div>
      <div class="grid g2 mt">
        <div class="card"><div class="card-head"><h3>Top pods by CPU</h3></div><div id="nsTopCBox">${topC.html}</div></div>
        <div class="card"><div class="card-head"><h3>Top pods by memory</h3></div><div id="nsTopMBox">${topM.html}</div></div>
      </div>
      ${d.problems.length ? `<div class="card mt"><div class="card-head"><h3>Pods needing attention</h3><span class="cap">${d.problems.length}</span></div><div id="nsProbBox">${probT.html}</div></div>` : ''}
      <div class="grid g2 mt">
        <div class="card"><div class="card-head"><h3>Objects</h3><span class="cap">click to open the list</span></div><div class="nsv-counts">${counts}</div></div>
        <div class="card"><div class="card-head"><h3>Nodes</h3><span class="cap">where this namespace's pods run</span></div>
          ${d.nodes.length ? `<div class="nsv-nodes">${d.nodes.map(n => `<a class="chip" href="${ctxPath(ctx)}/node/${enc(n.node)}" title="${n.pods} pod${n.pods === 1 ? '' : 's'}">${esc(n.node)} <b>${n.pods}</b></a>`).join('')}</div>` : '<div class="muted">No running pods.</div>'}</div>
      </div>
      ${d.pvcs.length || d.pvcErr ? `<div class="card mt"><div class="card-head"><h3>Volume claims</h3><span class="cap">${d.pvcs.length} · usage read with <code>df</code> in a pod that mounts the claim</span>
          ${d.exec && d.pvcs.length ? '<button class="btn sm right" id="nsPvcCheck">Check usage</button>' : ''}</div>
        ${d.pvcErr ? `<div class="muted">${esc(d.pvcErr)}</div>` : `<div class="tbl-wrap"><table class="t"><thead><tr><th>Claim</th><th>Status</th><th class="num">Size</th><th>Used</th></tr></thead><tbody>${pvcRows}</tbody></table></div>`}</div>` : ''}
      ${d.quotas.length || d.quotaErr ? `<div class="card mt"><div class="card-head"><h3>Resource quotas</h3></div>${d.quotaErr ? `<div class="muted">${esc(d.quotaErr)}</div>` : quotas}</div>` : ''}
      <div class="card mt"><div class="card-head"><h3>Recent events</h3><span class="cap">warnings first</span></div>${d.events.length ? eventsList(d.events.slice(0, 20)) : '<div class="muted">No events.</div>'}</div>`;

    drawUsageCharts('nsd', d.history, refsCpu, refsMem);
    wlT.bind($('#nsWlBox'), render);
    topC.bind($('#nsTopCBox'), render);
    topM.bind($('#nsTopMBox'), render);
    if ($('#nsProbBox')) probT.bind($('#nsProbBox'), render);
    $$('[data-go]', box).forEach(b => b.addEventListener('click', e => { e.preventDefault(); gotoWithNs(ctx, ns, `${ctxPath(ctx)}/${b.dataset.go}`); }));
    $$('[data-list]', box).forEach(a => a.addEventListener('click', e => {
      e.preventDefault();
      const [g, r] = a.dataset.list.split('|');
      gotoWithNs(ctx, ns, listHref(ctx, g, r));
    }));
    $$('tr[data-pvc]', box).forEach(tr => tr.addEventListener('click', () => { location.hash = objHref(ctx, '', 'persistentvolumeclaims', ns, tr.dataset.pvc); }));
    $('#nsPvcCheck')?.addEventListener('click', async e => {
      e.target.disabled = true;
      try { await loadPvcUsage(ctx, ns, { refresh: true }); } catch (err) { toast(err.message, 'err'); }
      render();
    });
    // the first time, read volume usage the server doesn't have cached yet
    if (!checkedPvc && d.exec && d.pvcs.some(p => p.phase === 'Bound' && !p.usage && !S.pvc[pvcKey(ctx, ns, p.name)])) {
      checkedPvc = true;
      loadPvcUsage(ctx, ns).then(() => { if (parseRoute().section === 'nsview') render(); }).catch(() => {});
    }
  };
  await live(15000, render);
}

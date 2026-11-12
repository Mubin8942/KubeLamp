'use strict';
/* Dashboards: clusters, overview, nodes, workloads, pods, events, deployment logs. */
// ------------------------------------------------------------------ access scope
const isNs = c => c && c.scope === 'namespaced';
const capOf = r => r.alloc || r.quota || 0;                    // capacity reference
const capName = c => isNs(c) ? 'Quota' : 'Allocatable';
function scopeText(c) {
  if (!c.scope) return '';
  if (!isNs(c)) return 'Cluster-wide access';
  return `Namespaces: ${nsShort(c.scopeNamespaces)}`;
}
// "a, b, c +12 more" for long namespace lists in one-line places
function nsShort(list, max = 3) {
  list = list || [];
  return list.length <= max ? list.join(', ') : `${list.slice(0, max).join(', ')} +${list.length - max} more`;
}
const FOUND_BY = { rancher: 'found automatically from your Rancher projects', namespaces: 'found automatically (namespaces where you may list pods)' };
async function openAccess(ctx) {
  const c = (await api('clusters')).find(x => x.name === ctx) || {};
  const dlg = document.createElement('dialog');
  const detected = c.accessMode === 'auto' && c.scope ? ` (detected: ${c.scope === 'namespaced' ? 'namespace-scoped' : 'cluster-wide'})` : '';
  dlg.innerHTML = `<form class="dlg" method="dialog">
    <h3>Access for ${esc(shortName(ctx))}</h3>
    <p class="muted" style="margin:0">How KubeLamp reads this cluster with your kubeconfig credentials.</p>
    <label class="radio"><input type="radio" name="mode" value="auto"><span><b>Automatic</b> — recommended${esc(detected)}
      <small>Use cluster-wide access if your user has a ClusterRole; if not, read the namespaces you have access to.</small></span></label>
    <label class="radio"><input type="radio" name="mode" value="cluster"><span><b>Cluster-wide</b>
      <small>Needs ClusterRole + ClusterRoleBinding. Shows nodes, every namespace and node capacity.</small></span></label>
    <label class="radio"><input type="radio" name="mode" value="namespaced"><span><b>Only my namespaces</b>
      <small>For users with Role + RoleBinding only. KubeLamp doesn't read cluster-wide lists.</small></span></label>
    ${c.scope === 'namespaced' ? `<div class="acc-found"><b>${c.scopeFoundBy ? `Found automatically (${c.scopeFoundBy === 'rancher' ? 'Rancher projects' : 'namespaces where you may list pods'})` : 'Not found automatically'}</b>
      <span class="muted">${c.scopeFoundBy ? 'KubeLamp looks again every 10 minutes and when you save, so new RoleBindings appear on their own.' : 'Your user can\'t list namespaces and this isn\'t a Rancher cluster: type your namespaces below.'}</span>
      ${(c.scopeNamespaces || []).length ? `<div class="chips acc-chips">${c.scopeNamespaces.map(n => `<span class="chip">${esc(n)}</span>`).join('')}</div>` : ''}</div>` : ''}
    <label class="field">Extra namespaces (optional)
      <small>Only for namespaces that aren't found automatically. Comma or newline separated.${c.namespace ? ` The context's namespace <code>${esc(c.namespace)}</code> is used when nothing is found.` : ''}</small>
      <textarea id="accNs" rows="3" spellcheck="false" placeholder="payments, ecrm">${esc((c.accessNamespaces || []).join(', '))}</textarea></label>
    <div class="err-line" id="accErr"></div>
    <div class="actions"><button type="button" class="btn" id="accCancel">Cancel</button><button type="submit" class="btn primary">Save</button></div>
  </form>`;
  document.body.appendChild(dlg);
  $(`input[value="${c.accessMode || 'auto'}"]`, dlg).checked = true;
  const close = () => { dlg.close(); dlg.remove(); };
  $('#accCancel', dlg).onclick = close;
  $('form', dlg).addEventListener('submit', async e => {
    e.preventDefault();
    const body = { context: ctx, mode: $('input[name=mode]:checked', dlg).value, namespaces: $('#accNs', dlg).value.split(/[\s,;]+/).filter(Boolean) };
    const r = await fetch(apiURL('access'), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) { $('#accErr', dlg).textContent = d.error || 'Could not save'; return; }
    $('button[type=submit]', dlg).textContent = 'Applying…';
    setTimeout(() => { close(); navigate(); }, 2500);
  });
  dlg.showModal();
}

// ------------------------------------------------------------------ HOME
// per-namespace problems on a cluster card: critical and warning items, worst namespaces first
function attnBlock(c) {
  const a = c.attention;
  if (!a || !a.namespaces) return `<div class="attn-ok">${stLabel('All namespaces are healthy', 'good')}</div>`;
  const head = a.critical ? stLabel(`${a.critical} critical`, 'critical') + (a.warning ? ' ' + stLabel(`${a.warning} warning${a.warning === 1 ? '' : 's'}`, 'warning') : '')
    : stLabel(`${a.warning} warning${a.warning === 1 ? '' : 's'}`, 'warning');
  return `<div class="attn-box"><div class="attn-head">${head}<span class="muted">in ${a.namespaces} namespace${a.namespaces === 1 ? '' : 's'}</span></div>
    <ul class="attn-list">${a.top.map(n => `<li class="${n.sev}"><i class="sdot"></i><a href="${ctxPath(c.name)}/namespaces" title="${esc(n.top.join('\n'))}"><b>${esc(n.name)}</b></a>
      <span class="muted">${n.critical ? n.critical + ' critical' : ''}${n.critical && n.warning ? ' · ' : ''}${n.warning ? n.warning + ' warning' + (n.warning === 1 ? '' : 's') : ''} — ${esc((n.top[0] || '').replace(/^\w+ /, ''))}</span></li>`).join('')}
    ${a.namespaces > a.top.length ? `<li class="muted more">+ ${a.namespaces - a.top.length} more — open Namespaces</li>` : ''}</ul></div>`;
}

// ------------------------------------------------------------------ TLS certificate expiry
const CERT_CLS = { ok: 'good', warn: 'warn', crit: 'crit', expired: 'crit' };
const certWhen = c => c.daysLeft < 0 ? `expired ${-c.daysLeft === 1 ? '1 day' : -c.daysLeft + ' days'} ago`
  : c.daysLeft === 0 ? (c.state === 'expired' ? 'expired today' : 'expires today')
  : c.daysLeft >= 730 ? `expires in ${Math.floor(c.daysLeft / 365)} years` : `expires in ${c.daysLeft === 1 ? '1 day' : c.daysLeft + ' days'}`;
const certDate = c => new Date(c.notAfter).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });
const certRole = { server: 'API server certificate', client: 'Client certificate', ca: 'Cluster CA' };
// one line for the cluster cards on the home page: the certificate that expires first
function certLine(c) {
  const r = c.certs;
  if (!r) return '';
  const s = r.soonest;
  if (!s) return r.insecure ? `<div class="cert-line muted" title="insecure-skip-tls-verify is set in the kubeconfig">TLS: certificate checks are off for this cluster</div>` : '';
  const all = r.certs.map(x => `${certRole[x.role]}: ${certWhen(x)} (${certDate(x)})`).join('\n');
  return `<div class="cert-line ${CERT_CLS[s.state]}" title="${esc(all)}"><svg viewBox="0 0 24 24"><rect x="5" y="10" width="14" height="10" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/></svg>
    <span>${esc(certRole[s.role])} ${esc(certWhen(s))}</span><span class="muted">${esc(certDate(s))}</span></div>`;
}
// the warning banner on the cluster pages
function certBanner(c) {
  const r = c.certs;
  if (!r || !['warn', 'crit', 'expired'].includes(r.worst)) return '';
  const bad = r.certs.filter(x => x.state !== 'ok').sort((a, b) => a.daysLeft - b.daysLeft);
  const exp = bad.some(x => x.state === 'expired');
  return `<div class="banner ${exp || r.worst === 'crit' ? 'err' : ''}"><b>${exp ? 'Certificate expired.' : 'Certificate expiring soon.'}</b><span>${bad.map(x => `${esc(certRole[x.role])} ${esc(certWhen(x))} (${esc(certDate(x))})`).join('; ')}.
    ${bad.some(x => x.role !== 'ca') ? 'On kubeadm clusters, renew with <code>kubeadm certs renew all</code> on each control-plane node, then restart the control-plane pods' + (bad.some(x => x.role === 'client') ? ' and use the new <code>admin.conf</code>' : '') + '.' : ''}</span></div>`;
}
function certCard(c) {
  const r = c.certs;
  if (!r) return '';
  const rows = r.certs.map(x => `<tr><td class="name">${esc(certRole[x.role])}${x.note ? `<span class="sub">${esc(x.note)}</span>` : ''}</td>
      <td><span class="mono">${esc(x.subject)}</span><span class="sub">issued by ${esc(x.issuer)}</span></td>
      <td>${esc(certDate(x))}<span class="sub">valid from ${esc(new Date(x.notBefore).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' }))}</span></td>
      <td><span class="ct-chip ${CERT_CLS[x.state]}">${esc(certWhen(x))}</span></td>
      <td class="muted cert-sans" title="${esc((x.sans || []).join(', '))}">${esc((x.sans || []).join(', '))}</td></tr>`).join('');
  return `<div class="card mt" id="certCard"><div class="card-head"><h3>TLS certificates</h3><span class="cap">from the connection and your kubeconfig · warns at 30 days</span></div>
    ${rows ? `<div class="tbl-wrap"><table class="t"><thead><tr><th>Certificate</th><th>Subject</th><th>Expires</th><th>Status</th><th>Names (SAN)</th></tr></thead><tbody>${rows}</tbody></table></div>` : ''}
    ${r.err && !r.certs.some(x => x.role === 'server') ? `<div class="muted" style="margin-top:6px">API server certificate: ${esc(r.err)}</div>` : ''}
    ${r.insecure ? `<div class="muted" style="margin-top:6px">TLS verification is off for this cluster (<code>insecure-skip-tls-verify</code> in the kubeconfig).</div>` : ''}
    <div class="cap" style="margin-top:8px">The API server certificate is the one this endpoint presents (a load balancer's certificate if it terminates TLS). Other control-plane certificates (etcd, front-proxy, kubelet, controller-manager, scheduler) are not visible through the API: on kubeadm clusters check them with <code>kubeadm certs check-expiration</code> on a control-plane node.</div></div>`;
}

async function viewHome() {
  const kc = await api('kubeconfig');
  S.kc = kc;
  if (kc.needsSetup) return viewConnect(kc);
  const render = async () => {
    await loadClusters(); renderNav({});
    const cards = S.clusters.map(c => {
      const sub = `<div class="srv" title="${esc(c.server)}">${esc(c.server)}</div>`;
      let body;
      if (c.status === 'loading') body = `<div class="muted">Connecting…</div>`;
      else if (c.status === 'error') body = `<div class="err"><b>${/forbidden|RBAC|nothing readable/i.test(c.error || '') ? 'No access.' : 'Unreachable.'}</b> ${esc(c.error)}</div>
        ${/forbidden|RBAC|nothing readable/i.test(c.error || '') ? `<button class="btn" data-access="${esc(c.name)}">Set namespaces…</button>` : ''}${certLine(c)}`;
      else body = `<div class="kv">
          ${isNs(c) ? `<div><span>Namespaces</span><strong>${(c.scopeNamespaces || []).length}</strong></div>` : `<div><span>Nodes</span><strong>${c.nodesReady}/${c.nodesTotal}</strong></div>`}
          <div><span>Pods</span><strong>${fmtNum(c.pods.running)}</strong></div>
          <div><span>Deployments</span><strong>${c.deployHealthy}/${c.deployTotal}</strong></div>
          <div><span>Warnings 1h</span><strong>${c.warnings1h}</strong></div></div>
        <div class="meters">
          <div><div class="card-head" style="margin-bottom:4px"><span class="cap">CPU</span><span class="right muted">${c.metricsAvailable ? (isNs(c) ? (c.cpu.quota ? 'vs quota' : 'vs requests') : '') : 'no metrics'}</span></div>${mcell(c.cpu.used, capOf(c.cpu) || c.cpu.req, capOf(c.cpu) ? c.cpu.req : null, fmtCPU, c.metricsAvailable)}</div>
          <div><div class="card-head" style="margin-bottom:4px"><span class="cap">Memory</span><span class="right muted">${c.metricsAvailable ? (isNs(c) ? (c.mem.quota ? 'vs quota' : 'vs requests') : '') : 'no metrics'}</span></div>${mcell(c.mem.used, capOf(c.mem) || c.mem.req, capOf(c.mem) ? c.mem.req : null, fmtMem, c.metricsAvailable)}</div>
        </div>
        ${attnBlock(c)}${certLine(c)}`;
      return `<div class="card ccard" data-ctx="${esc(c.name)}">
        <div class="top"><i class="dot ${c.status}" style="margin-top:5px"></i><div style="min-width:0;flex:1"><div class="nm">${esc(shortName(c.name))}</div>${sub}</div>
        <div style="display:flex;gap:4px;flex-direction:column;align-items:flex-end">${c.version ? `<span class="tag">${esc(c.version)}</span>` : ''}${c.current ? '<span class="tag cur">current</span>' : ''}${isNs(c) ? '<span class="tag" title="Role/RoleBinding access">namespace-scoped</span>' : ''}</div></div>
        ${body}</div>`;
    }).join('');
    $('#cards').innerHTML = cards || `<div class="empty-state"><h2>No clusters yet</h2><p>The loaded kubeconfig has no contexts.</p><button class="btn primary" type="button" onclick="openKubeconfigs()">Add a kubeconfig</button></div>`;
    $$('.ccard', main).forEach(el => el.addEventListener('click', () => { location.hash = ctxPath(el.dataset.ctx) + '/overview'; }));
    $$('[data-access]', main).forEach(b => b.addEventListener('click', e => { e.stopPropagation(); openAccess(b.dataset.access); }));
    const ok = S.clusters.filter(c => c.status === 'ok' || c.status === 'partial').length;
    $('#kcStat').textContent = `${S.clusters.length} context${S.clusters.length === 1 ? '' : 's'} · ${ok} reachable`;
  };
  main.innerHTML = `
    <div class="page-head"><div><h1>All clusters</h1><div class="sub"><span id="kcStat"></span></div></div>
      <div class="right"><button class="btn" id="kcManage">${KC_ICON.file}Kubeconfigs</button><button class="btn" id="refreshAll"><svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7"/></svg>Refresh</button></div></div>
    ${kc.error ? `<div class="banner err"><b>Kubeconfig problem.</b><span>${esc(kc.error)}</span></div>` : ''}
    <div class="grid g3" id="cards"></div>`;
  $('#kcManage').onclick = () => openKubeconfigs();
  $('#refreshAll').onclick = async () => { await fetch(apiURL('refresh'), { method: 'POST' }); setTimeout(render, 1500); };
  await live(5000, render);
}

// ------------------------------------------------------------------ cluster header
// a long namespace list: the first 12, then "+N more" that expands in place
function nsListHtml(list, max = 12) {
  const b = l => `<b class="nslist">${esc(l.join(', '))}</b>`;
  if (list.length <= max) return b(list);
  return `${b(list.slice(0, max))}<span class="ns-rest" hidden><b>, ${esc(list.slice(max).join(', '))}</b></span> <button class="ns-more" type="button" onclick="this.previousElementSibling.hidden=false;this.remove()">+${list.length - max} more</button>`;
}
function clusterHead(sum, title, crumbs = '') {
  const c = sum;
  return `<div class="page-head"><div>${crumbs}<h1>${esc(title)}</h1>
    <div class="sub"><span>${stLabel(c.status === 'ok' ? 'Connected' : c.status === 'partial' ? 'Partially readable' : c.status, c.status === 'ok' ? 'good' : 'warning')}</span>
    ${c.version ? `<span>${esc(c.version)}</span>` : ''}<span class="mono">${esc(c.server)}</span><span>${esc(c.authType)}</span>
    <span class="muted" id="updated">updated ${c.at ? age(c.at) + ' ago' : '—'}</span></div></div>
    <div class="right"><button class="btn" id="accessBtn" title="Cluster-wide or namespace-scoped (Role/RoleBinding) access"><svg viewBox="0 0 24 24"><rect x="4" y="10" width="16" height="10" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/></svg>${esc(isNs(c) ? 'Namespaces: ' + (c.scopeNamespaces || []).length : 'Access')}</button><button class="btn" id="refreshBtn"><svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7"/></svg>Refresh</button></div></div>
    ${isNs(c) ? `<div class="banner info ns-banner"><b>Namespace-scoped</b><span><b>${(c.scopeNamespaces || []).length} namespace${(c.scopeNamespaces || []).length === 1 ? '' : 's'}</b>${c.scopeFoundBy ? ` ${FOUND_BY[c.scopeFoundBy] || 'found automatically'}` : ''}: ${nsListHtml(c.scopeNamespaces || [], 4)}
      <span class="muted ns-note">${c.nodesTotal ? 'Nodes are shown because your user may list them; node requests count only your pods' : 'Nodes and cluster capacity need cluster-wide rights'}${c.cpu.quota || c.mem.quota ? '; ResourceQuota is used as the capacity line' : ''}. <a href="#" data-acc>Manage</a></span></span></div>` : ''}
    ${certBanner(c)}
    ${c.status === 'partial' ? `<div class="banner"><b>Some data is missing.</b><span>${esc(c.error)}</span></div>` : ''}
    ${!c.metricsAvailable ? (/forbidden|RBAC/i.test(c.metricsErr || '')
      ? `<div class="banner"><b>No live metrics.</b><span>Your Role doesn't allow reading metrics. Ask for <code>get, list</code> on <code>pods</code> in API group <code>metrics.k8s.io</code> in your namespaces.</span></div>`
      : `<div class="banner"><b>No live metrics.</b><span>metrics-server isn't answering (${esc(c.metricsErr || 'not installed')}). Install it to see CPU/memory usage: <code>kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml</code></span></div>`) : ''}`;
}
function bindRefresh(ctx, rerender) {
  const a = $('#accessBtn');
  if (a) a.onclick = () => openAccess(ctx);
  $$('[data-acc]', main).forEach(l => l.addEventListener('click', e => { e.preventDefault(); openAccess(ctx); }));
  const b = $('#refreshBtn');
  if (b) b.onclick = async () => { b.disabled = true; await fetch(apiURL('refresh?ctx=') + enc(ctx), { method: 'POST' }); setTimeout(async () => { await rerender(); b.disabled = false; }, 1800); };
}

// ------------------------------------------------------------------ OVERVIEW
async function viewOverview(route) {
  const ctx = route.ctx;
  main.innerHTML = '<div class="muted">Loading…</div>';
  const render = async () => {
    const d = await api('overview?ctx=' + enc(ctx));
    const s = d.summary;
    await loadClusters(); renderNav(route);
    const ns = isNs(s);
    const cpuCap = capOf(s.cpu), memCap = capOf(s.mem);
    const cpuP = pct(s.cpu.used, cpuCap), memP = pct(s.mem.used, memCap);
    const capLbl = ns ? 'quota' : 'allocatable';
    const nodesT = table('ovNodes', [
      { key: 'name', label: 'Node', cls: 'name', render: n => `${esc(n.name)}<span class="sub">${esc(n.roles.join(', '))} · ${esc(n.internalIP)}</span>` },
      { key: 'ready', label: 'Status', sort: n => (n.ready ? 1 : 0), render: n => stLabel(n.ready ? (n.pressure.length ? n.pressure.join(', ') : 'Ready') : 'NotReady', n.ready ? (n.pressure.length ? 'warning' : 'good') : 'critical') + (n.unschedulable ? ' <span class="tag">cordoned</span>' : '') },
      { key: 'cpu', label: 'CPU used / allocatable', sort: n => pct(n.cpu.used, n.cpu.alloc), render: n => mcell(n.cpu.used, n.cpu.alloc, n.cpu.req, fmtCPU, s.metricsAvailable) },
      { key: 'mem', label: 'Memory used / allocatable', sort: n => pct(n.mem.used, n.mem.alloc), render: n => mcell(n.mem.used, n.mem.alloc, n.mem.req, fmtMem, s.metricsAvailable) },
      { key: 'reqc', label: 'CPU req', num: true, sort: n => pct(n.cpu.req, n.cpu.alloc), render: n => pct(n.cpu.req, n.cpu.alloc).toFixed(0) + '%' },
      { key: 'reqm', label: 'Mem req', num: true, sort: n => pct(n.mem.req, n.mem.alloc), render: n => pct(n.mem.req, n.mem.alloc).toFixed(0) + '%' },
      { key: 'pods', label: 'Pods', num: true, render: n => `${n.pods}/${n.podCap}` },
      { key: 'spark', label: 'CPU 10m', nosort: true, render: n => spark(n.spark) },
    ], d.nodes, { href: n => ctxPath(ctx) + '/node/' + enc(n.name) });
    const podCols = (kind) => [
      { key: 'name', label: 'Pod', cls: 'name', render: p => `${esc(p.name)}<span class="sub">${esc(p.namespace)}</span>` },
      { key: 'used', label: kind === 'cpu' ? 'CPU' : 'Memory', num: true, render: p => kind === 'cpu' ? fmtCPU(p.cpu.used) : fmtMem(p.mem.used) },
      { key: 'req', label: 'of request', num: true, render: p => { const u = kind === 'cpu' ? p.cpu : p.mem; return u.req ? pct(u.used, u.req).toFixed(0) + '%' : '<span class="muted">none</span>'; } },
    ];
    const topC = table('topC', podCols('cpu'), d.topCPU, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name) });
    const topM = table('topM', podCols('mem'), d.topMem, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name) });
    const probT = table('probs', [
      { key: 'name', label: 'Pod', cls: 'name', render: p => `${esc(p.name)}<span class="sub">${esc(p.namespace)}</span>` },
      { key: 'status', label: 'Status', render: p => stLabel(p.status) },
      { key: 'restarts', label: 'Restarts', num: true },
      { key: 'created', label: 'Age', num: true, render: p => age(p.created) },
    ], d.problems, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name) });
    main.innerHTML = clusterHead(s, shortName(ctx)) + `
      <div class="tiles">
        ${ns ? `<div class="card tile"><div class="label">Namespaces</div><div class="value">${(s.scopeNamespaces || []).length}</div><div class="foot" title="${esc((s.scopeNamespaces || []).join(', '))}">${esc(nsShort(s.scopeNamespaces))}</div></div>`
          : `<div class="card tile"><div class="label">Nodes ready</div><div class="value">${s.nodesReady}<small>/ ${s.nodesTotal}</small></div><div class="foot">${s.nodesReady === s.nodesTotal ? stLabel('all ready', 'good') : stLabel((s.nodesTotal - s.nodesReady) + ' not ready', 'critical')}</div></div>`}
        <div class="card tile"><div class="label">Pods running</div><div class="value">${fmtNum(s.pods.running)}<small>/ ${fmtNum(s.pods.total)}</small></div><div class="foot">${s.pods.problem ? stLabel(s.pods.problem + (s.pods.problem > 1 ? ' need' : ' needs') + ' attention', 'critical') : stLabel('no problems', 'good')}</div></div>
        <div class="card tile"><div class="label">Deployments healthy</div><div class="value">${s.deployHealthy}<small>/ ${s.deployTotal}</small></div><div class="foot">${s.namespaces} namespaces</div></div>
        <div class="card tile"><div class="label">CPU used</div>${cpuCap
            ? `<div class="value">${s.metricsAvailable ? cpuP.toFixed(0) + '%' : '—'}</div><div class="foot">${fmtCPU(s.cpu.used)} of ${fmtCPU(cpuCap)} cores ${capLbl} · req ${pct(s.cpu.req, cpuCap).toFixed(0)}%</div><div class="meter-wrap">${meter(s.cpu.used, cpuCap, s.cpu.req)}</div>`
            : `<div class="value">${s.metricsAvailable ? fmtCPU(s.cpu.used) : '—'}<small>cores</small></div><div class="foot">requests ${fmtCPU(s.cpu.req)} · limits ${fmtCPU(s.cpu.lim)}</div>`}</div>
        <div class="card tile"><div class="label">Memory used</div>${memCap
            ? `<div class="value">${s.metricsAvailable ? memP.toFixed(0) + '%' : '—'}</div><div class="foot">${fmtMem(s.mem.used)} of ${fmtMem(memCap)} ${capLbl} · req ${pct(s.mem.req, memCap).toFixed(0)}%</div><div class="meter-wrap">${meter(s.mem.used, memCap, s.mem.req)}</div>`
            : `<div class="value">${s.metricsAvailable ? fmtMem(s.mem.used) : '—'}</div><div class="foot">requests ${fmtMem(s.mem.req)} · limits ${fmtMem(s.mem.lim)}</div>`}</div>
        <div class="card tile"><div class="label">Warning events · 1h</div><div class="value">${s.warnings1h}</div><div class="foot"><a href="${ctxPath(ctx)}/events">view events →</a></div></div>
      </div>
      <div class="grid g2">
        ${chartCard('ovCpu', ns ? 'CPU — my namespaces' : 'Cluster CPU', 'cores · last hour', [{ name: 'Requests', value: s.cpu.req }, { name: capName(s), value: cpuCap, soft: true }])}
        ${chartCard('ovMem', ns ? 'Memory — my namespaces' : 'Cluster memory', 'bytes · last hour', [{ name: 'Requests', value: s.mem.req }, { name: capName(s), value: memCap, soft: true }])}
      </div>
      <div class="grid g2 mt">
        <div class="card" id="nsCpu"><div class="card-head"><h3>CPU by namespace</h3><span class="cap">top 10, current</span></div>${hbars(d.namespaces, r => r.cpu.used, fmtCPU)}</div>
        <div class="card" id="nsMem"><div class="card-head"><h3>Memory by namespace</h3><span class="cap">top 10, current</span></div>${hbars(d.namespaces, r => r.mem.used, fmtMem)}</div>
      </div>
      ${ns && !d.nodes.length ? '' : `<div class="card mt"><div class="card-head"><h3>Nodes</h3><span class="cap">bar = usage · tick = requested${ns ? ' (your namespaces only)' : ''}</span><a class="right muted" href="${ctxPath(ctx)}/nodes">all nodes →</a></div><div id="nodesBox">${nodesT.html}</div></div>`}
      <div class="grid g2 mt">
        <div class="card"><div class="card-head"><h3>Top pods by CPU</h3></div><div id="topCBox">${topC.html}</div></div>
        <div class="card"><div class="card-head"><h3>Top pods by memory</h3></div><div id="topMBox">${topM.html}</div></div>
      </div>
      <div class="grid g2 mt">
        <div class="card"><div class="card-head"><h3>Pods needing attention</h3><span class="cap">${d.problems.length}</span></div><div id="probBox">${probT.html}</div></div>
        <div class="card"><div class="card-head"><h3>Recent warnings</h3></div>${eventsList(d.warnings.slice(0, 12))}</div>
      </div>
      ${certCard(s)}`;
    drawUsageCharts('ov', d.history, [{ name: 'Requests', value: s.cpu.req }, { name: capName(s), value: cpuCap, soft: true }], [{ name: 'Requests', value: s.mem.req }, { name: capName(s), value: memCap, soft: true }]);
    bindHbarTips($('#nsCpu'), d.namespaces, r => r.cpu.used, 'cpu');
    bindHbarTips($('#nsMem'), d.namespaces, r => r.mem.used, 'mem');
    [[nodesT, '#nodesBox'], [topC, '#topCBox'], [topM, '#topMBox'], [probT, '#probBox']].forEach(([t, sel]) => $(sel) && t.bind($(sel), render));
    bindRefresh(ctx, render);
  };
  await live(15000, render);
}

function eventsList(evs) {
  if (!evs.length) return '<div class="muted">No warnings 🎉</div>';
  return `<div class="tbl-wrap"><table class="t"><tbody>${evs.map(e => `<tr><td style="width:1%;white-space:nowrap">${stLabel(e.reason, e.type === 'Warning' ? 'serious' : 'good')}</td>
    <td><span>${esc(e.message)}</span><span class="sub muted" style="display:block;font-size:11.5px">${esc(e.kind)}/${esc(e.name)} · ${esc(e.namespace)} · ${e.count}× · ${age(e.when)} ago</span></td></tr>`).join('')}</tbody></table></div>`;
}

// ------------------------------------------------------------------ NODES
async function viewNodes(route) {
  const ctx = route.ctx;
  const render = async () => {
    const [d, sum] = await Promise.all([api('nodes?ctx=' + enc(ctx)), api('clusters').then(l => l.find(c => c.name === ctx))]);
    const q = ($('#nq')?.value || '').toLowerCase();
    const rows = d.nodes.filter(n => !q || n.name.toLowerCase().includes(q) || n.roles.join().includes(q));
    const t = table('nodes', [
      { key: 'name', label: 'Node', cls: 'name', render: n => `${esc(n.name)}<span class="sub">${esc(n.roles.join(', '))} · ${esc(n.internalIP)}</span>` },
      { key: 'ready', label: 'Status', sort: n => (n.ready ? 1 : 0), render: n => stLabel(n.ready ? (n.pressure.length ? n.pressure.join(', ') : 'Ready') : 'NotReady', n.ready ? (n.pressure.length ? 'warning' : 'good') : 'critical') + (n.unschedulable ? ' <span class="tag">cordoned</span>' : '') },
      { key: 'cpu', label: 'CPU', sort: n => pct(n.cpu.used, n.cpu.alloc), render: n => mcell(n.cpu.used, n.cpu.alloc, n.cpu.req, fmtCPU, d.metrics) },
      { key: 'mem', label: 'Memory', sort: n => pct(n.mem.used, n.mem.alloc), render: n => mcell(n.mem.used, n.mem.alloc, n.mem.req, fmtMem, d.metrics) },
      { key: 'pods', label: 'Pods', num: true, render: n => `${n.pods}/${n.podCap}` },
      { key: 'version', label: 'Kubelet' },
      { key: 'runtime', label: 'Runtime', render: n => esc(n.runtime.replace('containerd://', 'containerd ')) },
      { key: 'created', label: 'Age', num: true, sort: n => -new Date(n.created), render: n => age(n.created) },
      { key: 'spark', label: 'CPU 10m', nosort: true, render: n => spark(n.spark) },
    ], rows, { href: n => ctxPath(ctx) + '/node/' + enc(n.name) });
    if (isNs(sum) && !d.nodes.length) {
      main.innerHTML = clusterHead(sum, 'Nodes', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div>`) +
        `<div class="empty-state"><h2>Nodes need cluster-wide access</h2><p>Nodes are cluster-scoped, and your kubeconfig user only has Role/RoleBinding rights in ${esc(nsShort(sum.scopeNamespaces, 6))}.</p>
        <p class="muted">Pods still show which node they run on. To see nodes, ask for a ClusterRole with <code>get, list</code> on <code>nodes</code> (and <code>nodes</code> in <code>metrics.k8s.io</code>).</p></div>`;
      bindRefresh(ctx, render);
      return;
    }
    if (!$('#nodesBox')) {
      main.innerHTML = clusterHead(sum, 'Nodes', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div>`) +
        `<div class="filters"><input type="search" id="nq" placeholder="Filter by name or role"><span class="count" id="ncount"></span></div><div class="card" id="nodesBox"></div>`;
      $('#nq').addEventListener('input', render);
      bindRefresh(ctx, render);
    }
    $('#nodesBox').innerHTML = t.html; t.bind($('#nodesBox'), render);
    $('#ncount').textContent = `${rows.length} of ${d.nodes.length} nodes`;
  };
  await live(15000, render);
}

async function viewNode(route) {
  const ctx = route.ctx, name = route.args[0];
  const render = async () => {
    const d = await api(`node?ctx=${enc(ctx)}&name=${enc(name)}`);
    const n = d.node;
    const pt = table('nodePods', podColumnsWithActions(ctx, false), d.pods, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name) });
    main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/nodes">Nodes</a></div>
      <h1><span class="kind">Node</span>${esc(n.name)}</h1><div class="sub">${stLabel(n.ready ? 'Ready' : 'NotReady', n.ready ? 'good' : 'critical')}${n.unschedulable ? '<span class="tag">cordoned</span>' : ''}<span>${esc(n.roles.join(', '))}</span><span>${esc(n.version)}</span><span>${esc(n.internalIP)}</span></div></div>
      <div class="right">${ro() ? '' : `<button class="btn" id="aCordon">${ICON.pause}${n.unschedulable ? 'Uncordon' : 'Cordon'}</button>`}
        <a class="btn" href="${objHref(ctx, '', 'nodes', '', n.name, 'yaml')}">${ICON.edit}YAML</a></div></div>
      <div class="tiles" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        <div class="card tile"><div class="label">CPU used</div><div class="value">${fmtCPU(n.cpu.used)}<small>/ ${fmtCPU(n.cpu.alloc)} cores</small></div><div class="foot">requests ${fmtCPU(n.cpu.req)} · limits ${fmtCPU(n.cpu.lim)}</div>${meter(n.cpu.used, n.cpu.alloc, n.cpu.req)}</div>
        <div class="card tile"><div class="label">Memory used</div><div class="value">${fmtMem(n.mem.used)}<small>/ ${fmtMem(n.mem.alloc)}</small></div><div class="foot">requests ${fmtMem(n.mem.req)} · limits ${fmtMem(n.mem.lim)}</div>${meter(n.mem.used, n.mem.alloc, n.mem.req)}</div>
        <div class="card tile"><div class="label">Pods</div><div class="value">${n.pods}<small>/ ${n.podCap}</small></div>${meter(n.pods, n.podCap)}</div>
        <div class="card tile"><div class="label">Conditions</div><div class="value" style="font-size:15px;margin-top:6px">${n.pressure.length ? n.pressure.map(p => stLabel(p, 'warning')).join(' ') : stLabel('No pressure', 'good')}</div><div class="foot">${n.unschedulable ? 'cordoned' : 'schedulable'}</div></div>
      </div>
      <div class="grid g2">${chartCard('ndCpu', 'CPU', 'cores · last hour', [{ name: 'Requests', value: n.cpu.req }, { name: 'Allocatable', value: n.cpu.alloc, soft: true }])}
        ${chartCard('ndMem', 'Memory', 'last hour', [{ name: 'Requests', value: n.mem.req }, { name: 'Allocatable', value: n.mem.alloc, soft: true }])}</div>
      <div class="grid g2 mt"><div class="card"><h3>System</h3><div class="kvgrid" style="margin-top:8px">
        <span>OS</span><span>${esc(n.os)}</span><span>Kernel</span><span>${esc(n.kernel)}</span><span>Runtime</span><span>${esc(n.runtime)}</span>
        <span>Architecture</span><span>${esc(n.arch)}</span><span>Capacity</span><span>${fmtCPU(n.cpu.cap)} cores · ${fmtMem(n.mem.cap)}</span><span>Age</span><span>${age(n.created)}</span>
        <span>Taints</span><span>${n.taints.length ? n.taints.map(t => `<span class="chip">${esc(t)}</span>`).join(' ') : '—'}</span></div></div>
        <div class="card"><h3>Labels</h3><div class="chips" style="margin-top:8px">${Object.entries(n.labels || {}).map(([k, v]) => `<span class="chip">${esc(k)}${v ? '=' + esc(v) : ''}</span>`).join('')}</div></div></div>
      <div class="card mt"><div class="card-head"><h3>Pods on this node</h3><span class="cap">${d.pods.length}</span></div><div id="npBox">${pt.html}</div></div>
      <div class="card mt"><div class="card-head"><h3>Events</h3></div>${eventsList(d.events)}</div>`;
    $('#aCordon')?.addEventListener('click', async () => { if (await A.cordon(ctx, n.name, !n.unschedulable)) setTimeout(render, 1200); });
    drawUsageCharts('nd', d.history, [{ name: 'Requests', value: n.cpu.req }, { name: 'Allocatable', value: n.cpu.alloc, soft: true }], [{ name: 'Requests', value: n.mem.req }, { name: 'Allocatable', value: n.mem.alloc, soft: true }]);
    pt.bind($('#npBox'), render);
    bindPodActions($('#npBox'), pt.rows, ctx, render);
  };
  await live(15000, render);
}

// ------------------------------------------------------------------ WORKLOADS
async function nsOptions(ctx, selected, allLabel = 'All namespaces') {
  const nss = await api('namespaces?ctx=' + enc(ctx));
  return `<option value="">${allLabel}</option>` + nss.map(n => `<option ${n.name === selected ? 'selected' : ''}>${esc(n.name)}</option>`).join('');
}
function healthLabel(h) {
  return stLabel(h, { healthy: 'good', progressing: 'warning', 'scaled-down': 'warning', degraded: 'critical' }[h] || 'warning');
}
async function viewWorkloads(route) {
  const ctx = route.ctx;
  const sum = (await api('clusters')).find(c => c.name === ctx);
  const kindQ = route.q.kind || '';
  main.innerHTML = clusterHead(sum, kindQ ? kindQ + 's' : 'Workloads', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div>`) + `
    <div class="filters"><select id="wkind"><option value="">All kinds</option>${['Deployment', 'StatefulSet', 'DaemonSet'].map(k => `<option ${k === kindQ ? 'selected' : ''}>${k}</option>`).join('')}</select>
      <select id="whealth"><option value="">Any health</option><option value="healthy">Healthy</option><option value="unhealthy">Not healthy</option></select>
      <input type="search" id="wq" placeholder="Filter by name or image"><span class="count" id="wcount"></span></div>
    <div class="card" id="wBox"><div class="muted">Loading…</div></div>`;
  bindRefresh(ctx, () => render());
  const render = async () => {
    const d = await api(`workloads?ctx=${enc(ctx)}&ns=${enc(S.ns)}&kind=${enc($('#wkind').value)}`);
    const q = $('#wq').value.toLowerCase(), hf = $('#whealth').value;
    const rows = d.workloads.filter(w => (!q || w.name.toLowerCase().includes(q) || w.images.join().toLowerCase().includes(q))
      && (!hf || (hf === 'healthy') === (w.health === 'healthy')));
    const t = table('wl', [
      { key: 'name', label: 'Name', cls: 'name', render: w => `${esc(w.name)}<span class="sub">${esc(w.namespace)} · ${esc(w.kind)}</span>` },
      { key: 'health', label: 'Health', render: w => healthLabel(w.health) },
      { key: 'ready', label: 'Ready', num: true, sort: w => w.ready / (w.desired || 1), render: w => `${w.ready}/${w.desired}` },
      { key: 'cpu', label: 'CPU used / req', num: true, sort: w => w.cpu.used, render: w => d.metrics ? `${fmtCPU(w.cpu.used)} <span class="muted">/ ${w.cpu.req ? fmtCPU(w.cpu.req) : 'none'}</span>` : `<span class="muted">req ${w.cpu.req ? fmtCPU(w.cpu.req) : 'none'}</span>` },
      { key: 'mem', label: 'Memory used / req', num: true, sort: w => w.mem.used, render: w => d.metrics ? `${fmtMem(w.mem.used)} <span class="muted">/ ${w.mem.req ? fmtMem(w.mem.req) : 'none'}</span>` : `<span class="muted">req ${w.mem.req ? fmtMem(w.mem.req) : 'none'}</span>` },
      { key: 'restarts', label: 'Restarts', num: true },
      { key: 'spark', label: 'CPU 10m', nosort: true, render: w => spark(w.spark) },
      { key: 'created', label: 'Age', num: true, sort: w => -new Date(w.created), render: w => age(w.created) },
      { key: 'act', label: '', nosort: true, cls: 'act', render: w => `<a class="btn sm ghost" data-logs href="${ctxPath(ctx)}/logs?ns=${enc(w.namespace)}&kind=${enc(w.kind)}&name=${enc(w.name)}" title="Logs of all pods">${ICON.logs}Logs</a><button class="icon-btn" data-menu title="Actions">${ICON.more}</button>` },
    ], rows, { href: w => `${ctxPath(ctx)}/wl/${enc(w.kind)}/${enc(w.namespace)}/${enc(w.name)}`, sort: { key: 'name', dir: 'asc' } });
    $('#wBox').innerHTML = t.html; t.bind($('#wBox'), render);
    $$('[data-logs]', $('#wBox')).forEach(a => a.addEventListener('click', e => e.stopPropagation()));
    $$('[data-menu]', $('#wBox')).forEach(b => b.addEventListener('click', e => {
      e.stopPropagation();
      const w = t.rows[+b.closest('tr').dataset.i];
      const [g, r] = KIND_REF[w.kind];
      objectMenu(b, { ctx, group: g, resource: r, ns: w.namespace, name: w.name, kind: w.kind, replicas: w.desired }, render);
    }));
    $('#wcount').textContent = `${rows.length} of ${d.workloads.length}`;
  };
  ['#wkind', '#whealth'].forEach(s => $(s).addEventListener('change', render));
  $('#wq').addEventListener('input', render);
  await live(15000, render);
}

async function viewWorkload(route) {
  const ctx = route.ctx, [kind, ns, name] = route.args;
  const render = async () => {
    const d = await api(`workload?ctx=${enc(ctx)}&kind=${enc(kind)}&ns=${enc(ns)}&name=${enc(name)}`);
    const w = d.workload;
    const pt = table('wlPods', podColumnsWithActions(ctx, true), d.pods, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name) });
    const logsHref = `${ctxPath(ctx)}/logs?ns=${enc(ns)}&kind=${enc(kind)}&name=${enc(name)}`;
    const [g, r] = KIND_REF[kind];
    main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/workloads?kind=${enc(kind)}">${esc(kind)}s</a> / ${esc(ns)}</div>
      <h1><span class="kind">${esc(kind)}</span>${esc(name)}</h1><div class="sub">${healthLabel(w.health)}${d.strategy ? `<span>${esc(d.strategy)}</span>` : ''}<span>age ${age(w.created)}</span></div></div>
      <div class="right"><a class="btn primary" href="${logsHref}">${ICON.logs}Logs of all ${w.pods} pods</a>
        ${!ro() && kind !== 'DaemonSet' ? `<button class="btn" id="aScale">${ICON.scale}Scale</button>` : ''}
        ${!ro() ? `<button class="btn" id="aRestart">${ICON.restart}Restart</button>` : ''}
        ${!ro() ? `<button class="btn" id="aExecAll" title="Open a shell in every running pod, side by side, with broadcast input">${ICON.term}Shell in all pods</button>` : ''}
        <a class="btn" href="${objHref(ctx, g, r, ns, name, 'yaml')}">${ICON.edit}YAML</a>
        <button class="btn ghost" id="aMore" title="More actions">${ICON.more}</button></div></div>
      <div class="tiles" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        <div class="card tile"><div class="label">Replicas ready</div><div class="value">${w.ready}<small>/ ${w.desired}</small></div><div class="foot">updated ${w.updated} · available ${w.available}</div></div>
        <div class="card tile"><div class="label">CPU used</div><div class="value">${fmtCPU(w.cpu.used)}<small>cores</small></div><div class="foot">requests ${fmtCPU(w.cpu.req)} · limits ${fmtCPU(w.cpu.lim) || '—'}</div>${w.cpu.req ? meter(w.cpu.used, w.cpu.lim || w.cpu.req * 2, w.cpu.req) : ''}</div>
        <div class="card tile"><div class="label">Memory used</div><div class="value">${fmtMem(w.mem.used)}</div><div class="foot">requests ${fmtMem(w.mem.req)} · limits ${fmtMem(w.mem.lim) || '—'}</div>${w.mem.req ? meter(w.mem.used, w.mem.lim || w.mem.req * 2, w.mem.req) : ''}</div>
        <div class="card tile"><div class="label">Restarts</div><div class="value">${w.restarts}</div><div class="foot">across ${w.pods} pods</div></div>
      </div>
      <div class="grid g2">${chartCard('wlCpu', 'CPU — all pods', 'cores · last hour', [{ name: 'Requests', value: w.cpu.req }, { name: 'Limits', value: w.cpu.lim, soft: true }])}
        ${chartCard('wlMem', 'Memory — all pods', 'last hour', [{ name: 'Requests', value: w.mem.req }, { name: 'Limits', value: w.mem.lim, soft: true }])}</div>
      <div class="grid g2 mt"><div class="card"><h3>Selector</h3><div class="cap">logs and pods are matched with these labels</div><div class="chips">${w.selector.split(',').filter(Boolean).map(s => `<span class="chip">${esc(s)}</span>`).join('')}</div>
        ${(d.conditions || []).length ? `<h3 class="mt">Conditions</h3><div class="kvgrid" style="margin-top:6px">${d.conditions.map(c => `<span>${esc(c.type)}</span><span>${stLabel(c.status, c.status === 'True' ? 'good' : 'critical')} <span class="muted">${esc(c.reason || '')} ${esc(c.message || '')}</span></span>`).join('')}</div>` : ''}</div>
        <div class="card"><h3>Containers</h3><div class="tbl-wrap" style="margin-top:6px"><table class="t"><thead><tr><th>Name</th><th>Image</th><th>Requests</th><th>Limits</th></tr></thead><tbody>
        ${d.containers.map(c => `<tr><td class="name">${esc(c.name)}</td><td class="mono" style="word-break:break-all">${esc(c.image)}</td><td>${esc(Object.entries(c.requests || {}).map(([k, v]) => k + ' ' + v).join(', ') || '—')}</td><td>${esc(Object.entries(c.limits || {}).map(([k, v]) => k + ' ' + v).join(', ') || '—')}</td></tr>`).join('')}</tbody></table></div></div></div>
      <div class="card mt"><div class="card-head"><h3>Pods</h3><span class="cap">${d.pods.length}</span></div><div id="wpBox">${pt.html}</div></div>
      <div class="card mt"><div class="card-head"><h3>Events</h3><span class="cap">this ${esc(kind.toLowerCase())}, its ReplicaSets and pods</span></div>${eventsList(d.events.slice(0, 30))}</div>`;
    $('#aScale')?.addEventListener('click', async () => { if (await A.scale(ctx, g, r, ns, name, w.desired)) setTimeout(render, 1500); });
    $('#aRestart')?.addEventListener('click', async () => { if (await A.restart(ctx, g, r, ns, name, kind)) setTimeout(render, 1500); });
    $('#aExecAll')?.addEventListener('click', () => A.execAll(ctx, ns, d.pods.filter(p => p.phase === 'Running').map(p => p.name), d.containers.map(c => c.name)));
    $('#aMore').addEventListener('click', () => objectMenu($('#aMore'), { ctx, group: g, resource: r, ns, name, kind, replicas: w.desired },
      deleted => { if (deleted) location.hash = `${ctxPath(ctx)}/workloads?kind=${kind}`; else render(); }));
    drawUsageCharts('wl', d.history, [{ name: 'Requests', value: w.cpu.req }, { name: 'Limits', value: w.cpu.lim, soft: true }], [{ name: 'Requests', value: w.mem.req }, { name: 'Limits', value: w.mem.lim, soft: true }]);
    pt.bind($('#wpBox'), render);
    bindPodActions($('#wpBox'), pt.rows, ctx, render);
  };
  await live(15000, render);
}

// ------------------------------------------------------------------ PODS
function podColumns(ctx, compact) {
  const cols = [
    { key: 'name', label: 'Pod', cls: 'name', render: p => `${esc(p.name)}<span class="sub">${esc(p.namespace)}${p.owner ? ' · ' + esc(p.owner) : ''}</span>` },
    { key: 'status', label: 'Status', render: p => stLabel(p.status, p.problem ? 'critical' : statusClass(p.status))
        + (p.restarts > 0 && p.lastReason ? `<span class="sub">last restart: ${esc(p.lastReason)}${p.lastExit != null ? ` (${p.lastExit})` : ''}</span>` : '') },
    { key: 'ready', label: 'Ready', num: true },
    { key: 'restarts', label: 'Restarts', num: true, render: p => p.restarts > 0
        ? `${p.restarts}${p.lastRestart ? `<span class="sub">${age(p.lastRestart)} ago</span>` : ''}` : '0' },
    { key: 'cpu', label: 'CPU used / req / lim', num: true, sort: p => p.cpu.used, render: p => `${p.hasMetrics ? fmtCPU(p.cpu.used) : '—'} <span class="muted">/ ${fmtCPU(p.cpu.req)} / ${fmtCPU(p.cpu.lim)}</span>` },
    { key: 'mem', label: 'Memory used / req / lim', num: true, sort: p => p.mem.used, render: p => `${p.hasMetrics ? fmtMem(p.mem.used) : '—'} <span class="muted">/ ${fmtMem(p.mem.req)} / ${fmtMem(p.mem.lim)}</span>` },
  ];
  if (!compact) cols.push({ key: 'node', label: 'Node', cls: 'mono' });
  else cols.push({ key: 'node', label: 'Node' });
  cols.push({ key: 'created', label: 'Age', num: true, sort: p => -new Date(p.created), render: p => age(p.created) });
  return cols;
}
function podColumnsWithActions(ctx, compact) {
  const cols = podColumns(ctx, compact);
  cols.push({ key: 'act', label: '', nosort: true, cls: 'act', render: () => `${ro() ? '' : `<button class="icon-btn" data-exec title="Exec shell">${ICON.term}</button>`}<button class="icon-btn" data-menu title="Actions">${ICON.more}</button>` });
  return cols;
}
function bindPodActions(box, rows, ctx, after) {
  $$('[data-exec]', box).forEach(b => b.addEventListener('click', e => {
    e.stopPropagation();
    const p = rows[+b.closest('tr').dataset.i];
    A.exec(ctx, p.namespace, p.name, p.containers);
  }));
  $$('[data-menu]', box).forEach(b => b.addEventListener('click', e => {
    e.stopPropagation();
    const p = rows[+b.closest('tr').dataset.i];
    objectMenu(b, { ctx, group: '', resource: 'pods', ns: p.namespace, name: p.name, kind: 'Pod', containers: p.containers, fromList: true }, after);
  }));
}

async function viewPods(route) {
  const ctx = route.ctx;
  const sum = (await api('clusters')).find(c => c.name === ctx);
  main.innerHTML = clusterHead(sum, 'Pods', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div>`) + `
    <div class="filters"><select id="pst"><option value="">Any status</option><option value="problem">Needs attention</option><option value="Running">Running</option><option value="Pending">Pending</option><option value="Failed">Failed</option><option value="Succeeded">Completed</option></select>
      <input type="search" id="pq" placeholder="Filter by name, owner or node"><span class="count" id="pcount"></span></div>
    <div class="card" id="pBox"><div class="muted">Loading…</div></div>`;
  if (route.q.status) $('#pst').value = route.q.status;
  bindRefresh(ctx, () => render());
  // refresh-rate picker (top right), remembered across visits
  const rates = [[1000, '1s'], [5000, '5s'], [10000, '10s'], [30000, '30s'], [60000, '1 min']];
  let rate = 5000;
  try { rate = +localStorage.getItem('kd.pods.poll') || 5000; } catch {}
  if (!rates.some(([ms]) => ms === rate)) rate = 5000;
  $('.page-head .right').insertAdjacentHTML('afterbegin', `<label class="pollsel" title="How often this page reloads pods from the cluster"><span class="livedot"></span>Refresh
    <select id="pollRate">${rates.map(([ms, l]) => `<option value="${ms}" ${ms === rate ? 'selected' : ''}>${l}</option>`).join('')}</select></label>`);
  $('#pollRate').addEventListener('change', e => {
    rate = +e.target.value;
    try { localStorage.setItem('kd.pods.poll', String(rate)); } catch {}
    S.kick?.();
  });
  const render = async () => {
    const d = await api(`pods?ctx=${enc(ctx)}&ns=${enc(S.ns)}&fresh=1`);
    const up = $('#updated'); if (up) up.textContent = (d.stale ? 'showing cached data · ' : '') + 'updated ' + new Date().toLocaleTimeString();
    const q = $('#pq').value.toLowerCase(), stf = $('#pst').value;
    const rows = d.pods.filter(p => (!q || p.name.toLowerCase().includes(q) || (p.owner || '').toLowerCase().includes(q) || (p.node || '').toLowerCase().includes(q))
      && (!stf || (stf === 'problem' ? p.problem : p.phase === stf)));
    const cols = podColumns(ctx, false);
    cols.push({ key: 'act', label: '', nosort: true, cls: 'act', render: p => `${ro() ? '' : `<button class="icon-btn" data-exec title="Exec shell">${ICON.term}</button>`}<button class="icon-btn" data-menu title="Actions">${ICON.more}</button>` });
    const t = table('pods', cols, rows, { href: p => ctxPath(ctx) + '/pod/' + enc(p.namespace) + '/' + enc(p.name), limit: 1000 });
    $('#pBox').innerHTML = t.html; t.bind($('#pBox'), render);
    bindPodActions($('#pBox'), t.rows, ctx, render);
    $('#pcount').textContent = `${rows.length} of ${d.pods.length}`;
  };
  $('#pst').addEventListener('change', render);
  $('#pq').addEventListener('input', render);
  await live(() => rate, render);
}

async function viewPod(route) {
  const ctx = route.ctx, [ns, name] = route.args;
  const render = async () => {
    const d = await api(`pod?ctx=${enc(ctx)}&ns=${enc(ns)}&name=${enc(name)}`);
    const p = d.pod;
    const [okind, oname] = (p.owner || '').split('/');
    const logsHref = ['Deployment', 'StatefulSet', 'DaemonSet'].includes(okind)
      ? `${ctxPath(ctx)}/logs?ns=${enc(ns)}&kind=${enc(okind)}&name=${enc(oname)}`
      : `${ctxPath(ctx)}/logs?ns=${enc(ns)}&selector=${enc(Object.entries(p.labels || {}).filter(([k]) => k !== 'pod-template-hash').map(([k, v]) => k + '=' + v).join(','))}`;
    const ownerLink = ['Deployment', 'StatefulSet', 'DaemonSet'].includes(okind) ? `<a href="${ctxPath(ctx)}/wl/${enc(okind)}/${enc(ns)}/${enc(oname)}">${esc(p.owner)}</a>` : esc(p.owner || '—');
    main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/pods?ns=${enc(ns)}">Pods</a> / ${esc(ns)}</div>
      <h1>${esc(name)}</h1><div class="sub">${stLabel(p.status, p.problem ? 'critical' : statusClass(p.status))}<span>owner ${ownerLink}</span><span>node ${esc(p.node || '—')}</span><span>${esc(p.ip || '')}</span><span>${esc(p.qos)}</span></div></div>
      <div class="right">${ro() ? '' : `<button class="btn primary" id="aExec">${ICON.term}Exec shell</button>`}
        <a class="btn" href="${ctxPath(ctx)}/logs?ns=${enc(ns)}&pod=${enc(name)}">${ICON.logs}Logs</a>
        ${['Deployment', 'StatefulSet', 'DaemonSet'].includes(okind) ? `<a class="btn" href="${logsHref}">All ${esc(okind.toLowerCase())} logs</a>` : ''}
        <a class="btn" href="${objHref(ctx, '', 'pods', ns, name, 'yaml')}">${ICON.edit}YAML</a>
        <button class="btn ghost" id="aMore" title="More actions">${ICON.more}</button></div></div>
      <div class="tiles" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        <div class="card tile"><div class="label">CPU used</div><div class="value">${p.hasMetrics ? fmtCPU(p.cpu.used) : '—'}</div><div class="foot">req ${fmtCPU(p.cpu.req)} · lim ${fmtCPU(p.cpu.lim)}</div>${p.cpu.lim ? meter(p.cpu.used, p.cpu.lim, p.cpu.req) : ''}</div>
        <div class="card tile"><div class="label">Memory used</div><div class="value">${p.hasMetrics ? fmtMem(p.mem.used) : '—'}</div><div class="foot">req ${fmtMem(p.mem.req)} · lim ${fmtMem(p.mem.lim)}</div>${p.mem.lim ? meter(p.mem.used, p.mem.lim, p.mem.req) : ''}</div>
        <div class="card tile"><div class="label">Ready</div><div class="value">${esc(p.ready)}</div><div class="foot">age ${age(p.created)}</div></div>
        <div class="card tile"><div class="label">Restarts</div><div class="value">${p.restarts}</div><div class="foot">${p.lastReason ? 'last: ' + esc(p.lastReason) : '&nbsp;'}</div></div>
      </div>
      <div class="grid g2">${chartCard('pdCpu', 'CPU', 'cores · last hour', [{ name: 'Requests', value: p.cpu.req }, { name: 'Limits', value: p.cpu.lim, soft: true }])}
        ${chartCard('pdMem', 'Memory', 'last hour', [{ name: 'Requests', value: p.mem.req }, { name: 'Limits', value: p.mem.lim, soft: true }])}</div>
      <div class="card mt"><h3>Containers</h3><div class="tbl-wrap" style="margin-top:6px"><table class="t"><thead><tr><th>Name</th><th>State</th><th>Ready</th><th class="num">Restarts</th><th>Last termination</th><th>Image</th></tr></thead><tbody>
      ${(d.statuses || []).map(c => {
        const s = c.state || {}, ls = c.lastState || {};
        const state = s.running ? stLabel('Running', 'good') + ` <span class="muted">since ${age(s.running.startedAt)}</span>` : s.waiting ? stLabel(s.waiting.reason, 'critical') : s.terminated ? stLabel(s.terminated.reason, s.terminated.exitCode ? 'critical' : 'good') : '—';
        const last = ls.terminated ? `${esc(ls.terminated.reason)} (exit ${ls.terminated.exitCode}) ${age(ls.terminated.finishedAt)} ago` : '—';
        return `<tr><td class="name">${esc(c.name)}</td><td>${state}</td><td>${c.ready ? 'yes' : 'no'}</td><td class="num">${c.restartCount}</td><td>${last}</td><td class="mono" style="word-break:break-all">${esc(c.image)}</td></tr>`;
      }).join('') || '<tr><td colspan="6" class="muted">Not started yet</td></tr>'}</tbody></table></div></div>
      <div class="grid g2 mt"><div class="card"><h3>Labels</h3><div class="chips" style="margin-top:8px">${Object.entries(p.labels || {}).map(([k, v]) => `<span class="chip">${esc(k)}=${esc(v)}</span>`).join('')}</div></div>
        <div class="card"><h3>Conditions</h3><div class="kvgrid" style="margin-top:8px">${(d.conditions || []).map(c => `<span>${esc(c.type)}</span><span>${stLabel(c.status, c.status === 'True' ? 'good' : 'warning')} <span class="muted">${esc(c.reason || '')}</span></span>`).join('')}</div></div></div>
      <div class="card mt"><div class="card-head"><h3>Events</h3></div>${eventsList(d.events)}</div>`;
    $('#aExec')?.addEventListener('click', () => A.exec(ctx, ns, name, p.containers));
    $('#aMore').addEventListener('click', () => objectMenu($('#aMore'), { ctx, group: '', resource: 'pods', ns, name, kind: 'Pod', containers: p.containers },
      deleted => { if (deleted) location.hash = `${ctxPath(ctx)}/pods`; else render(); }));
    drawUsageCharts('pd', d.history, [{ name: 'Requests', value: p.cpu.req }, { name: 'Limits', value: p.cpu.lim, soft: true }], [{ name: 'Requests', value: p.mem.req }, { name: 'Limits', value: p.mem.lim, soft: true }]);
  };
  await live(15000, render);
}

// ------------------------------------------------------------------ EVENTS
async function viewEvents(route) {
  const ctx = route.ctx;
  const sum = (await api('clusters')).find(c => c.name === ctx);
  main.innerHTML = clusterHead(sum, 'Events', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div>`) + `
    <div class="filters"><select id="etype"><option value="Warning">Warnings</option><option value="">All types</option><option value="Normal">Normal</option></select>
      <input type="search" id="eq" placeholder="Filter by reason, object or message"><span class="count" id="ecount"></span></div>
    <div class="card" id="eBox"><div class="muted">Loading…</div></div>`;
  bindRefresh(ctx, () => render());
  const render = async () => {
    const evs = await api(`events?ctx=${enc(ctx)}&ns=${enc(S.ns)}`);
    const q = $('#eq').value.toLowerCase(), ty = $('#etype').value;
    const rows = evs.filter(e => (!ty || e.type === ty) && (!q || (e.reason + ' ' + e.name + ' ' + e.message).toLowerCase().includes(q)));
    const t = table('events', [
      { key: 'when', label: 'Last seen', sort: e => -new Date(e.when), render: e => age(e.when) + ' ago' },
      { key: 'type', label: 'Type', render: e => stLabel(e.type, e.type === 'Warning' ? 'serious' : 'good') },
      { key: 'reason', label: 'Reason', cls: 'name' },
      { key: 'name', label: 'Object', render: e => `${esc(e.kind)}/${esc(e.name)}<span class="sub">${esc(e.namespace)}</span>` },
      { key: 'message', label: 'Message', render: e => `<span style="white-space:normal">${esc(e.message)}</span>` },
      { key: 'count', label: 'Count', num: true },
    ], rows, { sort: { key: 'when', dir: 'asc' } });
    $('#eBox').innerHTML = t.html; t.bind($('#eBox'), render);
    $('#ecount').textContent = `${rows.length} events`;
  };
  $('#etype').addEventListener('change', render);
  $('#eq').addEventListener('input', render);
  await live(20000, render);
}

// ------------------------------------------------------------------ LOGS
const MAX_LINES = 20000;
const POD_SLOTS = ['--s1', '--s2', '--s3', '--s4', '--s5', '--s6', '--s7', '--s8'];
function levelOf(line) {
  if (/\b(level=)?(error|err|fatal|panic|critical|exception)\b|"level":"(error|fatal)"/i.test(line)) return 'error';
  if (/\b(level=)?(warn|warning)\b|"level":"warn/i.test(line)) return 'warn';
  return '';
}
async function viewLogs(route) {
  const ctx = route.ctx, q = route.q;
  const L = { lines: [], hidden: new Set(), colors: new Map(), paused: false, buffer: [], pods: [], ctrl: null, follow: q.follow !== '0', live: false };
  main.innerHTML = `<div class="logs-page">
    <div class="page-head" style="margin-bottom:10px"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a></div><h1>Logs</h1></div></div>
    <div class="logbar">
      <select id="lns" title="Namespace">${await nsOptions(ctx, q.ns || S.ns || '', 'Namespace…')}</select>
      <select id="lwl" title="Deployment / workload" style="min-width:220px"><option value="">Deployment…</option></select>
      <button class="btn ${q.selector ? 'on' : ''}" id="selMode" title="Use a custom label selector instead">Label selector</button>
      <input type="text" id="lsel" placeholder="app=web,tier=api" spellcheck="false" style="width:220px" ${q.selector ? '' : 'hidden'} value="${esc(q.selector || '')}">
      <select id="lct" title="Container"><option value="">All containers</option></select>
      <select id="ltail" title="Lines per container">${[100, 200, 500, 1000, 5000].map(n => `<option ${n == (q.tail || 200) ? 'selected' : ''} value="${n}">last ${n}</option>`).join('')}</select>
      <select id="lsince" title="Time window"><option value="">any time</option><option value="300">5 min</option><option value="900">15 min</option><option value="3600">1 hour</option><option value="21600">6 hours</option><option value="86400">24 hours</option></select>
      <label class="ck"><input type="checkbox" id="lfollow" ${L.follow ? 'checked' : ''}>Follow</label>
      <label class="ck" title="Logs from the previous (crashed) container instance"><input type="checkbox" id="lprev">Previous</label>
    </div>
    <div class="logbar">
      <input type="search" id="lq" placeholder="Search… (regex ok)" style="width:260px">
      <label class="ck"><input type="checkbox" id="lonly">Only matching</label>
      <select id="llevel"><option value="">All levels</option><option value="warn">Warn + error</option><option value="error">Errors only</option></select>
      <span class="grow"></span>
      <label class="ck"><input type="checkbox" id="lts" checked>Timestamps</label>
      <label class="ck"><input type="checkbox" id="lwrap">Wrap</label>
      <button class="btn" id="lpause">Pause</button><button class="btn" id="lclear">Clear</button>
      <a class="btn" id="ldl" href="#">Download</a>
    </div>
    <div class="podchips" id="lpods"></div>
    <div class="logview" id="lview"><div class="ll sys">Pick a namespace and deployment — logs from every pod it owns are merged here.</div></div>
    <div class="logstatus"><span id="lstate"></span><span id="lcount"></span><span id="lselshow" class="mono"></span></div>
  </div>`;

  const view = $('#lview');
  const fillWorkloads = async () => {
    const ns = $('#lns').value;
    if (!ns) { $('#lwl').innerHTML = '<option value="">Deployment…</option>'; return; }
    const d = await api(`workloads?ctx=${enc(ctx)}&ns=${enc(ns)}`);
    const by = k => d.workloads.filter(w => w.kind === k);
    $('#lwl').innerHTML = '<option value="">Deployment…</option>' + ['Deployment', 'StatefulSet', 'DaemonSet'].filter(k => by(k).length)
      .map(k => `<optgroup label="${k}s">${by(k).map(w => `<option value="${esc(k + '/' + w.name)}" ${q.kind === k && q.name === w.name ? 'selected' : ''}>${esc(w.name)} (${w.ready}/${w.desired})</option>`).join('')}</optgroup>`).join('');
  };
  await fillWorkloads();
  if (q.since) $('#lsince').value = q.since;

  let podOnly = q.pod || '';
  if (podOnly) $('#lsel').insertAdjacentHTML('afterend', `<span class="chip" id="podOnly" title="Showing one pod — click to pick a deployment instead" style="cursor:pointer">pod ${esc(podOnly)} ✕</span>`);
  $('#podOnly')?.addEventListener('click', () => { podOnly = ''; $('#podOnly').remove(); start(); });
  const params = () => {
    const p = new URLSearchParams({ ctx, ns: $('#lns').value, tail: $('#ltail').value, follow: $('#lfollow').checked ? '1' : '0' });
    if (podOnly) p.set('pod', podOnly);
    else if (!$('#lsel').hidden && $('#lsel').value.trim()) p.set('selector', $('#lsel').value.trim());
    else { const [k, n] = ($('#lwl').value || '/').split('/'); p.set('kind', k); p.set('name', n); }
    if ($('#lct').value) p.set('container', $('#lct').value);
    if ($('#lsince').value) p.set('since', $('#lsince').value);
    if ($('#lprev').checked) p.set('previous', '1');
    return p;
  };
  const ready = () => $('#lns').value && (podOnly || (!$('#lsel').hidden && $('#lsel').value.trim()) || ($('#lsel').hidden && $('#lwl').value));

  // ---- rendering
  let re = null;
  const compileSearch = () => {
    const s = $('#lq').value;
    if (!s) { re = null; return; }
    try { re = new RegExp(s, 'gi'); } catch { re = new RegExp(s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'gi'); }
  };
  const visible = l => {
    if (L.hidden.has(l.p)) return false;
    const lv = $('#llevel').value;
    if (lv && !(l.lv === 'error' || (lv === 'warn' && l.lv === 'warn'))) return false;
    if ($('#lonly').checked && re) { re.lastIndex = 0; if (!re.test(l.l)) return false; }
    return true;
  };
  const lineEl = l => {
    const div = document.createElement('div');
    div.className = 'll' + (l.lv ? ' lv-' + l.lv : '');
    if ($('#lts').checked && l.t) {
      const ts = document.createElement('span'); ts.className = 'ts';
      ts.textContent = l.t.slice(11, 23).replace('Z', ''); ts.title = l.t; div.appendChild(ts);
    }
    const src = document.createElement('span'); src.className = 'src';
    const dot = document.createElement('i'); dot.style.background = L.colors.get(l.p) || 'var(--muted)';
    src.appendChild(dot);
    src.appendChild(document.createTextNode(shortPod(l.p) + (L.multiCt ? '/' + l.c : '')));
    src.title = l.p + '/' + l.c;
    div.appendChild(src);
    const msg = document.createElement('span'); msg.className = 'msg';
    if (re) {
      re.lastIndex = 0; let last = 0, m;
      while ((m = re.exec(l.l)) && m[0]) {
        msg.appendChild(document.createTextNode(l.l.slice(last, m.index)));
        const mk = document.createElement('mark'); mk.textContent = m[0]; msg.appendChild(mk);
        last = m.index + m[0].length;
      }
      msg.appendChild(document.createTextNode(l.l.slice(last)));
    } else msg.textContent = l.l;
    div.appendChild(msg);
    return div;
  };
  const shortPod = p => { const parts = p.split('-'); return parts.length > 2 ? '…' + parts.slice(-2).join('-') : p; };
  const atBottom = () => view.scrollHeight - view.scrollTop - view.clientHeight < 40;
  const rerender = () => {
    compileSearch();
    const frag = document.createDocumentFragment();
    L.lines.filter(visible).slice(-MAX_LINES).forEach(l => frag.appendChild(lineEl(l)));
    view.replaceChildren(frag);
    view.scrollTop = view.scrollHeight;
    updateCount();
  };
  const sys = msg => {
    const div = document.createElement('div'); div.className = 'll sys'; div.textContent = msg;
    view.appendChild(div);
  };
  const append = batch => {
    const stick = atBottom();
    batch.forEach(l => { l.lv = levelOf(l.l); });
    L.lines.push(...batch);
    let drop = L.lines.length - MAX_LINES;
    if (drop > 0) L.lines.splice(0, drop);
    const frag = document.createDocumentFragment();
    batch.filter(visible).forEach(l => frag.appendChild(lineEl(l)));
    view.appendChild(frag);
    while (view.childElementCount > MAX_LINES) view.firstElementChild.remove();
    if (stick) view.scrollTop = view.scrollHeight;
    updateCount();
  };
  const updateCount = () => {
    $('#lcount').textContent = `${L.lines.length.toLocaleString()} lines${L.paused ? ` · ${L.buffer.length} buffered` : ''}`;
  };
  const renderPods = () => {
    L.multiCt = L.pods.some(p => p.containers.length > 1);
    L.pods.forEach(p => {
      if (!L.colors.has(p.name)) {
        const i = L.colors.size;
        L.colors.set(p.name, i < POD_SLOTS.length ? css(POD_SLOTS[i]) : css('--muted'));
      }
    });
    $('#lpods').innerHTML = `<span class="muted">${L.pods.length} pod${L.pods.length === 1 ? '' : 's'}:</span>` + L.pods.map(p => `<span class="podchip ${L.hidden.has(p.name) ? 'off' : ''}" data-p="${esc(p.name)}" title="${esc(p.name)} on ${esc(p.node || '?')} — click to hide/show">
      <i style="background:${L.colors.get(p.name)}"></i>${esc(shortPod(p.name))}<span class="stt">${esc(p.status)}${p.restarts ? ' · ↻' + p.restarts : ''}</span></span>`).join('');
    $$('.podchip', $('#lpods')).forEach(el => el.addEventListener('click', () => {
      const n = el.dataset.p; L.hidden.has(n) ? L.hidden.delete(n) : L.hidden.add(n); renderPods(); rerender();
    }));
    const cts = [...new Set(L.pods.flatMap(p => p.containers))];
    const cur = $('#lct').value;
    if (!cur) $('#lct').innerHTML = '<option value="">All containers</option>' + cts.map(c => `<option>${esc(c)}</option>`).join('');
  };

  // ---- streaming (fetch + manual SSE parsing so errors are readable and nothing auto-reconnects)
  const stop = () => { L.ctrl?.abort(); L.ctrl = null; L.live = false; };
  const start = async () => {
    stop();
    if (!ready()) return;
    const p = params();
    history.replaceState(null, '', `${ctxPath(ctx)}/logs?` + new URLSearchParams([...p].filter(([k]) => !['ctx'].includes(k))));
    $('#ldl').href = apiURL('logs/download?') + p;
    L.lines = []; L.buffer = []; L.pods = []; L.hidden.clear(); L.colors.clear();
    view.innerHTML = ''; $('#lpods').innerHTML = ''; sys('Connecting…');
    $('#lstate').className = ''; $('#lstate').textContent = 'connecting…';
    const ctrl = new AbortController(); L.ctrl = ctrl;
    S.abort.signal.addEventListener('abort', () => ctrl.abort());
    let resp;
    try { resp = await fetch(apiURL('logs?') + p, { signal: ctrl.signal }); }
    catch (e) { if (e.name !== 'AbortError') { view.innerHTML = ''; sys('Connection failed: ' + e.message); } return; }
    if (!resp.ok) {
      const d = await resp.json().catch(() => ({}));
      view.innerHTML = ''; sys('Error: ' + (d.error || resp.statusText)); $('#lstate').textContent = 'error'; return;
    }
    view.innerHTML = '';
    const reader = resp.body.getReader(), dec = new TextDecoder();
    let buf = '';
    L.live = true;
    $('#lstate').className = p.get('follow') === '1' ? 'live' : '';
    $('#lstate').textContent = p.get('follow') === '1' ? 'live' : 'loading…';
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, i); buf = buf.slice(i + 2);
          let ev = 'message', data = '';
          chunk.split('\n').forEach(line => {
            if (line.startsWith('event: ')) ev = line.slice(7);
            else if (line.startsWith('data: ')) data += line.slice(6);
          });
          if (!data) continue;
          const d = JSON.parse(data);
          if (ev === 'meta') { L.pods = d.pods; renderPods(); $('#lselshow').textContent = `${d.namespace} · ${d.selector}`; if (!d.pods.length) sys('No pods match this selector right now' + (p.get('follow') === '1' ? ' — waiting for new ones…' : '.')); }
          else if (ev === 'lines') { if (L.paused) { L.buffer.push(...d); updateCount(); } else append(d); }
          else if (ev === 'warn') sys('⚠ ' + d.message);
          else if (ev === 'ended') sys(`■ ${d.pod}/${d.container}: ${d.message}`);
          else if (ev === 'done') { $('#lstate').textContent = 'complete'; }
        }
      }
    } catch (e) { if (e.name === 'AbortError') return; sys('Stream interrupted: ' + e.message); }
    if (L.ctrl === ctrl) { $('#lstate').className = ''; if ($('#lstate').textContent !== 'complete') $('#lstate').textContent = 'stopped'; L.live = false; }
  };

  // ---- controls
  $('#lns').addEventListener('change', async () => { await fillWorkloads(); start(); });
  $('#lwl').addEventListener('change', () => { podOnly = ''; $('#podOnly')?.remove(); $('#lct').innerHTML = '<option value="">All containers</option>'; start(); });
  $('#selMode').addEventListener('click', () => {
    const on = $('#lsel').hidden; $('#lsel').hidden = !on; $('#lwl').hidden = on; $('#selMode').classList.toggle('on', on);
    if (on) $('#lsel').focus(); else start();
  });
  $('#lsel').addEventListener('keydown', e => { if (e.key === 'Enter') start(); });
  ['#lct', '#ltail', '#lsince', '#lfollow', '#lprev'].forEach(s => $(s).addEventListener('change', start));
  let st; $('#lq').addEventListener('input', () => { clearTimeout(st); st = setTimeout(rerender, 200); });
  ['#lonly', '#llevel', '#lts'].forEach(s => $(s).addEventListener('change', rerender));
  $('#lwrap').addEventListener('change', () => view.classList.toggle('wrap', $('#lwrap').checked));
  $('#lpause').addEventListener('click', () => {
    L.paused = !L.paused; $('#lpause').textContent = L.paused ? 'Resume' : 'Pause'; $('#lpause').classList.toggle('on', L.paused);
    if (!L.paused && L.buffer.length) { const b = L.buffer; L.buffer = []; append(b); }
    updateCount();
  });
  $('#lclear').addEventListener('click', () => { L.lines = []; view.innerHTML = ''; updateCount(); });
  if (q.selector) { $('#lsel').hidden = false; $('#lwl').hidden = true; }
  if (ready()) start();
}


// ------------------------------------------------------------------ NAMESPACES: only what needs attention
async function viewNamespaces(route) {
  const ctx = route.ctx;
  const sum = (await api('clusters')).find(c => c.name === ctx);
  main.innerHTML = clusterHead(sum, 'Namespace health', `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/nsview">Namespaces</a></div>`) + `
    <div class="filters"><label class="chk"><input type="checkbox" id="nsHealthy"> Show healthy namespaces</label>
      <span class="muted" id="nsScan"></span>
      <a class="btn ghost" href="${listHref(ctx, '', 'namespaces')}" style="margin-left:auto">All namespace objects</a></div>
    <div id="nsBox"><div class="muted">Loading…</div></div>`;
  let data = null, scanning = false, lastScan = 0, showHealthy = false;
  bindRefresh(ctx, () => scan(true));
  $('#nsHealthy').addEventListener('change', e => { showHealthy = e.target.checked; draw(); });

  const itemHref = (ns, it) => it.kind === 'pod' ? `${ctxPath(ctx)}/pod/${enc(ns)}/${enc(it.name)}`
    : it.kind === 'workload' ? `${ctxPath(ctx)}/wl/${enc(it.ref)}/${enc(ns)}/${enc(it.name)}`
    : it.kind === 'pvc' ? objHref(ctx, '', 'persistentvolumeclaims', ns, it.name)
    : `${ctxPath(ctx)}/events`;
  const draw = () => {
    if (!data) return;
    const all = data.namespaces.filter(n => !S.ns || n.name === S.ns);
    const bad = all.filter(n => n.sev !== 'ok'), ok = all.filter(n => n.sev === 'ok');
    const crit = all.reduce((a, n) => a + n.items.filter(i => i.sev === 'critical').length, 0);
    const warn = all.reduce((a, n) => a + n.items.filter(i => i.sev === 'warning').length, 0);
    const stor = all.reduce((a, n) => a + n.items.filter(i => i.kind === 'pvc').length, 0);
    const tile = (label, val, foot, cls) => `<div class="card tile"><div class="label">${label}</div><div class="value ${cls || ''}">${val}</div><div class="foot">${foot}</div></div>`;
    $('#nsBox').innerHTML = `
      <div class="tiles" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        ${tile('Need attention', bad.length, `of ${all.length} namespaces`, bad.length ? 'warn' : '')}
        ${tile('Critical', crit, 'failing pods, workloads or volumes', crit ? 'crit' : '')}
        ${tile('Warnings', warn, 'restarts, rollouts, events (last hour)', warn ? 'warn' : '')}
        ${tile('Storage', stor, `volume claims ≥ ${data.warnPct}% full or unhealthy`, stor ? 'warn' : '')}
      </div>
      ${bad.length ? bad.map(n => `<div class="card nscard ${n.sev}"><div class="card-head">
          <h3><a href="${ctxPath(ctx)}/nsview/${enc(n.name)}" title="Open the namespace overview">${esc(n.name)}</a></h3>
          ${stLabel(n.sev === 'critical' ? 'Critical' : 'Warning', n.sev)}
          <span class="cap">${n.running}/${n.pods} pods running · ${n.restarts} restarts · ${n.pvcs} volume claim${n.pvcs === 1 ? '' : 's'}</span></div>
          <ul class="attn">${n.items.map(it => `<li class="${it.sev}"><span class="ai"><i class="sdot"></i></span>
            <a href="${itemHref(n.name, it)}">${esc(it.name)}</a><span class="muted">${esc(it.detail)}</span></li>`).join('')}</ul></div>`).join('')
        : `<div class="card empty-ok"><b>Nothing needs attention.</b><span class="muted"> Every pod, workload and volume checked looks healthy.</span></div>`}
      ${ok.length ? (showHealthy
        ? `<div class="card" style="margin-top:12px"><h3>Healthy namespaces</h3><div class="chips" style="margin-top:8px">${ok.map(n => `<a class="chip" href="${ctxPath(ctx)}/nsview/${enc(n.name)}">${esc(n.name)} <span class="muted">${n.running}/${n.pods}</span></a>`).join('')}</div></div>`
        : `<div class="muted" style="margin-top:10px">${ok.length} healthy namespace${ok.length === 1 ? '' : 's'} hidden.</div>`) : ''}`;
    $$('[data-setns]', $('#nsBox')).forEach(a => a.addEventListener('click', e => {
      e.preventDefault();
      S.ns = a.dataset.setns; try { localStorage.setItem('kd.ns.' + ctx, S.ns); } catch {}
      renderTop(parseRoute()); location.hash = `${ctxPath(ctx)}/pods`;
    }));
    const up = $('#updated'); if (up) up.textContent = 'updated ' + new Date().toLocaleTimeString();
  };
  const load = async () => { data = await api(`nsoverview?ctx=${enc(ctx)}`); draw(); };

  // storage: one df per pod that mounts claims; the server caches readings for 5 minutes,
  // and namespaces are measured one at a time
  const scan = async force => {
    if (scanning || !data || !data.exec) return;
    const todo = data.namespaces.filter(n => n.pvcs > 0 && (!S.ns || n.name === S.ns));
    if (!todo.length) return;
    if (!force && Date.now() - lastScan < 5 * 60 * 1000 && todo.every(n => !n.pvcUnchecked)) return;
    scanning = true; lastScan = Date.now();
    try {
      for (let i = 0; i < todo.length; i++) {
        if (parseRoute().section !== 'namespaces') return;
        const el = $('#nsScan'); if (el) el.textContent = `Checking storage ${i + 1}/${todo.length}…`;
        try { await loadPvcUsage(ctx, todo[i].name, { refresh: !!force }); } catch {}
        await load();
      }
    } finally { scanning = false; const el = $('#nsScan'); if (el) el.textContent = ''; }
  };
  await live(20000, async () => { await load(); scan(false); });
}

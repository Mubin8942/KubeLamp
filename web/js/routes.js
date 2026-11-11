'use strict';
/* Routing: Traefik IngressRoutes with Gateway API HTTPRoutes shown beneath them,
   plus plain Ingresses — hosts → matches → backends in one place. */

const ROUTE_KINDS = [
  { key: 'ingressRoutes', title: 'IngressRoutes', sub: 'Traefik', has: 'ingressRoutes' },
  { key: 'httpRoutes', title: 'HTTPRoutes', sub: 'Gateway API', has: 'httpRoutes', child: true },
  { key: 'ingresses', title: 'Ingresses', sub: 'networking.k8s.io', has: 'ingresses' },
];

function routeBackendHtml(ctx, ns, b) {
  const bns = b.namespace || ns;
  const label = `${b.kind && b.kind !== 'Service' ? `<span class="muted">${esc(b.kind)}</span> ` : ''}${esc(b.namespace && b.namespace !== ns ? b.namespace + '/' : '')}${esc(b.name)}${b.port ? `<span class="muted">:${esc(b.port)}</span>` : ''}`;
  const w = b.weight != null ? ` <span class="rt-w" title="weight">${esc(b.weight)}</span>` : '';
  if (b.kind === 'Service' || !b.kind) return `<a class="rt-be" href="${objHref(ctx, '', 'services', bns, b.name)}">${label}</a>${w}`;
  return `<span class="rt-be">${label}</span>${w}`;
}

function routeRulesHtml(ctx, r) {
  if (!r.rules.length) return '<span class="muted">no rules</span>';
  return `<div class="rt-rules">${r.rules.map(rl => `<div class="rt-rule">
      <code class="rt-match" title="${esc(rl.match)}">${esc(rl.match)}</code>
      <svg class="rt-arrow" viewBox="0 0 24 24"><path d="M5 12h13M13 7l5 5-5 5"/></svg>
      <span class="rt-bes">${rl.backends.map(b => routeBackendHtml(ctx, r.namespace, b)).join('') || '<span class="muted">no backend</span>'}</span>
      ${rl.middlewares?.length ? `<span class="rt-mw" title="${esc(rl.middlewares.join(', '))}">${rl.middlewares.length} ${r.kind === 'HTTPRoute' ? 'filter' : 'middleware'}${rl.middlewares.length === 1 ? '' : 's'}</span>` : ''}
    </div>`).join('')}</div>`;
}

const routeHosts = r => r.hosts.length ? r.hosts.map(h => `<span class="chip">${esc(h)}</span>`).join('') : '<span class="muted">any host</span>';
const routeStatus = r => r.status ? `<i class="dot ${r.status === 'good' ? 'ok' : r.status === 'critical' ? 'error' : 'warn'}" title="${esc(r.statusMsg || '')}"></i>` : '';

function routesTable(ctx, id, list, showNs, redraw) {
  const cols = [
    { key: 'name', label: 'Name', cls: 'name', render: r => `${routeStatus(r)}<a href="${objHref(ctx, r.group, r.resource, r.namespace, r.name)}">${esc(r.name)}</a>` },
  ];
  if (showNs) cols.push({ key: 'namespace', label: 'Namespace' });
  cols.push(
    { key: 'hosts', label: 'Hosts', sort: r => r.hosts.join(','), render: r => `<div class="chips">${routeHosts(r)}</div>` },
    { key: 'entry', label: 'Entry', sort: r => r.entry.join(','), render: r => r.entry.map(e => `<span class="chip">${esc(e)}</span>`).join(' ') || '<span class="muted">—</span>' },
    { key: 'rules', label: 'Routes → backends', nosort: true, cls: 'rt-cell', render: r => routeRulesHtml(ctx, r) },
    { key: 'tls', label: 'TLS', render: r => r.tls ? `<span class="rt-tls" title="${esc(r.tls)}">${ICON_LOCK}</span>` : '<span class="muted">—</span>' },
    { key: 'age', label: 'Age', sort: r => -new Date(r.created), render: r => `<span class="muted">${age(r.created)}</span>` },
  );
  const t = table(id, cols, list, { sort: { key: 'name', dir: 'asc' } });
  return { html: t.html, bind: el => t.bind(el, redraw) };
}
const ICON_LOCK = '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><rect x="5" y="10.5" width="14" height="9.5" rx="2"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/></svg>';

function filterRoutes(list, q) {
  if (!q) return list;
  return list.filter(r => [r.name, r.namespace, ...r.hosts, ...r.entry, ...r.rules.flatMap(x => [x.match, ...x.backends.map(b => b.name)])].join(' ').toLowerCase().includes(q));
}

// the Routing page: IngressRoutes, then HTTPRoutes beneath them, then Ingresses
async function viewRoutes(route) {
  const ctx = route.ctx;
  main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / Network</div>
      <h1>Routing</h1><div class="sub"><span>${S.ns ? `Namespace ${esc(S.ns)}` : 'All namespaces'}</span><span class="muted">How traffic reaches your services: host → match → backend</span></div></div></div>
    <div class="filters"><input type="search" id="rtq" placeholder="Filter by name, host, path or service" style="width:340px"><span class="count" id="rtcount"></span></div>
    <div id="rtBox"><div class="card muted">Loading…</div></div>`;
  let data = null;
  const draw = () => {
    if (!data) return;
    const q = $('#rtq').value.toLowerCase().trim();
    let shown = 0, total = 0, html = '';
    const tables = [];
    ROUTE_KINDS.forEach(k => {
      if (!data.has[k.has]) return;
      const all = data[k.key], list = filterRoutes(all, q);
      shown += list.length; total += all.length;
      const t = routesTable(ctx, 'routes-' + k.key, list, !S.ns, draw);
      tables.push(t);
      html += `<section class="card flush rt-sec ${k.child && data.has.ingressRoutes ? 'rt-child' : ''}">
        <div class="card-head rt-head">${k.child && data.has.ingressRoutes ? '<span class="rt-tree" aria-hidden="true">↳</span>' : ''}<h3>${esc(k.title)}</h3><span class="cap">${esc(k.sub)} · ${list.length}${q ? ' of ' + all.length : ''}</span></div>
        <div data-rt="${tables.length - 1}">${t.html}</div></section>`;
    });
    if (!html) html = `<div class="empty-state"><h2>No routing APIs on this cluster</h2><p>KubeLamp looks for Traefik IngressRoutes (<code>traefik.io</code>), Gateway API HTTPRoutes and Ingresses.</p></div>`;
    if (data.warnings?.length) html = `<div class="banner"><b>Partly visible.</b><span>${esc(data.warnings.join('; '))}</span></div>` + html;
    $('#rtBox').innerHTML = html;
    tables.forEach((t, i) => t.bind($(`[data-rt="${i}"]`, $('#rtBox'))));
    $('#rtcount').textContent = `${shown} of ${total} routes`;
  };
  $('#rtq').addEventListener('input', draw);
  if (route.q.q) $('#rtq').value = route.q.q;
  await live(20000, async () => { data = await api(`routes?ctx=${enc(ctx)}&ns=${enc(S.ns)}`); draw(); });
}

// HTTPRoutes card under the IngressRoutes list page
async function httpRoutesUnder(ctx, box) {
  try {
    const data = await api(`routes?ctx=${enc(ctx)}&ns=${enc(S.ns)}`);
    if (!data.has.httpRoutes || !box.isConnected) return;
    const draw = () => {
      const t = routesTable(ctx, 'routes-under', data.httpRoutes, !S.ns, draw);
      box.innerHTML = `<section class="card flush rt-sec rt-child mt"><div class="card-head rt-head"><span class="rt-tree" aria-hidden="true">↳</span><h3>HTTPRoutes</h3>
        <span class="cap">Gateway API · ${data.httpRoutes.length}</span><div class="right"><a class="btn sm ghost" href="${ctxPath(ctx)}/routes">Routing overview</a></div></div>
        <div class="rt-tb">${t.html}</div></section>`;
      t.bind($('.rt-tb', box));
    };
    draw();
  } catch { /* the list above already reports access problems */ }
}

// detail page card: the normalised routing table for one route object
async function routeDetailCard(ctx, kind, ns, name) {
  const box = $('#rtDetail');
  if (!box) return;
  try {
    const data = await api(`routes?ctx=${enc(ctx)}&ns=${enc(ns)}`);
    const list = kind === 'IngressRoute' ? data.ingressRoutes : kind === 'HTTPRoute' ? data.httpRoutes : data.ingresses;
    const r = list.find(x => x.name === name && x.namespace === ns);
    if (!r || !box.isConnected) { box.remove(); return; }
    box.innerHTML = `<div class="card-head"><h3>Routing</h3>${r.statusMsg ? `<span class="cap">${routeStatus(r)} ${esc(r.statusMsg)}</span>` : ''}</div>
      <div class="kvgrid" style="margin:8px 0 12px"><span>Hosts</span><span class="chips">${routeHosts(r)}</span>
        <span>${kind === 'HTTPRoute' ? 'Gateways' : kind === 'Ingress' ? 'Class' : 'Entry points'}</span><span class="chips">${r.entry.map(e => `<span class="chip">${esc(e)}</span>`).join('') || '<span class="muted">default</span>'}</span>
        <span>TLS</span><span>${esc(r.tls || 'none')}</span></div>
      ${routeRulesHtml(ctx, r)}`;
  } catch { box.remove(); }
}

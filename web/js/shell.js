'use strict';
/* App shell: router, sidebar navigation (built from API discovery), cluster and
   namespace pickers, stop-server, boot. */

S.disc = {};
S.info = null;
S.ns = '';

const shortName = n => n.startsWith('arn:aws:eks:') ? n.split('/').pop() + ' (EKS)' : n;
async function loadClusters() {
  try { S.clusters = await api('clusters'); } catch { /* keep last */ }
}
async function ensureDiscovery(ctx) {
  if (!S.disc[ctx]) {
    try { S.disc[ctx] = await api('resources?ctx=' + enc(ctx)); }
    catch { S.disc[ctx] = { resources: [] }; }
  }
  return S.disc[ctx];
}

// ------------------------------------------------------------------ namespace filter
function loadNs(ctx) {
  try { S.ns = localStorage.getItem('kd.ns.' + ctx) || ''; } catch { S.ns = ''; }
}
function setNs(ctx, ns) {
  S.ns = ns;
  try { localStorage.setItem('kd.ns.' + ctx, ns); } catch {}
  const r = parseRoute();
  renderTop(r);
  if (r.section === 'nsview' && ns) { location.hash = `${ctxPath(ctx)}/nsview/${enc(ns)}`; return; }
  navigate();
}

// ------------------------------------------------------------------ navigation model
// [label, target] — target is a dashboard section, or [group, resource] for any API type
const NAV = [
  ['Cluster', [['Overview', 'overview'], ['Nodes', 'nodes'], ['Events', 'events']]],
  ['Namespace', [['Overview', 'nsview'], ['Health', 'namespaces']]],
  ['Workloads', [['Deployments', 'wl:Deployment'], ['StatefulSets', 'wl:StatefulSet'], ['DaemonSets', 'wl:DaemonSet'], ['Pods', 'pods'],
    ['Jobs', ['batch', 'jobs']], ['CronJobs', ['batch', 'cronjobs']], ['ReplicaSets', ['apps', 'replicasets']],
    ['Autoscalers', ['autoscaling', 'horizontalpodautoscalers']], ['Pod disruption budgets', ['policy', 'poddisruptionbudgets']], ['Deployment logs', 'logs']]],
  ['Network', [['Routing', 'routes'], ['Services', ['', 'services']], ['Ingresses', ['networking.k8s.io', 'ingresses']],
    ['IngressRoutes', [['traefik.io', 'ingressroutes'], ['traefik.containo.us', 'ingressroutes']]],
    ['HTTPRoutes', ['gateway.networking.k8s.io', 'httproutes'], 'sub'], ['GRPCRoutes', ['gateway.networking.k8s.io', 'grpcroutes'], 'sub'],
    ['Ingress classes', ['networking.k8s.io', 'ingressclasses']],
    ['Gateways', ['gateway.networking.k8s.io', 'gateways']], ['Gateway classes', ['gateway.networking.k8s.io', 'gatewayclasses']],
    ['Network policies', ['networking.k8s.io', 'networkpolicies']], ['Cilium policies', ['cilium.io', 'ciliumnetworkpolicies']],
    ['Cilium cluster policies', ['cilium.io', 'ciliumclusterwidenetworkpolicies']], ['Endpoint slices', ['discovery.k8s.io', 'endpointslices']]]],
  ['Config', [['ConfigMaps', ['', 'configmaps']], ['Secrets', ['', 'secrets']], ['ExternalSecrets', ['external-secrets.io', 'externalsecrets']],
    ['SecretStores', ['external-secrets.io', 'secretstores']], ['ClusterSecretStores', ['external-secrets.io', 'clustersecretstores']],
    ['Resource quotas', ['', 'resourcequotas']], ['Limit ranges', ['', 'limitranges']]]],
  ['Storage', [['Volume claims', ['', 'persistentvolumeclaims']], ['Volumes', ['', 'persistentvolumes']], ['Storage classes', ['storage.k8s.io', 'storageclasses']]]],
  ['Access', [['Service accounts', ['', 'serviceaccounts']], ['Roles', ['rbac.authorization.k8s.io', 'roles']], ['Role bindings', ['rbac.authorization.k8s.io', 'rolebindings']],
    ['Cluster roles', ['rbac.authorization.k8s.io', 'clusterroles']], ['Cluster role bindings', ['rbac.authorization.k8s.io', 'clusterrolebindings']]]],
];
const BUILTIN_GROUPS = new Set(['', 'apps', 'batch', 'autoscaling', 'policy', 'networking.k8s.io', 'rbac.authorization.k8s.io', 'storage.k8s.io',
  'coordination.k8s.io', 'discovery.k8s.io', 'events.k8s.io', 'flowcontrol.apiserver.k8s.io', 'admissionregistration.k8s.io', 'apiextensions.k8s.io',
  'apiregistration.k8s.io', 'certificates.k8s.io', 'node.k8s.io', 'scheduling.k8s.io', 'metrics.k8s.io', 'authentication.k8s.io',
  'authorization.k8s.io', 'resource.k8s.io', 'storagemigration.k8s.io', 'internal.apiserver.k8s.io']);

function navClosed(label) { try { return localStorage.getItem('kd.nav.' + label) === '0'; } catch { return false; } }
function setNavClosed(label, closed) { try { localStorage.setItem('kd.nav.' + label, closed ? '0' : '1'); } catch {} }

const NAV_ICON = {
  'All clusters': '<rect x="4" y="4" width="7" height="7" rx="1.5"/><rect x="13" y="4" width="7" height="7" rx="1.5"/><rect x="4" y="13" width="7" height="7" rx="1.5"/><rect x="13" y="13" width="7" height="7" rx="1.5"/>',
  Cluster: '<rect x="3.5" y="4" width="17" height="7" rx="2"/><rect x="3.5" y="13" width="17" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>',
  Namespace: '<rect x="3.5" y="5" width="17" height="14" rx="2.5"/><path d="M3.5 9.5h17"/>',
  Workloads: '<path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z"/><path d="M12 12l8-4.5M12 12v9M12 12L4 7.5"/>',
  Network: '<circle cx="12" cy="5.5" r="2"/><circle cx="5.5" cy="18.5" r="2"/><circle cx="18.5" cy="18.5" r="2"/><path d="M12 7.5V12m0 0l-5 5m5-5l5 5"/>',
  Config: '<path d="M4 7h9M17 7h3M4 12h3M11 12h9M4 17h11M19 17h1"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="12" r="2"/><circle cx="17" cy="17" r="2"/>',
  Storage: '<ellipse cx="12" cy="6" rx="7" ry="2.5"/><path d="M5 6v12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5V6M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5"/>',
  Access: '<path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6z"/><path d="M9.5 12l2 2 3.5-3.5"/>',
  'Custom resources': '<rect x="4" y="4" width="16" height="16" rx="3"/><path d="M12 8v8M8 12h8"/>',
};
const railIcon = label => `<svg viewBox="0 0 24 24">${NAV_ICON[label] || NAV_ICON['Custom resources']}</svg>`;

// the navigation as data: [{ label, items: [{ label, href, on, sub }] }], shared by the full sidebar and the rail
function navModel(route) {
  const ctx = route.ctx;
  const disc = S.disc[ctx];
  const has = (g, r) => !disc || disc.resources.some(x => x.group === g && x.resource === r);
  const cur = route.section === 'r' ? `r:${gv(route.args[0])}/${route.args[1]}` : route.section === 'workloads' ? 'wl:' + (route.q.kind || '') : route.section;
  const item = (label, target, sub) => {
    let href, key;
    if (Array.isArray(target) && Array.isArray(target[0])) target = target.find(t => has(t[0], t[1])) || target[0]; // alternative API groups
    if (Array.isArray(target)) {
      if (!has(target[0], target[1])) return null;
      href = listHref(ctx, target[0], target[1]); key = `r:${target[0]}/${target[1]}`;
    } else if (target.startsWith('wl:')) {
      href = `${ctxPath(ctx)}/workloads?kind=${target.slice(3)}`; key = target;
    } else { href = `${ctxPath(ctx)}/${target}`; key = target; }
    const on = key === cur || (route.section === 'wl' && key === 'wl:' + route.args[0]) || (route.section === 'node' && key === 'nodes') || (route.section === 'pod' && key === 'pods')
      || (route.section === 'o' && key === `r:${gv(route.args[0])}/${route.args[1]}`);
    return { label, href, on, sub: !!sub };
  };
  const groups = NAV.map(([label, items]) => ({ label, items: items.map(([l, t, sub]) => item(l, t, sub)).filter(Boolean) }));
  // HTTPRoutes/GRPCRoutes nest under IngressRoutes only when Traefik is installed
  groups.forEach(g => g.items.forEach((it, i) => { if (it.sub && !g.items.slice(0, i).some(p => p.label === 'IngressRoutes')) it.sub = false; }));
  // custom resources: everything else the cluster serves, grouped by API group
  if (disc) {
    const known = new Set(NAV.flatMap(([, items]) => items.filter(([, t]) => Array.isArray(t)).flatMap(([, t]) => Array.isArray(t[0]) ? t.map(x => x.join('/')) : [t.join('/')])));
    const crds = disc.resources.filter(r => !BUILTIN_GROUPS.has(r.group) && !known.has(r.group + '/' + r.resource));
    const byGroup = {};
    crds.forEach(r => (byGroup[r.group] = byGroup[r.group] || []).push(r));
    const items = [];
    Object.keys(byGroup).sort().forEach(g => {
      items.push({ head: g });
      byGroup[g].forEach(r => { const it = item(plural(r), [r.group, r.resource]); if (it) items.push(it); });
    });
    groups.push({ label: 'Custom resources', items });
  }
  return groups.filter(g => g.items.some(i => !i.head));
}

function renderNav(route) {
  const nav = $('#nav'), rail = $('#rail');
  if (!route.ctx) {
    nav.innerHTML = `<a class="nav-a ${route.page ? '' : 'on'}" href="#/">All clusters</a><div class="nav-h" style="cursor:default">Clusters</div>` +
      (S.clusters.map(c => `<a class="nav-a" href="${ctxPath(c.name)}/overview" title="${esc(c.name)}"><i class="dot ${c.status}"></i><span class="n">${esc(shortName(c.name))}</span>${c.scope === 'namespaced' ? '<span class="tag" title="Role/RoleBinding access">ns</span>' : ''}</a>`).join('')
        || '<div class="muted" style="padding:6px 10px">No contexts loaded</div>');
    rail.innerHTML = `<a class="rail-b ${route.page ? '' : 'on'}" href="#/" title="All clusters">${railIcon('All clusters')}</a><div class="rail-sep"></div>` +
      S.clusters.map(c => `<a class="rail-b" href="${ctxPath(c.name)}/overview" title="${esc(shortName(c.name))}"><b style="font-size:12px;font-weight:600">${esc(shortName(c.name).replace(/[^A-Za-z0-9]/g, '').slice(0, 2).toUpperCase())}</b><i class="dot ${c.status}"></i></a>`).join('');
    return;
  }
  const groups = navModel(route);
  const group = (label, body) => {
    const closed = navClosed(label);
    return `<button class="nav-h ${closed ? 'closed' : ''}" data-g="${esc(label)}"><svg viewBox="0 0 24 24"><path d="M7 10l5 5 5-5"/></svg>${esc(label)}</button><div class="nav-sub" ${closed ? 'hidden' : ''}>${body}</div>`;
  };
  let html = `<a class="nav-a nav-back" href="#/"><svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 6l-6 6 6 6"/></svg>All clusters</a>`;
  groups.forEach(g => {
    html += group(g.label, g.items.map(i => i.head ? `<div class="muted" style="font-size:11.5px;padding:8px 10px 2px" title="${esc(i.head)}">${esc(i.head)}</div>`
      : `<a class="nav-a ${i.on ? 'on' : ''} ${i.sub ? 'sub' : ''}" href="${i.href}">${i.sub ? '<span class="nav-tree" aria-hidden="true"></span>' : ''}<span class="n">${esc(i.label)}</span></a>`).join(''));
  });
  nav.innerHTML = html;
  $$('.nav-h', nav).forEach(h => h.addEventListener('click', () => {
    const closed = !h.classList.contains('closed');
    h.classList.toggle('closed', closed);
    h.nextElementSibling.hidden = closed;
    setNavClosed(h.dataset.g, closed);
  }));
  // collapsed rail: one icon per section, its pages in a flyout
  rail.innerHTML = `<a class="rail-b" href="#/" title="All clusters">${railIcon('All clusters')}</a><div class="rail-sep"></div>` +
    groups.map((g, gi) => `<button class="rail-b ${g.items.some(i => i.on) ? 'on' : ''}" type="button" data-gi="${gi}" title="${esc(g.label)}">${railIcon(g.label)}</button>`).join('');
  $$('.rail-b[data-gi]', rail).forEach(b => b.addEventListener('click', () => {
    const g = groups[+b.dataset.gi];
    openPop(b, g.items.filter(i => !i.head).map(i => ({ label: i.label, on: i.on, run: () => { location.hash = i.href; } })),
      { side: 'right', title: g.label, search: g.items.length > 14, placeholder: 'Filter ' + g.label.toLowerCase() });
  }));
}

// collapse / expand the sidebar; remembered in this browser
function setSideMin(min) {
  document.body.classList.toggle('side-min', min);
  try { localStorage.setItem('kd.side.min', min ? '1' : ''); } catch {}
  const t = $('#sideTog');
  t.title = min ? 'Expand sidebar' : 'Collapse sidebar';
  $('span', t).textContent = min ? 'Expand sidebar' : 'Collapse sidebar';
  $('#sideTogArrow').setAttribute('d', min ? 'M13.5 10l2 2-2 2' : 'M15.5 10l-2 2 2 2');
  closePop();
  requestAnimationFrame(() => { S.redraw && S.redraw(); window.dispatchEvent(new Event('resize')); });
}
$('#sideTog').addEventListener('click', () => setSideMin(!document.body.classList.contains('side-min')));
try { if (localStorage.getItem('kd.side.min') === '1') setSideMin(true); } catch {}

// ------------------------------------------------------------------ top bar
function renderTop(route) {
  const c = S.clusters.find(x => x.name === route.ctx);
  $('#ctxName').textContent = route.ctx ? shortName(route.ctx) : 'All clusters';
  $('#ctxDot').className = 'dot ' + (c ? c.status : route.ctx ? 'loading' : '');
  $('#ctxDot').hidden = !route.ctx;
  $('#nsPicker').hidden = !route.ctx;
  $('#nsName').textContent = S.ns || (c && c.scope === 'namespaced' ? 'My namespaces' : 'All namespaces');
}
$('#ctxPicker').addEventListener('click', () => {
  const route = parseRoute();
  const section = route.ctx && ['overview', 'nodes', 'workloads', 'pods', 'events', 'logs', 'namespaces', 'nsview'].includes(route.section) ? route.section : route.section === 'tools' ? 'tools/' + (route.args[0] || '') : 'overview';
  openPop($('#ctxPicker'), [{ label: 'All clusters', on: !route.ctx, run: () => { location.hash = '#/'; } }, 'sep',
    ...S.clusters.map(c => ({
      label: shortName(c.name), on: c.name === route.ctx, icon: `<i class="dot ${c.status}"></i>`,
      sub: c.status === 'error' ? 'unreachable' : c.scope === 'namespaced' ? 'namespaces' : (c.version || ''),
      run: () => { location.hash = `${ctxPath(c.name)}/${section}`; },
    }))], { search: S.clusters.length > 6, placeholder: 'Find a cluster' });
});
$('#nsPicker').addEventListener('click', async () => {
  const route = parseRoute();
  if (!route.ctx) return;
  let nss = [];
  try { nss = await api('namespaces?ctx=' + enc(route.ctx)); } catch {}
  openPop($('#nsPicker'), [{ label: 'All namespaces', on: !S.ns, run: () => setNs(route.ctx, '') }, 'sep',
    ...nss.map(n => ({ label: n.name, on: n.name === S.ns, sub: n.pods ? `${n.pods} pods` : '', run: () => setNs(route.ctx, n.name) }))],
  { search: true, placeholder: 'Find a namespace' });
});
$('#stopBtn').addEventListener('click', async () => {
  const ok = await modal({ title: 'Stop the KubeLamp server?', body: '<p>Dashboards, open terminals and log streams close. Open the KubeLamp app (or run <code>kubelamp</code>) to start it again.</p>',
    actions: [{ label: 'Cancel', value: false }, { label: 'Stop server', kind: 'danger solid', value: true, submit: true }] });
  if (!ok) return;
  S.timers.forEach(clearInterval); S.abort?.abort();
  await fetch('/api/quit', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).catch(() => {});
  document.body.innerHTML = `<div style="grid-column:1/-1;grid-row:1/-1;display:grid;place-items:center;text-align:center;color:var(--ink-2)">
    <div><h2 style="color:var(--ink);font-weight:600;margin:0 0 6px">KubeLamp has stopped</h2><p style="margin:0">Open the app again to restart it. You can close this tab.</p></div></div>`;
});

// ------------------------------------------------------------------ router
function parseRoute() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [path, qs] = raw.split('?');
  const p = path.split('/').map(decodeURIComponent);
  const q = Object.fromEntries(new URLSearchParams(qs || ''));
  if (p[0] === 'settings') return { page: 'settings', q };
  if (p[0] !== 'c' || !p[1]) return { q };
  return { ctx: p[1], section: p[2] || 'overview', args: p.slice(3), q };
}
let lastCtx = null;
async function navigate() {
  S.timers.forEach(clearInterval); S.timers = []; S.gen++; S.kick = null;
  S.abort?.abort(); S.abort = new AbortController();
  S.redraw = null; hideTip(); closePop();
  const route = parseRoute();
  if (route.ctx && route.ctx !== lastCtx) { loadNs(route.ctx); lastCtx = route.ctx; }
  renderTop(route);
  renderNav(route);
  $('#setBtn').classList.toggle('on', route.page === 'settings');
  if (route.ctx && !S.disc[route.ctx]) ensureDiscovery(route.ctx).then(() => { if (parseRoute().ctx === route.ctx) renderNav(parseRoute()); });
  const views = { overview: viewOverview, nodes: viewNodes, node: viewNode, workloads: viewWorkloads, wl: viewWorkload, pods: viewPods, namespaces: viewNamespaces, nsview: viewNsOverview, tools: viewTools,
    pod: viewPod, events: viewEvents, logs: viewLogs, r: viewResource, o: viewObject, routes: viewRoutes };
  main.scrollTop = 0;
  try {
    if (route.page === 'settings') viewSettings();
    else if (!route.ctx) await viewHome();
    else await (views[route.section] || viewOverview)(route);
  } catch (e) {
    if (e.name === 'AbortError') return;
    main.innerHTML = `<div class="banner err"><b>Can't load this view.</b><span>${esc(e.message)}</span></div>`;
  }
}
window.addEventListener('hashchange', navigate);

// poll helper: run fn now and every ms; first failure shows an error page, later ones a banner
function live(ms, fn) {
  let ok = false;
  const run = async () => {
    try { await fn(); ok = true; $('#liveErr')?.remove(); } catch (e) {
      if (e.name === 'AbortError') return;
      if (!ok) {
        const ctx = parseRoute().ctx;
        main.innerHTML = `<div class="empty-state"><h2>Can't reach ${esc(ctx ? shortName(ctx) : 'the server')}</h2><p>${esc(e.message)}</p>
          <p class="muted">${/forbidden|RBAC|nothing readable/i.test(e.message)
            ? 'Your credentials may only have Role/RoleBinding rights. Tell KubeLamp which namespaces you can read.'
            : /x509|certificate/i.test(e.message) ? 'The API server\'s TLS certificate could not be verified; see the certificate details below. KubeLamp retries automatically.'
            : `Check VPN or network access to the API server, and that <code>kubectl --context ${esc(ctx || '')} get pods</code> works. KubeLamp retries automatically.`}</p>
          ${ctx ? '<button class="btn primary" id="errAccess">Access settings</button>' : ''}</div><div id="errCerts"></div>`;
        $('#errAccess')?.addEventListener('click', () => openAccess(ctx));
        // an expired certificate is a common reason: show what KubeLamp could read about it
        if (ctx) api('clusters').then(list => {
          const c = (list || []).find(x => x.name === ctx), box = $('#errCerts');
          if (box && c && c.certs && c.certs.certs.length && (c.certs.worst !== 'ok' || /x509|certificate/i.test(e.message))) box.innerHTML = certBanner(c) + certCard(c);
        }).catch(() => {});
        return;
      }
      let b = $('#liveErr');
      if (!b) { main.insertAdjacentHTML('afterbegin', '<div class="banner err" id="liveErr"><b>Refresh failed.</b><span></span></div>'); b = $('#liveErr'); }
      $('span', b).textContent = e.message + ' Showing the last good data.';
    }
  };
  if (typeof ms !== 'function') { every(ms, run); return run(); }
  // dynamic interval (the Pods page lets you pick 1s…1min): a timeout chain that
  // waits for each refresh to finish, so slow clusters never pile up requests
  const gen = S.gen;
  let timer = null;
  const loop = () => {
    if (gen !== S.gen) return;
    timer = setTimeout(async () => {
      if (gen !== S.gen) return;
      if (!document.hidden) await run();
      loop();
    }, ms());
    S.timers.push(timer);
  };
  S.kick = async () => { clearTimeout(timer); await run(); loop(); }; // apply a new interval now
  const first = run();
  first.then(loop, loop);
  return first;
}

// ------------------------------------------------------------------ boot
(async () => {
  try {
    S.info = await api('info');
    $('#roBadge').hidden = !S.info.readOnly;
    if (!S.info.shell) $('#termNew').title = S.info.readOnly ? 'Shell disabled in read-only mode' : 'Shell only available on localhost';
  } catch {}
  await loadClusters();
  navigate();
  try { if (localStorage.getItem('kd.term.open') === '1') Term.show(true); } catch {}
  setInterval(async () => { await loadClusters(); const r = parseRoute(); renderTop(r); if (!r.ctx) renderNav(r); }, 20000);
})();

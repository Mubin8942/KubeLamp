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
  renderTop(parseRoute());
  navigate();
}

// ------------------------------------------------------------------ navigation model
// [label, target] — target is a dashboard section, or [group, resource] for any API type
const NAV = [
  ['Cluster', [['Overview', 'overview'], ['Nodes', 'nodes'], ['Namespaces', 'namespaces'], ['Events', 'events']]],
  ['Workloads', [['Deployments', 'wl:Deployment'], ['StatefulSets', 'wl:StatefulSet'], ['DaemonSets', 'wl:DaemonSet'], ['Pods', 'pods'],
    ['Jobs', ['batch', 'jobs']], ['CronJobs', ['batch', 'cronjobs']], ['ReplicaSets', ['apps', 'replicasets']],
    ['Autoscalers', ['autoscaling', 'horizontalpodautoscalers']], ['Pod disruption budgets', ['policy', 'poddisruptionbudgets']], ['Deployment logs', 'logs']]],
  ['Network', [['Services', ['', 'services']], ['Ingresses', ['networking.k8s.io', 'ingresses']], ['Ingress classes', ['networking.k8s.io', 'ingressclasses']],
    ['Gateways', ['gateway.networking.k8s.io', 'gateways']], ['Gateway classes', ['gateway.networking.k8s.io', 'gatewayclasses']],
    ['HTTPRoutes', ['gateway.networking.k8s.io', 'httproutes']], ['GRPCRoutes', ['gateway.networking.k8s.io', 'grpcroutes']],
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

function renderNav(route) {
  const nav = $('#nav');
  if (!route.ctx) {
    nav.innerHTML = `<a class="nav-a on" href="#/">All clusters</a><div class="nav-h" style="cursor:default">Clusters</div>` +
      (S.clusters.map(c => `<a class="nav-a" href="${ctxPath(c.name)}/overview" title="${esc(c.name)}"><i class="dot ${c.status}"></i><span class="n">${esc(shortName(c.name))}</span>${c.scope === 'namespaced' ? '<span class="tag" title="Role/RoleBinding access">ns</span>' : ''}</a>`).join('')
        || '<div class="muted" style="padding:6px 10px">No contexts loaded</div>');
    return;
  }
  const ctx = route.ctx;
  const disc = S.disc[ctx];
  const has = (g, r) => !disc || disc.resources.some(x => x.group === g && x.resource === r);
  const cur = route.section === 'r' ? `r:${gv(route.args[0])}/${route.args[1]}` : route.section === 'workloads' ? 'wl:' + (route.q.kind || '') : route.section;
  const item = (label, target) => {
    let href, key;
    if (Array.isArray(target)) {
      if (!has(target[0], target[1])) return '';
      href = listHref(ctx, target[0], target[1]); key = `r:${target[0]}/${target[1]}`;
    } else if (target.startsWith('wl:')) {
      href = `${ctxPath(ctx)}/workloads?kind=${target.slice(3)}`; key = target;
    } else { href = `${ctxPath(ctx)}/${target}`; key = target; }
    const on = key === cur || (route.section === 'wl' && key === 'wl:' + route.args[0]) || (route.section === 'node' && key === 'nodes') || (route.section === 'pod' && key === 'pods')
      || (route.section === 'o' && key === `r:${gv(route.args[0])}/${route.args[1]}`);
    return `<a class="nav-a ${on ? 'on' : ''}" href="${href}"><span class="n">${esc(label)}</span></a>`;
  };
  const group = (label, body) => {
    if (!body.trim()) return '';
    const closed = navClosed(label);
    return `<button class="nav-h ${closed ? 'closed' : ''}" data-g="${esc(label)}"><svg viewBox="0 0 24 24"><path d="M7 10l5 5 5-5"/></svg>${esc(label)}</button><div class="nav-sub" ${closed ? 'hidden' : ''}>${body}</div>`;
  };
  let html = `<a class="nav-a nav-back" href="#/"><svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 6l-6 6 6 6"/></svg>All clusters</a>`;
  NAV.forEach(([label, items]) => { html += group(label, items.map(([l, t]) => item(l, t)).join('')); });
  // custom resources: everything else the cluster serves, grouped by API group
  if (disc) {
    const known = new Set(NAV.flatMap(([, items]) => items.filter(([, t]) => Array.isArray(t)).map(([, t]) => t.join('/'))));
    const crds = disc.resources.filter(r => !BUILTIN_GROUPS.has(r.group) && !known.has(r.group + '/' + r.resource));
    const byGroup = {};
    crds.forEach(r => (byGroup[r.group] = byGroup[r.group] || []).push(r));
    const body = Object.keys(byGroup).sort().map(g => `<div class="muted" style="font-size:11.5px;padding:8px 10px 2px" title="${esc(g)}">${esc(g)}</div>` +
      byGroup[g].map(r => item(plural(r), [r.group, r.resource])).join('')).join('');
    html += group('Custom resources', body);
  }
  nav.innerHTML = html;
  $$('.nav-h', nav).forEach(h => h.addEventListener('click', () => {
    const closed = !h.classList.contains('closed');
    h.classList.toggle('closed', closed);
    h.nextElementSibling.hidden = closed;
    setNavClosed(h.dataset.g, closed);
  }));
}

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
  const section = route.ctx && ['overview', 'nodes', 'workloads', 'pods', 'events', 'logs'].includes(route.section) ? route.section : 'overview';
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
  if (route.ctx && !S.disc[route.ctx]) ensureDiscovery(route.ctx).then(() => { if (parseRoute().ctx === route.ctx) renderNav(parseRoute()); });
  const views = { overview: viewOverview, nodes: viewNodes, node: viewNode, workloads: viewWorkloads, wl: viewWorkload, pods: viewPods, namespaces: viewNamespaces,
    pod: viewPod, events: viewEvents, logs: viewLogs, r: viewResource, o: viewObject };
  main.scrollTop = 0;
  try {
    if (!route.ctx) await viewHome();
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
            : `Check VPN or network access to the API server, and that <code>kubectl --context ${esc(ctx || '')} get pods</code> works. KubeLamp retries automatically.`}</p>
          ${ctx ? '<button class="btn primary" id="errAccess">Access settings</button>' : ''}</div>`;
        $('#errAccess')?.addEventListener('click', () => openAccess(ctx));
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

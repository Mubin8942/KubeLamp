'use strict';
/* Generic resources: any type the cluster serves (incl. CRDs), kubectl-style tables,
   detail pages with a YAML editor, and the write actions. */

const gk = g => g || 'core';
const gv = k => (k === 'core' ? '' : k);
const listHref = (ctx, group, resource) => `${ctxPath(ctx)}/r/${enc(gk(group))}/${enc(resource)}`;
const objHref = (ctx, group, resource, ns, name, tab) =>
  `${ctxPath(ctx)}/o/${enc(gk(group))}/${enc(resource)}/${enc(ns || '_')}/${enc(name)}${tab ? '?tab=' + tab : ''}`;
const KIND_REF = {
  Pod: ['', 'pods'], Node: ['', 'nodes'], Deployment: ['apps', 'deployments'], StatefulSet: ['apps', 'statefulsets'],
  DaemonSet: ['apps', 'daemonsets'], Namespace: ['', 'namespaces'], Service: ['', 'services'],
};
// rich dashboard pages exist for these kinds
function richHref(ctx, kind, ns, name) {
  if (kind === 'Pod') return `${ctxPath(ctx)}/pod/${enc(ns)}/${enc(name)}`;
  if (kind === 'Node') return `${ctxPath(ctx)}/node/${enc(name)}`;
  if (['Deployment', 'StatefulSet', 'DaemonSet'].includes(kind)) return `${ctxPath(ctx)}/wl/${kind}/${enc(ns)}/${enc(name)}`;
  return null;
}
const ro = () => !!(S.info && S.info.readOnly);
function findRes(ctx, group, resource) {
  return (S.disc[ctx]?.resources || []).find(r => r.group === group && r.resource === resource);
}
const plural = r => r ? (r.kind.endsWith('y') ? r.kind.slice(0, -1) + 'ies' : r.kind.endsWith('s') ? r.kind + 'es' : r.kind + 's') : '';
const can = (r, verb) => !ro() && (!r || (r.verbs || []).includes(verb));

// ------------------------------------------------------------------ actions
const A = {
  async del(ctx, group, resource, ns, name, kind) {
    const ok = await confirmDanger(`Delete ${kind || resource} “${name}”?`,
      `${ns ? `In namespace <b>${esc(ns)}</b>. ` : ''}This can't be undone${kind === 'Pod' ? ' — its controller may recreate it' : ''}.`);
    if (!ok) return false;
    try {
      await send('DELETE', 'object?' + new URLSearchParams({ ctx, group, resource, ns: ns || '', name }));
      toast(`Deleted ${kind || resource} ${name}`);
      return true;
    } catch (e) { toast(e.message, 'err'); return false; }
  },
  async scale(ctx, group, resource, ns, name, current) {
    return modal({
      title: `Scale ${name}`,
      body: `<label class="field">Replicas<input type="number" id="scaleN" min="0" max="10000" value="${current ?? 1}"></label>`,
      actions: [{ label: 'Cancel', value: false }, {
        label: 'Scale', kind: 'primary', submit: true, run: async d => {
          const n = parseInt($('#scaleN', d).value, 10);
          if (!(n >= 0)) throw new Error('Enter 0 or more replicas');
          await send('POST', 'action', { ctx, group, resource, ns, name, action: 'scale', replicas: n });
          toast(`Scaled ${name} to ${n}`);
          return true;
        },
      }],
    });
  },
  async restart(ctx, group, resource, ns, name, kind) {
    const ok = await modal({ title: `Restart ${name}?`, body: `<p>Rolls out new pods for this ${esc(kind || 'workload')}, like <code>kubectl rollout restart</code>.</p>`,
      actions: [{ label: 'Cancel', value: false }, { label: 'Restart', kind: 'primary', value: true, submit: true }] });
    if (!ok) return false;
    try { await send('POST', 'action', { ctx, group, resource, ns, name, action: 'restart' }); toast(`Restarted ${name}`); return true; }
    catch (e) { toast(e.message, 'err'); return false; }
  },
  async cordon(ctx, name, on) {
    try { await send('POST', 'action', { ctx, group: '', resource: 'nodes', ns: '', name, action: on ? 'cordon' : 'uncordon' }); toast(`${on ? 'Cordoned' : 'Uncordoned'} ${name}`); return true; }
    catch (e) { toast(e.message, 'err'); return false; }
  },
  async exec(ctx, ns, pod, containers) {
    let c = containers && containers[0];
    if (containers && containers.length > 1) {
      c = await modal({ title: `Exec into ${pod}`, body: `<label class="field">Container<select id="execC">${containers.map(x => `<option>${esc(x)}</option>`).join('')}</select></label>`,
        actions: [{ label: 'Cancel', value: null }, { label: 'Open shell', kind: 'primary', submit: true, run: async d => $('#execC', d).value }] });
      if (!c) return;
    }
    Term.openExec(ctx, ns, pod, c);
  },
  async execAll(ctx, ns, pods, containers) {
    if (!pods.length) return toast('No running pods', 'err');
    const r = await modal({ title: `Shell in ${pods.length} pod${pods.length === 1 ? '' : 's'}`,
      body: `<p>Opens one session per pod, side by side. Broadcast is on: what you type goes to every ticked session, so check each command before pressing Enter.</p>
        ${containers.length > 1 ? `<label class="field">Container<select id="execC">${containers.map(x => `<option>${esc(x)}</option>`).join('')}</select></label>` : ''}
        ${pods.length > 12 ? `<p class="muted">Only the first 12 pods are opened.</p>` : ''}`,
      actions: [{ label: 'Cancel', value: null }, { label: 'Open shells', kind: 'primary', submit: true, run: async d => ({ c: $('#execC', d)?.value || containers[0] }) }] });
    if (!r) return;
    Term.openMulti(ctx, ns, pods, r.c);
  },
  logs: (ctx, ns, kind, name) => { location.hash = `${ctxPath(ctx)}/logs?ns=${enc(ns)}&kind=${enc(kind)}&name=${enc(name)}`; },
  yaml: (ctx, group, resource, ns, name) => { location.hash = objHref(ctx, group, resource, ns, name, 'yaml'); },
};

// row / header menu for one object
function objectMenu(anchor, o, after) {
  const { ctx, group, resource, ns, name, kind } = o;
  const res = findRes(ctx, group, resource);
  const items = [];
  const rich = richHref(ctx, kind, ns, name);
  if (rich && o.fromList) items.push({ label: 'Open dashboard', icon: ICON.eye, run: () => { location.hash = rich; } });
  if (o.fromList || rich) items.push({ label: 'View details & YAML', icon: ICON.eye, run: () => { location.hash = objHref(ctx, group, resource, ns, name); } });
  if (kind === 'Pod') {
    items.push({ label: 'Logs', icon: ICON.logs, run: () => { location.hash = `${ctxPath(ctx)}/logs?ns=${enc(ns)}&selector=${enc('')}&pod=${enc(name)}`; } });
    if (!ro()) items.push({ label: 'Exec shell', icon: ICON.term, run: () => A.exec(ctx, ns, name, o.containers) });
  }
  if (['Deployment', 'StatefulSet', 'DaemonSet'].includes(kind)) {
    items.push({ label: 'Logs of all pods', icon: ICON.logs, run: () => A.logs(ctx, ns, kind, name) });
    if (!ro() && kind !== 'DaemonSet') items.push({ label: 'Scale…', icon: ICON.scale, run: async () => { if (await A.scale(ctx, group, resource, ns, name, o.replicas)) after && after(); } });
    if (!ro()) items.push({ label: 'Restart', icon: ICON.restart, run: async () => { if (await A.restart(ctx, group, resource, ns, name, kind)) after && after(); } });
  }
  if (kind === 'Node' && !ro()) {
    items.push({ label: o.cordoned ? 'Uncordon' : 'Cordon', icon: ICON.pause, run: async () => { if (await A.cordon(ctx, name, !o.cordoned)) after && after(); } });
  }
  items.push({ label: can(res, 'update') ? 'Edit YAML' : 'View YAML', icon: ICON.edit, run: () => A.yaml(ctx, group, resource, ns, name) });
  if (can(res, 'delete')) {
    items.push('sep', { label: 'Delete', icon: ICON.del, danger: true, run: async () => { if (await A.del(ctx, group, resource, ns, name, kind)) after && after(true); } });
  }
  openPop(anchor, items);
}

// ------------------------------------------------------------------ generic list
async function viewResource(route) {
  const ctx = route.ctx, group = gv(route.args[0]), resource = route.args[1];
  await ensureDiscovery(ctx);
  const res = findRes(ctx, group, resource);
  if (!res) {
    main.innerHTML = `<div class="empty-state"><h2>${esc(resource)} isn't available on this cluster</h2><p>The API group <code>${esc(group || 'core')}</code> isn't installed, or your user can't see it.</p></div>`;
    return;
  }
  const title = plural(res);
  const nsActive = res.namespaced && S.ns;
  main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a>${group ? ` / <span class="mono">${esc(group)}</span>` : ''}</div>
      <h1>${esc(title)}</h1><div class="sub"><span>${res.namespaced ? (nsActive ? `Namespace ${esc(S.ns)}` : 'All namespaces') : 'Cluster-scoped'}</span><span class="muted mono">${esc(res.version)}</span></div></div>
      <div class="right"><button class="btn ghost" id="wideBtn" title="Show every column, like kubectl -o wide">Wide</button></div></div>
    <div class="filters"><input type="search" id="rq" placeholder="Filter by name, label or any column" style="width:320px"><span class="count" id="rcount"></span></div>
    <div class="card flush" id="rBox"><div class="muted" style="padding:16px">Loading…</div></div>
    ${resource === 'ingressroutes' && group.startsWith('traefik') ? '<div id="rtUnder"></div>' : ''}`;
  if ($('#rtUnder')) httpRoutesUnder(ctx, $('#rtUnder'));
  let wide = false, data = null;
  const isPvc = resource === 'persistentvolumeclaims' && group === '';
  if (isPvc) {
    $('.page-head .right').insertAdjacentHTML('afterbegin', `<button class="btn" id="pvcCheck" title="Run df inside a pod that mounts each volume. Readings are cached for 5 minutes to keep the load on pods low.">${ICON.edit}Check usage now</button>`);
    $('#pvcCheck').onclick = () => measure(true);
  }
  // usage: one df per pod (not per claim), namespaces measured one at a time, 5-minute cache on the server
  let measuring = false;
  const measure = async (force) => {
    if (!isPvc || !data || measuring || ro() && !force && false) return;
    const nss = [...new Set(data.rows.map(r => r.namespace).filter(Boolean))];
    if (nss.length > 12 && !force) {
      $('#rBox').insertAdjacentHTML('afterbegin', `<div class="banner info" id="pvcMany" style="margin:12px"><b>${nss.length} namespaces have claims.</b><span>Pick a namespace above to measure usage, or press “Check usage now” to measure all of them.</span></div>`);
      return;
    }
    measuring = true;
    const btn = $('#pvcCheck'); if (btn) btn.disabled = true;
    try {
      for (let i = 0; i < nss.length; i++) {
        if (parseRoute().section !== 'r') return;
        if (btn) btn.lastChild.textContent = `Checking ${i + 1}/${nss.length}…`;
        try { await loadPvcUsage(ctx, nss[i], { refresh: !!force }); } catch (e) { toast(`${nss[i]}: ${e.message}`, 'err'); }
        draw();
      }
    } finally { measuring = false; if (btn) { btn.disabled = false; btn.lastChild.textContent = 'Check usage now'; } }
  };
  $('#wideBtn').onclick = () => { wide = !wide; $('#wideBtn').classList.toggle('on', wide); draw(); };
  $('#rq').addEventListener('input', () => draw());
  const draw = () => {
    if (!data) return;
    const q = $('#rq').value.toLowerCase();
    const showNs = res.namespaced && !nsActive;
    const colIdx = data.columns.map((c, i) => [c, i]).filter(([c]) => (wide || !c.priority) && c.name.toLowerCase() !== 'name');
    const rows = data.rows.filter(r => !q || (r.name + ' ' + (r.namespace || '') + ' ' + JSON.stringify(r.cells) + ' ' + Object.entries(r.labels || {}).map(([k, v]) => k + '=' + v).join(' ')).toLowerCase().includes(q));
    const cols = [{ key: 'name', label: 'Name', cls: 'name', render: r => `<a href="${richHref(ctx, res.kind, r.namespace, r.name) || objHref(ctx, group, resource, r.namespace, r.name)}">${esc(r.name)}</a>` }];
    if (showNs) cols.push({ key: 'namespace', label: 'Namespace' });
    colIdx.forEach(([c, i]) => cols.push({
      key: 'c' + i, label: c.name, num: c.type === 'integer' || c.type === 'number',
      sort: c.name === 'Age' ? (r => -new Date(r.created)) : (r => r.cells[i] ?? ''),
      render: r => fmtCell(r.cells[i], c),
    }));
    if (isPvc) cols.push({ key: 'usage', label: 'Usage (df)', sort: r => S.pvc[pvcKey(ctx, r.namespace, r.name)]?.pct ?? -1,
      render: r => pvcCell(S.pvc[pvcKey(ctx, r.namespace, r.name)]) });
    cols.push({ key: 'act', label: '', nosort: true, cls: 'act', render: () => `<button class="icon-btn" data-menu title="Actions">${ICON.more}</button>` });
    const t = table('res-' + resource, cols, rows, { sort: { key: 'name', dir: 'asc' } });
    $('#rBox').innerHTML = t.html;
    t.bind($('#rBox'), draw);
    $$('[data-menu]', $('#rBox')).forEach(b => b.addEventListener('click', e => {
      e.stopPropagation();
      const r = t.rows[+b.closest('tr').dataset.i];
      objectMenu(b, { ctx, group, resource, ns: r.namespace, name: r.name, kind: res.kind, fromList: true }, load);
    }));
    $('#rcount').textContent = `${rows.length} of ${data.rows.length}`;
    if (data.warnings?.length) $('#rBox').insertAdjacentHTML('afterbegin', `<div class="banner" style="margin:12px"><b>Partly visible.</b><span>${esc(data.warnings.join('; '))}</span></div>`);
  };
  const load = async () => {
    try {
      const first = !data;
      data = await api(`table?ctx=${enc(ctx)}&group=${enc(group)}&resource=${enc(resource)}&ns=${enc(res.namespaced ? S.ns : '')}`);
      draw();
      if (first) measure(false);
    } catch (e) {
      if (e.name === 'AbortError') return;
      $('#rBox').innerHTML = `<div class="empty-state" style="padding:40px 20px"><h2>Can't list ${esc(title.toLowerCase())}</h2><p>${esc(e.message)}</p>
        ${/forbidden|RBAC/i.test(e.message) ? '<p class="muted">Your kubeconfig user lacks <code>list</code> permission here. Pick a namespace you can access from the namespace menu, or ask for a Role that includes this resource.</p>' : ''}</div>`;
    }
  };
  // the table helper links each row; we use explicit anchors instead
  await load();
  every(20000, async () => { await load(); if (isPvc) measure(false); });
}

function fmtCell(v, c) {
  if (v === null || v === undefined || v === '') return '<span class="muted">—</span>';
  if (typeof v === 'object') return esc(JSON.stringify(v));
  const s = String(v);
  if (/^(Running|Ready|Bound|Active|True|Succeeded|Completed|Available|SecretSynced)$/.test(s)) return stLabel(s, 'good');
  if (/^(Pending|ContainerCreating|Terminating|Unknown|Progressing)$/.test(s)) return stLabel(s, 'warning');
  if (/BackOff|Error|Failed|False|OOMKilled|Lost|Evicted/.test(s)) return stLabel(s, 'critical');
  return esc(s);
}

// ------------------------------------------------------------------ JSON → YAML (display only)
function yamlish(v, ind = '') {
  if (v === null || v === undefined) return 'null';
  if (Array.isArray(v)) {
    if (!v.length) return '[]';
    return v.map(x => {
      const s = yamlish(x, ind + '  ');
      return '\n' + ind + '- ' + (typeof x === 'object' && x && !Array.isArray(x) && Object.keys(x).length ? s.replace(/^\n\s*/, '') : s);
    }).join('');
  }
  if (typeof v === 'object') {
    const ks = Object.keys(v);
    if (!ks.length) return '{}';
    return ks.map(k => {
      const x = v[k];
      const s = yamlish(x, ind + '  ');
      return '\n' + ind + k + ':' + (typeof x === 'object' && x && (Array.isArray(x) ? x.length : Object.keys(x).length) ? s : ' ' + s);
    }).join('');
  }
  if (typeof v === 'string' && v.includes('\n')) return '|\n' + v.replace(/\n$/, '').split('\n').map(l => ind + l).join('\n');
  if (typeof v === 'string' && (/^(true|false|null|yes|no|~|-?\d+(\.\d+)?)$/i.test(v) || /[:#{}\[\],&*!|>'"%@`]/.test(v) || v === '')) return JSON.stringify(v);
  return String(v);
}

function b64decode(s) {
  try { return new TextDecoder().decode(Uint8Array.from(atob(s), c => c.charCodeAt(0))); } catch { return s; }
}

// ------------------------------------------------------------------ object detail
async function viewObject(route) {
  const ctx = route.ctx, group = gv(route.args[0]), resource = route.args[1], ns = route.args[2] === '_' ? '' : route.args[2], name = route.args[3];
  await ensureDiscovery(ctx);
  const res = findRes(ctx, group, resource) || { kind: resource, verbs: [], group, resource };
  let tab = route.q.tab || 'overview';
  let d;
  try { d = await api(`object?ctx=${enc(ctx)}&group=${enc(group)}&resource=${enc(resource)}&ns=${enc(ns)}&name=${enc(name)}`); }
  catch (e) {
    if (e.name === 'AbortError') return;
    main.innerHTML = `<div class="empty-state"><h2>Can't open ${esc(res.kind)} “${esc(name)}”</h2><p>${esc(e.message)}</p><a class="btn" href="${listHref(ctx, group, resource)}">Back to ${esc(plural(res))}</a></div>`;
    return;
  }
  const o = d.object || {}, md = o.metadata || {};
  const kind = o.kind || res.kind;
  const rich = richHref(ctx, kind, ns, name);
  const editable = can(res, 'update');
  const crumbs = `<div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${listHref(ctx, group, resource)}">${esc(plural(res))}</a>${ns ? ' / ' + esc(ns) : ''}</div>`;
  const actions = [];
  if (rich) actions.push(`<a class="btn" href="${rich}">${ICON.eye}Dashboard</a>`);
  if (kind === 'Pod' && !ro()) actions.push(`<button class="btn" data-act="exec">${ICON.term}Exec</button>`);
  if (['Deployment', 'StatefulSet', 'DaemonSet'].includes(kind)) actions.push(`<button class="btn" data-act="logs">${ICON.logs}Logs</button>`);
  actions.push(`<button class="btn" data-act="yaml">${ICON.edit}${editable ? 'Edit YAML' : 'YAML'}</button>`);
  if (can(res, 'delete')) actions.push(`<button class="btn danger" data-act="delete">${ICON.del}Delete</button>`);
  actions.push(`<button class="btn ghost" data-act="more" title="More actions">${ICON.more}</button>`);
  main.innerHTML = `<div class="page-head"><div>${crumbs}<h1><span class="kind">${esc(kind)}</span>${esc(name)}</h1>
      <div class="sub">${ns ? `<span>namespace ${esc(ns)}</span>` : ''}<span>created ${age(md.creationTimestamp)} ago</span><span class="muted mono">${esc(o.apiVersion || '')}</span></div></div>
      <div class="right">${actions.join('')}</div></div>
    <div class="tabs" role="tablist">
      <a data-tab="overview">Overview</a><a data-tab="yaml">YAML</a><a data-tab="events">Events${d.events?.length ? ` (${d.events.length})` : ''}</a></div>
    <div id="tabBody"></div>`;
  const refresh = () => navigate();
  $$('[data-act]', main).forEach(b => b.addEventListener('click', () => {
    const a = b.dataset.act;
    if (a === 'exec') A.exec(ctx, ns, name, (o.spec?.containers || []).map(c => c.name));
    if (a === 'logs') A.logs(ctx, ns, kind, name);
    if (a === 'yaml') show('yaml');
    if (a === 'delete') A.del(ctx, group, resource, ns, name, kind).then(ok => { if (ok) location.hash = listHref(ctx, group, resource); });
    if (a === 'more') objectMenu(b, { ctx, group, resource, ns, name, kind, replicas: o.spec?.replicas, cordoned: o.spec?.unschedulable, containers: (o.spec?.containers || []).map(c => c.name) },
      deleted => { if (deleted) location.hash = listHref(ctx, group, resource); else refresh(); });
  }));
  const show = t => {
    tab = t;
    $$('.tabs a', main).forEach(a => a.classList.toggle('on', a.dataset.tab === t));
    history.replaceState(null, '', objHref(ctx, group, resource, ns, name, t === 'overview' ? '' : t));
    if (t === 'overview') renderOverview(o, kind, ctx);
    if (t === 'yaml') renderYaml(d.yaml, editable, { ctx, group, resource, ns, name });
    if (t === 'events') $('#tabBody').innerHTML = `<div class="card">${eventsList(d.events || [])}</div>`;
  };
  $$('.tabs a', main).forEach(a => a.addEventListener('click', () => show(a.dataset.tab)));
  show(tab);
}

function renderOverview(o, kind, ctx) {
  const md = o.metadata || {};
  const kv = (obj, cls = 'chip') => Object.entries(obj || {}).map(([k, v]) => `<span class="${cls}">${esc(k)}${v !== '' ? '=' + esc(v) : ''}</span>`).join('') || '<span class="muted">none</span>';
  let special = '';
  if (kind === 'ConfigMap' || kind === 'Secret') {
    const data = o.data || {};
    const keys = Object.keys(data);
    special = `<div class="card"><div class="card-head"><h3>Data</h3><span class="cap">${keys.length} key${keys.length === 1 ? '' : 's'}${kind === 'Secret' ? ' · values are hidden until you reveal them' : ''}</span></div>
      <div class="datakv">${keys.map((k, i) => {
        const val = kind === 'Secret' ? b64decode(data[k]) : data[k];
        return `<div><div class="k"><span>${esc(k)}</span><span class="grow"></span>
          ${kind === 'Secret' ? `<button class="btn sm ghost" data-reveal="${i}">${ICON.eye}Reveal</button>` : ''}
          <button class="btn sm ghost" data-copy="${i}">${ICON.copy}Copy</button></div>
          <pre class="${kind === 'Secret' ? 'masked' : ''}" data-val="${i}">${kind === 'Secret' ? '••••••••••••' : esc(val)}</pre></div>`;
      }).join('') || '<span class="muted">No data</span>'}</div></div>`;
    setTimeout(() => {
      keys.forEach((k, i) => {
        const val = kind === 'Secret' ? b64decode(data[k]) : data[k];
        $(`[data-copy="${i}"]`)?.addEventListener('click', () => navigator.clipboard.writeText(val).then(() => toast(`Copied ${k}`)));
        $(`[data-reveal="${i}"]`)?.addEventListener('click', e => {
          const pre = $(`[data-val="${i}"]`), shown = !pre.classList.contains('masked');
          pre.classList.toggle('masked', shown);
          pre.textContent = shown ? '••••••••••••' : val;
          e.currentTarget.lastChild.textContent = shown ? 'Reveal' : 'Hide';
        });
      });
    });
  } else if (kind === 'PersistentVolumeClaim') {
    const key = pvcKey(ctx, md.namespace, md.name);
    special = `<div class="card"><div class="card-head"><h3>Volume usage</h3><span class="cap">measured with df inside a pod that mounts it</span>
      <div class="right"><button class="btn" id="pvcRe">Re-check</button></div></div><div id="pvcUse">${pvcCell(S.pvc[key])}</div>
      <div class="kvgrid" style="margin-top:10px"><span>Requested</span><span>${esc(o.spec?.resources?.requests?.storage || '—')}</span>
      <span>Class</span><span>${esc(o.spec?.storageClassName || '—')}</span><span>Volume</span><span class="mono">${esc(o.spec?.volumeName || '—')}</span></div></div>`;
    setTimeout(() => {
      const run = async force => {
        $('#pvcUse').innerHTML = '<span class="muted">checking…</span>';
        try { await loadPvcUsage(ctx, md.namespace, { name: md.name, refresh: force }); } catch (e) { S.pvc[key] = { state: 'error', err: e.message }; }
        if ($('#pvcUse')) $('#pvcUse').innerHTML = pvcCell(S.pvc[key]);
      };
      $('#pvcRe')?.addEventListener('click', () => run(true));
      run(false);
    });
  } else if (['IngressRoute', 'HTTPRoute', 'Ingress'].includes(kind)) {
    special = `<div class="card" id="rtDetail"><span class="muted">Loading routing…</span></div>`;
    setTimeout(() => routeDetailCard(ctx, kind, md.namespace || '', md.name));
  } else if (kind === 'Service') {
    const sp = o.spec || {};
    special = `<div class="card"><h3>Service</h3><div class="kvgrid" style="margin-top:8px">
      <span>Type</span><span>${esc(sp.type)}</span><span>Cluster IP</span><span class="mono">${esc(sp.clusterIP || '—')}</span>
      ${o.status?.loadBalancer?.ingress ? `<span>External</span><span class="mono">${esc(o.status.loadBalancer.ingress.map(i => i.ip || i.hostname).join(', '))}</span>` : ''}
      <span>Ports</span><span>${(sp.ports || []).map(p => `<span class="chip">${esc(p.name ? p.name + ' ' : '')}${esc(p.port)}→${esc(p.targetPort ?? p.port)}/${esc(p.protocol || 'TCP')}</span>`).join(' ')}</span>
      <span>Selector</span><span class="chips">${kv(sp.selector)}</span></div></div>`;
  }
  const conds = o.status?.conditions || [];
  const spec = o.spec ?? o.rules ?? o.subjects;
  $('#tabBody').innerHTML = `${special}
    <div class="grid g2 ${special ? 'mt' : ''}">
      <div class="card"><h3>Metadata</h3><div class="kvgrid" style="margin-top:8px">
        ${md.namespace ? `<span>Namespace</span><span>${esc(md.namespace)}</span>` : ''}
        <span>Created</span><span>${esc(new Date(md.creationTimestamp).toLocaleString())} (${age(md.creationTimestamp)} ago)</span>
        <span>UID</span><span class="mono">${esc(md.uid || '—')}</span>
        ${md.ownerReferences?.length ? `<span>Owned by</span><span>${md.ownerReferences.map(r => esc(r.kind + '/' + r.name)).join(', ')}</span>` : ''}
        ${md.finalizers?.length ? `<span>Finalizers</span><span class="mono">${esc(md.finalizers.join(', '))}</span>` : ''}
        <span>Resource version</span><span class="mono">${esc(md.resourceVersion || '—')}</span></div></div>
      <div class="card"><h3>Labels</h3><div class="chips" style="margin-top:8px">${kv(md.labels)}</div>
        <h3 class="mt">Annotations</h3><div class="chips" style="margin-top:8px">${kv(Object.fromEntries(Object.entries(md.annotations || {}).filter(([k]) => k !== 'kubectl.kubernetes.io/last-applied-configuration')))}</div></div>
    </div>
    ${conds.length ? `<div class="card mt"><h3>Conditions</h3><div class="tbl-wrap" style="margin-top:6px"><table class="t"><thead><tr><th>Type</th><th>Status</th><th>Reason</th><th>Message</th><th>Last change</th></tr></thead><tbody>
      ${conds.map(c => `<tr><td class="name">${esc(c.type)}</td><td>${stLabel(c.status, c.status === 'True' ? 'good' : c.status === 'False' ? 'critical' : 'warning')}</td><td>${esc(c.reason || '')}</td><td style="white-space:normal">${esc(c.message || '')}</td><td class="muted">${c.lastTransitionTime ? age(c.lastTransitionTime) + ' ago' : ''}</td></tr>`).join('')}</tbody></table></div></div>` : ''}
    ${spec !== undefined && kind !== 'ConfigMap' && kind !== 'Secret' ? `<div class="card mt"><div class="card-head"><h3>${o.spec ? 'Spec' : o.rules ? 'Rules' : 'Subjects'}</h3><span class="cap">read-only view · edit in the YAML tab</span></div>
      <div class="datakv"><pre>${esc(yamlish(spec).replace(/^\n/, ''))}</pre></div></div>` : ''}`;
}

function renderYaml(yaml, editable, ref) {
  $('#tabBody').innerHTML = `<div class="edbar">
      ${editable ? `<button class="btn primary" id="yApply">Apply changes</button><button class="btn" id="yReset">Reset</button>` : `<span class="muted">${ro() ? 'Read-only mode — editing is disabled.' : "Your user can't update this resource."}</span>`}
      <span class="grow"></span><span class="muted" id="yState"></span>
      <button class="btn ghost" id="yCopy">${ICON.copy}Copy</button></div>
    <div class="editor"><div class="gutter" id="yGut"></div><textarea id="yText" spellcheck="false" autocapitalize="off" autocomplete="off" ${editable ? '' : 'readonly'}></textarea></div>`;
  const ta = $('#yText'), gut = $('#yGut');
  ta.value = yaml;
  const lines = () => { gut.textContent = Array.from({ length: ta.value.split('\n').length }, (_, i) => i + 1).join('\n'); };
  const dirty = () => { $('#yState').textContent = ta.value !== yaml ? 'Unsaved changes' : ''; };
  lines();
  ta.addEventListener('input', () => { lines(); dirty(); });
  ta.addEventListener('scroll', () => { gut.scrollTop = ta.scrollTop; });
  ta.addEventListener('keydown', e => {
    if (e.key === 'Tab') { e.preventDefault(); document.execCommand('insertText', false, '  '); }
    if ((e.metaKey || e.ctrlKey) && e.key === 's') { e.preventDefault(); $('#yApply')?.click(); }
  });
  $('#yCopy').onclick = () => navigator.clipboard.writeText(ta.value).then(() => toast('YAML copied'));
  if (!editable) return;
  $('#yReset').onclick = () => { ta.value = yaml; lines(); dirty(); };
  $('#yApply').onclick = async () => {
    const b = $('#yApply'); b.disabled = true; b.textContent = 'Applying…';
    try {
      const r = await send('PUT', 'object', { ...ref, yaml: ta.value });
      yaml = r.yaml; ta.value = yaml; lines(); dirty();
      toast(`Applied changes to ${ref.name}`);
    } catch (e) {
      toast(e.message, 'err');
      $('#yState').textContent = 'Not applied';
    } finally { b.disabled = false; b.textContent = 'Apply changes'; }
  };
}

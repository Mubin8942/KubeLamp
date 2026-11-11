'use strict';
/* Tools menu (top bar) and the tools it opens. Add a tool by appending to TOOLS. */

const TOOL_GROUPS = ['Networking', 'Files'];
const TOOLS = [
  { id: 'conntest', group: 'Networking', name: 'Pod Connectivity Inspector', desc: 'Test TCP/HTTP reach from pods to IPs, ports and domains, and check network policies.', needsExec: true, view: viewConnTest },
  { id: 'dns', group: 'Networking', name: 'DNS Checker', desc: 'Resolve names from inside pods and compare the IPs each pod gets, with the pod\'s DNS settings.', needsExec: true, view: viewDnsCheck },
  { id: 'files', group: 'Files', name: 'File Transfer', desc: 'Download logs and assets from pods and upload files into mounted volumes, with size limits.', needsExec: true, view: viewFileTransfer },
  { id: 'explorer', group: 'Files', name: 'File Explorer', desc: 'Browse a container\'s folders and read any file its user is allowed to read.', needsExec: true, view: viewFileExplorer },
];

const toolsHref = (ctx, id) => `${ctxPath(ctx)}/tools/${id}`;
const firstCtx = () => (S.clusters.find(c => c.status !== 'error') || {}).name || null;

$('#toolsBtn').addEventListener('click', () => {
  const route = parseRoute();
  const ctx = route.ctx || firstCtx();
  const items = [];
  for (const g of TOOL_GROUPS) {
    items.push({ head: g });
    for (const t of TOOLS.filter(x => x.group === g)) {
      const off = t.needsExec && S.info && S.info.readOnly;
      items.push({
        label: t.name, sub: off ? 'read-only' : '', on: route.section === 'tools' && route.args[0] === t.id,
        run: () => off ? toast('Disabled: KubeLamp was started with -read-only', 'err')
          : ctx ? (location.hash = toolsHref(ctx, t.id)) : toast('No cluster is available yet', 'err'),
      });
    }
  }
  openPop($('#toolsBtn'), items);
});

async function viewTools(route) {
  const t = TOOLS.find(x => x.id === route.args[0]);
  if (t) return t.view(route);
  main.innerHTML = `<div class="page-head"><div><h1>Tools</h1></div></div>` + TOOL_GROUPS.map(g => `<div class="tools-group">${esc(g)}</div>
    <div class="tools-grid">${TOOLS.filter(x => x.group === g).map(x => `<a class="card tool-card" href="${toolsHref(route.ctx, x.id)}"><h3>${esc(x.name)}</h3><div class="cap">${esc(x.desc)}</div></a>`).join('')}</div>`).join('');
}

// ------------------------------------------------------------------ connectivity inspector
const CT_STATES = {
  open: { label: 'Open', cls: 'good', sym: '✓', tip: 'Connected: the path is allowed.' },
  refused: { label: 'Refused', cls: 'warn', sym: '↩', tip: 'The host answered with a reset: the network and firewall allow it, but nothing listens on that port.' },
  timeout: { label: 'Timeout', cls: 'crit', sym: '⏱', tip: 'No answer within the time limit. Usually a firewall DROP (or a network policy).' },
  unreachable: { label: 'Unreachable', cls: 'crit', sym: '✕', tip: 'No route to host / ICMP reject: a firewall REJECT or a routing problem.' },
  dns: { label: 'DNS failed', cls: 'dns', sym: '?', tip: 'The name did not resolve from inside the pod.' },
  tls: { label: 'TLS error', cls: 'tls', sym: '🔒', tip: 'TCP connected but the TLS handshake or certificate check failed: the path is open.' },
  notool: { label: 'No tool', cls: 'grey', sym: '–', tip: 'The container has a shell but no nc, curl, wget or bash.' },
  noshell: { label: 'No shell', cls: 'grey', sym: '–', tip: 'No sh in any container of this pod (distroless image).' },
  error: { label: 'Error', cls: 'grey', sym: '!', tip: 'Something else went wrong; see the details.' },
};
const CT_FAIL = new Set(['timeout', 'unreachable']);

async function ctSend(method, path, body) {
  const r = await fetch(apiURL(path), { method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw httpErr(r, d);
  return d;
}

async function viewConnTest(route) {
  const ctx = route.ctx;
  const key = 'kl.conntest.' + ctx, jobKey = 'kl.conntest.job.' + ctx;
  const roOnly = !!(S.info && S.info.readOnly);
  const saved = (() => { try { return JSON.parse(localStorage.getItem(key) || '{}'); } catch { return {}; } })();
  const F = Object.assign({ ns: S.ns || '', mode: 'per-node', pods: [], targets: '', ports: '', policy: true, timeout: 3, parallel: 8 }, saved);
  let nss = [], allPods = [], valid = false;
  try { nss = await api('namespaces?ctx=' + enc(ctx)); } catch {}
  if (!F.ns || !nss.some(n => n.name === F.ns)) F.ns = (S.ns && nss.some(n => n.name === S.ns) ? S.ns : (nss.find(n => n.pods) || nss[0] || {}).name) || '';

  main.innerHTML = `
    <div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/tools">Tools</a></div>
      <h1>Pod Connectivity Inspector</h1>
      <div class="sub"><span>Runs nc, curl or wget inside your pods to show whether the worker's firewall lets traffic through to a destination.</span></div></div></div>
    ${roOnly ? `<div class="banner err"><b>Read-only mode.</b><span>This tool runs commands inside pods, so it is disabled.</span></div>` : ''}
    <div class="card ct-form">
      <div class="ct-row">
        <label class="ct-f"><span>Namespace</span><select id="ctNs">${nss.map(n => `<option value="${esc(n.name)}" ${n.name === F.ns ? 'selected' : ''}>${esc(n.name)}${n.pods ? ` (${n.pods})` : ''}</option>`).join('')}</select></label>
        <div class="ct-f"><span>Test from</span>
          <div class="seg" id="ctMode">
            <button type="button" data-m="per-node">One pod per node</button><button type="button" data-m="all">All running pods</button><button type="button" data-m="pick">Pick pods</button>
          </div></div>
      </div>
      <div id="ctPick" class="ct-pick" hidden></div>
      <label class="ct-f"><span>Targets <small class="muted">IPs, ranges, CIDR, domains, URLs. Separate with commas, spaces or new lines</small></span>
        <textarea id="ctTargets" rows="3" spellcheck="false" placeholder="172.18.1.32-40, 10.10.5.7, db.internal:5432, https://api.example.com/health">${esc(F.targets)}</textarea></label>
      <label class="ct-f"><span>Ports <small class="muted">for targets without their own port. A domain without a port is checked over HTTPS</small></span>
        <input type="text" id="ctPorts" spellcheck="false" placeholder="22-25, 443" value="${esc(F.ports)}"></label>
      <div class="ct-row ct-opts">
        <label class="chk" title="Reads NetworkPolicies and Cilium policies and shows whether their egress rules would allow each connection. It sends no extra traffic."><input type="checkbox" id="ctPol" ${F.policy ? 'checked' : ''}> Check network policies (NetworkPolicy, Cilium)</label>
        <label class="ct-inl">Timeout <select id="ctTimeout">${[1, 2, 3, 5, 8, 10].map(v => `<option ${v === +F.timeout ? 'selected' : ''} value="${v}">${v}s</option>`).join('')}</select></label>
        <label class="ct-inl">Parallel per pod <select id="ctPar">${[1, 4, 8, 16].map(v => `<option ${v === +F.parallel ? 'selected' : ''} value="${v}">${v}</option>`).join('')}</select></label>
      </div>
      <div id="ctPrev" class="ct-prev muted">Enter targets to see how many checks will run.</div>
      <div class="ct-actions"><button class="btn primary" id="ctRun" disabled>Run test</button><button class="btn" id="ctCancel" hidden>Cancel</button></div>
    </div>
    <div id="ctOut"></div>`;

  // ---- form state
  const persist = () => { try { localStorage.setItem(key, JSON.stringify(F)); } catch {} };
  const read = () => {
    F.ns = $('#ctNs').value; F.targets = $('#ctTargets').value; F.ports = $('#ctPorts').value;
    F.policy = $('#ctPol').checked; F.timeout = +$('#ctTimeout').value; F.parallel = +$('#ctPar').value;
    persist();
  };
  const body = () => ({ ns: F.ns, podMode: F.mode, pods: F.pods, targets: F.targets, ports: F.ports, timeout: F.timeout, parallel: F.parallel, policyCheck: F.policy });
  const drawMode = () => {
    $$('#ctMode button').forEach(b => b.classList.toggle('on', b.dataset.m === F.mode));
    const pk = $('#ctPick');
    pk.hidden = F.mode !== 'pick';
    if (F.mode === 'pick') {
      pk.innerHTML = allPods.length ? allPods.map(p => `<label class="chk"><input type="checkbox" value="${esc(p.pod)}" ${F.pods.includes(p.pod) ? 'checked' : ''}> <span class="mono">${esc(p.pod)}</span> <span class="muted">${esc(p.node)} · ${esc(p.podIP)}</span></label>`).join('')
        : '<span class="muted">No running pods in this namespace.</span>';
      $$('input', pk).forEach(i => i.addEventListener('change', () => { F.pods = $$('input:checked', pk).map(x => x.value); persist(); schedulePreview(); }));
    }
  };
  let prevTimer = 0, prevSeq = 0;
  const schedulePreview = () => { clearTimeout(prevTimer); prevTimer = setTimeout(preview, 350); };
  async function preview() {
    const seq = ++prevSeq;
    const box = $('#ctPrev'), run = $('#ctRun');
    if (!box) return;
    if (!F.targets.trim()) { box.className = 'ct-prev muted'; box.textContent = 'Enter targets to see how many checks will run.'; valid = false; run.disabled = true; return; }
    let d;
    try { d = await ctSend('POST', 'tools/conntest/preview?ctx=' + enc(ctx), body()); } catch (e) { if (seq === prevSeq) { box.className = 'ct-prev err'; box.textContent = e.message; run.disabled = true; } return; }
    if (seq !== prevSeq || !$('#ctPrev')) return;
    if (F.mode !== 'all' && d.sources === undefined && !d.sourceErr) { /* keep */ }
    const pods = (d.sources || []).length;
    valid = !d.errors.length && d.count > 0 && pods > 0 && !roOnly;
    run.disabled = !valid || (T.job && T.job.state !== 'done' && T.job.state !== 'cancelled');
    if (d.errors.length) {
      box.className = 'ct-prev err';
      box.innerHTML = d.errors.map(e => `<div><span class="mono">${esc(e.token)}</span>: ${esc(e.msg)}</div>`).join('');
    } else {
      box.className = 'ct-prev';
      const nodes = new Set((d.sources || []).map(s => s.node)).size;
      box.innerHTML = `<b>${d.count}</b> check${d.count === 1 ? '' : 's'} × <b>${pods}</b> pod${pods === 1 ? '' : 's'} (${nodes} node${nodes === 1 ? '' : 's'}) = <b>${d.count * pods}</b> probes` +
        (d.sourceErr ? ` <span class="crit">${esc(d.sourceErr)}</span>` : '') +
        `<div class="chips" style="margin-top:6px">${d.sample.map(s => `<span class="chip">${esc(s)}</span>`).join('')}${d.count > d.sample.length ? `<span class="muted">+${d.count - d.sample.length} more</span>` : ''}</div>`;
    }
  }
  async function loadPods() {
    allPods = [];
    try { const d = await ctSend('POST', 'tools/conntest/preview?ctx=' + enc(ctx), { ns: F.ns, podMode: 'all', targets: '', ports: '' }); allPods = d.sources || []; } catch {}
    F.pods = F.pods.filter(p => allPods.some(a => a.pod === p));
    drawMode();
  }

  // ---- job state and results
  const T = { openPods: new Set(), job: null, sources: [], checks: [], res: new Map(), policy: {}, warnings: [], next: 0, view: 'target', onlyProblems: false, limit: 200, timer: 0 };
  const rkey = (si, ci) => si * 100000 + ci;
  const jobDone = () => !T.job || T.job.state === 'done' || T.job.state === 'cancelled';
  async function start() {
    read();
    $('#ctRun').disabled = true;
    try {
      const d = await ctSend('POST', 'tools/conntest?ctx=' + enc(ctx), body());
      attach(d.id);
    } catch (e) { toast(e.message, 'err'); $('#ctRun').disabled = !valid; }
  }
  function attach(id) {
    clearInterval(T.timer);
    T.job = { id, state: 'preparing', total: 0, done: 0 }; T.sources = []; T.checks = []; T.res = new Map(); T.policy = {}; T.warnings = []; T.next = 0; T.limit = 200; T.policyFetched = false;
    try { sessionStorage.setItem(jobKey, id); } catch {}
    $('#ctCancel').hidden = false; $('#ctRun').disabled = true;
    T.timer = setInterval(poll, 1000); S.timers.push(T.timer);
    poll();
  }
  let polling = false;
  async function poll() {
    if (polling || !T.job) return;
    polling = true;
    try {
      const first = T.next === 0 && !T.checks.length;
      const d = await ctSend('GET', `tools/conntest/${T.job.id}?from=${T.next}${first ? '&full=1' : ''}`);
      T.job = d; T.sources = d.sources; T.warnings = d.warnings || [];
      if (d.checks) { T.checks = d.checks; T.policy = d.policy || {}; }
      d.results.forEach(r => T.res.set(rkey(r.src, r.check), r));
      T.next = d.next;
      if (d.policyCheck && !T.policyFetched && d.state !== 'preparing') {
        // policy verdicts are ready once the job leaves "preparing": fetch them once
        T.policyFetched = true;
        const f = await ctSend('GET', `tools/conntest/${T.job.id}?from=${T.next}&full=1`);
        T.policy = f.policy || {}; T.checks = f.checks || T.checks;
      }
      drawResults();
      if (jobDone()) { clearInterval(T.timer); $('#ctCancel').hidden = true; $('#ctRun').disabled = !valid; }
    } catch (e) {
      clearInterval(T.timer); $('#ctCancel').hidden = true; $('#ctRun').disabled = !valid;
      T.job = null; try { sessionStorage.removeItem(jobKey); } catch {}
      if (!/not found/i.test(e.message)) toast(e.message, 'err');
    } finally { polling = false; }
  }

  const pv = (si, ci) => (T.policy[si] || T.policy[String(si)] || {})[ci];
  const cellHtml = (si, ci) => {
    const r = T.res.get(rkey(si, ci));
    const p = pv(si, ci);
    const pol = p && (p.s === 'blocked' || p.s === 'unknown') ? `<i class="ct-pol ${p.s}"></i>` : '';
    if (!r) return `<button class="ct-cell pending" data-s="${si}" data-c="${ci}" title="Waiting…"><span class="spin"></span>${pol}</button>`;
    const m = CT_STATES[r.state] || CT_STATES.error;
    return `<button class="ct-cell ${m.cls}" data-s="${si}" data-c="${ci}" title="${esc(m.label + (r.http ? ' · HTTP ' + r.http : '') + (r.ms ? ' · ' + r.ms + ' ms' : ''))}">${m.sym}${pol}</button>`;
  };
  const countsFor = (pairs) => {
    const c = {};
    pairs.forEach(([si, ci]) => { const r = T.res.get(rkey(si, ci)); const s = r ? r.state : 'pending'; c[s] = (c[s] || 0) + 1; });
    return c;
  };
  const chipsHtml = (c) => Object.entries(c).sort((a, b) => b[1] - a[1]).map(([s, n]) => {
    const m = CT_STATES[s]; return `<span class="ct-chip ${m ? m.cls : 'grey'}" title="${esc(m ? m.tip : 'Waiting for the probe')}">${n} ${esc(m ? m.label : 'pending')}</span>`;
  }).join('');

  // the addresses a host name resolved to, across the pods that tested it
  function ipsFor(ci) {
    const sets = new Map();
    T.sources.forEach((s, si) => {
      const r = T.res.get(rkey(si, ci));
      if (!r || !Array.isArray(r.ips)) return;
      const k = r.ips.join(',');
      if (!sets.has(k)) sets.set(k, []);
      sets.get(k).push(s);
    });
    if (!sets.size) return null;
    const all = [...new Set([...sets.keys()].flatMap(k => k ? k.split(',') : []))];
    return { all, sets, differ: sets.size > 1, none: !all.length };
  }
  const ipList = (ips, n = 4) => ips.slice(0, n).join(', ') + (ips.length > n ? ` +${ips.length - n}` : '');
  function ipsNote(ci) {
    const x = ipsFor(ci);
    if (!x) return '';
    if (x.none) return `<div class="ct-ips crit" title="The name returned no address inside the pods">→ no address</div>`;
    const tip = [...x.sets.entries()].map(([k, srcs]) => `${k ? k.split(',').join(', ') : 'no address'}  ←  ${srcs.map(s => s.node || s.pod).join(', ')}`).join('\n');
    return x.differ ? `<div class="ct-ips warn" title="${esc('Different pods got different answers:\n' + tip)}">→ ${esc(ipList(x.all))} <b>(differs by pod)</b></div>`
      : `<div class="ct-ips" title="${esc('Resolved inside the pods: ' + x.all.join(', '))}">→ ${esc(ipList(x.all))}</div>`;
  }

  function policyNote(ci) {
    if (!T.job || !T.job.policyCheck) return '';
    let blocked = 0, unknown = 0;
    T.sources.forEach((_, si) => { const p = pv(si, ci); if (p && p.s === 'blocked') blocked++; else if (p && p.s === 'unknown') unknown++; });
    if (!blocked && !unknown) return '';
    return `<div class="ct-pnote ${blocked ? 'blocked' : 'unknown'}">${blocked ? `Policy blocks ${blocked}/${T.sources.length}` : `Policy unclear on ${unknown}/${T.sources.length}`}</div>`;
  }

  function drawResults() {
    const out = $('#ctOut');
    if (!out || !T.job) return;
    const j = T.job, nS = T.sources.length, nC = T.checks.length;
    const all = []; T.sources.forEach((_, si) => T.checks.forEach((c, ci) => all.push([si, ci])));
    const totals = countsFor(all);
    const pct = j.total ? Math.round(j.done / j.total * 100) : 0;
    const failPol = all.filter(([si, ci]) => { const r = T.res.get(rkey(si, ci)); const p = pv(si, ci); return r && CT_FAIL.has(r.state) && p && p.s === 'blocked'; }).length;
    const openPol = all.filter(([si, ci]) => { const r = T.res.get(rkey(si, ci)); const p = pv(si, ci); return r && r.state === 'open' && p && p.s === 'blocked'; }).length;
    const nFail = (totals.timeout || 0) + (totals.unreachable || 0);
    const nRetry = nFail + (totals.dns || 0) + (totals.error || 0);
    const stateName = { preparing: 'Preparing…', running: 'Running…', done: 'Finished', cancelled: 'Cancelled' }[j.state] || j.state;
    let rowsHtml = '';
    if (T.view === 'target') {
      const rows = T.checks.map((c, ci) => ({ c, ci, counts: countsFor(T.sources.map((_, si) => [si, ci])) }))
        .filter(r => !T.onlyProblems || Object.keys(r.counts).some(s => !['open', 'refused', 'tls'].includes(s)) || (j.policyCheck && T.sources.some((_, si) => { const p = pv(si, r.ci); return p && p.s === 'blocked'; })));
      const shown = rows.slice(0, T.limit);
      rowsHtml = `<div class="ct-scroll"><table class="ct-matrix"><thead><tr><th class="sticky">Target</th><th>Result</th>${T.sources.map((s, si) => `<th class="src" title="${esc(`${s.pod}\nnode ${s.node}\npod IP ${s.podIP}\nhost IP ${s.hostIP}${s.hostNetwork ? '\nhost network' : ''}`)}"><span>${esc(s.node || s.pod)}</span>${s.hostNetwork ? '<small>hostNet</small>' : ''}${s.state === 'noshell' || s.state === 'notool' ? `<small>${esc(s.state)}</small>` : ''}</th>`).join('')}</tr></thead><tbody>
        ${shown.map(r => `<tr><td class="sticky mono">${esc(r.c.label)}${ipsNote(r.ci)}</td><td class="ct-verdict">${chipsHtml(r.counts)}${policyNote(r.ci)}</td>${T.sources.map((_, si) => `<td>${cellHtml(si, r.ci)}</td>`).join('')}</tr>`).join('')}
        </tbody></table></div>${rows.length > shown.length ? `<div style="padding:10px"><button class="btn sm" id="ctMore">Show ${Math.min(300, rows.length - shown.length)} more of ${rows.length - shown.length}</button></div>` : ''}
        ${!rows.length ? '<div class="muted" style="padding:14px">Nothing to show with this filter.</div>' : ''}`;
    } else {
      rowsHtml = `<div class="ct-scroll"><table class="ct-matrix"><thead><tr><th class="sticky">Pod</th><th>Node / IPs</th><th>Result</th><th>Network policies selecting this pod</th></tr></thead><tbody>
        ${T.sources.map((s, si) => {
          const counts = countsFor(T.checks.map((_, ci) => [si, ci]));
          const done = T.checks.map((c, ci) => ({ c, ci, r: T.res.get(rkey(si, ci)) })).filter(x => x.r);
          const okish = x => ['open', 'refused', 'tls'].includes(x.r.state);
          const bad = done.filter(x => !okish(x));
          const rank = { timeout: 0, unreachable: 1, dns: 2, error: 3, notool: 4, noshell: 5, refused: 6, tls: 7, open: 8 };
          const list = (T.onlyProblems ? bad : done).slice().sort((a, b) => (rank[a.r.state] ?? 9) - (rank[b.r.state] ?? 9) || a.ci - b.ci);
          return `<tr><td class="sticky mono">${esc(s.pod)}${s.container ? `<div class="muted">${esc(s.container)}${s.tool ? ' · ' + esc(s.tool) : ''}</div>` : ''}</td>
            <td><div>${esc(s.node)}</div><div class="muted mono">pod ${esc(s.podIP)} · host ${esc(s.hostIP)}${s.hostNetwork ? ' · host network' : ''}</div></td>
            <td class="ct-verdict">${chipsHtml(counts)}${list.length ? `<details class="ct-det" ${T.openPods.has(si) ? 'open' : ''} data-si="${si}"><summary>${T.onlyProblems ? `${bad.length} not open` : `All ${done.length} result${done.length === 1 ? '' : 's'}${bad.length ? ` · ${bad.length} not open` : ' · all reachable'}`}</summary>${list.slice(0, 80).map(x => { const m = CT_STATES[x.r.state] || CT_STATES.error; return `<div class="ct-detrow"><button class="linkish" data-s="${si}" data-c="${x.ci}">${esc(x.c.label)}</button>${Array.isArray(x.r.ips) && x.r.ips.length ? ` <span class="muted mono">(${esc(ipList(x.r.ips, 3))})</span>` : ''} <span class="ct-st ${m.cls}">${esc(m.label)}${x.r.http ? ' · HTTP ' + x.r.http : ''}</span></div>`; }).join('')}${list.length > 80 ? `<div class="muted">+${list.length - 80} more (use the CSV)</div>` : ''}</details>` : ''}</td>
            <td>${(s.policies || []).length ? (s.policies || []).map(n => `<span class="chip">${esc(n)}</span>`).join(' ') : `<span class="muted">${j.policyCheck ? 'none' : 'policy check is off'}</span>`}</td></tr>`;
        }).join('')}</tbody></table></div>`;
    }
    out.innerHTML = `
      <div class="card ct-res">
        <div class="card-head"><h3>${esc(stateName)}</h3><span class="cap">${j.done}/${j.total} probes · ${nC} check${nC === 1 ? '' : 's'} from ${nS} pod${nS === 1 ? '' : 's'}${j.timeout ? ` · ${j.timeout}s timeout` : ''}</span>
          <span class="right ct-tools">
            <div class="seg"><button type="button" data-v="target" class="${T.view === 'target' ? 'on' : ''}">By target</button><button type="button" data-v="pod" class="${T.view === 'pod' ? 'on' : ''}">By pod</button></div>
            <label class="chk"><input type="checkbox" id="ctProb" ${T.onlyProblems ? 'checked' : ''}> Only problems</label>
            <button class="btn sm" id="ctCopy" ${nFail ? '' : 'disabled'} title="Plain text you can paste into a firewall request">Copy for firewall request</button>
            <button class="btn sm" id="ctCsv">CSV</button>
            <button class="btn sm" id="ctRerun" ${nRetry ? '' : 'disabled'} title="Run the failed checks again from the pods that could run probes">Re-run failed</button>
          </span></div>
        <div class="ct-bar"><i style="width:${pct}%"></i></div>
        <div class="ct-sum">${chipsHtml(totals)}${failPol ? `<span class="ct-chip crit" title="Failures where a network policy also blocks the connection">${failPol} explained by network policy</span>` : ''}${openPol ? `<span class="ct-chip warn" title="The connection worked although the policy objects say it should be blocked: the policy may not be enforced by this cluster's network plugin, or the verdict is wrong for this target">${openPol} open despite a blocking policy</span>` : ''}</div>
        ${T.warnings.map(w => `<div class="banner info" style="margin:10px 0 0"><span>${esc(w)}</span></div>`).join('')}
        ${j.policyCheck ? '' : '<div class="muted" style="margin-top:8px;font-size:12.5px">Network policy check is off for this run.</div>'}
        ${rowsHtml}
        <details class="ct-legend"><summary>What the results mean</summary>
          <div>${Object.values(CT_STATES).map(m => `<div><span class="ct-chip ${m.cls}">${esc(m.label)}</span> ${esc(m.tip)}</div>`).join('')}
          <div><i class="ct-pol blocked"></i> Network policy says the connection is blocked. <i class="ct-pol unknown"></i> The policy check cannot decide (named ports, hostnames, rules that depend on DNS).</div>
          <div class="muted">A firewall sees the pod IP when pod traffic is routed without SNAT (for example Calico BGP without natOutgoing); otherwise it sees the node IP. Host-network pods always use the node IP. A policy verdict is read from policy objects; the probe result is what actually happened.</div></div></details>
      </div>`;
    $$('#ctOut .seg button').forEach(b => b.addEventListener('click', () => { T.view = b.dataset.v; drawResults(); }));
    $('#ctProb').addEventListener('change', e => { T.onlyProblems = e.target.checked; drawResults(); });
    $('#ctMore')?.addEventListener('click', () => { T.limit += 300; drawResults(); });
    $('#ctCopy').addEventListener('click', copyFirewall);
    $('#ctCsv').addEventListener('click', downloadCsv);
    $('#ctRerun').addEventListener('click', rerunFailed);
    $$('#ctOut [data-s][data-c]').forEach(b => b.addEventListener('click', () => detail(+b.dataset.s, +b.dataset.c)));
    $$('#ctOut details.ct-det[data-si]').forEach(d => d.addEventListener('toggle', () => { if (d.open) T.openPods.add(+d.dataset.si); else T.openPods.delete(+d.dataset.si); }));
  }

  function detail(si, ci) {
    const s = T.sources[si], c = T.checks[ci], r = T.res.get(rkey(si, ci)), p = pv(si, ci);
    const m = r ? (CT_STATES[r.state] || CT_STATES.error) : null;
    const row = (k, v) => v ? `<tr><td class="muted" style="padding-right:14px">${k}</td><td>${v}</td></tr>` : '';
    modal({ title: c.label, wide: true, body: `<table class="kv">
        ${row('Result', r ? `<span class="ct-chip ${m.cls}">${esc(m.label)}</span> ${esc(m.tip)}` : 'Waiting for the probe…')}
        ${row('Resolved to', r && Array.isArray(r.ips) ? (r.ips.length ? `<span class="mono">${esc(r.ips.join(', '))}</span> <span class="muted">(inside this pod)</span>` : 'no address (the name did not resolve inside this pod)') : '')}
        ${row('Connected to', r && r.via ? `<span class="mono">${esc(r.via)}</span>` : '')}
        ${row('Details', r && r.detail ? `<span class="mono">${esc(r.detail)}</span>` : '')}
        ${row('Tool', r && r.tool ? esc(r.tool) + (r.ms ? ` · ${r.ms} ms` : '') : '')}
        ${row('HTTP status', r && r.http ? r.http : '')}
        ${row('From pod', `<span class="mono">${esc(s.pod)}</span>${s.container ? ' · ' + esc(s.container) : ''}`)}
        ${row('Node', esc(s.node))}${row('Pod IP', `<span class="mono">${esc(s.podIP)}</span>`)}${row('Host IP', `<span class="mono">${esc(s.hostIP)}</span>`)}
        ${row('Network policy', p ? `<span class="ct-chip ${p.s === 'blocked' ? 'crit' : p.s === 'allowed' ? 'good' : 'warn'}">${esc(p.s)}</span> ${esc(p.w || '')}` : (T.job.policyCheck ? 'No policy applies to this pod’s egress.' : 'Not checked.'))}
        ${row('Note', r && r.state === 'open' && p && p.s === 'blocked' ? 'The connection worked although the policy objects say it should be blocked. The policy may not be enforced by this cluster’s network plugin, or the verdict does not fit this target (for example traffic to a Service IP is matched after translation).' : '')}
        ${row('Policies on this pod', (s.policies || []).map(n => `<span class="chip">${esc(n)}</span>`).join(' '))}
      </table>`, actions: [{ label: 'Close', value: true }] });
  }

  const targetText = c => c.mode === 'http' ? c.url : `${c.host}:${c.port}`;
  function failing() {
    const rows = [];
    T.sources.forEach((s, si) => T.checks.forEach((c, ci) => { const r = T.res.get(rkey(si, ci)); if (r && CT_FAIL.has(r.state)) rows.push({ s, si, c, ci, r, p: pv(si, ci) }); }));
    return rows;
  }
  async function copyFirewall() {
    const fl = failing();
    const lines = fl.map(x => `SRC node=${x.s.node} hostIP=${x.s.hostIP} podIP=${x.s.podIP} pod=${x.s.pod}  ->  ${x.c.label}${Array.isArray(x.r.ips) && x.r.ips.length ? ` [${x.r.ips.join(', ')}]` : ''}  ${x.r.state.toUpperCase()}${x.p && x.p.s === 'blocked' ? '  (network policy also blocks it)' : ''}`);
    const dst = [...new Set(fl.flatMap(x => Array.isArray(x.r.ips) && x.r.ips.length ? x.r.ips.map(ip => `${ip}:${x.c.port}`) : (/^[\d.]+$|:/.test(x.c.host) ? [`${x.c.host}:${x.c.port}`] : [])))];
    const srcs = [...new Set(fl.map(x => x.s.hostIP))];
    const txt = `Connectivity test from namespace ${T.job.ns} (cluster ${shortName(ctx)}), ${new Date().toISOString()}\nTimeout ${T.job.timeout}s. Failing connections:\n` + lines.join('\n') +
      (dst.length ? `\n\nSource node IPs: ${srcs.join(', ')}\nDestination IP:port: ${dst.join(', ')}` : '');
    try { await navigator.clipboard.writeText(txt); toast('Copied ' + lines.length + ' line' + (lines.length === 1 ? '' : 's')); } catch { toast('Could not copy: select the text from the CSV instead', 'err'); }
  }
  function downloadCsv() {
    const q = v => '"' + String(v ?? '').replace(/"/g, '""') + '"';
    const rows = [['target', 'resolved_ips', 'connected_ip', 'pod', 'node', 'pod_ip', 'host_ip', 'result', 'tool', 'ms', 'http', 'detail', 'policy', 'policy_detail'].map(q).join(',')];
    T.sources.forEach((s, si) => T.checks.forEach((c, ci) => {
      const r = T.res.get(rkey(si, ci)) || {}, p = pv(si, ci) || {};
      rows.push([c.label, Array.isArray(r.ips) ? r.ips.join(' ') : '', r.via, s.pod, s.node, s.podIP, s.hostIP, r.state || 'pending', r.tool, r.ms, r.http, r.detail, p.s, p.w].map(q).join(','));
    }));
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([rows.join('\n')], { type: 'text/csv' }));
    a.download = `connectivity-${T.job.ns}-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.csv`;
    a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 2000);
  }
  function rerunFailed() {
    const bad = [];
    T.sources.forEach((s, si) => T.checks.forEach((c, ci) => { const r = T.res.get(rkey(si, ci)); if (r && ['timeout', 'unreachable', 'dns', 'error'].includes(r.state)) bad.push({ s, c }); }));
    if (!bad.length) return;
    F.targets = [...new Set(bad.map(x => targetText(x.c)))].join('\n'); F.ports = '';
    F.mode = 'pick'; F.pods = [...new Set(bad.map(x => x.s.pod))];
    $('#ctTargets').value = F.targets; $('#ctPorts').value = '';
    persist(); drawMode();
    preview().then(() => { if (valid) start(); });
  }

  // ---- wiring
  $$('#ctMode button').forEach(b => b.addEventListener('click', () => { F.mode = b.dataset.m; persist(); drawMode(); schedulePreview(); }));
  ['#ctTargets', '#ctPorts'].forEach(s => $(s).addEventListener('input', () => { read(); schedulePreview(); }));
  ['#ctPol', '#ctTimeout', '#ctPar'].forEach(s => $(s).addEventListener('change', read));
  $('#ctNs').addEventListener('change', async () => { read(); F.pods = []; await loadPods(); schedulePreview(); });
  $('#ctRun').addEventListener('click', start);
  $('#ctCancel').addEventListener('click', async () => { try { await ctSend('DELETE', 'tools/conntest/' + T.job.id); } catch (e) { toast(e.message, 'err'); } });
  drawMode();
  await loadPods();
  preview();
  // re-attach to a test that was running when the page was left
  try { const id = sessionStorage.getItem(jobKey); if (id) attach(id); } catch {}
}

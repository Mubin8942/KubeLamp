'use strict';
/* Tools → Networking → DNS Checker: resolve names from inside pods and compare what each pod gets. */

const DNS_STATES = {
  ok: { label: 'Resolved', cls: 'good' },
  notfound: { label: 'Not found (NXDOMAIN)', cls: 'crit' },
  timeout: { label: 'DNS timeout', cls: 'crit' },
  servfail: { label: 'Server error (SERVFAIL/refused)', cls: 'crit' },
  error: { label: 'Error', cls: 'grey' },
  notool: { label: 'No DNS tool', cls: 'grey' },
  noshell: { label: 'No shell', cls: 'grey' },
};

async function viewDnsCheck(route) {
  const ctx = route.ctx;
  const key = 'kl.dns.' + ctx;
  const roOnly = !!(S.info && S.info.readOnly);
  const saved = (() => { try { return JSON.parse(localStorage.getItem(key) || '{}'); } catch { return {}; } })();
  const F = Object.assign({ ns: S.ns || '', mode: 'per-node', pods: [], names: '', expect: '', server: '', timeout: 3 }, saved);
  let nss = [], allPods = [], R = null, running = false;
  try { nss = await api('namespaces?ctx=' + enc(ctx)); } catch {}
  if (!F.ns || !nss.some(n => n.name === F.ns)) F.ns = (S.ns && nss.some(n => n.name === S.ns) ? S.ns : (nss.find(n => n.pods) || nss[0] || {}).name) || '';

  main.innerHTML = `
    <div class="page-head"><div><div class="crumbs"><a href="${ctxPath(ctx)}/overview">${esc(shortName(ctx))}</a> / <a href="${ctxPath(ctx)}/tools">Tools</a></div>
      <h1>DNS Checker</h1>
      <div class="sub"><span>Resolves names from inside your pods, so the answer is what the application sees: the pod's resolv.conf, search domains, cluster DNS and the node's upstream servers.</span></div></div></div>
    ${roOnly ? `<div class="banner err"><b>Read-only mode.</b><span>This tool runs commands inside pods, so it is disabled.</span></div>` : ''}
    <div class="card ct-form">
      <div class="ct-row">
        <label class="ct-f"><span>Namespace</span><select id="dnNs">${nss.map(n => `<option value="${esc(n.name)}" ${n.name === F.ns ? 'selected' : ''}>${esc(n.name)}${n.pods ? ` (${n.pods})` : ''}</option>`).join('')}</select></label>
        <div class="ct-f"><span>Test from</span>
          <div class="seg" id="dnMode"><button type="button" data-m="per-node">One pod per node</button><button type="button" data-m="all">All running pods</button><button type="button" data-m="pick">Pick pods</button></div></div>
      </div>
      <div id="dnPick" class="ct-pick" hidden></div>
      <label class="ct-f"><span>Host names <small class="muted">separate with commas, spaces or new lines. Short names like <span class="mono">kubernetes.default</span> use the pod's search domains</small></span>
        <textarea id="dnNames" rows="3" spellcheck="false" placeholder="api.example.com, db.internal, kubernetes.default">${esc(F.names)}</textarea></label>
      <div class="ct-row">
        <label class="ct-f" style="flex:1;min-width:260px"><span>Expected IPs or CIDRs <small class="muted">optional: answers outside this list are flagged</small></span>
          <input type="text" id="dnExpect" spellcheck="false" placeholder="10.1.1.5, 10.1.2.0/24" value="${esc(F.expect)}"></label>
        <label class="ct-f" style="min-width:200px"><span>DNS server <small class="muted">optional: ask this server instead of the pod's own</small></span>
          <input type="text" id="dnServer" spellcheck="false" placeholder="10.96.0.10" value="${esc(F.server)}"></label>
        <label class="ct-inl">Timeout <select id="dnTimeout">${[1, 2, 3, 5, 8, 10].map(v => `<option ${v === +F.timeout ? 'selected' : ''} value="${v}">${v}s</option>`).join('')}</select></label>
      </div>
      <div id="dnPrev" class="ct-prev muted"></div>
      <div class="ct-actions"><button class="btn primary" id="dnRun" ${roOnly ? 'disabled' : ''}>Resolve</button></div>
    </div>
    <div id="dnOut"></div>`;

  const persist = () => { try { localStorage.setItem(key, JSON.stringify(F)); } catch {} };
  const read = () => { F.ns = $('#dnNs').value; F.names = $('#dnNames').value; F.expect = $('#dnExpect').value; F.server = $('#dnServer').value; F.timeout = +$('#dnTimeout').value; persist(); };
  const drawMode = () => {
    $$('#dnMode button').forEach(b => b.classList.toggle('on', b.dataset.m === F.mode));
    const pk = $('#dnPick');
    pk.hidden = F.mode !== 'pick';
    if (F.mode === 'pick') {
      pk.innerHTML = allPods.length ? allPods.map(p => `<label class="chk"><input type="checkbox" value="${esc(p.pod)}" ${F.pods.includes(p.pod) ? 'checked' : ''}> <span class="mono">${esc(p.pod)}</span> <span class="muted">${esc(p.node)}</span></label>`).join('') : '<span class="muted">No running pods in this namespace.</span>';
      $$('input', pk).forEach(i => i.addEventListener('change', () => { F.pods = $$('input:checked', pk).map(x => x.value); persist(); }));
    }
  };
  async function loadPods() {
    allPods = [];
    try { const d = await ctSend('POST', 'tools/conntest/preview?ctx=' + enc(ctx), { ns: F.ns, podMode: 'all', targets: '', ports: '' }); allPods = d.sources || []; } catch {}
    F.pods = F.pods.filter(p => allPods.some(a => a.pod === p));
    drawMode();
    const nodes = new Set(allPods.map(p => p.node)).size;
    $('#dnPrev').textContent = allPods.length ? `${allPods.length} running pod${allPods.length === 1 ? '' : 's'} on ${nodes} node${nodes === 1 ? '' : 's'} in this namespace.` : 'No running pods in this namespace to test from.';
  }

  // ---- results
  const chip = (cls, text, title = '') => `<span class="ct-chip ${cls}" ${title ? `title="${esc(title)}"` : ''}>${esc(text)}</span>`;
  function draw() {
    const out = $('#dnOut');
    if (running) { out.innerHTML = '<div class="card"><div class="ct-bar"><i style="width:100%;animation:none;opacity:.5"></i></div><span class="muted">Resolving from inside the pods…</span></div>'; return; }
    if (!R) { out.innerHTML = ''; return; }
    const bad = new Set(R.unexpected || []);
    const srcs = R.sources;
    const tested = srcs.map((s, i) => i).filter(i => srcs[i].state === 'done');
    const skipped = srcs.filter(s => s.state !== 'done');
    const verdicts = [];
    const cards = R.names.map(name => {
      const rs = R.results.filter(r => r.name === name && tested.includes(r.src));
      const groups = new Map();
      for (const r of rs) {
        const k = r.state + '|' + r.ips.join(',') + '|' + (r.cnames || []).join(',');
        if (!groups.has(k)) groups.set(k, { r, srcs: [] });
        groups.get(k).srcs.push(r.src);
      }
      const gs = [...groups.values()].sort((a, b) => (b.r.state === 'ok') - (a.r.state === 'ok') || b.srcs.length - a.srcs.length);
      const okG = gs.filter(g => g.r.state === 'ok'), failN = gs.filter(g => g.r.state !== 'ok').reduce((n, g) => n + g.srcs.length, 0);
      let v;
      if (!rs.length) v = chip('grey', 'Not tested');
      else if (!okG.length) v = chip('crit', 'Does not resolve');
      else if (okG.length === 1 && !failN) v = chip('good', rs.length > 1 ? `Same answer on all ${rs.length} pods` : 'Resolved');
      else if (okG.length > 1) v = chip('warn', 'Different answers on different pods');
      else v = chip('warn', `Fails on ${failN} of ${rs.length} pods`);
      const unexp = okG.some(g => g.r.ips.some(ip => bad.has(ip)));
      verdicts.push({ name, ok: okG.length === 1 && !failN && !unexp, text: gs.map(g => `${g.r.state === 'ok' ? g.r.ips.join(', ') : (DNS_STATES[g.r.state] || {}).label || g.r.state} (${g.srcs.map(i => srcs[i].pod).join(', ')})`).join(' | ') });
      return `<div class="card dn-card"><div class="dn-head"><span class="mono dn-name">${esc(name)}</span>${v}${unexp ? chip('crit', 'Unexpected IP in answer') : ''}</div>
        ${gs.map(g => {
          const st = DNS_STATES[g.r.state] || DNS_STATES.error;
          const ips = g.r.state === 'ok'
            ? g.r.ips.map(ip => `<span class="chip ${bad.has(ip) ? 'dn-bad' : R.expect ? 'dn-good' : ''}">${esc(ip)}</span>`).join(' ') + ((g.r.cnames || []).length ? ` <span class="muted">via ${esc(g.r.cnames.join(' → '))}</span>` : '')
            : `${chip(st.cls, st.label)} <span class="muted dn-det">${esc(g.r.detail || '')}</span>`;
          const nodes = g.srcs.map(i => srcs[i]);
          return `<div class="dn-row"><div class="dn-ans">${ips}</div>
            <div class="dn-pods muted" title="${esc(nodes.map(s => s.pod + ' (' + s.node + ')').join('\n'))}">${g.srcs.length} pod${g.srcs.length === 1 ? '' : 's'}: ${esc(nodes.slice(0, 4).map(s => s.node).join(', '))}${nodes.length > 4 ? ` +${nodes.length - 4}` : ''} <span class="mono">${esc(g.r.tool)}</span></div></div>`;
        }).join('')}</div>`;
    }).join('');

    // resolv.conf grouped by identical settings
    const cfg = new Map();
    for (const i of tested) {
      const rv = srcs[i].resolv || {};
      const k = JSON.stringify([rv.nameservers, rv.search, rv.options]);
      if (!cfg.has(k)) cfg.set(k, { rv, pods: [] });
      cfg.get(k).pods.push(srcs[i]);
    }
    const cfgHtml = [...cfg.values()].map(c => `<div class="dn-cfg"><div><b>nameserver</b> ${(c.rv.nameservers || []).map(n => `<span class="chip">${esc(n)}</span>`).join(' ') || '<span class="muted">none</span>'}
      <b style="margin-left:10px">search</b> ${(c.rv.search || []).map(n => `<span class="chip">${esc(n)}</span>`).join(' ') || '<span class="muted">none</span>'}
      <b style="margin-left:10px">options</b> ${(c.rv.options || []).map(n => `<span class="chip">${esc(n)}</span>`).join(' ') || '<span class="muted">none</span>'}</div>
      <div class="muted dn-pods">${c.pods.length} pod${c.pods.length === 1 ? '' : 's'} (${esc(c.pods.slice(0, 4).map(p => p.node).join(', '))}${c.pods.length > 4 ? ` +${c.pods.length - 4}` : ''})</div></div>`).join('');

    out.innerHTML = `
      <div class="card ct-res"><div class="ct-top"><div><b>${R.names.length}</b> name${R.names.length === 1 ? '' : 's'} from <b>${tested.length}</b> pod${tested.length === 1 ? '' : 's'}${R.server ? ` · asked server <span class="mono">${esc(R.server)}</span>` : ''}</div>
        <div class="ct-tools"><button class="btn sm" id="dnCopy">Copy summary</button></div></div>
        ${R.expect ? `<div class="muted" style="margin-top:6px">Expected: <span class="mono">${esc(R.expect)}</span>. ${bad.size ? `<span class="crit-t">${bad.size} answered IP${bad.size === 1 ? ' is' : 's are'} outside it: ${[...bad].map(esc).join(', ')}</span>` : 'Every answer is inside it.'}</div>` : ''}
        ${skipped.length ? `<div class="ct-prev err" style="margin-top:10px"><b>Could not test from ${skipped.length} pod${skipped.length === 1 ? '' : 's'}:</b> ${skipped.map(s => `<span class="mono">${esc(s.pod)}</span> (${esc(s.err || s.state)})`).join(', ')}</div>` : ''}
      </div>
      ${cards}
      <div class="card"><h3 style="margin:0 0 8px">DNS settings inside the pods</h3>${cfgHtml || '<span class="muted">Not available.</span>'}
        <div class="muted" style="margin-top:8px;font-size:12.5px">With <span class="mono">ndots:5</span>, a name with fewer than five dots is first tried with every search domain, which can add delay and extra lookups for external names. Ending a name with a dot (<span class="mono">api.example.com.</span>) skips the search list.</div></div>`;
    $('#dnCopy').onclick = async () => {
      const txt = `DNS check from namespace ${F.ns} (cluster ${shortName(ctx)}), ${new Date().toISOString()}\n` + verdicts.map(v => `${v.ok ? 'OK ' : 'ISSUE'} ${v.name}: ${v.text}`).join('\n');
      try { await navigator.clipboard.writeText(txt); toast('Copied'); } catch { toast('Could not copy', 'err'); }
    };
  }

  async function run() {
    read();
    if (!F.names.trim()) { toast('Enter at least one host name', 'err'); return; }
    running = true; R = null; $('#dnRun').disabled = true; draw();
    try {
      R = await ctSend('POST', 'tools/dns?ctx=' + enc(ctx), { ns: F.ns, podMode: F.mode, pods: F.pods, names: F.names, expect: F.expect, server: F.server, timeout: F.timeout });
    } catch (e) { toast(e.message, 'err'); R = null; }
    running = false; $('#dnRun').disabled = roOnly; draw();
  }

  $$('#dnMode button').forEach(b => b.onclick = () => { F.mode = b.dataset.m; persist(); drawMode(); });
  $('#dnNs').onchange = () => { read(); F.pods = []; R = null; draw(); loadPods(); };
  ['#dnNames', '#dnExpect', '#dnServer', '#dnTimeout'].forEach(s => $(s).addEventListener('change', read));
  $('#dnRun').onclick = run;
  drawMode();
  await loadPods();
}

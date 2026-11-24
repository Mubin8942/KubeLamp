'use strict';
/* Tools → Alerting: rules, active alerts, history and (admins) the SMS / email channels.
   Admins make and change rules; everyone else sees them read-only, without the phone
   numbers and email addresses. Rules are checked on the server every 15 s. */

const AL_SEV = { critical: ['Critical', 'critical'], warning: ['Warning', 'warning'], info: ['Info', 'good'] };
const AL_VARS = ['status', 'severity', 'rule', 'summary', 'description', 'cluster', 'namespace', 'object', 'count', 'list', 'condition', 'time', 'since'];
const AL_SMS_VARS = ['to', 'to_plain', 'message', 'sender', 'username', 'password', 'service_id', 'fa', 'present_id', 'link_id', 'correlator', 'wsse_digest', 'wsse_nonce', 'wsse_created'];
const AL_REPEAT = [[0, 'Off'], [15, 'Every 15 min'], [30, 'Every 30 min'], [60, 'Every hour'], [120, 'Every 2 hours'], [240, 'Every 4 hours'], [720, 'Every 12 hours'], [1440, 'Every day']];
const alSev = s => { const [l, c] = AL_SEV[s] || [s, '']; return `<span class="st ${c}">${esc(l)}</span>`; };
const alDur = sec => !sec ? 'at once' : sec % 3600 === 0 ? `${sec / 3600} h` : sec % 60 === 0 ? `${sec / 60} min` : `${sec} s`;

async function viewAlerts(route) {
  const ctx = route.ctx;
  let D = null, tab = 'active', hist = null, chans = null, editing = null;
  try { tab = localStorage.getItem('kl.alerts.tab') || 'active'; } catch {}
  const load = async () => { D = await api('alerts'); if (!D.canEdit && tab === 'channels') tab = 'active'; };
  try { await load(); } catch (e) {
    main.innerHTML = `<div class="empty-state"><h2>Alerting is not available</h2><p>${esc(e.message)}</p></div>`;
    return;
  }
  const cond = id => D.catalog.find(c => c.id === id) || { label: id, target: '' };
  const canEdit = () => D.canEdit;

  function head() {
    const firing = D.active.filter(a => a.state === 'firing').length;
    return `<div class="page-head"><div><div class="crumbs">Tools</div><h1>Alerting</h1>
      <div class="sub"><span>${D.rules.length} rule${D.rules.length === 1 ? '' : 's'}</span><span>${firing ? stLabel(firing + ' firing', 'critical') : stLabel('nothing firing', 'good')}</span>
      <span class="muted">checked every 15 s · times in ${esc(D.zone || 'server time')}</span></div></div>
      <div class="right">${canEdit() ? `<button class="btn primary" id="alNew">${ICON.scale}New rule</button>` : ''}</div></div>
      ${!canEdit() ? '<div class="banner info"><b>View only.</b><span>Only admins can create or change alerts.</span></div>' : ''}
      ${canEdit() && !D.channels.sms && !D.channels.email ? '<div class="banner"><b>Nothing can be sent yet.</b><span>Set up SMS or email on the <a href="#" data-tab-go="channels">Channels</a> tab; until then alerts only show here.</span></div>' : ''}
      <div class="tabs" id="alTabs">${[['active', `Active${D.active.length ? ` (${D.active.length})` : ''}`], ['rules', `Rules (${D.rules.length})`], ['history', 'History'], ...(canEdit() ? [['channels', 'Channels']] : [])]
        .map(([k, l]) => `<a data-tab="${k}" class="${tab === k ? 'on' : ''}">${esc(l)}</a>`).join('')}</div>
      <div id="alBody"></div>`;
  }

  function render() {
    if (editing) return;
    main.innerHTML = head();
    $$('#alTabs a, [data-tab-go]').forEach(a => a.addEventListener('click', async e => {
      e.preventDefault();
      tab = a.dataset.tab || a.dataset.tabGo;
      try { localStorage.setItem('kl.alerts.tab', tab); } catch {}
      try { await load(); } catch {} // fresh counts and alerts for the tab
      render();
    }));
    $('#alNew')?.addEventListener('click', () => openEditor(null));
    ({ active: drawActive, rules: drawRules, history: drawHistory, channels: drawChannels })[tab]();
  }

  // ---------------------------------------------------------------- active alerts
  function drawActive() {
    const box = $('#alBody');
    box.innerHTML = `<div class="filters"><select id="alSevF" data-label="Severity"><option value="">All</option><option value="critical">Critical</option><option value="warning">Warning</option><option value="info">Info</option></select>
      <select id="alStF" data-label="State"><option value="">All</option><option value="firing">Firing</option><option value="pending">Pending</option><option value="silenced">Silenced</option></select>
      <input type="search" id="alQ" placeholder="Filter by rule, namespace or object" style="width:300px"><span class="count" id="alCount"></span></div>
      <div class="card flush" id="alTbl"></div>`;
    const draw = () => {
      const q = $('#alQ').value.toLowerCase(), sv = $('#alSevF').value, sf = $('#alStF').value;
      const rows = D.active.filter(a => (!sv || a.severity === sv) && (!sf || (sf === 'silenced' ? a.silenced : a.state === sf && !a.silenced))
        && (!q || [a.ruleName, a.namespace, a.object, a.summary, a.cluster].join(' ').toLowerCase().includes(q)));
      const cols = [
        { key: 'severity', label: 'Severity', sort: a => ['critical', 'warning', 'info'].indexOf(a.severity), render: a => alSev(a.severity) },
        { key: 'ruleName', label: 'Alert', cls: 'al-what', render: a => `<b>${esc(a.ruleName)}</b><div class="muted">${esc(a.summary)}</div>` },
        { key: 'object', label: 'Object', render: a => `<span class="mono">${esc((a.namespace ? a.namespace + '/' : '') + a.object)}</span>` },
        { key: 'cluster', label: 'Cluster', render: a => esc(shortName(a.cluster)) },
        { key: 'since', label: 'State', sort: a => -new Date(a.firedAt || a.since), render: a => a.silenced ? stLabel('silenced', 'serious')
          : a.state === 'firing' ? `${stLabel('firing', 'critical')} <span class="muted">${age(a.firedAt)}</span>` : `${stLabel('pending', 'warning')} <span class="muted">${age(a.since)}</span>` },
      ];
      if (canEdit()) cols.push({ key: 'act', label: '', nosort: true, cls: 'act', render: () => `<button class="icon-btn" data-menu title="Silence">${ICON.more}</button>` });
      const t = table('al-active', cols, rows, { sort: { key: 'severity', dir: 'asc' } });
      $('#alTbl').innerHTML = D.active.length ? t.html : '<div class="empty-state" style="padding:40px 20px"><h2>Nothing is firing</h2><p>Alerts show here while a rule matches.</p></div>';
      t.bind($('#alTbl'), draw);
      $('#alCount').textContent = `${rows.length} of ${D.active.length}`;
      $$('[data-menu]', $('#alTbl')).forEach(b => b.addEventListener('click', e => {
        e.stopPropagation();
        const a = t.rows[+b.closest('tr').dataset.i];
        const sil = D.silences.find(s => s.rule === a.rule && (!s.key || s.key === a.key));
        openPop(b, sil ? [{ label: 'Remove silence', run: () => unsilence(sil.id) }]
          : [{ head: 'Silence this alert' }, ...[[60, '1 hour'], [240, '4 hours'], [1440, '1 day'], [0, 'Until it clears']].map(([m, l]) => ({ label: l, run: () => silence(a.rule, a.key, m) })),
            'sep', { label: 'Silence the whole rule for 1 hour', run: () => silence(a.rule, '', 60) }]);
      }));
    };
    ['#alSevF', '#alStF'].forEach(s => $(s).addEventListener('change', draw));
    $('#alQ').addEventListener('input', draw);
    draw();
  }

  async function silence(rule, key, minutes) {
    try { await send('POST', 'alerts/silences', { rule, key, minutes }); toast(minutes ? `Silenced for ${minutes >= 60 ? minutes / 60 + ' h' : minutes + ' min'}` : 'Silenced until it clears'); await refresh(); } catch (e) { toast(e.message, 'err'); }
  }
  async function unsilence(id) {
    try { await send('DELETE', 'alerts/silences/' + enc(id)); toast('Silence removed'); await refresh(); } catch (e) { toast(e.message, 'err'); }
  }

  // ---------------------------------------------------------------- rules
  function scopeText(r) {
    const c = cond(r.condition), p = [];
    if (c.target === 'cluster') return 'whole cluster';
    if (!['node'].includes(c.target)) p.push(r.namespaces?.length ? r.namespaces.join(', ') : 'all namespaces');
    if (r.kind) p.push(r.kind);
    if (r.workload) p.push(r.workload);
    if (r.labels) p.push(r.labels);
    if (r.match) p.push(`“${r.match}”`);
    return p.join(' · ');
  }
  function condText(r) {
    const c = cond(r.condition);
    let s = c.label;
    if (c.threshold) s += ` ≥ ${r.threshold}${c.threshold.unit === '%' ? '%' : ' ' + c.threshold.unit}`;
    if (c.window) s += ` · ${r.window} min`;
    if (r.for) s += ` · for ${alDur(r.for)}`;
    return s;
  }
  function drawRules() {
    const box = $('#alBody');
    if (!D.rules.length) {
      box.innerHTML = `<div class="empty-state"><h2>No alert rules yet</h2><p>${canEdit() ? 'A rule watches a cluster for a problem (a crashing pod, a full volume, a node going down…) and sends an SMS or email when it happens.' : 'An admin has not set up any alerts.'}</p>
        ${canEdit() ? `<button class="btn primary" id="alNew2">${ICON.scale}New rule</button>` : ''}</div>`;
      $('#alNew2')?.addEventListener('click', () => openEditor(null));
      return;
    }
    box.innerHTML = `<div class="filters"><input type="search" id="alRQ" placeholder="Filter rules" style="width:300px"><span class="count" id="alRCount"></span></div><div class="card flush" id="alRTbl"></div>`;
    const draw = () => {
      const q = $('#alRQ').value.toLowerCase();
      const rows = D.rules.filter(r => !q || [r.name, r.description, r.cluster, cond(r.condition).label, scopeText(r)].join(' ').toLowerCase().includes(q));
      const cols = [
        { key: 'enabled', label: 'On', sort: r => r.enabled ? 0 : 1, render: r => `<label class="sw" title="${canEdit() ? 'Turn this rule on or off' : ''}"><input type="checkbox" data-on ${r.enabled ? 'checked' : ''} ${canEdit() ? '' : 'disabled'}><i></i></label>` },
        { key: 'name', label: 'Rule', cls: 'al-what', render: r => `<b>${esc(r.name)}</b>${r.description ? `<div class="muted">${esc(r.description.slice(0, 120))}</div>` : ''}` },
        { key: 'condition', label: 'Watches', render: r => `${esc(condText(r))}<div class="muted">${esc(scopeText(r))}</div>` },
        { key: 'cluster', label: 'Cluster', render: r => esc(shortName(r.cluster)) },
        { key: 'severity', label: 'Severity', sort: r => ['critical', 'warning', 'info'].indexOf(r.severity), render: r => alSev(r.severity) },
        { key: 'notify', label: 'Sends to', nosort: true, render: r => [r.smsCount ? `${r.smsCount} SMS` : '', r.emailCount ? `${r.emailCount} email` : ''].filter(Boolean).join(' · ') || '<span class="muted">page only</span>' },
        { key: 'status', label: 'Now', sort: r => -(r.firing * 1000 + r.pending), render: r => !r.enabled ? '<span class="muted">off</span>'
          : r.status?.error ? `<span class="st ${r.status.noData ? 'warning' : 'critical'}" title="${esc(r.status.error)}">${r.status.noData ? 'no data' : 'error'}</span>`
          : r.firing ? stLabel(`${r.firing} firing`, 'critical') : r.pending ? stLabel(`${r.pending} pending`, 'warning') : r.status?.checked ? stLabel('OK', 'good') : '<span class="muted">waiting</span>' },
        { key: 'act', label: '', nosort: true, cls: 'act', render: () => `<button class="icon-btn" data-menu title="Actions">${ICON.more}</button>` },
      ];
      const t = table('al-rules', cols, rows, { sort: { key: 'name', dir: 'asc' } });
      $('#alRTbl').innerHTML = t.html;
      t.bind($('#alRTbl'), draw);
      $('#alRCount').textContent = `${rows.length} of ${D.rules.length}`;
      $$('#alRTbl tbody tr').forEach(tr => {
        const r = t.rows[+tr.dataset.i];
        if (!r) return;
        $('[data-on]', tr)?.addEventListener('change', async e => {
          try { await send('POST', `alerts/rules/${enc(r.id)}/enabled`, { enabled: e.target.checked }); toast(e.target.checked ? 'Rule on' : 'Rule off'); await refresh(); }
          catch (err) { e.target.checked = !e.target.checked; toast(err.message, 'err'); }
        });
        $('.al-what', tr).addEventListener('click', () => openEditor(r));
        $('[data-menu]', tr).addEventListener('click', e => {
          e.stopPropagation();
          openPop(e.currentTarget, canEdit() ? [
            { label: 'Edit', run: () => openEditor(r) },
            { label: 'Duplicate', run: () => openEditor({ ...r, id: '', name: r.name + ' (copy)' }) },
            { label: 'Silence for 1 hour', run: () => silence(r.id, '', 60) },
            { label: 'Silence for 1 day', run: () => silence(r.id, '', 1440) },
            { label: 'History', run: () => { tab = 'history'; hist = null; render(); setTimeout(() => { const s = $('#alHRule'); if (s) { s.value = r.id; s.dispatchEvent(new Event('change')); } }); } },
            'sep', { label: 'Delete', danger: true, run: () => del(r) }] : [{ label: 'View', run: () => openEditor(r) }]);
        });
      });
    };
    $('#alRQ').addEventListener('input', draw);
    draw();
  }

  async function del(r) {
    if (!await confirmDanger('Delete alert rule', `Delete <b>${esc(r.name)}</b>? Its active alerts are cleared; history is kept.`)) return;
    try { await send('DELETE', 'alerts/rules/' + enc(r.id)); toast('Rule deleted'); await refresh(); } catch (e) { toast(e.message, 'err'); }
  }

  // ---------------------------------------------------------------- history
  async function drawHistory() {
    const box = $('#alBody');
    box.innerHTML = `<div class="filters"><select id="alHKind" data-label="Event"><option value="">All</option><option value="fired">Fired</option><option value="resolved">Resolved</option><option value="sent">Sent</option><option value="failed">Failed</option><option value="held">Held</option></select>
      <select id="alHRule" data-label="Rule"><option value="">All rules</option>${D.rules.map(r => `<option value="${esc(r.id)}">${esc(r.name)}</option>`).join('')}</select>
      <input type="search" id="alHQ" placeholder="Filter" style="width:240px"><span class="count" id="alHCount"></span>
      <button class="btn" id="alHDl">${ICON.logs}CSV</button></div><div class="card flush" id="alHTbl"><div class="muted" style="padding:16px">Loading…</div></div>`;
    try { hist = await api('alerts/history?limit=1000'); } catch (e) { $('#alHTbl').innerHTML = `<div class="banner err" style="margin:12px"><b>Not loaded.</b><span>${esc(e.message)}</span></div>`; return; }
    const pill = { fired: 'crit', resolved: 'good', sent: 'good', failed: 'crit', held: 'warn' };
    const shown = () => {
      const k = $('#alHKind').value, ru = $('#alHRule').value, q = $('#alHQ').value.toLowerCase();
      return hist.filter(h => (!k || h.kind === k) && (!ru || h.rule === ru) && (!q || [h.ruleName, h.namespace, h.object, h.text].join(' ').toLowerCase().includes(q)));
    };
    const draw = () => {
      const rows = shown();
      const t = table('al-hist', [
        { key: 'at', label: 'Time', sort: h => -new Date(h.at), render: h => `<span title="${esc(new Date(h.at).toLocaleString())}">${esc(new Date(h.at).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }))}</span>` },
        { key: 'kind', label: 'Event', render: h => `<span class="pill ${pill[h.kind] || ''}">${esc(h.kind)}</span>` },
        { key: 'ruleName', label: 'Rule', render: h => esc(h.ruleName) },
        { key: 'object', label: 'Object', render: h => `<span class="mono">${esc((h.namespace ? h.namespace + '/' : '') + (h.object || ''))}</span>` },
        { key: 'text', label: 'Details', nosort: true, render: h => esc(h.text) },
      ], rows, { sort: { key: 'at', dir: 'asc' }, limit: 1000 });
      $('#alHTbl').innerHTML = t.html;
      t.bind($('#alHTbl'), draw);
      $('#alHCount').textContent = `${rows.length} of ${hist.length}`;
    };
    ['#alHKind', '#alHRule'].forEach(s => $(s).addEventListener('change', draw));
    $('#alHQ').addEventListener('input', draw);
    $('#alHDl').addEventListener('click', () => downloadText(`alert-history-${new Date().toISOString().slice(0, 10)}.csv`,
      toCSV([['Time', 'Event', 'Rule', 'Severity', 'Cluster', 'Namespace', 'Object', 'Details'], ...shown().map(h => [new Date(h.at).toLocaleString(), h.kind, h.ruleName, h.severity || '', h.cluster || '', h.namespace || '', h.object || '', h.text])]), 'text/csv'));
    draw();
  }

  // ---------------------------------------------------------------- channels (admins)
  async function drawChannels() {
    const box = $('#alBody');
    box.innerHTML = '<div class="muted">Loading…</div>';
    try { chans = await api('alerts/channels'); } catch (e) { box.innerHTML = `<div class="banner err"><b>Not loaded.</b><span>${esc(e.message)}</span></div>`; return; }
    box.innerHTML = `<div class="al-ch"><div class="card" id="alSms"></div><div class="card" id="alMail"></div></div>
      <p class="muted" style="margin-top:12px">Usernames and passwords are stored encrypted in the data folder and are never shown again. To change one, press Edit and type the new value; leave it empty to keep the saved one.</p>`;
    chanCard('sms');
    chanCard('email');
  }

  const SMS_FIELDS = [['url', 'Gateway URL', 'https://sms.example.com/1/smsmessaging/outbound/{{sender}}/requests', 'wide'], ['sender', 'Sender (shown as {{sender}})', 'alerts'],
    ['serviceId', 'Service ID', ''], ['fa', 'FA', ''], ['presentId', 'Present ID', ''], ['linkId', 'Link ID', '']];
  const MAIL_FIELDS = [['host', 'SMTP server', 'smtp.example.com'], ['port', 'Port', '587'], ['from', 'From address', 'kubelamp@example.com'], ['fromName', 'From name', 'KubeLamp']];

  function chanCard(kind, edit = false) {
    const c = chans[kind], el = $(kind === 'sms' ? '#alSms' : '#alMail');
    const isSms = kind === 'sms';
    const set = isSms ? !!c.url : !!c.host;
    const title = isSms ? 'SMS' : 'Email';
    if (!edit && set) {
      const rows = isSms ? [['Status', c.enabled ? stLabel('on', 'good') : stLabel('off', 'warning')], ['Gateway', `<span class="mono">${esc(c.url)}</span>`], ['Request', esc(`${c.method} · ${c.contentType || 'no body type'} · auth ${c.auth}`)],
        ['Sender', esc(c.sender || '—')], ['Service ID', esc(c.serviceId || '—')], ['FA / Present ID / Link ID', esc([c.fa || '—', c.presentId || '—', c.linkId || '—'].join(' / '))],
        ['Username', c.usernameSet ? '•••••• <span class="muted">set</span>' : '<span class="muted">not set</span>'], ['Password', c.passwordSet ? '•••••• <span class="muted">set</span>' : '<span class="muted">not set</span>'],
        ...(c.auth === 'wsse' ? [['WSSE digest', c.wsseDigestSet ? `fixed (•••••• set) · nonce <span class="mono">${esc(c.wsseNonce)}</span> · created <span class="mono">${esc(c.wsseCreated)}</span>` : 'made from the password for every message']] : [])]
        : [['Status', c.enabled ? stLabel('on', 'good') : stLabel('off', 'warning')], ['Server', `<span class="mono">${esc(c.host)}:${esc(c.port || (c.security === 'tls' ? 465 : c.security === 'none' ? 25 : 587))}</span>`], ['Security', esc(c.security.toUpperCase())],
        ['From', esc(c.fromName ? `${c.fromName} <${c.from}>` : c.from)], ['Username', c.usernameSet ? '•••••• <span class="muted">set</span>' : '<span class="muted">not set</span>'], ['Password', c.passwordSet ? '•••••• <span class="muted">set</span>' : '<span class="muted">not set</span>']];
      el.innerHTML = `<div class="card-head"><h3>${title}</h3><span class="cap">changed ${age(chans.updated)} ago by ${esc(chans.updatedBy || '—')}</span></div>
        <dl class="al-kv">${rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join('')}</dl>
        <div class="al-row"><button class="btn" data-edit>${ICON.edit}Edit</button><input type="text" data-to placeholder="${isSms ? '+15550100' : 'you@example.com'}" style="width:200px"><button class="btn" data-test>Send test</button>
        <button class="btn ghost danger" data-rm>Remove</button></div>`;
      $('[data-edit]', el).onclick = () => chanCard(kind, true);
      $('[data-test]', el).onclick = async e => {
        const b = e.currentTarget; b.disabled = true;
        try { await send('POST', `alerts/channels/${kind}/test`, { to: $('[data-to]', el).value }); toast(`Test ${title.toLowerCase()} sent`); } catch (err) { toast(err.message, 'err'); }
        b.disabled = false;
      };
      $('[data-rm]', el).onclick = async () => {
        if (!await confirmDanger(`Remove ${title} settings`, `Remove the ${title.toLowerCase()} settings, including the saved username and password? Alerts will stop going out by ${title.toLowerCase()}.`, 'Remove')) return;
        try { chans = await send('DELETE', 'alerts/channels/' + kind); await refresh(true); chanCard(kind); } catch (err) { toast(err.message, 'err'); }
      };
      return;
    }
    if (isSms && !set) { const p = chans.presets[0]; Object.assign(c, { method: p.method, contentType: p.contentType, auth: p.auth, headers: p.headers, body: p.body }); }
    const f = (key, label, ph = '', cls = '') => `<label class="ct-f ${cls}"><span>${esc(label)}</span><input type="text" data-k="${key}" value="${esc(c[key] || '')}" placeholder="${esc(ph)}" spellcheck="false"></label>`;
    const secret = (key, label) => `<label class="ct-f"><span>${esc(label)}</span><input type="${key === 'username' ? 'text' : 'password'}" data-k="${key}" autocomplete="new-password" placeholder="${c[key + 'Set'] ? '•••••• saved: leave empty to keep' : ''}" spellcheck="false"></label>`;
    const sel = (key, label, opts) => `<label class="ct-f"><span>${esc(label)}</span><select data-k="${key}">${opts.map(([v, l]) => `<option value="${v}" ${String(c[key] || '') === v ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select></label>`;
    el.innerHTML = `<div class="card-head"><h3>${title}</h3><span class="cap">${set ? 'editing' : 'not set up'}</span></div>
      <div class="al-grid">
      <label class="ck al-full"><input type="checkbox" data-k="enabled" ${c.enabled || !set ? 'checked' : ''}> Send alerts by ${title.toLowerCase()}</label>
      ${isSms ? `
        <label class="ct-f al-full"><span>Request preset <small class="muted">fills method, type, auth, headers and body below</small></span><select id="alPreset"><option value="">Keep as it is</option>${chans.presets.map(p => `<option value="${p.id}">${esc(p.label)}</option>`).join('')}</select></label>
        ${SMS_FIELDS.map(([k, l, ph, cls]) => f(k, l, ph, cls === 'wide' ? 'al-full' : '')).join('')}
        ${secret('username', 'Username')}${secret('password', 'Password')}
        ${sel('method', 'Method', [['POST', 'POST'], ['GET', 'GET'], ['PUT', 'PUT']])}
        ${sel('contentType', 'Body type', [['application/json; charset=UTF-8', 'JSON (UTF-8)'], ['application/json', 'JSON'], ['application/x-www-form-urlencoded', 'Form'], ['text/plain', 'Plain text'], ['', 'None']])}
        ${sel('auth', 'Authentication', [['wsse', 'WSSE digest (Huawei SDP)'], ['basic', 'Basic (username + password)'], ['none', 'None (in the body or URL)']])}
        <div class="al-full al-grid al-wsse" id="alWsse">
          <div class="al-full muted">WSSE digest: leave these empty to make a fresh nonce and digest from the password for every message. If your gateway was set up with fixed values (as in a script that always sends the same Nonce, Created and PasswordDigest), enter them here.</div>
          ${f('wsseNonce', 'Fixed nonce', 'e.g. 2024012223595900001')}${f('wsseCreated', 'Fixed created', 'e.g. 2024-01-22T23:59:59Z')}${secret('wsseDigest', 'Fixed password digest')}
          ${c.wsseDigestSet ? '<label class="ck al-full"><input type="checkbox" data-k="clearDigest"> Forget the fixed digest and make it from the password</label>' : ''}
        </div>
        <label class="ct-f al-full"><span>Extra headers <small class="muted">one “Name: value” per line</small></span><textarea data-k="headers" rows="3" spellcheck="false">${esc(c.headers || '')}</textarea></label>
        <label class="ct-f al-full"><span>Body</span><textarea data-k="body" rows="5" spellcheck="false">${esc(c.body || chans.presets[0].body)}</textarea></label>
        <div class="al-full muted al-vars">Placeholders: ${AL_SMS_VARS.map(v => `<code>{{${v}}}</code>`).join(' ')}. Values are escaped for the body type.</div>
        <label class="ck al-full"><input type="checkbox" data-k="insecureTLS" ${c.insecureTLS ? 'checked' : ''}> Skip TLS certificate check (internal gateway with a private certificate)</label>`
      : `${MAIL_FIELDS.map(([k, l, ph]) => f(k, l, ph)).join('')}
        ${sel('security', 'Security', [['starttls', 'STARTTLS (usually port 587)'], ['tls', 'TLS (usually port 465)'], ['none', 'None (internal relay, port 25)']])}
        <span></span>${secret('username', 'Username')}${secret('password', 'Password')}
        <label class="ck al-full"><input type="checkbox" data-k="insecureTLS" ${c.insecureTLS ? 'checked' : ''}> Skip TLS certificate check (internal server with a private certificate)</label>`}
      </div><div class="err-line" data-err></div>
      <div class="al-row"><button class="btn primary" data-save>Save</button>${set ? '<button class="btn ghost" data-cancel>Cancel</button>' : ''}</div>`;
    const wsse = () => { const w = $('#alWsse', el); if (w) w.hidden = $('[data-k="auth"]', el).value !== 'wsse'; };
    $('[data-k="auth"]', el)?.addEventListener('change', wsse);
    $('#alPreset', el)?.addEventListener('change', e => {
      const p = chans.presets.find(x => x.id === e.target.value);
      if (!p) return;
      $('[data-k="method"]', el).value = p.method; $('[data-k="contentType"]', el).value = p.contentType; $('[data-k="auth"]', el).value = p.auth;
      $('[data-k="headers"]', el).value = p.headers; $('[data-k="body"]', el).value = p.body;
      wsse();
    });
    wsse();
    $('[data-cancel]', el)?.addEventListener('click', () => chanCard(kind));
    $('[data-save]', el).onclick = async e => {
      const body = {};
      $$('[data-k]', el).forEach(i => { body[i.dataset.k] = i.type === 'checkbox' ? i.checked : i.dataset.k === 'password' ? i.value : i.value.trim(); });
      if (!isSms) body.port = +body.port || 0;
      const b = e.currentTarget; b.disabled = true;
      try { chans = await send('PUT', 'alerts/channels/' + kind, body); toast(`${title} saved`); await refresh(true); chanCard(kind); }
      catch (err) { $('[data-err]', el).textContent = err.message; b.disabled = false; }
    };
  }

  // ---------------------------------------------------------------- rule editor
  async function openEditor(rule) {
    const ro = !canEdit();
    const isNew = !rule || !rule.id;
    const R = Object.assign({ name: '', enabled: true, severity: 'warning', description: '', cluster: ctx || (S.clusters[0] || {}).name || '', namespaces: [], kind: '', workload: '', labels: '',
      condition: 'pod_error', threshold: 0, window: 0, match: '', for: 0, sms: [], email: [], notifyResolved: true, repeat: 0, quietFrom: '', quietTo: '',
      smsText: D.defaults.smsText, emailSubject: D.defaults.emailSubject, emailBody: D.defaults.emailBody }, rule || {});
    ['namespaces', 'sms', 'email'].forEach(k => { if (!Array.isArray(R[k])) R[k] = []; });
    if (isNew && !rule) R.for = cond(R.condition).defFor || 0;
    editing = R;
    const groups = [...new Set(D.catalog.map(c => c.group))];
    const clusters = S.clusters.map(c => c.name);
    if (R.cluster && !clusters.includes(R.cluster)) clusters.unshift(R.cluster);
    const dis = ro ? 'disabled' : '';
    main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="#" id="alBack">Alerting</a> / Rules</div><h1>${ro ? esc(R.name) : isNew ? 'New alert rule' : 'Edit: ' + esc(R.name)}</h1>
        ${!isNew ? `<div class="sub"><span>made ${age(R.created)} ago</span><span>changed ${age(R.updated)} ago by ${esc(R.updatedBy || R.ownerName || '—')}</span><span class="muted">runs with ${esc(R.ownerName || '—')}'s cluster access</span></div>` : ''}</div>
        <div class="right">${ro ? '' : '<button class="btn" id="alCheck">Check now</button><button class="btn" id="alTest">Send test</button>'}<button class="btn ghost" id="alCancel">${ro ? 'Back' : 'Cancel'}</button>${ro ? '' : '<button class="btn primary" id="alSave">Save rule</button>'}</div></div>
      ${ro ? '<div class="banner info"><b>View only.</b><span>Only admins can change alert rules.</span></div>' : ''}
      <div class="al-ed">
      <section class="card"><h3>1 · Rule</h3><div class="al-grid">
        <label class="ct-f al-full"><span>Name</span><input type="text" id="rName" value="${esc(R.name)}" placeholder="e.g. Payments API crashing" maxlength="100" ${dis}></label>
        <div class="ct-f"><span>Severity</span><div class="seg" id="rSev">${Object.entries(AL_SEV).map(([k, [l]]) => `<button type="button" data-v="${k}" class="${R.severity === k ? 'on' : ''}" ${dis}>${l}</button>`).join('')}</div></div>
        <label class="ck"><input type="checkbox" id="rOn" ${R.enabled ? 'checked' : ''} ${dis}> Rule is on</label>
      </div></section>

      <section class="card"><h3>2 · What to watch</h3><div class="al-grid">
        <label class="ct-f"><span>Cluster</span><select id="rCluster" ${dis}>${clusters.map(c => `<option value="${esc(c)}" ${c === R.cluster ? 'selected' : ''}>${esc(shortName(c))}</option>`).join('')}</select></label>
        <label class="ct-f"><span>Condition</span><select id="rCond" ${dis}>${groups.map(g => `<optgroup label="${esc(g)}">${D.catalog.filter(c => c.group === g).map(c => `<option value="${c.id}" ${c.id === R.condition ? 'selected' : ''}>${esc(c.label)}</option>`).join('')}</optgroup>`).join('')}</select></label>
        <div class="al-full muted" id="rHelp"></div>
        <label class="ct-f" id="rThW"><span id="rThL"></span><input type="number" id="rTh" step="any" ${dis}></label>
        <label class="ct-f" id="rWinW"><span id="rWinL"></span><input type="number" id="rWin" step="any" ${dis}></label>
        <label class="ct-f al-full" id="rMatchW"><span id="rMatchL"></span><input type="text" id="rMatch" value="${esc(R.match)}" spellcheck="false" ${dis}></label>
        <label class="ct-f"><span>For <small class="muted">how long it must last before it fires</small></span><span class="al-inl"><input type="number" id="rFor" min="0" step="1" style="width:110px" ${dis}><select id="rForU" ${dis}><option value="1">seconds</option><option value="60">minutes</option><option value="3600">hours</option></select></span></label>
      </div></section>

      <section class="card" id="rScopeC"><h3>3 · Where</h3><div class="al-grid">
        <label class="ct-f al-full" id="rNsW"><span>Namespaces <small class="muted">comma separated, wildcards like <code>*-prod</code>; empty = all</small></span><input type="text" id="rNs" value="${esc(R.namespaces.join(', '))}" spellcheck="false" ${dis}>
          <span class="chips al-nsl" id="rNsList"></span></label>
        <label class="ct-f" id="rKindW"><span>Workload kind</span><select id="rKind" ${dis}>${['', 'Deployment', 'StatefulSet', 'DaemonSet', 'Job', 'CronJob'].map(k => `<option value="${k}" ${k === R.kind ? 'selected' : ''}>${k || 'Any'}</option>`).join('')}</select></label>
        <label class="ct-f" id="rWlW"><span id="rWlL">Name</span><input type="text" id="rWl" value="${esc(R.workload)}" placeholder="any — or api, worker-*, /^pay-/" spellcheck="false" ${dis}></label>
        <label class="ct-f al-full" id="rLblW"><span>Labels <small class="muted">optional, like <code>app=api,tier!=test</code></small></span><input type="text" id="rLbl" value="${esc(R.labels)}" spellcheck="false" ${dis}></label>
      </div></section>

      <section class="card"><h3>4 · Who gets it</h3><div class="al-grid">
        ${ro ? `<div class="al-full muted">${R.smsCount ? R.smsCount + ' phone number(s)' : 'No phone numbers'} · ${R.emailCount ? R.emailCount + ' email address(es)' : 'no email addresses'} <small>(hidden from non-admins)</small></div>` : `
        <label class="ct-f"><span>SMS to <small class="muted">one number per line</small>${D.channels.sms ? '' : ' <small class="warn-t">SMS not set up</small>'}</span><textarea id="rSms" rows="3" placeholder="+15550100" spellcheck="false">${esc((R.sms || []).join('\n'))}</textarea></label>
        <label class="ct-f"><span>Email to <small class="muted">one address per line</small>${D.channels.email ? '' : ' <small class="warn-t">email not set up</small>'}</span><textarea id="rMail" rows="3" placeholder="ops@example.com" spellcheck="false">${esc((R.email || []).join('\n'))}</textarea></label>`}
        <label class="ct-f"><span>Reminders while it keeps firing</span><select id="rRep" ${dis}>${AL_REPEAT.map(([v, l]) => `<option value="${v}" ${+R.repeat === v ? 'selected' : ''}>${l}</option>`).join('')}${AL_REPEAT.some(([v]) => v === +R.repeat) ? '' : `<option value="${R.repeat}" selected>Every ${R.repeat} min</option>`}</select></label>
        <label class="ck" id="rResW"><input type="checkbox" id="rRes" ${R.notifyResolved ? 'checked' : ''} ${dis}> Also send when it is resolved</label>
        <div class="ct-f al-full"><span>Quiet hours <small class="muted">only critical alerts are sent between these times (${esc(D.zone || 'server time')}); others wait until they end</small></span>
          <span class="al-inl"><input type="time" id="rQF" value="${esc(R.quietFrom)}" ${dis}> to <input type="time" id="rQT" value="${esc(R.quietTo)}" ${dis}>${ro ? '' : '<button type="button" class="linkish" id="rQX">clear</button>'}</span></div>
      </div></section>

      <section class="card"><h3>5 · Message</h3>
        <p class="muted al-p">Write what the people receiving it should know or do. Click a placeholder to insert it where the cursor is.</p>
        <div class="al-vars">${AL_VARS.map(v => `<button type="button" class="chip" data-var="${v}" ${dis}>{{${v}}}</button>`).join('')}</div>
        <div class="al-grid">
        <label class="ct-f al-full"><span>Description <small class="muted">{{description}}: what this alert means and what to do</small></span><textarea id="rDesc" rows="3" maxlength="2000" placeholder="e.g. The payments API is crashing. Check the last deploy; call the on-call engineer if it lasts more than 10 minutes." ${dis}>${esc(R.description)}</textarea></label>
        <label class="ct-f al-full"><span>SMS text <small class="muted" id="rSmsN"></small> <button type="button" class="linkish" data-reset="rSmsT" ${dis}>default</button></span><textarea id="rSmsT" rows="3" spellcheck="false" ${dis}>${esc(R.smsText)}</textarea></label>
        <label class="ct-f al-full"><span>Email subject <button type="button" class="linkish" data-reset="rSubj" ${dis}>default</button></span><input type="text" id="rSubj" value="${esc(R.emailSubject)}" spellcheck="false" ${dis}></label>
        <label class="ct-f al-full"><span>Email body <button type="button" class="linkish" data-reset="rBody" ${dis}>default</button></span><textarea id="rBody" rows="7" spellcheck="false" ${dis}>${esc(R.emailBody)}</textarea></label>
        </div></section>

      ${ro ? '' : `<section class="card" id="rOut"><h3>6 · Check and preview</h3><p class="muted al-p">“Check now” shows what this rule matches right now and the messages it would send. “Send test” sends them now, marked [TEST].</p><div id="rOutBody"></div></section>`}
      </div>`;

    const defs = { rSmsT: D.defaults.smsText, rSubj: D.defaults.emailSubject, rBody: D.defaults.emailBody };
    const unit = R.for && R.for % 3600 === 0 ? 3600 : R.for && R.for % 60 === 0 ? 60 : 1;
    $('#rFor').value = R.for / unit; $('#rForU').value = String(unit);
    let lastField = $('#rSmsT');
    ['#rDesc', '#rSmsT', '#rSubj', '#rBody'].forEach(s => $(s).addEventListener('focus', e => { lastField = e.target; }));
    $$('[data-var]').forEach(b => b.addEventListener('click', () => {
      const el = lastField, v = `{{${b.dataset.var}}}`;
      const s = el.selectionStart ?? el.value.length, en = el.selectionEnd ?? el.value.length;
      el.value = el.value.slice(0, s) + v + el.value.slice(en);
      el.focus(); el.selectionStart = el.selectionEnd = s + v.length;
      smsCount();
    }));
    $$('[data-reset]').forEach(b => b.addEventListener('click', () => { $('#' + b.dataset.reset).value = defs[b.dataset.reset]; smsCount(); }));
    const smsCount = () => { const n = $('#rSmsT').value.length; $('#rSmsN').textContent = `template ${n} characters; long messages go out as several SMS`; };
    $('#rSmsT').addEventListener('input', smsCount); smsCount();
    $$('#rSev button').forEach(b => b.addEventListener('click', () => { $$('#rSev button').forEach(x => x.classList.toggle('on', x === b)); }));
    $('#rQX')?.addEventListener('click', () => { $('#rQF').value = ''; $('#rQT').value = ''; });

    const syncCond = (changed) => {
      const c = cond($('#rCond').value);
      $('#rHelp').textContent = (c.help || '') + (c.metrics ? ' Needs metrics-server.' : '');
      const th = c.threshold, wi = c.window;
      $('#rThW').hidden = !th; $('#rWinW').hidden = !wi; $('#rMatchW').hidden = !c.match;
      if (th) { $('#rThL').textContent = `${th.label}${th.unit ? ` (${th.unit})` : ''}`; $('#rTh').min = th.min; $('#rTh').max = th.max; if (changed || !R.threshold) $('#rTh').value = th.def; else $('#rTh').value = R.threshold; }
      if (wi) { $('#rWinL').textContent = `${wi.label} (${wi.unit})`; $('#rWin').min = wi.min; $('#rWin').max = wi.max; if (changed || !R.window) $('#rWin').value = wi.def; else $('#rWin').value = R.window; }
      if (c.match) $('#rMatchL').textContent = c.match;
      if (changed) { const f = c.defFor || 0, u = f && f % 60 === 0 ? 60 : 1; $('#rFor').value = f / u; $('#rForU').value = String(u); }
      const t = c.target;
      $('#rScopeC').hidden = t === 'cluster';
      $('#rNsW').hidden = t === 'node';
      $('#rKindW').hidden = !['pod', 'workload', 'job', 'event'].includes(t);
      $('#rWlL').textContent = { node: 'Node names', pvc: 'Volume claim names', externalsecret: 'ExternalSecret names', cronjob: 'CronJob names', job: 'Job or CronJob names', event: 'Object names' }[t] || 'Workload or pod names';
      $('#rResW').hidden = !!c.instant;
    };
    $('#rCond').addEventListener('change', () => syncCond(true));
    syncCond(false);

    // namespaces of the cluster, click to add
    const loadNs = async () => {
      const box = $('#rNsList'); box.innerHTML = '';
      if (ro) return;
      let nss = [];
      try { nss = await api('namespaces?ctx=' + enc($('#rCluster').value)); } catch { return; }
      box.innerHTML = nss.slice(0, 60).map(n => `<button type="button" class="chip" data-ns="${esc(n.name)}">${esc(n.name)}</button>`).join('') + (nss.length > 60 ? `<span class="muted">+${nss.length - 60} more: type them</span>` : '');
      $$('[data-ns]', box).forEach(b => b.addEventListener('click', () => {
        const cur = $('#rNs').value.split(',').map(s => s.trim()).filter(Boolean);
        if (!cur.includes(b.dataset.ns)) cur.push(b.dataset.ns);
        $('#rNs').value = cur.join(', ');
      }));
    };
    $('#rCluster').addEventListener('change', loadNs);
    loadNs();

    const read = () => {
      const lines = s => (s || '').split(/[\n,;]+/).map(x => x.trim()).filter(Boolean);
      return { ...R, name: $('#rName').value, severity: $('#rSev button.on')?.dataset.v || 'warning', enabled: $('#rOn').checked, cluster: $('#rCluster').value,
        condition: $('#rCond').value, threshold: +$('#rTh').value || 0, window: +$('#rWin').value || 0, match: $('#rMatch').value,
        for: Math.round((+$('#rFor').value || 0) * +$('#rForU').value), namespaces: lines($('#rNs').value), kind: $('#rKindW').hidden ? '' : $('#rKind').value,
        workload: $('#rWl').value, labels: $('#rLbl').value, sms: lines($('#rSms')?.value), email: lines($('#rMail')?.value), repeat: +$('#rRep').value,
        notifyResolved: $('#rRes').checked, quietFrom: $('#rQF').value, quietTo: $('#rQT').value, description: $('#rDesc').value,
        smsText: $('#rSmsT').value, emailSubject: $('#rSubj').value, emailBody: $('#rBody').value };
    };
    const close = async () => { editing = null; await refresh(); };
    $('#alBack').addEventListener('click', e => { e.preventDefault(); close(); });
    $('#alCancel').addEventListener('click', close);
    $('#alSave')?.addEventListener('click', async e => {
      const b = e.currentTarget; b.disabled = true;
      try {
        await send(isNew ? 'POST' : 'PUT', isNew ? 'alerts/rules' : 'alerts/rules/' + enc(R.id), read());
        toast(isNew ? 'Rule created' : 'Rule saved');
        tab = 'rules';
        try { localStorage.setItem('kl.alerts.tab', tab); } catch {}
        await close();
      } catch (err) { toast(err.message, 'err'); b.disabled = false; }
    });
    const out = (html) => { $('#rOutBody').innerHTML = html; $('#rOut').scrollIntoView({ behavior: 'smooth', block: 'nearest' }); };
    $('#alCheck')?.addEventListener('click', async e => {
      const b = e.currentTarget; b.disabled = true; out('<div class="muted">Checking…</div>');
      try {
        const d = await send('POST', 'alerts/check', read());
        const m = d.matches || [];
        out(`${d.error ? `<div class="banner ${d.noData ? '' : 'err'}"><b>${d.noData ? 'No data.' : 'Error.'}</b><span>${esc(d.error)}</span></div>` : ''}
          <div class="al-check"><b>${m.length ? `${m.length} match${m.length === 1 ? '' : 'es'} right now` : 'Nothing matches right now'}</b>${m.length && +$('#rFor').value ? ` <span class="muted">(it fires once they last ${esc(alDur(read().for))})</span>` : ''}</div>
          ${m.length ? `<div class="tbl-wrap"><table class="t"><thead><tr><th>Object</th><th>Details</th></tr></thead><tbody>${m.slice(0, 50).map(a => `<tr><td class="mono">${esc((a.namespace ? a.namespace + '/' : '') + a.object)}</td><td>${esc(a.summary)}</td></tr>`).join('')}</tbody></table></div>` : ''}
          <h4>Preview${d.preview.sample === 'true' ? ' <small class="muted">(with a sample alert)</small>' : ''}</h4>
          <div class="al-prev"><div><span class="cap">SMS</span><pre>${esc(d.preview.sms)}</pre></div><div><span class="cap">Email</span><pre><b>${esc(d.preview.subject)}</b>\n\n${esc(d.preview.body)}</pre></div></div>`);
      } catch (err) { out(`<div class="banner err"><b>Can't check.</b><span>${esc(err.message)}</span></div>`); }
      b.disabled = false;
    });
    $('#alTest')?.addEventListener('click', async e => {
      const b = e.currentTarget; b.disabled = true;
      try {
        const h = await send('POST', 'alerts/test', read());
        out(`<div class="al-check"><b>Test sent</b></div>${h.map(x => `<div class="st ${x.kind === 'sent' ? 'good' : 'critical'}">${esc(x.text)}</div>`).join('')}`);
      } catch (err) { toast(err.message, 'err'); }
      b.disabled = false;
    });
  }

  async function refresh(quiet) {
    try { await load(); } catch (e) { if (!quiet) toast(e.message, 'err'); return; }
    if (!editing && tab !== 'channels' && tab !== 'history') render();
    else if (!editing && !quiet) render();
  }

  render();
  every(10000, async () => {
    if (editing || document.hidden || !['active', 'rules'].includes(tab)) return;
    if ($('#alBody input:focus, .dd[aria-expanded="true"]')) return; // don't redraw under the user's hands
    try { await load(); } catch { return; }
    const keep = $('#alQ')?.value ?? $('#alRQ')?.value;
    render();
    if (keep) { const q = $('#alQ') || $('#alRQ'); q.value = keep; q.dispatchEvent(new Event('input')); }
  });
}

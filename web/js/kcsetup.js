'use strict';
/* Kubeconfig from the user: a first-run "Connect your clusters" screen, and a manager
   (All clusters → Kubeconfigs) to add or remove files later. Upload a file, paste YAML,
   or give a path on the machine KubeLamp runs on. */

const KC_ICON = {
  upload: '<svg viewBox="0 0 24 24"><path d="M12 15V4M7.5 8.5L12 4l4.5 4.5"/><path d="M4 15v3.5A1.5 1.5 0 0 0 5.5 20h13a1.5 1.5 0 0 0 1.5-1.5V15"/></svg>',
  paste: '<svg viewBox="0 0 24 24"><rect x="5.5" y="4.5" width="13" height="16" rx="2"/><path d="M9 4.5V3.5h6v1M9 10h6M9 14h6"/></svg>',
  path: '<svg viewBox="0 0 24 24"><path d="M3.5 7.5a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/></svg>',
  file: '<svg viewBox="0 0 24 24"><path d="M6.5 3.5h7l4 4v12a1 1 0 0 1-1 1h-10a1 1 0 0 1-1-1v-15a1 1 0 0 1 1-1z"/><path d="M13.5 3.5v4h4"/></svg>',
};

// the add form; onDone() runs after a kubeconfig was added
function kcAddForm(box, kc, onDone) {
  const sep = navigator.platform.startsWith('Win') ? ';' : ':';
  box.innerHTML = `<div class="seg kc-tabs" role="tablist">
      <button type="button" data-t="upload" class="on">${KC_ICON.upload}Upload file</button>
      <button type="button" data-t="paste">${KC_ICON.paste}Paste</button>
      <button type="button" data-t="path">${KC_ICON.path}File path</button></div>
    <div class="kc-pane" data-p="upload">
      <label class="kc-drop" id="kcDrop">${KC_ICON.upload}<b>Drop kubeconfig files here</b><span class="muted">or click to choose · you can pick several</span>
        <input type="file" id="kcFile" multiple accept=".yaml,.yml,.conf,.config,.kubeconfig,text/yaml,text/plain,*/*"></label></div>
    <div class="kc-pane" data-p="paste" hidden>
      <label class="kc-f"><span>Name</span><input type="text" id="kcName" placeholder="e.g. production" spellcheck="false"></label>
      <label class="kc-f"><span>Kubeconfig YAML</span><textarea id="kcYaml" rows="10" spellcheck="false" placeholder="apiVersion: v1&#10;kind: Config&#10;clusters: …&#10;contexts: …&#10;users: …"></textarea></label>
      <button type="button" class="btn primary" id="kcPasteGo">Add kubeconfig</button></div>
    <div class="kc-pane" data-p="path" hidden>
      <label class="kc-f"><span>Path on ${kc.local ? 'this computer' : 'the KubeLamp server'}</span>
        <input type="text" id="kcPath" spellcheck="false" placeholder="${navigator.platform.startsWith('Win') ? 'C:\\Users\\you\\.kube\\config' : '~/.kube/config'}"></label>
      <p class="muted kc-hint">${kc.local ? `Separate several files with <code>${sep}</code>.` : 'The file must exist where KubeLamp runs (for Docker: inside the container, e.g. a mounted <code>/kube/config</code>).'}</p>
      <button type="button" class="btn primary" id="kcPathGo">Load file</button></div>
    <div class="kc-msg" id="kcMsg" role="status"></div>
    <p class="muted kc-hint">${kc.local ? 'Kubeconfigs you upload or paste are copied into KubeLamp\'s data folder (readable only by you) so they are there next time.'
      : 'Uploaded kubeconfigs are stored on the KubeLamp server in its data folder. For safety they must embed certificates (<code>*-data</code> fields) and may only use known login plugins (aws, kubelogin, gke-gcloud-auth-plugin…).'}</p>`;
  const msg = (t, kind = '') => { const m = $('#kcMsg', box); m.className = 'kc-msg ' + kind; m.innerHTML = t; };
  $$('.kc-tabs button', box).forEach(b => b.addEventListener('click', () => {
    $$('.kc-tabs button', box).forEach(x => x.classList.toggle('on', x === b));
    $$('.kc-pane', box).forEach(p => { p.hidden = p.dataset.p !== b.dataset.t; });
    msg('');
  }));
  const upload = async (name, yaml) => {
    const d = await send('POST', 'kubeconfig/upload', { name, yaml });
    return `${esc(name || 'kubeconfig')}: ${d.contexts} context${d.contexts === 1 ? '' : 's'}${d.shadowed?.length ? ` <span class="muted">(${esc(d.shadowed.join(', '))} already loaded from another file; that one is used)</span>` : ''}`;
  };
  const files = async list => {
    if (!list.length) return;
    msg('<span class="spin"></span> Loading…');
    const ok = [], bad = [];
    for (const f of list) {
      if (f.size > 4 << 20) { bad.push(`${esc(f.name)}: larger than 4 MB`); continue; }
      try { ok.push(await upload(f.name, await f.text())); } catch (e) { bad.push(`${esc(f.name)}: ${esc(e.message)}`); }
    }
    msg([...ok.map(x => '✓ ' + x), ...bad.map(x => '✕ ' + x)].join('<br>'), bad.length ? 'err' : 'ok');
    if (ok.length) onDone(!bad.length);
  };
  const input = $('#kcFile', box), drop = $('#kcDrop', box);
  input.addEventListener('change', () => { files([...input.files]); input.value = ''; });
  ['dragenter', 'dragover'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.add('over'); }));
  ['dragleave', 'drop'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.remove('over'); }));
  drop.addEventListener('drop', e => files([...(e.dataTransfer?.files || [])]));
  $('#kcPasteGo', box).addEventListener('click', async () => {
    const yaml = $('#kcYaml', box).value;
    if (!yaml.trim()) return msg('Paste the contents of a kubeconfig first.', 'err');
    try { msg('✓ ' + await upload($('#kcName', box).value.trim() || 'pasted', yaml), 'ok'); $('#kcYaml', box).value = ''; onDone(true); }
    catch (e) { msg('✕ ' + esc(e.message), 'err'); }
  });
  const goPath = async () => {
    const path = $('#kcPath', box).value.trim();
    if (!path) return msg('Enter a path.', 'err');
    try { await send('POST', 'kubeconfig/path', { path }); msg('✓ Loaded ' + esc(path), 'ok'); onDone(true); }
    catch (e) { msg('✕ ' + esc(e.message), 'err'); }
  };
  $('#kcPathGo', box).addEventListener('click', goPath);
  $('#kcPath', box).addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); goPath(); } });
}

// first run: nothing loaded yet
function viewConnect(kc) {
  renderNav({});
  main.innerHTML = `<div class="kc-welcome">
      <img src="logo.png" alt="" width="56" height="56">
      <h1>Connect your clusters</h1>
      <p class="ink2">KubeLamp needs a kubeconfig to reach your Kubernetes clusters. Add one to start — nothing is read from this machine until you do.</p>
      <div class="card kc-card" id="kcAdd"></div>
    </div>`;
  kcAddForm($('#kcAdd'), kc, () => setTimeout(() => navigate(), 600));
}

// All clusters → Kubeconfigs: what is loaded, remove, add more
async function openKubeconfigs() {
  let changed = false;
  await modal({
    title: 'Kubeconfigs', wide: true,
    body: `<div id="kcList" class="kc-list"></div><h4 class="kc-sub">Add a kubeconfig</h4><div id="kcAdd2"></div>`,
    actions: [{ label: 'Done', kind: 'primary', value: true, submit: true }],
    init: async d => {
      const list = async () => {
        const kc = await api('kubeconfig');
        $('#kcList', d).innerHTML = kc.sources.length ? kc.sources.map((s, i) => `<div class="kc-src">
            ${KC_ICON.file}<div class="kc-src-main"><b title="${esc(s.path)}">${esc(s.name)}</b>
            <span class="muted">${s.uploaded ? 'uploaded' : esc(s.path)}${s.error ? ` · <span class="crit-t">${esc(s.error)}</span>` : ''}</span>
            <div class="chips">${s.contexts.map(c => `<span class="chip">${esc(c)}</span>`).join('') || '<span class="muted">no contexts</span>'}</div></div>
            <button type="button" class="btn sm danger" data-rm="${i}">Remove</button></div>`).join('')
          : `<p class="muted">No kubeconfig loaded${kc.inCluster ? ' — only the in-cluster connection is used' : ''}.</p>`;
        $$('[data-rm]', d).forEach(b => b.addEventListener('click', async () => {
          const s = kc.sources[+b.dataset.rm];
          if (!confirm(`Remove “${s.name}”? Its clusters disappear from KubeLamp${s.uploaded ? ' and the stored copy is deleted' : ' (the file itself is not touched)'}.`)) return;
          try { await send('DELETE', 'kubeconfig?path=' + enc(s.path)); changed = true; list(); } catch (e) { toast(e.message, 'err'); }
        }));
        return kc;
      };
      const kc = await list();
      kcAddForm($('#kcAdd2', d), kc, () => { changed = true; list(); });
    },
  });
  if (changed) { await loadClusters(); navigate(); }
}

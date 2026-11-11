'use strict';
/* Settings page (#/settings): appearance — mode, accent colour (presets + picker), glass. */

const MODE_ICON = {
  system: '<rect x="3" y="4.5" width="18" height="12" rx="2"/><path d="M8.5 20h7M12 16.5V20"/>',
  light: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"/>',
  dark: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
};

function viewSettings() {
  const info = S.info || {};
  main.innerHTML = `<div class="page-head"><div><div class="crumbs"><a href="#/">KubeLamp</a></div><h1>Settings</h1>
      <div class="sub"><span>Appearance is saved in this browser</span></div></div>
      <div class="right"><button class="btn ghost" id="setReset">Reset to defaults</button></div></div>
    <div class="set-wrap">
      <section class="card set-sec">
        <div class="set-row"><div class="set-label"><h3>Theme</h3><p>Follow your system, or pick light or dark.</p></div>
          <div class="set-modes" role="radiogroup" aria-label="Theme">${['system', 'light', 'dark'].map(m => `
            <button class="set-mode" role="radio" data-mode="${m}">
              <span class="set-prev ${m}"><i></i><b></b><em></em></span>
              <span class="set-mname"><svg viewBox="0 0 24 24">${MODE_ICON[m]}</svg>${m[0].toUpperCase() + m.slice(1)}</span>
            </button>`).join('')}</div></div>
        <div class="set-row"><div class="set-label"><h3>Accent colour</h3><p>Used for buttons, highlights, selection and focus. Pick a preset or any colour.</p></div>
          <div class="set-accent">
            <div class="set-swatches">${Theme.PRESETS.map(([n, c]) => `<button class="set-sw" data-c="${c}" title="${n}" aria-label="${n}" style="--c:${c}"></button>`).join('')}
              <label class="set-sw set-custom" title="Custom colour"><input type="color" id="setPick" aria-label="Custom colour"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg></label></div>
            <div class="set-hex"><span class="set-cur" id="setCur"></span><input type="text" id="setHex" maxlength="7" spellcheck="false" aria-label="Hex colour"><span class="muted" id="setName"></span></div>
          </div></div>
        <div class="set-row"><div class="set-label"><h3>Glass effect</h3><p>Translucent, blurred panels over a soft accent-tinted backdrop.</p></div>
          <label class="switch"><input type="checkbox" id="setGlass"><span></span><b id="setGlassTxt"></b></label></div>
        <div class="set-row"><div class="set-label"><h3>Preview</h3><p>How controls look with these settings.</p></div>
          <div class="set-demo card"><div class="chips"><span class="kind">HTTPRoute</span><span class="chip">api.example.com</span><span class="st good">Running</span><span class="st warning">Pending</span></div>
            <div class="meter" style="margin:12px 0"><i style="width:62%"></i></div>
            <div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn primary">Primary</button><button class="btn">Default</button><button class="btn on">Selected</button><input type="search" placeholder="Focus me"></div></div></div>
      </section>
      <section class="card set-sec">
        <div class="set-row"><div class="set-label"><h3>About</h3><p>KubeLamp ${esc(info.version || '')}</p></div>
          <div class="kvgrid"><span>Mode</span><span>${info.readOnly ? 'Read-only' : 'Read & write'}</span><span>Local shell</span><span>${info.shell ? 'Enabled' : 'Disabled'}</span><span>Platform</span><span>${esc(info.os || '—')}</span></div></div>
      </section>
    </div>`;
  const sync = () => {
    const t = Theme.get();
    $$('.set-mode', main).forEach(b => { const on = b.dataset.mode === t.mode; b.classList.toggle('on', on); b.setAttribute('aria-checked', on); });
    const preset = Theme.PRESETS.find(([, c]) => c.toLowerCase() === t.accent.toLowerCase());
    $$('.set-sw[data-c]', main).forEach(b => b.classList.toggle('on', b.dataset.c.toLowerCase() === t.accent.toLowerCase()));
    $('.set-custom', main).classList.toggle('on', !preset);
    $('.set-custom', main).style.setProperty('--c', preset ? 'transparent' : t.accent);
    $('#setPick').value = t.accent;
    if (document.activeElement !== $('#setHex')) $('#setHex').value = t.accent;
    $('#setCur').style.background = t.accent;
    $('#setName').textContent = preset ? preset[0] + (preset[0] === 'Blue' ? ' (default)' : '') : 'Custom';
    $('#setGlass').checked = !!t.glass;
    $('#setGlassTxt').textContent = t.glass ? 'On' : 'Off';
  };
  $$('.set-mode', main).forEach(b => b.addEventListener('click', () => { Theme.set({ mode: b.dataset.mode }); sync(); }));
  $$('.set-sw[data-c]', main).forEach(b => b.addEventListener('click', () => { Theme.set({ accent: b.dataset.c }); sync(); }));
  $('#setPick').addEventListener('input', e => { Theme.set({ accent: e.target.value }); sync(); });
  $('#setHex').addEventListener('input', e => {
    let v = e.target.value.trim();
    if (!v.startsWith('#')) v = '#' + v;
    if (/^#[0-9a-f]{3}$/i.test(v)) v = '#' + [...v.slice(1)].map(x => x + x).join('');
    if (Theme.hexOk(v)) { Theme.set({ accent: v.toLowerCase() }); sync(); }
  });
  $('#setHex').addEventListener('blur', sync);
  $('#setGlass').addEventListener('change', e => { Theme.set({ glass: e.target.checked }); sync(); });
  $('#setReset').addEventListener('click', () => { Theme.reset(); sync(); toast('Appearance reset to defaults'); });
  sync();
}

// charts read colours at draw time: redraw the open view when the theme changes
window.addEventListener('themechange', () => { if (typeof S !== 'undefined' && S.redraw) requestAnimationFrame(() => S.redraw && S.redraw()); });

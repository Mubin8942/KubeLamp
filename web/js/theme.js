'use strict';
/* Appearance: light / dark / system mode, accent colour and the glass look.
   Loaded in <head> so the saved theme applies before the first paint.
   Stored per browser in localStorage (kd.theme). */

const Theme = (() => {
  const KEY = 'kd.theme';
  const DEFAULTS = { mode: 'system', accent: '#326ce5', glass: true };
  const PRESETS = [
    ['Blue', '#326ce5'], ['Indigo', '#5b5fe0'], ['Purple', '#8b5cf6'], ['Pink', '#db2777'],
    ['Red', '#dc3545'], ['Orange', '#ea6a1c'], ['Amber', '#d39b06'], ['Green', '#17a34a'],
    ['Teal', '#0e9488'], ['Cyan', '#0891b2'], ['Slate', '#64748b'],
  ];
  const media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;

  function load() {
    try { return { ...DEFAULTS, ...JSON.parse(localStorage.getItem(KEY) || '{}') }; } catch { return { ...DEFAULTS }; }
  }
  let cur = load();

  // ---- colour helpers
  const hexOk = h => /^#[0-9a-f]{6}$/i.test(h);
  const rgb = h => [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16));
  const hex = c => '#' + c.map(v => Math.max(0, Math.min(255, Math.round(v))).toString(16).padStart(2, '0')).join('');
  const mix = (a, b, t) => a.map((v, i) => v + (b[i] - v) * t);
  const lum = c => {
    const l = c.map(v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; });
    return 0.2126 * l[0] + 0.7152 * l[1] + 0.0722 * l[2];
  };
  const contrast = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };

  const resolved = () => cur.mode === 'system' ? (media && media.matches ? 'light' : 'dark') : cur.mode;

  function apply() {
    const root = document.documentElement;
    const mode = resolved();
    root.dataset.theme = mode;
    root.classList.toggle('glass', !!cur.glass);
    const acc = hexOk(cur.accent) ? cur.accent : DEFAULTS.accent;
    const c = rgb(acc);
    const white = [255, 255, 255], black = [0, 0, 0];
    const bg = mode === 'light' ? rgb('#f3f5f8') : rgb('#0f141b');
    // readable accent text: lighten on dark, darken on light until it reaches 4.5:1
    let ink = c, t = 0;
    while (contrast(ink, bg) < 4.5 && t < 1) { t += 0.05; ink = mix(c, mode === 'light' ? black : white, t); }
    const st = root.style;
    st.setProperty('--accent', acc);
    st.setProperty('--accent-rgb', c.join(','));
    st.setProperty('--accent-ink', hex(ink));
    st.setProperty('--accent-fg', contrast(c, white) >= 3 ? '#ffffff' : '#0b1016');
    st.setProperty('--sel', `rgba(${c.join(',')},${mode === 'light' ? 0.11 : 0.18})`);
    st.setProperty('--track', hex(mix(bg, c, mode === 'light' ? 0.2 : 0.28)));
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = mode === 'light' ? '#f3f5f8' : '#0f141b';
    window.dispatchEvent(new CustomEvent('themechange', { detail: { ...cur, resolved: mode } }));
  }

  function set(patch) {
    cur = { ...cur, ...patch };
    try { localStorage.setItem(KEY, JSON.stringify(cur)); } catch {}
    apply();
  }
  function reset() {
    cur = { ...DEFAULTS };
    try { localStorage.removeItem(KEY); } catch {}
    apply();
  }

  if (media) {
    const onSys = () => { if (cur.mode === 'system') apply(); };
    media.addEventListener ? media.addEventListener('change', onSys) : media.addListener(onSys);
  }
  apply();
  return { get: () => ({ ...cur }), set, reset, resolved, PRESETS, DEFAULTS, hexOk };
})();

'use strict';
/* Dropdowns: every <select> in the app is shown as a picker button plus a popover in the
   style of the namespace menu: search for long lists, group headings with counts, check
   mark on the selected option, keyboard control.

   The native <select> stays in the page (hidden) and keeps being the source of truth, so
   page code still reads select.value, listens for "change" and rewrites its options. New
   selects are picked up automatically. Opt out with data-native.
     data-label="Lines"   small label shown inside the button
     <option data-meta>   text on the right of an option; a trailing " (12)" or " (3/3)"
                          in the option text is shown there too (3/3 as a ready pill) */

const DD = (() => {
  const SEARCH_FROM = 9; // a search box for lists longer than 8 options
  const proto = HTMLSelectElement.prototype;
  const nValue = Object.getOwnPropertyDescriptor(proto, 'value');
  const nIndex = Object.getOwnPropertyDescriptor(proto, 'selectedIndex');
  const chev = '<svg class="chev" viewBox="0 0 24 24"><path d="M7 10l5 5 5-5"/></svg>';
  const check = '<svg class="ck" viewBox="0 0 24 24"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';
  const glass = '<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6.5"/><path d="M20 20l-4.2-4.2"/></svg>';
  const topLayer = typeof HTMLElement.prototype.showPopover === 'function';

  const pop = document.createElement('div');
  pop.className = 'dd-pop';
  pop.setAttribute('role', 'listbox');
  pop.tabIndex = -1;
  if (topLayer) pop.setAttribute('popover', 'manual'); // above modal dialogs too
  else pop.hidden = true;
  document.body.appendChild(pop);

  let cur = null; // { sel, btn, items, shown, kb, q }

  // ---- reading a <select>
  const split = text => {
    const m = /^(.*\S)\s+\((\d+(?:\/\d+)?)\)$/.exec(text);
    return m ? { text: m[1], meta: m[2] } : { text, meta: '' };
  };
  function readItems(sel) {
    const out = [];
    const add = (o, group) => {
      const s = split(o.textContent.trim());
      out.push({ value: o.value, text: s.text, meta: o.dataset.meta ?? s.meta, disabled: o.disabled, group, el: o });
    };
    for (const ch of sel.children) {
      if (ch.tagName === 'OPTGROUP') for (const o of ch.children) add(o, ch.label);
      else if (ch.tagName === 'OPTION') add(ch, '');
    }
    return out;
  }
  const isPlaceholder = (sel, o) => o && o.value === '' && /…$/.test(o.textContent.trim()) && sel.selectedIndex <= 0;

  function sync(sel) {
    const btn = sel._dd;
    if (!btn) return;
    const o = sel.options[nIndex.get.call(sel)];
    const label = sel.dataset.label;
    const s = o ? split(o.textContent.trim()) : { text: '' };
    btn.innerHTML = `${label ? `<span class="lbl">${esc(label)}</span>` : ''}<span class="val ${isPlaceholder(sel, o) ? 'ph' : ''}">${esc(o && o.dataset.short || s.text)}</span>${chev}`;
    btn.disabled = sel.disabled;
    btn.hidden = sel.hidden;
    btn.title = sel.title || '';
    if (cur && cur.sel === sel) render();
  }

  function enhance(sel) {
    if (sel._dd || sel.multiple || sel.size > 1 || sel.hasAttribute('data-native')) return;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'dd';
    btn.setAttribute('aria-haspopup', 'listbox');
    if (sel.style.width) btn.style.width = sel.style.width;
    if (sel.style.minWidth) btn.style.minWidth = sel.style.minWidth;
    if (sel.id) btn.dataset.for = sel.id;
    sel._dd = btn;
    sel.classList.add('dd-native');
    sel.after(btn);
    // page code sets .value / .selectedIndex directly: keep the button in step
    Object.defineProperty(sel, 'value', { configurable: true, get() { return nValue.get.call(this); }, set(v) { nValue.set.call(this, v); sync(this); } });
    Object.defineProperty(sel, 'selectedIndex', { configurable: true, get() { return nIndex.get.call(this); }, set(v) { nIndex.set.call(this, v); sync(this); } });
    new MutationObserver(() => sync(sel)).observe(sel, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['disabled', 'hidden', 'selected', 'label', 'title'] });
    sel.addEventListener('change', () => sync(sel));
    btn.addEventListener('click', e => { e.preventDefault(); cur && cur.sel === sel ? close() : open(sel); });
    btn.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) { e.preventDefault(); open(sel); }
    });
    sync(sel);
  }

  // ---- popover
  function open(sel) {
    close();
    const items = readItems(sel);
    cur = { sel, btn: sel._dd, items, q: '', kb: -1, search: items.length >= SEARCH_FROM };
    cur.btn.classList.add('open');
    cur.btn.setAttribute('aria-expanded', 'true');
    pop.innerHTML = (cur.search ? `<div class="dd-search">${glass}<input type="search" placeholder="Search ${items.length} options…" spellcheck="false" autocomplete="off"></div>` : '')
      + '<div class="dd-list"></div>'
      + (cur.search ? '<div class="dd-foot"><span><kbd>↑</kbd> <kbd>↓</kbd> move</span><span><kbd>Enter</kbd> select</span><span><kbd>Esc</kbd> close</span></div>' : '');
    // a modal dialog makes everything outside it inert: open inside the dialog then
    const host = sel.closest('dialog') || document.body;
    if (pop.parentNode !== host) host.appendChild(pop);
    if (topLayer) pop.showPopover(); else pop.hidden = false;
    render(true);
    place();
    const inp = $('input', pop);
    if (inp) {
      inp.addEventListener('input', () => { cur.q = inp.value.trim().toLowerCase(); cur.kb = -1; render(); });
      inp.focus();
    } else pop.focus();
  }

  function render(first) {
    if (!cur) return;
    const sel = cur.sel;
    cur.items = readItems(sel);
    const selIdx = nIndex.get.call(sel);
    const q = cur.q;
    const hl = t => {
      if (!q) return esc(t);
      const i = t.toLowerCase().indexOf(q);
      return i < 0 ? esc(t) : esc(t.slice(0, i)) + '<mark>' + esc(t.slice(i, i + q.length)) + '</mark>' + esc(t.slice(i + q.length));
    };
    // a placeholder ("Choose a workload…") is the empty state, not a choice
    const shown = cur.items.filter(it => !(it.value === '' && /…$/.test(it.text)) && (!q || (it.text + ' ' + it.group).toLowerCase().includes(q)));
    cur.shown = shown;
    if (first) cur.kb = Math.max(0, shown.findIndex(it => it.el.index === selIdx));
    const groups = [...new Set(shown.map(it => it.group))];
    const grouped = groups.some(g => g);
    let html = '';
    for (const g of groups) {
      const its = shown.filter(it => it.group === g);
      if (g) html += `<div class="dd-group"><span class="tx">${esc(g)}</span><span class="n">${its.length}</span></div>`;
      html += `<div class="dd-items ${g ? '' : 'flat'} ${grouped && !g ? 'top' : ''}">` + its.map(it => {
        const i = shown.indexOf(it);
        const pill = /^\d+\/\d+$/.test(it.meta) ? (() => { const [a, b] = it.meta.split('/').map(Number); return `<span class="pill ${a === b ? 'good' : a === 0 ? 'crit' : 'warn'}">${esc(it.meta)}</span>`; })() : '';
        return `<button type="button" role="option" class="dd-opt ${it.el.index === selIdx ? 'on' : ''} ${i === cur.kb ? 'kb' : ''}" data-i="${i}" ${it.disabled ? 'disabled' : ''} aria-selected="${it.el.index === selIdx}">
          ${check}<span class="tx">${hl(it.text)}</span>${pill || (it.meta ? `<span class="meta">${esc(it.meta)}</span>` : '')}</button>`;
      }).join('') + '</div>';
    }
    $('.dd-list', pop).innerHTML = html || '<div class="dd-none">No matches</div>';
    $$('.dd-opt', pop).forEach(b => {
      b.addEventListener('mousedown', e => e.preventDefault()); // keep focus in the search box
      b.addEventListener('click', () => choose(shown[+b.dataset.i]));
    });
    $('.dd-opt.kb', pop)?.scrollIntoView({ block: 'nearest' });
  }

  function place() {
    const r = cur.btn.getBoundingClientRect();
    pop.style.minWidth = Math.max(200, r.width) + 'px';
    pop.style.left = '0px'; pop.style.top = '0px';
    const pr = pop.getBoundingClientRect();
    const below = innerHeight - r.bottom - 10, above = r.top - 10;
    const top = pr.height <= below || below >= above ? r.bottom + 6 : Math.max(8, r.top - 6 - pr.height);
    pop.style.top = top + 'px';
    pop.style.left = Math.max(8, Math.min(r.left, innerWidth - pr.width - 8)) + 'px';
    pop.style.maxHeight = Math.max(160, (top > r.top ? below : above) - 6) + 'px';
  }

  function choose(it) {
    if (!it || it.disabled) return;
    const sel = cur.sel, btn = cur.btn;
    close();
    if (it.el.index !== nIndex.get.call(sel)) {
      nIndex.set.call(sel, it.el.index);
      sync(sel);
      sel.dispatchEvent(new Event('input', { bubbles: true }));
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }
    btn.focus();
  }

  function close() {
    if (!cur) return;
    cur.btn.classList.remove('open');
    cur.btn.setAttribute('aria-expanded', 'false');
    cur = null;
    if (topLayer) { try { pop.hidePopover(); } catch {} } else pop.hidden = true;
  }

  pop.addEventListener('keydown', e => {
    if (!cur) return;
    const n = cur.shown.length;
    const move = d => {
      if (!n) return;
      let i = cur.kb;
      for (let k = 0; k < n; k++) { i = (i + d + n) % n; if (!cur.shown[i].disabled) break; }
      cur.kb = i; render();
    };
    if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
    else if (e.key === 'Home' && !cur.search) { e.preventDefault(); cur.kb = 0; render(); }
    else if (e.key === 'End' && !cur.search) { e.preventDefault(); cur.kb = n - 1; render(); }
    else if (e.key === 'Enter') { e.preventDefault(); choose(cur.shown[cur.kb] || cur.shown[0]); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); const b = cur.btn; close(); b.focus(); } // not the dialog's Esc
    else if (e.key === 'Tab') close();
    else if (!cur.search && e.key.length === 1) { // type-ahead in short lists
      const i = cur.shown.findIndex(it => it.text.toLowerCase().startsWith(e.key.toLowerCase()));
      if (i >= 0) { cur.kb = i; render(); }
    }
  });
  document.addEventListener('mousedown', e => { if (cur && !pop.contains(e.target) && !cur.btn.contains(e.target)) close(); }, true);
  addEventListener('resize', () => close());
  document.addEventListener('scroll', e => { if (cur && !pop.contains(e.target)) close(); }, true);

  // pick up every select, now and later (pages, dialogs, popovers)
  const scan = root => {
    if (root.tagName === 'SELECT') enhance(root);
    else if (root.querySelectorAll) root.querySelectorAll('select').forEach(enhance);
  };
  new MutationObserver(ms => ms.forEach(m => m.addedNodes.forEach(n => n.nodeType === 1 && scan(n)))).observe(document.body, { childList: true, subtree: true });
  scan(document.body);

  return { enhance, close, sync, isOpen: () => !!cur };
})();

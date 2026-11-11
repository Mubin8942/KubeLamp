'use strict';
/* Sign-in, the forced password change, the user menu, and (for admins) user management. */

const Auth = (() => {
  let shown = false;

  const screen = (inner) => {
    $('#authScreen')?.remove();
    const el = document.createElement('div');
    el.id = 'authScreen';
    el.className = 'auth-screen';
    el.innerHTML = `<div class="auth-card card">${inner}</div>`;
    document.body.appendChild(el);
    document.body.classList.add('auth-on');
    return el;
  };
  const close = () => { $('#authScreen')?.remove(); document.body.classList.remove('auth-on'); shown = false; };
  const head = (title, sub) => `<div class="auth-head"><img src="logo.png" alt="" width="48" height="48"><h1>${esc(title)}</h1>${sub ? `<p>${sub}</p>` : ''}</div>`;
  const post = async (path, body) => {
    const r = await fetch(apiURL(path), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) throw httpErr(r, d);
    return d;
  };

  // resolves with the signed-in user
  function signIn(message) {
    if (shown) return new Promise(() => {});
    shown = true;
    S.timers.forEach(clearInterval); S.timers = []; S.abort?.abort();
    return new Promise(resolve => {
      const el = screen(`${head('Sign in to KubeLamp', message ? esc(message) : 'Use the account your administrator gave you.')}
        <form class="auth-form" id="authForm" autocomplete="on">
          <label class="kc-f"><span>Username</span><input type="text" id="auUser" autocomplete="username" autocapitalize="off" spellcheck="false" required></label>
          <label class="kc-f"><span>Password</span><input type="password" id="auPass" autocomplete="current-password" required></label>
          <div class="auth-err" id="auErr" role="alert"></div>
          <button class="btn primary auth-go" type="submit">Sign in</button>
        </form>`);
      $('#auUser', el).focus();
      $('#authForm', el).addEventListener('submit', async e => {
        e.preventDefault();
        const b = $('.auth-go', el); b.disabled = true; $('#auErr', el).textContent = '';
        try {
          const d = await post('auth/login', { username: $('#auUser', el).value.trim(), password: $('#auPass', el).value });
          close();
          resolve(d.user);
        } catch (err) {
          $('#auErr', el).textContent = err.message;
          $('#auPass', el).value = ''; $('#auPass', el).focus();
        } finally { b.disabled = false; }
      });
    });
  }

  // forced (temporary password) or voluntary; resolves with the updated user, or null if cancelled
  function changePassword(user, forced) {
    shown = true;
    return new Promise(resolve => {
      const el = screen(`${head(forced ? 'Choose your password' : 'Change password',
        forced ? `Hi <b>${esc(user.username)}</b> — you signed in with a temporary password. Set your own to continue.` : `Signed in as <b>${esc(user.username)}</b>. Other browsers are signed out after the change.`)}
        <form class="auth-form" id="pwForm">
          <input type="text" autocomplete="username" value="${esc(user.username)}" hidden>
          <label class="kc-f"><span>${forced ? 'Temporary password' : 'Current password'}</span><input type="password" id="pwCur" autocomplete="current-password" required></label>
          <label class="kc-f"><span>New password</span><input type="password" id="pwNew" autocomplete="new-password" minlength="8" required></label>
          <label class="kc-f"><span>Repeat new password</span><input type="password" id="pwRep" autocomplete="new-password" minlength="8" required></label>
          <p class="muted auth-hint">At least 8 characters, not your username.</p>
          <div class="auth-err" id="pwErr" role="alert"></div>
          <button class="btn primary auth-go" type="submit">${forced ? 'Set password and continue' : 'Change password'}</button>
          <button class="btn ghost" type="button" id="pwAlt">${forced ? 'Sign out' : 'Cancel'}</button>
        </form>`);
      $('#pwCur', el).focus();
      $('#pwAlt', el).onclick = async () => { if (forced) { await signOut(); } else { close(); resolve(null); } };
      $('#pwForm', el).addEventListener('submit', async e => {
        e.preventDefault();
        const err = $('#pwErr', el);
        if ($('#pwNew', el).value !== $('#pwRep', el).value) { err.textContent = 'The new passwords don\'t match.'; return; }
        const b = $('.auth-go', el); b.disabled = true; err.textContent = '';
        try {
          const d = await post('auth/password', { current: $('#pwCur', el).value, new: $('#pwNew', el).value });
          close();
          toast('Password changed');
          resolve(d.user);
        } catch (x) { err.textContent = x.message; } finally { b.disabled = false; }
      });
    });
  }

  async function signOut() {
    await fetch(apiURL('auth/logout'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).catch(() => {});
    try { sessionStorage.clear(); } catch {}
    location.replace(location.pathname); // fresh page: nothing of the old user stays in memory
  }

  // before the app starts: a signed-in user who has a real password
  async function ready() {
    let user = null;
    const r = await fetch(apiURL('auth/me'));
    if (r.ok) user = (await r.json()).user;
    if (!user) user = await signIn();
    while (user.mustChange) user = await changePassword(user, true) || user;
    return user;
  }

  function menu(anchor) {
    const u = S.info?.user || {};
    openPop(anchor, [
      { head: `${u.username || ''} · ${u.role === 'admin' ? 'admin' : 'user'}` },
      { label: 'Settings', icon: ICON.edit, run: () => { location.hash = '#/settings'; } },
      { label: 'Change password', icon: ICON.edit, run: () => changePassword(u, false) },
      'sep',
      { label: 'Sign out', icon: ICON.del, run: signOut },
    ]);
  }

  return { signIn, changePassword, signOut, ready, menu };
})();

// ------------------------------------------------------------------ admin: users (Settings page)
async function usersSection(box) {
  const me = S.info?.user || {};
  const load = async () => {
    let list;
    try { list = await api('users'); } catch (e) { box.innerHTML = `<p class="crit-t">${esc(e.message)}</p>`; return; }
    box.innerHTML = `<div class="tbl-wrap"><table class="t"><thead><tr><th>User</th><th>Role</th><th>Status</th><th>Created</th><th></th></tr></thead><tbody>
      ${list.map(u => `<tr data-id="${esc(u.id)}"><td class="name">${esc(u.username)}${u.id === me.id ? ' <span class="tag cur">you</span>' : ''}</td>
        <td>${u.role === 'admin' ? '<span class="kind">admin</span>' : 'user'}</td>
        <td>${u.mustChange ? stLabel('Temporary password', 'warning') : stLabel('Active', 'good')}</td>
        <td class="muted">${age(u.created)} ago</td>
        <td class="act"><button class="icon-btn" data-menu title="Actions">${ICON.more}</button></td></tr>`).join('')}</tbody></table></div>`;
    $$('[data-menu]', box).forEach(b => b.addEventListener('click', e => {
      e.stopPropagation();
      const u = list.find(x => x.id === b.closest('tr').dataset.id);
      const self = u.id === me.id;
      openPop(b, [
        { label: 'Reset password…', icon: ICON.edit, run: () => resetPw(u) },
        ...(self ? [] : [{ label: u.role === 'admin' ? 'Make regular user' : 'Make admin', icon: ICON.scale, run: () => setRole(u, u.role === 'admin' ? 'user' : 'admin') }]),
        ...(self ? [] : ['sep', { label: 'Delete user', icon: ICON.del, danger: true, run: () => del(u) }]),
      ]);
    }));
  };
  const genPw = () => { const a = new Uint8Array(12); crypto.getRandomValues(a); return Array.from(a, x => 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'[x % 55]).join(''); };
  const pwField = (id) => `<label class="field">Temporary password<span class="pw-row"><input type="text" id="${id}" autocomplete="off" spellcheck="false" minlength="8">
    <button type="button" class="btn sm" data-gen="${id}">Generate</button></span><small>They must change it the first time they sign in. Share it with them privately.</small></label>`;
  const bindGen = d => $$('[data-gen]', d).forEach(b => b.addEventListener('click', () => { $('#' + b.dataset.gen, d).value = genPw(); }));
  const add = () => modal({
    title: 'Add user',
    body: `<label class="field">Username<input type="text" id="nuName" autocomplete="off" spellcheck="false" autocapitalize="off"></label>
      ${pwField('nuPw')}
      <label class="field">Role<select id="nuRole"><option value="user">User — their own kubeconfigs and clusters</option><option value="admin">Admin — also manages users and server settings</option></select></label>`,
    init: bindGen,
    actions: [{ label: 'Cancel', value: false }, { label: 'Add user', kind: 'primary', submit: true, run: async d => {
      const pw = $('#nuPw', d).value;
      await send('POST', 'users', { username: $('#nuName', d).value.trim(), password: pw, role: $('#nuRole', d).value });
      return true;
    } }],
  }).then(ok => { if (ok) { toast('User added'); load(); } });
  const resetPw = u => modal({
    title: `Reset password for ${u.username}`, body: pwField('rpPw') + '<p>They are signed out everywhere.</p>', init: bindGen,
    actions: [{ label: 'Cancel', value: false }, { label: 'Reset password', kind: 'primary', submit: true, run: async d => {
      await send('PUT', 'users/' + enc(u.id), { password: $('#rpPw', d).value }); return true;
    } }],
  }).then(ok => { if (ok) { toast(`Password reset for ${u.username}`); load(); } });
  const setRole = async (u, role) => {
    try { await send('PUT', 'users/' + enc(u.id), { role }); toast(`${u.username} is now ${role === 'admin' ? 'an admin' : 'a regular user'}`); load(); } catch (e) { toast(e.message, 'err'); }
  };
  const del = async u => {
    if (!await confirmDanger(`Delete ${u.username}?`, 'They are signed out, and their kubeconfigs are deleted from the server. This can\'t be undone.')) return;
    try { await send('DELETE', 'users/' + enc(u.id)); toast(`Deleted ${u.username}`); load(); } catch (e) { toast(e.message, 'err'); }
  };
  $('#usersAdd')?.addEventListener('click', add);
  await load();
}

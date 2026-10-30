/* ==========================================================================
 * MedDesk 管理后台 — 前端脚本
 *
 * 为什么是独立文件：线上 CSP 为 `script-src 'self'`，页面内联 <script> 会被拦截，
 * 因此脚本必须作为同源外部资源提供（由 /admin/app.js 路由，no-cache）。
 *
 * 登录：口令在本机用 PBKDF2-SHA256(210000 次) 派生，只把派生值发给服务端；
 * 服务端再用 argon2id 校验并签发独立的后台 token（scope=admin，默认 8 小时）。
 * ========================================================================== */
(function () {
  'use strict';

  var API = '/api/v1';
  var TOKEN_KEY = 'meddesk.admin.token';
  var EMAIL_KEY = 'meddesk.admin.email';

  var state = {
    token: sessionStorage.getItem(TOKEN_KEY) || '',
    email: sessionStorage.getItem(EMAIL_KEY) || '',
    page: 1,
    size: 20,
    q: '',
    total: 0,
    items: [],
    busy: false
  };

  function $(id) { return document.getElementById(id); }

  /* ------------------------------ 工具 ------------------------------ */

  function toast(msg, isErr) {
    var el = $('toast');
    el.textContent = msg;
    el.className = 'toast toast--show' + (isErr ? ' toast--err' : '');
    clearTimeout(toast._t);
    toast._t = setTimeout(function () { el.className = 'toast'; }, 2600);
  }

  function b64(bytes) {
    var bin = '';
    for (var i = 0; i < bytes.length; i += 0x8000) {
      bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
    }
    return btoa(bin);
  }

  function b64ToBytes(text) {
    var bin = atob(text);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // 与 web/assets/js/crypto.js 的 derive() 完全一致：PBKDF2-SHA256，210000 次，32 字节。
  async function derivePasswordHash(password, saltB64) {
    var key = await crypto.subtle.importKey('raw', new TextEncoder().encode(password), 'PBKDF2', false, ['deriveBits']);
    var bits = await crypto.subtle.deriveBits({
      name: 'PBKDF2', hash: 'SHA-256', salt: b64ToBytes(saltB64), iterations: 210000
    }, key, 256);
    return b64(new Uint8Array(bits));
  }

  async function api(path, options) {
    options = options || {};
    var headers = { 'Content-Type': 'application/json' };
    if (state.token) headers.Authorization = 'Bearer ' + state.token;
    var resp = await fetch(API + path, {
      method: options.method || 'GET',
      headers: headers,
      body: options.body ? JSON.stringify(options.body) : undefined
    });
    var text = await resp.text();
    var data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
    // 登录/取盐的 401 是业务结果（口令不对），必须原样抛给表单显示；
    // 只有已登录之后的 401 才按「会话失效」处理并回登录页。
    if (resp.status === 401 && !options.raw) {
      logout(true);
      throw new Error('后台会话已失效，请重新登录');
    }
    if (!resp.ok) {
      var msg = (data && (data.message || data.error)) || ('请求失败（' + resp.status + '）');
      throw new Error(msg);
    }
    return data;
  }

  /* ------------------------------ 登录 / 登出 ------------------------------ */

  function showLogin() {
    $('adminView').hidden = true;
    $('authView').hidden = false;
    $('password').value = '';
    $('email').value = state.email || '';
    $('loginErr').textContent = '';
    setTimeout(function () { (state.email ? $('password') : $('email')).focus(); }, 30);
  }

  function showAdmin() {
    $('authView').hidden = true;
    $('adminView').hidden = false;
    $('whoami').textContent = state.email ? ('当前管理员：' + state.email) : '';
    loadAll();
  }

  function logout(silent) {
    state.token = '';
    sessionStorage.removeItem(TOKEN_KEY);
    sessionStorage.removeItem(EMAIL_KEY);
    if (!silent) toast('已退出');
    showLogin();
  }

  async function doLogin(evt) {
    evt.preventDefault();
    var email = ($('email').value || '').trim().toLowerCase();
    var password = $('password').value || '';
    var errEl = $('loginErr');
    var btn = $('loginBtn');
    errEl.textContent = '';
    if (!email || !password) { errEl.textContent = '请填写邮箱和口令'; return; }

    btn.disabled = true;
    btn.textContent = '登录中…';
    try {
      // 先取 KDF 盐：优先后台独立凭据的固定盐；未配置时退回该账号自身的盐。
      var saltResp;
      try {
        saltResp = await api('/admin/salt', { raw: true });
      } catch (e) {
        saltResp = await api('/auth/salt', { method: 'POST', body: { email: email }, raw: true });
      }
      var passwordHash = await derivePasswordHash(password, saltResp.kdfSalt);
      var out = await api('/admin/login', { method: 'POST', body: { email: email, passwordHash: passwordHash }, raw: true });
      state.token = out.token;
      state.email = out.email || email;
      sessionStorage.setItem(TOKEN_KEY, state.token);
      sessionStorage.setItem(EMAIL_KEY, state.email);
      showAdmin();
    } catch (e) {
      errEl.textContent = e.message || '登录失败';
    } finally {
      btn.disabled = false;
      btn.textContent = '登 录';
    }
  }

  /* ------------------------------ 数据加载 ------------------------------ */

  async function loadOverview() {
    try {
      var data = await api('/admin/overview');
      $('statUsers').textContent = data.users;
      $('statRecords').textContent = data.records;
      $('statGrants').textContent = data.activeGrants;
    } catch (e) { /* 概览失败不阻断列表 */ }
  }

  async function loadUsers() {
    var tbody = $('rows');
    tbody.textContent = '';
    var loading = document.createElement('tr');
    var td = document.createElement('td');
    td.colSpan = 4;
    td.className = 'empty';
    td.textContent = '加载中…';
    loading.appendChild(td);
    tbody.appendChild(loading);

    var qs = '/admin/users?page=' + state.page + '&size=' + state.size + (state.q ? ('&q=' + encodeURIComponent(state.q)) : '');
    var data = await api(qs);
    state.items = data.items || [];
    state.total = data.total || 0;
    renderRows();
    renderPager();
  }

  function renderRows() {
    var tbody = $('rows');
    tbody.textContent = '';
    if (!state.items.length) {
      var tr = document.createElement('tr');
      var td = document.createElement('td');
      td.colSpan = 4;
      td.className = 'empty';
      td.textContent = state.q ? '没有匹配的用户' : '还没有用户';
      tr.appendChild(td);
      tbody.appendChild(tr);
      return;
    }
    state.items.forEach(function (u) {
      var tr = document.createElement('tr');

      var tdEmail = document.createElement('td');
      tdEmail.dataset.label = '邮箱';
      tdEmail.textContent = u.email;
      tdEmail.style.wordBreak = 'break-all';

      // 恢复能力直接标在邮箱下面：管理员改口令前要一眼看出"这人能不能完全恢复"。
      if (u.recoveryEnabled || u.envelopeStale) {
        var marks = document.createElement('div');
        marks.style.cssText = 'margin-top:5px;display:flex;gap:6px;flex-wrap:wrap';
        function mark(text, color) {
          var sp = document.createElement('span');
          sp.textContent = text;
          sp.style.cssText = 'font-size:11px;line-height:1.6;padding:1px 7px;border-radius:999px;'
            + 'border:1px solid ' + color + ';color:' + color;
          return sp;
        }
        if (u.recoveryEnabled) marks.appendChild(mark('托管恢复可用', 'var(--ok, #1f9d55)'));
        if (u.envelopeStale) marks.appendChild(mark('待设备自愈', 'var(--warn, #b7791f)'));
        tdEmail.appendChild(marks);
      }

      var tdCreated = document.createElement('td');
      tdCreated.dataset.label = '创建时间';
      tdCreated.className = 'mono';
      tdCreated.textContent = fmtDate(u.createdAt);

      var tdCount = document.createElement('td');
      tdCount.dataset.label = '密文记录';
      tdCount.className = 'mono';
      tdCount.textContent = String(u.recordCount || 0);

      var tdActs = document.createElement('td');
      tdActs.dataset.label = '操作';
      tdActs.style.textAlign = 'right';
      tdActs.style.whiteSpace = 'nowrap';

      tdActs.appendChild(iconBtn('编辑邮箱', ICON_EDIT, function () { openEditEmail(u); }));
      tdActs.appendChild(iconBtn('改口令（默认保留数据）', ICON_KEY, function () { openResetPassword(u); }));
      tdActs.appendChild(iconBtn('删除用户', ICON_TRASH, function () { openDelete(u); }, true));

      tr.appendChild(tdEmail);
      tr.appendChild(tdCreated);
      tr.appendChild(tdCount);
      tr.appendChild(tdActs);
      tbody.appendChild(tr);
    });
  }

  function renderPager() {
    var pages = Math.max(1, Math.ceil(state.total / state.size));
    $('pagerInfo').textContent = '共 ' + state.total + ' 个用户 · 第 ' + state.page + ' / ' + pages + ' 页';
    $('prevBtn').disabled = state.page <= 1;
    $('nextBtn').disabled = state.page >= pages;
  }

  function fmtDate(v) {
    if (!v) return '–';
    var d = new Date(v);
    if (isNaN(d.getTime())) return '–';
    function p(n) { return n < 10 ? '0' + n : '' + n; }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  async function loadAll() {
    await loadOverview();
    try { await loadUsers(); } catch (e) { toast(e.message, true); }
  }

  /* ------------------------------ 图标 ------------------------------ */

  var ICON_EDIT = 'M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25ZM20.71 7.04a1 1 0 0 0 0-1.41l-2.34-2.34a1 1 0 0 0-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83Z';
  var ICON_KEY = 'M12.65 10A5.99 5.99 0 0 0 7 6a6 6 0 1 0 5.65 8H14v2h2v-2h2v-2h-2v-2h-3.35ZM7 14a2 2 0 1 1 0-4 2 2 0 0 1 0 4Z';
  var ICON_TRASH = 'M6 19a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V7H6v12ZM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4Z';

  function iconBtn(title, path, onClick, danger) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'icon-btn' + (danger ? ' icon-btn--danger' : '');
    b.title = title;
    b.setAttribute('aria-label', title);
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('aria-hidden', 'true');
    var p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', path);
    svg.appendChild(p);
    b.appendChild(svg);
    b.addEventListener('click', onClick);
    return b;
  }

  /* ------------------------------ 弹窗 ------------------------------ */

  function openSheet(title, desc, bodyNodes, confirmText, onConfirm, opts) {
    opts = opts || {};
    var sheet = $('sheet');
    sheet.textContent = '';

    var h = document.createElement('h2');
    h.className = 'sheet__title';
    h.textContent = title;
    sheet.appendChild(h);

    if (desc) {
      var p = document.createElement('p');
      p.className = 'sheet__desc';
      p.textContent = desc;
      sheet.appendChild(p);
    }

    if (opts.warn) {
      var w = document.createElement('div');
      w.className = 'warn';
      w.textContent = opts.warn;
      sheet.appendChild(w);
    }

    (bodyNodes || []).forEach(function (n) { sheet.appendChild(n); });

    var errEl = document.createElement('div');
    errEl.className = 'err';
    sheet.appendChild(errEl);

    var acts = document.createElement('div');
    acts.className = 'sheet__acts';

    var cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'btn btn--ghost';
    cancel.textContent = '取消';
    cancel.addEventListener('click', closeSheet);

    var ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'btn' + (opts.danger ? ' btn--danger' : '');
    ok.textContent = confirmText || '确定';
    ok.addEventListener('click', async function () {
      ok.disabled = true;
      var label = ok.textContent;
      ok.textContent = '处理中…';
      try {
        await onConfirm(errEl);
        closeSheet();
      } catch (e) {
        errEl.textContent = e.message || '操作失败';
      } finally {
        ok.disabled = false;
        ok.textContent = label;
      }
    });

    acts.appendChild(cancel);
    acts.appendChild(ok);
    sheet.appendChild(acts);

    $('mask').hidden = false;
    var first = sheet.querySelector('input');
    if (first) setTimeout(function () { first.focus(); }, 30);
  }

  function closeSheet() {
    $('mask').hidden = true;
    $('sheet').textContent = '';
  }

  function field(labelText, inputEl, hintText) {
    var wrap = document.createElement('div');
    wrap.className = 'field';
    var label = document.createElement('label');
    label.className = 'field__label';
    label.textContent = labelText;
    wrap.appendChild(label);
    wrap.appendChild(inputEl);
    if (hintText) {
      var hint = document.createElement('p');
      hint.className = 'hint';
      hint.textContent = hintText;
      wrap.appendChild(hint);
    }
    return wrap;
  }

  function input(attrs) {
    var el = document.createElement('input');
    el.className = 'input';
    Object.keys(attrs || {}).forEach(function (k) { el.setAttribute(k, attrs[k]); });
    return el;
  }

  function openAddUser() {
    var email = input({ type: 'email', placeholder: 'new-user@example.com', inputmode: 'email', autocomplete: 'off' });
    var pwd = input({ type: 'text', placeholder: '至少 8 位', autocomplete: 'off' });
    openSheet('添加用户', '服务端会为该账号生成独立的加密密钥材料；用户用此口令登录后即可正常使用。', [
      field('邮箱', email),
      field('初始口令', pwd, '请通过安全渠道告知对方，并建议登录后自行修改。')
    ], '创建', async function (errEl) {
      var e = (email.value || '').trim().toLowerCase();
      var p = pwd.value || '';
      if (!e) throw new Error('请填写邮箱');
      if (p.length < 8) throw new Error('口令至少 8 位');
      await api('/admin/users', { method: 'POST', body: { email: e, password: p } });
      toast('已创建 ' + e);
      state.page = 1;
      loadAll();
    });
  }

  function openEditEmail(u) {
    var email = input({ type: 'email', value: u.email, inputmode: 'email', autocomplete: 'off' });
    email.value = u.email;
    openSheet('编辑用户信息', '邮箱是登录标识，修改后用户需用新邮箱登录；数据与口令均不受影响。', [
      field('邮箱', email)
    ], '保存', async function () {
      var e = (email.value || '').trim().toLowerCase();
      if (!e) throw new Error('请填写邮箱');
      await api('/admin/users/' + encodeURIComponent(u.id), { method: 'PATCH', body: { email: e } });
      toast('已更新');
      loadUsers();
    });
  }

  // 改口令：三种模式，默认「保留数据」。
  //
  // 【为什么不再是单一"重置"】
  // 端到端加密下，服务端没有明文 DEK。旧实现直接换一把新 DEK → 旧密文永久作废。
  // 现在的默认路径只换口令与盐、保留信封，用户在仍持有 DEK 的设备上登录即可自愈；
  // 账号若开启了托管恢复，服务端还能直接重包裹，任何设备立刻可用。
  function openResetPassword(u) {
    var pwd = input({ type: 'text', placeholder: '新口令，至少 8 位', autocomplete: 'off' });

    var mode = document.createElement('select');
    mode.className = 'input';
    var options = [
      { v: 'keep', t: '直接改口令 · 数据保留（推荐）' },
      { v: 'escrow', t: '直接改口令 · 走服务端托管恢复（该账号已开启才可用）' },
      { v: 'wipe', t: '重置口令 · 清空云端密文（旧行为，数据作废）' }
    ];
    options.forEach(function (o) {
      var opt = document.createElement('option');
      opt.value = o.v;
      opt.textContent = o.t;
      mode.appendChild(opt);
    });
    // 开了托管的账号优先走 escrow：任何设备都能立刻用新口令，不留"要找回旧设备"的尾巴。
    mode.value = u.recoveryEnabled ? 'escrow' : 'keep';
    if (!u.recoveryEnabled) {
      // 没开托管就别让人选到一个必然失败的选项（服务端也会拒，但提示更晚）。
      mode.options[1].disabled = true;
    }

    var hint = document.createElement('p');
    hint.className = 'muted';
    hint.style.cssText = 'font-size:12px;line-height:1.7;margin:0 0 12px';

    var agree = document.createElement('label');
    agree.className = 'checkbox';
    agree.style.display = 'none';
    var cb = document.createElement('input');
    cb.type = 'checkbox';
    var span = document.createElement('span');
    span.textContent = '我明白：这会换一把新的数据密钥并清空该账号已同步的云端密文，数据不可恢复。';
    agree.appendChild(cb);
    agree.appendChild(span);

    function paintHint() {
      if (mode.value === 'keep') {
        hint.textContent = '只更换登录口令与盐，服务器上的密文与密钥信封一律不动。'
          + '用户下次在一台曾登录过该账号的设备上打开时，会自动用本机数据密钥完成密钥自愈；'
          + '在新设备上登录前，需要先在那台旧设备上打开一次。';
        agree.style.display = 'none';
      } else if (mode.value === 'escrow') {
        hint.textContent = '该账号已开启托管恢复：服务端解出数据密钥后用新口令重新包裹，'
          + '所有数据完整保留，任何设备都能立刻用新口令登录。';
        agree.style.display = 'none';
      } else {
        hint.textContent = '换一把新数据密钥并清空该账号的云端密文。只有在数据本就该作废、'
          + '或账号凭据已泄露时才用这个模式。';
        agree.style.display = '';
      }
    }
    paintHint();
    mode.addEventListener('change', paintHint);

    openSheet('修改登录口令', '账号：' + u.email, [
      field('新口令', pwd),
      field('数据如何处理', mode),
      hint,
      agree
    ], '确认修改', async function () {
      var p = (pwd.value || '').trim();
      if (p.length < 8) throw new Error('口令至少 8 位');
      if (mode.value === 'wipe' && !cb.checked) throw new Error('清空数据前请先勾选确认');
      var res = await api('/admin/users/' + encodeURIComponent(u.id) + '/password', {
        method: 'POST',
        body: { password: p, mode: mode.value }
      });
      if (res && res.mode === 'escrow') {
        toast('口令已修改，数据完整保留；用户可在任意设备用新口令登录');
      } else if (res && res.mode === 'keep') {
        toast('口令已修改，数据保留；用户需在一台曾登录过的设备上打开一次以完成密钥自愈');
      } else {
        toast('口令已重置，云端密文已清空');
      }
      loadAll();
    }, { danger: mode.value === 'wipe' });
  }

  function openDelete(u) {
    var confirm = input({ type: 'text', placeholder: '输入该用户邮箱以确认', autocomplete: 'off' });
    openSheet('删除用户', '账号：' + u.email, [
      field('确认邮箱', confirm, '删除将同时清空该账号的会话、同步密文、家庭授权与附件，不可恢复。')
    ], '永久删除', async function () {
      if ((confirm.value || '').trim().toLowerCase() !== String(u.email).toLowerCase()) {
        throw new Error('邮箱不匹配');
      }
      await api('/admin/users/' + encodeURIComponent(u.id), { method: 'DELETE' });
      toast('已删除 ' + u.email);
      if (state.items.length === 1 && state.page > 1) state.page -= 1;
      loadAll();
    }, { danger: true, warn: '此操作不可撤销。' });
  }

  /* ------------------------------ 事件绑定 ------------------------------ */

  function bind() {
    $('loginForm').addEventListener('submit', doLogin);
    $('logoutBtn').addEventListener('click', function () { logout(false); });
    $('refreshBtn').addEventListener('click', function () { loadAll(); });
    $('addBtn').addEventListener('click', openAddUser);
    $('mask').addEventListener('click', function (e) { if (e.target === $('mask')) closeSheet(); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeSheet(); });

    var timer = null;
    $('search').addEventListener('input', function () {
      clearTimeout(timer);
      var v = $('search').value.trim();
      timer = setTimeout(function () {
        state.q = v;
        state.page = 1;
        loadUsers().catch(function (e) { toast(e.message, true); });
      }, 260);
    });

    $('prevBtn').addEventListener('click', function () {
      if (state.page > 1) { state.page -= 1; loadUsers().catch(function (e) { toast(e.message, true); }); }
    });
    $('nextBtn').addEventListener('click', function () {
      if (state.page * state.size < state.total) { state.page += 1; loadUsers().catch(function (e) { toast(e.message, true); }); }
    });
  }

  /* ------------------------------ 启动 ------------------------------ */

  async function boot() {
    bind();
    if (!state.token) { showLogin(); return; }
    // 有 token 先用一次轻量请求验证是否仍有效
    try {
      await api('/admin/overview');
      showAdmin();
    } catch (e) {
      logout(true);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();

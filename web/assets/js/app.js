(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var doc = window.document;
  var authScreen;

  function buttonWithIcon(label, icon, className, onClick) {
    return MD.ui.el('button', {
      class: className || 'btn btn-ghost',
      type: 'button',
      onClick: onClick
    }, [MD.icon(icon), MD.ui.el('span', { text: label })]);
  }

  function setupChrome() {
    var app = doc.getElementById('app');
    var sidebar = doc.getElementById('sidebar');
    var scrim = doc.getElementById('scrim');
    var brand = doc.getElementById('brand');
    var sidebarFoot = doc.getElementById('sidebar-foot');
    var menuButton = doc.getElementById('btn-menu');
    var themeButton = doc.getElementById('btn-theme');
    var accountButton = doc.getElementById('btn-account');

    brand.appendChild(MD.ui.el('div', { class: 'brand-lockup' }, [MD.icon('pill'), MD.ui.el('span', { text: 'MedDesk 药事台' })]));
    sidebarFoot.textContent = '安全、离线优先 · v' + MD.version + ' · ' + String(MD.buildID || 'dev').slice(0, 8);
    menuButton.appendChild(MD.icon('menu'));
    accountButton.appendChild(MD.icon('user'));

    try {
      if (window.localStorage.getItem('meddesk.sidebar') === 'collapsed') app.classList.add('sidebar-collapsed');
    } catch (_) {
      // 本地偏好不可用时保持默认布局。
    }

    function closeSidebar() {
      sidebar.classList.remove('is-open');
      scrim.hidden = true;
    }

    menuButton.addEventListener('click', function () {
      MD.haptic.tap();
      if (window.matchMedia('(min-width: 1024px)').matches) {
        app.classList.toggle('sidebar-collapsed');
        try { window.localStorage.setItem('meddesk.sidebar', app.classList.contains('sidebar-collapsed') ? 'collapsed' : 'open'); } catch (_) { /* 忽略存储失败。 */ }
      } else {
        sidebar.classList.toggle('is-open');
        scrim.hidden = !sidebar.classList.contains('is-open');
      }
    });
    scrim.addEventListener('click', closeSidebar);

    function renderThemeButton() {
      MD.ui.clear(themeButton);
      var dark = MD.theme.resolved() === 'dark';
      themeButton.appendChild(MD.icon(dark ? 'sun' : 'moon'));
      themeButton.setAttribute('aria-label', dark ? '切换到浅色主题' : '切换到深色主题');
      themeButton.title = dark ? '浅色主题' : '深色主题';
    }
    renderThemeButton();
    MD.theme.onChange(renderThemeButton);
    themeButton.addEventListener('click', function () {
      MD.haptic.tap();
      MD.theme.toggle();
    });
    accountButton.addEventListener('click', openAccountSheet);
  }

  function openAccountSheet() {
    var session = MD.api.getSession();
    MD.haptic.tap();
    MD.ui.sheet({
      title: '账户',
      body: function (el) {
        el.appendChild(MD.ui.el('div', { class: 'stack' }, [
          MD.ui.el('div', { class: 'card card--flat' }, [
            MD.ui.el('strong', { text: '当前账户' }),
            MD.ui.el('div', { class: 'muted', text: session.userId || '未登录' })
          ]),
          MD.ui.el('p', { class: 'muted', text: '业务数据使用端到端加密保护，解密密钥不会发送到服务端。' })
        ]));
      },
      actions: [
        { label: '关闭', kind: 'ghost' },
        {
          label: '退出登录',
          kind: 'danger',
          onClick: async function () {
            await MD.api.logout();
            MD.sync.stopAuto();
            MD.sync.startAuto(60000);
            showAuth('login');
            MD.toast.show('已退出登录', 'success');
          }
        }
      ]
    });
  }

  function registerRoutes() {
    MD.router.register({
      id: 'settings',
      title: '设置',
      subtitle: '同步、备份与隐私',
      icon: 'settings',
      group: '系统',
      order: 40,
      comingSoon: false,
      render: renderSettings
    });

    [
      { id: 'health', title: '健康记录', subtitle: '体征与健康趋势', icon: 'heart', order: 100 },
      { id: 'todo', title: '日程待办', subtitle: '家庭日程与任务', icon: 'calendar', order: 110 },
      { id: 'ledger', title: '家庭账本', subtitle: '共同收支记录', icon: 'wallet', order: 120 }
    ].forEach(function (route) {
      MD.router.register({
        id: route.id,
        title: route.title,
        subtitle: route.subtitle,
        icon: route.icon,
        group: '更多',
        order: route.order,
        comingSoon: true,
        render: function (el) {
          el.appendChild(MD.ui.el('div', { class: 'card empty' }, [MD.icon(route.icon), MD.ui.el('p', { text: '即将上线' })]));
        }
      });
    });
  }

  function registerMedicationRoute() {
    if (MD.med && typeof MD.med.register === 'function') {
      MD.med.register();
      return;
    }
    MD.router.register({
      id: 'medication',
      title: '用药管理',
      subtitle: '今日用药与药品档案',
      icon: 'pill',
      group: '核心',
      order: 10,
      comingSoon: false,
      render: function (el) {
        el.appendChild(MD.ui.el('div', { class: 'card empty' }, [
          MD.icon('pill'),
          MD.ui.el('strong', { text: '用药模块正在加载' }),
          MD.ui.el('p', { class: 'muted', text: '平台层已就绪，请稍后刷新页面。' })
        ]));
      }
    });
  }

  function settingRow(title, description, control) {
    return MD.ui.el('div', { class: 'setting-item' }, [
      MD.ui.el('div', { class: 'setting-item__copy' }, [
        MD.ui.el('span', { class: 'setting-item__title', text: title }),
        MD.ui.el('span', { class: 'setting-item__desc', text: description })
      ]),
      control
    ]);
  }

  function themeControl() {
    var segment = MD.ui.el('div', { class: 'seg', role: 'group', 'aria-label': '主题模式' });
    [
      { id: 'auto', label: '自动' },
      { id: 'light', label: '浅色' },
      { id: 'dark', label: '深色' }
    ].forEach(function (option) {
      var active = MD.theme.get() === option.id;
      segment.appendChild(MD.ui.el('button', {
        class: 'seg__item' + (active ? ' is-active' : ''),
        type: 'button',
        'aria-pressed': active ? 'true' : 'false',
        text: option.label,
        onClick: function () {
          MD.haptic.tap();
          MD.theme.set(option.id);
          Array.prototype.forEach.call(segment.children, function (item) {
            var selected = item === this;
            item.classList.toggle('is-active', selected);
            item.setAttribute('aria-pressed', selected ? 'true' : 'false');
          }, this);
        }
      }));
    });
    return segment;
  }

  function downloadJSON(snapshot) {
    var blob = new Blob([JSON.stringify(snapshot, null, 2)], { type: 'application/json;charset=utf-8' });
    var url = URL.createObjectURL(blob);
    var link = doc.createElement('a');
    link.href = url;
    link.download = 'meddesk-backup-' + MD.util.dayKey(MD.util.nowMs()) + '.json';
    link.hidden = true;
    doc.body.appendChild(link);
    link.click();
    link.remove();
    window.setTimeout(function () { URL.revokeObjectURL(url); }, 0);
  }

  function syncLabel(value) {
    return { idle: '已同步', syncing: '同步中', offline: '离线', error: '同步失败' }[value] || '未知';
  }

  function renderSettings(view) {
    var syncDescription = MD.ui.el('span', { class: 'setting-item__desc', text: syncLabel(MD.sync.state) });
    var syncButton = buttonWithIcon('立即同步', 'sync', 'btn btn-ghost', async function () {
      syncButton.disabled = true;
      try {
        var result = await MD.sync.run();
        MD.toast.show('同步完成：上传 ' + result.pushed + ' 条，拉取 ' + result.pulled + ' 条', 'success');
      } catch (error) {
        MD.toast.show(error.message || '同步失败', 'error');
      } finally {
        syncButton.disabled = false;
      }
    });
    var syncRow = MD.ui.el('div', { class: 'setting-item' }, [
      MD.ui.el('div', { class: 'setting-item__copy' }, [
        MD.ui.el('span', { class: 'setting-item__title', text: '同步状态' }),
        syncDescription
      ]),
      syncButton
    ]);
    var stopStateWatch = MD.sync.onChange(function (next) {
      if (!syncDescription.isConnected) {
        stopStateWatch();
        return;
      }
      syncDescription.textContent = syncLabel(next);
    });

    var exportButton = buttonWithIcon('导出', 'download', 'btn btn-ghost', async function () {
      exportButton.disabled = true;
      try {
        downloadJSON(await MD.data.backup.export());
        MD.toast.show('JSON 备份已导出', 'success');
      } catch (error) {
        MD.toast.show(error.message || '导出失败', 'error');
      } finally {
        exportButton.disabled = false;
      }
    });

    var importInput = MD.ui.el('input', { type: 'file', accept: '.json,application/json', hidden: true });
    var importButton = buttonWithIcon('导入', 'upload', 'btn btn-ghost', function () { importInput.click(); });
    importInput.addEventListener('change', async function () {
      var file = importInput.files && importInput.files[0];
      if (!file) return;
      try {
        var confirmed = await MD.ui.confirm({ title: '导入备份', message: '备份将与现有数据合并，同一记录保留更新时间较新的版本。是否继续？', confirmText: '导入' });
        if (!confirmed) return;
        var snapshot = JSON.parse(await file.text());
        await MD.data.backup.import(snapshot);
        MD.toast.show('备份导入完成', 'success');
      } catch (error) {
        MD.toast.show(error.message || '导入失败', 'error');
      } finally {
        importInput.value = '';
      }
    });

    var clearExampleButton = buttonWithIcon('清空示例', 'trash', 'btn btn-ghost', async function () {
      var confirmed = await MD.ui.confirm({ title: '清空示例数据', message: '只会移除首次打开时创建的示例记录。', confirmText: '清空', danger: true });
      if (!confirmed) return;
      var changed = await MD.data.clearExamples();
      MD.toast.show(changed ? '示例数据已清空' : '没有可清空的示例数据', changed ? 'success' : 'info');
    });

    var wipeButton = buttonWithIcon('清空数据', 'trash', 'btn btn-danger', async function () {
      var first = await MD.ui.confirm({ title: '清空全部数据', message: '此操作将清除本机全部业务数据和当前会话，且无法撤销。', confirmText: '继续', danger: true });
      if (!first) return;
      var second = await MD.ui.confirm({ title: '再次确认', message: '请再次确认：立即永久清空本机全部数据？', confirmText: '永久清空', danger: true });
      if (!second) return;
      await MD.data.wipeAll();
      MD.haptic.warn();
      MD.toast.show('本机数据已清空', 'success');
      showAuth('login');
    });

    var passwordButton = buttonWithIcon('修改口令', 'lock', 'btn btn-ghost', function () {
      MD.toast.show('即将上线', 'info');
    });
    passwordButton.disabled = true;
    passwordButton.title = '即将上线';

    var remindButton = buttonWithIcon('开启提醒权限', 'clock', 'btn btn-ghost', async function () {
      if (!MD.remind || typeof MD.remind.ensurePermission !== 'function') {
        MD.toast.show('提醒模块未加载', 'error');
        return;
      }
      remindButton.disabled = true;
      try {
        var permission = await MD.remind.ensurePermission();
        MD.toast.show(permission === 'granted' ? '已开启用药提醒' : '通知权限未授予，请在浏览器设置中允许', permission === 'granted' ? 'success' : 'warn');
      } catch (error) {
        MD.toast.show(error.message || '无法申请通知权限', 'error');
      } finally {
        remindButton.disabled = false;
      }
    });

    var themeCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '外观' }),
      settingRow('主题', '跟随系统或手动选择深浅色', themeControl())
    ]);
    var syncCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '云端同步' }),
      syncRow,
      settingRow('用药提醒', '系统通知需要一次性授权', remindButton),
      settingRow('修改口令', '即将上线', passwordButton)
    ]);
    var backupControls = MD.ui.el('div', { class: 'row row--wrap' }, [exportButton, importButton, importInput]);
    var dataCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '数据管理' }),
      settingRow('JSON 备份', '明文快照仅下载到本机，请妥善保管', backupControls),
      settingRow('示例数据', '移除首次打开时创建的演示记录', clearExampleButton),
      settingRow('清空全部数据', '永久清除本机数据与会话', wipeButton)
    ]);
    var privacyCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '关于与隐私' }),
      settingRow('MedDesk 药事台', '版本 ' + MD.version + ' · 构建 ' + String(MD.buildID || 'dev').slice(0, 8), MD.icon('pill')),
      MD.ui.el('p', { class: 'muted', text: '业务数据采用端到端加密。加密和解密均在当前设备完成，服务端只见密文，无法读取用药记录。JSON 备份为明文，请只保存在可信位置。' })
    ]);

    view.appendChild(MD.ui.el('div', { class: 'stack' }, [themeCard, syncCard, dataCard, privacyCard]));
  }

  function hideAuth() {
    MD.ui.clear(authScreen);
  }

  function authBrand() {
    return MD.ui.el('div', { class: 'auth-card__brand' }, [
      MD.icon('pill'),
      MD.ui.el('div', {}, [
        MD.ui.el('h1', { text: 'MedDesk 药事台' }),
        MD.ui.el('p', { class: 'muted', text: '安全管理每一次用药' })
      ])
    ]);
  }

  function showUnlock() {
    MD.ui.clear(authScreen);
    var password = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: 'current-password', required: true, placeholder: '输入口令' });
    var errorText = MD.ui.el('div', { class: 'auth-error', role: 'alert' });
    var submit = MD.ui.el('button', { class: 'btn btn-primary', type: 'submit', text: '解锁' });
    var form = MD.ui.el('form', { class: 'stack' }, [
      MD.ui.el('div', { class: 'field' }, [MD.ui.el('label', { class: 'label', text: '口令' }), password]),
      errorText,
      submit,
      MD.ui.el('button', {
        class: 'btn btn-ghost',
        type: 'button',
        text: '使用其他账户',
        onClick: function () { MD.api.clearSession(); showAuth('login'); }
      })
    ]);
    form.addEventListener('submit', async function (event) {
      event.preventDefault();
      submit.disabled = true;
      errorText.textContent = '';
      try {
        var session = MD.api.getSession();
        var dek = await MD.crypto.unwrapDEK(session.wrappedDek, session.dekIv, password.value, MD.crypto.b64ToBytes(session.kdfSalt));
        MD.api.setDEK(dek);
        hideAuth();
        await afterAuthenticated();
      } catch (_) {
        errorText.textContent = '口令错误或加密凭据无效';
        MD.haptic.warn();
      } finally {
        submit.disabled = false;
      }
    });
    authScreen.appendChild(MD.ui.el('section', { class: 'auth-card card' }, [authBrand(), MD.ui.el('h2', { class: 'section-title', text: '解锁本地数据' }), MD.ui.el('p', { class: 'muted', text: '当前会话需要重新解锁加密密钥。' }), form]));
    window.setTimeout(function () { password.focus(); }, 0);
  }

  function showAuth(mode) {
    mode = mode === 'register' ? 'register' : 'login';
    MD.ui.clear(authScreen);
    var email = MD.ui.el('input', { class: 'input', type: 'email', inputMode: 'email', autocomplete: 'email', required: true, placeholder: 'name@example.com' });
    var password = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: mode === 'register' ? 'new-password' : 'current-password', required: true, minLength: 8, placeholder: '至少 8 位' });
    var confirmPassword = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: 'new-password', required: mode === 'register', minLength: 8, placeholder: '再次输入口令' });
    var errorText = MD.ui.el('div', { class: 'auth-error', role: 'alert' });
    var submit = MD.ui.el('button', { class: 'btn btn-primary', type: 'submit', text: mode === 'register' ? '创建账户' : '登录' });
    var fields = [
      MD.ui.el('div', { class: 'field' }, [MD.ui.el('label', { class: 'label', text: '邮箱' }), email]),
      MD.ui.el('div', { class: 'field' }, [MD.ui.el('label', { class: 'label', text: '口令' }), password])
    ];
    if (mode === 'register') fields.push(MD.ui.el('div', { class: 'field' }, [MD.ui.el('label', { class: 'label', text: '确认口令' }), confirmPassword]));
    fields.push(errorText, submit);
    var form = MD.ui.el('form', { class: 'stack' }, fields);
    form.addEventListener('submit', async function (event) {
      event.preventDefault();
      errorText.textContent = '';
      if (mode === 'register' && password.value !== confirmPassword.value) {
        errorText.textContent = '两次输入的口令不一致';
        return;
      }
      submit.disabled = true;
      submit.textContent = mode === 'register' ? '正在创建…' : '正在登录…';
      try {
        if (mode === 'register') await MD.api.register({ email: email.value, password: password.value });
        else await MD.api.login({ email: email.value, password: password.value });
        hideAuth();
        await afterAuthenticated();
        MD.haptic.success();
        MD.toast.show(mode === 'register' ? '账户创建成功' : '登录成功', 'success');
      } catch (error) {
        errorText.textContent = error.message || (mode === 'register' ? '注册失败' : '登录失败');
        MD.haptic.warn();
      } finally {
        submit.disabled = false;
        submit.textContent = mode === 'register' ? '创建账户' : '登录';
      }
    });

    var switchButton = MD.ui.el('button', {
      class: 'btn btn-ghost',
      type: 'button',
      text: mode === 'register' ? '已有账户，去登录' : '没有账户，立即注册',
      onClick: function () { showAuth(mode === 'register' ? 'login' : 'register'); }
    });
    authScreen.appendChild(MD.ui.el('section', { class: 'auth-card card' }, [
      authBrand(),
      MD.ui.el('h2', { class: 'section-title', text: mode === 'register' ? '创建账户' : '欢迎回来' }),
      MD.ui.el('p', { class: 'muted', text: mode === 'register' ? '口令仅在本机用于派生加密密钥。' : '登录后将在本机解锁端到端加密数据。' }),
      form,
      MD.ui.el('div', { class: 'auth-switch' }, switchButton)
    ]));
    window.setTimeout(function () { email.focus(); }, 0);
  }

  async function afterAuthenticated() {
    // 示例数据（含 1 条逾期、2 个档案、多种频次）由用药模块 med.js 统一预置；
    // 平台层的 MD.data.seedExamples() 仅在其缺失时兜底，避免两套示例数据重复叠加。
    if (!(MD.med && typeof MD.med.seedSamples === 'function')) await MD.data.seedExamples();
    MD.sync.run().catch(function (error) {
      if (MD.sync.state !== 'offline') MD.toast.show(error.message || '同步失败', 'error');
    });
  }

  function restoreSession() {
    if (!MD.api.hasSession()) {
      showAuth('login');
      return;
    }
    if (!MD.api.getDEK()) {
      showUnlock();
      return;
    }
    hideAuth();
    afterAuthenticated().catch(function (error) { MD.toast.show(error.message || '初始化数据失败', 'error'); });
  }

  // 契约 6.4：'auth:changed' 由 MD.api 在登录/登出/401 时 emit，平台层据此同步登录界面。
  function bindAuthEvents() {
    MD.bus.on('auth:changed', function (session) {
      if (session && MD.api.getDEK()) {
        if (authScreen && authScreen.firstChild) hideAuth();
        return;
      }
      if (!session) showAuth('login');
    });
  }

  function registerServiceWorker() {
    if (!('serviceWorker' in window.navigator)) return;
    window.navigator.serviceWorker.register('/sw.js').catch(function () {
      // Service Worker 注册失败不阻断主应用。
    });
  }

  doc.addEventListener('DOMContentLoaded', async function () {
    authScreen = doc.getElementById('auth-screen');
    try {
      await MD.db.init();
      MD.theme.init();
      setupChrome();
      registerRoutes();
      registerMedicationRoute();
      MD.router.start();
      // 契约 6.5：提醒模块需显式 init（探测能力并绑定 SW/可见性事件）。
      if (MD.remind && typeof MD.remind.init === 'function') MD.remind.init();
      bindAuthEvents();
      MD.api.onUnauthorized(function () {
        showAuth('login');
        MD.toast.show('登录已失效，请重新登录', 'warn');
      });
      restoreSession();
      MD.sync.startAuto(60000);
      registerServiceWorker();
    } catch (error) {
      MD.ui.clear(authScreen);
      authScreen.appendChild(MD.ui.el('section', { class: 'auth-card card' }, [
        authBrand(),
        MD.ui.el('h2', { class: 'section-title', text: '应用启动失败' }),
        MD.ui.el('p', { class: 'muted', text: error.message || '无法初始化本地数据' }),
        MD.ui.el('button', { class: 'btn btn-primary', type: 'button', text: '重新加载', onClick: function () { window.location.reload(); } })
      ]));
    }
  });
})(window);

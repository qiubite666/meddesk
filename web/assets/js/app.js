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

    brand.appendChild(MD.ui.el('div', { class: 'brand-lockup' }, [MD.icon('home-heart'), MD.ui.el('span', { text: '丘比特的工作台' })]));
    // 版本号本身已带 v 前缀（服务端 ldflags 注入形如 v0.2.3），不再额外加 'v' 避免出现「vv0.2.3」
    sidebarFoot.textContent = '安全、离线优先 · ' + (MD.version || '未注入') + ' · ' + String(MD.buildID || 'dev').slice(0, 8);
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
        var accountCard = MD.ui.el('div', { class: 'card card--flat' }, [
          MD.ui.el('strong', { text: '当前账户' }),
          MD.ui.el('div', { class: 'muted', text: session.userId || '未登录' })
        ]);
        var box = MD.ui.el('div', { class: 'stack' }, [
          accountCard,
          MD.ui.el('p', { class: 'muted', text: '业务数据使用端到端加密保护，解密密钥不会发送到服务端。' })
        ]);
        el.appendChild(box);

        // 管理员入口：是否显示由服务端判定（/auth/me 的 isAdmin），前端不硬编码邮箱 ——
        // 换管理员只需改 MEDDESK_ADMIN_EMAILS，不用发前端。
        (async function () {
          var info = null;
          try { info = await MD.api.me(); } catch (e) { return; }
          if (!info || !info.user) return;

          // 有邮箱就显示邮箱（比 userId 可读），没有再退回 userId。
          if (info.user.email) {
            MD.ui.clear(accountCard);
            accountCard.appendChild(MD.ui.el('strong', { text: '当前账户' }));
            accountCard.appendChild(MD.ui.el('div', { class: 'muted', text: info.user.email }));
          }
          if (!info.isAdmin) return;

          var adminBtn = MD.ui.el('button', {
            class: 'btn btn-ghost', type: 'button',
            style: { gap: '8px', justifyContent: 'flex-start' },
            onClick: function () {
              // 新标签打开：后台是独立页面（自己的登录态、独立 scope 的令牌），
              // 不能嵌在应用里，否则会和业务会话互相干扰。
              try { window.open('/admin', '_blank', 'noopener'); } catch (e) { /* 被拦截时忽略 */ }
              MD.ui.closeSheet();
            }
          }, [
            MD.icon('user'),
            MD.ui.el('span', { text: '网站后台' })
          ]);
          box.appendChild(adminBtn);
          box.appendChild(MD.ui.el('p', {
            class: 'muted',
            style: { fontSize: '12px', margin: '0' },
            text: '仅管理员可见：管理账号、重置口令、查看同步与授权概况。'
          }));
        })();
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
    // 家庭健康：与「用药提醒」同级（同为 group '健康'，order 2 紧随其后）。
    // 它必须出现在底部标签栏前 4 位里，因此 order 不能排到后面去。
    if (MD.family && typeof MD.family.register === 'function') MD.family.register();

    MD.router.register({
      id: 'settings',
      title: '设置',
      subtitle: '同步、备份与隐私',
      icon: 'settings',
      group: '系统',
      // 侧栏最后一位（低频入口放末尾）；底部栏则在最右侧（第 4 位），
      // 由 tabOrder 单独指定，二者互不影响。
      order: 6,
      tabOrder: 4,
      comingSoon: false,
      render: renderSettings
    });

    [
      // 「健康记录」已由「家庭健康」取代（家庭健康是真实功能，不再是占位）
      // 「记账本」占位已删除：真实实现在 ledger.js，它在加载期自行注册
      // 路由 id 'ledger'（order 105，排在日程待办 110 之上）。此处若再保留同 id
      // 项，registerRoutes() 在 DOMContentLoaded 里执行时会把真模块覆盖成「即将上线」。
      { id: 'todo', title: '日程待办', subtitle: '家庭日程与任务', icon: 'calendar', order: 5 }
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
      group: '健康',
      order: 10,
      comingSoon: false,
      render: function (el) {
        el.appendChild(MD.ui.el('div', { class: 'card empty' }, [
          MD.icon('home-heart'),
          MD.ui.el('strong', { text: '用药模块正在加载' }),
          MD.ui.el('p', { class: 'muted', text: '平台层已就绪，请稍后刷新页面。' })
        ]));
      }
    });
  }

  /** 通知能力诊断文案：一眼看清为何收不到通知（不支持 / 未授权 / 已拒绝） */
  function remindDiagText() {
    try {
      if (typeof window.Notification === 'undefined') {
        var ua = String(navigator.userAgent || '');
        if (/iPhone|iPad|iPod/i.test(ua)) return '当前浏览器不支持：请先用 Safari「添加到主屏幕」，再从主屏图标进入';
        return '当前浏览器不支持系统通知（小米/华为等自带浏览器常见）：请改用 Edge 或 Chrome 打开本站';
      }
      if (window.Notification.permission === 'granted') return '已授权 · 系统通知可用';
      if (window.Notification.permission === 'denied') return '已被拒绝：请在浏览器站点设置或系统设置中允许本站通知';
      return '未授权：点右侧按钮授权（安卓还需在系统设置中允许该浏览器通知 + 关闭省电限制）';
    } catch (e) { return '未知（浏览器限制了通知能力查询）'; }
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

  // 修改口令（无损）：数据不重置。
  // 原理：用旧口令解出 DEK，再用新口令重新包裹后提交；服务端只换凭据与信封。
  // 注意：成功后服务端会吊销全部会话（含本机），需要重新登录一次。
  function openChangePasswordSheet() {
    if (!MD.api || typeof MD.api.changePassword !== 'function') {
      MD.toast.show('当前版本不支持修改口令', 'error');
      return;
    }
    var oldIn = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: 'current-password', placeholder: '当前口令' });
    var newIn = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '新口令（至少 8 位）' });
    var repIn = MD.ui.el('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '再输一次新口令' });
    var errBox = MD.ui.el('p', { class: 'muted', style: { color: 'var(--danger)', fontSize: '13px', margin: '0', display: 'none' } });

    MD.ui.sheet({
      title: '修改口令',
      body: function (host) {
        host.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '10px' } }, [
          MD.ui.el('p', {
            class: 'muted',
            style: { fontSize: '12px', margin: '0' },
            text: '只更换登录口令，药物与档案数据全部保留。修改成功后所有设备（含本机）都需要用新口令重新登录一次。'
          }),
          MD.ui.el('div', { class: 'field' }, [MD.ui.el('span', { class: 'label', text: '当前口令' }), oldIn]),
          MD.ui.el('div', { class: 'field' }, [MD.ui.el('span', { class: 'label', text: '新口令' }), newIn]),
          MD.ui.el('div', { class: 'field' }, [MD.ui.el('span', { class: 'label', text: '确认新口令' }), repIn]),
          errBox
        ]));
      },
      actions: [
        { label: '取消', kind: 'ghost', onClick: function () { MD.ui.closeSheet(); } },
        {
          label: '确认修改', kind: 'primary',
          onClick: async function () {
            var oldPwd = String(oldIn.value || '');
            var newPwd = String(newIn.value || '');
            var rep = String(repIn.value || '');
            function fail(msg) {
              errBox.textContent = msg;
              errBox.style.display = '';
              return false;
            }
            errBox.style.display = 'none';
            if (!oldPwd) return fail('请输入当前口令');
            if (newPwd.length < 8) return fail('新口令至少 8 位');
            if (newPwd !== rep) return fail('两次输入的新口令不一致');
            if (newPwd === oldPwd) return fail('新口令不能与当前口令相同');
            try {
              await MD.api.changePassword(oldPwd, newPwd);
            } catch (e) {
              var msg = (e && e.message) ? e.message : '修改失败';
              return fail(msg);
            }
            MD.toast.show('口令已修改，请用新口令重新登录', 'success');
            try { MD.api.clearSession(); } catch (e2) { /* 忽略 */ }
            showAuth('login');
            return true;
          }
        }
      ]
    });
  }

  // 托管恢复：把自己的数据密钥用服务端恢复公钥再包一份，换取「管理员能直接改口令而不丢数据」。
  //
  // 这是明确的安全权衡，所以必须把代价写清楚，由用户自己决定：
  //   · 开启后，持有恢复私钥的一方（服务端/运维）具备解密本账号数据的能力；
  //   · 不开启也能改口令，只是需要在一台曾登录过的设备上打开一次（设备自愈）。
  // 重新渲染当前视图。
  //
  // 【为什么不能调 paint()】paint 是 family.js 内部的函数，app.js 作用域里没有它。
  // 这里走路由层重渲染（go 会再次执行 route.render），与侧边栏切换是同一条路径。
  function refreshCurrentView() {
    try {
      var id = MD.router && typeof MD.router.current === 'function' ? MD.router.current() : '';
      if (id && MD.router && typeof MD.router.go === 'function') MD.router.go(id);
    } catch (e) { /* 渲染失败不影响已经完成的写入 */ }
  }

  function openRecoverySheet(currentlyOn) {
    if (!MD.api || typeof MD.api.setRecoveryEnvelope !== 'function') {
      MD.toast.show('当前版本不支持托管恢复', 'error');
      return;
    }
    var errBox = MD.ui.el('p', {
      class: 'muted',
      style: { color: 'var(--danger)', fontSize: '13px', margin: '0', display: 'none' }
    });
    MD.ui.sheet({
      title: currentlyOn ? '关闭托管恢复' : '开启托管恢复',
      body: function (host) {
        host.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '10px' } }, [
          MD.ui.el('p', {
            class: 'muted',
            style: { fontSize: '13px', margin: '0', lineHeight: '1.7' },
            text: currentlyOn
              ? '关闭后服务端会删除那份恢复信封。之后管理员改你的口令时数据不会丢，但需要你在曾登录过的设备上打开一次才能恢复访问。'
              : '开启后，管理员可以直接修改你的登录口令，所有数据与档案完全保留，任何设备都能立刻用新口令登录。'
          }),
          currentlyOn ? null : MD.ui.el('p', {
            class: 'muted',
            style: { fontSize: '12px', margin: '0', lineHeight: '1.7', color: 'var(--danger)' },
            text: '代价要说清：开启意味着服务器持有一份能解开你数据密钥的恢复信封——持有该私钥的一方可解密你的健康数据。如果你更在意隐私，请不要开启；不开启同样能改口令，只是需要一台旧设备配合。'
          }),
          errBox
        ]));
      },
      actions: [
        { label: '取消', kind: 'ghost', onClick: function () { MD.ui.closeSheet(); return true; } },
        {
          label: currentlyOn ? '确认关闭' : '确认开启',
          kind: currentlyOn ? 'ghost' : 'primary',
          onClick: async function () {
            errBox.style.display = 'none';
            try {
              if (currentlyOn) {
                await MD.api.setRecoveryEnvelope('', '');
                MD.toast.show('已关闭托管恢复', 'success');
                refreshCurrentView();
                return true;
              }
              var dek = MD.api.getDEK();
              if (!dek) throw new Error('本机没有数据密钥，请重新登录后再试');
              var keyInfo = await MD.api.recoveryKey();
              if (!keyInfo || !keyInfo.enabled || !keyInfo.publicKey) {
                throw new Error('本部署未启用托管恢复（服务端未配置恢复密钥）');
              }
              var wrapped = await MD.crypto.wrapForRecovery(dek, keyInfo.publicKey);
              await MD.api.setRecoveryEnvelope(wrapped, '');
              MD.toast.show('已开启托管恢复', 'success');
              refreshCurrentView();
              return true;
            } catch (e) {
              // 注意：刷新视图失败不能算操作失败——写入已经生效了。
              // refreshCurrentView 内部已吞掉渲染异常，能走到这里的基本都是真失败。
              errBox.textContent = (e && e.message) || '操作失败';
              errBox.style.display = '';
              return false;
            }
          }
        }
      ]
    });
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
      // 先清云端：端到端加密下"只清本机"没用——重新登录后 pull 会把服务器上的
      // 旧记录（含用药历史）原样拉回来，看起来就是"删不掉"。
      // 必须在 clearSession 之前调用（需要 access token）。
      var cloudCleared = true;
      try {
        await MD.api.reset();
      } catch (e) {
        cloudCleared = false;
      }
      await MD.data.wipeAll();
      MD.haptic.warn();
      MD.toast.show(cloudCleared ? '本机与云端数据均已清空' : '本机已清空；云端清空失败，重新登录后旧数据可能回来',
        cloudCleared ? 'success' : 'warn');
      showAuth('login');
    });

    var passwordButton = buttonWithIcon('修改口令', 'lock', 'btn btn-ghost', function () {
      openChangePasswordSheet();
    });

    // 用药提醒开关：可开可关（与"通知权限"分开）。
    // 权限只有第一次会弹窗，之后由用户在系统里控制；这里的开关是"我想不想被提醒"，
    // 关掉之后既不再弹系统通知，也不再累积待提醒队列。
    var remindToggleBtn = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button' });
    function paintRemindToggle() {
      var on = true;
      try { on = MD.remind && typeof MD.remind.isEnabled === 'function' ? MD.remind.isEnabled() : true; } catch (e) { on = true; }
      remindToggleBtn.textContent = on ? '已开启 · 点击关闭' : '已关闭 · 点击开启';
      remindToggleBtn.className = 'btn ' + (on ? 'btn-primary' : 'btn-ghost');
    }
    paintRemindToggle();
    remindToggleBtn.addEventListener('click', async function () {
      if (!MD.remind || typeof MD.remind.setEnabled !== 'function') {
        MD.toast.show('提醒模块未加载', 'error');
        return;
      }
      var on = MD.remind.isEnabled();
      if (on) {
        MD.remind.setEnabled(false);
        paintRemindToggle();
        MD.toast.show('已关闭用药提醒（到点不会再弹）', 'success');
        return;
      }
      remindToggleBtn.disabled = true;
      try {
        var permission = await MD.remind.ensurePermission();
        if (permission === 'granted') {
          MD.remind.setEnabled(true);
          MD.toast.show('已开启用药提醒', 'success');
        } else if (permission === 'unsupported_pwa') {
          MD.toast.show('iPhone/iPad 请先用 Safari 打开本站 → 分享 → 「添加到主屏幕」，从主屏图标进入后再允许通知', 'warn');
        } else if (permission === 'unsupported') {
          MD.toast.show('当前浏览器不支持系统通知（小米/华为等自带浏览器常见）：请改用 Edge 或 Chrome 打开本站；页面内横幅提醒仍可用', 'warn');
        } else if (permission === 'denied') {
          MD.toast.show('通知已被拒绝：请在浏览器站点设置或系统设置中允许本站通知后重试', 'warn');
        } else {
          MD.toast.show('通知权限未授予，请在浏览器设置中允许本站通知', 'warn');
        }
      } catch (error) {
        MD.toast.show(error.message || '无法申请通知权限', 'error');
      } finally {
        remindToggleBtn.disabled = false;
        paintRemindToggle();
      }
    });

    var themeCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '外观' }),
      settingRow('主题', '跟随系统或手动选择深浅色', themeControl())
    ]);
    // 托管恢复开关的状态要问服务端（/auth/me）：先渲染占位再回填，避免整页等网络。
    var recoveryBtn = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button', text: '查询中…' });
    recoveryBtn.disabled = true;
    var recoveryOn = false;
    (async function () {
      try {
        var info = await MD.api.me();
        recoveryOn = Boolean(info && info.recoveryEnabled);
      } catch (e) { /* 离线时按未开启显示 */ }
      recoveryBtn.textContent = recoveryOn ? '已开启 · 点击关闭' : '未开启 · 点击开启';
      recoveryBtn.className = 'btn ' + (recoveryOn ? 'btn-primary' : 'btn-ghost');
      recoveryBtn.disabled = false;
    })();
    recoveryBtn.addEventListener('click', function () {
      if (recoveryBtn.disabled) return;
      openRecoverySheet(recoveryOn);
    });
    var recoveryRow = settingRow('托管恢复',
      '开启后管理员可直接改你的口令而数据不丢；代价是服务端持有一份可解密的恢复信封', recoveryBtn);

    var syncCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '云端同步' }),
      syncRow,
      settingRow('通知能力', remindDiagText(), MD.icon('clock')),
      settingRow('用药提醒', '可随时开关；关闭后到点不再弹任何提醒（系统权限需首次授权）', remindToggleBtn),
      settingRow('修改口令', '只换登录口令，数据全部保留（改完需重新登录一次）', passwordButton),
      recoveryRow
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
      settingRow('丘比特的工作台', '版本 ' + MD.version + ' · 构建 ' + String(MD.buildID || 'dev').slice(0, 8), MD.icon('home-heart')),
      MD.ui.el('p', { class: 'muted', text: '业务数据采用端到端加密。加密和解密均在当前设备完成，服务端只见密文，无法读取用药记录。JSON 备份为明文，请只保存在可信位置。' })
    ]);

    // 文件存储（WebDAV）配置区由 webdav.js 提供：模块未加载时自动跳过，保持解耦。
    var cards = [themeCard, syncCard, dataCard];
    if (MD.webdav && typeof MD.webdav.settingsSection === 'function') {
      cards.push(MD.webdav.settingsSection());
    }
    cards.push(privacyCard);
    view.appendChild(MD.ui.el('div', { class: 'stack' }, cards));
  }

  function hideAuth() {
    MD.ui.clear(authScreen);
  }

  function authBrand() {
    return MD.ui.el('div', { class: 'auth-card__brand' }, [
      MD.icon('home-heart'),
      MD.ui.el('div', {}, [
        MD.ui.el('h1', { text: '丘比特的工作台' }),
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
    // 图形验证码只出现在注册：登录已有失败次数限流，再加一层会明显拉长登录路径。
    var captchaField = null;
    if (mode === 'register') {
      captchaField = MD.ui.el('div', { class: 'field' }, [MD.ui.el('label', { class: 'label', text: '图形验证码' })]);
      fields.push(captchaField);
    }
    fields.push(errorText, submit);
    var form = MD.ui.el('form', { class: 'stack' }, fields);
    if (captchaField) MD.captcha.render(captchaField);
    form.addEventListener('submit', async function (event) {
      event.preventDefault();
      errorText.textContent = '';
      if (mode === 'register' && password.value !== confirmPassword.value) {
        errorText.textContent = '两次输入的口令不一致';
        return;
      }
      var payload = { email: email.value, password: password.value };
      if (mode === 'register') {
        var captcha = MD.captcha.value();
        if (!captcha.captchaCode) {
          errorText.textContent = '请输入图片中的验证码';
          return;
        }
        payload.captchaId = captcha.captchaId;
        payload.captchaCode = captcha.captchaCode;
      }
      submit.disabled = true;
      submit.textContent = mode === 'register' ? '正在创建…' : '正在登录…';
      try {
        if (mode === 'register') await MD.api.register(payload);
        else await MD.api.login({ email: email.value, password: password.value });
        hideAuth();
        await afterAuthenticated();
        MD.haptic.success();
        MD.toast.show(mode === 'register' ? '账户创建成功' : '登录成功', 'success');
      } catch (error) {
        errorText.textContent = error.message || (mode === 'register' ? '注册失败' : '登录失败');
        // 服务端无论注册成功与否都会消耗掉这张验证码，重试前必须换新图，
        // 否则用户填什么都只会得到「验证码错误或已过期」。
        if (mode === 'register') MD.captcha.refresh().catch(function () {});
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
    // 示例数据已按用户要求全局关闭：不再调用 med.seedSamples 或 data.seedExamples。
    // 两条播种路径都必须断开——只断一条会在模块加载顺序变化时“复活”示例数据。
    // 老账号里已存在的示例记录可在「设置 · 清空示例数据」里清理（该功能保留）。
    MD.sync.run().catch(function (error) {
      if (MD.sync.state !== 'offline') MD.toast.show(error.message || '同步失败', 'error');
    });
  }

  function restoreSession() {
    if (!MD.api.hasSession()) {
      showAuth('login');
      return;
    }
    // PWA「保持登录」：本机持久化了 refresh + DEK，静默刷新后直接进入主界面。
    if (!MD.api.getDEK() && storageGetPersisted()) {
      MD.api.refresh().then(function () {
        hideAuth();
        afterAuthenticated().catch(function (error) { MD.toast.show(error.message || '初始化数据失败', 'error'); });
        requestNotifyPermissionOnce();
      }).catch(function () {
        showAuth('login');
      });
      return;
    }
    if (!MD.api.getDEK()) {
      showUnlock();
      return;
    }
    hideAuth();
    afterAuthenticated().catch(function (error) { MD.toast.show(error.message || '初始化数据失败', 'error'); });
    requestNotifyPermissionOnce();
  }

  // 本机是否持久化了恢复材料（refresh + DEK）
  function storageGetPersisted() {
    try { return Boolean(window.localStorage.getItem('meddesk.refresh') || window.localStorage.getItem('meddesk.dek')); }
    catch (_) { return false; }
  }

  // 通知授权：整个生命周期只请求一次（被拒后浏览器不再弹，避免打扰）
  var notifyAsked = false;
  function requestNotifyPermissionOnce() {
    if (notifyAsked) return;
    notifyAsked = true;
    try {
      if (window.Notification && Notification.permission === 'default') {
        Notification.requestPermission();
      }
    } catch (_) { /* 不支持通知的浏览器静默跳过 */ }
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
    // 新 SW 接管后自动重载一次：sw.js 里 skipWaiting + clients.claim 会让新版本立刻接管，
    // 但当前页面仍是旧 HTML 渲染出来的，不重载就看不到新界面（发版后「看不到新功能」的主因）。
    // hadController 用于跳过「首次安装」那一次，避免首次访问白刷一次。
    try {
      var hadController = !!window.navigator.serviceWorker.controller;
      var reloaded = false;
      window.navigator.serviceWorker.addEventListener('controllerchange', function () {
        if (!hadController) { hadController = true; return; }
        if (reloaded) return;
        reloaded = true;
        window.location.reload();
      });
    } catch (_) { /* 不支持时静默 */ }
  }

  /**
   * 静默比对服务端构建指纹与当前页面指纹。
   * 场景：页面来自旧缓存（SW 尚未换版）→ 页面本身不会重载，用户就会长期停留在旧界面。
   * 命中不一致时在侧栏底部挂一个「有新版本 · 点击刷新」，由用户决定何时刷新（不打断录入）。
   */
  function watchBuildFreshness() {
    if (!MD.buildID || MD.buildID === 'dev' || typeof window.fetch !== 'function') return;
    window.setTimeout(function () {
      window.fetch('/', { method: 'HEAD', cache: 'no-store' }).then(function (res) {
        var tag = String(res.headers.get('ETag') || '').replace(/"/g, '');
        if (!tag || tag === MD.buildID) return;
        var foot = doc.getElementById('sidebar-foot');
        if (!foot || foot.querySelector('#build-update')) return;
        var btn = MD.ui.el('button', {
          class: 'btn btn-ghost',
          type: 'button',
          id: 'build-update',
          style: { marginTop: '8px', width: '100%' },
          text: '有新版本 · 点击刷新',
          onClick: function () { window.location.reload(); }
        });
        foot.appendChild(btn);
      }).catch(function () { /* 离线或跨域失败时忽略 */ });
    }, 2500);
  }

  doc.addEventListener('DOMContentLoaded', async function () {
    // 旧 Service Worker 自愈（带一次性锁，绝不循环刷新）：
    // 判定：MD.buildID === 'dev' 表示 HTML 里没有构建 meta（旧 SW 缓存吐出或未注入）。
    // 锁：localStorage 记录上次自愈时间，10 分钟内最多执行一次——即使判定异常也不会无限 reload。
    try {
      var HEAL_KEY = 'meddesk.heal.at';
      var HEAL_WINDOW = 10 * 60 * 1000;
      var lastHeal = Number(window.localStorage.getItem(HEAL_KEY) || 0) || 0;
      if (MD.buildID === 'dev' && navigator.onLine && navigator.serviceWorker && (Date.now() - lastHeal > HEAL_WINDOW)) {
        window.localStorage.setItem(HEAL_KEY, String(Date.now()));
        navigator.serviceWorker.getRegistrations().then(function (regs) {
          return Promise.all(regs.map(function (r) { return r.unregister(); }));
        }).then(function () {
          if (window.caches && caches.keys) {
            return caches.keys().then(function (keys) {
              return Promise.all(keys.map(function (k) { return caches.delete(k); }));
            });
          }
        }).then(function () { window.location.reload(); }).catch(function () {});
        return; // 自愈中，本次不继续初始化
      }
    } catch (e) { /* SW 不可用时跳过自愈 */ }

    // 版本变化自动重载（一次性）：发版后浏览器拿到新 index.html 时，
    // 若构建指纹与上次记录不同，自动刷新一次——用户无需手动强刷。
    // 循环防护：重载后指纹与记录一致即不再触发。
    try {
      var RELOAD_KEY = 'meddesk.reload.at';
      var lastReload = Number(window.localStorage.getItem(RELOAD_KEY) || 0) || 0;
      var __prevBuild = window.localStorage.getItem('meddesk.buildID');
      var __curBuild = (MD.buildID && MD.buildID !== 'dev') ? MD.buildID : '';
      // 10 分钟窗口内最多自动重载一次，避免任何判定抖动导致循环刷新
      if (__prevBuild && __curBuild && __prevBuild !== __curBuild && (Date.now() - lastReload > 10 * 60 * 1000)) {
        window.localStorage.setItem(RELOAD_KEY, String(Date.now()));
        window.localStorage.setItem('meddesk.buildID', __curBuild);
        window.location.reload();
        return;
      }
      if (__curBuild) window.localStorage.setItem('meddesk.buildID', __curBuild);
    } catch (e) { /* 存储不可用时跳过 */ }

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
      watchBuildFreshness();
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

(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var doc = window.document;
  var events = Object.create(null);

  function listeners(evt) {
    if (!events[evt]) events[evt] = [];
    return events[evt];
  }

  // 构建信息由服务端注入到 index.html 的 <meta>（而非内联 <script>）：
  // nginx CSP `script-src 'self'` 会拦截内联脚本，导致版本号恒为 undefined。
  function readMeta(name) {
    try {
      var el = document.querySelector('meta[name="' + name + '"]');
      return el ? String(el.getAttribute('content') || '') : '';
    } catch (e) { return ''; }
  }
  // 前端版本与 GitHub Release 一一对应：服务端 ldflags 版本经 meta 下发。
  MD.version = readMeta('md-version') || (typeof window !== 'undefined' && window.MD_VERSION) || 'dev';
  // 构建指纹：用于界面展示实际加载的构建 + 版本变化自动重载判定。
  MD.buildID = readMeta('md-build-id') || (typeof window !== 'undefined' && window.MD_BUILD_ID) || 'dev';

  MD.bus = {
    on: function (evt, fn) {
      if (typeof fn !== 'function') return fn;
      listeners(evt).push(fn);
      return fn;
    },
    off: function (evt, fn) {
      if (!events[evt]) return;
      events[evt] = events[evt].filter(function (item) { return item !== fn; });
    },
    emit: function (evt, payload) {
      listeners(evt).slice().forEach(function (fn) {
        try { fn(payload); } catch (error) { window.setTimeout(function () { throw error; }, 0); }
      });
    }
  };

  function pad(value) {
    return String(value).padStart(2, '0');
  }

  function asDate(ms) {
    return ms instanceof Date ? new Date(ms.getTime()) : new Date(ms == null ? Date.now() : ms);
  }

  MD.util = {
    uuid: function () {
      if (window.crypto && typeof window.crypto.randomUUID === 'function') return window.crypto.randomUUID();
      var bytes = new Uint8Array(16);
      window.crypto.getRandomValues(bytes);
      bytes[6] = (bytes[6] & 15) | 64;
      bytes[8] = (bytes[8] & 63) | 128;
      return Array.prototype.map.call(bytes, function (byte, index) {
        var hex = byte.toString(16).padStart(2, '0');
        return [4, 6, 8, 10].indexOf(index) >= 0 ? '-' + hex : hex;
      }).join('');
    },
    nowMs: function () { return Date.now(); },
    dayKey: function (ms) {
      var date = asDate(ms);
      return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate());
    },
    fmtDate: function (ms) {
      var date = asDate(ms);
      return date.getFullYear() + '年' + (date.getMonth() + 1) + '月' + date.getDate() + '日';
    },
    fmtTime: function (ms) {
      var date = asDate(ms);
      return pad(date.getHours()) + ':' + pad(date.getMinutes());
    },
    fmtDateTime: function (ms) {
      return MD.util.fmtDate(ms) + ' ' + MD.util.fmtTime(ms);
    },
    debounce: function (fn, ms) {
      var timer = null;
      return function () {
        var context = this;
        var args = arguments;
        window.clearTimeout(timer);
        timer = window.setTimeout(function () { fn.apply(context, args); }, ms);
      };
    },
    escapeHtml: function (value) {
      return String(value == null ? '' : value).replace(/[&<>'"]/g, function (char) {
        return { '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[char];
      });
    },
    clamp: function (value, min, max) {
      return Math.min(max, Math.max(min, value));
    },
    startOfDay: function (ms) {
      var date = asDate(ms);
      date.setHours(0, 0, 0, 0);
      return date.getTime();
    },
    addDays: function (ms, count) {
      var date = asDate(ms);
      date.setDate(date.getDate() + Number(count || 0));
      return date.getTime();
    }
  };

  MD.icon = function (name, className) {
    if (!doc || !doc.createElementNS) return null;
    var safeName = String(name || '').replace(/[^a-z0-9-]/gi, '');
    var svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
    var use = doc.createElementNS('http://www.w3.org/2000/svg', 'use');
    svg.setAttribute('class', 'icon' + (className ? ' ' + className : ''));
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    use.setAttribute('href', '#i-' + safeName);
    svg.appendChild(use);
    return svg;
  };

  MD.toast = {
    show: function (message, kind) {
      if (!doc) return;
      var host = doc.getElementById('toast-host');
      if (!host) return;
      var toast = doc.createElement('div');
      toast.className = 'toast';
      toast.dataset.kind = kind || 'info';
      toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
      toast.textContent = String(message || '');
      host.appendChild(toast);
      window.setTimeout(function () {
        toast.style.opacity = '0';
        toast.style.transform = 'translateY(8px)';
        window.setTimeout(function () { toast.remove(); }, 200);
      }, 2800);
      return toast;
    }
  };

  function vibrate(pattern) {
    try {
      if (window.navigator && typeof window.navigator.vibrate === 'function') window.navigator.vibrate(pattern);
    } catch (_) {
      // 触觉反馈不可用时静默降级。
    }
  }

  MD.haptic = {
    tap: function () { vibrate(10); },
    success: function () { vibrate([12, 35, 18]); },
    warn: function () { vibrate([24, 30, 24]); }
  };

  var themePreference = 'auto';
  var themeMedia = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

  function storedTheme() {
    try { return window.localStorage.getItem('meddesk.theme') || 'auto'; } catch (_) { return 'auto'; }
  }

  function resolvedTheme(value) {
    if (value === 'dark' || value === 'light') return value;
    return themeMedia && themeMedia.matches ? 'dark' : 'light';
  }

  function applyTheme(emit) {
    var resolved = resolvedTheme(themePreference);
    if (doc && doc.documentElement) doc.documentElement.setAttribute('data-theme', resolved);
    var meta = doc && doc.querySelector ? doc.querySelector('meta[name="theme-color"]') : null;
    if (meta) meta.setAttribute('content', resolved === 'dark' ? '#14211d' : '#0e9f6e');
    if (emit) MD.bus.emit('theme:changed', { value: themePreference, resolved: resolved });
  }

  MD.theme = {
    get: function () { return themePreference; },
    set: function (value) {
      if (['light', 'dark', 'auto'].indexOf(value) < 0) throw new TypeError('无效的主题模式');
      themePreference = value;
      try { window.localStorage.setItem('meddesk.theme', value); } catch (_) { /* 存储不可用时仅应用当前页面。 */ }
      applyTheme(true);
      return value;
    },
    toggle: function () {
      return MD.theme.set(resolvedTheme(themePreference) === 'dark' ? 'light' : 'dark');
    },
    onChange: function (fn) {
      MD.bus.on('theme:changed', fn);
      return function () { MD.bus.off('theme:changed', fn); };
    },
    init: function () {
      var value = storedTheme();
      themePreference = ['light', 'dark', 'auto'].indexOf(value) >= 0 ? value : 'auto';
      applyTheme(false);
      return themePreference;
    },
    resolved: function () { return resolvedTheme(themePreference); }
  };

  if (themeMedia) {
    var onSystemTheme = function () { if (themePreference === 'auto') applyTheme(true); };
    if (typeof themeMedia.addEventListener === 'function') themeMedia.addEventListener('change', onSystemTheme);
    else if (typeof themeMedia.addListener === 'function') themeMedia.addListener(onSystemTheme);
  }
  MD.theme.init();

  var sheetCleanup = null;

  function appendChildren(node, children) {
    if (children == null) return node;
    (Array.isArray(children) ? children : [children]).forEach(function (child) {
      if (child == null || child === false) return;
      if (Array.isArray(child)) return appendChildren(node, child);
      node.appendChild(child && child.nodeType ? child : doc.createTextNode(String(child)));
    });
    return node;
  }

  MD.ui = {
    el: function (tag, attrs, children) {
      var node = doc.createElement(tag);
      Object.keys(attrs || {}).forEach(function (key) {
        var value = attrs[key];
        if (value == null || value === false) return;
        if (key === 'class' || key === 'className') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key === 'html') node.innerHTML = value;
        else if (key === 'dataset') Object.keys(value).forEach(function (name) { node.dataset[name] = value[name]; });
        else if (key === 'style' && typeof value === 'object') Object.assign(node.style, value);
        else if (/^on[A-Z]/.test(key) && typeof value === 'function') node.addEventListener(key.slice(2).toLowerCase(), value);
        else if (key in node && key !== 'form') node[key] = value;
        else node.setAttribute(key, value === true ? '' : String(value));
      });
      return appendChildren(node, children);
    },
    html: function (node, html) {
      node.innerHTML = String(html == null ? '' : html);
      return node;
    },
    clear: function (node) {
      while (node.firstChild) node.removeChild(node.firstChild);
      return node;
    },
    confirm: function (options) {
      options = options || {};
      return new Promise(function (resolve) {
        var settled = false;
        function finish(value) {
          if (settled) return;
          settled = true;
          resolve(value);
        }
        MD.ui.sheet({
          title: options.title || '请确认',
          body: function (el) {
            el.appendChild(MD.ui.el('p', { class: 'muted', text: options.message || '' }));
          },
          actions: [
            { label: '取消', kind: 'ghost', onClick: function () { finish(false); } },
            { label: options.confirmText || '确认', kind: options.danger ? 'danger' : 'primary', onClick: function () { finish(true); } }
          ],
          onClose: function () { finish(false); }
        });
      });
    },
    sheet: function (options) {
      options = options || {};
      var host = doc.getElementById('modal-host');
      if (!host) return null;
      MD.ui.closeSheet();
      host.hidden = false;
      host.setAttribute('role', 'presentation');
      var panel = MD.ui.el('section', { class: 'sheet', role: 'dialog', 'aria-modal': 'true' });
      var titleId = 'sheet-title-' + MD.util.uuid();
      var close = MD.ui.el('button', { class: 'btn btn-icon', type: 'button', 'aria-label': '关闭' }, MD.icon('close'));
      var head = MD.ui.el('div', { class: 'sheet__head' }, [
        MD.ui.el('h2', { class: 'sheet__title', id: titleId, text: options.title || '' }),
        close
      ]);
      var body = MD.ui.el('div', { class: 'sheet__body' });
      var actions = MD.ui.el('div', { class: 'sheet__actions' });
      panel.setAttribute('aria-labelledby', titleId);
      if (typeof options.body === 'function') options.body(body);
      else if (options.body != null) appendChildren(body, options.body);
      (options.actions || []).forEach(function (action) {
        var kind = action.kind || 'ghost';
        var button = MD.ui.el('button', {
          class: 'btn btn-' + kind,
          type: 'button',
          text: action.label || '确定',
          onClick: function () {
            MD.haptic.tap();
            Promise.resolve(action.onClick ? action.onClick() : undefined).then(function (result) {
              if (result !== false) MD.ui.closeSheet();
            }).catch(function (error) {
              MD.toast.show(error && error.message ? error.message : '操作失败', 'error');
            });
          }
        });
        actions.appendChild(button);
      });
      panel.appendChild(head);
      panel.appendChild(body);
      if ((options.actions || []).length) panel.appendChild(actions);
      host.appendChild(panel);
      var onKey = function (event) { if (event.key === 'Escape') MD.ui.closeSheet(); };
      // 点击遮罩/空白处不关闭弹窗（用户要求：避免误触丢失已填内容）；
      // 关闭途径仅保留：右上角关闭按钮、底部取消按钮、Escape 键。
      doc.addEventListener('keydown', onKey);
      close.addEventListener('click', function () { MD.ui.closeSheet(); });
      sheetCleanup = function () {
        doc.removeEventListener('keydown', onKey);
        if (typeof options.onClose === 'function') options.onClose();
      };
      window.setTimeout(function () { close.focus(); }, 0);
      return panel;
    },
    closeSheet: function () {
      var host = doc && doc.getElementById('modal-host');
      if (!host || host.hidden) return;
      var cleanup = sheetCleanup;
      sheetCleanup = null;
      host.hidden = true;
      MD.ui.clear(host);
      if (cleanup) cleanup();
    }
  };

  var routes = [];
  var routeMap = Object.create(null);
  var activeId = null;
  var routerStarted = false;

  function sortedRoutes() {
    return routes.slice().sort(function (a, b) {
      return (Number(a.order) || 0) - (Number(b.order) || 0) || a.title.localeCompare(b.title, 'zh-CN');
    });
  }

  // 导航高亮：除自身命中外，子路由（声明 parent 的路由）激活时其父级也高亮，
  // 否则进入「工具箱 / 单价计算器」后侧栏「工具箱」会丢失选中态。
  function isActiveRoute(route) {
    if (!route || !activeId) return false;
    if (route.id === activeId) return true;
    var current = routeMap[activeId];
    return !!(current && current.parent && current.parent === route.id);
  }

  /**
   * 按 group 聚合并保持组内顺序；组间顺序取该组内最小 order，
   * 保证同名分组只渲染一次。
   * @param {Array} list 已排序且已过滤 hidden 的路由
   * @returns {Array<{name:string, items:Array}>}
   */
  function groupedRoutes(list) {
    var order = [];
    var map = Object.create(null);
    list.forEach(function (route) {
      var name = route.group || '功能';
      if (!map[name]) {
        map[name] = { name: name, items: [], order: Number(route.order) || 0 };
        order.push(map[name]);
      }
      map[name].items.push(route);
      var routeOrder = Number(route.order) || 0;
      if (routeOrder < map[name].order) map[name].order = routeOrder;
    });
    order.sort(function (a, b) { return a.order - b.order; });
    order.forEach(function (entry) { entry.blocks = categoriseRoutes(entry.items); });
    return order;
  }

  /**
   * 组内按 category 再分一层（如「工具箱 › 计算工具 › 单价计算器」）。
   * 未声明 category 的路由归为无标题块，与分类块按各自最小 order 排序。
   * @param {Array} items 组内已排序路由
   * @returns {Array<{title:string, items:Array}>}
   */
  function categoriseRoutes(items) {
    var blocks = [];
    var map = Object.create(null);
    items.forEach(function (route) {
      var title = route.category || '';
      if (!map[title]) {
        map[title] = { title: title, items: [], order: Number(route.order) || 0 };
        blocks.push(map[title]);
      }
      map[title].items.push(route);
      var routeOrder = Number(route.order) || 0;
      if (routeOrder < map[title].order) map[title].order = routeOrder;
    });
    blocks.sort(function (a, b) { return a.order - b.order; });
    return blocks;
  }

  function routeButton(route, tab) {
    var active = isActiveRoute(route);
    var button = MD.ui.el('button', {
      class: (tab ? 'tab-item' : 'nav-item') + (route.category ? ' nav-item--sub' : '') + (active ? ' is-active' : ''),
      type: 'button',
      dataset: { route: route.id },
      'aria-current': active ? 'page' : null,
      onClick: function () { MD.haptic.tap(); MD.router.go(route.id); }
    });
    button.appendChild(MD.icon(route.icon || 'pill'));
    if (tab) {
      button.appendChild(MD.ui.el('span', { text: route.title }));
    } else {
      button.appendChild(MD.ui.el('span', { class: 'nav-item__copy' }, [
        MD.ui.el('span', { class: 'nav-item__title', text: route.title }),
        route.subtitle ? MD.ui.el('span', { class: 'nav-item__subtitle', text: route.subtitle }) : null
      ]));
      if (route.comingSoon) button.appendChild(MD.ui.el('span', { class: 'badge nav-item__soon', text: '即将上线' }));
    }
    return button;
  }

  function renderNavigation() {
    if (!doc) return;
    var nav = doc.getElementById('nav-list');
    var tabbar = doc.getElementById('tabbar');
    // hidden 路由（工具箱子页等）不进主导航与底部标签栏：它只在所属容器内导航，
    // 直接暴露会让侧栏出现一堆零散条目。
    var list = sortedRoutes().filter(function (route) { return !route.hidden; });
    if (nav) {
      MD.ui.clear(nav);
      // 先按「组」聚合再渲染：若沿用全局 order 单序列，组内 order 与其他组交叉时
      // 会渲染出两个同名分组标题（如「工具箱 … 设置 … 工具箱」）。
      groupedRoutes(list).forEach(function (entry) {
        nav.appendChild(MD.ui.el('div', { class: 'nav-group', text: entry.name }));
        entry.blocks.forEach(function (block) {
          // block.title 为空表示该组下无分类的直挂条目，不渲染分类标题
          if (block.title) nav.appendChild(MD.ui.el('div', { class: 'nav-group nav-group--sub', text: block.title }));
          block.items.forEach(function (route) { nav.appendChild(routeButton(route, false)); });
        });
      });
    }
    if (tabbar) {
      MD.ui.clear(tabbar);
      // 底部标签栏只放一级入口：子路由（工具箱下各工具）放进来会挤掉主模块。
      list.filter(function (route) { return !route.parent; }).slice(0, 4)
        .forEach(function (route) { tabbar.appendChild(routeButton(route, true)); });
    }
  }

  function renderRoute(route) {
    var main = doc.getElementById('app-main');
    if (!main) return;
    MD.ui.clear(main);
    var view = MD.ui.el('section', { class: 'view', id: 'view-' + route.id });
    main.appendChild(view);
    var title = doc.getElementById('page-title');
    if (title) title.textContent = route.title;
    try {
      route.render(view);
    } catch (error) {
      view.appendChild(MD.ui.el('div', { class: 'card empty' }, [
        MD.icon('warning'),
        MD.ui.el('strong', { text: '页面暂时无法显示' }),
        MD.ui.el('p', { class: 'muted', text: error && error.message ? error.message : '未知错误' })
      ]));
      MD.toast.show('页面加载失败', 'error');
    }
    renderNavigation();
  }

  MD.router = {
    register: function (route) {
      if (!route || !route.id || typeof route.render !== 'function') throw new TypeError('路由配置无效');
      if (routeMap[route.id]) {
        var index = routes.indexOf(routeMap[route.id]);
        routes[index] = route;
      } else {
        routes.push(route);
      }
      routeMap[route.id] = route;
      if (routerStarted) renderNavigation();
      return route;
    },
    go: function (id, options) {
      var route = routeMap[id];
      if (!route) return false;
      if (route.comingSoon) {
        MD.toast.show('即将上线', 'info');
        return false;
      }
      activeId = id;
      if (!(options && options.fromHash) && window.location && window.location.hash !== '#/' + id) window.location.hash = '#/' + id;
      renderRoute(route);
      var sidebar = doc.getElementById('sidebar');
      var scrim = doc.getElementById('scrim');
      if (sidebar) sidebar.classList.remove('is-open');
      if (scrim) scrim.hidden = true;
      return true;
    },
    current: function () { return activeId; },
    /**
     * 查询已注册路由（返回副本，调用方可安全遍历）。
     * @param {Object} [filter] 支持 {parent, category, hidden:false}
     * @returns {Array} 按 order、标题排序后的路由数组
     */
    list: function (filter) {
      var out = routes.slice();
      filter = filter || {};
      if (filter.parent != null) out = out.filter(function (route) { return route.parent === filter.parent; });
      if (filter.category != null) out = out.filter(function (route) { return route.category === filter.category; });
      if (filter.hidden === false) out = out.filter(function (route) { return !route.hidden; });
      return out.sort(function (a, b) {
        return (Number(a.order) || 0) - (Number(b.order) || 0) || a.title.localeCompare(b.title, 'zh-CN');
      });
    },
    start: function () {
      if (routerStarted) return activeId;
      routerStarted = true;
      renderNavigation();
      window.addEventListener('hashchange', function () {
        var id = (window.location.hash || '').replace(/^#\/?/, '');
        if (id && id !== activeId) MD.router.go(id, { fromHash: true });
      });
      var hashId = (window.location.hash || '').replace(/^#\/?/, '');
      var initial = routeMap[hashId] && !routeMap[hashId].comingSoon ? hashId : null;
      if (!initial) {
        var first = sortedRoutes().filter(function (route) { return !route.comingSoon && !route.hidden; })[0];
        initial = first && first.id;
      }
      if (initial) MD.router.go(initial, { fromHash: hashId === initial });
      return activeId;
    }
  };
})(window);

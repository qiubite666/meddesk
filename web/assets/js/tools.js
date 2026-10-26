/* ==========================================================================
 * tools.js —— MedDesk「工具箱」容器与共享构件
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js：MD.ui.el / MD.icon / MD.router / MD.toast / MD.util / MD.haptic
 *
 * 分层约定：
 *   注册层 register(spec)  →  外壳层 mount(view, toolId)  →  构件层 field/select/resultCard
 *   工具子页只负责自己的表单与计算，公共外观统一由本文件提供。
 *
 * 零外部依赖：无 CDN、无网络请求，所有计算均在本地完成。
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};

  var HOME_ID = 'tools';            // 工具箱首页路由 id
  var GROUP = '工具箱';             // 侧栏分组名
  var PREFIX = HOME_ID + '/';       // 子路由前缀
  var DEFAULT_CATEGORY = '其他';    // 未声明分类的工具归入此类

  // 结果卡片语气 → CSS 变量（复用 app.css 已有变量，不新增全局类）
  var TONE_VAR = { ok: 'var(--ok)', warn: 'var(--warn)', danger: 'var(--danger)' };

  /* ============================ 1. 内部小工具 ============================ */

  function el(tag, attrs, children) {
    return MD.ui.el(tag, attrs, children);
  }

  function tap() {
    if (MD.haptic && typeof MD.haptic.tap === 'function') MD.haptic.tap();
  }

  function go(id) {
    if (MD.router && typeof MD.router.go === 'function') MD.router.go(id);
  }

  // 已注册的工具子路由（按 order、标题排序后的副本）
  function subRoutes() {
    if (!MD.router || typeof MD.router.list !== 'function') return [];
    return MD.router.list({ parent: HOME_ID });
  }

  // 'tools/unit-price' → 'unit-price'
  function shortIdOf(route) {
    var id = String(route && route.id || '');
    return id.indexOf(PREFIX) === 0 ? id.slice(PREFIX.length) : id;
  }

  // 按 category 分组，组顺序取组内首个工具的 order
  function groupTools(list) {
    var order = [];
    var map = {};
    list.forEach(function (route) {
      var name = route.category || DEFAULT_CATEGORY;
      if (!map[name]) {
        map[name] = { name: name, items: [] };
        order.push(name);
      }
      map[name].items.push(route);
    });
    return order.map(function (name) { return map[name]; });
  }

  function slug(text) {
    return String(text || '').replace(/[^0-9A-Za-z\u4e00-\u9fa5]+/g, '-') || 'cat';
  }

  function muted(text, fontSize) {
    return el('span', { class: 'muted', text: text, style: { fontSize: fontSize || '13px' } });
  }

  // 行内文字按钮（面包屑用，避免引入新的全局类）
  function linkButton(text, onClick) {
    return el('button', {
      type: 'button',
      text: text,
      style: {
        padding: '0',
        border: '0',
        background: 'transparent',
        color: 'var(--brand)',
        font: 'inherit',
        fontWeight: '700',
        cursor: 'pointer'
      },
      onClick: function () {
        tap();
        onClick();
      }
    });
  }

  /* ============================ 2. 解析与格式化 ============================ */

  // 解析数字：空串 / 非数字 / NaN / Infinity 一律返回 null
  function parseNum(str) {
    if (str == null) return null;
    var text = String(str).replace(/[,\s]/g, '');
    if (!text) return null;
    if (!/^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$/.test(text)) return null;
    var num = Number(text);
    if (!isFinite(num) || isNaN(num)) return null;
    return num;
  }

  // 格式化数字：digits 为小数位数（默认 2），非法值返回 '—'，整数部分加千分位
  function fmtNum(n, digits) {
    var num = typeof n === 'number' ? n : parseNum(n);
    if (num == null || !isFinite(num)) return '—';
    var fixed = num.toFixed(digits == null ? 2 : Math.max(0, Number(digits) || 0));
    var parts = fixed.split('.');
    parts[0] = parts[0].replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    return parts.join('.');
  }

  /* ============================ 3. 表单构件 ============================ */

  // 数字输入行
  function field(opts) {
    opts = opts || {};

    var error = el('div', {
      class: 'muted',
      hidden: true,
      style: { fontSize: '12px', color: 'var(--danger)' }
    });

    function setError(message) {
      var text = message == null ? '' : String(message);
      error.textContent = text;
      error.hidden = !text;
      input.style.borderColor = text ? 'var(--danger)' : '';
      if (text) input.setAttribute('aria-invalid', 'true');
      else input.removeAttribute('aria-invalid');
    }

    var input = el('input', {
      class: 'input',
      type: 'number',
      inputmode: 'decimal',
      value: opts.value == null || opts.value === '' ? '' : String(opts.value),
      placeholder: opts.placeholder || '请输入',
      step: opts.step == null ? 'any' : String(opts.step),
      min: opts.min == null ? null : String(opts.min),
      'aria-label': opts.label || '',
      onInput: function () {
        var raw = String(input.value || '');
        var num = parseNum(raw);
        // 空值不报错（视为"未填写"），只有填了却解析不出数字才提示
        setError(raw.replace(/\s/g, '') === '' || num != null ? '' : '请输入有效数字');
        if (typeof opts.onInput === 'function') opts.onInput(num);
      }
    });

    var row = el('div', { class: 'field' }, [
      el('div', { class: 'row row--wrap', style: { gap: '8px' } }, [
        el('span', { class: 'label', text: opts.label || '' }),
        opts.unit ? el('span', { class: 'badge', text: opts.unit }) : null
      ]),
      input,
      opts.hint ? el('div', { class: 'muted', text: opts.hint, style: { fontSize: '12px' } }) : null,
      error
    ]);

    return { row: row, input: input, setError: setError };
  }

  // 下拉选择行
  function select(opts) {
    opts = opts || {};

    var node = el('select', {
      class: 'select',
      'aria-label': opts.label || '',
      onChange: function () {
        if (typeof opts.onChange === 'function') opts.onChange(node.value);
      }
    }, (opts.options || []).map(function (item) {
      return el('option', {
        value: item.value == null ? '' : String(item.value),
        text: item.label == null ? String(item.value == null ? '' : item.value) : String(item.label)
      });
    }));

    if (opts.value != null) node.value = String(opts.value);

    var row = el('div', { class: 'field' }, [
      el('div', { class: 'row row--wrap', style: { gap: '8px' } }, [
        el('span', { class: 'label', text: opts.label || '' })
      ]),
      node,
      opts.hint ? el('div', { class: 'muted', text: opts.hint, style: { fontSize: '12px' } }) : null
    ]);

    return { row: row, select: node };
  }

  /* ============================ 4. 结果卡片 ============================ */

  function resultCard(opts) {
    opts = opts || {};
    var tone = TONE_VAR[opts.tone] || '';

    var card = el('div', { class: 'card' });
    if (tone) card.style.borderLeft = '4px solid ' + tone;

    if (opts.title) {
      card.appendChild(el('div', { class: 'row', style: { marginBottom: '6px' } }, [
        el('strong', { text: opts.title })
      ]));
    }

    var list = el('div', { class: 'list' });
    (opts.items || []).forEach(function (item) {
      if (!item) return;
      var strong = !!item.strong;
      list.appendChild(el('div', { class: 'list-item' }, [
        el('span', { class: 'muted', text: item.label || '', style: { fontSize: '13px' } }),
        el('span', { class: 'spacer' }),
        el('span', {
          text: item.value == null ? '—' : String(item.value),
          style: {
            fontWeight: strong ? '800' : '600',
            fontSize: strong ? '18px' : '15px',
            color: strong && tone ? tone : ''
          }
        }),
        item.unit ? el('span', { class: 'muted', text: item.unit, style: { fontSize: '12px' } }) : null
      ]));
    });
    card.appendChild(list);

    return card;
  }

  /* ============================ 5. 导航条 ============================ */

  // 工具子页用的分类导航：按分类分组列出全部工具，当前工具高亮
  function toolNav(activeShortId) {
    var groups = groupTools(subRoutes());
    if (!groups.length) return null;

    var wrap = el('div', { class: 'stack', style: { gap: '14px' } });
    groups.forEach(function (group) {
      // 分类标题独占一行（flex-basis 100% 强制换行），工具 chip 另起一行，层次更清楚
      var row = el('div', { class: 'row row--wrap', style: { gap: '10px' } }, [
        el('span', {
          class: 'muted',
          text: group.name,
          style: { fontSize: '12px', fontWeight: '700', flex: '0 0 100%' }
        })
      ]);
      group.items.forEach(function (route) {
        var active = shortIdOf(route) === activeShortId;
        row.appendChild(el('button', {
          class: 'chip' + (active ? ' chip--active' : ''),
          type: 'button',
          'aria-current': active ? 'page' : null,
          onClick: function () {
            tap();
            go(route.id);
          }
        }, [
          MD.icon(route.icon || 'toolbox'),
          el('span', { text: route.title })
        ]));
      });
      wrap.appendChild(row);
    });
    return wrap;
  }

  // 首页分类导航：点击滚动到对应分类区块
  function homeNavChip(name, section) {
    return el('button', {
      class: 'chip',
      type: 'button',
      text: name,
      onClick: function () {
        tap();
        if (!section) return;
        try {
          section.scrollIntoView({ behavior: 'smooth', block: 'start' });
        } catch (error) {
          section.scrollIntoView();
        }
      }
    });
  }

  // 首页工具卡片：点击进子路由
  function toolCard(route) {
    return el('button', {
      class: 'card',
      type: 'button',
      style: {
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        gap: '6px',
        width: '100%',
        textAlign: 'left',
        cursor: 'pointer'
      },
      onClick: function () {
        tap();
        go(route.id);
      }
    }, [
      el('span', { style: { color: 'var(--brand)', lineHeight: '0' } }, MD.icon(route.icon || 'toolbox')),
      el('strong', { text: route.title, style: { fontSize: '16px' } }),
      route.subtitle ? muted(route.subtitle, '12px') : null
    ]);
  }

  /* ============================ 6. 外壳 mount ============================ */

  // 渲染工具页统一外壳，返回供工具页填充的内容容器
  function mount(view, toolId) {
    if (!view) throw new TypeError('mount 需要 view 容器');

    var shortId = String(toolId == null ? '' : toolId);
    if (shortId.indexOf(PREFIX) === 0) shortId = shortId.slice(PREFIX.length);

    var route = null;
    subRoutes().forEach(function (item) {
      if (shortIdOf(item) === shortId) route = item;
    });

    var category = route && route.category ? route.category : '';
    var title = route ? route.title : shortId;
    var subtitle = route ? route.subtitle : '';

    // 面包屑：工具箱 / 分类 / 工具名
    var crumbs = el('nav', {
      class: 'row row--wrap',
      'aria-label': '面包屑导航',
      // 面包屑与下方标题之间留出一档呼吸空间，避免三级路径贴住大标题
      style: { gap: '8px', fontSize: '13px', marginBottom: '14px' }
    }, [linkButton(GROUP, function () { go(HOME_ID); })]);
    if (category) {
      crumbs.appendChild(muted('/'));
      crumbs.appendChild(muted(category));
    }
    if (title) {
      crumbs.appendChild(muted('/'));
      crumbs.appendChild(muted(title));
    }
    view.appendChild(crumbs);

    // 页面标题区：图标 +（标题 / 副标题 纵向微间距），下方留出与分类导航的间隔
    view.appendChild(el('div', { class: 'row', style: { gap: '14px', marginBottom: '22px' } }, [
      el('span', { style: { color: 'var(--brand)', lineHeight: '0' } }, MD.icon(route && route.icon ? route.icon : 'toolbox')),
      el('div', { style: { display: 'flex', flexDirection: 'column', gap: '5px', minWidth: '0' } }, [
        el('div', { text: title, style: { fontSize: '19px', fontWeight: '800', lineHeight: '1.25' } }),
        subtitle ? muted(subtitle, '13px') : null
      ])
    ]));

    // 分类导航条
    var nav = toolNav(shortId);
    if (nav) {
      nav.style.marginBottom = '22px';
      view.appendChild(nav);
    }

    var content = el('div', { class: 'stack' });
    view.appendChild(content);
    return content;
  }

  /* ============================ 7. 工具箱首页 ============================ */

  function renderHome(view) {
    var groups = groupTools(subRoutes());
    var count = groups.reduce(function (sum, group) { return sum + group.items.length; }, 0);

    view.appendChild(el('div', { class: 'card' }, [
      el('div', { class: 'row', style: { gap: '12px' } }, [
        el('span', { style: { color: 'var(--brand)', lineHeight: '0' } }, MD.icon('toolbox')),
        el('div', { style: { flex: '1 1 auto', minWidth: '0' } }, [
          el('div', { text: GROUP, style: { fontSize: '18px', fontWeight: '800' } }),
          muted('按分类选择工具，全部在本地计算，不上传任何数据。', '12px')
        ]),
        count ? el('span', { class: 'badge', text: count + ' 个工具' }) : null
      ])
    ]));

    if (!groups.length) {
      view.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('toolbox'),
        el('strong', { text: '暂无可用工具' }),
        el('p', { class: 'muted', text: '工具上线后会自动出现在这里。' })
      ]));
      return;
    }

    var nav = el('div', { class: 'row row--wrap', style: { gap: '8px' } });
    view.appendChild(nav);

    groups.forEach(function (group) {
      var section = el('section', {
        class: 'stack',
        id: 'tools-cat-' + slug(group.name),
        style: { gap: '10px' }
      }, [
        el('div', { class: 'row', style: { gap: '8px' } }, [
          el('h2', { class: 'section-title', text: group.name }),
          el('span', { class: 'badge', text: String(group.items.length) })
        ]),
        el('div', { class: 'grid' }, group.items.map(toolCard))
      ]);
      view.appendChild(section);
      nav.appendChild(homeNavChip(group.name, section));
    });
  }

  function registerHome() {
    if (!MD.router || typeof MD.router.register !== 'function') return null;
    return MD.router.register({
      id: HOME_ID,
      title: GROUP,
      subtitle: '分类工具集合',
      icon: 'toolbox',
      group: GROUP,
      order: 20,
      // 概览页只作落地页存在：侧栏「工具箱」是分组标题（与健康同级、不可点击），
      // 工具直接从二级分类「计算工具」进入，避免与分组标题重复成一个可点条目。
      hidden: true,
      render: renderHome
    });
  }

  /* ============================ 8. 注册子工具 ============================ */

  function register(spec) {
    if (!spec || !spec.id || typeof spec.render !== 'function') throw new TypeError('工具配置无效');
    if (!MD.router || typeof MD.router.register !== 'function') throw new Error('路由未就绪，无法注册工具');

    var route = MD.router.register({
      id: PREFIX + spec.id,
      title: spec.title || spec.id,
      subtitle: spec.subtitle || '',
      icon: spec.icon || 'toolbox',
      group: GROUP,
      parent: HOME_ID,
      // 工具在侧栏可见：挂在「工具箱 › <分类>」下，作为二级分类条目
      hidden: false,
      category: spec.category || DEFAULT_CATEGORY,
      order: spec.order,
      render: spec.render
    });

    // 首页已打开时新注册的工具应立即出现（工具脚本晚于本文件加载）
    if (typeof MD.router.current === 'function' && MD.router.current() === HOME_ID) go(HOME_ID);

    return route;
  }

  /* ============================ 9. 导出 ============================ */

  MD.tools = {
    register: register,
    mount: mount,
    field: field,
    select: select,
    resultCard: resultCard,
    parseNum: parseNum,
    fmtNum: fmtNum
  };

  registerHome();
})(window);

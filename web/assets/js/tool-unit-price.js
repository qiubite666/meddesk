/* ==========================================================================
 * tool-unit-price.js —— 工具箱「单价计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 分层约定：
 *   输入层（field / select） → 计算层 evaluate() → 渲染层 paintSide() 与
 *   paintConclusion()；两个渲染函数互不调用，统一由 refresh() 编排。
 *
 * 零外部依赖：无 CDN、无网络请求，全部计算在浏览器本地完成。
 * 安全约定：界面文字一律走 textContent，不使用 innerHTML 拼接用户输入。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 1. 常量 ============================ */

  var DASH = '—';   // 无法计算时的占位符：绝不输出 NaN / Infinity / undefined
  var EPS = 1e-9;   // 单价相等的判定阈值（相对误差）

  var UNITS = [
    { value: '个', label: '个' },
    { value: '克', label: '克' },
    { value: '公斤', label: '公斤' },
    { value: '毫升', label: '毫升' },
    { value: '升', label: '升' },
    { value: '米', label: '米' }
  ];

  // 同量纲单位折算表：1 单位 = factor 个基准单位。
  // 仅当两侧量纲相同（克 vs 公斤、毫升 vs 升）才换算后比较；量纲不同只提示、不换算。
  var UNIT_BASE = {
    '个': { base: '个', factor: 1 },
    '克': { base: '克', factor: 1 },
    '公斤': { base: '克', factor: 1000 },
    '毫升': { base: '毫升', factor: 1 },
    '升': { base: '毫升', factor: 1000 },
    '米': { base: '米', factor: 1 }
  };

  /* ============================ 2. 数值工具 ============================ */

  // 任意输入 → number | null（空、全空格、非数字一律为 null，不抛错）
  function toNumber(value) {
    if (typeof value === 'number') return isFinite(value) ? value : null;
    if (MD.tools && typeof MD.tools.parseNum === 'function') {
      var parsed = MD.tools.parseNum(value);
      return typeof parsed === 'number' && isFinite(parsed) ? parsed : null;
    }
    var raw = String(value == null ? '' : value).replace(/[,，\s]/g, '');
    if (raw === '') return null;
    var num = Number(raw);
    return isFinite(num) ? num : null;
  }

  // 按量级挑选小数位：大额少留、小额多留，避免 0.0033 元/克 被四舍五入成 0.00
  function digitsFor(value) {
    var abs = Math.abs(Number(value) || 0);
    if (abs >= 1) return 2;
    if (abs >= 0.1) return 3;
    if (abs >= 0.01) return 4;
    if (abs >= 0.001) return 5;
    return 6;
  }

  function fmt(value, digits) {
    if (typeof value !== 'number' || !isFinite(value)) return DASH;
    if (MD.tools && typeof MD.tools.fmtNum === 'function') {
      var text = MD.tools.fmtNum(value, digits);
      if (text != null && String(text) !== '' && String(text).indexOf('NaN') < 0) return String(text);
    }
    return value.toFixed(digits);
  }

  function unitBase(unit) {
    return UNIT_BASE[unit] || { base: unit || '单位', factor: 1 };
  }

  /* ============================ 3. 计算层 ============================ */

  /**
   * 单侧求值：任何不合法输入都返回 unitPrice = null，并给出可读原因。
   * @param {{price:number|null,qty:number|null,unit:string}} state
   * @returns {{unitPrice:number|null,unit:string,error:string,note:string}}
   */
  function evaluate(state) {
    var out = { unitPrice: null, unit: state.unit, error: '', note: '' };
    // 未填全：空值、全空格、非数字字符都落在这里，显示「—」而非报错
    if (state.price == null || state.qty == null) {
      out.note = '填写价格与数量后自动计算';
      return out;
    }
    if (state.price < 0) {
      out.error = '价格不能为负数';
      return out;
    }
    // 数量为 0 或负数 → 提示，单价显示「—」而不是 Infinity
    if (state.qty <= 0) {
      out.error = '数量需大于 0';
      return out;
    }
    var value = state.price / state.qty;
    out.unitPrice = isFinite(value) ? value : null;
    if (out.unitPrice == null) out.error = '无法计算，请检查输入';
    return out;
  }

  /* ============================ 4. 输入层（单栏） ============================ */

  function buildSide(title, defaultUnit, onChange) {
    var state = { price: null, qty: null, unit: defaultUnit };

    var priceField = MD.tools.field({
      label: '价格', unit: '元', placeholder: '例如 12.5', step: '0.01', min: '0',
      onInput: function (value) { state.price = toNumber(value); onChange(); }
    });

    var qtyField = MD.tools.field({
      label: '数量', placeholder: '例如 500', step: '0.01', min: '0', hint: '需大于 0',
      onInput: function (value) { state.qty = toNumber(value); onChange(); }
    });

    var unitField = MD.tools.select({
      label: '单位', value: defaultUnit, options: UNITS,
      onChange: function (value) { state.unit = UNIT_BASE[value] ? value : defaultUnit; onChange(); }
    });

    // 单价展示区（自建，随输入实时改写 textContent）
    var unitValue = MD.ui.el('div', {
      text: DASH,
      style: { fontSize: '22px', fontWeight: '700', lineHeight: '1.2', fontVariantNumeric: 'tabular-nums' }
    });
    var unitSuffix = MD.ui.el('span', { class: 'muted', text: '元/' + defaultUnit, style: { fontSize: '13px' } });
    var unitNote = MD.ui.el('div', { class: 'muted', text: '填写价格与数量后自动计算', style: { fontSize: '12px', marginTop: '2px' } });

    var unitBox = MD.ui.el('div', {
      style: {
        marginTop: '12px', padding: '10px 12px',
        border: '1px solid var(--border)', borderRadius: 'var(--radius, 12px)', background: 'var(--surface-2)'
      }
    }, [
      MD.ui.el('div', { class: 'muted', text: '单价', style: { fontSize: '12px' } }),
      MD.ui.el('div', { style: { display: 'flex', alignItems: 'baseline', gap: '6px', flexWrap: 'wrap' } }, [unitValue, unitSuffix]),
      unitNote
    ]);

    var panel = MD.ui.el('div', { class: 'card', style: { flex: '1 1 300px', minWidth: '0' } }, [
      MD.ui.el('h3', { class: 'section-title', text: title, style: { margin: '0 0 10px' } }),
      MD.ui.el('div', { class: 'stack', style: { gap: '10px' } }, [priceField.row, qtyField.row, unitField.row]),
      unitBox
    ]);

    // 字段级错误提示优先交给 field.setError（紧邻出错的输入框），
    // 只有它不可用时才退回单价框内的注释，避免同一句话重复出现两遍。
    var hasErrorSlot = typeof priceField.setError === 'function' && typeof qtyField.setError === 'function';
    var priceErrorText = '价格不能为负数';
    var qtyErrorText = '数量需大于 0';
    var shownError = { price: '', qty: '' };

    // 只在自己的提示发生变化时才写回字段，避免覆盖 field 自带的「请输入有效数字」
    function applyError(name, field, message) {
      if (!hasErrorSlot || shownError[name] === message) return;
      shownError[name] = message;
      field.setError(message);
    }

    // 渲染层：只负责把计算结果写进 DOM
    function paint(result) {
      unitSuffix.textContent = '元/' + (result.unit || '单位');
      applyError('price', priceField, result.error === priceErrorText ? result.error : '');
      applyError('qty', qtyField, result.error === qtyErrorText ? result.error : '');
      if (result.error) {
        unitValue.textContent = DASH;
        if (hasErrorSlot) {
          unitNote.textContent = '';
          unitNote.className = 'muted';
          unitNote.style.color = '';
        } else {
          unitNote.textContent = result.error;
          unitNote.className = '';
          unitNote.style.color = 'var(--danger)';
        }
      } else if (result.unitPrice == null) {
        unitValue.textContent = DASH;
        unitNote.textContent = result.note || '填写价格与数量后自动计算';
        unitNote.className = 'muted';
        unitNote.style.color = '';
      } else {
        unitValue.textContent = fmt(result.unitPrice, digitsFor(result.unitPrice));
        unitNote.textContent = '';
        unitNote.className = 'muted';
        unitNote.style.color = '';
      }
    }

    return { state: state, panel: panel, paint: paint };
  }

  /* ============================ 5. 渲染层（对比结论） ============================ */

  function paintConclusion(host, a, b) {
    MD.ui.clear(host);

    var title = '对比结论';
    var tone = '';
    var items = [];

    // 情况一：两侧都拿不到单价
    if (a.unitPrice == null && b.unitPrice == null) {
      var blockers = [];
      if (a.error) blockers.push('商品 A：' + a.error);
      if (b.error) blockers.push('商品 B：' + b.error);
      items.push({
        label: '提示',
        value: blockers.length ? blockers.join('；') : '在上方分别填写两个商品的价格与数量，即可自动比较谁更划算。',
        strong: true
      });
      if (blockers.length) tone = 'warn';
      host.appendChild(MD.tools.resultCard({ title: title, items: items, tone: tone }));
      return;
    }

    // 情况二：只有一侧填全 → 只展示该侧单价，引导补另一侧
    if (a.unitPrice == null || b.unitPrice == null) {
      var ready = a.unitPrice == null ? b : a;
      var readyName = a.unitPrice == null ? '商品 B' : '商品 A';
      var pending = a.unitPrice == null ? a : b;
      var pendingName = a.unitPrice == null ? '商品 A' : '商品 B';
      items.push({
        label: readyName + ' 单价',
        value: fmt(ready.unitPrice, digitsFor(ready.unitPrice)),
        unit: '元/' + (ready.unit || '单位'),
        strong: true
      });
      items.push({
        label: '提示',
        value: pending.error ? pendingName + '：' + pending.error : '请填写另一侧进行对比'
      });
      if (pending.error) tone = 'warn';
      host.appendChild(MD.tools.resultCard({ title: title, items: items, tone: tone }));
      return;
    }

    // 情况三：两侧都算出来了
    var baseA = unitBase(a.unit);
    var baseB = unitBase(b.unit);
    var comparable = baseA.base === baseB.base;
    // 同量纲（克 vs 公斤）折算到基准单位后比较；异量纲（个 vs 克）只按各自单位比较并提示
    var valueA = comparable ? a.unitPrice / baseA.factor : a.unitPrice;
    var valueB = comparable ? b.unitPrice / baseB.factor : b.unitPrice;

    var diff = Math.abs(valueA - valueB);
    var scale = Math.max(Math.abs(valueA), Math.abs(valueB));
    var same = scale <= 0 ? true : diff <= scale * EPS;
    var pct = same || scale <= 0 ? 0 : (diff / scale) * 100;
    if (!isFinite(pct) || pct < 0) pct = 0;

    if (same) {
      items.push({ label: '结论', value: '两者单价相同', strong: true });
    } else {
      var winner = valueA < valueB ? '商品 A' : '商品 B';
      items.push({ label: '结论', value: winner + ' 更划算', strong: true });
      items.push({
        label: '差价',
        value: fmt(diff, Math.max(digitsFor(valueA), digitsFor(valueB))),
        unit: comparable ? '元/' + baseA.base : '元/单位'
      });
      items.push({ label: '便宜幅度', value: fmt(pct, 1) + '%' });
    }
    items.push({ label: 'A 单价', value: fmt(a.unitPrice, digitsFor(a.unitPrice)), unit: '元/' + (a.unit || '单位') });
    items.push({ label: 'B 单价', value: fmt(b.unitPrice, digitsFor(b.unitPrice)), unit: '元/' + (b.unit || '单位') });

    if (a.unit !== b.unit) {
      if (comparable) {
        items.push({
          label: '说明',
          value: '单位不同（' + a.unit + ' / ' + b.unit + '），已统一折算为「元/' + baseA.base + '」后比较。'
        });
      } else {
        items.push({
          label: '说明',
          value: '单位不同（' + a.unit + ' / ' + b.unit + '），单价按各自单位计算，直接比较仅供参考。'
        });
        tone = 'warn';
      }
    } else {
      tone = 'ok';
    }

    host.appendChild(MD.tools.resultCard({ title: title, items: items, tone: tone }));
  }

  /* ============================ 6. 页面装配 ============================ */

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'unit-price') : null) || view;

    var columns = MD.ui.el('div', { class: 'row row--wrap', style: { alignItems: 'stretch', gap: '12px' } });
    var host = MD.ui.el('div', { style: { marginTop: '12px' } });

    function refresh() {
      var resultA = evaluate(A.state);
      var resultB = evaluate(B.state);
      A.paint(resultA);
      B.paint(resultB);
      paintConclusion(host, resultA, resultB);
    }

    var A = buildSide('商品 A', '个', refresh);
    var B = buildSide('商品 B', '个', refresh);
    columns.appendChild(A.panel);
    columns.appendChild(B.panel);

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '单价 = 价格 ÷ 数量，任一侧改动都会立即重算。价格为 0 视为合法，数量需大于 0。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(columns);
    content.appendChild(host);

    refresh();
  }

  /* ============================ 7. 注册 ============================ */

  MD.tools.register({
    id: 'unit-price',
    title: '单价计算器',
    subtitle: '两个商品谁更划算',
    icon: 'scale',
    category: '计算工具',
    order: 10,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

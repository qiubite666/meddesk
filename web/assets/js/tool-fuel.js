/* ==========================================================================
 * tool-fuel.js —— MedDesk 工具箱「油耗计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js：MD.ui / MD.icon / MD.toast / MD.haptic
 *   tools.js：MD.tools.register / mount / field / resultCard / parseNum / fmtNum
 *
 * 计算全部在浏览器本地完成，零外部依赖、零网络请求。
 * 分层约定：输入层（field） → 计算层 compute() → 渲染层 paint()。
 *
 * 边界约定（任何情况下都不出现 NaN / Infinity / undefined）：
 *   里程 ≤ 0 或为空 → 百公里油耗、每公里成本显示「—」并提示「里程需大于 0」
 *   加油量 = 0     → 合法，油耗为 0
 *   油价 ≤ 0 或为空 → 只显示油耗，费用相关显示「—」
 *   非法字符 / 超大数值 → 「—」
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 0. 常量 ============================ */

  var DASH = '—';            // 无效或未填写时的占位符
  var MAX_INPUT = 1e9;       // 输入上限：超出视为无效，避免天文数字与精度崩坏
  var MAX_OUTPUT = 1e12;     // 输出上限：超出只显示「—」
  var TOOL_ID = 'fuel';

  /* ============================ 1. 数值工具 ============================ */

  // 加油量允许 0，里程与油价必须为正数；非有限值、负数、超大值一律视为未填写。
  function positive(value) {
    return value != null && isFinite(value) && value > 0 && value <= MAX_INPUT ? value : null;
  }

  function nonNegative(value) {
    return value != null && isFinite(value) && value >= 0 && value <= MAX_INPUT ? value : null;
  }

  // 统一出口：任何不可展示的数值都收敛为「—」。
  function show(value, digits) {
    if (value == null || !isFinite(value) || Math.abs(value) > MAX_OUTPUT) return DASH;
    return MD.tools.fmtNum(value, digits);
  }

  // 结果是否在可展示范围之外（非有限值或超出上限）。
  function overflow(value) {
    return value != null && (!isFinite(value) || Math.abs(value) > MAX_OUTPUT);
  }

  /* ============================ 2. 计算层 ============================ */

  /**
   * @param {{liters:number|null, km:number|null, price:number|null}} state
   * @returns {{per100:number|null, cost:number|null, perKm:number|null, cost100:number|null}}
   */
  function compute(state) {
    var liters = state.liters;
    var km = state.km;
    var price = state.price;
    var result = { per100: null, cost: null, perKm: null, cost100: null };

    // 本次油费只依赖加油量与油价，里程无效时依然可算。
    if (liters != null && price != null) result.cost = liters * price;

    // 与里程相关的指标，必须里程 > 0 才有意义。
    if (liters != null && km != null) {
      result.per100 = liters / km * 100;
      if (result.cost != null) {
        result.perKm = result.cost / km;
        result.cost100 = result.per100 * price;
      }
      // 里程过小时派生值会爆掉，此时三个里程指标一起置空，避免出现「油耗为—但每公里费用有值」。
      if (overflow(result.per100) || overflow(result.perKm) || overflow(result.cost100)) {
        result.per100 = null;
        result.perKm = null;
        result.cost100 = null;
      }
    }
    return result;
  }

  /* ============================ 3. 渲染层 ============================ */

  function render(view) {
    var content = MD.tools.mount(view, TOOL_ID);
    var state = { liters: null, km: null, price: null };

    /* ---- 3.1 输入区 ---- */

    var grid = MD.ui.el('div', {
      style: {
        display: 'grid',
        gridTemplateColumns: 'repeat(auto-fit, minmax(190px, 1fr))',
        gap: '12px'
      }
    });

    var litersField = MD.tools.field({
      label: '加油量',
      unit: '升',
      value: '',
      placeholder: '如 40',
      step: '0.01',
      min: '0',
      hint: '本次加了多少升油',
      onInput: function (value) {
        state.liters = nonNegative(value);
        litersField.setError(value != null && value < 0 ? '加油量不能为负数' : '');
        paint();
      }
    });

    var kmField = MD.tools.field({
      label: '行驶里程',
      unit: '公里',
      value: '',
      placeholder: '如 500',
      step: '0.1',
      min: '0',
      hint: '两次加油之间跑了多少公里',
      onInput: function (value) {
        state.km = positive(value);
        var typed = kmField.input && String(kmField.input.value || '').trim() !== '';
        kmField.setError(typed && state.km == null ? '里程需大于 0' : '');
        paint();
      }
    });

    var priceField = MD.tools.field({
      label: '油价',
      unit: '元/升',
      value: '',
      placeholder: '如 8',
      step: '0.01',
      min: '0',
      hint: '留空则只算油耗',
      onInput: function (value) {
        state.price = positive(value);
        var typed = priceField.input && String(priceField.input.value || '').trim() !== '';
        priceField.setError(typed && value != null && value <= 0 ? '油价需大于 0' : '');
        paint();
      }
    });

    grid.appendChild(litersField.row);
    grid.appendChild(kmField.row);
    grid.appendChild(priceField.row);

    var inputCard = MD.ui.el('div', { class: 'card' }, [
      MD.ui.el('div', { class: 'row' }, [
        MD.icon(TOOL_ID),
        MD.ui.el('strong', { text: '加油数据' })
      ]),
      grid
    ]);

    /* ---- 3.2 结果区 ---- */

    // 结果随输入即时重绘；aria-live 让读屏软件也能感知变化。
    var resultHost = MD.ui.el('div', { 'aria-live': 'polite' });
    var hint = MD.ui.el('p', { class: 'muted', style: { margin: '8px 0 0', fontSize: '12px' } });

    function paint() {
      var result = compute(state);
      var ready = result.per100 != null;

      MD.ui.clear(resultHost);
      resultHost.appendChild(MD.tools.resultCard({
        title: '计算结果',
        tone: ready ? 'ok' : '',
        items: [
          { label: '百公里油耗', value: show(result.per100, 2), unit: 'L/100km', strong: true },
          { label: '每公里油费', value: show(result.perKm, 2), unit: '元/km' },
          { label: '本次油费', value: show(result.cost, 2), unit: '元' },
          { label: '每百公里花费', value: show(result.cost100, 2), unit: '元/100km' }
        ]
      }));

      // 底部提示：解释为什么某些指标为空。
      var text = '';
      if (state.km == null) text = '填写大于 0 的里程后，显示百公里油耗与每公里成本。';
      else if (state.price == null) text = '已显示油耗；填写油价后显示费用相关结果。';
      else if (state.liters == null) text = '填写加油量后，显示完整结果。';
      hint.textContent = ready && state.price != null ? '按当前油价换算，每百公里约需 ' + show(result.cost100, 2) + ' 元。' : text;
    }

    var note = MD.ui.el('p', {
      class: 'muted',
      style: { margin: '0', fontSize: '12px' },
      text: '公式：百公里油耗 = 加油量 ÷ 里程 × 100；本次油费 = 加油量 × 油价；每公里成本 = 本次油费 ÷ 里程。'
    });

    content.appendChild(inputCard);
    content.appendChild(MD.ui.el('div', { style: { marginTop: '12px' } }, [resultHost, hint]));
    content.appendChild(MD.ui.el('div', { style: { marginTop: '12px' } }, note));

    paint();
  }

  /* ============================ 4. 注册 ============================ */

  MD.tools.register({
    id: TOOL_ID,
    title: '油耗计算器',
    subtitle: '百公里油耗与每公里成本',
    icon: 'fuel',
    category: '计算工具',
    order: 30,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

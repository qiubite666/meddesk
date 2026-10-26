/* ==========================================================================
 * tool-convert.js —— MedDesk 工具箱「单位换算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js：MD.ui / MD.icon / MD.toast / MD.haptic
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 计算全部在浏览器本地完成，零外部依赖、零网络请求。
 * 分层约定：单位表（CATEGORIES） → 计算层 convert() → 渲染层 paint()。
 *
 * 换算模型：
 *   比例型（长度/重量/体积）：统一换算到该类别的基准单位（米/千克/升）再转出。
 *   偏移型（温度）：先归一到摄氏度，再由摄氏度转出，绝不可按倍率处理。
 *
 * 边界约定（任何情况下都不出现 NaN / Infinity / undefined）：
 *   输入为空 / 非法字符 / 超出上限 → 结果一律显示「—」
 *   同一单位互转     → 直接返回原值，不经过基准单位，避免浮点误差
 *   极大 / 极小数值  → 自动切换科学计数法；真正溢出为 Infinity 时显示「—」
 *   温度低于绝对零度 → 仍给出结果，但结果卡转为 warn 并提示
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 0. 常量 ============================ */

  var DASH = '—';            // 无效或未填写时的占位符
  var MAX_INPUT = 1e12;      // 输入上限：超出（或非有限值）视为无效
  var SCI_MIN = 1e-6;        // 小于该绝对值改用科学计数法
  var SCI_MAX = 1e12;        // 大于该绝对值改用科学计数法
  var ABSOLUTE_ZERO_C = -273.15;
  var TOOL_ID = 'convert';

  /* ============================ 1. 单位表 ============================ */

  // factor：1 该单位 = factor × 基准单位；温度类别不使用 factor（走偏移公式）。
  var CATEGORIES = [
    {
      id: 'length', label: '长度', base: '米',
      from: 'm', to: 'ft',
      units: [
        { id: 'mm', name: '毫米', factor: 0.001 },
        { id: 'cm', name: '厘米', factor: 0.01 },
        { id: 'm', name: '米', factor: 1 },
        { id: 'km', name: '千米', factor: 1000 },
        { id: 'in', name: '英寸', factor: 0.0254 },
        { id: 'ft', name: '英尺', factor: 0.3048 },
        { id: 'mi', name: '英里', factor: 1609.344 }
      ]
    },
    {
      id: 'mass', label: '重量', base: '千克',
      from: 'kg', to: 'lb',
      units: [
        { id: 'mg', name: '毫克', factor: 1e-6 },
        { id: 'g', name: '克', factor: 0.001 },
        { id: 'kg', name: '千克', factor: 1 },
        { id: 'jin', name: '斤', factor: 0.5 },
        { id: 't', name: '吨', factor: 1000 },
        { id: 'oz', name: '盎司', factor: 0.028349523125 },
        { id: 'lb', name: '磅', factor: 0.45359237 }
      ]
    },
    {
      id: 'volume', label: '体积', base: '升',
      from: 'l', to: 'ml',
      units: [
        { id: 'ml', name: '毫升', factor: 0.001 },
        { id: 'l', name: '升', factor: 1 },
        { id: 'm3', name: '立方米', factor: 1000 },
        { id: 'cm3', name: '立方厘米', factor: 0.001 },
        { id: 'gal', name: '加仑(美)', factor: 3.785411784 },
        { id: 'pt', name: '品脱(美)', factor: 0.473176473 }
      ]
    },
    {
      id: 'temp', label: '温度', base: '摄氏度', affine: true,
      from: 'c', to: 'f',
      units: [
        { id: 'c', name: '摄氏度', factor: 1 },
        { id: 'f', name: '华氏度', factor: 1 },
        { id: 'k', name: '开尔文', factor: 1 }
      ]
    }
  ];

  /* ============================ 2. 计算层 ============================ */

  /** 类别查询：找不到时回退到第一个类别，保证渲染永不拿到 undefined。 */
  function categoryOf(id) {
    for (var i = 0; i < CATEGORIES.length; i++) {
      if (CATEGORIES[i].id === id) return CATEGORIES[i];
    }
    return CATEGORIES[0];
  }

  function unitOf(category, id) {
    if (!category) return null;
    for (var i = 0; i < category.units.length; i++) {
      if (category.units[i].id === id) return category.units[i];
    }
    return null;
  }

  // 温度：归一到摄氏度（唯一入口，杜绝按倍率换算的写法混进来）。
  function toCelsius(unitId, value) {
    if (unitId === 'f') return (value - 32) * 5 / 9;
    if (unitId === 'k') return value - 273.15;
    return value;
  }

  // 温度：由摄氏度转出。
  function fromCelsius(unitId, celsius) {
    if (unitId === 'f') return celsius * 9 / 5 + 32;
    if (unitId === 'k') return celsius + 273.15;
    return celsius;
  }

  /** 换算到类别基准单位（米 / 千克 / 升 / 摄氏度）。 */
  function toBase(category, unit, value) {
    if (category.affine) return toCelsius(unit.id, value);
    return value * unit.factor;
  }

  /** 由类别基准单位换算到目标单位。 */
  function fromBase(category, unit, base) {
    if (category.affine) return fromCelsius(unit.id, base);
    return base / unit.factor;
  }

  /**
   * @param {Object} category 当前类别
   * @param {number} value    待换算数值（已确保为有限数）
   * @param {Object} from     源单位
   * @param {Object} to       目标单位
   * @returns {number}
   */
  function convert(category, value, from, to) {
    if (!from || !to) return NaN;
    if (from.id === to.id) return value;      // 同单位互转：原值返回，规避浮点往返误差
    return fromBase(category, to, toBase(category, from, value));
  }

  /** 数值是否可参与计算：非空、有限、未超出输入上限。 */
  function usable(value) {
    return typeof value === 'number' && isFinite(value) && Math.abs(value) <= MAX_INPUT ? value : null;
  }

  /* ============================ 3. 数值格式化 ============================ */

  // fmtNum 可能保留尾随 0（如 0.00100000），统一裁掉，读数更干净。
  function trimZeros(text) {
    if (text.indexOf('.') < 0) return text;
    return text.replace(/0+$/, '').replace(/\.$/, '');
  }

  function fmt(value, digits) {
    var text = MD.tools.fmtNum(value, digits);
    return trimZeros(typeof text === 'string' ? text : String(value));
  }

  // 极大 / 极小值走科学计数法，避免长串数字撑破布局，也避免 Infinity 出镜。
  function sci(value) {
    return value.toExponential(6).replace(/(\.\d*?)0+e/, '$1e').replace(/\.e/, 'e');
  }

  // 按「有效数字」而非固定小数位选精度：1 英里→1609.344 米 与 1 英里→1.609344 千米
  // 都要保住全部有效位；尾随 0 由 trimZeros 裁掉，不影响读数。
  function digitsFor(abs) {
    if (abs === 0) return 0;
    var intDigits = Math.max(Math.floor(Math.log10(abs)) + 1, 0);
    var decimals = 10 - intDigits;
    return Math.max(2, Math.min(10, decimals));
  }

  // 统一出口：任何不可展示的数值都收敛为「—」。
  function show(value) {
    if (value == null || typeof value !== 'number' || !isFinite(value)) return DASH;
    var abs = Math.abs(value);
    if (abs !== 0 && (abs >= SCI_MAX || abs < SCI_MIN)) return sci(value);
    return fmt(value, digitsFor(abs));
  }

  /* ============================ 4. 渲染层 ============================ */

  function render(view) {
    var content = MD.tools.mount(view, TOOL_ID);
    var category = CATEGORIES[0];
    var state = { value: null, from: category.from, to: category.to };

    /* ---- 4.1 类别切换 ---- */

    var chipHost = MD.ui.el('div', { class: 'row row--wrap', style: { gap: '8px' } });

    function buildChips() {
      MD.ui.clear(chipHost);
      CATEGORIES.forEach(function (item) {
        var active = item.id === category.id;
        chipHost.appendChild(MD.ui.el('button', {
          class: 'chip' + (active ? ' chip--active' : ''),
          type: 'button',
          'aria-pressed': active ? 'true' : 'false',
          text: item.label,
          onClick: function () {
            MD.haptic.tap();
            if (item.id === category.id) return;
            category = item;
            state.from = item.from;
            state.to = item.to;
            buildChips();
            buildUnits();
            paint();
          }
        }));
      });
    }

    /* ---- 4.2 数值输入 ---- */

    var valueField = MD.tools.field({
      label: '数值',
      value: '',
      placeholder: '如 1',
      step: 'any',
      hint: '输入待换算的数值，支持小数与负数',
      onInput: function (value) {
        state.value = usable(value);
        var typed = valueField.input && String(valueField.input.value || '').trim() !== '';
        valueField.setError(typed && state.value == null ? '请输入有效数值' : '');
        paint();
      }
    });

    /* ---- 4.3 单位选择（随类别重建） ---- */

    var unitHost = MD.ui.el('div');
    var fromSelect = null;
    var toSelect = null;

    function optionsOf() {
      return category.units.map(function (unit) {
        return { value: unit.id, label: unit.name };
      });
    }

    function buildUnits() {
      MD.ui.clear(unitHost);

      fromSelect = MD.tools.select({
        label: '源单位',
        value: state.from,
        options: optionsOf(),
        onChange: function (value) { state.from = value; paint(); }
      });

      toSelect = MD.tools.select({
        label: '目标单位',
        value: state.to,
        options: optionsOf(),
        onChange: function (value) { state.to = value; paint(); }
      });

      var swap = MD.ui.el('button', {
        class: 'btn btn-ghost',
        type: 'button',
        title: '交换源单位与目标单位',
        'aria-label': '交换单位',
        style: { alignSelf: 'end' },
        onClick: function () {
          MD.haptic.tap();
          var previous = state.from;
          state.from = state.to;
          state.to = previous;
          if (fromSelect) fromSelect.select.value = state.from;
          if (toSelect) toSelect.select.value = state.to;
          paint();
        }
      }, [MD.icon('swap'), MD.ui.el('span', { text: '交换' })]);

      unitHost.appendChild(MD.ui.el('div', {
        style: {
          display: 'grid',
          gridTemplateColumns: 'minmax(0, 1fr) auto minmax(0, 1fr)',
          alignItems: 'end',
          gap: '8px'
        }
      }, [fromSelect.row, swap, toSelect.row]));
    }

    /* ---- 4.4 结果区 ---- */

    // 结果随输入即时重绘；aria-live 让读屏软件也能感知变化。
    var resultHost = MD.ui.el('div', { 'aria-live': 'polite' });
    var hint = MD.ui.el('p', { class: 'muted', style: { margin: '8px 0 0', fontSize: '12px' } });

    function paint() {
      var from = unitOf(category, state.from) || category.units[0];
      var to = unitOf(category, state.to) || category.units[0];
      var value = state.value;
      var ready = value != null;

      var out = ready ? convert(category, value, from, to) : null;
      var rate = convert(category, 1, from, to);       // 1 源单位 = ? 目标单位
      var inverse = convert(category, 1, to, from);    // 1 目标单位 = ? 源单位

      // 温度：归一到摄氏度判断是否跌破绝对零度。
      var belowZero = false;
      if (ready && category.affine) {
        var celsius = toCelsius(from.id, value);
        belowZero = celsius < ABSOLUTE_ZERO_C;
      }

      MD.ui.clear(resultHost);
      resultHost.appendChild(MD.tools.resultCard({
        title: '换算结果',
        tone: belowZero ? 'warn' : (ready ? 'ok' : ''),
        items: [
          { label: '结果', value: show(out), unit: to.name, strong: true },
          { label: '换算关系', value: '1 ' + from.name + ' = ' + show(rate) + ' ' + to.name },
          { label: '反向关系', value: '1 ' + to.name + ' = ' + show(inverse) + ' ' + from.name }
        ]
      }));

      if (!ready) {
        hint.textContent = '输入数值后即时显示换算结果。';
      } else if (belowZero) {
        hint.textContent = '低于绝对零度（0 K，-273.15 °C），请检查输入数值。';
      } else if (from.id === to.id) {
        hint.textContent = '源单位与目标单位相同，直接返回原值。';
      } else {
        hint.textContent = '以' + category.base + '为基准换算，' + category.label + '类共 ' + category.units.length + ' 个单位可选。';
      }
    }

    /* ---- 4.5 组装 ---- */

    var note = MD.ui.el('p', {
      class: 'muted',
      style: { margin: '0', fontSize: '12px' },
      text: '长度/重量/体积按固定倍率换算；温度采用偏移公式：°F = °C × 9/5 + 32，K = °C + 273.15。'
    });

    content.appendChild(MD.ui.el('div', { class: 'card' }, [
      // sprite 里没有 i-convert，统一复用 i-swap（与注册图标保持一致）。
      MD.ui.el('div', { class: 'row' }, [MD.icon('swap'), MD.ui.el('strong', { text: '单位换算' })]),
      MD.ui.el('div', { style: { marginTop: '12px' } }, chipHost),
      MD.ui.el('div', { style: { marginTop: '12px' } }, valueField.row),
      MD.ui.el('div', { style: { marginTop: '12px' } }, unitHost)
    ]));

    content.appendChild(MD.ui.el('div', { style: { marginTop: '12px' } }, [resultHost, hint]));
    content.appendChild(MD.ui.el('div', { style: { marginTop: '12px' } }, note));

    buildChips();
    buildUnits();
    paint();
  }

  /* ============================ 5. 注册 ============================ */

  MD.tools.register({
    id: TOOL_ID,
    title: '单位换算器',
    subtitle: '长度/重量/体积/温度',
    icon: 'swap',
    category: '计算工具',
    order: 40,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

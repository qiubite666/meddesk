/* ==========================================================================
 * tool-health-bmi.js —— 工具箱「BMI 计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数，不碰 DOM） → 渲染层 paint()
 * 零外部依赖、零网络请求；界面文字一律走 textContent，不用 innerHTML 拼输入。
 *
 * 【分级标准 · 有意不同于国际口径】
 * 采用《中国成人超重和肥胖症预防控制指南》/ WS/T 428-2013：
 *   偏瘦 <18.5 ｜ 正常 18.5~23.9 ｜ 超重 24~27.9 ｜ 肥胖 ≥28
 * 不采用 WHO 的 18.5~24.9 / 25~29.9 / ≥30：中国人群在更低 BMI 上就已出现
 * 代谢风险，用 WHO 口径会把一部分中国超重人群判成「正常」。
 * 对 18 岁以下会显式提示「成人标准不适用」，不静默给结论。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  // 英制输入在入口处折成公制，计算层只认 cm / kg，避免两套公式并存
  var INCH_CM = 2.54;
  var LB_KG = 0.45359237;

  var NORMAL_LOW = 18.5;
  var NORMAL_HIGH = 23.9;
  var OVERWEIGHT = 24;
  var OBESE = 28;
  var REFERENCE_BMI = 22;

  var SANITY = { min: 12, max: 60 };

  /**
   * 中国成人分级。用显式区间而不是「上界数组 + 小于比较」，
   * 是因为边界值（正好 24.0、正好 28.0）在浮点加法下极易判错。
   */
  function classify(bmi) {
    if (bmi < NORMAL_LOW) return { label: '偏瘦', risk: '营养不良风险', tone: 'warn' };
    if (bmi < OVERWEIGHT) return { label: '正常', risk: '健康体重范围', tone: 'ok' };
    if (bmi < OBESE) return { label: '超重', risk: '轻度肥胖风险', tone: 'warn' };
    return { label: '肥胖', risk: '中重度健康风险', tone: 'danger' };
  }

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

  function fmt(value, digits) {
    if (typeof value !== 'number' || !isFinite(value)) return DASH;
    if (MD.tools && typeof MD.tools.fmtNum === 'function') {
      var text = MD.tools.fmtNum(value, digits);
      if (text != null && String(text) !== '' && String(text).indexOf('NaN') < 0) return String(text);
    }
    return value.toFixed(digits);
  }

  function deltaText(delta) {
    if (typeof delta !== 'number' || !isFinite(delta)) return DASH;
    var sign = delta > 0 ? '+' : (delta < 0 ? '-' : '');
    return sign + fmt(Math.abs(delta), 1);
  }

  /**
   * @param {{age:number|null, height:number|null, weight:number|null}} state
   *        height / weight 一律为公制（cm / kg），英制已在上层折算。
   */
  function evaluate(state) {
    var out = {
      bmi: null, category: '', risk: '', tone: '', low: null, high: null,
      delta: null, error: '', note: ''
    };
    var h = state.height, w = state.weight;

    if (h == null || w == null) { out.error = '填写身高与体重后自动计算'; return out; }
    if (h <= 0) { out.error = '身高需大于 0'; return out; }
    if (w <= 0) { out.error = '体重需大于 0'; return out; }

    var m = h / 100;
    var bmi = w / (m * m);

    out.bmi = bmi;
    var lv = classify(bmi);
    out.category = lv.label;
    out.risk = lv.risk;
    out.tone = lv.tone;
    out.low = NORMAL_LOW * m * m;
    out.high = NORMAL_HIGH * m * m;
    out.delta = w - REFERENCE_BMI * m * m;

    if (bmi < SANITY.min || bmi > SANITY.max) {
      out.note = '结果超出常见范围，请核对身高单位是否为厘米';
    }
    if (state.age != null && state.age > 0 && state.age < 18) {
      out.note = (out.note ? out.note + '；' : '') + '18 岁以下请用儿童青少年 BMI 百分位标准，本结果不适用';
    }
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);
    var items = [
      { label: 'BMI', value: fmt(result.bmi, 1), strong: true },
      { label: '分级', value: result.category || DASH, strong: true },
      { label: '风险提示', value: result.risk || DASH },
      {
        label: '健康体重区间',
        value: result.low == null ? DASH : (fmt(result.low, 1) + ' ~ ' + fmt(result.high, 1)),
        unit: 'kg'
      },
      { label: '与参考值（BMI 22）之差', value: deltaText(result.delta), unit: 'kg' }
    ];
    if (result.note) items.push({ label: '提示', value: result.note });
    if (result.error) items.push({ label: '无法计算', value: result.error });

    host.appendChild(MD.tools.resultCard({
      title: '计算结果',
      tone: result.error ? 'warn' : result.tone,
      items: items
    }));
  }

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-bmi') : null) || view;
    var state = { system: 'metric', age: null, height: null, weight: null };
    var host = MD.ui.el('div');
    var inputsHost = MD.ui.el('div', { class: 'stack', style: { gap: '12px' } });

    function refresh() { paint(host, evaluate(state)); }

    function buildInputs() {
      MD.ui.clear(inputsHost);
      var metric = state.system === 'metric';

      inputsHost.appendChild(MD.tools.field({
        label: '年龄', unit: '岁', placeholder: '例如 30', step: '1', min: '1',
        onInput: function (v) { state.age = toNumber(v); refresh(); }
      }).row);

      inputsHost.appendChild(MD.tools.field({
        label: '身高',
        unit: metric ? 'cm' : 'in',
        placeholder: metric ? '例如 170' : '例如 67',
        step: '0.1', min: '0',
        onInput: function (v) {
          var n = toNumber(v);
          state.height = n == null ? null : (metric ? n : n * INCH_CM);
          refresh();
        }
      }).row);

      inputsHost.appendChild(MD.tools.field({
        label: '体重',
        unit: metric ? 'kg' : 'lb',
        placeholder: metric ? '例如 65' : '例如 143',
        step: '0.1', min: '0',
        onInput: function (v) {
          var n = toNumber(v);
          state.weight = n == null ? null : (metric ? n : n * LB_KG);
          refresh();
        }
      }).row);
    }

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: 'BMI = 体重(kg) ÷ 身高(m)²。分级采用中国成人标准（WS/T 428-2013）：偏瘦 <18.5，正常 18.5~23.9，超重 24~27.9，肥胖 ≥28。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));

    content.appendChild(MD.tools.select({
      label: '单位制', value: 'metric',
      options: [
        { value: 'metric', label: '公制（厘米 / 千克）' },
        { value: 'imperial', label: '英制（英寸 / 磅）' }
      ],
      onChange: function (v) {
        if (v === state.system) return;
        state.system = v;
        // 切单位制必须清空数值：否则 170 会被当 170 英寸，得出荒谬结论
        state.height = null;
        state.weight = null;
        buildInputs();
        refresh();
      }
    }).row);

    content.appendChild(inputsHost);
    content.appendChild(host);

    // 注意事项：与分级标准配套，避免把 BMI 当唯一指标
    content.appendChild(MD.ui.el('div', {
      style: { marginTop: '16px', fontSize: '13px', lineHeight: '1.7', color: 'var(--muted, #666)' }
    }, [
      MD.ui.el('div', { text: '注意事项', style: { fontWeight: '700', marginBottom: '6px' } }),
      MD.ui.el('div', { text: '· BMI 只反映体重与身高的比例，不能区分肌肉与脂肪' }),
      MD.ui.el('div', { text: '· 运动员、孕妇、水肿人群可能偏高或偏低，需单独判断' }),
      MD.ui.el('div', { text: '· 18 岁以下请使用儿童青少年百分位标准，不适用本页结论' }),
      MD.ui.el('div', { text: '· 建议结合腰围、体脂率一起看，单看 BMI 容易误判' })
    ]));

    buildInputs();
    refresh();
  }

  MD.tools.register({
    id: 'health-bmi',
    title: 'BMI 计算器',
    subtitle: '身高体重指数与分级（中国标准）',
    icon: 'heart',
    category: '健康工具',
    order: 50,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

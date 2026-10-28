/* ==========================================================================
 * tool-health-bmi.js —— 工具箱「BMI 计算器」（Contract Health #2）
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 分层约定：
 *   输入层（field） → 计算层 evaluate()（纯函数，不碰 DOM） → 渲染层 paint()
 *
 * 零外部依赖：无网络请求，全部计算在浏览器本地完成，不上传任何数据。
 * 安全约定：界面文字一律走 textContent，不使用 innerHTML 拼接用户输入。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 1. 常量 ============================ */

  var DASH = '—';   // 无法计算时的占位符：绝不输出 NaN / Infinity / undefined

  // 分级阈值采用《中国成人超重和肥胖症预防控制指南》/ WS/T 428-2013：
  // 中国人群在较低 BMI 上即出现代谢风险，正常上限是 23.9 而非 WHO 的 24.9，
  // 超重从 24 起、肥胖从 28 起。这里不提供标准切换，避免同一数据两种口径打架。
  var NORMAL_LOW = 18.5;
  var NORMAL_HIGH = 23.9;
  var OVERWEIGHT = 24;
  var OBESE = 28;

  // 参考中心值取 22（亚洲人群常用），用于给出「还差多少到参考体重」
  var REFERENCE_BMI = 22;

  // 结果超出该区间时提示核对输入，但仍照常出结果——不替用户判定"不可能"
  var SANITY = { min: 12, max: 60 };

  /* ============================ 2. 数值工具 ============================ */

  // 任意输入 → number | null（空、全空格、非数字一律 null，不抛错）
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

  /* ============================ 3. 计算层（纯函数） ============================ */

  function classify(bmi) {
    if (bmi < NORMAL_LOW) return { label: '偏瘦', tone: 'warn' };
    if (bmi <= NORMAL_HIGH) return { label: '正常', tone: 'ok' };
    if (bmi < OBESE) return { label: '超重', tone: 'warn' };
    return { label: '肥胖', tone: 'danger' };
  }

  /**
   * BMI 求值。任何不合法输入都返回 bmi = null 并给出可读原因。
   * @param {{height:number|null, weight:number|null}} state  身高(cm)、体重(kg)
   * @returns {{bmi:number|null, category:string, tone:string,
   *            low:number|null, high:number|null, delta:number|null,
   *            error:string, note:string}}
   */
  function evaluate(state) {
    var out = {
      bmi: null, category: '', tone: '',
      low: null, high: null, delta: null,
      error: '', note: ''
    };

    // 未填全：空值、全空格、非数字字符都落在这里，显示「—」而不是报错
    if (state.height == null || state.weight == null) {
      out.note = '填写身高与体重后自动计算';
      return out;
    }
    if (state.height <= 0) {
      out.error = '身高需大于 0';
      return out;
    }
    if (state.weight <= 0) {
      out.error = '体重需大于 0';
      return out;
    }

    var meters = state.height / 100;
    var bmi = state.weight / (meters * meters);
    if (!isFinite(bmi) || bmi <= 0) {
      out.error = '无法计算，请检查输入';
      return out;
    }

    var level = classify(bmi);
    out.bmi = bmi;
    out.category = level.label;
    out.tone = level.tone;
    // 健康体重区间由正常 BMI 区间反推：体重 = BMI × 身高(m)²
    out.low = NORMAL_LOW * meters * meters;
    out.high = NORMAL_HIGH * meters * meters;
    // delta > 0 表示高于参考体重，< 0 表示低于
    out.delta = state.weight - REFERENCE_BMI * meters * meters;

    if (bmi < SANITY.min || bmi > SANITY.max) {
      out.note = '结果超出常见范围，请核对身高单位是否为厘米';
    }
    return out;
  }

  /* ============================ 4. 渲染层 ============================ */

  function deltaText(delta) {
    if (typeof delta !== 'number' || !isFinite(delta)) return DASH;
    var sign = delta > 0 ? '+' : (delta < 0 ? '-' : '');
    return sign + fmt(Math.abs(delta), 1);
  }

  function paint(host, result) {
    MD.ui.clear(host);

    var items = [
      { label: 'BMI', value: fmt(result.bmi, 1), strong: true },
      { label: '分级', value: result.category || DASH, strong: true },
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

  /* ============================ 5. 页面装配 ============================ */

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-bmi') : null) || view;
    var state = { height: null, weight: null };
    var host = MD.ui.el('div');

    function refresh() {
      paint(host, evaluate(state));
    }

    var heightField = MD.tools.field({
      label: '身高', unit: 'cm', placeholder: '例如 170', step: '0.1', min: '0',
      onInput: function (value) { state.height = toNumber(value); refresh(); }
    });

    var weightField = MD.tools.field({
      label: '体重', unit: 'kg', placeholder: '例如 65', step: '0.1', min: '0',
      onInput: function (value) { state.weight = toNumber(value); refresh(); }
    });

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: 'BMI = 体重(kg) ÷ 身高(m)²。分级采用中国成人标准（正常 18.5~23.9，超重 24~27.9，肥胖 ≥28）。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '12px' } }, [
      heightField.row,
      weightField.row
    ]));
    content.appendChild(host);

    refresh();
  }

  /* ============================ 6. 注册 ============================ */

  MD.tools.register({
    id: 'health-bmi',
    title: 'BMI 计算器',
    subtitle: '身高体重指数与分级',
    icon: 'heart',
    category: '健康工具',
    order: 50,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

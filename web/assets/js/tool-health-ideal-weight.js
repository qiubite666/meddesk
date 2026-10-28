/* ==========================================================================
 * tool-health-ideal-weight.js —— 工具箱「理想体重」（Contract Health #5）
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 分层约定：
 *   输入层（select / field） → 计算层 evaluate()（纯函数，不碰 DOM） → 渲染层 paint()
 *
 * 零外部依赖：无网络请求，全部计算在浏览器本地完成，不上传任何数据。
 * 安全约定：界面文字一律走 textContent，不使用 innerHTML 拼接用户输入。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 1. 常量 ============================ */

  var DASH = '—';   // 无法计算时的占位符：绝不输出 NaN / Infinity / undefined

  // 健康体重区间的 BMI 边界（中国成人标准 WS/T 428-2013）
  var NORMAL_LOW = 18.5;
  var NORMAL_HIGH = 23.9;
  // 亚洲人群常用的单点参考值
  var REFERENCE_BMI = 22;

  var GENDERS = [
    { value: 'female', label: '女' },
    { value: 'male', label: '男' }
  ];

  // 超出该身高范围时提示核对单位，但不阻止计算
  var SANITY = { min: 100, max: 250 };

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

  /**
   * 理想体重求值。任何不合法输入都返回 reference = null 并给出可读原因。
   * @param {{gender:string, height:number|null}} state  性别、身高(cm)
   * @returns {{reference:number|null, low:number|null, high:number|null,
   *            who:number|null, broca:number|null, error:string, note:string}}
   */
  function evaluate(state) {
    var out = {
      reference: null, low: null, high: null, who: null, broca: null,
      error: '', note: ''
    };

    if (state.height == null) {
      out.note = '填写身高后自动计算';
      return out;
    }
    if (state.height <= 0) {
      out.error = '身高需大于 0';
      return out;
    }

    var meters = state.height / 100;
    // 三种口径各有适用场景，一起给出来比只给一个数字更不容易被误读：
    // reference 按亚洲人群常用 BMI 22；who 为世卫推荐公式；broca 是国内临床最常用的简易估算。
    out.reference = REFERENCE_BMI * meters * meters;
    out.low = NORMAL_LOW * meters * meters;
    out.high = NORMAL_HIGH * meters * meters;
    out.who = state.gender === 'male' ? (state.height - 80) * 0.7 : (state.height - 70) * 0.6;
    out.broca = state.height - 105;

    if (state.height < SANITY.min || state.height > SANITY.max) {
      out.note = '结果超出常见范围，请核对身高单位是否为厘米';
    }
    return out;
  }

  /* ============================ 4. 渲染层 ============================ */

  function paint(host, result) {
    MD.ui.clear(host);

    if (result.reference == null) {
      host.appendChild(MD.tools.resultCard({
        title: '计算结果',
        tone: result.error ? 'warn' : '',
        items: [{ label: result.error ? '无法计算' : '提示', value: result.error || result.note || DASH, strong: true }]
      }));
      return;
    }

    var items = [
      { label: '理想体重（BMI 22）', value: fmt(result.reference, 1), unit: 'kg', strong: true },
      {
        label: '健康体重区间',
        value: fmt(result.low, 1) + ' ~ ' + fmt(result.high, 1),
        unit: 'kg'
      },
      { label: '世卫（WHO）公式', value: fmt(result.who, 1), unit: 'kg' },
      { label: 'Broca 改良公式', value: fmt(result.broca, 1), unit: 'kg' }
    ];
    if (result.note) items.push({ label: '提示', value: result.note });

    host.appendChild(MD.tools.resultCard({ title: '计算结果', tone: 'ok', items: items }));
  }

  /* ============================ 5. 页面装配 ============================ */

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-ideal-weight') : null) || view;
    var state = { gender: 'female', height: null };
    var host = MD.ui.el('div');

    function refresh() {
      paint(host, evaluate(state));
    }

    var genderField = MD.tools.select({
      label: '性别', value: state.gender, options: GENDERS,
      hint: '仅世卫公式区分性别',
      onChange: function (value) { state.gender = value === 'male' ? 'male' : 'female'; refresh(); }
    });

    var heightField = MD.tools.field({
      label: '身高', unit: 'cm', placeholder: '例如 170', step: '0.1', min: '0',
      onInput: function (value) { state.height = toNumber(value); refresh(); }
    });

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '理想体重由身高推算，三种公式口径不同、结果略有差异，均仅供参考。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '12px' } }, [
      genderField.row,
      heightField.row
    ]));
    content.appendChild(host);

    refresh();
  }

  /* ============================ 6. 注册 ============================ */

  MD.tools.register({
    id: 'health-ideal-weight',
    title: '理想体重',
    subtitle: '按身高推算参考体重',
    icon: 'scale',
    category: '健康工具',
    order: 80,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

/* ==========================================================================
 * tool-health-bmr.js —— 工具箱「基础代谢率计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求。
 *
 * 【公式口径】Mifflin-St Jeor（1990），目前临床上最常用的 BMR 估算式：
 *   男：BMR = 10 × 体重(kg) + 6.25 × 身高(cm) − 5 × 年龄 + 5
 *   女：BMR = 10 × 体重(kg) + 6.25 × 身高(cm) − 5 × 年龄 − 161
 * 相比 Harris-Benedict，它在现代人群中偏差更小，故取此式。
 * TDEE = BMR × 活动系数。活动系数采用通行五档（1.2 / 1.375 / 1.55 / 1.725 / 1.9）。
 * 注意：这是「估算」，个体差异可达 ±10%；甲状腺疾病、孕期等情况不适用。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  // 活动系数：这里用直接下拉选择，不做「复选框再联动显示」——
  // 少一次交互，用户看到的就是他要的答案。
  var ACTIVITY = [
    { value: 'sedentary', label: '久坐（几乎不运动）', factor: 1.2 },
    { value: 'light', label: '轻度（每周 1~3 次）', factor: 1.375 },
    { value: 'moderate', label: '中度（每周 3~5 次）', factor: 1.55 },
    { value: 'active', label: '高度（每周 6~7 次）', factor: 1.725 },
    { value: 'very_active', label: '极高（体力工作/一日两练）', factor: 1.9 }
  ];

  function factorOf(id) {
    for (var i = 0; i < ACTIVITY.length; i++) {
      if (ACTIVITY[i].value === id) return ACTIVITY[i].factor;
    }
    return ACTIVITY[0].factor;
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

  // 千卡数值统一取整显示：小数位对热量没有意义，反而放大「精确性」错觉
  function fmtKcal(value) {
    if (typeof value !== 'number' || !isFinite(value)) return DASH;
    return String(Math.round(value));
  }

  /**
   * @param {{gender:string, age:number|null, height:number|null, weight:number|null, activity:string}} state
   */
  function evaluate(state) {
    var out = { bmr: null, tdee: null, perKg: null, tone: 'ok', error: '', note: '' };
    var sex = state.gender === 'female' ? 'female' : 'male';
    var h = state.height, w = state.weight, age = state.age;

    if (h == null || w == null || age == null) { out.error = '填写性别、年龄、身高、体重后自动计算'; return out; }
    if (h <= 0) { out.error = '身高需大于 0'; return out; }
    if (w <= 0) { out.error = '体重需大于 0'; return out; }
    if (age <= 0 || age > 120) { out.error = '年龄需在 1 ~ 120 之间'; return out; }

    var base = 10 * w + 6.25 * h - 5 * age;
    var bmr = sex === 'male' ? base + 5 : base - 161;

    if (!isFinite(bmr) || bmr < 500 || bmr > 6000) {
      out.error = '结果超出常见范围，请核对输入';
      out.tone = 'warn';
      return out;
    }

    out.bmr = bmr;
    out.tdee = bmr * factorOf(state.activity);
    out.perKg = bmr / w;
    out.note = '估算值个体差异可达 ±10%，疾病或孕期请以医生意见为准';
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);
    var items = [
      { label: '基础代谢率', value: fmtKcal(result.bmr), unit: 'kcal/天', strong: true },
      { label: '每日总消耗（TDEE）', value: fmtKcal(result.tdee), unit: 'kcal/天', strong: true },
      { label: '每公斤体重消耗', value: fmt(result.perKg, 1), unit: 'kcal/kg' }
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
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-bmr') : null) || view;
    var state = { gender: 'male', age: null, height: null, weight: null, activity: 'sedentary' };
    var host = MD.ui.el('div');

    function refresh() { paint(host, evaluate(state)); }

    var genderField = MD.tools.select({
      label: '性别', value: 'male',
      options: [{ value: 'male', label: '男' }, { value: 'female', label: '女' }],
      onChange: function (v) { state.gender = v; refresh(); }
    });
    var ageField = MD.tools.field({
      label: '年龄', unit: '岁', placeholder: '例如 30', step: '1', min: '1',
      onInput: function (v) { state.age = toNumber(v); refresh(); }
    });
    var heightField = MD.tools.field({
      label: '身高', unit: 'cm', placeholder: '例如 170', step: '0.1', min: '0',
      onInput: function (v) { state.height = toNumber(v); refresh(); }
    });
    var weightField = MD.tools.field({
      label: '体重', unit: 'kg', placeholder: '例如 65', step: '0.1', min: '0',
      onInput: function (v) { state.weight = toNumber(v); refresh(); }
    });
    var activityField = MD.tools.select({
      label: '活动强度', value: 'sedentary',
      options: ACTIVITY.map(function (a) { return { value: a.value, label: a.label }; }),
      onChange: function (v) { state.activity = v; refresh(); }
    });

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '采用 Mifflin-St Jeor 公式：男 = 10×体重 + 6.25×身高 − 5×年龄 + 5；女 = 同式 − 161。每日总消耗 = 基础代谢 × 活动系数。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '12px' } }, [
      genderField.row, ageField.row, heightField.row, weightField.row, activityField.row
    ]));
    content.appendChild(host);
    refresh();
  }

  MD.tools.register({
    id: 'health-bmr',
    title: '基础代谢率计算器',
    subtitle: '基础代谢与每日消耗',
    icon: 'heart',
    category: '健康工具',
    order: 70,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

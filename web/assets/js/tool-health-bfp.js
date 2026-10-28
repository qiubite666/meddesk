/* ==========================================================================
 * tool-health-bfp.js —— 工具箱「体脂率计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *             MD.tools.parseNum / fmtNum
 *
 * 分层约定：输入层（field/select） → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求，全部本地计算，不上传任何数据。
 * 界面文字一律走 textContent，不用 innerHTML 拼用户输入。
 *
 * 【公式口径】Deurenberg 体脂率估算式（1991，基于 BMI 的替代法）：
 *   体脂率(%) = 1.20 × BMI + 0.23 × 年龄 − 10.8 × 性别 − 5.4
 *   其中 性别 男=1、女=0；BMI = 体重(kg) / 身高(m)²
 * 选它的原因：只需身高体重年龄性别四项，不要求用户量腰围颈围，
 * 在手机上能立刻出结果；围度法（US Navy）更准但门槛高，留作后续迭代。
 * 已知偏差：肌肉量大的人会被高估、老年人会被低估（该式基于人群统计）。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  // 按性别区分的体脂率分级（男性阈值显著低于女性，这是生理差异，不是笔误）
  var LEVELS = {
    male: [
      { max: 10, label: '偏低', tone: 'warn' },
      { max: 20, label: '正常', tone: 'ok' },
      { max: 25, label: '偏高', tone: 'warn' },
      { max: Infinity, label: '肥胖', tone: 'danger' }
    ],
    female: [
      { max: 18, label: '偏低', tone: 'warn' },
      { max: 28, label: '正常', tone: 'ok' },
      { max: 33, label: '偏高', tone: 'warn' },
      { max: Infinity, label: '肥胖', tone: 'danger' }
    ]
  };

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

  function classify(sex, bodyFat) {
    var levels = LEVELS[sex] || LEVELS.male;
    for (var i = 0; i < levels.length; i++) {
      if (bodyFat <= levels[i].max) return { label: levels[i].label, tone: levels[i].tone };
    }
    return { label: DASH, tone: 'warn' };
  }

  /**
   * 求值。任何不合法输入都返回 bodyFat=null 并给出可读原因，绝不输出 NaN。
   * @param {{gender:string, age:number|null, height:number|null, weight:number|null}} state
   */
  function evaluate(state) {
    var out = { bmi: null, bodyFat: null, category: '', tone: '', fatMass: null, leanMass: null, error: '', note: '' };
    var sex = state.gender === 'female' ? 'female' : 'male';
    var h = state.height, w = state.weight, age = state.age;

    if (h == null || w == null) { out.error = '填写身高、体重、年龄后自动计算'; return out; }
    if (h <= 0) { out.error = '身高需大于 0'; return out; }
    if (w <= 0) { out.error = '体重需大于 0'; return out; }
    if (age == null || age <= 0 || age > 120) { out.error = '年龄需在 1 ~ 120 之间'; return out; }

    var m = h / 100;
    var bmi = w / (m * m);
    var bf = 1.20 * bmi + 0.23 * age - 10.8 * (sex === 'male' ? 1 : 0) - 5.4;

    // 公式在极端输入下可能给出负数或荒谬值，这类结果没有解释意义，直接不给数
    if (!isFinite(bf) || bf < 2 || bf > 70) {
      out.bmi = bmi;
      out.error = '结果超出该公式的适用范围，请核对输入';
      return out;
    }

    out.bmi = bmi;
    out.bodyFat = bf;
    var cls = classify(sex, bf);
    out.category = cls.label;
    out.tone = cls.tone;
    out.fatMass = w * bf / 100;
    out.leanMass = w - out.fatMass;
    out.note = '该式基于人群统计，肌肉量大者易被高估';
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);
    var items = [
      { label: '体脂率', value: result.bodyFat == null ? DASH : fmt(result.bodyFat, 1) + '%', strong: true },
      { label: '分级', value: result.category || DASH, strong: true },
      { label: 'BMI（参考）', value: fmt(result.bmi, 1) },
      { label: '脂肪重量', value: result.fatMass == null ? DASH : fmt(result.fatMass, 1), unit: 'kg' },
      { label: '去脂体重', value: result.leanMass == null ? DASH : fmt(result.leanMass, 1), unit: 'kg' }
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
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-bfp') : null) || view;
    var state = { gender: 'male', age: null, height: null, weight: null };
    var host = MD.ui.el('div');

    function refresh() { paint(host, evaluate(state)); }

    var genderField = MD.tools.select({
      label: '性别',
      value: 'male',
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

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '体脂率 = 1.20 × BMI + 0.23 × 年龄 − 10.8 × 性别 − 5.4（男性性别取 1，女性取 0）。男性正常 10~20%，女性正常 18~28%。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '12px' } }, [
      genderField.row, ageField.row, heightField.row, weightField.row
    ]));
    content.appendChild(host);
    refresh();
  }

  MD.tools.register({
    id: 'health-bfp',
    title: '体脂率计算器',
    subtitle: '按 BMI 估算体脂百分比',
    icon: 'heart',
    category: '健康工具',
    order: 60,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

/* ==========================================================================
 * tool-health-bmr.js —— 工具箱「基础代谢率计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求；界面文字一律 textContent。
 *
 * 【两种公式，都给出，便于对照】
 * 1) Mifflin-St Jeor（1990，现代人群偏差更小，推荐）
 *      男：10×kg + 6.25×cm − 5×年龄 + 5
 *      女：10×kg + 6.25×cm − 5×年龄 − 161
 * 2) Harris-Benedict 修订版（Roza & Shizgal 1984，经典式）
 *      男：88.362 + 13.397×kg + 4.799×cm − 5.677×年龄
 *      女：447.593 + 9.247×kg + 3.098×cm − 4.330×年龄
 *
 * TDEE = BMR × 活动系数。
 * 目标热量 = TDEE + 每周目标体重变化折算的日差额；1 kg 脂肪按 7700 kcal 折算。
 * 目标热量低于 BMR 时给出警告——长期低于基础代谢的摄入不建议自行执行。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';
  var KCAL_PER_KG_FAT = 7700;

  var FORMULAS = [
    { value: 'mifflin', label: 'Mifflin-St Jeor 公式（推荐）' },
    { value: 'harris', label: 'Harris-Benedict 公式（经典）' },
    { value: 'both', label: '两种公式对比' }
  ];

  // 活动系数：直接下拉选择，不做「先勾选再显示」的多余交互
  var ACTIVITY = [
    { value: 'sedentary', label: '久坐不动（很少或没有运动，办公室工作）', factor: 1.2 },
    { value: 'light', label: '轻度活动（每周运动 1~3 天）', factor: 1.375 },
    { value: 'moderate', label: '中度活动（每周运动 3~5 天）', factor: 1.55 },
    { value: 'active', label: '高度活动（每周运动 6~7 天）', factor: 1.725 },
    { value: 'very_active', label: '极高度活动（每天运动，体力劳动者或运动员）', factor: 1.9 }
  ];

  // 体重管理目标：weeklyKg 为正表示增重，为负表示减重
  var GOALS = [
    { value: 'fast_loss', label: '快速减脂（每周 −0.75 kg）', weeklyKg: -0.75 },
    { value: 'slow_loss', label: '缓慢减脂（每周 −0.5 kg）', weeklyKg: -0.5 },
    { value: 'mild_loss', label: '温和减脂（每周 −0.25 kg）', weeklyKg: -0.25 },
    { value: 'maintain', label: '维持体重', weeklyKg: 0 },
    { value: 'mild_gain', label: '缓慢增肌（每周 +0.25 kg）', weeklyKg: 0.25 },
    { value: 'fast_gain', label: '快速增肌（每周 +0.5 kg）', weeklyKg: 0.5 }
  ];

  function factorOf(id) {
    for (var i = 0; i < ACTIVITY.length; i++) if (ACTIVITY[i].value === id) return ACTIVITY[i].factor;
    return ACTIVITY[0].factor;
  }

  function goalOf(id) {
    for (var i = 0; i < GOALS.length; i++) if (GOALS[i].value === id) return GOALS[i];
    return GOALS[3];
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

  // 热量取整显示：小数位对千卡没有意义，反而放大「精确性」错觉
  function kcal(value) {
    if (typeof value !== 'number' || !isFinite(value)) return DASH;
    return String(Math.round(value));
  }

  function mifflin(sex, w, h, age) {
    var base = 10 * w + 6.25 * h - 5 * age;
    return sex === 'male' ? base + 5 : base - 161;
  }

  function harris(sex, w, h, age) {
    return sex === 'male'
      ? 88.362 + 13.397 * w + 4.799 * h - 5.677 * age
      : 447.593 + 9.247 * w + 3.098 * h - 4.330 * age;
  }

  /**
   * @param {{gender:string, age:number|null, height:number|null, weight:number|null,
   *          activity:string, goal:string, formula:string}} s
   */
  function evaluate(s) {
    var out = {
      mifflin: null, harris: null, bmr: null, tdee: null, target: null,
      perKg: null, tone: 'ok', error: '', note: '', goalLabel: ''
    };
    var sex = s.gender === 'female' ? 'female' : 'male';
    var h = s.height, w = s.weight, age = s.age;

    if (h == null || w == null || age == null) { out.error = '填写性别、年龄、身高、体重后自动计算'; return out; }
    if (h <= 0) { out.error = '身高需大于 0'; return out; }
    if (w <= 0) { out.error = '体重需大于 0'; return out; }
    if (age <= 0 || age > 120) { out.error = '年龄需在 1 ~ 120 之间'; return out; }

    var m = mifflin(sex, w, h, age);
    var hr = harris(sex, w, h, age);
    var bmr = s.formula === 'harris' ? hr : m;

    if (!isFinite(bmr) || bmr < 500 || bmr > 6000) {
      out.error = '结果超出常见范围，请核对输入';
      out.tone = 'warn';
      return out;
    }

    out.mifflin = m;
    out.harris = hr;
    out.bmr = bmr;
    out.tdee = bmr * factorOf(s.activity);
    out.perKg = bmr / w;

    var goal = goalOf(s.goal);
    out.goalLabel = goal.label;
    // 每周变化量折算成每日差额：7700 kcal ≈ 1 kg 脂肪
    var daily = goal.weeklyKg * KCAL_PER_KG_FAT / 7;
    out.target = out.tdee + daily;

    if (out.target < bmr) {
      out.tone = 'warn';
      out.note = '目标热量已低于基础代谢，长期如此需在专业人士指导下进行';
    }
    return out;
  }

  function paint(host, result, formula) {
    MD.ui.clear(host);
    var items = [];

    if (formula === 'both') {
      items.push({ label: 'Mifflin-St Jeor', value: kcal(result.mifflin), unit: 'kcal/天', strong: true });
      items.push({ label: 'Harris-Benedict', value: kcal(result.harris), unit: 'kcal/天', strong: true });
    } else {
      items.push({ label: '基础代谢率（BMR）', value: kcal(result.bmr), unit: 'kcal/天', strong: true });
    }

    items.push({ label: '每日总消耗（TDEE）', value: kcal(result.tdee), unit: 'kcal/天', strong: true });
    items.push({ label: '目标热量 · ' + (result.goalLabel || '维持体重'), value: kcal(result.target), unit: 'kcal/天' });
    items.push({ label: '每公斤体重消耗', value: fmt(result.perKg, 1), unit: 'kcal/kg' });
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
    var s = { gender: 'male', age: null, height: null, weight: null, activity: 'sedentary', goal: 'maintain', formula: 'mifflin' };
    var host = MD.ui.el('div');

    function refresh() { paint(host, evaluate(s), s.formula); }

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: 'BMR 是静息状态下的最低能量消耗；TDEE 再乘活动系数。目标热量按每周体重变化折算（1 kg 脂肪≈7700 kcal）。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));

    content.appendChild(MD.tools.select({
      label: '性别', value: 'male',
      options: [{ value: 'male', label: '男' }, { value: 'female', label: '女' }],
      onChange: function (v) { s.gender = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '年龄', unit: '岁', placeholder: '例如 30', step: '1', min: '1',
      onInput: function (v) { s.age = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '身高', unit: 'cm', placeholder: '例如 170', step: '0.1', min: '0',
      onInput: function (v) { s.height = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '体重', unit: 'kg', placeholder: '例如 65', step: '0.1', min: '0',
      onInput: function (v) { s.weight = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '计算公式', value: 'mifflin', options: FORMULAS,
      onChange: function (v) { s.formula = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '活动水平', value: 'sedentary',
      options: ACTIVITY.map(function (a) { return { value: a.value, label: a.label }; }),
      onChange: function (v) { s.activity = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '体重管理目标', value: 'maintain',
      options: GOALS.map(function (g) { return { value: g.value, label: g.label }; }),
      onChange: function (v) { s.goal = v; refresh(); }
    }).row);

    content.appendChild(host);

    content.appendChild(MD.ui.el('div', {
      style: { marginTop: '16px', fontSize: '13px', lineHeight: '1.7', color: 'var(--muted, #666)' }
    }, [
      MD.ui.el('div', { text: '注意事项', style: { fontWeight: '700', marginBottom: '6px' } }),
      MD.ui.el('div', { text: '· 两种公式结果通常相差 5% 以内，不必纠结取哪一个' }),
      MD.ui.el('div', { text: '· 估算值个体差异可达 ±10%，甲状腺疾病、孕期请以医生意见为准' }),
      MD.ui.el('div', { text: '· 每周减重超过 1 kg 通常难以持续，也不利于保住肌肉' }),
      MD.ui.el('div', { text: '· 增肌需要配合力量训练，单纯加热量主要长脂肪' })
    ]));

    refresh();
  }

  MD.tools.register({
    id: 'health-bmr',
    title: '基础代谢率计算器',
    subtitle: '两种公式与目标热量',
    icon: 'heart',
    category: '健康工具',
    order: 70,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

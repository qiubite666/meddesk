/* ==========================================================================
 * tool-health-fitness-age.js —— 工具箱「心血管健康年龄评估」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求；界面文字一律 textContent。
 *
 * 【算法口径 · 请务必读这一段】
 * 参考站只说明「基于 VO2max 推算心血管年龄」，未公开具体公式。
 * 本实现用公开文献重建一个多元模型，各步都可核对：
 *   1) 最大心率      HRmax = 208 − 0.7 × 实际年龄        （Tanaka 2001）
 *   2) 摄氧量基线    VO2max = 15.3 × (HRmax / 静息心率)   （Uth-Sørensen 2004）
 *   3) 体重修正      BMI 越高，按体重计的摄氧量越低：
 *                    factor = clamp(1 − (BMI − 22) × 0.006, 0.85, 1.05)
 *   4) 生活方式修正（四项相乘，系数见下方常量表）
 *                    运动频率 × 每次时长 × 运动强度 × 吸烟情况
 *   5) 人群参考曲线  以 20 岁为峰值：男 48、女 38 ml/kg/min，
 *                    此后每岁降 0.45（男）/ 0.35（女）
 *   6) 反解健康年龄  健康年龄 = 20 + (峰值 − 你的 VO2max) / 下降速率
 *
 * 【为什么必须把话说清楚】
 * 第 3、4 步的系数是为「还原参考站的多元输入」而做的合理近似，不是某个
 * 公开量表的原值。所以本页结果是**相对参考**而非临床指标，结论区固定显示
 * 「仅供参考，非医学诊断」。
 *
 * 【已知局限】
 * - 静息心率测量状态影响极大（走动后、咖啡因、紧张都会抬高）。
 * - 服用 β 受体阻滞剂等降心率药物者，心率被压低会显著高估摄氧量。
 * - 该模型不能替代运动心肺功能试验（CPET）。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  var PEAK_AGE = 20;
  var REF = {
    male: { peak: 48, slope: 0.45 },
    female: { peak: 38, slope: 0.35 }
  };

  var AGE_MIN = 18;
  var AGE_MAX = 90;

  // 生活方式系数：取自训练量与心血管适能的常见量效关系，用于近似还原参考站的多因子输入
  var FREQ = [
    { value: 'none', label: '几乎不运动', factor: 0.90 },
    { value: 'f1', label: '1~2 次/周', factor: 1.00 },
    { value: 'f3', label: '3~4 次/周', factor: 1.07 },
    { value: 'f5', label: '5~6 次/周', factor: 1.13 },
    { value: 'daily', label: '每天运动', factor: 1.18 }
  ];
  var DURATION = [
    { value: 'd15', label: '少于 15 分钟', factor: 0.97 },
    { value: 'd30', label: '15~30 分钟', factor: 1.00 },
    { value: 'd60', label: '30~60 分钟', factor: 1.03 },
    { value: 'd90', label: '超过 60 分钟', factor: 1.06 }
  ];
  var INTENSITY = [
    { value: 'low', label: '低强度（散步）', factor: 0.95 },
    { value: 'midlow', label: '中低强度（快走）', factor: 1.00 },
    { value: 'mid', label: '中强度（慢跑）', factor: 1.04 },
    { value: 'midhigh', label: '中高强度（跑步）', factor: 1.07 },
    { value: 'high', label: '高强度（剧烈运动）', factor: 1.10 }
  ];
  var SMOKING = [
    { value: 'never', label: '不吸烟', factor: 1.00 },
    { value: 'quit', label: '已戒烟', factor: 0.97 },
    { value: 'sometimes', label: '偶尔吸烟', factor: 0.95 },
    { value: 'daily', label: '每天吸烟', factor: 0.90 }
  ];

  function pick(list, id) {
    for (var i = 0; i < list.length; i++) if (list[i].value === id) return list[i];
    return list[0];
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

  function bmiFactor(bmi) {
    if (bmi == null) return 1;
    var f = 1 - (bmi - 22) * 0.006;
    return Math.max(0.85, Math.min(1.05, f));
  }

  /**
   * @param {{gender:string, age:number|null, height:number|null, weight:number|null,
   *          restingHr:number|null, freq:string, duration:string, intensity:string, smoking:string}} s
   */
  function evaluate(s) {
    var out = {
      vo2max: null, fitnessAge: null, hrMax: null, delta: null, bmi: null,
      category: '', tone: '', error: '', note: ''
    };
    var sex = s.gender === 'female' ? 'female' : 'male';
    var age = s.age, hr = s.restingHr;

    if (age == null || hr == null) { out.error = '填写年龄与静息心率后自动计算'; return out; }
    if (age <= 0 || age > 120) { out.error = '年龄需在 1 ~ 120 之间'; return out; }
    // 低于 30 或高于 150 基本属测量错误或极端病理状态，直接拒绝
    if (hr < 30 || hr > 150) { out.error = '静息心率需在 30 ~ 150 次/分之间'; return out; }

    var hrMax = 208 - 0.7 * age;
    var base = 15.3 * (hrMax / hr);
    if (!isFinite(base) || base <= 0) { out.error = '无法从当前输入估算摄氧量'; return out; }

    var bmi = null;
    if (s.height != null && s.weight != null && s.height > 0 && s.weight > 0) {
      var m = s.height / 100;
      bmi = s.weight / (m * m);
    }

    var vo2 = base
      * bmiFactor(bmi)
      * pick(FREQ, s.freq).factor
      * pick(DURATION, s.duration).factor
      * pick(INTENSITY, s.intensity).factor
      * pick(SMOKING, s.smoking).factor;

    // 人类 VO2max 的生理边界，越界说明输入或模型已不可信
    vo2 = Math.max(10, Math.min(80, vo2));

    var ref = REF[sex];
    var rawAge = PEAK_AGE + (ref.peak - vo2) / ref.slope;
    var clamped = Math.max(AGE_MIN, Math.min(AGE_MAX, rawAge));
    var delta = clamped - age;

    out.hrMax = hrMax;
    out.vo2max = vo2;
    out.bmi = bmi;
    out.fitnessAge = clamped;
    out.delta = delta;

    if (delta <= -5) { out.category = '优于同龄人'; out.tone = 'ok'; }
    else if (delta <= 5) { out.category = '与年龄相符'; out.tone = 'ok'; }
    else if (delta <= 10) { out.category = '略高于实际年龄'; out.tone = 'warn'; }
    else { out.category = '明显高于实际年龄'; out.tone = 'danger'; }

    if (rawAge !== clamped) {
      out.note = '按公式算出的年龄超出合理区间，已收窄显示，请核对心率测量';
    }
    out.note = (out.note ? out.note + '；' : '') + '仅供参考，非医学诊断';
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);
    var items = [
      { label: '心血管健康年龄', value: result.fitnessAge == null ? DASH : fmt(result.fitnessAge, 1), unit: '岁', strong: true },
      { label: '与本人年龄之差', value: deltaText(result.delta), unit: '岁', strong: true },
      { label: '评估', value: result.category || DASH },
      { label: '估算最大摄氧量', value: result.vo2max == null ? DASH : fmt(result.vo2max, 1), unit: 'ml/kg/min' },
      { label: '估算最大心率', value: result.hrMax == null ? DASH : fmt(result.hrMax, 0), unit: '次/分' }
    ];
    if (result.bmi != null) items.push({ label: 'BMI（参与修正）', value: fmt(result.bmi, 1) });
    if (result.note) items.push({ label: '提示', value: result.note });
    if (result.error) items.push({ label: '无法计算', value: result.error });

    host.appendChild(MD.tools.resultCard({
      title: '计算结果',
      tone: result.error ? 'warn' : result.tone,
      items: items
    }));
  }

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-fitness-age') : null) || view;
    var s = {
      gender: 'male', age: null, height: null, weight: null, restingHr: null,
      freq: 'none', duration: 'd30', intensity: 'midlow', smoking: 'never'
    };
    var host = MD.ui.el('div');

    function refresh() { paint(host, evaluate(s)); }

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '静息心率请取清晨刚醒、未下床时的一分钟脉搏。本工具结合心率、体重与运动习惯估算心血管年龄。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));

    content.appendChild(MD.tools.select({
      label: '性别', value: 'male',
      options: [{ value: 'male', label: '男' }, { value: 'female', label: '女' }],
      onChange: function (v) { s.gender = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '实际年龄', unit: '岁', placeholder: '例如 30', step: '1', min: '1',
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

    content.appendChild(MD.tools.field({
      label: '静息心率', unit: '次/分', placeholder: '例如 65', step: '1', min: '30',
      onInput: function (v) { s.restingHr = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.ui.el('div', { text: '生活方式', style: { fontWeight: '700', margin: '6px 0 0' } }));

    content.appendChild(MD.tools.select({
      label: '每周运动频率', value: 'none',
      options: FREQ.map(function (x) { return { value: x.value, label: x.label }; }),
      onChange: function (v) { s.freq = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '每次运动时长', value: 'd30',
      options: DURATION.map(function (x) { return { value: x.value, label: x.label }; }),
      onChange: function (v) { s.duration = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '运动强度', value: 'midlow',
      options: INTENSITY.map(function (x) { return { value: x.value, label: x.label }; }),
      onChange: function (v) { s.intensity = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '吸烟情况', value: 'never',
      options: SMOKING.map(function (x) { return { value: x.value, label: x.label }; }),
      onChange: function (v) { s.smoking = v; refresh(); }
    }).row);

    content.appendChild(host);

    content.appendChild(MD.ui.el('div', {
      style: { marginTop: '16px', fontSize: '13px', lineHeight: '1.7', color: 'var(--muted, #666)' }
    }, [
      MD.ui.el('div', { text: '什么是心血管年龄', style: { fontWeight: '700', marginBottom: '6px' } }),
      MD.ui.el('div', { text: '由最大摄氧量推算出的心肺「生理年龄」，可反映心血管系统的实际状态，通常与身份证年龄不相等。' }),
      MD.ui.el('div', { text: '如何改善', style: { fontWeight: '700', margin: '10px 0 6px' } }),
      MD.ui.el('div', { text: '规律有氧运动最有效。坚持 6~12 周训练，摄氧量可提升 10~20%，相当于心血管年龄年轻 5~10 岁。' }),
      MD.ui.el('div', { text: '注意事项', style: { fontWeight: '700', margin: '10px 0 6px' } }),
      MD.ui.el('div', { text: '· 本评估仅供参考，不能替代专业医学检查' }),
      MD.ui.el('div', { text: '· 服用降心率药物者结果偏差较大，不建议参考' }),
      MD.ui.el('div', { text: '· 有心血管家族史或相关症状，请咨询医生' })
    ]));

    refresh();
  }

  MD.tools.register({
    id: 'health-fitness-age',
    title: '心血管健康年龄评估',
    subtitle: '由心率与运动习惯推算',
    icon: 'heart',
    category: '健康工具',
    order: 90,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

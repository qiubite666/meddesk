/* ==========================================================================
 * tool-health-fitness-age.js —— 工具箱「心血管健康年龄评估」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求。
 *
 * 【口径说明 · 请务必读】
 * 该工具用「静息心率」反推最大摄氧量（VO2max），再换算成同龄人群中的等效年龄。
 *   1) 最大心率：HRmax = 208 − 0.7 × 年龄      （Tanaka 2001，比 220−年龄 更准）
 *   2) 摄氧量估算：VO2max = 15.3 × (HRmax / 静息心率)   （Uth-Sørensen 2004）
 *   3) 人群参考值：以 20 岁为峰值，男性 48、女性 38 ml/kg/min，
 *      此后每岁下降 0.45（男）/ 0.35（女）ml/kg/min —— 这是公开文献的常见下降速率区间取值，
 *      用于构造「同龄平均线」，从而反解出等效年龄：
 *      健康年龄 = 20 + (参考峰值 − 你的 VO2max) / 下降速率
 *
 * 【已知局限，必须让用户看到】
 * - 静息心率受测量时状态影响极大（刚走动、咖啡因、紧张都会抬高），一次测量不足以定论。
 * - 该换算是人群统计模型，不是医学诊断；不能替代运动心肺功能试验（CPET）。
 * - 服用 β 受体阻滞剂等降心率药物的人会被显著低估心率、高估摄氧量，结果不可用。
 * 因此本工具在结果卡里固定展示一条「仅供参考，非医学诊断」的提示。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  // 人群参考模型参数（ml/kg/min）
  var PEAK_AGE = 20;
  var REF = {
    male: { peak: 48, slope: 0.45 },
    female: { peak: 38, slope: 0.35 }
  };

  var AGE_MIN = 18;
  var AGE_MAX = 90;

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

  /**
   * @param {{gender:string, age:number|null, restingHr:number|null}} state
   */
  function evaluate(state) {
    var out = { vo2max: null, fitnessAge: null, hrMax: null, delta: null, category: '', tone: '', error: '', note: '' };
    var sex = state.gender === 'female' ? 'female' : 'male';
    var age = state.age, hr = state.restingHr;

    if (age == null || hr == null) { out.error = '填写年龄与静息心率后自动计算'; return out; }
    if (age <= 0 || age > 120) { out.error = '年龄需在 1 ~ 120 之间'; return out; }
    // 静息心率低于 30 或高于 150 基本属于测量错误或极端病理状态，直接拒绝
    if (hr < 30 || hr > 150) { out.error = '静息心率需在 30 ~ 150 次/分之间'; return out; }

    var hrMax = 208 - 0.7 * age;
    var vo2 = 15.3 * (hrMax / hr);
    if (!isFinite(vo2) || vo2 <= 0) { out.error = '无法从当前输入估算摄氧量'; return out; }

    var ref = REF[sex];
    var fitnessAge = PEAK_AGE + (ref.peak - vo2) / ref.slope;
    // 反解可能落到不合理区间（心率异常低时会算出个位数年龄），钳制后仍需提示
    var clamped = Math.max(AGE_MIN, Math.min(AGE_MAX, fitnessAge));
    var delta = clamped - age;

    out.vo2max = vo2;
    out.hrMax = hrMax;
    out.fitnessAge = clamped;
    out.delta = delta;

    if (delta <= -5) { out.category = '优于同龄人'; out.tone = 'ok'; }
    else if (delta <= 5) { out.category = '与年龄相符'; out.tone = 'ok'; }
    else if (delta <= 10) { out.category = '略高于实际年龄'; out.tone = 'warn'; }
    else { out.category = '明显高于实际年龄'; out.tone = 'danger'; }

    if (fitnessAge !== clamped) out.note = '按公式算得的年龄超出合理区间，已收窄显示，请核对心率测量';
    out.note = (out.note ? out.note + '；' : '') + '仅供参考，非医学诊断；静息心率请于安静状态下测量';
    return out;
  }

  function deltaText(delta) {
    if (typeof delta !== 'number' || !isFinite(delta)) return DASH;
    var sign = delta > 0 ? '+' : (delta < 0 ? '-' : '');
    return sign + fmt(Math.abs(delta), 1);
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
    var state = { gender: 'male', age: null, restingHr: null };
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
    var hrField = MD.tools.field({
      label: '静息心率', unit: '次/分', placeholder: '例如 65', step: '1', min: '30',
      onInput: function (v) { state.restingHr = toNumber(v); refresh(); }
    });

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '静息心率取清晨刚醒、未下床时的一分钟脉搏最准。本工具由心率反推最大摄氧量并换算为等效年龄，属人群统计估算，不能替代医学检查。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));
    content.appendChild(MD.ui.el('div', { class: 'stack', style: { gap: '12px' } }, [
      genderField.row, ageField.row, hrField.row
    ]));
    content.appendChild(host);
    refresh();
  }

  MD.tools.register({
    id: 'health-fitness-age',
    title: '心血管健康年龄评估',
    subtitle: '由静息心率推算健康年龄',
    icon: 'heart',
    category: '健康工具',
    order: 90,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

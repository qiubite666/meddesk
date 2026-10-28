/* ==========================================================================
 * tool-health-ideal-weight.js —— 工具箱「理想体重计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求；界面文字一律 textContent。
 *
 * 【四个经典公式，均以「超过 5 英尺(60 英寸)的部分」为增量】
 *   Devine  (1974)：男 50 + 2.3×over ｜ 女 45.5 + 2.3×over
 *   Robinson(1983)：男 52 + 1.9×over ｜ 女 49   + 1.7×over
 *   Hamwi   (1964)：男 48 + 2.7×over ｜ 女 45.5 + 2.2×over
 *   Miller  (1983)：男 56.2+1.41×over ｜ 女 53.1+1.36×over
 *   其中 over = 身高(英寸) − 60，身高不足 5 英尺时按 0 计（公式本身不适用矮身高）。
 *
 * 【骨架类型修正】小骨架 −10%、中等 0%、大骨架 +10%，只作用于上面四个公式值；
 * BMI 区间本身就是范围，不做骨架修正。
 * 「自动」判定用身高分档（无腕围输入时的通行近似）：
 *   男 <165cm 小 / 165~178 中 / >178 大；女 <152cm 小 / 152~165 中 / >165 大。
 *
 * 【健康 BMI 区间采用中国标准 18.5~23.9】
 * 与本项目 BMI 计算器保持一致，不混用 WHO 的 24.9，避免同一套数据两种结论。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';
  var CM_PER_IN = 2.54;
  var OVER_BASE_IN = 60;

  var BMI_LOW = 18.5;
  var BMI_HEALTH_HIGH = 23.9;   // 中国标准
  var BMI_OVERWEIGHT = 24;
  var BMI_OBESE = 28;

  var FORMULAS = [
    { value: 'devine', label: 'Devine 公式（1974）' },
    { value: 'robinson', label: 'Robinson 公式（1983）' },
    { value: 'hamwi', label: 'Hamwi 公式（1964）' },
    { value: 'miller', label: 'Miller 公式（1983）' },
    { value: 'bmi', label: '健康 BMI 范围（18.5~23.9）' }
  ];

  var FRAMES = [
    { value: 'small', label: '小骨架（−10%）', mul: 0.9 },
    { value: 'medium', label: '中等骨架', mul: 1 },
    { value: 'large', label: '大骨架（+10%）', mul: 1.1 },
    { value: 'auto', label: '自动判定', mul: 1 }
  ];

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

  function autoFrame(sex, cm) {
    if (sex === 'male') return cm < 165 ? 'small' : (cm > 178 ? 'large' : 'medium');
    return cm < 152 ? 'small' : (cm > 165 ? 'large' : 'medium');
  }

  function frameMul(frameValue, sex, cm) {
    var v = frameValue === 'auto' ? autoFrame(sex, cm) : frameValue;
    for (var i = 0; i < FRAMES.length; i++) if (FRAMES[i].value === v) return { mul: FRAMES[i].mul, resolved: v };
    return { mul: 1, resolved: 'medium' };
  }

  // 四个经典公式统一入口：sex 决定基数与增量
  function classic(id, sex, over) {
    var male = sex === 'male';
    if (id === 'devine') return (male ? 50 : 45.5) + 2.3 * over;
    if (id === 'robinson') return (male ? 52 : 49) + 1.9 * over;
    if (id === 'hamwi') return (male ? 48 : 45.5) + 2.7 * over;
    if (id === 'miller') return (male ? 56.2 : 53.1) + 1.41 * over;
    return null;
  }

  var FRAME_LABEL = { small: '小骨架', medium: '中等骨架', large: '大骨架' };

  /**
   * @param {{gender:string, age:number|null, height:number|null, weight:number|null,
   *          frame:string, formula:string}} s
   */
  function evaluate(s) {
    var out = {
      ideal: null, low: null, high: null, diff: null, bmiNow: null,
      frameText: '', formulaLabel: '', comparison: [], ranges: [],
      error: '', note: ''
    };
    var sex = s.gender === 'female' ? 'female' : 'male';
    var h = s.height;

    if (h == null) { out.error = '填写身高后自动计算'; return out; }
    if (h <= 0) { out.error = '身高需大于 0'; return out; }

    var m = h / 100;
    var hIn = h / CM_PER_IN;
    var over = Math.max(0, hIn - OVER_BASE_IN);

    var fm = frameMul(s.frame, sex, h);
    out.frameText = FRAME_LABEL[fm.resolved] || DASH;

    out.low = BMI_LOW * m * m;
    out.high = BMI_HEALTH_HIGH * m * m;

    // 五个口径全算一遍，既用于当前选择，也用于下方对比表
    ['devine', 'robinson', 'hamwi', 'miller'].forEach(function (id) {
      var val = classic(id, sex, over);
      out.comparison.push({
        label: id.charAt(0).toUpperCase() + id.slice(1) + ' 公式',
        value: val == null ? DASH : fmt(val * fm.mul, 1),
        unit: 'kg'
      });
    });
    out.comparison.push({
      label: '健康 BMI 区间',
      value: fmt(out.low, 1) + ' ~ ' + fmt(out.high, 1),
      unit: 'kg'
    });

    // 当前选择的公式
    if (s.formula === 'bmi') {
      out.ideal = (out.low + out.high) / 2;   // 取区间中点作为单一参考值
      out.formulaLabel = '健康 BMI 区间中点';
    } else {
      var v = classic(s.formula, sex, over);
      out.ideal = v == null ? null : v * fm.mul;
      out.formulaLabel = (FRAMES.filter(function (f) { return f.value === s.formula; })[0] || {}).label || '经典公式';
    }

    if (out.ideal == null || !isFinite(out.ideal) || out.ideal < 25 || out.ideal > 200) {
      out.error = '身高过矮时经典公式不适用（公式以 5 英尺为基准），建议改看健康 BMI 区间';
      out.ideal = null;
      return out;
    }

    // BMI 四档对应体重区间，用中国标准切分
    out.ranges = [
      { label: '过轻', range: '< ' + fmt(BMI_LOW * m * m, 1), tone: 'warn' },
      { label: '健康', range: fmt(BMI_LOW * m * m, 1) + ' ~ ' + fmt(BMI_HEALTH_HIGH * m * m, 1), tone: 'ok' },
      { label: '超重', range: fmt(BMI_OVERWEIGHT * m * m, 1) + ' ~ ' + fmt((BMI_OBESE * m * m) - 0.1, 1), tone: 'warn' },
      { label: '肥胖', range: '≥ ' + fmt(BMI_OBESE * m * m, 1), tone: 'danger' }
    ];

    if (s.weight != null && s.weight > 0) {
      out.diff = s.weight - out.ideal;
      out.bmiNow = s.weight / (m * m);
    }
    if (hIn <= OVER_BASE_IN) out.note = '身高不足 5 英尺，经典公式误差较大，建议以健康 BMI 区间为准';
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);

    var main = [
      { label: '理想体重（' + (result.formulaLabel || '—') + '）', value: result.ideal == null ? DASH : fmt(result.ideal, 1), unit: 'kg', strong: true },
      { label: '健康体重区间', value: fmt(result.low, 1) + ' ~ ' + fmt(result.high, 1), unit: 'kg', strong: true },
      { label: '骨架类型', value: result.frameText || DASH }
    ];
    if (result.diff != null) {
      var sign = result.diff > 0 ? '+' : (result.diff < 0 ? '-' : '');
      main.push({ label: '与当前体重之差', value: sign + fmt(Math.abs(result.diff), 1), unit: 'kg' });
    }
    if (result.bmiNow != null) main.push({ label: '当前 BMI', value: fmt(result.bmiNow, 1) });
    if (result.note) main.push({ label: '提示', value: result.note });
    if (result.error) main.push({ label: '无法计算', value: result.error });

    host.appendChild(MD.tools.resultCard({
      title: '理想体重分析',
      tone: result.error ? 'warn' : 'ok',
      items: main
    }));

    // 公式对比表：让用户看到各口径的离散程度，而不是迷信某一个数字
    if (result.comparison.length) {
      host.appendChild(MD.tools.resultCard({
        title: '各公式结果对比',
        tone: 'ok',
        items: result.comparison
      }));
    }

    // BMI 四档对应的体重区间
    if (result.ranges.length) {
      host.appendChild(MD.tools.resultCard({
        title: 'BMI 健康范围（体重）',
        tone: 'ok',
        items: result.ranges.map(function (r) {
          return { label: r.label, value: r.range, unit: 'kg' };
        })
      }));
    }
  }

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-ideal-weight') : null) || view;
    var s = { gender: 'male', age: null, height: null, weight: null, frame: 'auto', formula: 'devine' };
    var host = MD.ui.el('div');

    function refresh() { paint(host, evaluate(s)); }

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '理想体重是一个区间而不是一个点。下方会同时给出四个经典公式与健康 BMI 区间的结果，供你交叉参考。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));

    content.appendChild(MD.tools.select({
      label: '性别', value: 'male',
      options: [{ value: 'male', label: '男' }, { value: 'female', label: '女' }],
      onChange: function (v) { s.gender = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '年龄', unit: '岁', placeholder: '例如 30（可选）', step: '1', min: '0',
      onInput: function (v) { s.age = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '身高', unit: 'cm', placeholder: '例如 170', step: '0.1', min: '0',
      onInput: function (v) { s.height = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.field({
      label: '当前体重', unit: 'kg', placeholder: '例如 65（可选）', step: '0.1', min: '0',
      onInput: function (v) { s.weight = toNumber(v); refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '骨架类型', value: 'auto',
      options: FRAMES.map(function (f) { return { value: f.value, label: f.label }; }),
      onChange: function (v) { s.frame = v; refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '计算公式', value: 'devine', options: FORMULAS,
      onChange: function (v) { s.formula = v; refresh(); }
    }).row);

    content.appendChild(host);

    content.appendChild(MD.ui.el('div', {
      style: { marginTop: '16px', fontSize: '13px', lineHeight: '1.7', color: 'var(--muted, #666)' }
    }, [
      MD.ui.el('div', { text: '体重管理建议', style: { fontWeight: '700', marginBottom: '6px' } }),
      MD.ui.el('div', { text: '· 四个经典公式的离散度通常在 3~5 kg，落在健康 BMI 区间内即属正常' }),
      MD.ui.el('div', { text: '· 骨架类型靠身高近似判定，若知道腕围可自行按小/大骨架手动选择' }),
      MD.ui.el('div', { text: '· 增肌人群不必追求公式值，肌肉量高时体重略超区间是正常的' }),
      MD.ui.el('div', { text: '· 老年人与孕期人群不宜按本页结论控制体重' })
    ]));

    refresh();
  }

  MD.tools.register({
    id: 'health-ideal-weight',
    title: '理想体重计算器',
    subtitle: '五个公式与骨架修正',
    icon: 'heart',
    category: '健康工具',
    order: 80,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

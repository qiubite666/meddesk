/* ==========================================================================
 * tool-health-bfp.js —— 工具箱「体脂率计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js ：MD.ui / MD.icon / MD.util
 *   tools.js：MD.tools.register / mount / field / select / resultCard
 *
 * 分层约定：输入层 → 计算层 evaluate()（纯函数） → 渲染层 paint()
 * 零外部依赖、零网络请求；界面文字一律 textContent。
 *
 * 【三种算法的口径，写在下面便于复核】
 * 1) BMI 公式法（Deurenberg 1991）：
 *      体脂率 = 1.20 × BMI + 0.23 × 年龄 − 10.8 × 性别 − 5.4   （男=1，女=0）
 *      只需身高体重年龄性别；肌肉量大的人会被高估、老人会被低估。
 * 2) 美国海军公式法（US Navy，公制）：
 *      男：495 / (1.0324 − 0.19077·lg(腰围−颈围) + 0.15456·lg(身高)) − 450
 *      女：495 / (1.29579 − 0.35004·lg(腰围+臀围−颈围) + 0.22100·lg(身高)) − 450
 *      围度与身高单位均为 cm；比 BMI 法准，但要量三个围度。
 * 3) 皮褶厚度法（Jackson-Pollock 三点简化版，Siri 体密度换算）：
 *      男：密度 = 1.10938 − 0.0008267·S + 0.0000016·S² − 0.0002574·年龄
 *      女：密度 = 1.0994921 − 0.0009929·S + 0.0000023·S² − 0.0001392·年龄
 *      S 为三点皮褶之和（mm）；体脂率 = 495 / 密度 − 450
 *      男取 胸部+腹部+大腿，女取 三头肌+髂上+大腿。
 *
 * 【分级】男女各五档，阈值不可互换（生理差异，不是笔误）：
 *   男：必需 2~5 ｜ 运动员 6~13 ｜ 健康 14~17 ｜ 可接受 18~24 ｜ 肥胖 ≥25
 *   女：必需 10~13 ｜ 运动员 14~20 ｜ 健康 21~24 ｜ 可接受 25~31 ｜ 肥胖 ≥32
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';

  // 老 WebView 上 Math.log10 可能缺失，统一走这里
  var lg = Math.log10 || function (x) { return Math.log(x) / Math.LN10; };

  var METHODS = [
    { value: 'bmi', label: 'BMI 公式法' },
    { value: 'navy', label: '美国海军公式法' },
    { value: 'skinfold', label: '皮褶厚度法（简化版）' }
  ];

  // 用「小于上界」的写法，边界值不会因浮点加法判错
  var TIERS = {
    male: [
      { max: 6, label: '必需脂肪', tone: 'danger' },
      { max: 14, label: '运动员', tone: 'ok' },
      { max: 18, label: '健康', tone: 'ok' },
      { max: 25, label: '可接受', tone: 'warn' },
      { max: Infinity, label: '肥胖', tone: 'danger' }
    ],
    female: [
      { max: 14, label: '必需脂肪', tone: 'danger' },
      { max: 21, label: '运动员', tone: 'ok' },
      { max: 25, label: '健康', tone: 'ok' },
      { max: 32, label: '可接受', tone: 'warn' },
      { max: Infinity, label: '肥胖', tone: 'danger' }
    ]
  };

  var TIER_TEXT = {
    male: '男性：必需脂肪 2~5%，运动员 6~13%，健康 14~17%，可接受 18~24%，肥胖 ≥25%',
    female: '女性：必需脂肪 10~13%，运动员 14~20%，健康 21~24%，可接受 25~31%，肥胖 ≥32%'
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

  function classify(sex, bf) {
    var tiers = TIERS[sex] || TIERS.male;
    for (var i = 0; i < tiers.length; i++) {
      if (bf < tiers[i].max) return tiers[i];
    }
    return tiers[tiers.length - 1];
  }

  // 三种算法分别实现；任何不合法输入都返回 error，由上层统一报错，绝不返回 NaN
  function byBmi(s, sex) {
    if (s.height == null || s.weight == null || s.age == null) {
      return { error: 'BMI 公式法需要身高、体重、年龄' };
    }
    if (s.height <= 0 || s.weight <= 0) return { error: '身高与体重需大于 0' };
    if (s.age <= 0 || s.age > 120) return { error: '年龄需在 1 ~ 120 之间' };
    var m = s.height / 100;
    var bmi = s.weight / (m * m);
    return { bf: 1.20 * bmi + 0.23 * s.age - 10.8 * (sex === 'male' ? 1 : 0) - 5.4, bmi: bmi };
  }

  function byNavy(s, sex) {
    if (s.height == null || s.neck == null || s.waist == null) {
      return { error: '海军公式需要身高、颈围、腰围' };
    }
    if (sex === 'female' && s.hip == null) return { error: '女性海军公式还需臀围' };
    if (s.neck <= 0 || s.waist <= 0 || s.height <= 0) return { error: '围度与身高需大于 0' };

    var bf;
    if (sex === 'male') {
      var diff = s.waist - s.neck;
      if (diff <= 0) return { error: '腰围需大于颈围' };
      bf = 495 / (1.0324 - 0.19077 * lg(diff) + 0.15456 * lg(s.height)) - 450;
    } else {
      if (s.hip == null || s.hip <= 0) return { error: '臀围需大于 0' };
      var sum = s.waist + s.hip - s.neck;
      if (sum <= 0) return { error: '腰围+臀围需大于颈围' };
      bf = 495 / (1.29579 - 0.35004 * lg(sum) + 0.22100 * lg(s.height)) - 450;
    }
    return { bf: bf };
  }

  function bySkinfold(s, sex) {
    if (s.age == null || s.f1 == null || s.f2 == null || s.f3 == null) {
      return { error: '皮褶法需要年龄与三处皮褶厚度' };
    }
    if (s.age <= 0 || s.age > 120) return { error: '年龄需在 1 ~ 120 之间' };
    if (s.f1 <= 0 || s.f2 <= 0 || s.f3 <= 0) return { error: '皮褶厚度需大于 0（单位毫米）' };

    var sum = s.f1 + s.f2 + s.f3;
    var density = sex === 'male'
      ? 1.10938 - 0.0008267 * sum + 0.0000016 * sum * sum - 0.0002574 * s.age
      : 1.0994921 - 0.0009929 * sum + 0.0000023 * sum * sum - 0.0001392 * s.age;

    if (!isFinite(density) || density <= 0) return { error: '输入超出该公式适用范围' };
    return { bf: 495 / density - 450 };
  }

  /**
   * @param {{method:string, gender:string, age:number|null, height:number|null,
   *          weight:number|null, neck:number|null, waist:number|null, hip:number|null,
   *          f1:number|null, f2:number|null, f3:number|null}} s
   */
  function evaluate(s) {
    var out = { bf: null, bmi: null, category: '', tone: '', fatMass: null, leanMass: null, error: '', note: '' };
    var sex = s.gender === 'female' ? 'female' : 'male';
    var r;

    if (s.method === 'navy') r = byNavy(s, sex);
    else if (s.method === 'skinfold') r = bySkinfold(s, sex);
    else r = byBmi(s, sex);

    if (r.error) { out.error = r.error; return out; }

    var bf = r.bf;
    // 生理上不可能的比例直接拒绝，不给「看似合理」的错数
    if (!isFinite(bf) || bf < 2 || bf > 70) {
      out.error = '结果超出该公式适用范围，请核对输入';
      return out;
    }

    out.bf = bf;
    out.bmi = r.bmi == null ? null : r.bmi;
    var tier = classify(sex, bf);
    out.category = tier.label;
    out.tone = tier.tone;

    if (s.weight != null && s.weight > 0) {
      out.fatMass = s.weight * bf / 100;
      out.leanMass = s.weight - out.fatMass;
    }
    out.note = TIER_TEXT[sex];
    return out;
  }

  function paint(host, result) {
    MD.ui.clear(host);
    var items = [
      { label: '体脂率', value: result.bf == null ? DASH : fmt(result.bf, 1) + '%', strong: true },
      { label: '分级', value: result.category || DASH, strong: true },
      { label: '脂肪重量', value: result.fatMass == null ? DASH : fmt(result.fatMass, 1), unit: 'kg' },
      { label: '去脂体重', value: result.leanMass == null ? DASH : fmt(result.leanMass, 1), unit: 'kg' }
    ];
    if (result.bmi != null) items.push({ label: 'BMI（参考）', value: fmt(result.bmi, 1) });
    if (result.note) items.push({ label: '参考标准', value: result.note });
    if (result.error) items.push({ label: '无法计算', value: result.error });

    host.appendChild(MD.tools.resultCard({
      title: '计算结果',
      tone: result.error ? 'warn' : result.tone,
      items: items
    }));
  }

  function render(view) {
    var content = (MD.tools && typeof MD.tools.mount === 'function' ? MD.tools.mount(view, 'health-bfp') : null) || view;
    var s = {
      method: 'bmi', gender: 'male', age: null, height: null, weight: null,
      neck: null, waist: null, hip: null, f1: null, f2: null, f3: null
    };
    var host = MD.ui.el('div');
    var inputsHost = MD.ui.el('div', { class: 'stack', style: { gap: '12px' } });

    function refresh() { paint(host, evaluate(s)); }

    function num(key, label, unit, placeholder) {
      return MD.tools.field({
        label: label, unit: unit, placeholder: placeholder, step: '0.1', min: '0',
        onInput: function (v) { s[key] = toNumber(v); refresh(); }
      }).row;
    }

    function buildInputs() {
      MD.ui.clear(inputsHost);
      var female = s.gender === 'female';

      // 年龄与性别对三种算法都有影响，固定放最前
      inputsHost.appendChild(MD.tools.field({
        label: '年龄', unit: '岁', placeholder: '例如 30', step: '1', min: '1',
        onInput: function (v) { s.age = toNumber(v); refresh(); }
      }).row);

      if (s.method === 'navy') {
        inputsHost.appendChild(num('height', '身高', 'cm', '例如 170'));
        inputsHost.appendChild(num('neck', '颈围', 'cm', '喉结下方最细处'));
        inputsHost.appendChild(num('waist', '腰围', 'cm', '肚脐水平一周'));
        if (female) inputsHost.appendChild(num('hip', '臀围', 'cm', '臀部最丰满处'));
        inputsHost.appendChild(num('weight', '体重（可选）', 'kg', '填了才显示脂肪重量'));
      } else if (s.method === 'skinfold') {
        inputsHost.appendChild(num('weight', '体重（可选）', 'kg', '填了才显示脂肪重量'));
        if (female) {
          inputsHost.appendChild(num('f1', '三头肌皮褶', 'mm', '手臂后侧'));
          inputsHost.appendChild(num('f2', '髂上皮褶', 'mm', '髂骨上方斜向'));
          inputsHost.appendChild(num('f3', '大腿皮褶', 'mm', '大腿前侧中点'));
        } else {
          inputsHost.appendChild(num('f1', '胸部皮褶', 'mm', '乳头与腋窝连线中点'));
          inputsHost.appendChild(num('f2', '腹部皮褶', 'mm', '肚脐旁 2cm 垂直捏起'));
          inputsHost.appendChild(num('f3', '大腿皮褶', 'mm', '大腿前侧中点'));
        }
      } else {
        inputsHost.appendChild(num('height', '身高', 'cm', '例如 170'));
        inputsHost.appendChild(num('weight', '体重', 'kg', '例如 65'));
      }
    }

    content.appendChild(MD.ui.el('p', {
      class: 'muted',
      text: '选择一种算法后填写对应数据。三种方法结果会有差异，建议固定用同一种来追踪变化。',
      style: { margin: '0 0 12px', fontSize: '13px' }
    }));

    content.appendChild(MD.tools.select({
      label: '性别', value: 'male',
      options: [{ value: 'male', label: '男' }, { value: 'female', label: '女' }],
      onChange: function (v) { s.gender = v; buildInputs(); refresh(); }
    }).row);

    content.appendChild(MD.tools.select({
      label: '计算方法', value: 'bmi', options: METHODS,
      onChange: function (v) { s.method = v; buildInputs(); refresh(); }
    }).row);

    content.appendChild(inputsHost);
    content.appendChild(host);

    content.appendChild(MD.ui.el('div', {
      style: { marginTop: '16px', fontSize: '13px', lineHeight: '1.7', color: 'var(--muted, #666)' }
    }, [
      MD.ui.el('div', { text: '健康建议', style: { fontWeight: '700', marginBottom: '6px' } }),
      MD.ui.el('div', { text: '· 建议每月测一次，看趋势比看单次数值更有意义' }),
      MD.ui.el('div', { text: '· 结合体重与体脂率一起评估，不要只盯体重数字' }),
      MD.ui.el('div', { text: '· 体脂率过低同样有害，男性低于 6%、女性低于 14% 需关注' }),
      MD.ui.el('div', { text: '· 不同方法不可直接比较，追踪时请固定同一种算法' })
    ]));

    buildInputs();
    refresh();
  }

  MD.tools.register({
    id: 'health-bfp',
    title: '体脂率计算器',
    subtitle: '三种算法与五档分级',
    icon: 'heart',
    category: '健康工具',
    order: 60,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);

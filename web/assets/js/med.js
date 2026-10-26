/* ==========================================================================
 * med.js —— MedDesk「用药提醒」模块（A8）
 *
 * 依赖（只读调用，不修改其文件）：
 *   A6：MD.util / MD.ui / MD.data / MD.api / MD.router / MD.crypto / MD.toast
 *       MD.haptic / MD.bus / MD.theme
 *   A7：MD.sched / MD.remind
 *
 * 分层约定（单向依赖）：
 *   数据层 loadData()  →  计算层 computeVM()  →  渲染层 renderXxx(el, vm)
 *   渲染函数之间严禁互相调用，统一由 refreshAll() 编排。
 *
 * 零外部依赖：无 CDN、无图表库；图标与统计图表均为手写内联 SVG。
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};
  var med = MD.med = MD.med || {};

  /* ============================ 0. 常量 ============================ */

  var LS_PROFILE = 'md.profileId';        // 记住上次选中的档案
  var LS_SAMPLE = 'md.sampleSeeded';      // 示例数据是否已播种过（清空后不再重播）
  var SNOOZE_OPTIONS = [10, 30, 60];      // 推迟分钟选项
  var HISTORY_DAYS = 90;                  // 历史默认展示天数
  var HISTORY_STEP = 90;                  // 「加载更早」步长
  var MISS_GRACE_MS = 60 * 60 * 1000;     // 计划时间超过 1 小时仍 pending → 视为漏服

  var STATUS_TEXT = { taken: '已服用', skipped: '已跳过', snoozed: '已推迟', pending: '待服用', missed: '漏服' };
  var STATUS_TONE = { taken: 'ok', skipped: 'warn', snoozed: 'warn', pending: '', missed: 'danger' };

  var RELATIONS = ['本人', '配偶', '子女', '父母', '其他'];
  var UNITS = ['片', '粒', 'ml', '滴', '袋', '支', 'g', 'mg', 'IU'];
  var FORMS = ['片剂', '胶囊', '颗粒', '口服液', '注射剂', '外用', '滴剂', '其他'];
  var FOODS = ['无特殊', '空腹', '餐前', '随餐', '餐后'];
  var CYCLE_UNITS = [{ id: 'day', label: '天' }, { id: 'week', label: '周' }, { id: 'month', label: '月' }];

  // A7 未就绪时的频次兜底（与契约 6.5 KINDS 完全一致）
  var FALLBACK_KINDS = [
    { id: 'daily', label: '每日' },
    { id: 'twice_daily', label: '每日两次' },
    { id: 'interval_hours', label: '按小时间隔' },
    { id: 'specific_dates', label: '指定日期' },
    { id: 'custom_interval', label: '自定义间隔' },
    { id: 'cycle', label: '周期性用药（21天服/7天停）' }
  ];

  /* ============================ 1. 内联 SVG 图标 ============================ */

  function svg(paths, opt) {
    opt = opt || {};
    var vb = opt.viewBox || '0 0 24 24';
    var extra = opt.fill ? 'fill="currentColor" stroke="none"' : 'fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"';
    return '<svg viewBox="' + vb + '" width="' + (opt.size || 20) + '" height="' + (opt.size || 20) +
      '" aria-hidden="true" ' + extra + '>' + paths + '</svg>';
  }

  var ICON = {
    check: svg('<path d="M4.5 12.5 9.5 17.5 19.5 6.5"/>'),
    clock: svg('<circle cx="12" cy="12" r="8"/><path d="M12 7.5V12l3.2 2"/>'),
    close: svg('<path d="M6 6l12 12M18 6 6 18"/>'),
    plus: svg('<path d="M12 5v14M5 12h14"/>'),
    minus: svg('<path d="M5 12h14"/>'),
    skip: svg('<path d="M6 12h9M13 8l4 4-4 4"/>'),
    trash: svg('<path d="M4.5 7h15M9.5 7V4.8h5V7M6.5 7l1 12.2h9l1-12.2"/><path d="M10.5 10.5v6M13.5 10.5v6"/>'),
    copy: svg('<rect x="8.5" y="8.5" width="11" height="11" rx="1.5"/><path d="M15.5 5.5v-1A1.5 1.5 0 0 0 14 3H5a1.5 1.5 0 0 0-1.5 1.5v9A1.5 1.5 0 0 0 5 15h.5"/>'),
    edit: svg('<path d="M4.5 19.5h4l10-10a2.1 2.1 0 0 0-3-3l-10 10v4Z"/><path d="M14 6.5 17.5 10"/>'),
    chevron: svg('<path d="M9 6l6 6-6 6"/>'),
    user: svg('<circle cx="12" cy="8.5" r="3.5"/><path d="M5 19.5c1.2-3.4 3.8-5 7-5s5.8 1.6 7 5"/>'),
    camera: svg('<path d="M4 8.5h3l1.5-2h7L17 8.5h3v10H4v-10Z"/><circle cx="12" cy="13.5" r="3"/>'),
    download: svg('<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5"/><path d="M5 19h14"/>'),
    alert: svg('<path d="M12 5 3.5 19h17L12 5Z"/><path d="M12 10v4M12 16.5v.6"/>')
  };

  /* ============================ 2. 样式注入（.md- 前缀） ============================ */

  var CSS = [
    '.md-wrap{display:block}',
    '.md-cols{display:grid;grid-template-columns:1fr;gap:var(--space,12px)}',
    '@media(min-width:1024px){.md-cols{grid-template-columns:minmax(0,1.75fr) minmax(300px,1fr);align-items:start}}',
    '.md-stack{display:flex;flex-direction:column;gap:var(--space,12px)}',
    '.md-section{margin-top:var(--space,12px)}',
    '.md-section__bar{display:flex;align-items:center;gap:8px;margin:0 0 8px}',
    '.md-section__bar .section-title{margin:0;flex:1 1 auto}',
    /* 今日 */
    '.md-today__head{display:flex;align-items:center;gap:12px;margin-bottom:10px}',
    '.md-ring{flex:0 0 auto;line-height:0}',
    '.md-today__sum{font-size:15px;font-weight:600}',
    '.md-dose{display:flex;gap:10px;align-items:flex-start;padding:10px;border:1px solid var(--border);border-radius:var(--radius,10px);background:var(--surface-2);transition:background 160ms cubic-bezier(.2,.8,.2,1),border-color 160ms}',
    '.md-dose+.md-dose{margin-top:8px}',
    '.md-dose--overdue{border-color:var(--danger);background:rgba(224,49,49,.10)}',
    '.md-dose__time{flex:0 0 52px;font-size:15px;font-weight:600;font-variant-numeric:tabular-nums}',
    '.md-dose__main{flex:1 1 auto;min-width:0}',
    '.md-dose__name{font-size:15px;font-weight:600;display:flex;flex-wrap:wrap;gap:6px;align-items:center}',
    '.md-dose__warn{margin-top:4px;color:var(--danger);font-size:13px;font-weight:600}',
    '.md-dose__acts{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px;width:100%}',
    '.md-dose__acts .btn{min-height:44px;min-width:44px;padding:0 14px}',
    '@media(min-width:600px){.md-dose__acts{width:auto;margin-top:0}}',
    /* 档案 chips */
    '.md-chips{display:flex;gap:8px;overflow-x:auto;padding:2px 0;scrollbar-width:thin}',
    '.md-chips .chip{display:inline-flex;align-items:center;gap:6px;min-height:40px;flex:0 0 auto}',
    '.md-dot{width:10px;height:10px;border-radius:50%;display:inline-block;background:var(--brand)}',
    /* 药物卡片 */
    '.md-med{display:flex;gap:10px;align-items:center;padding:10px;border:1px solid var(--border);border-radius:var(--radius,10px);background:var(--surface);cursor:pointer;transition:background 160ms,transform 160ms}',
    '.md-med:hover{background:var(--surface-2)}',
    '.md-med+.md-med{margin-top:8px}',
    '.md-med__thumb{flex:0 0 48px;width:48px;height:48px;border-radius:8px;object-fit:cover;background:var(--surface-2);border:1px solid var(--border)}',
    '.md-med__body{flex:1 1 auto;min-width:0}',
    '.md-med__name{font-size:15px;font-weight:600;display:flex;flex-wrap:wrap;gap:6px;align-items:center}',
    /* 历史 */
    '.md-day{margin-bottom:10px}',
    '.md-day__head{font-size:13px;font-weight:600;color:var(--text-dim);display:flex;gap:8px;align-items:center;margin-bottom:4px}',
    '.md-hist{display:flex;gap:8px;align-items:center;width:100%;text-align:left;padding:10px;border:1px solid var(--border);border-radius:var(--radius,10px);background:var(--surface);cursor:pointer;transition:background 160ms}',
    '.md-hist:hover{background:var(--surface-2)}',
    '.md-hist+.md-hist{margin-top:6px}',
    '.md-hist__time{flex:0 0 50px;font-variant-numeric:tabular-nums;font-weight:600}',
    '.md-hist__name{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    /* 统计 */
    '.md-stats__cards{display:grid;grid-template-columns:1fr 1fr;gap:8px}',
    '.md-kpi{padding:10px;border:1px solid var(--border);border-radius:var(--radius,10px);background:var(--surface-2)}',
    '.md-kpi__rate{font-size:22px;font-weight:700;line-height:1.1;font-variant-numeric:tabular-nums}',
    '.md-kpi__grid{display:grid;grid-template-columns:1fr 1fr;gap:2px 8px;margin-top:6px;font-size:12px;color:var(--text-dim)}',
    '.md-chart{width:100%;height:auto;display:block;margin-top:8px}',
    '.md-heat__head{display:flex;align-items:center;gap:8px;margin:12px 0 6px}',
    '.md-heat__head .section-title{margin:0;flex:1 1 auto}',
    '.md-heat__grid{display:grid;grid-template-columns:repeat(7,1fr);gap:4px}',
    '.md-heat__wd{font-size:11px;color:var(--text-dim);text-align:center}',
    '.md-heat__cell{aspect-ratio:1/1;border-radius:5px;border:1px solid var(--border);min-height:22px;display:flex;align-items:center;justify-content:center;font-size:10px;color:var(--text)}',
    '.md-legend{display:flex;align-items:center;gap:6px;font-size:11px;color:var(--text-dim);margin-top:6px}',
    '.md-legend i{width:14px;height:10px;border-radius:3px;display:inline-block}',
    /* 表单 */
    '.md-form{display:flex;flex-direction:column;gap:10px;max-height:64vh;overflow:auto;padding-right:2px}',
    '.md-form .input,.md-form .select,.md-form .textarea{font-size:16px;min-height:44px;width:100%}',
    '.md-form .textarea{min-height:72px}',
    '.md-kindpick{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:6px}',
    '@media(min-width:560px){.md-kindpick{grid-template-columns:repeat(3,minmax(0,1fr))}}',
    '.md-kindpick .chip{min-height:44px;justify-content:center;text-align:center}',
    '.md-adv{border:1px solid var(--border);border-radius:var(--radius,10px);padding:8px 10px;background:var(--surface-2)}',
    '.md-adv>summary{cursor:pointer;font-size:14px;font-weight:600;min-height:44px;display:flex;align-items:center}',
    '.md-adv__body{display:flex;flex-direction:column;gap:10px;margin-top:8px}',
    '.md-row2{display:grid;grid-template-columns:1fr 1fr;gap:8px}',
    '.md-switch{display:flex;align-items:center;justify-content:space-between;gap:10px;min-height:44px}',
    '.md-switch input{width:22px;height:22px}',
    '.md-photo{display:flex;gap:10px;align-items:center}',
    '.md-photo img{width:64px;height:64px;border-radius:8px;object-fit:cover;border:1px solid var(--border);background:var(--surface-2)}',
    '.md-photo input[type=file]{font-size:16px;min-height:44px;flex:1 1 auto}',
    '.md-hint{font-size:12px;color:var(--text-dim)}',
    '.md-seg{display:flex;gap:6px;flex-wrap:wrap}',
    /* 导出 */
    '.md-export__row{display:flex;gap:8px;align-items:center;margin-bottom:8px;flex-wrap:wrap}',
    '.md-export__row .select,.md-export__row .input{font-size:16px;min-height:44px;flex:1 1 140px}',
    '.md-export__btns{display:flex;gap:8px}',
    '.md-export__btns .btn{min-height:44px}',
    /* 兜底模态（仅在 MD.ui.sheet 不可用时使用） */
    '.md-modal{position:fixed;inset:0;z-index:60;display:flex;align-items:flex-end;justify-content:center;background:rgba(0,0,0,.45)}',
    '@media(min-width:768px){.md-modal{align-items:center}}',
    '.md-modal__panel{width:100%;max-width:560px;max-height:86vh;overflow:auto;background:var(--surface);border:1px solid var(--border);border-radius:14px 14px 0 0;padding:14px;box-shadow:var(--shadow)}',
    '@media(min-width:768px){.md-modal__panel{border-radius:14px}}',
    '.md-modal__title{font-size:16px;font-weight:600;margin:0 0 10px}',
    '.md-modal__acts{display:flex;gap:8px;justify-content:flex-end;margin-top:12px;flex-wrap:wrap}',
    '.md-modal__acts .btn{min-height:44px}'
  ].join('\n');

  function injectStyle() {
    if (typeof document === 'undefined' || document.getElementById('md-a8-style')) return;
    var st = document.createElement('style');
    st.id = 'md-a8-style';
    st.textContent = CSS;
    document.head.appendChild(st);
  }

  /* ============================ 3. 通用工具（优先使用 A6） ============================ */

  function esc(s) {
    if (MD.util && typeof MD.util.escapeHtml === 'function') return MD.util.escapeHtml(s == null ? '' : String(s));
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function nowMs() { return MD.util && MD.util.nowMs ? MD.util.nowMs() : Date.now(); }
  function uid() { return MD.util && MD.util.uuid ? MD.util.uuid() : 'id-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10); }
  function clamp(n, a, b) { n = Number(n) || 0; return n < a ? a : (n > b ? b : n); }
  function toast(msg, kind) { if (MD.toast && MD.toast.show) MD.toast.show(msg, kind || 'info'); }
  function tap() { if (MD.haptic && MD.haptic.tap) MD.haptic.tap(); }
  function ok() { if (MD.haptic && MD.haptic.success) MD.haptic.success(); }

  function fmtDate(ms) { return MD.util && MD.util.fmtDate ? MD.util.fmtDate(ms) : fbDate(ms); }
  function fmtTime(ms) { return MD.util && MD.util.fmtTime ? MD.util.fmtTime(ms) : fbTime(ms); }
  function fmtDateTime(ms) { return MD.util && MD.util.fmtDateTime ? MD.util.fmtDateTime(ms) : (fbDate(ms) + ' ' + fbTime(ms)); }
  function dayKey(ms) { return MD.util && MD.util.dayKey ? MD.util.dayKey(ms) : fbDayKey(ms); }
  function startOfDay(ms) {
    if (MD.util && MD.util.startOfDay) return MD.util.startOfDay(ms);
    var d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime();
  }
  function addDays(ms, n) {
    if (MD.util && MD.util.addDays) return MD.util.addDays(ms, n);
    return ms + n * 86400000;
  }
  function fbDate(ms) {
    var d = new Date(ms);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }
  function fbTime(ms) {
    var d = new Date(ms);
    return pad(d.getHours()) + ':' + pad(d.getMinutes());
  }
  function fbDayKey(ms) { return fbDate(ms); }
  function pad(n) { return (n < 10 ? '0' : '') + n; }
  function monthStartMs(key) { var y = +key.slice(0, 4), m = +key.slice(5, 7) - 1; var d = new Date(y, m, 1); d.setHours(0, 0, 0, 0); return d.getTime(); }
  function monthKeyOf(ms) { var d = new Date(ms); return d.getFullYear() + '-' + pad(d.getMonth() + 1); }
  function shiftMonth(key, delta) {
    var y = +key.slice(0, 4), m = +key.slice(5, 7) - 1 + delta;
    var d = new Date(y, m, 1);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1);
  }
  function daysInMonth(key) { var y = +key.slice(0, 4), m = +key.slice(5, 7); return new Date(y, m, 0).getDate(); }
  function atTime(dayMs, hhmm) {
    var p = String(hhmm || '08:00').split(':');
    return startOfDay(dayMs) + (parseInt(p[0], 10) || 0) * 3600000 + (parseInt(p[1], 10) || 0) * 60000;
  }

  /* ============================ 4. 模块状态 ============================ */

  var S = {
    root: null,          // 视图根元素
    el: {},              // 各分区容器
    data: null,          // 最近一次加载的数据快照
    doseMap: {},         // occKey -> DoseVM（供事件处理取上下文）
    profileId: 'all',    // 当前档案：'all' 或具体 id
    historyDays: HISTORY_DAYS,
    heatMonth: monthKeyOf(Date.now()),
    busy: false,
    queued: false,
    mounted: false,
    formPhotoKey: ''
  };

  function curProfileId() {
    if (S.profileId && S.profileId !== 'all') return S.profileId;
    var ps = (S.data && S.data.profiles) || [];
    return ps.length ? ps[0].id : '';
  }
  function isAll() { return !S.profileId || S.profileId === 'all'; }

  /* ============================ 5. 数据层 ============================ */

  function alive(list) {
    return (list || []).filter(function (r) { return r && !r.deleted; });
  }

  function loadData() {
    return Promise.all([
      MD.data.profiles.list(),
      MD.data.meds.list(),
      MD.data.schedules.list(),
      MD.data.intakes.list()
    ]).then(function (r) {
      return {
        profiles: alive(r[0]),
        meds: alive(r[1]),
        schedules: alive(r[2]),
        intakes: alive(r[3])
      };
    });
  }

  /** 首次打开且本地无任何用药记录时写入示例数据（含 1 条逾期） */
  function ensureSamples(d) {
    if (d.meds.length) return Promise.resolve(d);
    var seeded = false;
    try { seeded = global.localStorage.getItem(LS_SAMPLE) === '1'; } catch (e) { seeded = false; }
    if (seeded) return Promise.resolve(d);
    return seedSamples()
      .then(function () { try { global.localStorage.setItem(LS_SAMPLE, '1'); } catch (e) { /* 忽略 */ } })
      .then(loadData);
  }

  function seedSamples() {
    var now = nowMs();
    var today = startOfDay(now);
    var selfId = uid(), elderId = uid();

    var profiles = [
      { id: selfId, name: '本人', relation: '本人', birthDate: '1990-05-12', color: '#4f8cff', note: '示例档案', sample: true, ver: 1 },
      { id: elderId, name: '李爷爷', relation: '父母', birthDate: '1948-03-02', color: '#f2994a', note: '示例档案 · 需提醒', sample: true, ver: 1 }
    ];

    var m1 = uid(), m2 = uid(), m3 = uid(), m4 = uid();
    var meds = [
      { id: m1, profileId: selfId, name: '维生素 D3', dosageAmount: 1, dosageUnit: '粒', form: '胶囊', strength: '400IU',
        foodInstruction: '随餐', critical: false, active: true, stock: 60, refillThreshold: 20,
        startDate: dayKey(addDays(today, -3)), sample: true, ver: 1 },
      { id: m2, profileId: selfId, name: '钙尔奇 D', dosageAmount: 1, dosageUnit: '片', form: '片剂', strength: '600mg',
        foodInstruction: '餐后', critical: false, active: true, stock: 8, refillThreshold: 15,
        startDate: dayKey(addDays(today, -25)), sample: true, ver: 1 },
      { id: m3, profileId: elderId, name: '氨氯地平片', dosageAmount: 1, dosageUnit: '片', form: '片剂', strength: '5mg',
        foodInstruction: '空腹', prescriber: '王医生', critical: true, active: true, stock: 30, refillThreshold: 10,
        startDate: dayKey(addDays(today, -40)), sample: true, ver: 1 },
      { id: m4, profileId: elderId, name: '阿托伐他汀钙片', dosageAmount: 1, dosageUnit: '片', form: '片剂', strength: '20mg',
        foodInstruction: '餐后', critical: false, active: true, stock: 24, refillThreshold: 10,
        startDate: dayKey(addDays(today, -40)), sample: true, ver: 1 }
    ];

    var s1 = uid(), s2 = uid(), s3 = uid(), s4 = uid();
    var schedules = [
      { id: s1, medicationId: m1, kind: 'daily', times: ['08:00'], critical: false,
        startDate: dayKey(addDays(today, -3)), sample: true, ver: 1 },
      { id: s2, medicationId: m2, kind: 'cycle', times: ['12:00'], cycleOnDays: 21, cycleOffDays: 7,
        cycleStart: dayKey(addDays(today, -25)), critical: false, startDate: dayKey(addDays(today, -25)), sample: true, ver: 1 },
      { id: s3, medicationId: m3, kind: 'daily', times: ['07:30'], critical: true,
        startDate: dayKey(addDays(today, -40)), sample: true, ver: 1 },
      { id: s4, medicationId: m4, kind: 'twice_daily', times: ['09:00', '21:00'], critical: false,
        startDate: dayKey(addDays(today, -40)), sample: true, ver: 1 }
    ];

    // 服药记录：近 12 天的历史 + 1 条「昨天 08:00 未处理」的逾期项（由 A7 顺延到今天）
    var intakes = [];
    var doseOf = { m1: '1 粒', m2: '1 片', m3: '1 片', m4: '1 片' };
    var seeds = [
      { s: s1, m: m1, p: selfId, t: '08:00' },
      { s: s3, m: m3, p: elderId, t: '07:30' },
      { s: s4, m: m4, p: elderId, t: '09:00' },
      { s: s4, m: m4, p: elderId, t: '21:00' }
    ];
    for (var i = 1; i <= 12; i++) {
      var dayMs = addDays(today, -i);
      for (var k = 0; k < seeds.length; k++) {
        var sd = seeds[k];
        if (i === 1 && sd.s === s1) continue; // 昨天 08:00 留给下面的「未处理」示例
        var tm = atTime(dayMs, sd.t);
        // 历史里只保留「昨天维生素 D3」这一条未处理（逾期），其余按 7 天一次跳过
        var status = (i % 7 === 0 ? 'skipped' : 'taken');
        intakes.push({
          occKey: occKeyOf({ id: sd.s }, tm),
          medicationId: sd.m, profileId: sd.p, scheduledFor: tm,
          status: status,
          takenAt: status === 'taken' ? tm + 300000 : 0,
          doseText: doseOf[sd.m], sample: true, ver: 1
        });
      }
    }
    // 昨天的维生素 D3 未处理 → 逾期
    var yMs = atTime(addDays(today, -1), '08:00');
    intakes.push({
      occKey: occKeyOf({ id: s1 }, yMs), medicationId: m1, profileId: selfId,
      scheduledFor: yMs, status: 'pending', takenAt: 0, doseText: '1 粒', sample: true, ver: 1
    });
    // 今天早上的两次已服用，便于直观看到进度环
    [s3, s4].forEach(function (sid, idx) {
      var mm = sid === s3 ? m3 : m4;
      var pp = elderId;
      var tt = atTime(today, idx === 0 ? '07:30' : '09:00');
      intakes.push({
        occKey: occKeyOf({ id: sid }, tt), medicationId: mm, profileId: pp,
        scheduledFor: tt, status: 'taken', takenAt: tt + 240000, doseText: '1 片', sample: true, ver: 1
      });
    });

    // 统一打示例标记：sample（本模块识别）+ _example（与 A6 示例清理保持一致）
    [profiles, meds, schedules, intakes].forEach(function (arr) {
      arr.forEach(function (r) { r.sample = true; r._example = true; });
    });

    return Promise.all(profiles.map(save('profiles')))
      .then(function () { return Promise.all(meds.map(save('meds'))); })
      .then(function () { return Promise.all(schedules.map(save('schedules'))); })
      .then(function () { return Promise.all(intakes.map(save('intakes'))); });
  }

  function save(table) {
    return function (rec) { return MD.data[table].save(rec); };
  }

  function occKeyOf(schedule, timeMs) {
    if (MD.sched && typeof MD.sched.occKey === 'function') return MD.sched.occKey(schedule, timeMs);
    return schedule.id + '@' + timeMs;
  }

  /** 清空示例数据：只删除 sample:true 的记录 */
  function clearSamples() {
    var d = S.data || {};
    var ids = {
      profiles: (d.profiles || []).filter(isSample).map(byId),
      meds: (d.meds || []).filter(isSample).map(byId),
      schedules: (d.schedules || []).filter(isSample).map(byId),
      intakes: (d.intakes || []).filter(isSample).map(byId)
    };
    // 示例药物如有照片则一并清理
    (d.meds || []).filter(isSample).forEach(function (m) {
      ((m.photoKeys) || []).forEach(function (k) { if (MD.data.blobs && MD.data.blobs.remove) MD.data.blobs.remove(k); });
    });
    try { global.localStorage.setItem(LS_SAMPLE, '1'); } catch (e) { /* 忽略 */ }
    // 先清 A6 预置示例（若平台层提供），再清本模块示例
    var pre = (MD.data && typeof MD.data.clearExamples === 'function')
      ? Promise.resolve(MD.data.clearExamples()).catch(function () { return false; })
      : Promise.resolve(false);
    return pre.then(function () {
      return Promise.all([
        Promise.all(ids.profiles.map(del('profiles'))),
        Promise.all(ids.meds.map(del('meds'))),
        Promise.all(ids.schedules.map(del('schedules'))),
        Promise.all(ids.intakes.map(del('intakes')))
      ]);
    });
  }
  function isSample(r) { return r && (r.sample === true || r._example === true); }
  function byId(r) { return r.id; }
  function del(table) {
    return function (id) { return MD.data[table].remove(id); };
  }

  /** 按 occKey 幂等 upsert 服药记录（已存在则改状态并 ver++） */
  function applyIntake(occKey, status, extra) {
    var list = ((S.data && S.data.intakes) || []);
    var cur = null;
    for (var i = 0; i < list.length; i++) { if (list[i] && list[i].occKey === occKey) { cur = list[i]; break; } }
    var rec = {};
    if (cur) { for (var k in cur) if (Object.prototype.hasOwnProperty.call(cur, k)) rec[k] = cur[k]; }
    else { rec = { id: uid(), occKey: occKey }; }
    rec.status = status;
    rec.ver = (Number(rec.ver) || 0) + 1;
    rec.updatedAt = nowMs();
    if (extra) {
      for (var e in extra) if (Object.prototype.hasOwnProperty.call(extra, e)) rec[e] = extra[e];
    }
    rec.takenAt = (status === 'taken') ? (cur && cur.takenAt ? cur.takenAt : nowMs()) : 0;
    if (status === 'snoozed' && extra && extra.snoozeUntil) rec.snoozeUntil = extra.snoozeUntil;
    if (status !== 'snoozed') rec.snoozeUntil = 0;
    // 撤销为待服用 = 用户显式动作：打 undoneAt，统计不再把它自动归一为漏服
    rec.undoneAt = (status === 'pending') ? nowMs() : 0;
    return MD.data.intakes.save(rec);
  }

  /* ============================ 6. 计算层 ============================ */

  function computeVM(d) {
    var now = nowMs();
    var pid = isAll() ? null : S.profileId;
    var meds = isAll() ? d.meds : d.meds.filter(function (m) { return m.profileId === pid; });
    var medIds = {};
    meds.forEach(function (m) { medIds[m.id] = true; });
    var schedules = d.schedules.filter(function (s) { return medIds[s.medicationId]; });
    var intakes = isAll() ? d.intakes : d.intakes.filter(function (i) { return i.profileId === pid; });

    // 今日剂量（A7 负责排程与昨天顺延）
    var doses = [];
    if (MD.sched && typeof MD.sched.buildToday === 'function') {
      doses = MD.sched.buildToday({
        meds: meds, schedules: schedules, intakes: intakes, profileId: pid, nowMs: now
      }) || [];
    }
    doses = doses.slice().sort(orderDose);

    // 供事件处理按 occKey 反查上下文（只读缓存，不参与渲染）
    S.doseMap = {};
    var medById = {};
    meds.forEach(function (m) { medById[m.id] = m; });
    var profileNameOf = {};
    d.profiles.forEach(function (p) { profileNameOf[p.id] = p.name; });
    doses.forEach(function (x) {
      S.doseMap[x.occKey] = x;
      if (!x.profileName) x.profileName = profileNameOf[x.profileId] || '';
      if (!x.dosageText) x.dosageText = '';
      var _m = medById[x.medId];
      x.strengthText = (_m && _m.strength) || '';
    });

    var done = 0, overdueCount = 0;
    doses.forEach(function (x) {
      if (x.status === 'taken' || x.status === 'skipped') done++;
      if (x.overdue) overdueCount++;
    });
    var todayRate = doses.length ? done / doses.length : 0;

    // 药物视图模型
    var schedByMed = {};
    schedules.forEach(function (s) { if (!schedByMed[s.medicationId]) schedByMed[s.medicationId] = s; });
    var medVM = meds.map(function (m) {
      var sc = schedByMed[m.id];
      var need = m.stock != null && m.refillThreshold != null && Number(m.stock) <= Number(m.refillThreshold);
      return {
        med: m,
        strengthText: m.strength || '',
        schedText: sc ? schedTextShort(sc) : '未设置排程',
        needRefill: need,
        photoKey: (m.photoKeys && m.photoKeys[0]) || ''
      };
    }).sort(function (a, b) { return String(a.med.name).localeCompare(String(b.med.name), 'zh-Hans-CN'); });

    // 历史（近 N 天，按日分组，倒序）
    var fromMs = startOfDay(addDays(now, -(S.historyDays - 1)));
    var toMs = startOfDay(now) + 86400000;
    var histRaw = intakes.filter(function (i) { return i.scheduledFor >= fromMs && i.scheduledFor < toMs; });
    var medNameOf = {};
    d.meds.forEach(function (m) { medNameOf[m.id] = m.name; });
    var dayMap = {}, dayOrder = [];
    histRaw.forEach(function (i) {
      var key = dayKey(i.scheduledFor);
      if (!dayMap[key]) { dayMap[key] = []; dayOrder.push(key); }
      dayMap[key].push({
        id: i.id, occKey: i.occKey, timeMs: i.scheduledFor,
        name: medNameOf[i.medicationId] || '已删除药物',
        doseText: i.doseText || '',
        status: normalizeStatus(i),
        note: i.note || ''
      });
    });
    dayOrder.sort().reverse();
    var history = dayOrder.map(function (key) {
      return {
        key: key,
        label: fmtDate(new Date(key + 'T00:00:00').getTime()) + ' · ' + weekdayCn(new Date(key + 'T00:00:00').getDay()),
        items: dayMap[key].sort(function (a, b) { return a.timeMs - b.timeMs; })
      };
    });

    // 依从性统计
    var stats7 = adherenceOf(intakes, startOfDay(addDays(now, -6)), now);
    var stats30 = adherenceOf(intakes, startOfDay(addDays(now, -29)), now);
    var bars = [];
    for (var b = 29; b >= 0; b--) {
      var ds = startOfDay(addDays(now, -b));
      var de = ds + 86400000;
      var dayIntakes = intakes.filter(function (i) { return i.scheduledFor >= ds && i.scheduledFor < de; });
      // 今天：统计终点必须是「当前时刻」而非当天 24:00——否则 22:00 的未来计划
      // 会被 A7 漏服判定（due <= 区间终点-15min）误判为漏服，导致今日柱红色。
      var a = adherenceOf(dayIntakes, ds, b === 0 ? Math.min(now, de) : de);
      bars.push({ dayMs: ds, key: dayKey(ds), rate: a.rate, total: a.total, taken: a.taken, missed: a.missed, isToday: b === 0 });
    }

    // 日历热力图
    var mk = S.heatMonth;
    var heat = { month: mk, cells: [], lead: 0, count: daysInMonth(mk) };
    var first = new Date(monthStartMs(mk));
    heat.lead = (first.getDay() + 6) % 7; // 周一为首列
    var i2;
    for (i2 = 0; i2 < heat.lead; i2++) heat.cells.push(null);
    for (i2 = 1; i2 <= heat.count; i2++) {
      var dms = monthStartMs(mk) + (i2 - 1) * 86400000;
      var dIntakes = intakes.filter(function (x) { return x.scheduledFor >= dms && x.scheduledFor < dms + 86400000; });
      // 今天：终点用当前时刻（理由同 bars——未来计划不算漏服）
      var dEnd = (dms === startOfDay(now)) ? now : dms + 86400000;
      var aa = adherenceOf(dIntakes, dms, dEnd);
      heat.cells.push({ day: i2, dayMs: dms, rate: aa.rate, total: aa.total, taken: aa.taken, missed: aa.missed });
    }

    return {
      now: now, profiles: d.profiles, profileId: S.profileId,
      doses: doses, todayRate: todayRate, done: done, total: doses.length, overdueCount: overdueCount,
      meds: medVM, history: history, hasMoreHistory: true,
      stats7: stats7, stats30: stats30, bars: bars, heat: heat
    };
  }

  function orderDose(a, b) {
    var ad = isDone(a) ? 1 : 0, bd = isDone(b) ? 1 : 0;
    if (ad !== bd) return ad - bd;
    if (isDone(a) && !!a.overdue !== !!b.overdue) return (a.overdue ? -1 : 1);
    return (a.timeMs || 0) - (b.timeMs || 0);
  }
  function isDone(x) { return x.status === 'taken' || x.status === 'skipped'; }

  function adherenceOf(intakes, fromMs, toMs) {
    if (MD.sched && typeof MD.sched.adherence === 'function') {
      // A7 只统计 taken/skipped/missed；这里先把「已过计划时间仍 pending」归一为 missed，避免漏服不计入分母
      var now = nowMs();
      // undoneAt：用户显式「撤销为待服用」的时间戳。两点语义：
      //   a) 带 undoneAt 的 pending 完全不参与统计（A7 引擎无法识别 undoneAt，会把它判成漏服，故直接剔除）；
      //   b) 无 undoneAt 且已过宽限期的 pending 归一为 missed（漏服防线保留）。
      var list = [];
      intakes.forEach(function (i) {
        if (i.status === 'pending') {
          if (i.undoneAt) return; // 显式撤销：不计入分母，直到重新打卡
          if ((i.scheduledFor || 0) + MISS_GRACE_MS < now) {
            list.push(Object.assign({}, i, { status: 'missed' }));
            return;
          }
        }
        list.push(i);
      });
      var r = MD.sched.adherence({ intakes: list, fromMs: fromMs, toMs: toMs }) || {};
      return {
        total: r.total || 0, taken: r.taken || 0, skipped: r.skipped || 0,
        missed: r.missed || 0,
        // rate === null 表示区间内没有任何完结打卡（分母为 0）：
        // 必须保留 null 交给渲染层显示「空白/—」，强转 0 会被误画成红色 0%。
        rate: (r.rate === null || r.rate === undefined) ? null : r.rate,
        streakDays: r.streakDays || 0
      };
    }
    // A7 未就绪时退化为按记录统计（不重复实现排程算法）
    var t = 0, k = 0, s = 0, m = 0;
    intakes.forEach(function (i) {
      t++;
      var st = normalizeStatus(i);
      if (st === 'taken') k++; else if (st === 'skipped') s++; else if (st === 'missed') m++;
    });
    return { total: t, taken: k, skipped: s, missed: m, rate: t ? k / t : null, streakDays: 0 };
  }

  /** 归一化状态：计划时间已过 1 小时仍 pending → 漏服 */
  function normalizeStatus(i) {
    var st = i.status || 'pending';
    if (st === 'pending' && !i.undoneAt && (i.scheduledFor || 0) + MISS_GRACE_MS < nowMs()) return 'missed';
    return st;
  }

  function weekdayCn(d) { return ['周日', '周一', '周二', '周三', '周四', '周五', '周六'][d] || ''; }

  /* ============================ 7. 渲染层（互不调用） ============================ */

  function mount(root) {
    injectStyle();
    root.innerHTML = [
      '<div class="md-wrap">',
      '  <section class="card md-stack" id="md-today"></section>',
      '  <div class="md-cols" style="margin-top:var(--space,12px)">',
      '    <div class="md-stack">',
      '      <section id="md-profiles"></section>',
      '      <section id="md-meds"></section>',
      '      <section id="md-history"></section>',
      '    </div>',
      '    <div class="md-stack">',
      '      <section id="md-stats"></section>',
      '      <section id="md-export"></section>',
      '    </div>',
      '  </div>',
      '</div>'
    ].join('');
    S.el = {
      today: root.querySelector('#md-today'),
      profiles: root.querySelector('#md-profiles'),
      meds: root.querySelector('#md-meds'),
      history: root.querySelector('#md-history'),
      stats: root.querySelector('#md-stats'),
      'export': root.querySelector('#md-export')
    };
    S.mounted = true;
  }

  function setHtml(node, html) {
    if (!node) return;
    if (MD.ui && typeof MD.ui.html === 'function') MD.ui.html(node, html);
    else node.innerHTML = html;
  }

  /* ---- ① 今天要处理 ---- */
  function renderToday(el, vm) {
    if (!el) return;
    var r = clamp(vm.todayRate, 0, 1);
    var C = 2 * Math.PI * 18;
    var ring = '<svg class="md-ring" viewBox="0 0 44 44" width="52" height="52" aria-hidden="true">' +
      '<circle cx="22" cy="22" r="18" fill="none" stroke="var(--border)" stroke-width="5"/>' +
      '<circle cx="22" cy="22" r="18" fill="none" stroke="' + (r >= 0.8 ? 'var(--ok)' : (r >= 0.5 ? 'var(--warn)' : 'var(--danger)')) + '"' +
      ' stroke-width="5" stroke-linecap="round" stroke-dasharray="' + (C * r).toFixed(1) + ' ' + C.toFixed(1) + '" transform="rotate(-90 22 22)"/>' +
      '<text x="22" y="26" text-anchor="middle" font-size="12" font-weight="700" fill="var(--text)">' + Math.round(r * 100) + '%</text>' +
      '</svg>';

    var head = '<div class="md-today__head">' + ring +
      '<div><div class="md-today__sum">今天要处理</div>' +
      '<div class="muted" style="font-size:13px">已完成 ' + vm.done + '/' + vm.total +
      (vm.overdueCount ? ' · <span style="color:var(--danger);font-weight:600">逾期 ' + vm.overdueCount + ' 项</span>' : '') +
      '</div></div></div>';

    var body;
    if (!vm.doses.length) {
      body = '<div class="empty">今天没有需要处理的用药</div>';
    } else {
      body = '<div>' + vm.doses.map(doseCard).join('') + '</div>';
    }

    var foot = '<div class="md-export__row" style="margin-top:10px">' +
      '<button class="btn btn-primary" data-act="med-add" type="button">' + ICON.plus + '添加药物</button>' +
      '<button class="btn btn-ghost" data-act="sample-clear" type="button">' + ICON.trash + '清空示例数据</button>' +
      '</div>';

    setHtml(el, head + body + foot);
  }

  function doseCard(x) {
    var over = !!x.overdue;
    var st = x.status || 'pending';
    var done = isDone(x);
    var txt = STATUS_TEXT[st] || st;
    var tone = STATUS_TONE[st] || '';
    var overdueLabel = '';
    if (over) {
      var mins = Math.max(1, Math.round((vmNow() - x.timeMs) / 60000));
      overdueLabel = mins >= 60
        ? '昨天未处理 · 逾期 ' + Math.floor(mins / 60) + ' 小时'
        : '逾期 ' + mins + ' 分钟';
    }
    var acts = done
      ? '<div class="md-dose__acts"><button class="btn btn-ghost" type="button" data-act="undo" data-occ="' + esc(x.occKey) + '">撤销</button></div>'
      : '<div class="md-dose__acts">' +
        '<button class="btn btn-primary" type="button" data-act="take" data-occ="' + esc(x.occKey) + '">' + ICON.check + '已服用</button>' +
        '<button class="btn btn-ghost" type="button" data-act="skip" data-occ="' + esc(x.occKey) + '">' + ICON.skip + '跳过</button>' +
        '<button class="btn btn-ghost" type="button" data-act="snooze" data-occ="' + esc(x.occKey) + '">' + ICON.clock + '推迟</button>' +
        '</div>';

    return '<div class="md-dose' + (over ? ' md-dose--overdue' : '') + '">' +
      '<div class="md-dose__time">' + esc(fmtTime(x.timeMs)) + '</div>' +
      '<div class="md-dose__main">' +
      '<div class="md-dose__name">' + esc(x.medName || '药物') +
      (x.critical ? '<span class="badge badge--danger">关键</span>' : '') +
      (st !== 'pending' ? '<span class="badge' + (tone ? ' badge--' + tone : '') + '">' + esc(txt) + '</span>' : '') +
      '</div>' +
      '<div class="muted" style="font-size:13px">' + [[x.dosageText, x.strengthText].filter(Boolean).join(' '), x.profileName].filter(Boolean).map(esc).join(' · ') + '</div>' +
      (overdueLabel ? '<div class="md-dose__warn">' + ICON.alert + ' ' + esc(overdueLabel) + '</div>' : '') +
      '</div>' + acts + '</div>';
  }
  function vmNow() { return (S.vm && S.vm.now) || nowMs(); }

  /* ---- ② 家庭档案切换器 ---- */
  function renderProfiles(el, vm) {
    if (!el) return;
    var chips = '<button class="chip' + (isAll() ? ' chip--active' : '') + '" type="button" data-act="profile" data-id="all">全部</button>';
    chips += vm.profiles.map(function (p) {
      var active = p.id === S.profileId;
      return '<button class="chip' + (active ? ' chip--active' : '') + '" type="button" data-act="profile" data-id="' + esc(p.id) + '">' +
        '<span class="md-dot" style="background:' + esc(p.color || 'var(--brand)') + '"></span>' + esc(p.name) +
        '<span class="muted" style="font-size:12px">' + esc(p.relation || '') + '</span>' +
        (active ? '<span data-act="profile-edit" data-id="' + esc(p.id) + '" style="display:inline-flex">' + ICON.edit + '</span>' : '') +
        '</button>';
    }).join('');
    chips += '<button class="chip" type="button" data-act="profile-new">' + ICON.plus + '新建档案</button>';
    setHtml(el, '<div class="section-title">家庭档案</div><div class="md-chips">' + chips + '</div>');
  }

  /* ---- ③ 药物列表 ---- */
  function renderMeds(el, vm) {
    if (!el) return;
    var head = '<div class="md-section__bar"><div class="section-title">药物清单（' + vm.meds.length + '）</div></div>';
    var body;
    if (!vm.meds.length) {
      body = '<div class="empty">还没有药物，点击「添加药物」开始</div>';
    } else {
      body = vm.meds.map(function (m) {
        return '<div class="md-med" data-act="med-open" data-id="' + esc(m.med.id) + '">' +
          '<img class="md-med__thumb" alt="" data-thumb="' + esc(m.photoKey) + '" src="">' +
          '<div class="md-med__body">' +
          '<div class="md-med__name">' + esc(m.med.name) +
          (m.needRefill ? '<span class="badge badge--danger">需补药</span>' : '') +
          (m.med.critical ? '<span class="badge badge--warn">关键</span>' : '') +
          '</div>' +
          '<div class="muted" style="font-size:13px">' + esc([doseTextOf(m.med), m.strengthText].filter(Boolean).join(' ')) + ' · ' + esc(m.schedText) + '</div>' +
          (m.med.stock != null ? '<div class="muted" style="font-size:12px">库存 ' + esc(m.med.stock) + ' ' + esc(m.med.dosageUnit || '') +
            (m.med.refillThreshold != null ? ' · 阈值 ' + esc(m.med.refillThreshold) : '') + '</div>' : '') +
          '</div>' +
          '<div class="md-med__acts" style="display:flex;align-items:center;gap:4px">' +
          '<button class="btn-icon" type="button" data-act="med-copy" data-id="' + esc(m.med.id) + '" aria-label="复制">' + ICON.copy + '</button>' +
          '<button class="btn-icon" type="button" data-act="med-del" data-id="' + esc(m.med.id) + '" aria-label="删除">' + ICON.trash + '</button>' +
          '</div>' +
          '</div>';
      }).join('');
    }
    setHtml(el, head + body);
    fillThumbs(el);
  }

  /** 排程描述（展示精简版）：去掉「（YYYY-MM-DD 起）」开始时间与「自定义间隔」前缀 */
  function schedTextShort(sc) {
    var s = (MD.sched && MD.sched.describe ? MD.sched.describe(sc) : '') || '';
    // 去掉起始日期，保留「…止」结束日期（若有）
    s = s.replace(/（([^）]*)）/g, function (whole, inner) {
      var parts = inner.split('，').filter(function (p) { return !/\d{4}-\d{2}-\d{2}\s*起$/.test(p.trim()); });
      return parts.length ? '（' + parts.join('，') + '）' : '';
    });
    // cycle 描述去重：kindLabel 已含「（21天服/7天停）」，tail 的「服 N 天停 M 天」信息重复
    s = s.replace('周期性用药（21天服/7天停） 服', '周期性用药 服');
    return s.replace(/^自定义间隔\s*/, '').replace(/\s+/g, ' ').trim();
  }

  function doseTextOf(m) {
    if (m.dosageAmount == null || m.dosageAmount === '') return '';
    return m.dosageAmount + ' ' + (m.dosageUnit || '');
  }

  /** 异步填充照片缩略图（仅补 src，不触发其它渲染） */
  function fillThumbs(el) {
    if (!el || !MD.data.blobs || !MD.data.blobs.get) return;
    var imgs = el.querySelectorAll('img[data-thumb]');
    Array.prototype.forEach.call(imgs, function (img) {
      var key = img.getAttribute('data-thumb');
      if (!key) { img.style.display = 'none'; return; }
      Promise.resolve(MD.data.blobs.get(key)).then(function (v) {
        if (!v) { img.style.display = 'none'; return; }
        img.src = (typeof v === 'string') ? v : URL.createObjectURL(v);
      }).catch(function () { img.style.display = 'none'; });
    });
  }

  /* ---- ⑤ 用药历史 ---- */
  function renderHistory(el, vm) {
    if (!el) return;
    var head = '<div class="md-section__bar"><div class="section-title">用药历史（近 ' + S.historyDays + ' 天）</div></div>';
    var body;
    if (!vm.history.length) {
      body = '<div class="empty">暂无用药记录</div>';
    } else {
      body = vm.history.map(function (day) {
        return '<div class="md-day"><div class="md-day__head">' + esc(day.label) + '<span class="muted">' + day.items.length + ' 条</span></div>' +
          day.items.map(function (it) {
            var st = it.status;
            var tone = STATUS_TONE[st] || '';
            return '<div class="md-hist" data-act="hist-open" data-id="' + esc(it.id) + '" role="button" tabindex="0">' +
              '<span class="md-hist__time">' + esc(fmtTime(it.timeMs)) + '</span>' +
              '<span class="md-hist__name">' + esc(it.name) + '</span>' +
              '<span class="muted" style="font-size:12px">' + esc(it.doseText) + '</span>' +
              '<span class="badge' + (tone ? ' badge--' + tone : '') + '">' + esc(STATUS_TEXT[st] || st) + '</span>' +
              '</div>';
          }).join('') + '</div>';
      }).join('');
    }
    var foot = '<button class="btn btn-ghost" type="button" data-act="hist-more" style="min-height:44px;margin-top:4px">' + ICON.chevron + '加载更早</button>';
    setHtml(el, head + body + foot);
  }

  /* ---- ⑥ 依从性统计（内联 SVG 柱状图 + 热力图） ---- */
  function renderStats(el, vm) {
    if (!el) return;
    var html = '<div class="card">' +
      '<div class="section-title">依从性统计</div>' +
      '<div class="md-stats__cards">' + kpiCard('近 7 天', vm.stats7) + kpiCard('近 30 天', vm.stats30) + '</div>' +
      barChart(vm.bars) +
      heatmap(vm.heat) +
      '</div>';
    setHtml(el, html);
  }

  function kpiCard(title, a) {
    // rate === null：区间内无完结打卡（分母为 0）→ 显示「—」中性色，不判红。
    var noData = (a.rate === null || a.rate === undefined);
    var pct = noData ? null : Math.round(a.rate * 100);
    var color = noData ? 'var(--text-dim, #888)'
      : (a.rate >= 0.8 ? 'var(--ok)' : (a.rate >= 0.5 ? 'var(--warn)' : 'var(--danger)'));
    return '<div class="md-kpi"><div class="muted" style="font-size:12px">' + esc(title) + '</div>' +
      '<div class="md-kpi__rate" style="color:' + color + '">' + (noData ? '—' : pct + '%') + '</div>' +
      '<div class="md-kpi__grid">' +
      '<span>总次数 ' + a.total + '</span><span>已服 ' + a.taken + '</span>' +
      '<span>跳过 ' + a.skipped + '</span><span>漏服 ' + a.missed + '</span>' +
      '</div>' +
      '<div class="muted" style="font-size:12px;margin-top:4px">连续达标 ' + (a.streakDays || 0) + ' 天</div>' +
      '</div>';
  }

  /** 近 30 天柱状图（手写 SVG，无图表库） */
  function barChart(bars) {
    var W = 300, H = 84, padB = 16, n = bars.length || 1;
    var bw = W / n, gap = Math.min(2, bw * 0.22);
    var parts = [];
    bars.forEach(function (b, i) {
      var noData = (b.rate === null || b.rate === undefined) || b.total === 0; // 无完结打卡 → 空白占位
      var h = noData ? 1.5 : Math.max(1.5, Math.round(b.rate * (H - padB - 4)));
      var x = i * bw + gap / 2, w = Math.max(1, bw - gap);
      var y = H - padB - h;
      var fill = noData ? 'var(--border)' : (b.rate >= 0.8 ? 'var(--ok)' : (b.rate >= 0.5 ? 'var(--warn)' : 'var(--danger)'));
      parts.push('<rect x="' + x.toFixed(2) + '" y="' + y + '" width="' + w.toFixed(2) + '" height="' + h + '" rx="1.5" fill="' + fill + '"/>');
    });
    var ticks = [0, 9, 19, 29].map(function (i) {
      var b = bars[i];
      return '<text x="' + ((i + 0.5) * bw).toFixed(1) + '" y="' + (H - 4) + '" font-size="8" fill="var(--text-dim)" text-anchor="middle">' +
        (b ? b.key.slice(5).replace('-', '/') : '') + '</text>';
    }).join('');
    return '<div class="section-title" style="margin-top:12px;font-size:13px">近 30 天每日依从率</div>' +
      '<svg class="md-chart" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img" aria-label="近30天依从率柱状图">' +
      '<line x1="0" y1="' + (H - padB) + '" x2="' + W + '" y2="' + (H - padB) + '" stroke="var(--border)" stroke-width="1"/>' +
      parts.join('') + ticks + '</svg>';
  }

  /** 日历热力图（可切换月份，颜色深浅=当日依从率） */
  function heatmap(heat) {
    var wd = ['一', '二', '三', '四', '五', '六', '日'].map(function (w) {
      return '<div class="md-heat__wd">' + w + '</div>';
    }).join('');
    var cells = heat.cells.map(function (c) {
      if (!c) return '<div class="md-heat__cell" style="opacity:.25"></div>';
      var noData = c.total === 0 || c.rate === null || c.rate === undefined;
      var bg = noData ? 'transparent' : heatColor(c.rate, c.missed, c.total);
      var tip = noData ? '无完结记录' : '依从率 ' + Math.round(c.rate * 100) + '%';
      return '<div class="md-heat__cell" style="background:' + bg + '" title="' + heat.month + '-' + c.day + ' ' + tip + '">' + c.day + '</div>';
    }).join('');
    return '<div class="md-heat__head">' +
      '<button class="btn-icon" type="button" data-act="heat-prev" aria-label="上一月">' + ICON.minus + '</button>' +
      '<div class="section-title" style="font-size:14px">' + esc(heat.month.replace('-', ' 年 ')) + ' 月</div>' +
      '<button class="btn-icon" type="button" data-act="heat-next" aria-label="下一月">' + ICON.plus + '</button>' +
      '</div>' +
      '<div class="md-heat__grid">' + wd + cells + '</div>' +
      '<div class="md-legend"><span>差</span>' +
      '<i style="background:hsl(4 70% 58%)"></i><i style="background:hsl(48 70% 52%)"></i><i style="background:hsl(144 70% 44%)"></i>' +
      '<span>好</span><span class="spacer"></span><span class="muted">空白=无计划</span></div>';
  }

  function heatColor(rate, missed, total) {
    if (!total || rate === null || rate === undefined) return 'transparent';
    var r = clamp(rate, 0, 1);
    var badRatio = clamp((missed || 0) / total, 0, 1);
    var hue = 4 + r * 140 - badRatio * 20;
    var light = 60 - r * 16;
    return 'hsl(' + clamp(hue, 0, 160).toFixed(0) + ' 70% ' + clamp(light, 34, 66).toFixed(0) + '%)';
  }

  /* ---- ⑦ 数据导出 ---- */
  function renderExport(el, vm) {
    if (!el) return;
    var opts = '<option value="month">本月</option><option value="30">近 30 天</option>' +
      '<option value="90">近 90 天</option><option value="custom">自定义</option>';
    var pops = '<option value="all">全部档案</option>' + vm.profiles.map(function (p) {
      return '<option value="' + esc(p.id) + '">' + esc(p.name) + '</option>';
    }).join('');
    setHtml(el, '<div class="card">' +
      '<div class="section-title">数据导出</div>' +
      '<div class="md-export__row"><label class="label" for="md-exp-range">范围</label>' +
      '<select class="select" id="md-exp-range">' + opts + '</select></div>' +
      '<div class="md-export__row" id="md-exp-custom" hidden>' +
      '<input class="input" type="date" id="md-exp-from" aria-label="开始日期">' +
      '<input class="input" type="date" id="md-exp-to" aria-label="结束日期"></div>' +
      '<div class="md-export__row"><label class="label" for="md-exp-profile">档案</label>' +
      '<select class="select" id="md-exp-profile">' + pops + '</select></div>' +
      '<div class="md-export__btns">' +
      '<button class="btn btn-primary" type="button" data-act="exp-csv">' + ICON.download + '导出 CSV</button>' +
      '<button class="btn btn-ghost" type="button" data-act="exp-pdf">' + ICON.download + '导出 PDF</button>' +
      '</div>' +
      '<div class="md-hint" style="margin-top:8px">导出的明文数据仅在本机生成，不会写入服务端日志。</div>' +
      '</div>');
  }

  function renderAll(vm) {
    S.vm = vm;
    renderToday(S.el.today, vm);
    renderProfiles(S.el.profiles, vm);
    renderMeds(S.el.meds, vm);
    renderHistory(S.el.history, vm);
    renderStats(S.el.stats, vm);
    renderExport(S.el['export'], vm);
  }

  /* ============================ 8. 统一刷新入口 ============================ */

  function refreshAll() {
    if (!S.root || !S.mounted || !MD.data) return Promise.resolve();
    if (S.busy) { S.queued = true; return Promise.resolve(); }
    S.busy = true;
    return loadData()
      .then(ensureSamples)
      .then(function (d) {
        S.data = d;
        var vm = computeVM(d);
        renderAll(vm);
        pushReminders(vm);
      })
      .catch(function (err) {
        if (global.console) console.error('[med] refreshAll 失败', err);
      })
      .then(function () {
        S.busy = false;
        if (S.queued) { S.queued = false; return refreshAll(); }
      });
  }

  /** 把今日待提醒列表交给 A7 */
  function pushReminders(vm) {
    if (!MD.remind || typeof MD.remind.schedule !== 'function') return;
    try {
      var pending = vm.doses.filter(function (x) { return !isDone(x); });
      MD.remind.schedule(pending);
    } catch (e) { /* 提醒不可用不影响视图 */ }
  }

  /* ============================ 9. 表单与抽屉 ============================ */

  function confirmBox(opt) {
    if (MD.ui && typeof MD.ui.confirm === 'function') return MD.ui.confirm(opt);
    return Promise.resolve(global.confirm((opt && opt.message) || '确认操作？'));
  }

  /** 打开抽屉；MD.ui.sheet 不可用时降级为自带模态 */
  function openSheet(opt) {
    if (MD.ui && typeof MD.ui.sheet === 'function') {
      try { MD.ui.sheet(opt); return; } catch (e) { /* 继续降级 */ }
    }
    var host = document.getElementById('modal-host') || document.body;
    var wrap = document.createElement('div');
    wrap.className = 'md-modal';
    var bodyEl = document.createElement('div');
    if (typeof opt.body === 'function') opt.body(bodyEl); else if (opt.body) bodyEl.appendChild(opt.body);
    var acts = (opt.actions || []).map(function (a) {
      var b = document.createElement('button');
      b.className = 'btn ' + (a.kind === 'danger' ? 'btn-danger' : (a.kind === 'primary' ? 'btn-primary' : 'btn-ghost'));
      b.type = 'button';
      b.textContent = a.label || '';
      b.onclick = function () { if (a.onClick) a.onClick(); close(); };
      return b;
    });
    var title = document.createElement('div');
    title.className = 'md-modal__title';
    title.textContent = opt.title || '';
    var panel = document.createElement('div');
    panel.className = 'md-modal__panel';
    panel.appendChild(title);
    panel.appendChild(bodyEl);
    var actWrap = document.createElement('div');
    actWrap.className = 'md-modal__acts';
    acts.forEach(function (b) { actWrap.appendChild(b); });
    var cancel = document.createElement('button');
    cancel.className = 'btn btn-ghost';
    cancel.type = 'button';
    cancel.textContent = '取消';
    cancel.onclick = function () { close(); };
    actWrap.appendChild(cancel);
    panel.appendChild(actWrap);
    wrap.appendChild(panel);
    // 降级弹窗同样不响应遮罩点击关闭（与 MD.ui.sheet 行为一致）
    function close() { if (wrap.parentNode) wrap.parentNode.removeChild(wrap); }
    if (host.hidden !== undefined) host.hidden = false;
    host.appendChild(wrap);
    return { close: close };
  }

  function closeSheet() { if (MD.ui && typeof MD.ui.closeSheet === 'function') MD.ui.closeSheet(); }

  function fieldRow(label, inner) {
    return '<div class="field"><span class="label">' + esc(label) + '</span>' + inner + '</div>';
  }
  function inputText(id, value, ph, type) {
    return '<input class="input" id="' + id + '" type="' + (type || 'text') + '" value="' + esc(value == null ? '' : value) + '" placeholder="' + esc(ph || '') + '">';
  }
  function selectBox(id, options, value) {
    return '<select class="select" id="' + id + '">' + options.map(function (o) {
      var v = typeof o === 'string' ? o : o.id, t = typeof o === 'string' ? o : o.label;
      return '<option value="' + esc(v) + '"' + (String(v) === String(value) ? ' selected' : '') + '>' + esc(t) + '</option>';
    }).join('') + '</select>';
  }

  /* ---- 档案表单 ---- */
  function openProfileForm(profileId) {
    var d = S.data || { profiles: [] };
    var p = null;
    d.profiles.forEach(function (x) { if (x.id === profileId) p = x; });
    var body = document.createElement('div');
    body.className = 'md-form';
    body.innerHTML =
      fieldRow('姓名', inputText('md-p-name', p ? p.name : '', '例如：本人')) +
      fieldRow('关系', selectBox('md-p-rel', RELATIONS, p ? p.relation : '本人')) +
      fieldRow('出生日期', inputText('md-p-birth', p ? p.birthDate : '', '', 'date')) +
      fieldRow('主题色', '<input class="input" id="md-p-color" type="color" value="' + esc(p && p.color ? p.color : '#4f8cff') + '" style="min-height:44px;padding:2px">') +
      fieldRow('备注', '<textarea class="textarea" id="md-p-note" placeholder="过敏史、注意事项等">' + esc(p ? p.note : '') + '</textarea>');

    openSheet({
      title: p ? '编辑档案' : '新建档案',
      body: body,
      actions: [{
        label: '保存', kind: 'primary', onClick: function () {
          var name = (body.querySelector('#md-p-name').value || '').trim();
          if (!name) { toast('请填写姓名', 'warn'); return false; }
          var rec = {
            id: p ? p.id : uid(),
            name: name,
            relation: body.querySelector('#md-p-rel').value,
            birthDate: body.querySelector('#md-p-birth').value,
            color: body.querySelector('#md-p-color').value,
            note: body.querySelector('#md-p-note').value,
            archived: p ? !!p.archived : false,
            ver: (Number(p && p.ver) || 0) + 1
          };
          MD.data.profiles.save(rec).then(function () {
            ok(); toast('档案已保存', 'success');
            closeSheet();
            if (!p) S.profileId = rec.id;
            refreshAll();
          }).catch(function (e) { toast('保存失败：' + msgOf(e), 'error'); });
        }
      }]
    });
  }

  /* ---- 药物表单 ---- */
  function openMedForm(medId) {
    var d = S.data || { meds: [], schedules: [], profiles: [] };
    var m = null, sc = null;
    d.meds.forEach(function (x) { if (x.id === medId) m = x; });
    if (m) d.schedules.forEach(function (s) { if (s.medicationId === m.id) sc = s; });

    S.formPhotoKey = (m && m.photoKeys && m.photoKeys[0]) || '';
    var kind = (sc && sc.kind) || 'daily';

    var body = document.createElement('div');
    body.className = 'md-form';
    body.innerHTML =
      fieldRow('药名', inputText('md-f-name', m ? m.name : '', '例如：维生素 D3')) +
      fieldRow('剂量（数量）', inputText('md-f-amount', m ? m.dosageAmount : 1, '例如：1', 'number')) +
      '<div class="field"><span class="label">频次</span><div class="md-kindpick" id="md-f-kindpick">' +
      kinds().map(function (k) {
        return '<button class="chip' + (k.id === kind ? ' chip--active' : '') + '" type="button" data-kind="' + esc(k.id) + '">' + esc(k.label) + '</button>';
      }).join('') + '</div></div>' +
      '<div id="md-f-kind"></div>' +
      '<details class="md-adv"><summary>高级选项（剂型、库存、提醒等）</summary><div class="md-adv__body">' +
      '<div class="md-row2">' +
      fieldRow('剂量单位', selectBox('md-f-unit', UNITS, m ? m.dosageUnit : '片')) +
      fieldRow('剂型', selectBox('md-f-form', FORMS, m ? m.form : '片剂')) +
      '</div>' +
      '<div class="md-row2">' +
      fieldRow('规格', inputText('md-f-strength', m ? m.strength : '', '例如：400IU')) +
      fieldRow('饮食说明', selectBox('md-f-food', FOODS, m ? m.foodInstruction : '无特殊')) +
      '</div>' +
      '<div class="md-row2">' +
      fieldRow('处方医生', inputText('md-f-prescriber', m ? m.prescriber : '', '')) +
      fieldRow('药房', inputText('md-f-pharmacy', m ? m.pharmacy : '', '')) +
      '</div>' +
      fieldRow('副作用', '<textarea class="textarea" id="md-f-side" placeholder="例如：偶有胃部不适">' + esc(m ? m.sideEffects : '') + '</textarea>') +
      fieldRow('备注', '<textarea class="textarea" id="md-f-notes" placeholder="补充说明">' + esc(m ? m.notes : '') + '</textarea>') +
      '<div class="md-row2">' +
      fieldRow('库存', inputText('md-f-stock', m && m.stock != null ? m.stock : '', '', 'number')) +
      fieldRow('补药阈值', inputText('md-f-threshold', m && m.refillThreshold != null ? m.refillThreshold : '', '', 'number')) +
      '</div>' +
      '<div class="md-row2">' +
      fieldRow('开始日期', inputText('md-f-start', m ? m.startDate : dayKey(nowMs()), '', 'date')) +
      fieldRow('结束日期', inputText('md-f-end', m ? m.endDate : '', '', 'date')) +
      '</div>' +
      fieldRow('所属档案', selectBox('md-f-profile', d.profiles.map(function (p) {
        return { id: p.id, label: p.name };
      }), m ? m.profileId : curProfileId())) +
      '<div class="md-switch"><span>关键提醒（重复提醒并尝试穿透勿扰）</span>' +
      '<input type="checkbox" id="md-f-critical"' + (m && m.critical ? ' checked' : '') + '></div>' +
      '<div class="field"><span class="label">药物照片</span><div class="md-photo">' +
      '<img id="md-f-preview" alt="" src="" hidden>' +
      '<input type="file" id="md-f-photo" accept="image/*" capture="environment">' +
      '</div><div class="md-hint">最长边压缩到 1280px、JPEG 0.82 后加密保存；未登录仅存本地。</div></div>' +
      '</div></details>';

    openSheet({
      title: m ? '编辑药物' : '添加药物',
      body: body,
      actions: [{
        // 返回 false 时 A6 不关闭抽屉（校验未通过需保留已填内容）
        label: '保存', kind: 'primary', onClick: function () { return saveMedForm(body, m, sc); }
      }]
    });

    // 频次切换 → 重绘对应字段（仅重绘表单内区域）
    var pick = body.querySelector('#md-f-kindpick');
    pick.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-kind]') : null;
      if (!btn) return;
      tap();
      Array.prototype.forEach.call(pick.querySelectorAll('.chip'), function (c) { c.classList.remove('chip--active'); });
      btn.classList.add('chip--active');
      renderKindFields(body, btn.getAttribute('data-kind'), sc);
    });
    renderKindFields(body, kind, sc);

    // 照片选择 → 压缩预览
    var fileInput = body.querySelector('#md-f-photo');
    fileInput.addEventListener('change', function () {
      var f = fileInput.files && fileInput.files[0];
      if (!f) return;
      compressImage(f).then(function (out) {
        S.pendingPhoto = out;
        var pv = body.querySelector('#md-f-preview');
        pv.src = out.dataURL;
        pv.hidden = false;
      }).catch(function () { toast('图片处理失败', 'error'); });
    });

    if (S.formPhotoKey && MD.data.blobs && MD.data.blobs.get) {
      Promise.resolve(MD.data.blobs.get(S.formPhotoKey)).then(function (v) {
        if (!v) return;
        var pv = body.querySelector('#md-f-preview');
        if (!pv) return;
        pv.src = (typeof v === 'string') ? v : URL.createObjectURL(v);
        pv.hidden = false;
      }).catch(function () { /* 忽略 */ });
    }
  }

  function kinds() { return (MD.sched && MD.sched.KINDS) ? MD.sched.KINDS : FALLBACK_KINDS; }

  /** 按频次渲染动态字段（仅作用于表单内部） */
  function renderKindFields(body, kind, sc) {
    var box = body.querySelector('#md-f-kind');
    if (!box) return;
    var times = (sc && sc.times && sc.times.length) ? sc.times : (kind === 'twice_daily' ? ['08:00', '20:00'] : ['08:00']);
    var html = '';
    if (kind === 'daily' || kind === 'twice_daily') {
      html = '<div class="md-row2">' +
        fieldRow('首次服药时间', inputText('md-f-time', times[0], '', 'time')) +
        (kind === 'twice_daily' ? fieldRow('第二次时间', inputText('md-f-time2', times[1] || '20:00', '', 'time')) : '') +
        '</div>';
    } else if (kind === 'interval_hours') {
      html = '<div class="md-row2">' +
        fieldRow('首次服药时间', inputText('md-f-time', times[0], '', 'time')) +
        fieldRow('间隔小时', inputText('md-f-interval', sc && sc.intervalHours ? sc.intervalHours : 8, '', 'number')) +
        '</div>';
    } else if (kind === 'specific_dates') {
      var dates = (sc && sc.specificDates && sc.specificDates.length) ? sc.specificDates : [dayKey(nowMs())];
      html = fieldRow('服药时间', inputText('md-f-time', times[0], '', 'time')) +
        '<div class="field"><span class="label">指定日期</span><div id="md-f-dates">' +
        dates.map(function (v) { return '<input class="input" type="date" data-date="1" value="' + esc(v) + '">'; }).join('') +
        '</div><button class="btn btn-ghost" type="button" id="md-f-add-date" style="min-height:44px;margin-top:6px">' + ICON.plus + '添加日期</button></div>';
    } else if (kind === 'custom_interval') {
      html = '<div class="md-row2">' +
        fieldRow('首次服药时间', inputText('md-f-time', times[0], '', 'time')) +
        fieldRow('每 N', inputText('md-f-everyn', sc && sc.everyN ? sc.everyN : 2, '', 'number')) +
        '</div>' + fieldRow('单位', selectBox('md-f-cunit', CYCLE_UNITS, sc && sc.unit ? sc.unit : 'day'));
    } else { // cycle
      html = '<div class="md-row2">' +
        fieldRow('服药时间', inputText('md-f-time', times[0], '', 'time')) +
        fieldRow('周期开始日', inputText('md-f-cyclestart', sc && sc.cycleStart ? sc.cycleStart : dayKey(nowMs()), '', 'date')) +
        '</div><div class="md-row2">' +
        fieldRow('服用天数', inputText('md-f-on', sc && sc.cycleOnDays ? sc.cycleOnDays : 21, '', 'number')) +
        fieldRow('停药天数', inputText('md-f-off', sc && sc.cycleOffDays ? sc.cycleOffDays : 7, '', 'number')) +
        '</div>';
    }
    box.innerHTML = html;
    var addBtn = box.querySelector('#md-f-add-date');
    if (addBtn) {
      addBtn.addEventListener('click', function () {
        tap();
        var wrap = box.querySelector('#md-f-dates');
        var i2 = document.createElement('input');
        i2.className = 'input';
        i2.type = 'date';
        i2.setAttribute('data-date', '1');
        i2.value = dayKey(nowMs());
        wrap.appendChild(i2);
      });
    }
  }

  function saveMedForm(body, m, sc) {
    var name = (body.querySelector('#md-f-name').value || '').trim();
    var amount = body.querySelector('#md-f-amount').value;
    if (!name) { toast('请填写药名', 'warn'); return false; }
    if (amount === '' || isNaN(Number(amount))) { toast('请填写剂量', 'warn'); return false; }

    var kindBtn = body.querySelector('#md-f-kindpick .chip--active');
    var kind = kindBtn ? kindBtn.getAttribute('data-kind') : 'daily';
    var time1 = val(body, '#md-f-time') || '08:00';
    var times = [time1];
    if (kind === 'twice_daily') times.push(val(body, '#md-f-time2') || '20:00');

    var id = m ? m.id : uid();
    var rec = {
      id: id,
      profileId: val(body, '#md-f-profile') || curProfileId(),
      name: name,
      dosageAmount: Number(amount),
      dosageUnit: val(body, '#md-f-unit') || '片',
      form: val(body, '#md-f-form') || '',
      strength: val(body, '#md-f-strength') || '',
      foodInstruction: val(body, '#md-f-food') || '无特殊',
      prescriber: val(body, '#md-f-prescriber') || '',
      pharmacy: val(body, '#md-f-pharmacy') || '',
      sideEffects: val(body, '#md-f-side') || '',
      notes: val(body, '#md-f-notes') || '',
      critical: !!(body.querySelector('#md-f-critical') && body.querySelector('#md-f-critical').checked),
      active: true,
      startDate: val(body, '#md-f-start') || dayKey(nowMs()),
      endDate: val(body, '#md-f-end') || '',
      stock: numOrNull(val(body, '#md-f-stock')),
      refillThreshold: numOrNull(val(body, '#md-f-threshold')),
      photoKeys: S.formPhotoKey ? [S.formPhotoKey] : [],
      ver: (Number(m && m.ver) || 0) + 1
    };

    var sched = {
      id: sc ? sc.id : uid(),
      medicationId: id,
      kind: kind,
      times: times,
      critical: rec.critical,
      startDate: rec.startDate,
      endDate: rec.endDate,
      remindOffsetMin: sc && sc.remindOffsetMin != null ? sc.remindOffsetMin : 0,
      ver: (Number(sc && sc.ver) || 0) + 1
    };
    if (kind === 'interval_hours') sched.intervalHours = Number(val(body, '#md-f-interval')) || 8;
    if (kind === 'specific_dates') {
      var ds = [];
      Array.prototype.forEach.call(body.querySelectorAll('[data-date]'), function (i2) { if (i2.value) ds.push(i2.value); });
      sched.specificDates = ds;
    }
    if (kind === 'custom_interval') {
      sched.everyN = Number(val(body, '#md-f-everyn')) || 2;
      sched.unit = val(body, '#md-f-cunit') || 'day';
    }
    if (kind === 'cycle') {
      sched.cycleOnDays = Number(val(body, '#md-f-on')) || 21;
      sched.cycleOffDays = Number(val(body, '#md-f-off')) || 7;
      sched.cycleStart = val(body, '#md-f-cyclestart') || rec.startDate;
    }

    var photoTask = S.pendingPhoto ? savePhoto(S.pendingPhoto, id) : Promise.resolve(null);
    photoTask.then(function (key) {
      if (key) rec.photoKeys = [key];
      S.pendingPhoto = null;
      return MD.data.meds.save(rec);
    }).then(function () {
      return MD.data.schedules.save(sched);
    }).then(function () {
      ok(); toast('药物已保存', 'success');
      closeSheet();
      refreshAll();
    }).catch(function (e) { toast('保存失败：' + msgOf(e), 'error'); });
  }

  function val(body, sel) {
    var n = body.querySelector(sel);
    return n ? n.value : '';
  }
  function numOrNull(v) { return (v === '' || v == null || isNaN(Number(v))) ? null : Number(v); }
  function msgOf(e) { return (e && (e.message || e.msg)) ? (e.message || e.msg) : '未知错误'; }

  /** 压缩图片到最长边 ≤1280px，JPEG 0.82 */
  function compressImage(file) {
    return new Promise(function (resolve, reject) {
      var url = URL.createObjectURL(file);
      var img = new Image();
      img.onload = function () {
        var max = 1280;
        var w = img.width, h = img.height;
        var scale = Math.min(1, max / Math.max(w, h));
        var cw = Math.max(1, Math.round(w * scale)), ch = Math.max(1, Math.round(h * scale));
        var cv = document.createElement('canvas');
        cv.width = cw; cv.height = ch;
        var ctx = cv.getContext('2d');
        ctx.drawImage(img, 0, 0, cw, ch);
        var dataURL = cv.toDataURL('image/jpeg', 0.82);
        dataURLToBlob(dataURL).then(function (b) {
          URL.revokeObjectURL(url);
          resolve({ blob: b, dataURL: dataURL });
        }).catch(reject);
      };
      img.onerror = function () { URL.revokeObjectURL(url); reject(new Error('无法读取图片')); };
      img.src = url;
    });
  }

  function dataURLToBlob(dataURL) {
    if (global.fetch && global.Request) {
      return fetch(dataURL).then(function (r) { return r.blob(); });
    }
    return new Promise(function (resolve) {
      var parts = dataURL.split(',');
      var bin = atob(parts[1]);
      var arr = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
      resolve(new Blob([arr], { type: 'image/jpeg' }));
    });
  }

  /** 保存照片：本地 blob 存储 + 已登录时上传密文 */
  function savePhoto(out, recordId) {
    var key = 'med-photo-' + uid();
    return Promise.resolve(MD.data.blobs.save(key, out.blob))
      .then(function () {
        return encryptForUpload(out.blob).then(function (enc) {
          if (!enc) return false;          // 无加密能力 → 仅本地
          if (!isLoggedIn()) return false; // 未登录 → 仅本地
          return Promise.resolve(MD.api.uploadAttachment({ recordId: recordId, ivB64: enc.ivB64, blob: enc.blob }))
            .then(function () { return true; })
            .catch(function () { return false; });
        });
      })
      .then(function (uploaded) {
        if (!uploaded) toast('照片已保存到本地' + (isLoggedIn() ? '（云端上传稍后重试）' : '（登录后自动上传）'), 'info');
        return key;
      })
      .catch(function (e) { toast('照片保存失败：' + msgOf(e), 'error'); return null; });
  }

  function isLoggedIn() {
    if (MD.api && typeof MD.api.isAuthed === 'function') { try { return !!MD.api.isAuthed(); } catch (e) { return false; } }
    if (MD.api && typeof MD.api.session === 'function') {
      try { var s = MD.api.session(); return !!(s && s.userId); } catch (e) { return false; }
    }
    return false;
  }

  /** 附件加密：MD.crypto.encryptBlob(dek, blob) -> {ivB64, blob}；取不到 DEK 时返回 null（仅本地保存） */
  function encryptForUpload(blob) {
    var dek = (MD.api && typeof MD.api.getDEK === 'function') ? MD.api.getDEK() : null;
    if (!dek || !MD.crypto || typeof MD.crypto.encryptBlob !== 'function') return Promise.resolve(null);
    return Promise.resolve(MD.crypto.encryptBlob(dek, blob)).catch(function () { return null; });
  }

  /* ---- 推迟 / 历史操作 ---- */
  function openSnooze(occKey) {
    openSheet({
      title: '推迟提醒',
      body: (function () {
        var box = document.createElement('div');
        box.innerHTML = '<div class="muted" style="font-size:14px">选择推迟时长，到点后会再次提醒。</div>';
        return box;
      })(),
      actions: SNOOZE_OPTIONS.map(function (min) {
        return {
          label: min + ' 分钟后',
          kind: min === 10 ? 'primary' : 'ghost',
          onClick: function () { doSnooze(occKey, min); }
        };
      })
    });
  }

  function doSnooze(occKey, minutes) {
    var dose = S.doseMap[occKey] || {};
    var until = nowMs() + minutes * 60000;
    if (MD.remind && typeof MD.remind.snooze === 'function') {
      try { MD.remind.snooze(occKey, minutes); } catch (e) { /* 忽略 */ }
    }
    applyIntake(occKey, 'snoozed', {
      medicationId: dose.medId, profileId: dose.profileId, scheduledFor: dose.timeMs,
      doseText: dose.dosageText, snoozeUntil: until, note: '推迟 ' + minutes + ' 分钟'
    }).then(function () {
      ok(); toast('已推迟 ' + minutes + ' 分钟', 'success');
      closeSheet();
      refreshAll();
    }).catch(function (e) { toast('操作失败：' + msgOf(e), 'error'); });
  }

  function openHistoryActions(intakeId) {
    var list = ((S.data && S.data.intakes) || []);
    var it = null;
    list.forEach(function (x) { if (x.id === intakeId) it = x; });
    if (!it) return;
    var box = document.createElement('div');
    var medName = '';
    ((S.data && S.data.meds) || []).forEach(function (m) { if (m.id === it.medicationId) medName = m.name; });
    box.innerHTML = '<div style="font-size:14px">' + esc(medName) + ' · ' + esc(fmtDateTime(it.scheduledFor)) + '</div>' +
      '<div class="muted" style="font-size:13px;margin-top:4px">当前状态：' + esc(STATUS_TEXT[normalizeStatus(it)] || it.status) + '</div>';
    openSheet({
      title: '修改记录',
      body: box,
      actions: [
        { label: '记为已服用', kind: 'primary', onClick: function () { setIntakeStatus(it, 'taken'); } },
        { label: '记为已跳过', kind: 'ghost', onClick: function () { setIntakeStatus(it, 'skipped'); } },
        { label: '撤销为待服用', kind: 'ghost', onClick: function () { setIntakeStatus(it, 'pending'); } },
        {
          label: '删除记录', kind: 'danger', onClick: function () {
            confirmBox({ title: '删除记录', message: '删除后无法恢复，确认删除？', danger: true }).then(function (yes) {
              if (!yes) return;
              MD.data.intakes.remove(it.id).then(function () {
                toast('已删除', 'success');
                closeSheet();
                refreshAll();
              });
            });
            // 返回 false：阻止 action 外层微任务立即 closeSheet——
            // 否则刚弹出的确认框会被同步返回的 undefined 触发的外层关闭逻辑吞掉（「点删除没反应」的根因）。
            // 确认框生命周期由自身按钮管理：确认→删除→内部 closeSheet；取消→onClose 兜底。
            return false;
          }
        }
      ]
    });
  }

  function setIntakeStatus(it, status) {
    var rec = {};
    for (var k in it) if (Object.prototype.hasOwnProperty.call(it, k)) rec[k] = it[k];
    rec.status = status;
    rec.ver = (Number(rec.ver) || 0) + 1;
    rec.updatedAt = nowMs();
    rec.takenAt = status === 'taken' ? (it.takenAt || nowMs()) : 0;
    rec.undoneAt = status === 'pending' ? nowMs() : 0;
    MD.data.intakes.save(rec).then(function () {
      ok(); toast('已更新为「' + (STATUS_TEXT[status] || status) + '」', 'success');
      closeSheet();
      refreshAll();
    }).catch(function (e) { toast('操作失败：' + msgOf(e), 'error'); });
  }

  /* ============================ 10. 事件与交互 ============================ */

  function onRootClick(e) {
    var node = e.target && e.target.closest ? e.target.closest('[data-act]') : null;
    if (!node || !S.root || !S.root.contains(node)) return;
    var act = node.getAttribute('data-act');
    var id = node.getAttribute('data-id');
    var occ = node.getAttribute('data-occ');
    tap();

    if (act === 'take' || act === 'skip' || act === 'undo') {
      var status = act === 'take' ? 'taken' : (act === 'skip' ? 'skipped' : 'pending');
      var dose = S.doseMap[occ] || {};
      applyIntake(occ, status, {
        medicationId: dose.medId, profileId: dose.profileId,
        scheduledFor: dose.timeMs, doseText: dose.dosageText
      }).then(function () {
        if (act === 'take') ok();
        toast('已标记：' + STATUS_TEXT[status], 'success');
        refreshAll();
      }).catch(function (err) { toast('操作失败：' + msgOf(err), 'error'); });
      return;
    }
    if (act === 'snooze') { openSnooze(occ); return; }
    if (act === 'med-add') { openMedForm(null); return; }
    if (act === 'med-open') { openMedForm(id); return; }
    if (act === 'med-del') { confirmDeleteMed(id); return; }
    if (act === 'med-copy') { promptCopyMed(id); return; }
    if (act === 'profile') { switchProfile(id); return; }
    if (act === 'profile-new') { openProfileForm(null); return; }
    if (act === 'profile-edit') { e.stopPropagation(); openProfileForm(id); return; }
    if (act === 'hist-open') { openHistoryActions(id); return; }
    if (act === 'hist-more') { S.historyDays += HISTORY_STEP; refreshAll(); return; }
    if (act === 'heat-prev') { S.heatMonth = shiftMonth(S.heatMonth, -1); refreshAll(); return; }
    if (act === 'heat-next') { S.heatMonth = shiftMonth(S.heatMonth, 1); refreshAll(); return; }
    if (act === 'sample-clear') { confirmClearSamples(); return; }
    if (act === 'exp-csv' || act === 'exp-pdf') { runExport(act === 'exp-csv' ? 'csv' : 'pdf'); return; }
  }

  function switchProfile(id) {
    S.profileId = id || 'all';
    try { global.localStorage.setItem(LS_PROFILE, S.profileId); } catch (e) { /* 忽略 */ }
    refreshAll();
  }

  /** 复制药物：点击后直接自动创建副本（除药名外继承全部属性与排程），无需填写 */
  function promptCopyMed(id) {
    var m = null;
    ((S.data && S.data.meds) || []).forEach(function (x) { if (x.id === id) m = x; });
    if (!m) return;
    // 自动命名：原名（副本）；重名时依次追加序号（副本 2）（副本 3）…
    var existing = {};
    ((S.data && S.data.meds) || []).forEach(function (x) { existing[x.name] = true; });
    var newName = m.name + '（副本）';
    for (var n = 2; existing[newName]; n++) { newName = m.name + '（副本 ' + n + '）'; }
    copyMedication(id, newName);
  }

  /** 以新药名创建副本：继承原药全部字段（photoKeys 清空），并复制其全部排程；服药历史不复制 */
  function copyMedication(id, newName) {
    var m = null;
    ((S.data && S.data.meds) || []).forEach(function (x) { if (x.id === id) m = x; });
    if (!m) return Promise.resolve();
    var now = nowMs();
    var nm = {};
    for (var k in m) if (Object.prototype.hasOwnProperty.call(m, k)) nm[k] = m[k];
    nm.id = uid();
    nm.name = newName;
    nm.ver = 1;
    nm.updatedAt = now;
    nm.photoKeys = [];            // 照片附件属于原药，不随复制（避免两个药共享同一附件）
    var tasks = [MD.data.meds.save(nm)];
    ((S.data && S.data.schedules) || []).forEach(function (sc) {
      if (!sc || sc.medicationId !== id) return;
      var ns = {};
      for (var k2 in sc) if (Object.prototype.hasOwnProperty.call(sc, k2)) ns[k2] = sc[k2];
      ns.id = uid();
      ns.medicationId = nm.id;
      ns.ver = 1;
      ns.updatedAt = now;
      tasks.push(MD.data.schedules.save(ns));
    });
    return Promise.all(tasks).then(function () {
      toast('已复制为「' + newName + '」', 'success');
      refreshAll();
    }).catch(function (e) { toast('复制失败：' + msgOf(e), 'error'); });
  }

  function confirmDeleteMed(id) {
    var m = null;
    ((S.data && S.data.meds) || []).forEach(function (x) { if (x.id === id) m = x; });
    if (!m) return;
    confirmBox({ title: '删除药物', message: '将同时删除该药物的排程，确认删除「' + m.name + '」？', danger: true })
      .then(function (yes) {
        if (!yes) return;
        var tasks = [MD.data.meds.remove(id)];
        ((S.data && S.data.schedules) || []).forEach(function (s) {
          if (s.medicationId === id) tasks.push(MD.data.schedules.remove(s.id));
        });
        ((m.photoKeys) || []).forEach(function (k) {
          if (MD.data.blobs && MD.data.blobs.remove) tasks.push(Promise.resolve(MD.data.blobs.remove(k)));
        });
        return Promise.all(tasks).then(function () { toast('已删除', 'success'); refreshAll(); });
      })
      .catch(function (e) { toast('删除失败：' + msgOf(e), 'error'); });
  }

  function confirmClearSamples() {
    confirmBox({ title: '清空示例数据', message: '只删除带示例标记的记录，你的真实数据不受影响。', danger: true })
      .then(function (yes) {
        if (!yes) return;
        return clearSamples().then(function () { toast('示例数据已清空', 'success'); refreshAll(); });
      })
      .catch(function (e) { toast('清空失败：' + msgOf(e), 'error'); });
  }

  /* ---- 导出 ---- */
  function exportRange() {
    var sel = S.root && S.root.querySelector('#md-exp-range');
    var mode = sel ? sel.value : '30';
    var now = nowMs();
    if (mode === 'month') return { from: monthStartMs(monthKeyOf(now)), to: startOfDay(now) + 86400000, label: '本月' };
    if (mode === '90') return { from: startOfDay(addDays(now, -89)), to: startOfDay(now) + 86400000, label: '近 90 天' };
    if (mode === 'custom') {
      var f = S.root.querySelector('#md-exp-from'), t = S.root.querySelector('#md-exp-to');
      var from = f && f.value ? new Date(f.value + 'T00:00:00').getTime() : startOfDay(now);
      var to = t && t.value ? new Date(t.value + 'T00:00:00').getTime() + 86400000 : startOfDay(now) + 86400000;
      return { from: from, to: to, label: '自定义' };
    }
    return { from: startOfDay(addDays(now, -29)), to: startOfDay(now) + 86400000, label: '近 30 天' };
  }

  function runExport(format) {
    if (!MD['export'] || !MD['export'].rows) { toast('导出模块未加载', 'error'); return; }
    var r = exportRange();
    var psel = S.root && S.root.querySelector('#md-exp-profile');
    var pid = psel ? psel.value : 'all';
    var pname = '全部档案';
    ((S.data && S.data.profiles) || []).forEach(function (p) { if (p.id === pid) pname = p.name; });
    toast('正在生成' + (format === 'csv' ? ' CSV' : ' PDF') + '…', 'info');
    MD['export'].rows({ from: r.from, to: r.to, profileId: pid === 'all' ? null : pid })
      .then(function (rows) {
        if (!rows || !rows.length) { toast('所选范围内没有记录', 'warn'); return null; }
        var meta = {
          title: '用药记录 · ' + pname,
          profile: pname,
          rangeFrom: dayKey(r.from),
          rangeTo: dayKey(Math.min(r.to, startOfDay(nowMs()))),
          range: r.label
        };
        return format === 'csv' ? MD['export'].csv(rows, meta) : MD['export'].pdf(rows, meta);
      })
      .then(function (done) {
        if (done === null) return;
        ok(); toast('导出完成', 'success');
      })
      .catch(function (e) { toast('导出失败：' + msgOf(e), 'error'); });
  }

  /* ============================ 11. 提醒联动 ============================ */

  function bindBus() {
    if (!MD.bus || !MD.bus.on) return;
    MD.bus.on('remind:action', function (p) {
      var occKey = pick(p, 'occKey');
      var action = pick(p, 'action', 'status');
      if (!occKey) return;
      var status = (action === 'take' || action === 'taken') ? 'taken'
        : (action === 'skip' || action === 'skipped') ? 'skipped' : 'pending';
      var dose = S.doseMap[occKey] || {};
      applyIntake(occKey, status, {
        medicationId: dose.medId || pick(p, 'medicationId', 'medId'),
        profileId: dose.profileId || pick(p, 'profileId'),
        scheduledFor: dose.timeMs || pick(p, 'scheduledFor', 'timeMs'),
        doseText: dose.dosageText || pick(p, 'dosageText')
      }).then(refreshAll).catch(function () { /* 忽略 */ });
    });
    MD.bus.on('remind:snooze', function (p) {
      var occKey = pick(p, 'occKey');
      if (!occKey) return;
      var minutes = Number(pick(p, 'minutes', 'min', 'snoozeMinutes')) || 10;
      doSnooze(occKey, minutes);
    });
    MD.bus.on('data:changed', function () { refreshAll(); });
    // 契约 6.5 的 MD.remind.onFire(cb)：页面内提示提醒已触发（不在这里 refresh，避免与
    // pushReminders → remind.schedule 形成重入）。
    if (MD.remind && typeof MD.remind.onFire === 'function') {
      MD.remind.onFire(function (dose) {
        toast('用药提醒：' + (dose && dose.medName ? dose.medName : '药物') +
          ' · ' + fmtTime(dose && dose.timeMs ? dose.timeMs : nowMs()), 'info');
      });
    }
  }

  /** 兼容不同子 agent 的载荷字段名 */
  function pick(p, a, b, c) {
    p = p || {};
    if (p[a] !== undefined) return p[a];
    if (b && p[b] !== undefined) return p[b];
    if (c && p[c] !== undefined) return p[c];
    if (p.dose && p.dose[a] !== undefined) return p.dose[a];
    return undefined;
  }

  /* ============================ 12. 注册与生命周期 ============================ */

  function render(el) {
    S.root = el;
    try {
      var saved = global.localStorage.getItem(LS_PROFILE);
      if (saved) S.profileId = saved;
    } catch (e) { /* 忽略 */ }
    if (!S.root.__mdBound) {
      S.root.addEventListener('click', onRootClick);
      S.root.addEventListener('change', onRangeChange);
      S.root.__mdBound = true;
    }
    mount(el);
    refreshAll();
  }

  /** 导出范围切换为「自定义」时显示日期输入 */
  function onRangeChange(e) {
    if (!e.target || e.target.id !== 'md-exp-range') return;
    var box = S.root && S.root.querySelector('#md-exp-custom');
    if (!box) return;
    box.hidden = (e.target.value !== 'custom');
  }

  function register() {
    if (!MD.router || typeof MD.router.register !== 'function') return;
    // icon 必须是 index.html 内联 sprite 中的符号名（core.js 用 '#i-<name>' 引用），
    // 不能传 HTML 字符串，否则 <use href> 指向不存在的 id 会渲染成空白图标。
    MD.router.register({
      id: 'medication',
      title: '用药提醒',
      subtitle: '今日剂量与依从性',
      icon: 'pill',
      group: '核心',
      order: 1,
      render: render
    });
    bindBus();
  }

  /* ============================ 13. 对外接口 ============================ */

  med.register = register;
  med.refresh = refreshAll;
  med.openMedForm = openMedForm;
  med.openProfileForm = openProfileForm;
  med.seedSamples = seedSamples;
  med.clearSamples = clearSamples;

  // 供 refreshAll 之外的调试使用；不在生产交互中依赖
  med._state = S;
})(typeof window !== 'undefined' ? window : this);

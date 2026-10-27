/* =============================================================================
 * MedDesk · schedule.js（归属 A7）
 * 排程引擎：纯函数，无 DOM、无网络、无 I/O、无副作用。
 *
 * 依赖（只按契约调用，不修改）：
 *   MD.util.dayKey(ms) / MD.util.startOfDay(ms) / MD.util.addDays(ms, n)
 *   若 A6 尚未提供，本文件内建等价的本地时区实现（YYYY-MM-DD 本国本地日期）。
 *
 * 时间基准：一律使用「本地时区」毫秒时间戳；日期键 dayKey = 'YYYY-MM-DD'（本地）。
 * 所有日期推进都用「本地日历算术」（先定位到当天 12:00 再做 setDate），
 * 以避免夏令时（DST）导致的 23/25 小时误差。
 * ============================================================================= */
(function (global) {
  'use strict';

  var MD = global.MD || (global.MD = {});

  /* ---------------------------------------------------------------- 常量 */
  var SECOND_MS = 1000;
  var MINUTE_MS = 60 * SECOND_MS;
  var HOUR_MS = 60 * MINUTE_MS;
  var DAY_MS = 24 * HOUR_MS;

  var OVERDUE_GRACE_MS = 15 * MINUTE_MS; // 超过计划时间 15 分钟才判为逾期/漏服
  var YESTERDAY_SPAN_MS = DAY_MS;        // buildToday 向前多看 24 小时（昨天未处理顺延到今天）
  var MAX_SCAN_DAYS = 3660;              // 扫描上限约 10 年，防御死循环
  var FALLBACK_ANCHOR = '1970-01-01';    // custom_interval 缺 startDate 时的确定性降级锚点

  var KINDS = [
    { id: 'daily', label: '每日' },
    { id: 'twice_daily', label: '每日两次' },
    { id: 'thrice_daily', label: '每日三次' },
    { id: 'interval_hours', label: '按小时间隔' },
    { id: 'specific_dates', label: '指定日期' },
    { id: 'custom_interval', label: '自定义间隔' },
    { id: 'cycle', label: '周期性用药（21天服/7天停）' }
  ];

  var DEFAULT_TIMES = ['08:00'];
  var DEFAULT_TWICE_TIMES = ['08:00', '20:00'];
  var DEFAULT_THRICE_TIMES = ['08:00', '13:00', '20:00'];

  /* ------------------------------------------------------- 基础时间工具 */
  function pad2(n) { return (n < 10 ? '0' : '') + n; }
  function pad4(n) { n = String(n); while (n.length < 4) { n = '0' + n; } return n; }

  /** 取 MD.util（A6 提供）；缺失时返回空对象，走内建实现 */
  function U() { return (MD && MD.util) || {}; }

  /** 毫秒时间戳 -> 'YYYY-MM-DD'（本地时区） */
  function dayKeyOf(ms) {
    var u = U();
    if (typeof u.dayKey === 'function') { return u.dayKey(ms); }
    var d = new Date(ms);
    return pad4(d.getFullYear()) + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  }

  /** 毫秒时间戳 -> 当天 00:00:00.000（本地时区） */
  function startOfDay(ms) {
    var u = U();
    if (typeof u.startOfDay === 'function') { return u.startOfDay(ms); }
    var d = new Date(ms);
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  }

  /** 本地日历天推进 n 天（DST 安全）：先锚到当天 12:00，再加减天数，最后归零 */
  function addDaysLocal(ms, n) {
    var u = U();
    if (typeof u.addDays === 'function') {
      var r = u.addDays(ms, n);
      if (typeof r === 'number' && isFinite(r)) { return r; }
    }
    var d = new Date(ms);
    d.setHours(12, 0, 0, 0);
    d.setDate(d.getDate() + n);
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  }

  var RE_DAYKEY = /^(\d{4})-(\d{2})-(\d{2})$/;

  /** 'YYYY-MM-DD' -> {y,m,d}；非法返回 null */
  function parseDayKey(k) {
    if (typeof k !== 'string') { return null; }
    var m = RE_DAYKEY.exec(k.trim());
    if (!m) { return null; }
    var y = +m[1], mo = +m[2], d = +m[3];
    if (mo < 1 || mo > 12 || d < 1 || d > 31) { return null; }
    // 再次用 Date 校验（排除 02-30 之类）
    var dt = new Date(y, mo - 1, d, 12, 0, 0, 0);
    if (dt.getFullYear() !== y || dt.getMonth() !== mo - 1 || dt.getDate() !== d) { return null; }
    return { y: y, m: mo, d: d };
  }

  /** 'YYYY-MM-DD' -> 当天 00:00 本地毫秒；非法返回 NaN */
  function keyToMs(k) {
    var p = parseDayKey(k);
    if (!p) { return NaN; }
    return new Date(p.y, p.m - 1, p.d, 0, 0, 0, 0).getTime();
  }

  /** 两个 dayKey 之间相差的天数（DST 安全，四舍五入消除 23/25 小时误差） */
  function diffDays(aKey, bKey) {
    var a = keyToMs(aKey), b = keyToMs(bKey);
    if (!isFinite(a) || !isFinite(b)) { return NaN; }
    return Math.round((b - a) / DAY_MS);
  }

  /** 某年某月的天数 */
  function daysInMonth(y, m) { return new Date(y, m, 0, 12, 0, 0, 0).getDate(); }

  /** 解析 'HH:MM[:SS]' -> {h,m,s}；非法返回 null */
  function parseTime(s) {
    if (typeof s !== 'string') { return null; }
    var m = /^(\d{1,2}):(\d{2})(?::(\d{2}))?$/.exec(s.trim());
    if (!m) { return null; }
    var h = +m[1], mi = +m[2], se = m[3] ? +m[3] : 0;
    if (h > 23 || mi > 59 || se > 59) { return null; }
    return { h: h, m: mi, s: se };
  }

  /** 本地日历天 + 距零点分钟数 -> 毫秒（按墙上时钟构造，DST 安全） */
  function msOnDay(p, minutesFromMidnight) {
    return new Date(p.y, p.m - 1, p.d, 0, minutesFromMidnight, 0, 0).getTime();
  }

  function num(v, def) {
    var n = Number(v);
    return (isFinite(n) ? n : def);
  }

  /** 数字格式化：去掉浮点噪声（0.5 -> '0.5'，1 -> '1'） */
  function fmtNum(n) {
    if (n === null || n === undefined || n === '' || !isFinite(Number(n))) { return ''; }
    var r = Math.round(Number(n) * 1000) / 1000;
    return String(r);
  }

  /* -------------------------------------------------- Schedule 字段读取 */
  function normTimes(times) {
    var out = [];
    if (Object.prototype.toString.call(times) === '[object Array]') {
      for (var i = 0; i < times.length; i++) {
        var s = times[i];
        if (typeof s === 'number' && isFinite(s)) {
          // 兼容「分钟数」写法（如 480 表示 08:00）
          var mm = Math.max(0, Math.min(1439, Math.round(s)));
          s = pad2(Math.floor(mm / 60)) + ':' + pad2(mm % 60);
        }
        if (parseTime(s)) { out.push(String(s).trim()); }
      }
    } else if (parseTime(times)) {
      out.push(String(times).trim());
    }
    return out;
  }

  /** 各 kind 在该日需要的时间点（返回 'HH:MM' 列表） */
  function timeListFor(schedule) {
    var kind = schedule.kind;
    var times = normTimes(schedule.times);
    if (kind === 'twice_daily') {
      if (times.length === 0) { return DEFAULT_TWICE_TIMES.slice(); }
      return times.slice(0, 2);
    }
    if (kind === 'thrice_daily') {
      if (times.length === 0) { return DEFAULT_THRICE_TIMES.slice(); }
      return times.slice(0, 3);
    }
    if (kind === 'interval_hours') {
      return times.length ? times.slice(0, 1) : DEFAULT_TIMES.slice();
    }
    return times.length ? times : DEFAULT_TIMES.slice();
  }

  /* ----------------------------------------------------- 生效判定 */
  /** 是否落在 startDate/endDate 限定的生效区间内 */
  function inRange(schedule, dayKey) {
    var s = parseDayKey(schedule.startDate) ? schedule.startDate : null;
    var e = parseDayKey(schedule.endDate) ? schedule.endDate : null;
    if (s && dayKey < s) { return false; }
    if (e && dayKey > e) { return false; }
    return true;
  }

  /** custom_interval 在某日是否命中（day/week/month） */
  function customIntervalHit(schedule, dayKey) {
    var everyN = Math.max(1, Math.round(num(schedule.everyN, 1)));
    var anchor = parseDayKey(schedule.startDate) ? schedule.startDate : FALLBACK_ANCHOR;
    var unit = schedule.unit === 'week' || schedule.unit === 'month' ? schedule.unit : 'day';
    if (unit === 'day') {
      var dd = diffDays(anchor, dayKey);
      return dd >= 0 && (dd % everyN) === 0;
    }
    if (unit === 'week') {
      var dw = diffDays(anchor, dayKey);
      return dw >= 0 && (dw % (everyN * 7)) === 0;
    }
    // month：按月序号差值取模，并把「目标日」夹到当月最后一天（01-31 -> 02-28/02-29）
    var pa = parseDayKey(anchor), pb = parseDayKey(dayKey);
    var ia = pa.y * 12 + (pa.m - 1);
    var ib = pb.y * 12 + (pb.m - 1);
    var dm = ib - ia;
    if (dm < 0 || (dm % everyN) !== 0) { return false; }
    var targetDay = Math.min(pa.d, daysInMonth(pb.y, pb.m));
    return pb.d === targetDay;
  }

  /** cycle 在某日是否处于服药期 */
  function cycleHit(schedule, dayKey) {
    var start = parseDayKey(schedule.cycleStart) ? schedule.cycleStart : null;
    if (!start) { return false; }
    var on = Math.max(0, Math.round(num(schedule.cycleOnDays, 21)));
    var off = Math.max(0, Math.round(num(schedule.cycleOffDays, 7)));
    var span = on + off;
    if (span <= 0) { return false; }
    var idx = diffDays(start, dayKey);
    if (idx < 0) { return false; }         // 周期开始之前不算
    if (off <= 0) { return true; }         // 无停药期则一直有效
    return (idx % span) < on;
  }

  /**
   * 判断某排程在指定日期（'YYYY-MM-DD'，本地）是否生效。
   * 综合：deleted / 生效区间 / 各 kind 规则；cycle 的停药期返回 false。
   */
  function isActiveOn(schedule, dayKey) {
    if (!schedule || typeof dayKey !== 'string') { return false; }
    if (schedule.deleted === true) { return false; }
    var k = dayKey.trim();
    if (!parseDayKey(k)) { return false; }
    if (!inRange(schedule, k)) { return false; }

    switch (schedule.kind) {
      case 'daily':
      case 'twice_daily':
      case 'thrice_daily':
        return true;
      case 'interval_hours':
        // 每天从起始时间起按小时间隔发生（当天 24:00 前的部分），跨天次日重新开始
        return true;
      case 'specific_dates':
        var ds = schedule.specificDates;
        if (Object.prototype.toString.call(ds) !== '[object Array]') { return false; }
        for (var i = 0; i < ds.length; i++) {
          if (typeof ds[i] === 'string' && ds[i].trim() === k) { return true; }
        }
        return false;
      case 'custom_interval':
        return customIntervalHit(schedule, k);
      case 'cycle':
        return cycleHit(schedule, k);
      default:
        return false;
    }
  }

  /* ------------------------------------------------- 单日 occurrence */
  /** 某排程在指定日期产生的所有时间点（毫秒数组，升序，已按当天 24:00 截断） */
  function dayOccMs(schedule, dayKey) {
    var p = parseDayKey(dayKey);
    if (!p) { return []; }
    var out = [];

    if (schedule.kind === 'interval_hours') {
      var base = parseTime(timeListFor(schedule)[0]) || { h: 8, m: 0, s: 0 };
      var iv = num(schedule.intervalHours, 8);
      if (!isFinite(iv) || iv <= 0) { iv = 8; }
      var stepMin = iv * 60;                       // 支持小数小时（如 1.5）
      var startMin = base.h * 60 + base.m;
      for (var i = 0; i < 500; i++) {
        var mins = startMin + i * stepMin;
        if (mins >= 1440) { break; }               // 仅在当天 24:00 前生成
        out.push(msOnDay(p, Math.round(mins)));
      }
      return out;
    }

    var times = timeListFor(schedule);
    for (var j = 0; j < times.length; j++) {
      var t = parseTime(times[j]);
      if (t) { out.push(msOnDay(p, t.h * 60 + t.m) + t.s * SECOND_MS); }
    }
    out.sort(function (a, b) { return a - b; });
    return out;
  }

  /** 生成 occurrence 键：`${schedule.id}@${timeMs}` */
  function occKey(schedule, timeMs) {
    var id = (schedule && schedule.id !== undefined && schedule.id !== null) ? schedule.id : '';
    return String(id) + '@' + String(timeMs);
  }

  /**
   * 列出排程在 [fromMs, toMs] 闭区间内的所有 occurrence。
   * 返回 [{ timeMs, medId, schedId, occKey }]，按时间升序。
   */
  function occurrences(schedule, fromMs, toMs) {
    fromMs = num(fromMs, NaN);
    toMs = num(toMs, NaN);
    if (!schedule || !isFinite(fromMs) || !isFinite(toMs) || toMs < fromMs) { return []; }

    var out = [];
    var cursor = startOfDay(fromMs);
    var lastDay = startOfDay(toMs);
    var endKey = parseDayKey(schedule.endDate) ? schedule.endDate : null;
    var guard = 0;

    while (cursor <= lastDay && guard < MAX_SCAN_DAYS) {
      guard++;
      var k = dayKeyOf(cursor);
      if (endKey && k > endKey) { break; }
      if (isActiveOn(schedule, k)) {
        var list = dayOccMs(schedule, k);
        for (var i = 0; i < list.length; i++) {
          var t = list[i];
          if (t >= fromMs && t <= toMs) {
            out.push({ timeMs: t, medId: schedule.medicationId, schedId: schedule.id, occKey: occKey(schedule, t) });
          }
        }
      }
      cursor = addDaysLocal(cursor, 1);
    }
    out.sort(function (a, b) { return a.timeMs - b.timeMs || (a.occKey < b.occKey ? -1 : 1); });
    return out;
  }

  /**
   * 取排程在 afterMs 之后（严格大于）的下一次 occurrence；没有则 null。
   * 最多向前扫描 MAX_SCAN_DAYS 天，遇到 endDate 提前终止。
   */
  function nextAfter(schedule, afterMs) {
    afterMs = num(afterMs, NaN);
    if (!schedule || !isFinite(afterMs)) { return null; }
    var cursor = startOfDay(afterMs);
    var endKey = parseDayKey(schedule.endDate) ? schedule.endDate : null;
    var guard = 0;
    while (guard < MAX_SCAN_DAYS) {
      guard++;
      var k = dayKeyOf(cursor);
      if (endKey && k > endKey) { return null; }
      if (isActiveOn(schedule, k)) {
        var list = dayOccMs(schedule, k);
        for (var i = 0; i < list.length; i++) {
          if (list[i] > afterMs) {
            return { timeMs: list[i], medId: schedule.medicationId, schedId: schedule.id, occKey: occKey(schedule, list[i]) };
          }
        }
      }
      cursor = addDaysLocal(cursor, 1);
    }
    return null;
  }

  /* ------------------------------------------------------- 人类可读描述 */
  function kindLabel(id) {
    for (var i = 0; i < KINDS.length; i++) { if (KINDS[i].id === id) { return KINDS[i].label; } }
    return id || '未知';
  }

  function joinTimes(times) {
    var arr = (times && times.length) ? times : DEFAULT_TIMES.slice();
    return arr.join('、');
  }

  /** 人类可读描述，如 "每日 08:00、20:00" */
  function describe(schedule) {
    if (!schedule) { return ''; }
    var lbl = kindLabel(schedule.kind);
    var times = timeListFor(schedule);
    var tail = '';

    switch (schedule.kind) {
      case 'daily':
        tail = joinTimes(times);
        break;
      case 'twice_daily':
      case 'thrice_daily':
        tail = joinTimes(times);
        break;
      case 'interval_hours':
        tail = '每 ' + fmtNum(num(schedule.intervalHours, 8)) + ' 小时（' + (times[0] || '08:00') + ' 起）';
        break;
      case 'specific_dates':
        var ds = (Object.prototype.toString.call(schedule.specificDates) === '[object Array]') ? schedule.specificDates : [];
        tail = (ds.length <= 3 ? ds.join('、') : (ds.length + ' 个日期')) + ' ' + joinTimes(times);
        break;
      case 'custom_interval':
        var unitTxt = schedule.unit === 'week' ? '周' : (schedule.unit === 'month' ? '个月' : '天');
        tail = '每 ' + fmtNum(num(schedule.everyN, 1)) + ' ' + unitTxt + ' ' + joinTimes(times);
        break;
      case 'cycle':
        tail = '服 ' + Math.round(num(schedule.cycleOnDays, 21)) + ' 天停 ' + Math.round(num(schedule.cycleOffDays, 7)) + ' 天 ' + joinTimes(times);
        break;
      default:
        tail = joinTimes(times);
    }

    var s = lbl + ' ' + tail;
    var from = parseDayKey(schedule.startDate) ? schedule.startDate : '';
    var to = parseDayKey(schedule.endDate) ? schedule.endDate : '';
    if (from || to) { s += '（' + (from ? from + ' 起' : '') + (to ? (from ? '，' : '') + to + ' 止' : '') + '）'; }
    return s.replace(/\s+/g, ' ').trim();
  }

  /* ------------------------------------------------------------ buildToday */
  function indexBy(list, keyFn) {
    var map = Object.create(null);
    if (Object.prototype.toString.call(list) !== '[object Array]') { return map; }
    for (var i = 0; i < list.length; i++) {
      var it = list[i];
      if (!it || it.deleted === true) { continue; }
      var k = keyFn(it);
      if (k && map[k] === undefined) { map[k] = it; }
    }
    return map;
  }

  function normStatus(s) {
    s = String(s || '').toLowerCase();
    if (s === 'taken' || s === 'skip' || s === 'skipped') { return s === 'taken' ? 'taken' : 'skipped'; }
    if (s === 'missed') { return 'missed'; }
    if (s === 'snoozed' || s === 'snooze') { return 'snoozed'; }
    return 'pending';
  }

  function doseText(med) {
    if (!med) { return ''; }
    var amt = fmtNum(med.dosageAmount);
    var unit = med.dosageUnit ? String(med.dosageUnit) : '';
    if (!amt && !unit) { return ''; }
    return (amt && unit) ? (amt + ' ' + unit) : (amt || unit);
  }

  /**
   * 构建「今天要处理」列表。
   * @param {Object} arg {meds, schedules, intakes, profileId, nowMs}
   * @returns {Array} DoseVM[] 按 timeMs 升序
   * DoseVM = {occKey, medId, schedId, profileId, medName, dosageText, timeMs,
   *           status, intakeId, overdue, critical, remindOffsetMin}
   */
  function buildToday(arg) {
    arg = arg || {};
    var meds = arg.meds || [];
    var schedules = arg.schedules || [];
    var intakes = arg.intakes || [];
    var profileId = (arg.profileId === undefined ? null : arg.profileId);
    var nowMs = num(arg.nowMs, Date.now());

    var medMap = indexBy(meds, function (m) { return m && m.id; });
    var byOcc = indexBy(intakes, function (it) { return it && it.occKey; });
    // 兜底索引：A8 若以 `medicationId@scheduledFor` 形式写入 occKey 也能匹配上
    var byMedTime = indexBy(intakes, function (it) {
      return (it && it.medicationId !== undefined && it.scheduledFor !== undefined)
        ? (String(it.medicationId) + '@' + String(it.scheduledFor)) : null;
    });

    var fromMs = startOfDay(nowMs) - YESTERDAY_SPAN_MS; // 昨天的 00:00
    var toMs = startOfDay(nowMs) + DAY_MS - 1;          // 今天的 23:59:59.999
    var todayStartMs = startOfDay(nowMs);               // 用于区分「昨天的」与「今天的」剂量
    var out = [];

    for (var i = 0; i < schedules.length; i++) {
      var sch = schedules[i];
      if (!sch || sch.deleted === true) { continue; }
      var med = medMap[sch.medicationId] || null;
      if (med && med.deleted === true) { continue; }
      if (profileId !== null && profileId !== undefined) {
        var ownerId = med ? med.profileId : profileId;
        if (ownerId !== profileId) { continue; }
      }

      var occs = occurrences(sch, fromMs, toMs);
      for (var j = 0; j < occs.length; j++) {
        var occ = occs[j];
        var key = occ.occKey;
        var intake = byOcc[key] || byMedTime[String(sch.medicationId) + '@' + occ.timeMs] || null;
        var status, overdue = false;

        if (intake) {
          var st = normStatus(intake.status);
          if (st === 'taken' || st === 'skipped' || st === 'missed') {
            status = st;
          } else if (st === 'snoozed') {
            var until = num(intake.snoozeUntil, 0);
            if (until > nowMs) {
              status = 'snoozed';
            } else {
              status = 'pending';
              overdue = occ.timeMs < (nowMs - OVERDUE_GRACE_MS);
            }
          } else {
            status = 'pending';
            overdue = occ.timeMs < (nowMs - OVERDUE_GRACE_MS);
          }
        } else if (occ.timeMs < (nowMs - OVERDUE_GRACE_MS)) {
          // 无人处理且已过宽限期 -> 漏服（昨天未处理的也会出现在这里，不会消失）
          status = 'missed';
          overdue = true;
        } else {
          status = 'pending';
        }

        // 昨天的剂量只在「未完成」时顺延展示：昨天已服用/已跳过的不要再占用今日列表，
        // 否则跨过零点后同一种药会同时出现「昨天已完成」与「今天待处理」两条，看起来就是重复提醒。
        // 昨天漏服(missed)与待处理(pending)仍然保留，保证昨天没做完的不会凭空消失。
        if (occ.timeMs < todayStartMs && (status === 'taken' || status === 'skipped')) { continue; }

        var name = med ? (med.name || String(med.id || '').slice(0, 8)) : String(sch.medicationId || '').slice(0, 8);
        out.push({
          occKey: key,
          medId: sch.medicationId,
          schedId: sch.id,
          profileId: med ? med.profileId : profileId,
          medName: name,
          dosageText: doseText(med),
          timeMs: occ.timeMs,
          status: status,
          intakeId: intake ? intake.id : null,
          overdue: overdue,
          critical: !!(sch.critical || (med && med.critical)),
          // 非契约字段：供 MD.remind 计算提前量（A8 可忽略）
          remindOffsetMin: num(sch.remindOffsetMin, 0)
        });
      }
    }

    out.sort(function (a, b) {
      if (a.timeMs !== b.timeMs) { return a.timeMs - b.timeMs; }
      if (a.medName !== b.medName) { return a.medName < b.medName ? -1 : 1; }
      return a.occKey < b.occKey ? -1 : 1;
    });
    return out;
  }

  /* ------------------------------------------------------------ adherence */
  function intakeTime(it) {
    var t = num(it.takenAt, NaN);
    if (!isFinite(t) || t <= 0) { t = num(it.scheduledFor, NaN); }
    return isFinite(t) ? t : 0;
  }

  /**
   * 依从性统计。
   * @param {Object} arg {intakes, fromMs, toMs}
   * @returns {{total:number, taken:number, skipped:number, missed:number, rate:number|null, streakDays:number}}
   * rate = taken / (taken + skipped + missed)；分母为 0 时返回 null。
   */
  function adherence(arg) {
    arg = arg || {};
    var list = arg.intakes || [];
    var fromMs = num(arg.fromMs, 0);
    var toMs = num(arg.toMs, Date.now());
    if (!isFinite(fromMs)) { fromMs = 0; }
    if (!isFinite(toMs)) { toMs = Date.now(); }
    if (toMs < fromMs) { var tmp = fromMs; fromMs = toMs; toMs = tmp; }

    var taken = 0, skipped = 0, missed = 0;
    var takenDaySet = Object.create(null);
    var items = Object.prototype.toString.call(list) === '[object Array]' ? list : [];

    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      if (!it || it.deleted === true) { continue; }
      var t = intakeTime(it);
      if (t < fromMs || t > toMs) { continue; }
      var st = normStatus(it.status);
      if (st === 'taken') {
        taken++;
        takenDaySet[dayKeyOf(t)] = true;
      } else if (st === 'skipped') {
        skipped++;
      } else if (st === 'missed') {
        missed++;
      } else {
        // pending / snoozed：计划时间已过宽限期且未改期 -> 计为漏服
        var due = num(it.scheduledFor, t);
        var until = num(it.snoozeUntil, 0);
        if (until > toMs) { continue; }
        if (due <= (toMs - OVERDUE_GRACE_MS)) { missed++; }
      }
    }

    var denom = taken + skipped + missed;
    var rate = denom === 0 ? null : Math.round((taken / denom) * 10000) / 10000;

    // 连续服药天数：从 toMs 当天往前数，每天只要有 1 条已服用记录即计入
    var streak = 0;
    var cursor = startOfDay(toMs);
    for (var g = 0; g < MAX_SCAN_DAYS; g++) {
      if (takenDaySet[dayKeyOf(cursor)]) {
        streak++;
        cursor = addDaysLocal(cursor, -1);
      } else {
        break;
      }
    }

    return { total: denom, taken: taken, skipped: skipped, missed: missed, rate: rate, streakDays: streak };
  }

  /* ---------------------------------------------------------------- 导出 */
  MD.sched = {
    KINDS: KINDS,
    occKey: occKey,
    isActiveOn: isActiveOn,
    occurrences: occurrences,
    nextAfter: nextAfter,
    describe: describe,
    buildToday: buildToday,
    adherence: adherence
  };

  if (typeof module !== 'undefined' && module.exports) { module.exports = MD.sched; }
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this));

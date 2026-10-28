/**
 * 原生离线操作回填适配器。
 *
 * 【为什么单独一个文件】
 * 安卓通知按钮在 App 被系统杀掉时也能点，点击动作先存在原生侧（DoseStore），
 * 等 App 回到前台再由 MD.nativeAlarm.drainAndApply() 拉取。原生侧只认得 occKey，
 * 而业务层要的是一条完整的服药记录。本文件负责完成这层「只带 occKey → 完整记录」的转换。
 *
 * 【为什么不复用 med.js 里的 applyIntake】
 * 那个函数读的是 med.js 模块内的内存态 S.data.intakes。App 长时间在后台时该快照可能已过期，
 * 用它做 upsert 有重复建记录的风险。这里一律以 MD.data.intakes.list() 为准（读取权威存储），
 * 字段语义则与 applyIntake 保持一致，避免两套规则。
 *
 * 【约束】
 * - 依赖注入式：native-alarm.js 通过存在性判断调用 MD.record.markStatus，本文件缺失也不影响主流程。
 * - 不在原生之外的环境做任何事：本模块不做 Capacitor 判断，是否调用由调用方决定。
 */
(function (window) {
  'use strict';

  var MD = window.MD || (window.MD = {});

  // 原生端动作的取值集合，与 capacitor-plugins/meddesk-alarm/definitions.ts 中的
  // DoseAction.action 保持一致。此处再列一遍是为了解耦：任一侧调整时，这里会明显失配而不是静默出错。
  //
  // 【为什么没有 opened】
  // opened 意味着用户只是点开了通知、并未在「已服用/跳过/稍后」里做选择。
  // 为它写一条记录会凭空产生一条 pending 并带上 undoneAt，既污染服药历史，
  // 也会让「跨零点不重复提醒」那条顺延规则误判。故这里不做映射，
  // markStatus 返回 false（语义是「我跳过了这条」，调用方不当作失败处理）。
  var ACTION_TO_STATUS = {
    taken: 'taken',
    skipped: 'skipped',
    snoozed: 'snoozed'
  };

  function uid() {
    return Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
  }

  function num(v, d) {
    var n = Number(v);
    return isFinite(n) ? n : d;
  }

  /**
   * 从 occKey 反解出 {schedId, scheduledFor}。
   * occKey 格式由 schedule.js 的 occKey(schedule, timeMs) 定义为 '<schedId>@<timeMs>'。
   * 解不出来返回 null，调用方会跳过该条而不是写入半截数据。
   */
  function parseOccKey(occKey) {
    var s = String(occKey == null ? '' : occKey);
    var at = s.lastIndexOf('@');
    if (at <= 0 || at === s.length - 1) return null;
    var timeMs = num(s.slice(at + 1), NaN);
    if (!isFinite(timeMs)) return null;
    return { schedId: s.slice(0, at), scheduledFor: timeMs };
  }

  function indexBy(list, keyFn) {
    var map = {};
    (list || []).forEach(function (item) {
      var k = keyFn(item);
      if (k !== null && k !== undefined) map[k] = item;
    });
    return map;
  }

  /**
   * 按 occKey 幂等地把服药记录写成目标状态。
   *
   * @param {{occKey:string, status:string, atMs:number}} opts
   *        occKey  必填，来自 buildToday()
   *        status  取值见 ACTION_TO_STATUS
   *        atMs    操作发生时刻（Unix 毫秒），用作 takenAt / updatedAt
   * @returns {Promise<boolean>} 是否真的写入了一条记录。
   *          返回 false 的情况：occKey 无法解析、status 未在映射表中、缺少必要数据层接口。
   *          注意返回 false **不代表失败**——「用户只是点开通知」本来就不该写记录。
   */
  async function markStatus(opts) {
    opts = opts || {};
    var occKey = opts.occKey;
    var status = ACTION_TO_STATUS[opts.status];
    if (!status) return false;

    var parsed = parseOccKey(occKey);
    if (!parsed) return false;

    var data = MD.data;
    if (!data || !data.intakes || typeof data.intakes.list !== 'function') return false;
    if (typeof data.intakes.save !== 'function') return false;

    var atMs = num(opts.atMs, 0) || Date.now();

    // 已有记录优先：只增量改状态，保住 medicationId / profileId / ver 等历史字段。
    var existing = null;
    try {
      var list = await data.intakes.list();
      existing = indexBy(list, function (r) { return r && r.occKey; })[occKey] || null;
    } catch (e) {
      return false; // 数据层不可读时不猜测，交由上层重试
    }

    var rec = {};
    if (existing) {
      Object.keys(existing).forEach(function (k) { rec[k] = existing[k]; });
    } else {
      rec.id = uid();
      rec.occKey = occKey;
      rec.scheduledFor = parsed.scheduledFor;

      // 新记录缺 medicationId / profileId 会导致后续统计与同步丢失归属，
      // 因此必须从排程与药物表补出来；补不出来则宁可不写，避免产生孤儿记录。
      var filled = await resolveOwner(parsed.schedId);
      if (!filled) return false;
      rec.medicationId = filled.medicationId;
      rec.profileId = filled.profileId;
    }

    rec.status = status;
    rec.ver = (Number(rec.ver) || 0) + 1;
    rec.updatedAt = atMs;
    // 与 applyIntake 的语义一致：只有真正服用才记 takenAt，其余置 0。
    rec.takenAt = status === 'taken' ? atMs : 0;
    // 撤销为待服用需要打 undoneAt，否则统计会把这条自动归一成漏服。
    rec.undoneAt = status === 'pending' ? atMs : 0;
    if (status !== 'snoozed') rec.snoozeUntil = 0;
    // snoozed 时必须带上原生给出的 snoozeUntilMs，否则 schedule.js 读到 0，
    // 会把「稍后」立刻退化成 pending，用户点的「稍后」等于白点。
    // 时长由原生侧（10 分钟常量）给出，这里不自己推算。
    if (status === 'snoozed') rec.snoozeUntil = num(opts.snoozeUntilMs, 0);

    await data.intakes.save(rec);
    return true;
  }

  /**
   * 由排程 id 反查药物归属。occKey 里只有 schedId，
   * 必须落到 schedule → medication 两级才能拿到 medicationId 与 profileId。
   */
  async function resolveOwner(schedId) {
    var data = MD.data;
    if (!data || !data.schedules || typeof data.schedules.list !== 'function') return null;
    if (!data.meds || typeof data.meds.list !== 'function') return null;
    try {
      var schedules = await data.schedules.list();
      var sch = indexBy(schedules, function (s) { return s && s.id; })[schedId] || null;
      if (!sch) return null;
      var meds = await data.meds.list();
      var med = indexBy(meds, function (m) { return m && m.id; })[sch.medicationId] || null;
      if (!med) return null;
      return {
        medicationId: sch.medicationId,
        profileId: med.profileId || sch.profileId || ''
      };
    } catch (e) {
      return null;
    }
  }

  MD.record = {
    markStatus: markStatus,
    _parseOccKey: parseOccKey, // 供离线断言脚本验证编码解析
    _statusOf: function (action) { return ACTION_TO_STATUS[action] || null; }
  };
})(typeof window !== 'undefined' ? window : this);

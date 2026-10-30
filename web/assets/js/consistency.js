/* ==========================================================================
 * consistency.js —— 数据与人员（profile）的关联一致性维护
 *
 * 依赖（只读调用，不修改其文件）：
 *   data.js：MD.data.meds / schedules / intakes / profiles、MD.data.onChange
 *   sync.js：MD.sync.markDirty（写入后触发同步，由 data 层自动调用，这里不重复调）
 *
 * 【要解决的两个不一致】
 * 1) 药物改归属：把「布洛芬」从"本人"改到"母亲"，但它的排程与历史仍挂着旧 profileId，
 *    导致母亲的档案里看不到这个药，而本人档案里还残留。
 * 2) 药物被删除：排程还在，会继续产出「该服药了」的剂量，等于幽灵提醒。
 *
 * 【为什么做成独立模块而不是改 med.js】
 * med.js 已经 1857 行且是活跃修改区，往里塞级联逻辑会让「谁负责写」变得模糊。
 * 这里改为订阅 MD.data.onChange：任何来源的写入（界面、同步回放、导入备份）
 * 都会经过同一个事件，因此一致性规则只有一个实现点，且天然覆盖同步拉取到的数据。
 *
 * 【幂等性】
 * 所有写入都先比对当前值，相同则跳过，避免「写→事件→再写」的无限循环。
 * 本模块只对 table='medication' 动作，对 schedule/intake 的写入不触发自身。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var started = false;
  var running = false;
  var pending = false;

  function num(v, d) {
    var n = Number(v);
    return isFinite(n) ? n : d;
  }

  async function listSafe(coll, filter) {
    try {
      var list = await coll.list();
      return (list || []).filter(function (r) {
        return r && r.deleted !== true && (!filter || filter(r));
      });
    } catch (e) {
      return [];
    }
  }

  /**
   * 把某个药物的归属同步到它的排程与历史记录。
   * @returns {Promise<number>} 实际改动条数
   */
  async function propagateProfile(medId, profileId) {
    if (!medId || profileId == null || profileId === '') return 0;
    var changed = 0;

    var schedules = await listSafe(MD.data.schedules, function (s) { return s.medicationId === medId; });
    for (var i = 0; i < schedules.length; i++) {
      if (schedules[i].profileId === profileId) continue;
      schedules[i].profileId = profileId;
      schedules[i].updatedAt = Date.now();
      await MD.data.schedules.save(schedules[i]);
      changed++;
    }

    var intakes = await listSafe(MD.data.intakes, function (t) { return t.medicationId === medId; });
    for (var j = 0; j < intakes.length; j++) {
      if (intakes[j].profileId === profileId) continue;
      intakes[j].profileId = profileId;
      intakes[j].updatedAt = Date.now();
      await MD.data.intakes.save(intakes[j]);
      changed++;
    }
    return changed;
  }

  /**
   * 药物被删除时，把它的排程一并软删，避免继续产生待服剂量。
   * 历史（intake）保留：那是既成事实，删掉会让依从性统计失真。
   */
  async function retireSchedules(medId) {
    var schedules = await listSafe(MD.data.schedules, function (s) { return s.medicationId === medId; });
    var changed = 0;
    for (var i = 0; i < schedules.length; i++) {
      schedules[i].deleted = true;
      schedules[i].updatedAt = Date.now();
      await MD.data.schedules.save(schedules[i]);
      changed++;
    }
    return changed;
  }

  /** 人员档案被删除时，其名下药物一并软删（否则会成为无主数据） */
  async function retireMedsOfProfile(profileId) {
    if (profileId == null || profileId === '') return 0;
    var meds = await listSafe(MD.data.meds, function (m) { return m.profileId === profileId; });
    var changed = 0;
    for (var i = 0; i < meds.length; i++) {
      meds[i].deleted = true;
      meds[i].updatedAt = Date.now();
      await MD.data.meds.save(meds[i]);
      await retireSchedules(meds[i].id);
      changed++;
    }
    return changed;
  }

  /**
   * 彻底清空某档案名下的全部业务数据（「删档案 = 删这个人的数据」）。
   *
   * 【与 retireMedsOfProfile 的区别】
   * retireMedsOfProfile 只软删药物与排程，会留下该档案的用药历史（intake）成为孤儿；
   * 本函数把历史一并清掉，语义与用户在确认框里看到的「删除该档案的全部数据」一致。
   *
   * 【为什么不会误删别人的药】
   * medication.profileId 是**单值**字段，一条药只归属一个档案，"两个人吃同一种药"
   * 在数据上是两条独立记录。这里按 profileId 精确匹配，别人的记录（profileId 不同）
   * 与未归类药物（profileId 为空）都不会命中。
   *
   * 全部为软删（deleted=true + updatedAt），会进同步队列，其他设备能感知到，
   * 不是本地硬删。
   */
  async function purgeProfileData(profileId) {
    var out = { meds: 0, schedules: 0, intakes: 0 };
    if (profileId == null || profileId === '') return out;

    async function softDelete(table, items, counter) {
      for (var i = 0; i < items.length; i++) {
        if (!items[i] || items[i].deleted === true) continue;
        items[i].deleted = true;
        items[i].updatedAt = Date.now();
        await table.save(items[i]);
        out[counter]++;
      }
    }

    // 先清历史再清药物：历史是"发生过的事实"，先落软删能让中途失败时
    // 至少不留下"药没了但历史还在"的半截状态。
    await softDelete(MD.data.intakes,
      await listSafe(MD.data.intakes, function (x) { return x.profileId === profileId; }), 'intakes');
    await softDelete(MD.data.schedules,
      await listSafe(MD.data.schedules, function (x) { return x.profileId === profileId; }), 'schedules');
    await softDelete(MD.data.meds,
      await listSafe(MD.data.meds, function (x) { return x.profileId === profileId; }), 'meds');

    return out;
  }

  async function medById(id) {
    try {
      var list = await MD.data.meds.list();
      for (var i = 0; i < (list || []).length; i++) if (list[i] && list[i].id === id) return list[i];
    } catch (e) { /* 读不到就跳过 */ }
    return null;
  }

  /**
   * 处理一次数据变更事件。
   * 事件载荷形如 {table, id, action}，action ∈ 'save' | 'remove' | 'conflict'。
   */
  async function handle(evt) {
    if (!evt || evt.table !== 'medication' || !evt.id) return 0;

    if (evt.action === 'remove') return retireSchedules(evt.id);

    // save：把归属同步出去（幂等，值相同不会产生写入）
    var med = await medById(evt.id);
    if (!med || med.deleted === true) return retireSchedules(evt.id);
    return propagateProfile(med.id, med.profileId);
  }

  /** 串行执行：事件可能连发（批量导入），并发处理会重复写同一条 */
  async function drain() {
    if (running) { pending = true; return; }
    running = true;
    try {
      do {
        pending = false;
        await tick();
      } while (pending);
    } finally {
      running = false;
    }
  }

  var queue = [];
  async function tick() {
    while (queue.length) {
      var evt = queue.shift();
      try { await handle(evt); } catch (e) { /* 单条失败不影响后续 */ }
    }
  }

  /**
   * 全量体检：把历史上已经错位的数据修一遍。
   * 用于首次启用本模块、或同步拉回旧数据之后的兜底。
   * @returns {Promise<{meds:number, changed:number}>}
   */
  async function repairAll() {
    var meds = await listSafe(MD.data.meds);
    var changed = 0;
    for (var i = 0; i < meds.length; i++) {
      if (meds[i].profileId == null) continue;
      changed += await propagateProfile(meds[i].id, meds[i].profileId);
    }
    return { meds: meds.length, changed: changed };
  }

  function start() {
    if (started) return true;
    if (!MD.data || typeof MD.data.onChange !== 'function') return false;
    MD.data.onChange(function (evt) {
      queue.push(evt);
      // 用微任务排队，避免在 data 层写事务中途再次写库
      Promise.resolve().then(drain);
    });
    started = true;
    return true;
  }

  MD.consistency = {
    start: start,
    handle: handle,
    propagateProfile: propagateProfile,
    retireSchedules: retireSchedules,
    retireMedsOfProfile: retireMedsOfProfile,
    purgeProfileData: purgeProfileData,
    repairAll: repairAll,
    _isStarted: function () { return started; }
  };

  // 自启动：data.js 已先行加载，此处可直接接线。
  // 若因脚本顺序变化导致 data 层尚未就绪，退到下一轮事件循环再试一次，
  // 避免「脚本加载成功但监听没挂上」这种静默失效。
  if (!start()) {
    Promise.resolve().then(function () { start(); });
  }
})(typeof window !== 'undefined' ? window : this);

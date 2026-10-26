/* =============================================================================
 * MedDesk · remind.js（归属 A7）
 * 提醒调度：Notification API + 定时器。不直接操作业务 DOM，不读写 IndexedDB。
 *
 * 【关于「关键提醒穿透勿扰」的如实说明 —— 请务必阅读】
 * 1. Web 平台**无法真正突破系统级勿扰/静音**：Notification 是否出声、是否弹窗、
 *    最终由操作系统与浏览器的通知策略决定。本模块能为关键提醒做的最大努力是：
 *      · requireInteraction: true（通知不自动消失，需用户交互）
 *      · silent: false / renotify: true（尽量重新提示）
 *      · navigator.vibrate([200,120,200])（仅在支持振动的设备上生效）
 *      · 每 60 秒重复提醒，最多 3 次，直到被标记为「已服用/跳过」
 * 2. 若要让关键提醒真正突破勿扰，**必须由用户手动把本站/PWA 加入系统通知白名单**
 *    （iOS：设置 → 通知 → MedDesk → 允许通知 + 锁定屏幕/历史记录；
 *     Android：设置 → 应用 → MedDesk → 通知 → 允许 + 关闭勿扰豁免限制；
 *     macOS/Windows：系统通知中心中把浏览器/已安装 PWA 设为「重要/优先」）。
 *    本模块不做任何欺骗性行为，也不伪装系统通道。
 * 3. 未来真正可靠的穿透方案是 **Web Push（urgency=high + 服务端定时下发）**：
 *    页面关闭时本地定时器即失效，只有 Push 能到达。此处预留扩展点
 *    `MD.remind.registerPush(vapidPublicKey)`，待服务端支持后实现
 *    （Push 的 urgency=high 在部分平台可申请「时效性通知」配额）。
 *
 * 依赖（只按契约调用，不修改）：MD.util.fmtTime / MD.bus.emit / MD.haptic.warn。
 * 持久化**不在**本模块：所有动作通过 MD.bus 事件抛出，由 A8 落库。
 * ============================================================================= */
(function (global) {
  'use strict';

  var MD = global.MD || (global.MD = {});

  /* ---------------------------------------------------------------- 常量 */
  var TICK_MS = 1000;                    // 每秒校准一次
  var CRITICAL_REPEAT_MS = 60000;        // 关键提醒重复间隔
  var CRITICAL_MAX_TIMES = 3;            // 关键提醒最多提醒次数（含首次）
  var FIRE_WINDOW_MS = 6 * 3600 * 1000;  // 距触发点 6 小时内才补发，避免打开页面时轰炸历史提醒
  var DEFAULT_SNOOZE_MIN = 10;           // snooze() 未给分钟数时的默认推迟时长

  /* ---------------------------------------------------------------- 状态 */
  var state = { supported: false, permission: 'default', running: false };

  var doses = [];                        // 当前待提醒列表（DoseVM 副本）
  var doseMap = Object.create(null);     // occKey -> dose
  var fireLog = Object.create(null);     // occKey -> {count, nextAt, lastAt}
  var handled = Object.create(null);     // occKey -> true（已被 taken/skip 处理）
  var snoozeMap = Object.create(null);   // occKey -> 推迟到该时刻（ms）
  var fireCbs = [];
  var timers = [];                       // 所有 setTimeout id，stop() 时统一清理
  var tickTimer = null;                  // setInterval id
  var inited = false;

  /* ------------------------------------------------------------ 小工具 */
  function isArr(v) { return Object.prototype.toString.call(v) === '[object Array]'; }

  function nowMs() {
    var u = MD && MD.util;
    if (u && typeof u.nowMs === 'function') { return u.nowMs(); }
    return Date.now();
  }

  /** 时间格式化 HH:MM（本地时区），优先复用 A6 的 MD.util.fmtTime */
  function fmtTime(ms) {
    var u = MD && MD.util;
    if (u && typeof u.fmtTime === 'function') {
      try { return u.fmtTime(ms); } catch (e) { /* 降级 */ }
    }
    var d = new Date(ms);
    var hh = d.getHours(), mm = d.getMinutes();
    return (hh < 10 ? '0' : '') + hh + ':' + (mm < 10 ? '0' : '') + mm;
  }

  /** 安全的事件抛出：MD.bus 缺失时不抛错 */
  function emit(evt, payload) {
    try {
      if (MD && MD.bus && typeof MD.bus.emit === 'function') { MD.bus.emit(evt, payload); }
    } catch (e) { /* 静默降级 */ }
  }

  function hapticWarn() {
    try { if (MD && MD.haptic && typeof MD.haptic.warn === 'function') { MD.haptic.warn(); } } catch (e) { /* 静默 */ }
  }

  function vibrateStrong() {
    try {
      var nav = global.navigator;
      if (nav && typeof nav.vibrate === 'function') { nav.vibrate([200, 120, 200]); }
    } catch (e) { /* 静默 */ }
  }

  function normPerm(p) {
    p = String(p || '').toLowerCase();
    return (p === 'granted' || p === 'denied') ? p : 'default';
  }

  function refreshPermission() {
    state.supported = typeof global.Notification !== 'undefined';
    state.permission = state.supported ? normPerm(global.Notification.permission) : 'denied';
    return state.permission;
  }

  /* -------------------------------------------------------- 定时器管理 */
  function track(id) { timers.push(id); return id; }
  function untrack(id) {
    var i = timers.indexOf(id);
    if (i >= 0) { timers.splice(i, 1); }
  }
  function setTracked(fn, ms) {
    var id = global.setTimeout(function () { untrack(id); fn(); }, ms);
    return track(id);
  }
  function clearTracked() {
    for (var i = timers.length - 1; i >= 0; i--) {
      try { global.clearTimeout(timers[i]); } catch (e) { /* 静默 */ }
    }
    timers.length = 0;
  }

  /* ------------------------------------------------------ 通知构造/展示 */
  function buildOptions(dose, seq) {
    var critical = !!dose.critical;
    var body = (dose.medName || '药物') + (dose.dosageText ? ' ' + dose.dosageText : '') + ' · ' + fmtTime(dose.timeMs);
    if (seq > 0) { body += '（第 ' + (seq + 1) + ' 次提醒）'; }
    var opts = {
      body: body,
      tag: dose.occKey,                 // 同一 occKey 复用/替换同一条通知
      renotify: true,
      silent: false,
      requireInteraction: critical,     // 关键提醒不自动消失
      data: { occKey: dose.occKey, medId: dose.medId, schedId: dose.schedId, critical: critical },
      actions: [
        { action: 'taken', title: '已服用' },
        { action: 'skip', title: '跳过' }
      ]
    };
    // icon/badge 由 A6 的 manifest 提供，此处不硬编码任何资源路径
    return opts;
  }

  /** 尝试关闭某 occKey 的常驻通知（仅在 Service Worker 通道下可行） */
  function closeByTag(occKey) {
    try {
      var nav = global.navigator;
      if (!nav || !nav.serviceWorker) { return; }
      nav.serviceWorker.ready.then(function (reg) {
        if (!reg || typeof reg.getNotifications !== 'function') { return; }
        return reg.getNotifications({ tag: occKey });
      }).then(function (list) {
        if (!list) { return; }
        for (var i = 0; i < list.length; i++) {
          try { list[i].close(); } catch (e) { /* 静默 */ }
        }
      }).catch(function () { /* 静默 */ });
    } catch (e) { /* 静默 */ }
  }

  /**
   * 展示通知：优先 ServiceWorkerRegistration.showNotification（支持 actions 按钮），
   * 失败或不可用时回退到 new Notification + onclick 兜底。
   */
  function showNotification(dose, seq) {
    if (!state.supported) { return; }
    if (state.permission !== 'granted') { return; }
    var title = dose.critical ? '重要用药提醒' : '用药提醒';
    var opts = buildOptions(dose, seq || 0);
    var done = false;

    var fallback = function () {
      if (done) { return; }
      done = true;
      try {
        var n = new global.Notification(title, opts);
        // 兜底：无 actions 支持时，点击通知视为「已服用」
        n.onclick = function () {
          try { n.close(); } catch (e) { /* 静默 */ }
          try { if (global.window && typeof global.window.focus === 'function') { global.window.focus(); } } catch (e) { /* 静默 */ }
          emit('remind:action', { occKey: dose.occKey, action: 'taken' });
          markHandled(dose.occKey);
        };
      } catch (e) { /* 静默降级：不支持就什么都不做 */ }
    };

    try {
      var nav = global.navigator;
      if (nav && nav.serviceWorker && nav.serviceWorker.ready) {
        nav.serviceWorker.ready.then(function (reg) {
          if (!reg || typeof reg.showNotification !== 'function') { fallback(); return; }
          return reg.showNotification(title, opts).then(function () { done = true; }, fallback);
        }).catch(fallback);
        return;
      }
    } catch (e) { /* 落到 fallback */ }
    fallback();
  }

  /* ------------------------------------------------------------ 触发逻辑 */
  function markHandled(occKey) {
    if (!occKey) { return; }
    handled[occKey] = true;
    delete fireLog[occKey];
  }

  /** 某 dose 当前是否已经「无需再提醒」 */
  function isSettled(dose) {
    if (!dose) { return true; }
    if (handled[dose.occKey]) { return true; }
    var s = String(dose.status || '').toLowerCase();
    return s === 'taken' || s === 'skip' || s === 'skipped';
  }

  /** 触发时刻：timeMs 减去提前提醒分钟数 */
  function triggerAt(dose) {
    var off = Number(dose.remindOffsetMin);
    if (!isFinite(off)) { off = 0; }
    return dose.timeMs - off * 60000;
  }

  function fireOnce(dose, seq) {
    var log = fireLog[dose.occKey] || { count: 0, nextAt: 0, lastAt: 0 };
    log.count = seq + 1;
    log.lastAt = nowMs();
    log.nextAt = nowMs() + CRITICAL_REPEAT_MS;
    fireLog[dose.occKey] = log;

    showNotification(dose, seq);
    hapticWarn();
    if (dose.critical) { vibrateStrong(); }

    // 页面内回调（A8 可用于刷新界面/播放提示音）
    var cbs = fireCbs.slice();
    for (var i = 0; i < cbs.length; i++) {
      try { cbs[i](dose); } catch (e) { /* 单个回调失败不影响其他 */ }
    }
    emit('remind:fire', { dose: dose, seq: seq });
  }

  /** 单次校准：遍历待提醒列表，决定是否触发 */
  function tickOnce() {
    if (!state.running) { return; }
    var now = nowMs();
    for (var i = 0; i < doses.length; i++) {
      var dose = doses[i];
      if (!dose || !dose.occKey) { continue; }
      if (isSettled(dose)) { continue; }

      var base = triggerAt(dose);
      var until = Number(snoozeMap[dose.occKey] || 0);
      // 推迟期间不提醒；推迟到期后按 until 重新触发
      var dueAt = until > now ? until : Math.max(base, until);
      if (now < dueAt) { continue; }
      // 只补发「刚刚过去」的提醒，避免打开页面时轰炸几天前的历史
      if (!until && (now - base) > FIRE_WINDOW_MS) { continue; }

      var log = fireLog[dose.occKey];
      var seq = log ? log.count : 0;
      if (!dose.critical) {
        if (log) { continue; }               // 普通提醒只发一次
        fireOnce(dose, 0);
        continue;
      }
      // 关键提醒：每 60 秒重复，最多 CRITICAL_MAX_TIMES 次
      if (log && log.count >= CRITICAL_MAX_TIMES) { continue; }
      if (log && now < log.nextAt) { continue; }
      if (seq >= CRITICAL_MAX_TIMES) { continue; }
      fireOnce(dose, seq);
    }
  }

  /* -------------------------------------------------------------- 生命周期 */
  function bindBus() {
    try {
      if (MD && MD.bus && typeof MD.bus.on === 'function') {
        MD.bus.on('remind:action', function (p) {
          if (p && (p.action === 'taken' || p.action === 'skip')) { markHandled(p.occKey); }
        });
        // A8 若直接落库后广播数据变更，此处同步清理已完成的提醒
        MD.bus.on('data:changed', function () { /* 由 A8 重新调用 schedule(doses) 校准 */ });
      }
    } catch (e) { /* 静默 */ }
  }

  function bindVisibility() {
    try {
      var doc = global.document;
      if (!doc || typeof doc.addEventListener !== 'function') { return; }
      doc.addEventListener('visibilitychange', function () {
        // 页面恢复可见时重新校准（移动端定时器可能被节流/挂起）
        if (doc.visibilityState === 'visible') {
          refreshPermission();
          if (state.running) { tickOnce(); }
        }
      }, false);
    } catch (e) { /* 静默 */ }
  }

  /** 接收 Service Worker 转发的通知点击事件（A6 的 sw.js 可 postMessage 过来） */
  function bindSwMessage() {
    try {
      var nav = global.navigator;
      if (!nav || !nav.serviceWorker || typeof nav.serviceWorker.addEventListener !== 'function') { return; }
      nav.serviceWorker.addEventListener('message', function (ev) {
        var d = ev && ev.data;
        if (!d || !d.type) { return; }
        if (d.type === 'remind:action' && d.occKey) {
          emit('remind:action', { occKey: d.occKey, action: d.action || 'taken' });
          if (d.action === 'taken' || d.action === 'skip') { markHandled(d.occKey); }
        } else if (d.type === 'remind:snooze' && d.occKey) {
          emit('remind:snooze', { occKey: d.occKey, until: d.until || 0 });
        }
      });
    } catch (e) { /* 静默 */ }
  }

  /** 初始化：探测能力、绑定事件。可重复调用，幂等。 */
  function init() {
    if (inited) { return state; }
    inited = true;
    refreshPermission();
    bindBus();
    bindVisibility();
    bindSwMessage();
    return state;
  }

  /** 请求通知权限 -> Promise<'granted'|'denied'|'default'>；不支持时静默返回 'denied' */
  function ensurePermission() {
    init();
    if (!state.supported) {
      // 区分「不支持」与「iOS 未安装为 PWA」——两者提示文案不同：
      // iOS Safari 普通标签页里没有 Notification 对象，必须「添加到主屏幕」后从主屏打开才可用。
      var ua = String((global.navigator && global.navigator.userAgent) || '');
      var isIOS = /iPhone|iPad|iPod/i.test(ua);
      var isStandalone = false;
      try {
        isStandalone = (global.matchMedia && global.matchMedia('(display-mode: standalone)').matches) ||
          (global.navigator && global.navigator.standalone === true);
      } catch (e) { /* 忽略 */ }
      if (isIOS && !isStandalone) { return Promise.resolve('unsupported_pwa'); }
      return Promise.resolve('unsupported');
    }
    if (state.permission === 'granted') { return Promise.resolve('granted'); }
    return new Promise(function (resolve) {
      var settled = false;
      var done = function (p) {
        if (settled) { return; }
        settled = true;
        state.permission = normPerm(p);
        resolve(state.permission);
      };
      try {
        var ret = global.Notification.requestPermission(function (p) { done(p); });
        if (ret && typeof ret.then === 'function') {
          ret.then(function (p) { done(p); }, function () { done(state.permission); });
        }
      } catch (e) {
        done(state.permission);
      }
    });
  }

  /**
   * 设置待提醒列表（由 A8 调用，通常传 MD.sched.buildToday 的结果）。
   * 已 taken/skipped 的条目会被标记处理；重复调用只做增量校准。
   */
  function schedule(list) {
    init();
    var arr = isArr(list) ? list : [];
    doses = [];
    doseMap = Object.create(null);
    for (var i = 0; i < arr.length; i++) {
      var d = arr[i];
      if (!d || !d.occKey) { continue; }
      if (isSettled(d)) { markHandled(d.occKey); continue; }
      doses.push(d);
      doseMap[d.occKey] = d;
    }
    // 丢弃已不在列表中的触发记录
    var next = Object.create(null);
    for (var k in fireLog) {
      if (Object.prototype.hasOwnProperty.call(fireLog, k) && doseMap[k]) { next[k] = fireLog[k]; }
    }
    fireLog = next;
    // 新列表可能已到期，先校准一次；
    // A8 只会调用 schedule()，因此未启动时惰性自启（要暂停请显式调用 stop()）。
    if (state.running) { tickOnce(); } else { start(); }
  }

  /** 推迟提醒：更新内部计时器并广播 remind:snooze；持久化由 A8 完成 */
  function snooze(occKey, minutes) {
    init();
    if (!occKey) { return; }
    var m = Number(minutes);
    if (!isFinite(m) || m <= 0) { m = DEFAULT_SNOOZE_MIN; }
    var until = nowMs() + m * 60000;
    snoozeMap[occKey] = until;
    // 推迟后重置关键提醒的重复计数，到期后可再次重复
    if (fireLog[occKey]) { fireLog[occKey].nextAt = until; fireLog[occKey].count = 0; }
    delete handled[occKey];
    closeByTag(occKey);
    setTracked(function () { if (state.running) { tickOnce(); } }, Math.max(1000, until - nowMs()));
    emit('remind:snooze', { occKey: occKey, until: until });
  }

  /** 注册提醒触发回调 cb(dose)；返回取消注册函数 */
  function onFire(cb) {
    if (typeof cb !== 'function') { return function () { /* noop */ }; }
    fireCbs.push(cb);
    return function () {
      var i = fireCbs.indexOf(cb);
      if (i >= 0) { fireCbs.splice(i, 1); }
    };
  }

  /** 启动定时器 */
  function start() {
    init();
    if (state.running) { return state; }
    state.running = true;
    try {
      tickTimer = global.setInterval(function () {
        try { tickOnce(); } catch (e) { /* 单次异常不能中断定时器 */ }
      }, TICK_MS);
    } catch (e) { /* 静默 */ }
    tickOnce();
    return state;
  }

  /** 停止并清理所有定时器（不清理已发出的系统通知） */
  function stop() {
    state.running = false;
    if (tickTimer !== null && tickTimer !== undefined) {
      try { global.clearInterval(tickTimer); } catch (e) { /* 静默 */ }
      tickTimer = null;
    }
    clearTracked();
    return state;
  }

  /**
   * 预留扩展点：注册 Web Push（服务端定时下发 + urgency=high 才能真正穿透勿扰）。
   * 当前版本未实现服务端推送通道，固定返回 rejected Promise。
   */
  function registerPush(vapidPublicKey) {
    return Promise.reject(new Error('not_implemented'));
  }

  /* ---------------------------------------------------------------- 导出 */
  // 加载即探测一次能力，保证未调用 init() 时 state 也是准确的
  try { refreshPermission(); } catch (e) { /* 静默 */ }

  MD.remind = {
    init: init,
    start: start,
    stop: stop,
    ensurePermission: ensurePermission,
    schedule: schedule,
    snooze: snooze,
    onFire: onFire,
    registerPush: registerPush,
    state: state
  };

  if (typeof module !== 'undefined' && module.exports) { module.exports = MD.remind; }
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this));

/* =============================================================================
 * MedDesk · native-alarm.js（归属 Contract B）
 * Web 侧桥接：把「今天待服用的剂量」注册为原生闹钟，并把离线期间的通知操作写回服药记录。
 *
 * 依赖（只按契约调用，不修改）：
 *   MD.sched.buildToday({meds, schedules, intakes, profileId, nowMs}) -> DoseVM[]
 *   MD.record.markStatus({occKey, status, atMs, snoozeUntilMs})           由集成方注入
 *   window.Capacitor.Plugins.MeddeskAlarm                              由 Contract A 提供
 *
 * 用到的插件方法与用途（契约 1.3）：
 *   schedule({doses})   把今日待服剂量注册成系统闹钟（同 occKey 幂等覆盖）
 *   cancelAll()         先清后注册，避免已服用的残留闹钟继续响
 *   drainActions()      拉取并清空离线期间的通知操作（取走即清空）
 *   status()            查询通知权限与系统状态（纯查询，不申请权限）
 *   requestPermissions() 申请通知权限（Android 13+ 的 POST_NOTIFICATIONS）
 *   openNotificationSettings() 跳系统通知设置页，引导用户手动授权
 *   openBatterySettings()      跳电池优化豁免页，国产 ROM 保活的关键一步
 *
 * 命名映射（务必保留，避免后人找不到）：
 *   Kotlin 侧方法名是 requestPermissions（复数）；
 *   本模块对外统一暴露 requestPermission（单数），内部再转发到复数版本。
 *
 * 设计原则：本模块在普通浏览器里必须是「无害的空转」——
 * 所有原生能力都用 isNative() 守卫，缺失时返回零值 Promise，绝不把异常抛给 UI 层。
 * ============================================================================= */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  // 与 Contract A 的 Capacitor plugin id 一一对应，改名必须两边同时改。
  var PLUGIN_ID = 'MeddeskAlarm';

  // 只有「尚未被处理」的剂量才需要系统闹钟：
  // taken / skipped 已完成；snoozed 由 remind.js 的页面内计时器接管，
  // 再注册一条原生闹钟会让用户在「稍后提醒」之外额外收到重复通知。
  var ALARM_STATUS = { pending: true, missed: true };

  // 只归一「同义写法」，不替集成方做语义决策：
  // opened（用户只是点开通知）原样透传，是否写记录由 MD.record.markStatus 内部决定。
  var ACTION_STATUS = {
    taken: 'taken',
    skip: 'skipped',
    skipped: 'skipped',
    snooze: 'snoozed',
    snoozed: 'snoozed',
    opened: 'opened'
  };

  function pad2(value) { return (value < 10 ? '0' : '') + value; }

  function isArray(value) {
    return Object.prototype.toString.call(value) === '[object Array]';
  }

  /** 时间基准统一走 core.js，避免两个时钟口径不一致导致漏注册 */
  function nowMs() {
    return (MD.util && typeof MD.util.nowMs === 'function') ? MD.util.nowMs() : Date.now();
  }

  function num(value, fallback) {
    var parsed = Number(value);
    return isFinite(parsed) ? parsed : fallback;
  }

  /**
   * 毫秒时间戳 -> 'HH:mm'。
   * 优先复用 MD.util.fmtTime 保证与界面文案一致；core.js 尚未加载时回落内建实现，
   * 这样即使本文件被排在 core.js 之前也不会报错。
   */
  function hhmm(ms) {
    if (MD.util && typeof MD.util.fmtTime === 'function') return MD.util.fmtTime(ms);
    var date = new Date(ms);
    return pad2(date.getHours()) + ':' + pad2(date.getMinutes());
  }

  /** 只用 debug 级：非原生环境是常态，不能污染控制台（契约 2.3-1） */
  function log(message, error) {
    if (typeof console === 'undefined' || typeof console.debug !== 'function') return;
    try {
      console.debug('[MD.nativeAlarm] ' + message, error === undefined ? '' : error);
    } catch (_) { /* 控制台被禁用时忽略 */ }
  }

  /* ------------------------------------------------------------ 插件访问 */

  /**
   * 取原始 Capacitor 插件对象，取不到一律返回 null。
   * 只读 window.Capacitor，不 require/import 任何 npm 包（本项目无构建步骤）。
   */
  function plugins() {
    try {
      var capacitor = window.Capacitor;
      if (!capacitor || !capacitor.Plugins) return null;
      var plugin = capacitor.Plugins[PLUGIN_ID];
      return (plugin && typeof plugin === 'object') ? plugin : null;
    } catch (error) {
      log('读取插件失败', error);
      return null;
    }
  }

  /**
   * 是否处于原生容器。
   * 光有插件对象还不够：@capacitor/core 在浏览器里同样会注册插件对象（web 实现），
   * 因此必须再看平台判定，否则浏览器会被误判为原生并触发无意义的桥调用。
   */
  function isNative() {
    var plugin = plugins();
    if (!plugin) return false;
    try {
      var capacitor = window.Capacitor;
      if (typeof capacitor.isNativePlatform === 'function') return !!capacitor.isNativePlatform();
      var platform = typeof capacitor.getPlatform === 'function' ? String(capacitor.getPlatform() || '') : '';
      return !!platform && platform !== 'web';
    } catch (error) {
      log('判定原生环境失败', error);
      return false;
    }
  }

  /* ------------------------------------------------------------ 数据收集 */

  /**
   * 从 MD.data 读取集合并交给 MD.sched.buildToday 计算今日剂量。
   * 这里只做只读访问，任何写回都必须经由 MD.record.markStatus（契约 2.3-4）。
   * @param {Object} opts
   * @returns {Promise<Array>} DoseVM[]；数据层不可用时返回空数组
   */
  function collectFromData(opts) {
    var data = MD.data;
    if (!data || !data.meds || typeof data.meds.list !== 'function') return Promise.resolve([]);
    if (!data.schedules || !data.intakes) return Promise.resolve([]);
    // data.js 的集合接口是异步的，因此这里必须并行取回后再计算。
    return Promise.all([data.meds.list(), data.schedules.list(), data.intakes.list()])
      .then(function (lists) {
        return MD.sched.buildToday({
          meds: isArray(lists[0]) ? lists[0] : [],
          schedules: isArray(lists[1]) ? lists[1] : [],
          intakes: isArray(lists[2]) ? lists[2] : [],
          profileId: opts.profileId === undefined ? null : opts.profileId,
          nowMs: num(opts.nowMs, nowMs())
        }) || [];
      });
  }

  /**
   * 取得待处理的剂量条目。
   * 优先级：显式传入的 doses（便于集成方复用已算好的 VM）> collect 回调 > 默认 MD.data。
   * @param {Object} opts
   * @returns {Promise<Array>}
   */
  function resolveEntries(opts) {
    try {
      if (isArray(opts.doses)) return Promise.resolve(opts.doses);
      if (typeof opts.collect === 'function') return Promise.resolve(opts.collect());
      if (!MD.sched || typeof MD.sched.buildToday !== 'function') return Promise.resolve([]);
      return collectFromData(opts);
    } catch (error) {
      log('收集剂量失败', error);
      return Promise.resolve([]);
    }
  }

  /* ------------------------------------------------------------ 转换 */

  /**
   * DoseVM -> DoseAlarm。
   * occKey / atMs 是原生侧必填项，缺失的条目直接丢弃（由调用方计入 skipped）。
   * @param {Object} entry buildToday 返回的一条剂量
   * @returns {Object|null}
   */
  function toAlarm(entry) {
    if (!entry) return null;
    var occKey = String(entry.occKey || '');
    var atMs = num(entry.timeMs, NaN);
    if (!occKey || !isFinite(atMs) || atMs <= 0) return null;
    var name = String(entry.medName || '').trim();
    var dosage = String(entry.dosageText || '').trim();
    return {
      occKey: occKey,
      atMs: atMs,
      title: (name || '用药') + ' 该服药了',
      // 文案格式由契约 2.3-3 固定；剂量缺失时用兜底词，避免出现「· 08:00」这种半截文案
      body: (dosage || '按医嘱服用') + ' · ' + hhmm(atMs)
    };
  }

  /**
   * 筛出需要注册闹钟的剂量。
   * @param {Array} entries buildToday 结果
   * @param {number} now 当前毫秒
   * @returns {{doses: Array, skipped: number}}
   */
  function selectAlarms(entries, now) {
    var doses = [];
    var skipped = 0;
    if (!isArray(entries)) return { doses: doses, skipped: skipped };
    for (var i = 0; i < entries.length; i++) {
      var entry = entries[i];
      if (!entry) { skipped++; continue; }
      var status = String(entry.status || '').toLowerCase();
      if (!ALARM_STATUS[status]) { skipped++; continue; }
      var alarm = toAlarm(entry);
      if (!alarm) { skipped++; continue; }
      // 原生层规定 atMs 早于当前时间的剂量会被丢弃并计入 failed。
      // 与其发起一批注定失败的桥调用、把 failed 数字搞得毫无参考价值，
      // 不如在本层就跳过并计入 skipped（missed 项几乎总是落在这里）。
      if (alarm.atMs <= now) { skipped++; continue; }
      doses.push(alarm);
    }
    return { doses: doses, skipped: skipped };
  }

  /* ------------------------------------------------------------ 结果构造 */

  /** 零值结果：保证四个字段恒定存在，调用方可以无脑解构 */
  function zeroResult(reason) {
    return { scheduled: 0, skipped: 0, failed: [], reason: reason || null };
  }

  /**
   * 取注册失败的 occKey 列表。
   * 与契约 1.3 保持同一口径：failed 是字符串数组（元素为 occKey，取不到时为 '#0' 这类下标占位），
   * 容忍插件回传 null / 非数组。
   */
  function failedKeys(result) {
    if (!result || !isArray(result.failed)) return [];
    return result.failed.slice();
  }

  /* ------------------------------------------------------------ 对外方法 */

  /**
   * 同步今日闹钟。
   * @param {Object} [opts] {doses, collect, profileId, nowMs}
   * @returns {Promise<{scheduled:number, skipped:number, failed:string[], reason:string|null}>}
   *          failed 为注册失败的 occKey 列表（与契约 1.3 同口径）；reason 为 null 表示正常完成。
   */
  function syncAlarms(opts) {
    opts = opts || {};
    var plugin = plugins();
    if (!isNative() || !plugin || typeof plugin.schedule !== 'function') {
      // 非原生环境静默降级（契约 2.3-1）；插件缺失同样按零值处理，不打扰用户。
      return Promise.resolve(zeroResult(plugin ? 'schedule-unavailable' : 'plugin-missing'));
    }
    return resolveEntries(opts).then(function (entries) {
      var selected = selectAlarms(entries, num(opts.nowMs, nowMs()));
      // 先清后注册：上一轮注册过的剂量可能已被标记为已服用/已跳过，
      // 若不清掉，系统闹钟仍会准时响（契约 1.3 的 schedule 只覆盖同 occKey，不处理撤销）。
      return clearAll(plugin).then(function () {
        if (!selected.doses.length) return { scheduled: 0, failed: [] };
        return plugin.schedule({ doses: selected.doses });
      }).then(function (result) {
        return {
          scheduled: num(result && result.scheduled, 0),
          skipped: selected.skipped,
          failed: failedKeys(result),
          reason: null
        };
      });
    }).catch(function (error) {
      log('同步闹钟失败，降级为零值', error);
      return zeroResult('sync-failed');
    });
  }

  /**
   * 取消全部已注册闹钟。清不掉也不算致命错误：下一轮 syncAlarms 会重新覆盖。
   * @param {Object} plugin
   * @returns {Promise<*>}
   */
  function clearAll(plugin) {
    if (!plugin || typeof plugin.cancelAll !== 'function') return Promise.resolve(null);
    return Promise.resolve(plugin.cancelAll()).catch(function (error) {
      log('取消闹钟失败，继续尝试注册', error);
      return null;
    });
  }

  /** DoseAction.action -> 服药状态别名归一；契约外的值返回 null */
  function actionStatus(action) {
    return ACTION_STATUS[String(action || '').toLowerCase()] || null;
  }

  /**
   * 调用集成方注入的写回接口，并把它统一收敛成 Promise<boolean>。
   * 之所以包一层：集成方既可能写同步实现（直接 return false）也可能写 async 实现，
   * 直接判 `=== false` 会在 async 版本上永远不成立，导致「拒绝写入」被误计为成功。
   * @returns {Promise<boolean>} true 表示确实写了一条记录
   */
  function applyAction(item) {
    try {
      return Promise.resolve(MD.record.markStatus({
        occKey: String(item.occKey),
        status: actionStatus(item.action),
        atMs: num(item.atMs, nowMs()),
        // 透传原生算好的「稍后」时刻，本层不参与计算：
        // 原生在点「稍后」的当下已按该时间重排同一 occKey 的闹钟，
        // B 侧若再自行 schedule() 一次就会重复提醒；
        // 而交给 MD.record 落库后，schedule.js 才能用 snoozeUntil > nowMs 判定仍是 snoozed 而非退回 pending。
        snoozeUntilMs: num(item.snoozeUntilMs, 0)
      })).then(function (result) {
        return result !== false;
      });
    } catch (error) {
      // 同步实现直接 throw 时转成 reject，交给调用方统一「跳过该条」处理，
      // 否则一次同步异常会中断整条串行链，后面的操作全部丢失。
      return Promise.reject(error);
    }
  }

  /**
   * 取回离线期间暂存的服药操作并写回记录，返回成功处理的条数。
   * 写回接口由集成方注入（MD.record.markStatus），缺失时静默返回 0（契约 2.3-4）。
   * 注意：这里不会为 snoozed 重新注册闹钟——原生侧在点「稍后」的当下已按其内部常量
   * 重排过同一 occKey，B 侧再调 schedule() 只会造成双重提醒。
   * @returns {Promise<number>}
   */
  function drainAndApply() {
    var plugin = plugins();
    if (!isNative() || !plugin || typeof plugin.drainActions !== 'function') return Promise.resolve(0);
    if (!(MD.record && typeof MD.record.markStatus === 'function')) {
      log('MD.record.markStatus 未注入，跳过离线操作写回');
      return Promise.resolve(0);
    }
    return Promise.resolve(plugin.drainActions()).then(function (result) {
      var actions = (result && isArray(result.actions)) ? result.actions : [];
      var applied = 0;
      // 逐条串行处理：既保证写回顺序与操作发生顺序一致，也避免并发写同一条记录。
      var chain = Promise.resolve();
      actions.forEach(function (item) {
        if (!item || !item.occKey || !actionStatus(item.action)) return;
        chain = chain.then(function () {
          return applyAction(item).then(function (ok) {
            if (ok) applied++; // 集成方返回 false 表示拒绝该条（如 occKey 已失效）
          }).catch(function (error) {
            // 单条写回失败不能影响其余条目的应用。
            log('离线操作写回失败', error);
          });
        });
      });
      return chain.then(function () { return applied; });
    }).catch(function (error) {
      log('拉取离线操作失败，降级为 0', error);
      return 0;
    });
  }

  /**
   * 把原生回传的权限对象收敛成 AlarmStatus（与 definitions.ts 一致的三字段，全为布尔）。
   * 原生端可能回传 0/1 或字符串，直接透传会让调用方的判断失效。
   * @param {*} value
   * @returns {{notifications:boolean, exactAlarm:boolean, ignoringBattery:boolean}|null}
   */
  function normalizeStatus(value) {
    if (!value || typeof value !== 'object') return null;
    return {
      notifications: !!value.notifications,
      exactAlarm: !!value.exactAlarm,
      ignoringBattery: !!value.ignoringBattery
    };
  }

  /**
   * 查询通知权限与系统状态。失败或非原生返回 null。
   * @returns {Promise<{notifications:boolean, exactAlarm:boolean, ignoringBattery:boolean}|null>}
   */
  function statusAsync() {
    var plugin = plugins();
    if (!isNative() || !plugin || typeof plugin.status !== 'function') return Promise.resolve(null);
    return Promise.resolve(plugin.status()).then(normalizeStatus).catch(function (error) {
      log('查询闹钟状态失败', error);
      return null;
    });
  }

  /**
   * 申请通知权限（Android 13+ 的 POST_NOTIFICATIONS）。
   * 转发到插件的 requestPermissions()（复数）——命名映射见文件头。
   * 非原生、插件不支持或失败时返回 null，不抛异常。
   * @returns {Promise<{notifications:boolean, exactAlarm:boolean, ignoringBattery:boolean}|null>}
   */
  function requestPermission() {
    var plugin = plugins();
    if (!isNative() || !plugin || typeof plugin.requestPermissions !== 'function') return Promise.resolve(null);
    return Promise.resolve(plugin.requestPermissions()).then(normalizeStatus).catch(function (error) {
      log('申请通知权限失败', error);
      return null;
    });
  }

  /**
   * 调系统设置页。这类跳转可能因为 ROM 定制而失败（没有对应 Activity），
   * 所以失败只记 debug，不向上传播。
   * @param {string} method 插件方法名
   * @param {string} label  用于日志的可读名
   * @returns {Promise<void>}
   */
  function openSettings(method, label) {
    var plugin = plugins();
    if (!isNative() || !plugin || typeof plugin[method] !== 'function') return Promise.resolve();
    return Promise.resolve(plugin[method]()).catch(function (error) {
      log(label + '失败', error);
    });
  }

  function openNotificationSettings() {
    return openSettings('openNotificationSettings', '打开通知设置页');
  }

  function openBatterySettings() {
    return openSettings('openBatterySettings', '打开电池优化设置页');
  }

  /* ------------------------------------------------------------ 导出 */

  MD.nativeAlarm = {
    isNative: isNative,
    syncAlarms: syncAlarms,
    drainAndApply: drainAndApply,
    statusAsync: statusAsync,
    requestPermission: requestPermission,
    openNotificationSettings: openNotificationSettings,
    openBatterySettings: openBatterySettings,
    plugins: plugins
  };
})(typeof window !== 'undefined' ? window : this);

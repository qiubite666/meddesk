(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var STATES = ['idle', 'syncing', 'offline', 'error'];
  var state = 'idle';
  var running = null;
  var timer = null;
  var autoMs = 60000;
  var eventsBound = false;
  var dirtyTimer = null;

  function setState(next) {
    if (STATES.indexOf(next) < 0) return;
    state = next;
    MD.sync.state = next;
    renderIndicator();
    MD.bus.emit('sync:state', next);
  }

  function renderIndicator() {
    var host = window.document && window.document.getElementById('sync-indicator');
    if (!host) return;
    var labels = { idle: '已同步', syncing: '同步中', offline: '离线', error: '失败' };
    MD.ui.clear(host);
    host.appendChild(MD.ui.el('button', {
      class: 'sync-status',
      type: 'button',
      dataset: { state: state },
      title: state === 'error' ? '同步失败，点击重试' : labels[state],
      onClick: function () { if (state !== 'syncing') MD.sync.run(); }
    }, [
      MD.ui.el('span', { class: 'sync-status__dot' }),
      MD.ui.el('span', { text: labels[state] })
    ]));
  }

  function envelopeTable(item) {
    return item.table || item.tableName || item.table_name;
  }

  function envelopeId(item) {
    return item.id || item.recordId || item.record_id;
  }

  function itemIV(item) {
    return item.ivB64 || item.iv || '';
  }

  function itemCT(item) {
    return item.ctB64 || item.ct || '';
  }

  function better(incoming, current) {
    if (!current) return true;
    var incomingVer = Number(incoming.ver) || 0;
    var currentVer = Number(current.ver) || 0;
    var incomingAt = Number(incoming.updatedAt) || 0;
    var currentAt = Number(current.updatedAt) || 0;
    return incomingVer > currentVer ||
      (incomingVer === currentVer && incomingAt > currentAt) ||
      (incomingVer === currentVer && incomingAt === currentAt && Boolean(incoming.deleted) && !current.deleted);
  }

  function syncRecord(record) {
    var clean = Object.assign({}, record);
    delete clean._dirty;
    delete clean._example;
    return clean;
  }

  async function deviceId() {
    var id = await MD.db.metaGet('sync.deviceId');
    if (!id) {
      id = MD.util.uuid();
      await MD.db.metaSet('sync.deviceId', id);
    }
    return id;
  }

  async function dirtyRecords() {
    var records = [];
    for (var index = 0; index < MD.data._tables.length; index += 1) {
      var table = MD.data._tables[index];
      var items = await MD.db.list(table);
      items.forEach(function (record) {
        if (record._dirty) records.push({ table: table, record: record });
      });
    }
    return records;
  }

  async function encryptedItem(table, record, dek, localDeviceId) {
    var encrypted = await MD.crypto.encrypt(dek, JSON.stringify(syncRecord(record)));
    return {
      table: table,
      id: record.id,
      ver: Number(record.ver) || 0,
      updatedAt: Number(record.updatedAt) || 0,
      deleted: Boolean(record.deleted),
      iv: encrypted.ivB64,
      ct: encrypted.ctB64,
      deviceId: localDeviceId
    };
  }

  async function decryptItem(item, dek) {
    var plain = await MD.crypto.decrypt(dek, itemIV(item), itemCT(item));
    var record = JSON.parse(plain);
    record.id = envelopeId(item) || record.id;
    record.ver = Number(item.ver == null ? record.ver : item.ver) || 0;
    record.updatedAt = Number(item.updatedAt == null ? record.updatedAt : item.updatedAt) || 0;
    record.deleted = Boolean(item.deleted == null ? record.deleted : item.deleted);
    return record;
  }

  function itemKey(item) {
    return envelopeTable(item) + ':' + envelopeId(item);
  }

  async function clearApplied(applied, sent) {
    var appliedKeys = Object.create(null);
    // 服务端 SyncPushResponse.applied 是 []string（"table:id"），不是对象数组。
    // 按对象解析会生成 "undefined:undefined"，一条都匹配不上，
    // _dirty 永远清不掉 → 每轮重推 → 多设备 ver 军备竞赛互相覆盖（改名丢失的根源）。
    (applied || []).forEach(function (item) {
      if (typeof item === 'string') { appliedKeys[item] = true; return; }
      appliedKeys[itemKey(item)] = true;
    });
    if (!applied || !applied.length) {
      var conflictKeys = Object.create(null);
      sent.conflicts.forEach(function (item) { conflictKeys[itemKey(item)] = true; });
      sent.items.forEach(function (item) {
        if (!conflictKeys[itemKey(item)]) appliedKeys[itemKey(item)] = true;
      });
    }
    for (var index = 0; index < sent.locals.length; index += 1) {
      var local = sent.locals[index];
      if (!appliedKeys[local.table + ':' + local.record.id]) continue;
      var current = await MD.db.get(local.table, local.record.id);
      if (current && current._dirty && current.ver === local.record.ver && current.updatedAt === local.record.updatedAt) {
        current._dirty = false;
        await MD.db.put(local.table, current);
      }
    }
  }

  async function adoptServer(item, dek, notify) {
    var table = envelopeTable(item);
    var id = envelopeId(item);
    if (MD.data._tables.indexOf(table) < 0 || !id) return false;
    var remote = await decryptItem(item, dek);
    var local = await MD.db.get(table, id);
    // 墓碑保护：本地已删（墓碑干净），远端是活记录且并不严格更新 → 保持删除。
    // 没有这条，平局分支会用远端活记录覆盖墓碑，被删的行随同步"复活"。
    if (local && local.deleted === true && !remote.deleted && !better(remote, local)) return false;
    if (!local || better(remote, local) || (!local._dirty && !better(local, remote))) {
      remote._dirty = false;
      await MD.db.put(table, remote);
      if (notify) MD.toast.show('该记录已在其他设备更新', 'info');
      MD.bus.emit('data:changed', { table: table, id: id, action: 'sync' });
      return true;
    }
    return false;
  }

  async function resolveConflicts(conflicts, dek, localDeviceId) {
    var retryItems = [];
    var retryLocals = [];
    for (var index = 0; index < conflicts.length; index += 1) {
      var item = conflicts[index];
      var table = envelopeTable(item);
      var id = envelopeId(item);
      if (MD.data._tables.indexOf(table) < 0 || !id) continue;
      var remote = await decryptItem(item, dek);
      var local = await MD.db.get(table, id);

      // 本地有未推送的改动，且改动时间不早于远端 → 以本地为准。
      //
      // 【为什么不能只比 ver】
      // 服务端只按 ver 裁决（in.Ver > cur.Ver）。而故障期（v0.3.26 及以前）的
      // 冲突重试会把同一条记录的 ver 一路抬高，实测能到几千。之后用户改名拿到的是
      // base+500，仍然低于那条陈旧副本 → 推送冲突 → 远端"胜出" → 本地被覆盖回旧值，
      // 用户看到的就是"改了又变回去"。这里把本地 ver 直接抬到「远端 ver + 1」再推一次，
      // 用最新一次真实编辑覆盖陈旧副本；只抬一次，重试若仍冲突则按远端采纳，不会来回拉锯。
      if (local && local._dirty && !remote.deleted && better(remote, local)
          && (Number(local.updatedAt) || 0) >= (Number(remote.updatedAt) || 0)) {
        local.ver = (Number(remote.ver) || 0) + 1;
        local.updatedAt = MD.util.nowMs();
        await MD.db.put(table, local);
        retryItems.push(await encryptedItem(table, local, dek, localDeviceId));
        retryLocals.push({ table: table, record: local });
        continue;
      }

      if (!local || better(remote, local)) {
        remote._dirty = false;
        await MD.db.put(table, remote);
        MD.toast.show('该记录已在其他设备更新', 'info');
        MD.bus.emit('data:changed', { table: table, id: id, action: 'conflict' });
      } else if (local._dirty) {
        // 平局（ver/updatedAt 完全相等）：服务器拒绝等 ver 推送，原样重试永远冲突，
        // 脏标记永远清不掉 → 每分钟重推同样的字节（实测死循环）。
        // 让本地胜出：ver+1 落库后再推，服务器必然接受，一轮收敛。
        if (!better(remote, local) && !better(local, remote)) {
          local.ver = (Number(local.ver) || 0) + 1;
          local.updatedAt = MD.util.nowMs();
          await MD.db.put(table, local);
        }
        retryItems.push(await encryptedItem(table, local, dek, localDeviceId));
        retryLocals.push({ table: table, record: local });
      }
    }
    if (!retryItems.length) return;
    var response = await MD.api.push(retryItems);
    var retryConflicts = response.conflicts || [];
    await clearApplied(response.applied || [], { items: retryItems, locals: retryLocals, conflicts: retryConflicts });
    for (var retryIndex = 0; retryIndex < retryConflicts.length; retryIndex += 1) {
      await adoptServer(retryConflicts[retryIndex], dek, true);
    }
  }

  async function pushDirty(dek) {
    var dirty = await dirtyRecords();
    if (!dirty.length) return { count: 0, conflicts: 0 };
    var localDeviceId = await deviceId();
    var conflictCount = 0;
    for (var offset = 0; offset < dirty.length; offset += 500) {
      var locals = dirty.slice(offset, offset + 500);
      var items = [];
      for (var index = 0; index < locals.length; index += 1) {
        items.push(await encryptedItem(locals[index].table, locals[index].record, dek, localDeviceId));
      }
      var response = await MD.api.push(items);
      var conflicts = response.conflicts || [];
      conflictCount += conflicts.length;
      await clearApplied(response.applied || [], { items: items, locals: locals, conflicts: conflicts });
      await resolveConflicts(conflicts, dek, localDeviceId);
    }
    return { count: dirty.length, conflicts: conflictCount };
  }

  async function pullRemote(dek) {
    var cursor = Number(await MD.db.metaGet('sync.cursor')) || 0;
    var pulled = 0;
    var pages = 0;
    var hasMore = true;
    while (hasMore && pages < 100) {
      var response = await MD.api.pull(cursor, 300);
      var items = response.items || response.changes || [];
      for (var index = 0; index < items.length; index += 1) {
        if (await adoptServer(items[index], dek, false)) pulled += 1;
      }
      var nextCursor = Number(response.cursor == null ? cursor : response.cursor) || cursor;
      hasMore = Boolean(response.hasMore || response.has_more) && nextCursor > cursor;
      cursor = nextCursor;
      await MD.db.metaSet('sync.cursor', cursor);
      pages += 1;
    }
    return pulled;
  }

  async function runSync() {
    if (window.navigator && window.navigator.onLine === false) {
      setState('offline');
      return { pushed: 0, pulled: 0, conflicts: 0 };
    }
    var dek = MD.api.getDEK();
    if (!MD.api.hasSession() || !dek) {
      setState('idle');
      return { pushed: 0, pulled: 0, conflicts: 0 };
    }
    setState('syncing');
    try {
      var pushResult = await pushDirty(dek);
      var pulled = await pullRemote(dek);
      setState('idle');
      return { pushed: pushResult.count, pulled: pulled, conflicts: pushResult.conflicts };
    } catch (error) {
      setState(error.network || (window.navigator && window.navigator.onLine === false) ? 'offline' : 'error');
      throw error;
    }
  }

  function onOnline() {
    setState('idle');
    MD.sync.run().catch(function () { /* 状态机已记录失败。 */ });
  }

  function onOffline() { setState('offline'); }

  function onVisibility() {
    if (window.document.visibilityState === 'visible') MD.sync.run().catch(function () { /* 状态机已记录失败。 */ });
  }

  function bindEvents() {
    if (eventsBound) return;
    window.addEventListener('online', onOnline);
    window.addEventListener('offline', onOffline);
    window.document.addEventListener('visibilitychange', onVisibility);
    eventsBound = true;
  }

  function unbindEvents() {
    if (!eventsBound) return;
    window.removeEventListener('online', onOnline);
    window.removeEventListener('offline', onOffline);
    window.document.removeEventListener('visibilitychange', onVisibility);
    eventsBound = false;
  }

  MD.sync = {
    state: state,
    run: function () {
      if (running) return running;
      running = runSync().finally(function () { running = null; });
      return running;
    },
    startAuto: function (ms) {
      autoMs = Number(ms) > 0 ? Number(ms) : 60000;
      MD.sync.stopAuto();
      bindEvents();
      timer = window.setInterval(function () {
        MD.sync.run().catch(function () { /* 自动同步失败由状态机呈现。 */ });
      }, autoMs);
      renderIndicator();
      MD.sync.run().catch(function () { /* 启动同步失败由状态机呈现。 */ });
    },
    stopAuto: function () {
      if (timer) window.clearInterval(timer);
      timer = null;
      unbindEvents();
    },
    onChange: function (fn) {
      MD.bus.on('sync:state', fn);
      return function () { MD.bus.off('sync:state', fn); };
    },
    markDirty: function () {
      window.clearTimeout(dirtyTimer);
      dirtyTimer = window.setTimeout(function () {
        if (timer && window.document.visibilityState !== 'hidden') {
          MD.sync.run().catch(function () { /* 延迟同步失败由状态机呈现。 */ });
        }
      }, 1200);
    }
  };

  if (window.navigator && window.navigator.onLine === false) setState('offline');
  else renderIndicator();
})(window);

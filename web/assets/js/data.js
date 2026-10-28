(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var TABLES = ['profile', 'medication', 'schedule', 'intake', 'family'];

  function publicRecord(record) {
    if (!record) return record;
    var clean = Object.assign({}, record);
    delete clean._dirty;
    delete clean._example;
    return clean;
  }

  function matches(record, filter) {
    if (typeof filter === 'function') return filter(record);
    if (!filter || typeof filter !== 'object') return true;
    return Object.keys(filter).every(function (key) { return record[key] === filter[key]; });
  }

  function notify(table, id, action) {
    var payload = { table: table, id: id, action: action || 'save' };
    MD.bus.emit('data:changed', payload);
    if (MD.sync && typeof MD.sync.markDirty === 'function') MD.sync.markDirty();
  }

  function collection(table) {
    return {
      list: async function (filter) {
        var records = await MD.db.list(table);
        return records.filter(function (record) {
          return !record.deleted && matches(publicRecord(record), filter);
        }).map(publicRecord);
      },
      get: async function (id) {
        return publicRecord(await MD.db.get(table, id));
      },
      save: async function (object) {
        object = object || {};
        var id = object.id || MD.util.uuid();
        var existing = await MD.db.get(table, id);
        var record = Object.assign({}, existing || {}, object, {
          id: id,
          updatedAt: MD.util.nowMs(),
          ver: Math.max(Number(existing && existing.ver) || 0, Number(object.ver) || 0) + 1,
          deleted: Boolean(object.deleted),
          _dirty: true
        });
        await MD.db.put(table, record);
        notify(table, id, record.deleted ? 'remove' : 'save');
        return publicRecord(record);
      },
      remove: async function (id) {
        var existing = await MD.db.get(table, id);
        if (!existing) return;
        var record = Object.assign({}, existing, {
          deleted: true,
          updatedAt: MD.util.nowMs(),
          ver: (Number(existing.ver) || 0) + 1,
          _dirty: true
        });
        await MD.db.put(table, record);
        notify(table, id, 'remove');
      }
    };
  }

  async function exportBackup() {
    var tables = {};
    await Promise.all(TABLES.map(async function (table) {
      tables[table] = (await MD.db.list(table)).map(publicRecord);
    }));
    return { version: MD.version, exportedAt: MD.util.nowMs(), tables: tables };
  }

  async function importBackup(snapshot) {
    if (!snapshot || typeof snapshot !== 'object' || !snapshot.tables || typeof snapshot.tables !== 'object') {
      throw new TypeError('备份文件格式无效');
    }
    for (var tableIndex = 0; tableIndex < TABLES.length; tableIndex += 1) {
      var table = TABLES[tableIndex];
      var incoming = Array.isArray(snapshot.tables[table]) ? snapshot.tables[table] : [];
      for (var index = 0; index < incoming.length; index += 1) {
        var record = incoming[index];
        if (!record || !record.id) continue;
        var existing = await MD.db.get(table, record.id);
        if (!existing || Number(record.updatedAt || 0) > Number(existing.updatedAt || 0)) {
          await MD.db.put(table, Object.assign({}, record, { _dirty: true }));
        }
      }
    }
    notify('*', '*', 'import');
  }

  async function wipeAll() {
    await Promise.all(TABLES.map(function (table) { return MD.db.clear(table); }));
    await Promise.all([
      MD.db.blobClear ? MD.db.blobClear() : Promise.resolve(),
      MD.db.metaClear ? MD.db.metaClear() : Promise.resolve()
    ]);
    if (MD.api && typeof MD.api.clearSession === 'function') MD.api.clearSession();
    MD.bus.emit('data:changed', { table: '*', id: '*', action: 'wipe' });
  }

  async function seedExamples() {
    var seeded = await MD.db.metaGet('examples.seeded');
    var profileRecords = await MD.db.list('profile');
    if (seeded || profileRecords.some(function (item) { return !item.deleted; })) return false;

    var now = MD.util.nowMs();
    var profileId = MD.util.uuid();
    var medMorningId = MD.util.uuid();
    var medEveningId = MD.util.uuid();
    var hour = new Date(now).getHours();
    var overdueHour = Math.max(0, hour - 1);
    var overdueTime = String(overdueHour).padStart(2, '0') + ':00';
    var futureTime = String(Math.min(23, hour + 2)).padStart(2, '0') + ':00';
    var records = [
      ['profile', { id: profileId, name: '示例成员', relation: '本人', color: '#0e9f6e', note: '可在设置中清空示例数据', archived: false }],
      ['medication', { id: medMorningId, profileId: profileId, name: '示例维生素', dosageAmount: 1, dosageUnit: '片', form: '片剂', photoKeys: [], critical: false, active: true, stock: 20, refillThreshold: 5 }],
      ['medication', { id: medEveningId, profileId: profileId, name: '示例处方药', dosageAmount: 1, dosageUnit: '粒', form: '胶囊', foodInstruction: '饭后服用', photoKeys: [], critical: true, active: true, stock: 8, refillThreshold: 3 }],
      ['schedule', { id: MD.util.uuid(), medicationId: medMorningId, kind: 'daily', times: [overdueTime], startDate: MD.util.dayKey(now), remindOffsetMin: 0, critical: false }],
      ['schedule', { id: MD.util.uuid(), medicationId: medEveningId, kind: 'daily', times: [futureTime], startDate: MD.util.dayKey(now), remindOffsetMin: 10, critical: true }]
    ];
    for (var index = 0; index < records.length; index += 1) {
      var pair = records[index];
      await MD.db.put(pair[0], Object.assign({}, pair[1], {
        updatedAt: now + index,
        ver: 1,
        deleted: false,
        _dirty: true,
        _example: true
      }));
    }
    await MD.db.metaSet('examples.seeded', true);
    notify('*', '*', 'seed');
    return true;
  }

  async function clearExamples() {
    var changed = false;
    for (var tableIndex = 0; tableIndex < TABLES.length; tableIndex += 1) {
      var table = TABLES[tableIndex];
      var records = await MD.db.list(table);
      for (var index = 0; index < records.length; index += 1) {
        if (!records[index]._example || records[index].deleted) continue;
        var tombstone = Object.assign({}, records[index], {
          deleted: true,
          updatedAt: MD.util.nowMs(),
          ver: (Number(records[index].ver) || 0) + 1,
          _dirty: true
        });
        await MD.db.put(table, tombstone);
        changed = true;
      }
    }
    if (changed) notify('*', '*', 'clear-examples');
    return changed;
  }

  MD.data = {
    profiles: collection('profile'),
    meds: collection('medication'),
    schedules: collection('schedule'),
    intakes: collection('intake'),
    // 家庭档案：与账号相互独立的成员条目，可编辑姓名/关系/备注。
    // 关联来的他账号数据不落在这里（体积不可控且会撑大同步负载），只存指纹，
    // 明文快照放在 blobs，键名 family:<id>。
    family: collection('family'),
    blobs: {
      save: function (key, value) { return MD.db.blobSave(key, value); },
      get: function (key) { return MD.db.blobGet(key); },
      remove: function (key) { return MD.db.blobDel(key); }
    },
    backup: {
      export: exportBackup,
      import: importBackup
    },
    wipeAll: wipeAll,
    onChange: function (fn) {
      MD.bus.on('data:changed', fn);
      return function () { MD.bus.off('data:changed', fn); };
    },
    seedExamples: seedExamples,
    clearExamples: clearExamples,
    _tables: TABLES.slice(),
    _publicRecord: publicRecord
  };
})(window);

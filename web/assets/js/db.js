(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var DB_NAME = 'meddesk';
  var DB_VERSION = 1;
  var connection = null;
  var opening = null;

  function asError(error, fallback) {
    if (error instanceof Error) return error;
    return new Error((error && error.message) || fallback || '数据库操作失败');
  }

  // 数据库连接采用单例 Promise，避免启动阶段重复打开连接。
  function openDB() {
    if (connection) return Promise.resolve(connection);
    if (opening) return opening;
    opening = new Promise(function (resolve, reject) {
      if (!window.indexedDB) {
        reject(new Error('当前浏览器不支持 IndexedDB'));
        return;
      }
      var request = window.indexedDB.open(DB_NAME, DB_VERSION);
      request.onupgradeneeded = function () {
        var db = request.result;
        if (!db.objectStoreNames.contains('records')) {
          var records = db.createObjectStore('records', { keyPath: ['table', 'id'] });
          records.createIndex('table', 'table', { unique: false });
        }
        if (!db.objectStoreNames.contains('blobs')) db.createObjectStore('blobs', { keyPath: 'key' });
        if (!db.objectStoreNames.contains('meta')) db.createObjectStore('meta', { keyPath: 'key' });
      };
      request.onsuccess = function () {
        connection = request.result;
        opening = null;
        connection.onversionchange = function () {
          connection.close();
          connection = null;
        };
        resolve(connection);
      };
      request.onerror = function () {
        opening = null;
        reject(asError(request.error, '无法打开本地数据库'));
      };
      request.onblocked = function () {
        opening = null;
        reject(new Error('本地数据库升级被其他页面阻止'));
      };
    });
    return opening;
  }

  // 所有事务错误都通过 reject 抛出，不返回伪成功对象。
  function transaction(storeName, mode, operation) {
    return openDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var settled = false;
        var tx;
        try {
          tx = db.transaction(storeName, mode);
          operation(tx.objectStore(storeName), resolve, reject, tx);
        } catch (error) {
          reject(asError(error));
          return;
        }
        tx.onabort = function () {
          if (!settled) reject(asError(tx.error, '数据库事务已中止'));
        };
        tx.onerror = function () {
          if (!settled) reject(asError(tx.error, '数据库事务失败'));
        };
        var originalResolve = resolve;
        resolve = function (value) {
          if (settled) return;
          settled = true;
          originalResolve(value);
        };
      });
    });
  }

  function requestResult(request, resolve, reject, transform) {
    request.onsuccess = function () {
      try {
        resolve(transform ? transform(request.result) : request.result);
      } catch (error) {
        reject(asError(error));
      }
    };
    request.onerror = function () { reject(asError(request.error)); };
  }

  function cleanRecord(record) {
    if (!record) return record;
    var clean = Object.assign({}, record);
    delete clean.table;
    return clean;
  }

  MD.db = {
    init: openDB,
    get: function (table, id) {
      return transaction('records', 'readonly', function (store, resolve, reject) {
        requestResult(store.get([table, id]), resolve, reject, cleanRecord);
      });
    },
    put: function (table, record) {
      if (!record || !record.id) return Promise.reject(new TypeError('记录缺少 id'));
      var stored = Object.assign({}, record, { table: table });
      return transaction('records', 'readwrite', function (store, resolve, reject) {
        requestResult(store.put(stored), resolve, reject, function () { return cleanRecord(stored); });
      });
    },
    del: function (table, id) {
      return transaction('records', 'readwrite', function (store, resolve, reject) {
        requestResult(store.delete([table, id]), resolve, reject, function () { return undefined; });
      });
    },
    list: function (table) {
      return transaction('records', 'readonly', function (store, resolve, reject) {
        var index = store.index('table');
        var range = window.IDBKeyRange.only(table);
        requestResult(index.getAll(range), resolve, reject, function (items) {
          return items.map(cleanRecord);
        });
      });
    },
    clear: function (table) {
      return transaction('records', 'readwrite', function (store, resolve, reject) {
        if (!table) {
          requestResult(store.clear(), resolve, reject, function () { return undefined; });
          return;
        }
        var request = store.index('table').openKeyCursor(window.IDBKeyRange.only(table));
        request.onerror = function () { reject(asError(request.error)); };
        request.onsuccess = function () {
          var cursor = request.result;
          if (!cursor) {
            resolve();
            return;
          }
          store.delete(cursor.primaryKey);
          cursor.continue();
        };
      });
    },
    blobSave: function (key, value) {
      return transaction('blobs', 'readwrite', function (store, resolve, reject) {
        requestResult(store.put({ key: key, value: value }), resolve, reject, function () { return value; });
      });
    },
    blobGet: function (key) {
      return transaction('blobs', 'readonly', function (store, resolve, reject) {
        requestResult(store.get(key), resolve, reject, function (entry) { return entry ? entry.value : undefined; });
      });
    },
    blobDel: function (key) {
      return transaction('blobs', 'readwrite', function (store, resolve, reject) {
        requestResult(store.delete(key), resolve, reject, function () { return undefined; });
      });
    },
    blobClear: function () {
      return transaction('blobs', 'readwrite', function (store, resolve, reject) {
        requestResult(store.clear(), resolve, reject, function () { return undefined; });
      });
    },
    metaGet: function (key) {
      return transaction('meta', 'readonly', function (store, resolve, reject) {
        requestResult(store.get(key), resolve, reject, function (entry) { return entry ? entry.value : undefined; });
      });
    },
    metaSet: function (key, value) {
      return transaction('meta', 'readwrite', function (store, resolve, reject) {
        requestResult(store.put({ key: key, value: value }), resolve, reject, function () { return value; });
      });
    },
    metaDel: function (key) {
      return transaction('meta', 'readwrite', function (store, resolve, reject) {
        requestResult(store.delete(key), resolve, reject, function () { return undefined; });
      });
    },
    metaClear: function () {
      return transaction('meta', 'readwrite', function (store, resolve, reject) {
        requestResult(store.clear(), resolve, reject, function () { return undefined; });
      });
    }
  };
})(window);

(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var BASE = '/api/v1';
  var PREFIX = 'meddesk.';
  var sessionFields = ['access', 'refresh', 'userId', 'expiresAt', 'kdfSalt', 'wrappedDek', 'dekIv'];
  var unauthorizedHandlers = [];
  var memoryDEK = null;
  var refreshPromise = null;

  // 「保持登录」：refresh token 与 DEK 双写 localStorage——
  // 添加到主屏（PWA）后重新打开时自动恢复登录态，无需重新登录/解锁。
  // 安全边界：材料仅存本机（依赖设备锁屏保护），登出/401 时彻底清除。
  function storageGet(key) {
    try {
      if (key === 'refresh' || key === 'dek') {
        var v = window.sessionStorage.getItem(PREFIX + key);
        if (v != null) return v;
        return window.localStorage.getItem(PREFIX + key);
      }
      return window.sessionStorage.getItem(PREFIX + key);
    } catch (_) { return null; }
  }

  function storageSet(key, value) {
    try {
      if (value == null) {
        window.sessionStorage.removeItem(PREFIX + key);
        if (key === 'refresh' || key === 'dek') window.localStorage.removeItem(PREFIX + key);
        return;
      }
      window.sessionStorage.setItem(PREFIX + key, String(value));
      if (key === 'refresh' || key === 'dek') window.localStorage.setItem(PREFIX + key, String(value));
    } catch (_) {
      // 隐私模式禁用存储时，会话仅维持在当前调用链中。
    }
  }

  function accountSaltKey(email) {
    return 'meddesk.auth.salt.' + String(email || '').trim().toLowerCase();
  }

  function rememberAccountSalt(email, saltB64) {
    try { window.localStorage.setItem(accountSaltKey(email), saltB64); } catch (_) { /* 缓存登录盐值失败不影响当前会话。 */ }
  }

  function knownAccountSalt(email) {
    try { return window.localStorage.getItem(accountSaltKey(email)); } catch (_) { return null; }
  }

  function normalizeSession(payload, previous) {
    payload = payload || {};
    previous = previous || {};
    var user = payload.user || {};
    return {
      userId: payload.userId || payload.user_id || user.id || previous.userId || '',
      access: payload.access || payload.accessToken || payload.access_token || previous.access || '',
      refresh: payload.refresh || payload.refreshToken || payload.refresh_token || previous.refresh || '',
      expiresAt: payload.expiresAt || payload.expires_at || previous.expiresAt || 0,
      kdfSalt: payload.kdfSalt || payload.kdf_salt || previous.kdfSalt || '',
      wrappedDek: payload.wrappedDek || payload.wrappedDEK || payload.wrapped_dek || previous.wrappedDek || '',
      dekIv: payload.dekIv || payload.dekIV || payload.dek_iv || previous.dekIv || ''
    };
  }

  function getSession() {
    var session = {};
    sessionFields.forEach(function (field) {
      var value = storageGet(field);
      if (value != null) session[field] = field === 'expiresAt' ? Number(value) : value;
    });
    return session;
  }

  function setSession(payload) {
    var session = normalizeSession(payload, getSession());
    sessionFields.forEach(function (field) { storageSet(field, session[field]); });
    MD.bus.emit('auth:changed', session);
    return session;
  }

  function setDEK(dek) {
    memoryDEK = dek ? new Uint8Array(dek) : null;
    storageSet('dek', memoryDEK ? MD.crypto.bytesToB64(memoryDEK) : null);
    return memoryDEK;
  }

  function getDEK() {
    if (memoryDEK) return memoryDEK;
    var encoded = storageGet('dek');
    if (!encoded) return null;
    try {
      memoryDEK = MD.crypto.b64ToBytes(encoded);
      return memoryDEK;
    } catch (_) {
      storageSet('dek', null);
      return null;
    }
  }

  function clearSession() {
    sessionFields.concat(['dek']).forEach(function (field) { storageSet(field, null); });
    memoryDEK = null;
    MD.bus.emit('auth:changed', null);
  }

  function notifyUnauthorized() {
    clearSession();
    unauthorizedHandlers.slice().forEach(function (fn) {
      try { fn(); } catch (error) { window.setTimeout(function () { throw error; }, 0); }
    });
  }

  async function parseResponse(response, responseType) {
    if (responseType === 'blob') return response.blob();
    if (response.status === 204) return null;
    var text = await response.text();
    if (!text) return null;
    try { return JSON.parse(text); } catch (_) { return text; }
  }

  function apiError(response, payload) {
    var error = new Error(payload && payload.message ? payload.message : '请求失败（' + response.status + '）');
    error.status = response.status;
    error.code = payload && payload.code;
    error.details = payload && payload.details;
    return error;
  }

  async function refreshSession() {
    if (refreshPromise) return refreshPromise;
    var refreshToken = storageGet('refresh');
    if (!refreshToken) throw new Error('登录已失效');
    refreshPromise = window.fetch(BASE + '/auth/refresh', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
      body: JSON.stringify({ refresh: refreshToken, refreshToken: refreshToken })
    }).then(async function (response) {
      var payload = await parseResponse(response);
      if (!response.ok) throw apiError(response, payload);
      return setSession(payload);
    }).finally(function () {
      refreshPromise = null;
    });
    return refreshPromise;
  }

  async function request(path, options, retried) {
    options = options || {};
    var headers = new Headers(options.headers || {});
    var access = storageGet('access');
    if (access && !headers.has('Authorization')) headers.set('Authorization', 'Bearer ' + access);
    if (options.body != null && !(options.body instanceof FormData) && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    if (!headers.has('Accept')) headers.set('Accept', options.responseType === 'blob' ? '*/*' : 'application/json');

    var response;
    try {
      response = await window.fetch(BASE + path, Object.assign({}, options, { headers: headers }));
    } catch (error) {
      error.network = true;
      throw error;
    }

    if (response.status === 401 && !options.skipAuthRefresh && !retried) {
      try {
        await refreshSession();
        return request(path, options, true);
      } catch (_) {
        notifyUnauthorized();
      }
    }

    var payload = await parseResponse(response, options.responseType);
    if (!response.ok) {
      if (response.status === 401 && !options.skipUnauthorized) notifyUnauthorized();
      throw apiError(response, payload);
    }
    return payload;
  }

  async function unlockSession(session, password) {
    if (!session.kdfSalt || !session.wrappedDek || !session.dekIv) throw new Error('账户缺少加密凭据');
    var salt = MD.crypto.b64ToBytes(session.kdfSalt);
    var dek = await MD.crypto.unwrapDEK(session.wrappedDek, session.dekIv, password, salt);
    setDEK(dek);
    return session;
  }

  MD.api = {
    register: async function (credentials) {
      credentials = credentials || {};
      var email = String(credentials.email || '').trim().toLowerCase();
      var password = String(credentials.password || '');
      var salt = MD.crypto.generateSalt();
      var dek = MD.crypto.newDEK();
      var derived = await MD.crypto.derive(password, salt);
      var wrapped = await MD.crypto.wrapDEK(dek, password, salt);
      var saltB64 = MD.crypto.bytesToB64(salt);
      var payload = await request('/auth/register', {
        method: 'POST',
        skipAuthRefresh: true,
        body: JSON.stringify({
          email: email,
          passwordHash: derived.hashB64,
          kdfSalt: saltB64,
          wrappedDek: wrapped.wrappedB64,
          dekIv: wrapped.ivB64
        })
      });
      var session = setSession(normalizeSession(payload, {
        kdfSalt: saltB64,
        wrappedDek: wrapped.wrappedB64,
        dekIv: wrapped.ivB64
      }));
      rememberAccountSalt(email, saltB64);
      setDEK(dek);
      return session;
    },
    login: async function (credentials) {
      credentials = credentials || {};
      var email = String(credentials.email || '').trim().toLowerCase();
      var password = String(credentials.password || '');
      // 盐值获取顺序：调用方显式提供 → 本机缓存 → 服务端握手（新设备首次登录）。
      // /auth/salt 对不存在的账户也返回随机盐（防枚举），用假盐派生的哈希登录必然失败。
      var saltB64 = credentials.kdfSalt || knownAccountSalt(email);
      if (!saltB64) {
        try {
          var saltResp = await request('/auth/salt', {
            method: 'POST',
            skipAuthRefresh: true,
            skipUnauthorized: true,
            body: JSON.stringify({ email: email })
          });
          saltB64 = saltResp && (saltResp.kdfSalt || saltResp.KDFSalt);
        } catch (e) { /* 网络失败走下方统一报错 */ }
        if (!saltB64) throw new Error('获取账户盐值失败，请检查网络后重试');
        rememberAccountSalt(email, saltB64);
      }
      var derived = await MD.crypto.derive(password, MD.crypto.b64ToBytes(saltB64));
      var payload = await request('/auth/login', {
        method: 'POST',
        skipAuthRefresh: true,
        body: JSON.stringify({ email: email, passwordHash: derived.hashB64 })
      });
      var session = setSession(normalizeSession(payload, { kdfSalt: saltB64 }));
      rememberAccountSalt(email, session.kdfSalt || saltB64);
      return unlockSession(session, password);
    },
    refresh: refreshSession,
    logout: async function () {
      var token = storageGet('refresh');
      try {
        if (token) await request('/auth/logout', {
          method: 'POST',
          skipAuthRefresh: true,
          skipUnauthorized: true,
          body: JSON.stringify({ refresh: token, refreshToken: token })
        });
      } finally {
        clearSession();
      }
    },
    me: function () { return request('/auth/me'); },
    push: function (changes) {
      return request('/sync/push', { method: 'POST', body: JSON.stringify({ changes: changes, items: changes }) });
    },
    pull: function (cursor, limit) {
      var params = new URLSearchParams({ cursor: String(cursor || 0), limit: String(limit || 300) });
      return request('/sync/pull?' + params.toString());
    },
    stats: function () { return request('/sync/stats'); },
    reset: function () { return request('/sync/reset', { method: 'POST', body: '{}' }); },
    uploadAttachment: function (options) {
      options = options || {};
      var form = new FormData();
      form.append('file', options.blob, options.blob && options.blob.name ? options.blob.name : 'attachment.bin');
      form.append('iv', options.ivB64 || '');
      if (options.recordId) form.append('recordId', options.recordId);
      return request('/attachments', { method: 'POST', body: form });
    },
    getAttachment: async function (id) {
      var headers = new Headers();
      var access = storageGet('access');
      if (access) headers.set('Authorization', 'Bearer ' + access);
      var response = await window.fetch(BASE + '/attachments/' + encodeURIComponent(id), { headers: headers });
      if (response.status === 401) {
        try {
          await refreshSession();
          return MD.api.getAttachment(id);
        } catch (_) {
          notifyUnauthorized();
        }
      }
      if (!response.ok) throw apiError(response, await parseResponse(response));
      return { blob: await response.blob(), ivB64: response.headers.get('X-MedDesk-IV') || '' };
    },
    deleteAttachment: function (id) {
      return request('/attachments/' + encodeURIComponent(id), { method: 'DELETE' });
    },
    postExport: function (format, payload) {
      if (['csv', 'pdf'].indexOf(format) < 0) return Promise.reject(new TypeError('不支持的导出格式'));
      return request('/export/' + format, { method: 'POST', responseType: 'blob', body: JSON.stringify(payload) });
    },
    onUnauthorized: function (fn) {
      if (typeof fn === 'function') unauthorizedHandlers.push(fn);
      return function () {
        unauthorizedHandlers = unauthorizedHandlers.filter(function (item) { return item !== fn; });
      };
    },
    isAuthed: function () { return MD.api.hasSession() && Boolean(MD.api.getDEK()); },
    session: function () { return normalizeSession(getSession(), {}); },
    getSession: getSession,
    setSession: setSession,
    clearSession: clearSession,
    getDEK: getDEK,
    setDEK: setDEK,
    hasSession: function () {
      if (Boolean(storageGet('access') && storageGet('userId'))) return true;
      // PWA 重开场景：sessionStorage 已空，但本机持久化了 refresh + DEK → 可静默恢复
      return Boolean(storageGet('refresh') && storageGet('dek'));
    }
  };
})(window);

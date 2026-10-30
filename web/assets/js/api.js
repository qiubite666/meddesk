(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  // 注意：REMOTE_API_BASE 必须声明在 `var BASE = apiBase()` 之前。
  // var 会提升但赋值不会，若写在后面，apiBase() 在初始化期读到的就是 undefined，
  // 原生环境未设覆盖值时会拼出 "undefined/api/v1"，表现为所有接口全部失败。
  // 默认值写成可覆盖形式：换域名或自建实例时，只需在 index.html 里
  // <script>window.MD_API_BASE='https://你的域名/api/v1'</script> 即可。
  var REMOTE_API_BASE = 'https://meddesk.qiubite.fun/api/v1';

  var BASE = apiBase();
  var PREFIX = 'meddesk.';

  /**
   * 决定 API 基址。
   *
   * 【为什么需要这个判断】
   * Capacitor 安卓壳里把所有网页资产打包进 APK，页面源不再是服务器域名，
   * 而是 https://localhost（capacitor.config.json 的 androidScheme 指定）。
   * 此时 fetch('/api/v1/...') 会被解析成 https://localhost/api/v1/...，
   * 打到设备自身必然失败——App 装上后表现为「账号密码都对但什么都加载不出来」。
   * 因此原生环境必须切到绝对地址；浏览器与 PWA 仍走同源相对路径，行为完全不变。
   */
  function apiBase() {
    try {
      if (window.Capacitor && typeof window.Capacitor.isNativePlatform === 'function'
        && window.Capacitor.isNativePlatform()) {
        var override = window.MD_API_BASE;
        if (typeof override === 'string') {
          var v = override.replace(/\/+$/, '');
          // 公网域名强制 https：凭据与密文信封绝不能走明文通道。
          var isHttps = /^https:\/\/\S+/i.test(v);
          // 仅本机与私有网段放行 http，否则模拟器/局域网联调时会「改了配置却不生效」，
          // 表现为请求静默回落到内置默认地址，极难排查。
          var isLocalHttp = /^http:\/\/(localhost|127\.0\.0\.1|10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(?:1[6-9]|2\d|3[01])\.\d+\.\d+)(?::\d+)?(?:\/\S*)?$/i.test(v);
          if (isHttps || isLocalHttp) return v;
        }
        return REMOTE_API_BASE;
      }
    } catch (e) {
      // 壳环境探测本身出错时按浏览器处理，宁可用相对路径也不要静默发到错误地址。
    }
    return '/api/v1';
  }
  var sessionFields = ['access', 'refresh', 'userId', 'expiresAt', 'kdfSalt', 'wrappedDek', 'dekIv'];
  var unauthorizedHandlers = [];
  var memoryDEK = null;
  var refreshPromise = null;
  var unauthorizedNotified = false;

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
      dekIv: payload.dekIv || payload.dekIV || payload.dek_iv || previous.dekIv || '',
      // 管理员用「保留数据」模式改过口令：信封还是旧口令包裹的，不能拿新口令去解。
      envelopeStale: payload.envelopeStale === true || payload.envelope_stale === true
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
    // 新会话建立：重新武装登出通知，否则重新登录后再次失效会静默无提示。
    unauthorizedNotified = false;
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

  /**
   * 修改口令（无损：数据不重置）。
   *
   * 【为什么必须由客户端做】
   * 服务端只存 wrapped_dek（被 KEK 包裹的数据密钥），KEK 由「口令 → PBKDF2 → HKDF」得到。
   * 换口令必然换 KEK，因此必须用旧口令解出 DEK，再用新口令重新包裹一次 ——
   * 这一步只能在持有明文 DEK 的设备上完成，服务端和管理员都做不到。
   *
   * 成功后服务端会吊销全部会话（含本机），需要用户用新口令重新登录一次。
   */
  async function changePassword(oldPassword, newPassword) {
    var session = getSession();
    if (!session || !session.kdfSalt) throw new Error('登录状态异常，请重新登录后再改');
    var dek = getDEK();
    if (!dek) throw new Error('本机没有数据密钥，请用旧口令重新登录后再改');

    var oldSalt = MD.crypto.b64ToBytes(session.kdfSalt);
    var oldDerived = await MD.crypto.derive(oldPassword, oldSalt);

    var newSalt = MD.crypto.generateSalt();
    var newDerived = await MD.crypto.derive(newPassword, newSalt);
    var wrapped = await MD.crypto.wrapDEK(dek, newPassword, newSalt);

    // request() 直接把 body 交给 fetch，必须是字符串（否则会发出 [object Object]）
    await request('/auth/change-password', {
      method: 'POST',
      skipUnauthorized: true,
      body: JSON.stringify({
        oldPasswordHash: oldDerived.hashB64,
        newPasswordHash: newDerived.hashB64,
        kdfSalt: MD.crypto.bytesToB64(newSalt),
        wrappedDek: wrapped.wrappedB64,
        dekIv: wrapped.ivB64
      })
    });
    return true;
  }

  function clearSession() {
    sessionFields.concat(['dek']).forEach(function (field) { storageSet(field, null); });
    memoryDEK = null;
    // 会话结束：共享的刷新结果与锁一并作废。否则登出后其他标签页刷新时
    // 会命中这份已失效的 access，白白多发一轮 401 才回到正常。
    lsSet(REFRESH_SHARE, null);
    lsSet(REFRESH_LOCK, null);
    MD.bus.emit('auth:changed', null);
  }

  function notifyUnauthorized() {
    // 幂等：request / 附件下载 / 记账本三条路径可能同时发现令牌失效，
    // 重复清 session 与重复触发登出回调会让登录页闪两次、抛出双份 toast。
    if (unauthorizedNotified) return;
    unauthorizedNotified = true;
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

  // ---------------------------------------------------------------------------
  // 令牌刷新的跨标签页协调
  //
  // 背景：access 只存 sessionStorage（每个标签页独立），refresh 存 localStorage（跨标签页共享）。
  // 多个标签页同时收到 401 时，会各自拿同一个 refresh token 去打 /auth/refresh：
  //   · 服务端把并行刷新视为令牌重放风险（虽有 REFRESH_REUSE_GRACE 宽限）
  //   · nginx 对 /auth/refresh 有独立限流，多标签页会把额度成倍消耗
  // 做法：Web Locks（原子、无死锁）+ localStorage 租约锁（兜底）+ 短期共享刷新结果。
  // 最坏情况退化为「各刷各的」，与改造前一致，不会更差。
  // ---------------------------------------------------------------------------
  var REFRESH_LOCK = 'refresh.lock';
  var REFRESH_SHARE = 'refresh.share';
  var LOCK_TTL = 5000;            // 锁租约：持有者崩溃后最多 5s 自动失效
  var SHARE_TTL = 5 * 60 * 1000;  // 共享结果有效期：5 分钟内其他标签页可直接采用
  var tabId = Math.random().toString(36).slice(2) + '-' + Date.now().toString(36);

  function lsGet(key) {
    try { return window.localStorage.getItem(PREFIX + key); } catch (_) { return null; }
  }

  function lsSet(key, value) {
    try {
      if (value == null) window.localStorage.removeItem(PREFIX + key);
      else window.localStorage.setItem(PREFIX + key, String(value));
    } catch (_) { /* 隐私模式禁用存储：协调退化为各刷各的 */ }
  }

  function lsJson(key) {
    var raw = lsGet(key);
    if (!raw) return null;
    try { return JSON.parse(raw); } catch (_) { return null; }
  }

  function sleep(ms) {
    return new Promise(function (resolve) { window.setTimeout(resolve, ms); });
  }

  // refresh token 以 localStorage 为准：sessionStorage 里可能是本标签页登录时的旧副本，
  // 别的标签页轮换后本地仍读到过期值，服务端会判定为重放并吊销全部会话。
  function currentRefreshToken() {
    var shared = lsGet('refresh');
    if (shared) return shared;
    return storageGet('refresh');
  }

  function readShare(since) {
    var share = lsJson(REFRESH_SHARE);
    if (!share || !share.access) return null;
    if (since && Number(share.at) <= since) return null;   // 早于本次等待，无用
    if (Date.now() - Number(share.at) > SHARE_TTL) return null;
    var mine = storageGet('userId');
    // 未登录（登录页）或不同账号的标签页不得采纳别人的令牌
    if (mine && share.userId && String(mine) !== String(share.userId)) return null;
    return share;
  }

  // 采用其他标签页的刷新结果：只覆盖 access/expiresAt，不整体 setSession——
  // 避免把本标签页的 DEK、refresh 等内存态一并替换掉。
  function adoptShared(share) {
    storageSet('access', share.access);
    if (share.expiresAt) storageSet('expiresAt', share.expiresAt);
    var session = getSession();
    MD.bus.emit('auth:changed', session);
    return session;
  }

  function publishSession(session) {
    if (!session || !session.access) return;
    var share = {
      access: session.access,
      expiresAt: session.expiresAt || 0,
      userId: session.userId || storageGet('userId') || '',
      at: Date.now()
    };
    lsSet(REFRESH_SHARE, JSON.stringify(share));
    try {
      if (authChannel) {
        authChannel.postMessage({
          type: 'session', access: share.access,
          expiresAt: share.expiresAt, userId: share.userId
        });
      }
    } catch (_) { /* 广播失败不影响本次刷新结果 */ }
  }

  var authChannel = null;
  try {
    if (typeof window.BroadcastChannel === 'function') authChannel = new window.BroadcastChannel('meddesk-auth');
  } catch (_) { authChannel = null; }
  if (authChannel) {
    authChannel.onmessage = function (event) {
      var data = event && event.data;
      if (!data || data.type !== 'session' || !data.access) return;
      var mine = storageGet('userId');
      if (!mine) return;                                   // 登录页不采纳
      if (data.userId && String(mine) !== String(data.userId)) return;  // 不同账号
      if (data.access === storageGet('access')) return;
      // 提前换上新 access：本标签页下次请求就不会再吃一个 401
      storageSet('access', data.access);
      if (data.expiresAt) storageSet('expiresAt', data.expiresAt);
    };
  }

  function lockOwnerAlive() {
    var lock = lsJson(REFRESH_LOCK);
    if (!lock || !lock.id || !lock.at) return false;
    if (Date.now() - Number(lock.at) > LOCK_TTL) return false;   // 过期视为无主
    return String(lock.id) !== tabId;
  }

  function tryLock() {
    if (lockOwnerAlive()) return false;
    lsSet(REFRESH_LOCK, JSON.stringify({ id: tabId, at: Date.now() }));
    var check = lsJson(REFRESH_LOCK);
    return !!(check && String(check.id) === tabId);
  }

  function unlockRefresh() {
    var lock = lsJson(REFRESH_LOCK);
    if (lock && String(lock.id) === tabId) lsSet(REFRESH_LOCK, null);
  }

  async function withRefreshLock(task) {
    var locks = window.navigator && window.navigator.locks;
    if (locks && typeof locks.request === 'function') {
      var ran = false;
      try {
        return await locks.request('meddesk:token-refresh', async function () {
          ran = true;
          return await task();
        });
      } catch (error) {
        // task 自身抛出的错误（401/429）必须原样上抛；只有锁 API 异常才降级重跑，
        // 否则会把一次刷新请求打成两次。
        if (ran) throw error;
      }
    }
    var deadline = Date.now() + 3000;
    while (Date.now() < deadline) {
      if (tryLock()) {
        try { return await task(); } finally { unlockRefresh(); }
      }
      await sleep(120);
    }
    // 等不到锁也不能让用户卡住：自己刷一次，最坏多打一个 /auth/refresh。
    return task();
  }

  // 刷新失败的分类，决定「要不要把用户踢下线」：
  //   401/403        → refresh token 真的失效，必须重新登录
  //   429 / 5xx / 网络 → 服务端暂时不可用（含 nginx 限流），退避重试即可
  // 旧实现把任何失败都当登录失效，于是「被限流」表现为「被踢出登录」。
  function refreshRetryable(error) {
    if (!error) return false;
    if (error.network) return true;
    var status = Number(error.status || 0);
    return status === 0 || status === 429 || status >= 500;
  }

  var REFRESH_BACKOFF = [800, 1800, 3600];   // 合计约 6.2s，足够跨过一个限流窗口

  async function postRefresh(refreshToken) {
    var response;
    try {
      response = await window.fetch(BASE + '/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
        body: JSON.stringify({ refresh: refreshToken, refreshToken: refreshToken })
      });
    } catch (error) {
      error.network = true;
      throw error;
    }
    var payload = await parseResponse(response);
    if (!response.ok) throw apiError(response, payload);
    return setSession(payload);
  }

  async function performRefresh() {
    var startedAt = Date.now();

    // 1) 别的标签页可能刚刷新过：先看共享结果，命中就完全不必发请求。
    var cached = readShare(0);
    if (cached) return adoptShared(cached);

    if (!currentRefreshToken()) {
      var missing = new Error('登录已失效');
      missing.authInvalid = true;
      throw missing;
    }

    // 2) 加锁后再看一次：等锁期间别的标签页可能已完成刷新。
    return withRefreshLock(async function () {
      var fresh = readShare(startedAt);
      if (fresh) return adoptShared(fresh);

      // 锁内重读：refresh token 可能已被别的标签页轮换，旧值服务端会判重放。
      var tokenNow = currentRefreshToken();
      if (!tokenNow) {
        // 等锁期间用户可能已经在别的标签页登出：这是「会话结束」而不是「暂时故障」。
        var gone = new Error('登录已失效');
        gone.authInvalid = true;
        notifyUnauthorized();
        throw gone;
      }
      var lastError = null;

      for (var attempt = 0; attempt <= REFRESH_BACKOFF.length; attempt++) {
        try {
          var session = await postRefresh(tokenNow);
          publishSession(session);   // 广播出去，其他标签页不必再刷
          return session;
        } catch (error) {
          lastError = error;
          if (!refreshRetryable(error) || attempt === REFRESH_BACKOFF.length) break;
          await sleep(REFRESH_BACKOFF[attempt] + Math.floor(Math.random() * 300));
          // 退避期间别的标签页成功了就直接采用，不再重复打接口
          var shared = readShare(startedAt);
          if (shared) return adoptShared(shared);
          tokenNow = currentRefreshToken() || tokenNow;
        }
      }

      if (refreshRetryable(lastError)) {
        // 明确标记「暂时不可用」：调用方不得据此清登录态。
        lastError.transient = true;
      } else {
        // refresh token 真的失效 = 会话已终止。无论从哪条路径发现（普通请求 / 附件 /
        // 记账本），都在这里统一登出，避免「提示登录已失效但 session 还在」的半死状态。
        lastError.authInvalid = true;
        notifyUnauthorized();
      }
      throw lastError;
    });
  }

  function refreshSession() {
    if (refreshPromise) return refreshPromise;
    refreshPromise = performRefresh().finally(function () { refreshPromise = null; });
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
      } catch (error) {
        // 只有「refresh token 真的失效」才登出。限流(429)/网络/5xx 属于暂时故障，
        // 清 session 会让用户以为账号被踢下线，实际等几秒就恢复。
        if (!error || !error.transient) notifyUnauthorized();
        throw error;
      }
    }

    // 网关或应用层限流：幂等请求（GET）自动退避重试一次。
    // 不重试写请求——无法判断服务端是否已经执行，重放可能造成重复入账。
    if (response.status === 429 && !retried && String(options.method || 'GET').toUpperCase() === 'GET') {
      await sleep(1200 + Math.floor(Math.random() * 600));
      return request(path, options, true);
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
        skipUnauthorized: true,
        body: JSON.stringify({
          email: email,
          passwordHash: derived.hashB64,
          kdfSalt: saltB64,
          wrappedDek: wrapped.wrappedB64,
          dekIv: wrapped.ivB64,
          // 注册必须先过图形验证码；两个字段由调用方（MD.captcha.value()）提供。
          captchaId: credentials.captchaId || '',
          captchaCode: credentials.captchaCode || ''
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
        // 登录失败的 401 是业务结果（口令不对），必须作为普通错误抛给表单显示；
        // 走全局未授权流程会 clearSession + 重渲染登录页，把错误提示吞掉。
        skipUnauthorized: true,
        body: JSON.stringify({ email: email, passwordHash: derived.hashB64 })
      });
      var session = setSession(normalizeSession(payload, { kdfSalt: saltB64 }));
      rememberAccountSalt(email, session.kdfSalt || saltB64);

      // 管理员用「保留数据」模式改过口令：服务端换了口令与盐，但信封仍是旧口令包裹的，
      // 拿新口令去解必然失败（表现为"口令对了却进不去"）。此时改用本机缓存的 DEK，
      // 并用新口令重新包裹一份回填 —— 数据一条不动，服务端全程拿不到 DEK。
      if (session.envelopeStale) {
        var localDek = getDEK();
        if (!localDek) {
          throw new Error('这个账号的口令已被管理员重置。数据没有丢：请先用一台曾经登录过该账号的设备打开一次以恢复密钥（那台设备会自动完成），再来这里登录。');
        }
        setDEK(localDek);
        try {
          var repairSalt = MD.crypto.b64ToBytes(session.kdfSalt || saltB64);
          var rewrapped = await MD.crypto.wrapDEK(localDek, password, repairSalt);
          await request('/auth/repair-envelope', {
            method: 'POST',
            skipAuthRefresh: true,
            skipUnauthorized: true,
            body: JSON.stringify({ wrappedDek: rewrapped.wrappedB64, dekIv: rewrapped.ivB64 })
          });
          session.envelopeStale = false;
          session.wrappedDek = rewrapped.wrappedB64;
          session.dekIv = rewrapped.ivB64;
          setSession(session);
        } catch (e) {
          // 修复失败不影响本次使用：本机已有 DEK，能正常读写；
          // 下次登录若仍是旧信封，会再试一次。
        }
        return session;
      }

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
    // 当前账号信息：邮箱、是否开启托管恢复、是否管理员（isAdmin 决定「网站后台」入口）。
    me: function () { return request('/auth/me'); },
    push: function (changes) {
      return request('/sync/push', { method: 'POST', body: JSON.stringify({ changes: changes, items: changes }) });
    },
    pull: function (cursor, limit) {
      var params = new URLSearchParams({ cursor: String(cursor || 0), limit: String(limit || 300) });
      return request('/sync/pull?' + params.toString());
    },
    stats: function () { return request('/sync/stats'); },
    // 服务端要求确认词 DELETE-ALL，缺了会 400（历史实现只发 {}，导致"清空云端"从未生效）。
    reset: function () {
      return request('/sync/reset', { method: 'POST', body: JSON.stringify({ confirm: 'DELETE-ALL' }) });
    },
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
        } catch (error) {
          if (!error || !error.transient) notifyUnauthorized();
          throw error;
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
    changePassword: changePassword,

    /** 取托管恢复公钥（未启用时 enabled=false）。 */
    recoveryKey: function () { return request('/auth/recovery-key', { skipAuthRefresh: true }); },

    /**
     * 开启/关闭托管恢复。
     *
     * 开启后，服务端持有一份被恢复公钥包裹的 DEK —— 管理员可以直接改口令且数据无损，
     * 任何设备都能立刻用新口令登录。代价是持有恢复私钥的一方可解密该账号数据，
     * 因此这是用户自愿开启的开关，默认关闭。
     */
    setRecoveryEnvelope: function (wrappedB64, ivB64) {
      return request('/auth/recovery-envelope', {
        method: 'POST',
        body: JSON.stringify({ wrappedDek: wrappedB64 || '', dekIv: ivB64 || '' })
      });
    },
    // 底层请求封装对外暴露：WebDAV 等扩展模块需要复用同一套 Bearer 鉴权与 401 refresh，
    // 否则各自裸 fetch 会丢掉自动续期。仅供本仓库内部模块使用。
    request: request,
    setDEK: setDEK,
    hasSession: function () {
      if (Boolean(storageGet('access') && storageGet('userId'))) return true;
      // PWA 重开场景：sessionStorage 已空，但本机持久化了 refresh + DEK → 可静默恢复
      return Boolean(storageGet('refresh') && storageGet('dek'));
    }
  };
})(window);

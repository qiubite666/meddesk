/* ==========================================================================
 * family-link.js —— 家庭关联「8 位数字短码」客户端库
 *
 * 依赖（只读调用）：
 *   api.js   ：MD.api.request（自带 Bearer 与 401 自动续期）、MD.api.getDEK
 *   crypto.js：MD.crypto.wrapDEK / unwrapDEK / encrypt / decrypt
 *              generateSalt / bytesToB64 / b64ToBytes
 *
 * 【为什么分两步取码】
 * 短码要先由服务端生成（产品要求随机、非用户输入），而「用短码派生 KEK 包裹
 * 自己的 DEK」这一步又必须知道短码本身。所以流程拆成：
 *   ① POST /family/grants/code   取一个 8 位数字
 *   ② 本地用该码派生 KEK，包裹自己的 DEK
 *   ③ POST /family/grants        提交「码 + 信封」
 * 服务端全程不保存明文码，只存 HMAC(pepper, code) 作为查找键。
 *
 * 【解密链路】
 * 查看方兑换拿到 {kdfSalt, wrappedDek, dekIv} → 用同一个短码派生 KEK →
 * 解出 owner 的 DEK → 用它解密 GET /family/records 返回的密文。
 * owner 的 DEK 只缓存在内存，不落盘，页面刷新即失效（下次需重新兑换）。
 *
 * 【一次性语义】
 * 短码只能兑换一次：服务端只在 status='pending' 时匹配，兑换后即绑定查看方。
 * 重复兑换同一码只会返回「已被使用」，不会产生第二条授权。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var CODE_LEN = 8;
  var dekCache = Object.create(null); // grantId -> Uint8Array（仅内存）

  /** 去掉空格/连字符并校验是否 8 位纯数字；非法返回 null */
  function normalizeCode(raw) {
    var digits = String(raw == null ? '' : raw).replace(/[^0-9]/g, '');
    return digits.length === CODE_LEN ? digits : null;
  }

  /** 展示用：1234 5678，便于口述与核对 */
  function formatCode(raw) {
    var code = normalizeCode(raw);
    if (!code) return String(raw == null ? '' : raw);
    return code.slice(0, 4) + ' ' + code.slice(4);
  }

  function requireAuth() {
    if (!MD.api || typeof MD.api.request !== 'function') throw new Error('登录状态不可用');
    var dek = MD.api.getDEK ? MD.api.getDEK() : null;
    if (!dek) throw new Error('账号未解锁，请重新登录');
    return dek;
  }

  /** ① 向服务端要一个新短码 */
  async function requestCode() {
    requireAuth();
    var resp = await MD.api.request('/family/grants/code', { method: 'POST' });
    var code = normalizeCode(resp && resp.code);
    if (!code) throw new Error('服务端返回的短码格式异常');
    return code;
  }

  /**
   * ②③ 生成授权：取码 → 包裹自己的 DEK → 提交信封。
   * @returns {Promise<{code:string, grantId:string, expiresAt:number}>}
   */
  async function createGrant() {
    var myDek = requireAuth();
    var code = await requestCode();

    var salt = MD.crypto.generateSalt();
    var wrapped = await MD.crypto.wrapDEK(myDek, code, salt);
    var payload = {
      code: code,
      kdfSalt: MD.crypto.bytesToB64(salt),
      wrappedDek: wrapped.wrappedB64,
      dekIv: wrapped.ivB64
    };

    var resp = await MD.api.request('/family/grants', {
      method: 'POST',
      body: JSON.stringify(payload)
    });
    return {
      code: code,
      grantId: resp && resp.grantId,
      expiresAt: (resp && resp.expiresAt) || 0
    };
  }

  /**
   * 兑换短码并解出 owner 的 DEK（内存缓存）。
   * @returns {Promise<{grantId:string, ownerId:string, expiresAt:number}>}
   */
  async function redeem(rawCode) {
    var code = normalizeCode(rawCode);
    if (!code) throw new Error('请输入 8 位数字短码');

    var g = await MD.api.request('/family/grants/redeem', {
      method: 'POST',
      body: JSON.stringify({ code: code })
    });
    if (!g || !g.grantId || !g.wrappedDek) throw new Error('兑换响应异常');

    var salt = MD.crypto.b64ToBytes(g.kdfSalt);
    var ownerDek;
    try {
      ownerDek = await MD.crypto.unwrapDEK(g.wrappedDek, g.dekIv, code, salt);
    } catch (e) {
      // AES-GCM 校验失败基本只有一种原因：拿到的信封与码不匹配
      throw new Error('短码与授权不匹配，请确认输入正确');
    }
    dekCache[g.grantId] = ownerDek;
    return { grantId: g.grantId, ownerId: g.ownerId || '', expiresAt: g.expiresAt || 0 };
  }

  function hasDek(grantId) { return Boolean(dekCache[grantId]); }

  /**
   * 拉取并解密 owner 的记录。
   * @param {string} grantId
   * @param {{sinceSeq?:number, limit?:number}} [opts]
   * @returns {Promise<{records:Array, nextSeq:number, hasMore:boolean}>}
   */
  async function fetchRecords(grantId, opts) {
    opts = opts || {};
    var ownerDek = dekCache[grantId];
    if (!ownerDek) throw new Error('授权未解锁，请重新输入短码');

    var q = '/family/records?grantId=' + encodeURIComponent(grantId)
      + '&sinceSeq=' + (Number(opts.sinceSeq) || 0)
      + '&limit=' + (Number(opts.limit) || 200);

    var resp = await MD.api.request(q, { method: 'GET' });
    var items = (resp && resp.items) || [];

    var records = [];
    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      var rec = {
        table: it.table,
        id: it.recordId,
        ver: it.ver,
        updatedAt: it.updatedAt,
        deleted: Boolean(it.deleted),
        seq: it.seq,
        data: null
      };
      // 删除标记的记录没有可解正文，只在同步层有意义
      if (!it.deleted && it.iv && it.ct) {
        try {
          var plain = await MD.crypto.decrypt(ownerDek, it.iv, it.ct);
          rec.data = JSON.parse(plain);
        } catch (e) {
          rec.data = null;   // 单条解不开不能拖垮整批
        }
      }
      records.push(rec);
    }
    return { records: records, nextSeq: (resp && resp.nextSeq) || 0, hasMore: Boolean(resp && resp.hasMore) };
  }

  /** 我作为所有者的授权列表（不含密钥材料） */
  async function listGrants() {
    var resp = await MD.api.request('/family/grants', { method: 'GET' });
    return (resp && resp.items) || [];
  }

  /** 撤销授权；成功后清掉本地缓存，避免继续用旧 DEK 拉取 */
  async function revoke(grantId) {
    await MD.api.request('/family/grants/' + encodeURIComponent(grantId), { method: 'DELETE' });
    delete dekCache[grantId];
  }

  /** 仅测试用：清空内存缓存 */
  function clearCache() { dekCache = Object.create(null); }

  MD.familyLink = {
    CODE_LEN: CODE_LEN,
    normalizeCode: normalizeCode,
    formatCode: formatCode,
    requestCode: requestCode,
    createGrant: createGrant,
    redeem: redeem,
    hasDek: hasDek,
    fetchRecords: fetchRecords,
    listGrants: listGrants,
    revoke: revoke,
    clearCache: clearCache
  };
})(typeof window !== 'undefined' ? window : this);

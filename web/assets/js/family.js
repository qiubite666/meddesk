/* ==========================================================================
 * family.js —— MedDesk「家庭健康」
 *
 * 依赖（只读调用，不修改其文件）：
 *   core.js  ：MD.ui / MD.icon / MD.toast / MD.router / MD.util
 *   data.js  ：MD.data.family / blobs / meds / intakes
 *   sched.js ：MD.sched.adherence（依从性统计）
 *   crypto.js：MD.crypto.derive / wrapDEK / unwrapDEK / encrypt / decrypt
 *              generateSalt / bytesToB64 / b64ToBytes / newDEK
 *
 * 【为什么用「关联码」而不是服务端直连】
 * MedDesk 是端到端加密的：业务数据用每个账号自己的 DEK 加密后上传，
 * 服务端只有密文信封，**它既不知道该给谁看什么，也没有能力解密**。
 * 因此「关联账号后查看对方数据」不能靠服务端转发，只能由数据所有方
 * 主动把一份快照交给对方。
 *
 * 本模块的做法（不新增后端接口、不破坏 E2E）：
 *   1. 数据所有方在「家庭关联」页输入一段口令，导出被该口令加密的快照，
 *      得到一段「关联码」（纯文本）。
 *   2. 需要查看的一方粘贴关联码并输入同一口令，即可解出快照，落到某个
 *      家庭档案成员下。
 *   快照加密链：随机 32B 内容密钥 → AES-GCM 加密快照正文，
 *              再用口令派生的 KEK 包裹内容密钥（复用 wrapDEK，不新造密码学）。
 *   服务端、本模块都不保存口令；口令丢失等于关联码作废，需要重新导出。
 *
 * 【已知限制，界面上也明说】
 *   关联码是**快照**而非实时同步：对方后续的数据变化不会自动出现，
 *   需要重新生成一次。真正的实时共享需要双方在线协商共享密钥，
 *   属于后续迭代（需要后端配合记录「谁被授权读谁」）。
 *
 * 【家庭档案与账号相互独立】
 *   档案条目存在 family 表里（随账号同步），字段由用户自己填，不绑定账号；
 *   只有「已关联」的成员才附带快照引用（family:<id> 的 blob）。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  var DASH = '—';
  var DAY_MS = 24 * 60 * 60 * 1000;
  var RELATIONS = ['本人', '父亲', '母亲', '配偶', '子女', '祖父母', '其他'];

  var STATUS_CN = {
    taken: '已服用',
    skipped: '已跳过',
    missed: '漏服',
    snoozed: '已推迟',
    pending: '待服用'
  };

  // 页面内状态：单一路由，内部切换视图，避免为每个子页注册路由
  var S = {
    tab: 'archive', view: 'list', memberId: null,
    msg: '', err: '', busy: false,
    lastCode: '',      // 8 位短码（新方式）
    legacyCode: '',    // 长码快照（旧方式，独立字段，避免互相覆盖）
    showLegacy: false  // 是否展开离线快照区
  };

  function nowMs() {
    return (MD.util && typeof MD.util.nowMs === 'function') ? MD.util.nowMs() : Date.now();
  }

  function el(tag, props, children) {
    return MD.ui.el(tag, props, children);
  }

  function clear(node) {
    MD.ui.clear(node);
  }

  function pad2(n) { return n < 10 ? '0' + n : String(n); }

  function fmtDate(ms) {
    if (!ms || !isFinite(ms)) return DASH;
    var d = new Date(ms);
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  }

  function fmtDateTime(ms) {
    if (!ms || !isFinite(ms)) return DASH;
    var d = new Date(ms);
    return fmtDate(ms) + ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  function pct(rate) {
    if (rate == null || !isFinite(rate)) return DASH;
    return Math.round(rate * 1000) / 10 + '%';
  }

  function uid() {
    return 'fm' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
  }

  function toast(text, kind) {
    if (MD.toast && typeof MD.toast.show === 'function') MD.toast.show(text, kind || 'success');
  }

  // crypto.js 的 toBase64Url / fromBase64Url 只处理字节，不处理字符串，
  // 所以中文内容（药名、备注）必须先做 UTF-8 编解码，否则会被当成 base64 解析。
  var utf8Encoder = new TextEncoder();
  var utf8Decoder = new TextDecoder();

  function utf8Encode(str) { return utf8Encoder.encode(String(str)); }
  function utf8Decode(bytes) { return utf8Decoder.decode(bytes); }

  /* ============================ 1. 数据层 ============================ */

  // family 表里有三类记录，靠字段形态区分身份：
  //   1) id = 'grant:<grantId>'     授权与密钥（ownerId / dekIv / dekCt / linkedAt）
  //   2) id = 'lp-<ownerProfileId>' 关联投影档案（ownerProfileId + grantId + 可改的姓名/关系/备注）
  //   3) profileId 非空             迁移前的旧式单档案关联（一个授权只能挂一个档案）
  //
  // 【为什么投影档案放 family 而不是 profile】
  // 投影代表"别人家的人"。一旦写进 profile 表，用药提醒的档案切换器就会列出它，
  // 用户能给它建排程——而那些排程会带着 owner 的 profileId 落进自己的账号，产生脏数据。
  // 放在 family 表天然与用药提醒隔离，也不需要改 med.js 的任何逻辑。
  function grantRowId(grantId) { return 'grant:' + String(grantId || ''); }
  function linkedRowId(ownerProfileId) { return 'lp-' + String(ownerProfileId || ''); }

  async function listFamilyRows() {
    var rows = [];
    try { rows = await MD.data.family.list(); } catch (e) { return []; }
    return (rows || []).filter(function (r) { return r && r.deleted !== true; });
  }

  // 把原始行按身份分三桶。判据顺序有讲究：
  // 先看 ownerProfileId（投影最具体），再看 id 前缀（grant），最后才是旧式。
  function classifyRows(rows) {
    var grants = {}, profileLinks = {}, linked = [];
    (rows || []).forEach(function (r) {
      if (!r) return;
      if (r.ownerProfileId) { linked.push(r); return; }
      if (r.grantId && /^grant:/.test(String(r.id || ''))) { grants[String(r.grantId)] = r; return; }
      var pid = r.profileId || r.id;
      if (pid) profileLinks[pid] = r;
    });
    return { grants: grants, profileLinks: profileLinks, linked: linked };
  }

  async function loadIndex() { return classifyRows(await listFamilyRows()); }

  /* ---------- 授权与密钥（按 grant 存，一个授权对应多个投影档案） ---------- */

  async function getGrant(grantId) {
    if (!grantId) return null;
    var idx = await loadIndex();
    return idx.grants[String(grantId)] || null;
  }

  async function saveGrant(grantId, patch) {
    var existing = await getGrant(grantId);
    var rec = Object.assign({}, existing || {}, {
      id: grantRowId(grantId),
      grantId: String(grantId),
      updatedAt: nowMs()
    }, patch || {});
    rec.id = grantRowId(grantId);          // patch 里不允许改主键
    rec.grantId = String(grantId);
    if (!rec.linkedAt) rec.linkedAt = nowMs();
    await MD.data.family.save(rec);
    return rec;
  }

  // 撤销/失效时清掉整个授权：密钥材料、投影档案一并删除。
  // 只删本账号的记录，服务端那条 grant 由数据所有方自己处置。
  async function dropGrant(grantId) {
    var idx = await loadIndex();
    var g = idx.grants[String(grantId)];
    if (g && g.id) { try { await MD.data.family.remove(g.id); } catch (e) { /* 忽略 */ } }
    for (var i = 0; i < idx.linked.length; i++) {
      var r = idx.linked[i];
      if (String(r.grantId || '') === String(grantId) && r.id) {
        try { await MD.data.family.remove(r.id); } catch (e) { /* 忽略 */ }
        try { await dropSnapshot(r.id); } catch (e) { /* 忽略 */ }
      }
    }
    if (MD.familyLink && typeof MD.familyLink.forgetDek === 'function') {
      try { MD.familyLink.forgetDek(grantId); } catch (e) { /* 忽略 */ }
    }
  }

  /* ---------- 投影档案读写 ---------- */

  async function upsertLinked(ownerProfileId, patch) {
    var id = linkedRowId(ownerProfileId);
    var rows = await listFamilyRows();
    var existing = null;
    for (var i = 0; i < rows.length; i++) if (rows[i].id === id) { existing = rows[i]; break; }
    var rec = Object.assign({}, existing || {}, {
      id: id,
      ownerProfileId: String(ownerProfileId),
      updatedAt: nowMs()
    }, patch || {});
    rec.id = id;
    if (!rec.createdAt) rec.createdAt = nowMs();
    await MD.data.family.save(rec);
    return rec;
  }

  async function dropLinked(ownerProfileId) {
    var id = linkedRowId(ownerProfileId);
    try { await MD.data.family.remove(id); } catch (e) { /* 忽略 */ }
    try { await dropSnapshot(id); } catch (e) { /* 忽略 */ }
  }

  // 档案列表 = 本账号档案（profile 表） + 关联投影档案（family 表）。
  //
  // 【为什么本账号档案读 profile】
  // 用药提醒和家庭健康原本各有一张档案表（profile / family），同一个"母亲"
  // 要在两处各建一遍。现在本账号档案只有一份（profile），家庭健康不再另存一套。
  //
  // 【投影档案为什么单列】
  // 它是"别人家的人"在本地的影子：姓名/关系/备注可在本地改（不同步回对方），
  // 药物清单仍然实时读对方账号。两者混在一张表会互相污染，所以分开存、合并显示。
  async function listMembers() {
    var profiles = [], idx = { grants: {}, profileLinks: {}, linked: [] };
    try { profiles = await MD.data.profiles.list(); } catch (e) { profiles = []; }
    try { idx = await loadIndex(); } catch (e) { /* 保留空索引 */ }

    var out = [];

    (profiles || []).filter(function (p) {
      return p && p.deleted !== true && !p.archived;
    }).forEach(function (p) {
      var link = idx.profileLinks[p.id] || {};
      out.push({
        id: p.id,
        source: 'local',
        name: p.name || '未命名',
        relation: p.relation || '',
        note: p.note || '',
        color: p.color || '',
        birthDate: p.birthDate || '',
        createdAt: p.createdAt || 0,
        updatedAt: p.updatedAt || 0,
        // 旧式关联：迁移前一个授权只挂一个档案（密钥也挂在档案上）。
        // 新模型里密钥挂 grant，所以这里只记录 legacyGrantId，供详情页降级读取。
        grantId: '',
        legacyGrantId: link.grantId || '',
        ownerId: link.ownerId || '',
        dekIv: link.dekIv,
        dekCt: link.dekCt,
        linkedAt: Number(link.linkedAt) || 0,
        snapshotAt: Number(link.snapshotAt) || Number(link.linkedAt) || 0
      });
    });

    (idx.linked || []).filter(function (r) {
      // 对方已删掉的档案：隐藏但保留记录（本账号可能写过备注，物理删不可恢复）
      return r && !r.ownerDeleted;
    }).forEach(function (r) {
      out.push({
        id: r.id,
        source: 'linked',
        name: r.name || '未命名',
        relation: r.relation || '',
        note: r.note || '',
        color: r.color || '',
        birthDate: r.birthDate || '',
        createdAt: r.createdAt || 0,
        updatedAt: r.updatedAt || 0,
        grantId: r.grantId || '',
        ownerProfileId: r.ownerProfileId || '',
        ownerId: r.ownerId || '',
        // 投影不持有密钥，密钥在 grant 行上；列表里只透传 grantId
        linkedAt: Number(r.linkedAt) || 0
      });
    });

    // 本账号档案在前（自己的家人优先），投影按关联时间在后
    return out.sort(function (a, b) {
      if (a.source !== b.source) return a.source === 'local' ? -1 : 1;
      if (a.source === 'local') return (a.createdAt || 0) - (b.createdAt || 0);
      return (a.linkedAt || 0) - (b.linkedAt || 0);
    });
  }

  /* ---------- 关联信息的读写 ---------- */

  async function getLink(profileId) {
    var map = await listLinks();
    return map[profileId] || null;
  }

  async function saveLink(profileId, patch) {
    var existing = await getLink(profileId);
    var rec = Object.assign({ id: uid(), profileId: profileId }, existing || {}, patch || {});
    rec.id = existing ? existing.id : rec.id;
    rec.profileId = profileId;
    rec.updatedAt = nowMs();
    if (!rec.linkedAt) rec.linkedAt = nowMs();
    if (!rec.createdAt) rec.createdAt = rec.linkedAt;
    await MD.data.family.save(rec);
    return rec;
  }

  async function dropLink(profileId) {
    var existing = await getLink(profileId);
    if (existing && existing.id) {
      try { await MD.data.family.remove(existing.id); } catch (e) { /* 忽略 */ }
    }
  }

  // 表单只提交姓名/关系/备注。出生日期、主题色这些属于用药提醒侧的字段
  // 必须从原记录原样带回 —— 否则在家庭健康点一次保存就会把它们清空。
  async function saveProfileFromForm(rec) {
    var base = null;
    try { base = await MD.data.profiles.get(rec.id); } catch (e) { base = null; }
    var merged = Object.assign({}, base || {}, {
      id: rec.id,
      name: rec.name,
      relation: rec.relation,
      note: rec.note,
      archived: base ? Boolean(base.archived) : false,
      createdAt: (base && base.createdAt) ? base.createdAt : rec.createdAt,
      updatedAt: rec.updatedAt
    });
    if (!merged.color) merged.color = '#4f8cff';
    return MD.data.profiles.save(merged);
  }

  // 旧数据迁移：迁移前的 family 记录自带 name/relation（它本身就是档案）。
  // 现在档案归 profile，family 只留关联信息，所以把带 name 的记录提升为 profile，
  // 再补一条只含关联信息的记录，最后软删旧记录。
  // 幂等：profile 复用旧档案 id，重复执行只是覆盖同一条；link 已存在则跳过。
  async function migrateLegacyMembers() {
    var rows = [];
    try { rows = await MD.data.family.list(); } catch (e) { return 0; }
    var legacy = (rows || []).filter(function (r) {
      return r && r.deleted !== true && (r.name || r.relation);
    });
    if (!legacy.length) return 0;

    var changed = 0;
    for (var i = 0; i < legacy.length; i++) {
      var old = legacy[i];
      try {
        await MD.data.profiles.save({
          id: old.id,
          name: old.name || '未命名',
          relation: old.relation || '其他',
          note: old.note || '',
          color: '#4f8cff',
          archived: false,
          createdAt: old.createdAt || nowMs(),
          updatedAt: nowMs()
        });
        // 旧式关联（一个授权只挂一个档案，密钥也在这条记录上）：
        // 把密钥提升到 grant 行，授权从此覆盖对方的**全部**档案。
        // 原 profile 保留为本账号档案（不再挂授权），投影由后续同步补齐。
        if (old.grantId) {
          var gExisting = await getGrant(old.grantId);
          if (!gExisting) {
            await saveGrant(old.grantId, {
              ownerId: old.ownerId || '',
              dekIv: old.dekIv,
              dekCt: old.dekCt,
              linkedAt: old.linkedAt || nowMs(),
              legacyProfileId: old.id
            });
          }
        }
        await MD.data.family.remove(old.id);
        changed++;
      } catch (e) {
        // 单条失败不阻断其余档案，下次进入页面会重试
      }
    }
    return changed;
  }

  // 把「一个授权挂一个档案」的旧式关联升级为「一个授权覆盖全部档案」。
  //
  // 识别条件：记录上有 profileId + grantId + dekIv，且不是投影、不是 grant 行。
  // 上一版兑换时把密钥写在这种记录上（saveLink(profileId, {...})），
  // 它没有 name 字段，所以 migrateLegacyMembers 的过滤条件抓不到它 ——
  // 必须单独扫一遍，否则这些授权会变成"有密钥但没有 grant 行"的孤儿：
  // 投影建不出来，刷新档案也遍历不到。
  async function migrateLegacyGrants() {
    var idx = await loadIndex();
    var n = 0;
    var ids = Object.keys(idx.profileLinks || {});
    for (var i = 0; i < ids.length; i++) {
      var pid = ids[i];
      var link = idx.profileLinks[pid];
      if (!link || !link.grantId || !link.dekIv || !link.dekCt) continue;
      if (!idx.grants[String(link.grantId)]) {
        await saveGrant(link.grantId, {
          ownerId: link.ownerId || '',
          dekIv: link.dekIv,
          dekCt: link.dekCt,
          linkedAt: Number(link.linkedAt) || nowMs(),
          legacyProfileId: pid
        });
        n++;
      }
      // 旧 link 记录已无用（密钥搬到 grant 行，profile 本身保留为本账号档案）
      try { await MD.data.family.remove(link.id || pid); } catch (e) { /* 忽略 */ }
    }
    return n;
  }

  async function getMember(id) {
    var list = await listMembers();
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  function blobKey(id) { return 'family:' + id; }

  async function putSnapshot(id, data) {
    return MD.data.blobs.save(blobKey(id), JSON.stringify(data));
  }

  async function getSnapshot(id) {
    try {
      var raw = await MD.data.blobs.get(blobKey(id));
      if (!raw) return null;
      return typeof raw === 'string' ? JSON.parse(raw) : raw;
    } catch (e) {
      return null;
    }
  }

  async function dropSnapshot(id) {
    try { await MD.data.blobs.remove(blobKey(id)); } catch (e) { /* 忽略 */ }
  }

  /* ============================ 2. 采集本账号数据 ============================ */

  // 组装一份可分享的快照：只含药物清单、用药历史、依从性，不含备注/附件/设置。
  async function collect(days) {
    var span = Math.max(1, Math.min(365, Number(days) || 90));
    var fromMs = nowMs() - span * DAY_MS;

    var meds = [], intakes = [], schedules = [];
    try { meds = await MD.data.meds.list(); } catch (e) { meds = []; }
    try { intakes = await MD.data.intakes.list(); } catch (e) { intakes = []; }
    try { schedules = await MD.data.schedules.list(); } catch (e) { schedules = []; }

    var medMap = {};
    (meds || []).forEach(function (m) { if (m && m.id) medMap[m.id] = m; });

    var schedByMed = {};
    (schedules || []).forEach(function (sc) {
      if (!sc || sc.deleted === true || !sc.medicationId) return;
      if (!schedByMed[sc.medicationId]) schedByMed[sc.medicationId] = sc;
    });

    var medList = (meds || []).filter(function (m) { return m && m.deleted !== true; })
      .map(function (m) {
        return {
          name: m.name || '未命名药物',
          dosage: doseTextOf(m),
          strength: m.strength || '',
          // 与主链路同口径：离线快照也要能看到频次
          schedText: schedTextOf(schedByMed[m.id])
        };
      });

    var history = (intakes || []).filter(function (it) {
      if (!it || it.deleted === true) return false;
      var t = Number(it.scheduledFor || 0);
      return t >= fromMs && t <= nowMs();
    }).map(function (it) {
      var med = medMap[it.medicationId];
      return {
        t: Number(it.scheduledFor || 0),
        s: String(it.status || 'pending'),
        n: med ? (med.name || '未命名药物') : '未知药物'
      };
    }).sort(function (a, b) { return b.t - a.t; }).slice(0, 400);

    var adherence = { total: 0, taken: 0, skipped: 0, missed: 0, rate: null, streakDays: 0 };
    if (MD.sched && typeof MD.sched.adherence === 'function') {
      adherence = MD.sched.adherence({ intakes: intakes, fromMs: fromMs, toMs: nowMs() });
    }

    return {
      v: 1,
      exportedAt: nowMs(),
      days: span,
      meds: medList,
      history: history,
      adherence: adherence
    };
  }

  /* ============================ 3. 关联码 ============================ */

  /**
   * 生成关联码。
   * 加密链：新建随机内容密钥 → 用它 AES-GCM 加密快照正文 →
   *         用「口令 + 随机盐」派生的 KEK 包裹该内容密钥。
   * 关联码本身携带盐与两段密文，不含口令，也不含任何明文。
   */
  async function makeCode(pass, days) {
    if (!pass || String(pass).length < 6) throw new Error('口令至少 6 位');
    var payload = await collect(days);

    var contentKey = MD.crypto.newDEK();
    var body = await MD.crypto.encrypt(contentKey, JSON.stringify(payload));
    var salt = MD.crypto.generateSalt();
    var wrapped = await MD.crypto.wrapDEK(contentKey, String(pass), salt);

    var envelope = {
      v: 1,
      salt: MD.crypto.bytesToB64(salt),
      wrap: wrapped.wrappedB64,
      wrapIv: wrapped.ivB64,
      iv: body.ivB64,
      ct: body.ctB64
    };
    // 注意：toBase64Url 要的是「字节」，不是字符串。
    // 直接传 JSON 字符串会被 toBytes 判定为 base64 而走 atob，报
    // "The string to be decoded is not correctly encoded"。必须先 UTF-8 编码。
    return MD.crypto.toBase64Url(utf8Encode(JSON.stringify(envelope)));
  }

  async function readCode(code, pass) {
    if (!code) throw new Error('请粘贴关联码');
    if (!pass || String(pass).length < 6) throw new Error('口令至少 6 位');

    // 粘贴时常带换行/空格，先剔掉再解码
    var cleaned = String(code).replace(/[\s\r\n]+/g, '');
    var bytes = MD.crypto.fromBase64Url(cleaned);
    var envelope = JSON.parse(utf8Decode(bytes));
    if (!envelope || envelope.v !== 1) throw new Error('关联码格式不支持');

    var salt = MD.crypto.b64ToBytes(envelope.salt);
    var contentKey;
    try {
      contentKey = await MD.crypto.unwrapDEK(envelope.wrap, envelope.wrapIv, String(pass), salt);
    } catch (e) {
      // 口令错时 AES-GCM 校验失败，必须给出可理解的提示而不是抛原始错误
      throw new Error('口令不正确，或该关联码已损坏');
    }
    var plain = await MD.crypto.decrypt(contentKey, envelope.iv, envelope.ct);
    var data = JSON.parse(plain);
    if (!data || data.v !== 1) throw new Error('关联码内容版本不支持');
    return data;
  }

  /* ============================ 4. 视图：家庭成员表单 ============================ */

  function memberForm(host, member, done) {
    clear(host);
    var draft = {
      name: member ? (member.name || '') : '',
      relation: member ? (member.relation || '其他') : '其他',
      note: member ? (member.note || '') : ''
    };

    function inputRow(label, node) {
      return el('div', { class: 'field' }, [
        el('span', { class: 'label', text: label }),
        node
      ]);
    }

    var nameInput = el('input', {
      class: 'input', type: 'text', placeholder: '例如 张伟', autocomplete: 'off',
      onInput: function (e) { draft.name = e.target.value; }
    });
    nameInput.value = draft.name;

    // 旧档案的关系值可能是已废弃的「父母」（枚举改成父亲/母亲之前写入的）。
    // 若不把它显式列出来，下拉没有任何 selected → 浏览器默认选第一项「本人」，
    // 用户一打开编辑再保存，真实关系就被静默改坏了。这里补一个标注为旧值的选项。
    var relationOptions = RELATIONS.slice();
    var isLegacyRelation = Boolean(draft.relation) && RELATIONS.indexOf(draft.relation) < 0;
    if (isLegacyRelation) relationOptions.push(draft.relation);

    var relationSelect = el('select', {
      class: 'select',
      onChange: function (e) { draft.relation = e.target.value; }
    }, relationOptions.map(function (r) {
      var label = (isLegacyRelation && r === draft.relation) ? r + '（旧值）' : r;
      var opt = el('option', { value: r, text: label });
      if (r === draft.relation) opt.selected = true;
      return opt;
    }));

    var noteInput = el('input', {
      class: 'input', type: 'text', placeholder: '例如 高血压，每日两次（可选）', autocomplete: 'off',
      onInput: function (e) { draft.note = e.target.value; }
    });
    noteInput.value = draft.note;

    var errorBox = el('p', { class: 'muted', style: { color: 'var(--danger)', minHeight: '18px', margin: '0' } });

    function submit() {
      if (!draft.name.trim()) { errorBox.textContent = '请填写姓名'; return; }
      done({
        id: member ? member.id : uid(),
        name: draft.name.trim(),
        relation: draft.relation,
        note: draft.note.trim(),
        createdAt: member ? member.createdAt : nowMs(),
        updatedAt: nowMs()
      });
    }

    host.appendChild(el('div', { class: 'stack', style: { gap: '12px' } }, [
      inputRow('姓名', nameInput),
      inputRow('关系', relationSelect),
      inputRow('备注', noteInput),
      errorBox,
      el('div', { class: 'row row--wrap', style: { gap: '10px' } }, [
        el('button', {
          class: 'btn btn-primary', type: 'button', text: member ? '保存修改' : '添加档案',
          onClick: submit
        }),
        el('button', {
          class: 'btn btn-ghost', type: 'button', text: '取消',
          onClick: function () { done(null); }
        })
      ])
    ]));
  }

  /* ============================ 5. 视图：档案详情 ============================ */

  function sectionCard(title, rows, tone) {
    return el('div', { class: 'card', style: { marginBottom: '12px' } }, [
      el('div', { style: { fontWeight: '700', marginBottom: '10px' } }, [el('span', { text: title })]),
      el('div', { class: 'stack', style: { gap: '8px' } }, rows.length ? rows : [
        el('span', { class: 'muted', text: '暂无数据' })
      ])
    ]);
  }

  function kvRow(k, v) {
    return el('div', {
      class: 'row',
      style: { justifyContent: 'space-between', alignItems: 'baseline', gap: '12px' }
    }, [
      el('span', { class: 'muted', text: k, style: { fontSize: '13px' } }),
      el('span', { text: v, style: { fontWeight: '600' } })
    ]);
  }

  /**
   * 渲染详情。刻意不出现「今天要处理」区块：
   * 家庭档案是查阅用途，不对他人数据产生待办压力。
   */
  function renderDetail(host, member, data) {
    clear(host);

    // 实时拉取失败会在这里留一条告警，避免用户以为看到的是最新数据
    if (S.err) {
      host.appendChild(el('p', {
        class: 'muted',
        text: S.err,
        style: { color: 'var(--danger)', fontSize: '13px', margin: '0 0 10px' }
      }));
    }

    host.appendChild(el('div', { class: 'row', style: { gap: '10px', alignItems: 'center', marginBottom: '14px' } }, [
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '返回',
        onClick: function () { S.view = 'list'; S.memberId = null; paint(); }
      }),
      el('div', { style: { display: 'flex', flexDirection: 'column', gap: '2px', minWidth: '0' } }, [
        el('div', { text: member.name || '未命名', style: { fontSize: '17px', fontWeight: '700' } }),
        el('span', {
          class: 'muted',
          // 本账号档案的数据就在本地，说"实时"会让人以为在连服务器；
          // 关联档案才是真的每次进页面从对方账号拉一次。
          text: (member.relation || '')
            + (data ? (' · ' + (member.source === 'linked'
              ? '实时同步 · ' + fmtDateTime(data.exportedAt)
              : '本账号数据')) : '')
        })
      ])
    ]));

    if (member.note) {
      host.appendChild(el('p', { class: 'muted', text: member.note, style: { margin: '0 0 12px', fontSize: '13px' } }));
    }

    if (!data) {
      var isLinkedMember = member.source === 'linked';
      host.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('home-heart'),
        el('strong', { text: isLinkedMember ? '暂时读不到对方数据' : '尚未关联数据' }),
        el('p', {
          class: 'muted',
          text: isLinkedMember
            ? '可能原因：本机密钥材料缺失、对方已撤销授权，或网络不可达。请让对方重新生成一次关联码再输入。'
            : '这个档案属于本账号，药物清单与用药历史来自「用药提醒」，建好药物后这里会自动显示。'
        }),
        el('div', { class: 'row', style: { gap: '10px', marginTop: '12px' } }, [
          el('button', {
            class: 'btn btn-primary', type: 'button',
            text: isLinkedMember ? '去关联' : '去用药提醒',
            onClick: function () {
              if (isLinkedMember) { S.tab = 'link'; S.view = 'list'; paint(); }
              else { MD.router.go('med'); }
            }
          }),
          el('button', {
            class: 'btn btn-ghost', type: 'button', text: '删除此档案',
            style: { color: 'var(--danger)' },
            onClick: async function () {
              if (await deleteMember(member)) { S.view = 'list'; S.memberId = null; }
              paint();
            }
          })
        ])
      ]));
      return;
    }

    // 1. 药物清单（放最上面：这是打开档案最想看的东西）
    host.appendChild(sharedMedsCard(member, data));

    // 2. 依从性统计（按要求放到药物清单下面）
    var adh = data.adherence || {};
    host.appendChild(sectionCard('依从性统计（近 ' + (data.days || 90) + ' 天）', [
      kvRow('依从率', pct(adh.rate)),
      kvRow('应服次数', String(adh.total == null ? DASH : adh.total)),
      kvRow('已服用', String(adh.taken == null ? DASH : adh.taken)),
      kvRow('已跳过', String(adh.skipped == null ? DASH : adh.skipped)),
      kvRow('漏服', String(adh.missed == null ? DASH : adh.missed)),
      kvRow('连续达标', (adh.streakDays == null ? DASH : adh.streakDays) + ' 天')
    ]));

    // 3. 用药历史
    var hist = (data.history || []).slice(0, 60);
    var histRows = hist.map(function (h) {
      return el('div', {
        class: 'row',
        style: { justifyContent: 'space-between', alignItems: 'baseline', gap: '12px' }
      }, [
        el('span', { text: fmtDateTime(h.t), style: { fontSize: '13px', flex: '0 0 auto' } }),
        el('span', { text: h.n, style: { fontSize: '13px', flex: '1 1 auto', minWidth: '0' } }),
        el('span', {
          text: STATUS_CN[h.s] || h.s,
          style: {
            fontSize: '12px',
            fontWeight: '700',
            color: h.s === 'taken' ? 'var(--ok)' : (h.s === 'missed' ? 'var(--danger)' : 'var(--text-dim)')
          }
        })
      ]);
    });
    host.appendChild(sectionCard('用药历史（最近 ' + hist.length + ' 条）', histRows));

    host.appendChild(el('p', {
      class: 'muted',
      text: data.realtime
        ? '数据实时读取自对方的账号（每次进入本页刷新）。对方撤销授权后将无法继续查看。'
        : '数据来自对方主动分享的离线快照，非实时。需要最新数据请让对方重新生成关联码。',
      style: { fontSize: '12px', margin: '4px 0 0' }
    }));

    // 详情页也要能删：看完数据想清掉这个档案是很自然的动作，
    // 否则用户必须退回列表再找一次。
    host.appendChild(el('div', { class: 'row', style: { marginTop: '16px' } }, [
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '删除此档案',
        style: { color: 'var(--danger)' },
        onClick: async function () {
          if (await deleteMember(member)) { S.view = 'list'; S.memberId = null; }
          paint();
        }
      })
    ]));
  }

  /* ============================ 5.9 删除档案 ============================ */

  // 删除档案：只动**本账号**的三样东西，绝不触碰关联人的任何数据。
  //
  // 允许操作的范围（全部属于本账号）：
  //   1. family 表里的这一条档案（软删，进本地同步队列）
  //   2. 本地快照 blobs（family:<id>，仅本机，不入同步）
  //   3. 内存里缓存的对方 DEK（forgetDek）
  //
  // 明确不做的事：
  //   - 不删除、不修改任何 medication / schedule / intake 记录
  //     （对方的数据在你这里只是内存里的视图，从未写入本地表）
  //   - 不调用撤销接口：DELETE /family/grants/:id 按 owner_id 限权，
  //     只有数据所有方能撤销；查看方删掉本地档案后服务端那条授权仍 active，
  //     但不影响对方数据，也不该由查看方替对方做决定。
  //
  // 回归保护：test-family-flow.js 里有断言，若这里被加上对
  // MD.data.meds / MD.data.schedules / MD.data.intakes 的写操作，测试会红。
  async function deleteMember(member) {
    if (!member) return false;
    var ok = await MD.ui.confirm({
      title: '删除档案',
      message: member.source === 'linked'
        ? ('确定移除「' + (member.name || '未命名') + '」？只删除本账号里的这份关联副本，'
          + '对方的账号、对方的档案与药物清单都不受影响。')
        : ('确定删除「' + (member.name || '未命名') + '」？这是本账号的档案，'
          + '删除后「用药提醒」里也会一起消失。'),
      confirmText: '删除',
      danger: true
    });
    if (!ok) return false;

    try {
      if (member.source === 'linked') {
        // 投影档案：只删本账号这份副本。对方的账号、对方的档案、授权关系都不动。
        await dropLinked(member.ownerProfileId);
        toast('已移除');
        return true;
      }

      await dropSnapshot(member.id);
      await dropLink(member.id);
      // 删的是本账号的档案主体（profile），用药提醒里也会一起消失 ——
      // 这正是「两处是同一份档案」的应有语义。
      await MD.data.profiles.remove(member.id);
      toast('已删除');
      return true;
    } catch (e) {
      S.err = '删除失败：' + (e && e.message ? e.message : '未知错误');
      return false;
    }
  }

  /* ==================== 5.7 药物清单：跨账号共同维护 ==================== */

  // 药物清单卡片。
  //
  // 【展示口径必须与用药提醒一致】
  // 用户明确要求"家庭健康的药物清单 = 用药提醒的药物清单，包括格式、属性、频次"。
  // 这里按 med.js 的同一套拼法：剂量 + 规格 · 频次，下面再补库存/关键标记。
  // 数据本身来自 buildSnapshot，两侧同源，所以不会分叉。
  //
  // 【canEdit】
  //   - 关联投影：写回对方账号（grant 有效即可）
  //   - 本账号档案：写本地表，走自己的同步链路
  function sharedMedsCard(member, data) {
    var isLinked = Boolean(member && member.source === 'linked' && member.grantId);
    var isLocal = Boolean(member && member.source === 'local');
    var canEdit = (isLinked && MD.familyLink && typeof MD.familyLink.pushRecords === 'function')
      || (isLocal && MD.data && MD.data.meds);
    var rows = [];

    (data.meds || []).forEach(function (m) {
      var line1 = [m.dosage, m.strength].filter(Boolean).join(' ');
      var sub = [line1, m.schedText].filter(Boolean).join(' · ');

      var nameRow = el('div', { class: 'row', style: { gap: '6px', alignItems: 'center' } }, [
        el('div', { text: m.name, style: { fontSize: '13px', fontWeight: '600' } })
      ]);
      if (m.critical) nameRow.appendChild(el('span', { class: 'badge badge--warn', text: '关键' }));

      var metaRows = [el('span', { class: 'muted', text: sub || DASH, style: { fontSize: '12px' } })];
      if (m.stock != null) {
        metaRows.push(el('span', {
          class: 'muted',
          text: '库存 ' + m.stock + ' ' + (m.dosageUnit || ''),
          style: { fontSize: '12px' }
        }));
      }

      rows.push(el('div', {
        class: 'row',
        style: { justifyContent: 'space-between', alignItems: 'center', gap: '10px' }
      }, [
        el('div', { style: { display: 'flex', flexDirection: 'column', gap: '3px', minWidth: '0' } },
          [nameRow].concat(metaRows)),
        canEdit ? el('div', { class: 'row', style: { gap: '6px', flex: '0 0 auto' } }, [
          el('button', {
            class: 'btn btn-ghost', type: 'button', text: '改',
            onClick: function () { openMedForm(member, data, m); }
          }),
          el('button', {
            class: 'btn btn-ghost', type: 'button', text: '删',
            style: { color: 'var(--danger)' },
            onClick: function () { removeSharedMed(member, data, m); }
          })
        ]) : null
      ]));
    });

    if (!rows.length) {
      rows.push(el('p', {
        class: 'muted',
        text: isLinked ? '对方还没有为这个成员录入药物' : '还没有药物，点「+ 添加药物」开始',
        style: { fontSize: '13px', margin: '0' }
      }));
    }

    if (canEdit) {
      rows.push(el('button', {
        class: 'btn btn-ghost', type: 'button', text: '+ 添加药物',
        style: { marginTop: '6px' },
        onClick: function () { openMedForm(member, data, null); }
      }));
    }

    return sectionCard('药物清单（' + (data.meds || []).length + '）', rows);
  }

  function openMedForm(member, data, med) {
    if (!MD.ui || typeof MD.ui.sheet !== 'function') {
      toast('当前环境不支持编辑', 'error');
      return;
    }

    var isLocal = Boolean(member && member.source === 'local');

    var nameIn = el('input', {
      class: 'input', type: 'text', value: med ? med.name : '',
      placeholder: '例如：二甲双胍'
    });
    var doseIn = el('input', {
      class: 'input', type: 'text', value: med ? (med.dosage || '') : '',
      placeholder: '例如：0.5 g / 1 片'
    });
    var strengthIn = el('input', {
      class: 'input', type: 'text', value: med ? (med.strength || '') : '',
      placeholder: '例如：500mg（选填）'
    });

    MD.ui.sheet({
      title: med ? '编辑药物' : '添加药物',
      body: function (host) {
        host.appendChild(el('div', { class: 'stack', style: { gap: '10px' } }, [
          el('div', { class: 'field' }, [el('span', { class: 'label', text: '药物名称' }), nameIn]),
          el('div', { class: 'field' }, [el('span', { class: 'label', text: '剂量' }), doseIn]),
          el('div', { class: 'field' }, [el('span', { class: 'label', text: '规格' }), strengthIn])
        ]));
        host.appendChild(el('p', {
          class: 'muted',
          text: isLocal
            ? '改动写入本账号，与「用药提醒」里看到的药物清单是同一份数据。'
            : '改动会写进对方的账号，对方下次同步即可看到；双方同时修改同一条时会提示以最新值为准。',
          style: { fontSize: '12px', margin: '10px 0 0' }
        }));
      },
      actions: [
        { label: '取消', kind: 'ghost', onClick: function () { MD.ui.closeSheet(); } },
        {
          label: '保存', kind: 'primary',
          onClick: function () {
            var name = String(nameIn.value || '').trim();
            if (!name) { toast('请填写药物名称', 'error'); return false; }
            submitSharedMed(member, data, med, {
              name: name,
              dosage: String(doseIn.value || '').trim(),
              strength: String(strengthIn.value || '').trim()
            });
            return true;
          }
        }
      ]
    });
  }

  // 归属成员（profileId）：
  //   - 关联投影：只能是被查看的那个 owner 档案（ownerProfileId），否则药会挂到
  //     对方账号里别的成员名下；
  //   - 本账号档案：就是当前档案的 id。
  function pickProfileId(member, med, data) {
    if (member && member.source === 'linked' && member.ownerProfileId) return member.ownerProfileId;
    if (member && member.source === 'local' && member.id) return member.id;
    if (med && med.profileId) return med.profileId;
    var list = (data && data.meds) || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i].profileId) return list[i].profileId;
    }
    return '';
  }

  async function submitSharedMed(member, data, med, values) {
    var isLocal = Boolean(member && member.source === 'local');
    try {
      if (isLocal) {
        await saveLocalMed(member, med, values);
        toast('已保存');
        S.err = '';
        paint();
        return;
      }

      var id = med ? med.id : uid();
      var res = await MD.familyLink.pushRecords(member.grantId, [{
        id: id,
        ver: med ? (Number(med.ver) || 0) + 1 : 1,
        deleted: false,
        payload: {
          id: id,
          profileId: pickProfileId(member, med, data),
          name: values.name,
          dosage: values.dosage,
          strength: values.strength || '',
          updatedAt: nowMs()
        }
      }]);

      var conflicts = (res && res.conflicts) || [];
      if (conflicts.length) {
        // 冲突时服务端保留的是更高版本，客户端重新拉取就能看到胜出值
        toast('对方刚改过同一条，已显示最新值', 'warn');
      } else {
        toast('已保存');
      }
      S.err = '';
      paint();
    } catch (e) {
      S.err = '保存失败：' + (e && e.message ? e.message : '未知错误');
      paint();
    }
  }

  // 本账号药物写入：必须基于原记录合并，否则只改名字会把规格/库存/照片等
  // 同一条记录上的其它字段抹掉（那些字段由用药提醒的表单维护）。
  async function saveLocalMed(member, med, values) {
    var base = null;
    if (med && med.id) {
      try { base = await MD.data.meds.get(med.id); } catch (e) { base = null; }
    }
    var rec = Object.assign({}, base || {}, {
      id: med ? med.id : uid(),
      profileId: (base && base.profileId) || pickProfileId(member, med, null),
      name: values.name,
      dosage: values.dosage,
      strength: values.strength || '',
      updatedAt: nowMs()
    });
    if (!base) rec.createdAt = nowMs();
    return MD.data.meds.save(rec);
  }

  async function removeSharedMed(member, data, med) {
    var isLocal = Boolean(member && member.source === 'local');
    var ok = await MD.ui.confirm({
      title: '删除药物',
      message: isLocal
        ? '确定删除「' + med.name + '」？会从本账号的药清单里移除，并同步到其它设备。'
        : '确定从「' + (member.name || '未命名') + '」的药物清单删除「' + med.name + '」？'
          + '这条药物会从对方账号里一并删除，双方都生效。',
      confirmText: '删除',
      danger: true
    });
    if (!ok) return;

    try {
      if (isLocal) {
        await MD.data.meds.remove(med.id);
        toast('已删除');
        S.err = '';
        paint();
        return;
      }
      await MD.familyLink.pushRecords(member.grantId, [{
        id: med.id,
        ver: (Number(med.ver) || 0) + 1,
        deleted: true,
        payload: { id: med.id, deleted: true, updatedAt: nowMs() }
      }]);
      toast('已删除');
      S.err = '';
      paint();
    } catch (e) {
      S.err = '删除失败：' + (e && e.message ? e.message : '未知错误');
      paint();
    }
  }

  /* ============================ 6. 视图：家庭档案列表 ============================ */

  // 编辑投影档案：只改本账号这一份副本。
  // 写 family 表而不是 profile 表 —— 投影不是"我家的成员"，它是"别人家的人在我这里的影子"，
  // 姓名/关系/备注属于本账号的私有注释，绝不同步回对方（对方也看不到）。
  async function saveLinkedProfileForm(rec) {
    var ownerProfileId = String(rec.id || '').replace(/^lp-/, '');
    if (!ownerProfileId) throw new Error('档案标识异常');
    await upsertLinked(ownerProfileId, {
      name: rec.name,
      relation: rec.relation,
      note: rec.note,
      updatedAt: nowMs()
    });
  }

  async function renderArchive(host) {
    clear(host);

    if (S.view === 'new' || S.view === 'edit') {
      var editing = S.view === 'edit' ? await getMember(S.memberId) : null;
      host.appendChild(el('div', { style: { fontWeight: '700', marginBottom: '12px' } }, [
        el('span', { text: editing ? '编辑档案' : '新增档案' })
      ]));
      if (editing && editing.source === 'linked') {
        host.appendChild(el('p', {
          class: 'muted',
          text: '这是关联档案：姓名/关系/备注只改本账号里的显示，不会影响对方的账号；'
            + '药物清单仍然与对方实时同步。',
          style: { fontSize: '12px', margin: '0 0 10px' }
        }));
      }
      memberForm(host, editing, async function (rec) {
        if (!rec) { S.view = 'list'; S.memberId = null; paint(); return; }
        try {
          if (editing && editing.source === 'linked') {
            await saveLinkedProfileForm({ id: editing.id, name: rec.name, relation: rec.relation, note: rec.note });
          } else {
            await saveProfileFromForm(rec);
          }
          S.view = 'list'; S.memberId = null;
          toast('已保存');
        } catch (e) {
          S.err = '保存失败：' + (e && e.message ? e.message : '未知错误');
        }
        paint();
      });
      return;
    }

    if (S.view === 'detail') {
      var member = await getMember(S.memberId);
      if (!member) { S.view = 'list'; paint(); return; }
      var data = await loadMemberData(member);
      renderDetail(host, member, data);
      return;
    }

    var members = await listMembers();

    host.appendChild(el('div', { class: 'row row--wrap', style: { gap: '10px', marginBottom: '14px' } }, [
      el('button', {
        class: 'btn btn-primary', type: 'button', text: '新增档案',
        onClick: function () { S.view = 'new'; S.memberId = null; paint(); }
      }),
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '家庭关联',
        onClick: function () { S.tab = 'link'; paint(); }
      }),
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '刷新档案',
        onClick: async function () {
          S.msg = ''; S.err = '';
          var n = await refreshAllLinked();
          S.msg = n > 0 ? ('已刷新，同步了 ' + n + ' 处档案变化') : '档案已是最新';
          paint();
        }
      })
    ]));

    if (!members.length) {
      host.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('home-heart'),
        el('strong', { text: '还没有家庭档案' }),
        el('p', { class: 'muted', text: '档案与「用药提醒」共用同一份名单，在这里或那里添加都可以。关联对方账号后，对方的全部家庭档案会同步到这里。' })
      ]));
      return;
    }

    members.forEach(function (m) {
      var badge;
      if (m.source === 'linked') {
        badge = '关联档案 · 药物实时同步';
      } else if (m.legacyGrantId) {
        badge = '已关联（旧方式）';
      } else {
        badge = '本账号';
      }

      host.appendChild(el('div', { class: 'card', style: { marginBottom: '10px' } }, [
        el('div', { class: 'row', style: { justifyContent: 'space-between', alignItems: 'center', gap: '12px' } }, [
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '3px', minWidth: '0' } }, [
            el('div', { text: m.name || '未命名', style: { fontWeight: '700' } }),
            el('span', {
              class: 'muted',
              text: (m.relation || '') + ' · ' + badge,
              style: { fontSize: '12px' }
            })
          ]),
          el('div', { class: 'row', style: { gap: '8px', flex: '0 0 auto' } }, [
            el('button', {
              class: 'btn btn-ghost', type: 'button', text: '查看',
              onClick: function () { S.view = 'detail'; S.memberId = m.id; paint(); }
            }),
            el('button', {
              class: 'btn btn-ghost', type: 'button', text: '编辑',
              onClick: function () { S.view = 'edit'; S.memberId = m.id; paint(); }
            }),
            el('button', {
              class: 'btn btn-ghost', type: 'button', text: '删除',
              style: { color: 'var(--danger)' },
              onClick: async function () {
                if (await deleteMember(m)) { S.view = 'list'; S.memberId = null; }
                paint();
              }
            })
          ])
        ])
      ]));
    });
  }

  // 逐个授权拉一次对方记录，刷新投影档案清单（对方新增/删除了家庭成员时用）。
  async function refreshAllLinked() {
    var idx = await loadIndex();
    var ids = Object.keys(idx.grants || {});
    var changed = 0;
    for (var i = 0; i < ids.length; i++) {
      var g = idx.grants[ids[i]];
      if (!g) continue;
      try {
        var dek = await openOwnerDekOf(g);
        if (!dek || !MD.familyLink) continue;
        MD.familyLink.adoptDek(ids[i], dek);
        var recs = await pullAll(ids[i]);
        var ents = recordsToEntities(recs);
        var list = (ents.profiles || []).filter(function (p) {
          return p && p.deleted !== true && !p.archived;
        });
        changed += await syncLinkedProfiles(ids[i], g.ownerId || '', list);
      } catch (e) {
        // 单个授权失败不影响其它授权
      }
    }
    return changed;
  }

  /* ==================== 5.5 短码关联：实时数据通路 ==================== */

  // 用「我自己账号的 DEK」把对方的 DEK 再加密一份，存到 member 记录上。
  //
  // 【为什么不继续用短码派生密钥】
  // 短码是 8 位数字，职责只有「一次性把密钥交出去」。它兑换即作废，
  // 页面一刷新就没法再用它解包 —— 若没有替代的解锁材料，查看方刷新一次
  // 就永久失去访问权。所以换一把长期可用的锁：我自己账号的 DEK
  // （服务端拿不到，且随账号加密同步）。对方的 DEK 仍然只有密文过服务端。
  // 返回加密后的密钥材料，由调用方交给 saveLink 落库。
  // 不在这里写库：档案与关联信息现在是两张表，混在一起写会把
  // 姓名/关系这些档案字段又抄一份进关联表。
  // 一把 grant 对应对方**整个账号**的查阅权，所以密钥挂在 grant 上，
  // 不再挂在某个档案上。早先是"一个授权只能看一个档案"，密钥自然跟着档案走；
  // 现在一个授权要覆盖对方全部档案，密钥跟着档案就会重复存 N 份且容易不一致。
  async function sealOwnerDek(ownerDek) {
    var myDek = MD.api && MD.api.getDEK ? MD.api.getDEK() : null;
    if (!myDek) throw new Error('账号未解锁，请重新登录');
    var body = await MD.crypto.encrypt(myDek, JSON.stringify(Array.prototype.slice.call(ownerDek)));
    return { dekIv: body.ivB64, dekCt: body.ctB64 };
  }

  // 接收任意含 dekIv/dekCt 的对象（grant 行，或迁移前挂在档案上的旧式关联）
  async function openOwnerDekOf(holder) {
    if (!holder || !holder.dekIv || !holder.dekCt) return null;
    var myDek = MD.api && MD.api.getDEK ? MD.api.getDEK() : null;
    if (!myDek) return null;
    try {
      var plain = await MD.crypto.decrypt(myDek, holder.dekIv, holder.dekCt);
      return new Uint8Array(JSON.parse(plain));
    } catch (e) {
      // 换了账号或本地材料损坏时，降级为「需要重新关联」，不要把页面卡死
      return null;
    }
  }

  /* ---------- 投影档案同步：一个授权 → 对方全部档案 ---------- */

  // 把对方的 profile 列表同步成本地投影档案。
  //
  // 【关键取舍：姓名/关系/备注只在首次创建时复制】
  // 用户明确要求"关联人支持修改姓名、关系、备注，这个是独立本账号的不会同步到别的账号"。
  // 所以已存在的投影绝不用对方的新值覆盖——否则查看方改完名字，对方一改档案就被冲掉。
  //
  // 【对方删了档案怎么办】
  // 标记 ownerDeleted 并从列表隐藏，不物理删除：查看方可能在里面写过备注，
  // 误删不可恢复；隐藏已经能满足"同步一致"的观感。
  async function syncLinkedProfiles(grantId, ownerId, ownerProfiles) {
    if (!grantId) return 0;
    var rows = await listFamilyRows();
    var byOwnerPid = {};
    rows.forEach(function (r) {
      if (r && r.ownerProfileId && String(r.grantId || '') === String(grantId)) {
        byOwnerPid[String(r.ownerProfileId)] = r;
      }
    });

    var seen = {}, changed = 0;
    var list = ownerProfiles || [];
    for (var i = 0; i < list.length; i++) {
      var p = list[i];
      if (!p || p.deleted === true || !p.id) continue;
      var opid = String(p.id);
      seen[opid] = true;
      var existing = byOwnerPid[opid];

      if (!existing) {
        await upsertLinked(opid, {
          grantId: String(grantId),
          ownerId: ownerId || '',
          name: p.name || '未命名',
          relation: p.relation || '',
          note: p.note || '',
          color: p.color || '#4f8cff',
          birthDate: p.birthDate || '',
          createdAt: Number(p.createdAt) || nowMs(),
          linkedAt: nowMs(),
          ownerDeleted: false
        });
        changed++;
      } else {
        // 已存在：只维护关联元数据，姓名/关系/备注保持查看方本地的值
        var patch = { grantId: String(grantId), ownerId: ownerId || '' };
        if (existing.ownerDeleted) patch.ownerDeleted = false;
        if (existing.grantId !== String(grantId) || existing.ownerDeleted) {
          await upsertLinked(opid, patch);
          changed++;
        }
      }
    }

    // 对方已不在的档案：隐藏而非删除
    for (var opid2 in byOwnerPid) {
      if (!Object.prototype.hasOwnProperty.call(byOwnerPid, opid2)) continue;
      if (seen[opid2]) continue;
      var row = byOwnerPid[opid2];
      if (!row || row.ownerDeleted) continue;
      await upsertLinked(opid2, { ownerDeleted: true });
      changed++;
    }
    return changed;
  }

  // 从已拉到的记录里同步（详情页顺带刷新，省一次请求）
  async function cacheLinkedProfiles(grantId, ownerId, ownerProfiles) {
    try { return await syncLinkedProfiles(grantId, ownerId, ownerProfiles); }
    catch (e) { return 0; }
  }

  // 排程描述（与用药提醒完全一致的展示口径）。
  // med.js 里叫 schedTextShort，这里复刻同一套规则：去掉起始日期、去重周期说明。
  function schedTextOf(sc) {
    if (!sc) return '未设置排程';
    if (MD.sched && typeof MD.sched.describe === 'function') {
      var t = MD.sched.describe(sc) || '';
      t = t.replace(/（([^）]*)）/g, function (whole, inner) {
        var parts = inner.split('，').filter(function (p) { return !/\d{4}-\d{2}-\d{2}\s*起$/.test(p.trim()); });
        return parts.length ? '（' + parts.join('，') + '）' : '';
      });
      t = t.replace('周期性用药（21天服/7天停） 服', '周期性用药 服');
      return t.replace(/^自定义间隔\s*/, '').replace(/\s+/g, ' ').trim() || '未设置排程';
    }
    return '未设置排程';
  }

  function doseTextOf(m) {
    if (!m) return '';
    if (m.dosageAmount == null || m.dosageAmount === '') {
      return m.dosage || m.doseText || m.dose || '';
    }
    return m.dosageAmount + ' ' + (m.dosageUnit || '');
  }

  // 统一快照构建：本账号档案与关联档案走同一个函数。
  //
  // 【为什么必须统一】
  // 用户要求"用药提醒里的药物清单 = 家庭健康本人的药物清单 = 关联人看到的药物清单，
  // 包括格式、属性、频次"。只要两侧各自写一套映射，字段迟早分叉（上一版就丢了频次）。
  // 现在唯一入口是这里，输入都是明文实体（本地的或解密后的），输出必然同形。
  //
  // entities: { meds:[], schedules:[], intakes:[] }
  // profileId: 只取归属该人的数据；传空表示不过滤
  function buildSnapshot(entities, days, profileId) {
    var now = nowMs();
    var span = Math.max(1, Math.min(365, Number(days) || 90));
    var fromMs = now - span * DAY_MS;
    entities = entities || {};

    var allMeds = (entities.meds || []).filter(function (m) {
      return m && m.deleted !== true;
    });
    var scoped = profileId
      ? allMeds.filter(function (m) { return String(m.profileId || '') === String(profileId); })
      : allMeds;

    // 排程按药物归属反查：schedule.medicationId → medication.id
    var medIds = {};
    scoped.forEach(function (m) { medIds[String(m.id)] = true; });
    var schedByMed = {};
    (entities.schedules || []).forEach(function (sc) {
      if (!sc || sc.deleted === true) return;
      var mid = String(sc.medicationId || '');
      if (!mid || !medIds[mid]) return;
      // 与 med.js 一致：取第一条排程作为展示口径
      if (!schedByMed[mid]) schedByMed[mid] = sc;
    });

    var medList = scoped.map(function (m) {
      var sc = schedByMed[String(m.id)];
      return {
        id: m.id,
        ver: Number(m.__ver) || 0,
        profileId: m.profileId || '',
        name: m.name || '未命名药物',
        dosage: doseTextOf(m),
        // 属性、频次、库存等一并带出，保证与用药提醒列表同信息量
        strength: m.strength || '',
        form: m.form || '',
        dosageUnit: m.dosageUnit || '',
        dosageAmount: (m.dosageAmount == null ? null : m.dosageAmount),
        stock: (m.stock == null ? null : m.stock),
        critical: Boolean(m.critical),
        refillThreshold: (m.refillThreshold == null ? null : m.refillThreshold),
        schedText: schedTextOf(sc),
        schedId: sc ? sc.id : '',
        note: m.note || ''
      };
    }).sort(function (a, b) {
      return String(a.name).localeCompare(String(b.name), 'zh-Hans-CN');
    });

    // 服药记录只取该人的；intake.profileId 缺失时用药物归属兜底（旧数据可能没写）
    var medProfile = {};
    allMeds.forEach(function (m) { medProfile[String(m.id)] = String(m.profileId || ''); });
    var scopedIntakes = (entities.intakes || []).filter(function (it) {
      if (!it || it.deleted === true) return false;
      if (!profileId) return true;
      var pid = String(it.profileId || '');
      if (!pid) pid = medProfile[String(it.medicationId || '')] || '';
      return pid === String(profileId);
    });

    var windowIntakes = scopedIntakes.filter(function (it) {
      var t = Number(it.scheduledFor || 0);
      return t >= fromMs && t <= now;
    });

    var nameById = {};
    allMeds.forEach(function (m) { nameById[String(m.id)] = m.name || '未命名药物'; });

    var history = windowIntakes.map(function (it) {
      return {
        t: Number(it.scheduledFor || 0),
        s: String(it.status || 'pending'),
        n: nameById[String(it.medicationId || '')] || '未知药物'
      };
    }).sort(function (a, b) { return b.t - a.t; }).slice(0, 400);

    var adherence = { total: 0, taken: 0, skipped: 0, missed: 0, rate: null, streakDays: 0 };
    if (MD.sched && typeof MD.sched.adherence === 'function') {
      try {
        adherence = MD.sched.adherence({ intakes: windowIntakes, fromMs: fromMs, toMs: now }) || adherence;
      } catch (e) { /* 依从性算不出来不影响其余展示 */ }
    }

    return {
      v: 3, realtime: true, exportedAt: now, days: span,
      meds: medList, history: history, adherence: adherence
    };
  }

  // 把对方的 sync 记录拆成 buildSnapshot 认识的明文实体。
  // 同时保留 ver：写回时要用它做乐观并发（服务端按 ver 裁决冲突）。
  function recordsToEntities(records) {
    var meds = [], schedules = [], intakes = [], profiles = [];
    (records || []).forEach(function (r) {
      if (!r || r.deleted || !r.data) return;
      var d = r.data;
      if (r.table === 'medication') {
        var m = Object.assign({}, d);
        m.__ver = Number(r.ver) || 0;
        meds.push(m);
      } else if (r.table === 'schedule') {
        schedules.push(d);
      } else if (r.table === 'intake') {
        intakes.push(d);
      } else if (r.table === 'profile') {
        var p = Object.assign({}, d);
        p.__ver = Number(r.ver) || 0;
        profiles.push(p);
      }
    });
    return { meds: meds, schedules: schedules, intakes: intakes, profiles: profiles };
  }

  // 本账号数据（本地表）→ 与关联侧同形的实体集合
  async function collectLocalEntities() {
    var meds = [], schedules = [], intakes = [];
    try { meds = (await MD.data.meds.list()) || []; } catch (e) { meds = []; }
    try { schedules = (await MD.data.schedules.list()) || []; } catch (e) { schedules = []; }
    try { intakes = (await MD.data.intakes.list()) || []; } catch (e) { intakes = []; }
    return { meds: meds, schedules: schedules, intakes: intakes };
  }

  // 分页拉全量。20 页上限是硬闸：游标万一不推进也不会无限翻页，
  // 而个人用药数据远超 200 条的情况本身就不正常。
  async function pullAll(grantId) {
    var acc = [], sinceSeq = 0, pages = 0, hasMore = true;
    while (hasMore && pages < 20) {
      var page = await MD.familyLink.fetchRecords(grantId, { sinceSeq: sinceSeq, limit: 200 });
      var items = (page && page.records) || [];
      acc = acc.concat(items);
      if (!items.length) break;
      if (page.nextSeq) sinceSeq = page.nextSeq;
      hasMore = Boolean(page && page.hasMore);
      pages++;
    }
    return acc;
  }

  // 详情页取数。
  //
  // 两条路，输出同形（都经 buildSnapshot），所以详情页不需要知道数据来自哪里：
  //   - 本账号档案：读本地表，按 profileId 过滤 —— 这就是"家庭健康的本人 =
  //     用药提醒的本人"。此前这里直接返回 null，页面显示"请去关联"的空态，
  //     等于本账号自己的档案反而看不到自己的药。
  //   - 关联投影：拉 owner 的密文记录，按 ownerProfileId 过滤后解密组装。
  async function loadMemberData(member) {
    if (!member) return null;

    if (member.source === 'linked' && member.grantId && MD.familyLink) {
      var grant = await getGrant(member.grantId);
      var ownerDek = await openOwnerDekOf(grant);
      if (!ownerDek) {
        S.err = '本机密钥材料缺失（可能换过设备或账号），请让对方重新生成关联码。';
        return null;
      }
      MD.familyLink.adoptDek(member.grantId, ownerDek);
      try {
        var recs = await pullAll(member.grantId);
        var ents = recordsToEntities(recs);
        await cacheLinkedProfiles(member.grantId, grant ? grant.ownerId : '', ents.profiles);
        return buildSnapshot(ents, 90, member.ownerProfileId);
      } catch (e) {
        S.err = '实时数据拉取失败：' + (e && e.message ? e.message : '未知错误');
        return await getSnapshot(member.id);
      }
    }

    // 旧式单档案关联（迁移前）：仍然按整个 owner 的数据展示，保持可用
    if (member.legacyGrantId && MD.familyLink) {
      var legacy = await getGrant(member.legacyGrantId);
      if (legacy) {
        // 迁移把密钥提到了 grant 行；万一还没迁到，退回读档案自己身上的那份
        var ldek = await openOwnerDekOf(legacy);
        if (!ldek && member.dekIv && member.dekCt) {
          ldek = await openOwnerDekOf({ dekIv: member.dekIv, dekCt: member.dekCt });
        }
        if (ldek) {
          MD.familyLink.adoptDek(member.legacyGrantId, ldek);
          try {
            var lrecs = await pullAll(member.legacyGrantId);
            return buildSnapshot(recordsToEntities(lrecs), 90, '');
          } catch (e2) {
            S.err = '实时数据拉取失败：' + (e2 && e2.message ? e2.message : '未知错误');
          }
        }
      }
      var snapOld = await getSnapshot(member.id);
      if (snapOld) return snapOld;
    }

    // 本账号档案
    var entities = await collectLocalEntities();
    return buildSnapshot(entities, 90, member.id);
  }

  function renderLink(host) {
    clear(host);

    // 提示区统一放顶部：生成、兑换、复制、授权列表的结果都写这里。
    // whiteSpace: pre-line 是为了让「授权列表」这种多行文本能正常换行。
    var msg = el('p', {
      class: 'muted',
      style: { minHeight: '18px', margin: '0 0 12px', fontSize: '13px', whiteSpace: 'pre-line' }
    });
    if (S.err) { msg.textContent = S.err; msg.style.color = 'var(--danger)'; }
    else if (S.msg) { msg.textContent = S.msg; }

    /* ---------- ① 数据所有方：生成 8 位短码 ---------- */
    var codeView = el('div', {
      style: {
        fontFamily: 'monospace', fontSize: '26px', fontWeight: '700',
        letterSpacing: '5px', textAlign: 'center', padding: '14px 6px',
        borderRadius: '10px', background: 'rgba(127,127,127,0.10)',
        display: S.lastCode ? 'block' : 'none'
      },
      text: (S.lastCode && MD.familyLink) ? MD.familyLink.formatCode(S.lastCode) : ''
    });

    host.appendChild(el('div', { class: 'card', style: { marginBottom: '14px' } }, [
      el('div', { style: { fontWeight: '700', marginBottom: '6px' } }, [el('span', { text: '① 我是数据所有方：生成 8 位关联码' })]),
      el('p', {
        class: 'muted',
        text: '生成一串 8 位数字，家人输入后即可查看你的药物清单、用药历史与依从性。数字一次性使用，兑换后即绑定对方账号。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        codeView,
        el('div', { class: 'row row--wrap', style: { gap: '10px' } }, [
          el('button', {
            class: 'btn btn-primary', type: 'button', text: '生成关联码',
            onClick: async function () {
              S.err = ''; S.msg = '';
              try {
                if (!MD.familyLink) throw new Error('短码模块未加载，请刷新页面');
                var res = await MD.familyLink.createGrant();
                S.lastCode = res.code;   // 先落状态，再整页重绘，才能回显
                // 服务端只存 HMAC，不回显明文，所以本机留一份才能事后查看
                if (MD.familyLink.rememberGrant) {
                  MD.familyLink.rememberGrant({
                    grantId: res.grantId, code: res.code, createdAt: nowMs()
                  });
                }
                S.msg = '已生成。请在 24 小时内让对方输入，逾期自动失效。';
                toast('关联码已生成');
              } catch (e) {
                S.err = '生成失败：' + (e && e.message ? e.message : '未知错误');
              }
              paint();
            }
          }),
          el('button', {
            class: 'btn btn-ghost', type: 'button', text: '复制数字',
            onClick: function () {
              if (!S.lastCode) { toast('还没有生成关联码', 'error'); return; }
              try {
                if (navigator.clipboard && navigator.clipboard.writeText) {
                  navigator.clipboard.writeText(S.lastCode);
                  toast('已复制');
                } else { toast('请长按上方数字手动复制'); }
              } catch (e) { toast('请长按上方数字手动复制'); }
            }
          })
        ]),
        el('button', {
          class: 'btn btn-ghost', type: 'button', text: '查看我发出的授权',
          onClick: async function () {
            S.err = ''; S.msg = '';
            try {
              if (!MD.familyLink) throw new Error('短码模块未加载，请刷新页面');
              var list = await MD.familyLink.listGrants();
              var locals = MD.familyLink.localGrants ? MD.familyLink.localGrants() : [];
              var codeMap = {};
              (locals || []).forEach(function (x) {
                if (x && x.grantId) codeMap[x.grantId] = x.code;
              });

              if (!list || !list.length) {
                S.msg = '本账号还没有发出过授权';
              } else {
                S.msg = list.map(function (g, i) {
                  var gid = g.grantId || g.grantID || g.id || '';
                  var state = g.status === 'active' ? '已激活（对方已兑换）'
                    : (g.status === 'pending' ? '待兑换（24 小时后失效）' : '已失效');
                  // 服务端不保存明文码，只有生成方本机留了档才显示得出来
                  var codeText = codeMap[gid] ? ('  ' + codeMap[gid])
                    : '  （本机未记录，换设备后不可见）';
                  return (i + 1) + '.' + codeText + '\n     ' + state + ' · ' + fmtDateTime(g.createdAt || 0);
                }).join('\n');
              }
            } catch (e) {
              S.err = '读取授权失败：' + (e && e.message ? e.message : '未知错误');
            }
            paint();
          }
        })
      ])
    ]));

    /* ---------- ② 查看方：输入短码 ---------- */

    // 不再让用户挑「保存到哪个档案」。
    // 一个 8 位码换的是对方**整个账号**的查阅权，对方的每个家庭成员都会
    // 在本账号里生成一条投影档案，所以挑一个档案挂上去在语义上就是错的：
    // 挑了"本人"就看不到对方的"儿子"，还得再换一次码（而码是一次性的）。
    var codeIn = el('input', {
      class: 'input', type: 'text', inputmode: 'numeric', maxlength: '9',
      placeholder: '输入 8 位数字', autocomplete: 'off'
    });

    host.appendChild(el('div', { class: 'card', style: { marginBottom: '14px' } }, [
      el('div', { style: { fontWeight: '700', marginBottom: '6px' } }, [el('span', { text: '② 我是查看方：输入关联码' })]),
      el('p', {
        class: 'muted',
        text: '数字只能使用一次。关联成功后，对方的全部家庭档案会同步到你的「家庭档案」，'
          + '药物清单与对方实时保持一致；档案的姓名/关系/备注由你自己维护，不会影响对方。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '8 位关联码' }), codeIn]),
        el('button', {
          class: 'btn btn-primary', type: 'button', text: '关联并同步',
          onClick: async function () {
            S.err = ''; S.msg = '';
            try {
              if (!MD.familyLink) throw new Error('短码模块未加载，请刷新页面');
              var code = MD.familyLink.normalizeCode(codeIn.value);
              if (!code) throw new Error('请输入 8 位数字关联码');

              var res = await MD.familyLink.redeem(code);
              var grantId = res.grantId;
              var ownerId = res.ownerId || '';
              var ownerDek = MD.familyLink.getDek(grantId);
              if (!ownerDek) throw new Error('密钥恢复失败，请让对方重新生成关联码');

              // 短码一次性作废，必须立刻用本账号 DEK 把对方 DEK 落盘 —— 这是之后唯一的解锁途径。
              var sealed = await sealOwnerDek(ownerDek);
              await saveGrant(grantId, {
                ownerId: ownerId,
                dekIv: sealed.dekIv,
                dekCt: sealed.dekCt,
                linkedAt: nowMs()
              });

              // 一次性把对方全部记录拉下来：profile 建投影，medication/schedule/intake 留在
              // 各自投影的详情页里按 profileId 现场过滤（不做整体快照，避免一份数据看所有人）。
              var recs = await pullAll(grantId);
              var ents = recordsToEntities(recs);

              var ownerProfiles = (ents.profiles || []).filter(function (p) {
                return p && p.deleted !== true && !p.archived;
              });

              if (!ownerProfiles.length) {
                S.msg = '关联成功，但对方还没有创建家庭档案（在「用药提醒 → 家庭档案」里建一个后，'
                  + '你这边再点一次「刷新档案」即可看到）。';
                paint();
                return;
              }

              var n = await syncLinkedProfiles(grantId, ownerId, ownerProfiles);
              toast('关联成功，已同步 ' + ownerProfiles.length + ' 个档案');
              S.msg = '已同步对方的 ' + ownerProfiles.length + ' 个家庭档案：'
                + ownerProfiles.map(function (p) { return p.name || '未命名'; }).join('、')
                + '。每个档案的药物清单与对方实时一致。';
              S.tab = 'archive'; S.view = 'list'; S.memberId = null;
              paint();
              void n;
            } catch (e) {
              var reason = (e && e.message) ? e.message : '未知错误';
              if (reason.indexOf('短码无效') >= 0) {
                // 统一文案是为了不泄露「这个码是否存在」。但只给一句「无效」会让
                // 用户不知道下一步该做什么，这里补充可能原因与可执行动作。
                reason += '\n可能原因：① 已超过 24 小时未兑换；② 已被兑换过。'
                  + '\n下一步：让对方重新生成一张。';
              }
              S.err = '关联失败：' + reason;
              paint();
            }
          }
        })
      ])
    ]));

    /* ---------- ③ 离线快照（旧方式，折叠保留） ---------- */
    host.appendChild(el('div', { class: 'card' }, [
      el('div', {
        class: 'row',
        style: { justifyContent: 'space-between', alignItems: 'center', cursor: 'pointer', gap: '10px' },
        onClick: function () { S.showLegacy = !S.showLegacy; paint(); }
      }, [
        el('div', { style: { fontWeight: '700' } }, [el('span', { text: '离线快照（旧方式，需口令）' })]),
        el('span', { class: 'muted', text: S.showLegacy ? '收起' : '展开', style: { fontSize: '13px', flex: '0 0 auto' } })
      ]),
      S.showLegacy
        ? (function () {
            var box = el('div', { style: { marginTop: '12px' } });
            renderLinkLegacy(box);
            return box;
          })()
        : el('p', {
            class: 'muted',
            text: '双方都能联网时无需此方式。它把数据打包成一段文本带走，适合无网络场景，但不会自动更新。',
            style: { fontSize: '12px', margin: '8px 0 0' }
          })
    ]));

    host.appendChild(msg);
  }

  function renderLinkLegacy(host) {
    clear(host);

    var codeBox = el('textarea', {
      class: 'input',
      placeholder: '生成后此处显示关联码，可复制发给家人',
      rows: '4',
      style: { width: '100%', fontSize: '12px', fontFamily: 'monospace', resize: 'vertical' }
    });
    // 关键：生成后页面会整页重绘，textarea 是新建节点，
    // 必须从状态回填，否则「提示已生成但框里是空的」。
    codeBox.value = S.legacyCode || '';

    var passOut = el('input', {
      class: 'input', type: 'text', placeholder: '设置一个至少 6 位的口令',
      autocomplete: 'off'
    });

    var daysIn = el('input', {
      class: 'input', type: 'number', value: '90', min: '7', max: '365', step: '1'
    });

    host.appendChild(el('div', { class: 'card', style: { marginBottom: '14px' } }, [
      el('div', { style: { fontWeight: '700', marginBottom: '6px' } }, [el('span', { text: '① 我是数据所有方：生成关联码' })]),
      el('p', {
        class: 'muted',
        text: '把你的药物清单、用药历史与依从性统计打包加密，得到一段关联码。对方需输入同一口令才能解开。口令不会被保存，丢失只能重新生成。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '口令' }), passOut]),
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '历史范围（天）' }), daysIn]),
        el('div', { class: 'row row--wrap', style: { gap: '10px' } }, [
          el('button', {
            class: 'btn btn-primary', type: 'button', text: '生成关联码',
            onClick: async function () {
              S.err = ''; S.msg = ''; paintBusy(host, true);
              try {
                var code = await makeCode(passOut.value, daysIn.value);
                S.legacyCode = code;   // 先落状态，再重绘，才能回显
                S.msg = '已生成，请把关联码和口令分别发给家人（建议不要在同一聊天里一起发）';
                toast('关联码已生成');
              } catch (e) {
                S.err = '生成失败：' + (e && e.message ? e.message : '未知错误');
              }
              S.view = 'link'; paint();
            }
          }),
          el('button', {
            class: 'btn btn-ghost', type: 'button', text: '复制关联码',
            onClick: function () {
              var payload = codeBox.value || S.legacyCode;
              if (!payload) { toast('还没有生成关联码', 'error'); return; }
              try {
                if (navigator.clipboard && navigator.clipboard.writeText) {
                  navigator.clipboard.writeText(payload);
                  toast('已复制');
                } else {
                  codeBox.value = payload; codeBox.select();
                  toast('请长按手动复制');
                }
              } catch (e) { toast('请长按手动复制'); }
            }
          })
        ]),
        codeBox
      ])
    ]));

    // 第二个区块：作为接收方导入
    var importCode = el('textarea', {
      class: 'input',
      placeholder: '粘贴家人给你的关联码',
      rows: '4',
      style: { width: '100%', fontSize: '12px', fontFamily: 'monospace', resize: 'vertical' }
    });
    var importPass = el('input', {
      class: 'input', type: 'text', placeholder: '输入对方设置的口令', autocomplete: 'off'
    });
    var targetSelect = el('select', { class: 'select' });

    listMembers().then(function (members) {
      clear(targetSelect);
      if (!members.length) {
        targetSelect.appendChild(el('option', { value: '', text: '（先新增一个家庭档案）' }));
        return;
      }
      members.forEach(function (m) {
        targetSelect.appendChild(el('option', { value: m.id, text: m.name + '（' + (m.relation || '') + '）' }));
      });
    });

    host.appendChild(el('div', { class: 'card' }, [
      el('div', { style: { fontWeight: '700', marginBottom: '6px' } }, [el('span', { text: '② 我是查看方：导入关联码' })]),
      el('p', {
        class: 'muted',
        text: '解出的数据会保存到所选的家庭档案下，可在「家庭档案」中查看药物清单、用药历史与依从性。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '关联码' }), importCode]),
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '口令' }), importPass]),
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '保存到' }), targetSelect]),
        el('button', {
          class: 'btn btn-primary', type: 'button', text: '导入并查看',
          onClick: async function () {
            S.err = ''; S.msg = '';
            try {
              var data = await readCode(importCode.value, importPass.value);
              var memberId = targetSelect.value;
              if (!memberId) throw new Error('请先新增一个家庭档案，再选择保存位置');
              var member = await getMember(memberId);
              if (!member) throw new Error('目标档案不存在');
              await putSnapshot(memberId, data);
              member.snapshotAt = nowMs();
              member.updatedAt = nowMs();
              await MD.data.family.save(member);
              toast('导入成功');
              S.tab = 'archive'; S.view = 'detail'; S.memberId = memberId; S.msg = ''; S.err = '';
              paint();
            } catch (e) {
              S.err = '导入失败：' + (e && e.message ? e.message : '未知错误');
              paint();
            }
          }
        })
      ])
    ]));

  }

  function paintBusy(host, busy) {
    S.busy = busy;
  }

  /* ============================ 8. 页面外壳 ============================ */

  var pageRoot = null;

  async function paint() {
    if (!pageRoot) return;
    var host = pageRoot;
    clear(host);

    // 页头
    host.appendChild(el('div', { style: { marginBottom: '16px' } }, [
      el('div', { style: { fontSize: '19px', fontWeight: '800' } }, [el('span', { text: '家庭健康' })]),
      el('p', {
        class: 'muted',
        text: '档案与「用药提醒」共用同一份成员名单，两处增删改都会同步；关联对方账号后，双方都能维护同一份药物清单。',
        style: { fontSize: '13px', margin: '5px 0 0' }
      })
    ]));

    // 分段切换
    host.appendChild(el('div', {
      class: 'row row--wrap',
      style: { gap: '10px', marginBottom: '16px' }
    }, [
      el('button', {
        class: 'chip' + (S.tab === 'archive' ? ' chip--active' : ''),
        type: 'button',
        onClick: function () { S.tab = 'archive'; S.view = 'list'; S.msg = ''; S.err = ''; paint(); }
      }, [MD.icon('home-heart'), el('span', { text: '家庭档案' })]),
      el('button', {
        class: 'chip' + (S.tab === 'link' ? ' chip--active' : ''),
        type: 'button',
        onClick: function () { S.tab = 'link'; S.msg = ''; S.err = ''; paint(); }
      }, [MD.icon('link'), el('span', { text: '家庭关联' })])
    ]));

    var body = el('div');
    host.appendChild(body);

    if (S.tab === 'link') renderLink(body);
    else await renderArchive(body);
  }

  // 打开页面时的预处理：两步幂等迁移 + 必要时自动补齐投影档案。
  //
  // 第三步的条件很窄（有授权但一条投影都没有），只在"刚升级上来"时触发一次；
  // 平时进入页面不发任何额外请求，不会拖慢首屏。
  async function boot() {
    try { await migrateLegacyMembers(); } catch (e) { /* 单条失败下次会重试 */ }
    try { await migrateLegacyGrants(); } catch (e) { /* 同上 */ }
    try {
      var idx = await loadIndex();
      var hasGrant = Object.keys(idx.grants || {}).length > 0;
      var hasLinked = (idx.linked || []).length > 0;
      if (hasGrant && !hasLinked) await refreshAllLinked();
    } catch (e) { /* 网络不可达时保持本地视图 */ }
  }

  function render(view) {
    pageRoot = view;
    boot().then(function () { paint(); }).catch(function () { paint(); });
  }

  MD.family = {
    // 供测试与「刷新档案」按钮使用
    buildSnapshot: buildSnapshot,
    syncLinkedProfiles: syncLinkedProfiles,
    refreshAllLinked: refreshAllLinked,
    migrateLegacyGrants: migrateLegacyGrants,
    migrateLegacyMembers: migrateLegacyMembers,
    listMembers: listMembers,
    register: function () {
      if (!MD.router || typeof MD.router.register !== 'function') return;
      MD.router.register({
        id: 'family',
        title: '家庭健康',
        subtitle: '家庭档案与关联',
        icon: 'home-heart',
        group: '健康',
        order: 2,          // 紧随「用药提醒」（order 1），保证底部标签栏前 4 位有它
        comingSoon: false,
        render: render
      });
    },
    render: render,
    // 供测试与外部调用
    collect: collect,
    makeCode: makeCode,
    readCode: readCode
  };
})(typeof window !== 'undefined' ? window : this);

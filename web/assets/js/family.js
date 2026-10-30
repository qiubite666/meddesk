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

  // 别名行：查看方对本账号可见的「姓名/关系/备注」只存这里。
  //
  // 【为什么要单独一行】
  // 这些字段此前写在投影行（lp-xxx）上。投影行同时被两方写：本账号（改名）与
  // 同步流程（对方档案变化、其他设备回放）。一旦有设备仍在跑旧代码反复重写该行，
  // 它每次都带新的 updatedAt，改名就会被当成"旧值"覆盖回去（实测：6 秒 11 次推送）。
  // 把显示字段搬到独立 id（alias:xxx）后，旧代码不认识这条记录，也就写不到它，
  // 改名在结构上不再可能被覆盖。
  function aliasRowId(ownerProfileId) { return 'alias:' + String(ownerProfileId || ''); }
  function isAliasRow(r) { return Boolean(r && /^alias:/.test(String(r.id || ''))); }

  // 姓名/关系归一化：忽略空白与大小写。
  // 迁移创建的空壳常缺关系字段（'' ），而投影有（'本人'），严格相等会漏判 ——
  // 漏判的直接后果就是列表里同时出现「本人·本账号」和「本人·关联档案」两条，
  // 用户很容易改错那条（实测发生：改了本账号那条，投影纹丝不动，随后又被同步回旧值）。
  function normText(v) { return String(v == null ? '' : v).replace(/\s+/g, '').toLowerCase(); }

  // 是否同一个人：姓名必须相同；关系相同、或任一方为空，都视为同一人。
  function samePerson(a, b) {
    var na = normText(a && a.name), nb = normText(b && b.name);
    if (!na || na !== nb) return false;
    var ra = normText(a && a.relation), rb = normText(b && b.relation);
    return ra === rb || !ra || !rb;
  }

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
      if (isAliasRow(r)) return;                       // 别名行不参与身份分类
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

  // 删除投影时给足版本余量。
  // 【为什么不能只 remove】还有设备在跑旧代码时，它会把自己本地的投影行继续推上云；
  // 若墓碑的 ver 低于那份旧副本，墓碑会被"复活"，已被导入的档案又冒出一条关联档案。
  async function tombstoneRow(id, extraVer) {
    if (!id) return false;
    try {
      var row = await MD.db.get('family', id);
      if (row) {
        await MD.data.family.save(Object.assign({}, row, {
          deleted: true,
          ver: (Number(row.ver) || 0) + (extraVer || 5000),
          updatedAt: nowMs()
        }));
        return true;
      }
    } catch (e) { /* 没有 MD.db 的环境（测试替身）走下面的 remove */ }
    try { await MD.data.family.remove(id); return true; } catch (e) { return false; }
  }

  async function dropLinked(ownerProfileId) {
    var id = linkedRowId(ownerProfileId);
    await tombstoneRow(id);
    await tombstoneRow(aliasRowId(ownerProfileId));
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
    var aliasMap = {};
    try { profiles = await MD.data.profiles.list(); } catch (e) { profiles = []; }
    try {
      var rows = await listFamilyRows();
      idx = classifyRows(rows);
      // 显示字段以别名行为准：投影行可能被同步/旧设备重写，别名行只由本账号改名时写。
      rows.forEach(function (r) {
        if (isAliasRow(r) && r.aliasFor) aliasMap[String(r.aliasFor)] = r;
      });
    } catch (e) { /* 保留空索引 */ }

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

    // 已经导入成本账号档案的人，投影一律不再显示。
    // 这是"导入即独立"之后的兜底：即使旧设备把投影行重新推上云，
    // 也不会在列表里多出一条影子档案。
    var importedOpids = {};
    (profiles || []).forEach(function (p) {
      if (p && p.deleted !== true && p.importedFrom) importedOpids[String(p.importedFrom)] = true;
      if (p && p.deleted !== true && /^imp-/.test(String(p.id || ''))) {
        importedOpids[String(p.id).replace(/^imp-/, '')] = true;
      }
    });

    (idx.linked || []).filter(function (r) {
      // 对方已删掉的档案：隐藏但保留记录（本账号可能写过备注，物理删不可恢复）
      if (!r || r.ownerDeleted) return false;
      if (r.ownerProfileId && importedOpids[String(r.ownerProfileId)]) return false;
      return true;
    }).forEach(function (r) {
      var alias = aliasMap[String(r.ownerProfileId || '')] || null;
      out.push({
        id: r.id,
        source: 'linked',
        // 有别名就用别名（本账号自定义），否则退回投影自带的（对方原始值）
        name: (alias && alias.aliasName) || r.name || '未命名',
        relation: alias ? (alias.aliasRelation || '') : (r.relation || ''),
        note: alias ? (alias.aliasNote || '') : (r.note || ''),
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

    // ===== 去重：旧模型迁移残留的本地档案 vs 新模型同步的关联投影 =====
    // v0.3.21 之前，关联人的档案被 migrateLegacyMembers 提升成本账号档案（写进 profile 表）；
    // 新模型又用 syncLinkedProfiles 把同一人的数据同步成关联投影（family 表）。
    // 结果是同一个人在列表里出现两份（本账号 + 关联档案），且改其中一份另一份不变，
    // 看起来像"改了又变回去"。
    // 这里把"撞名撞关系、且自己没有用药数据"的本地档案从家庭健康列表隐藏 ——
    // 它只是空壳，关联投影才是实时同步对方数据的那份。
    // 带用药数据的本地档案（用户真实创建的家人）一律保留，绝不误删。
    var linkedRows = (idx.linked || []).filter(function (r) { return r && !r.ownerDeleted; });
    var usedPids = {};
    try {
      (await MD.data.meds.list() || []).forEach(function (m) { if (m && m.profileId) usedPids[m.profileId] = true; });
    } catch (e) { /* 读不到用药数据就不隐藏，宁多勿漏 */ }
    var shellIds = await legacyShellProfileIds();
    out = out.filter(function (m) {
      if (m.source !== 'local') return true;
      var byName = linkedRows.some(function (r) { return samePerson(m, r); });
      var byTomb = Boolean(shellIds[m.id]);
      if (!byName && !byTomb) return true;
      return Boolean(usedPids[m.id]);  // 有用药数据的真实档案保留；空壳隐藏
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
    // 与关联投影同样的"改名必胜"策略：故障期曾有多台设备把同一条记录的 ver 抬得很高，
    // 普通 +1 的编辑会被残留旧副本覆盖（用户视角：改了又变回去）。显示字段是低频写、
    // 语义上"后写优先"，这里给足安全余量，一次写入即越过历史爬升区间。
    merged.ver = (Number(base && base.ver) || 0) + 500;
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
      return r && r.deleted !== true && !isAliasRow(r) && (r.name || r.relation);
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
  /* ======================= 导入即独立：把对方数据复制成本账号档案 =======================
   *
   * 设计变更（用户要求）：不再做「实时同步」。
   * 以前关联档案是对方数据的实时镜像，两边共用同一批记录，于是任何一端写入都会互相覆盖
   * ——改名被冲掉、药物被改回去，反复发生。现在改成一次性导入：
   *   · 对方的档案 → 本账号新建一份 profile（id 前缀 imp-，记住 importedFrom）
   *   · 药物/排程/服药记录 → 复制到本账号同名表，归属指向新建的 profile
   *   · 导入完立刻删掉授权与密钥，此后没有任何跨账号通路
   * 结果：数据各自独立，本地怎么改都不会影响对方，对方改什么也不会影响你。
   * 附带好处：导入的是真正的本账号档案，所以「用药提醒」的家庭档案里能直接看到这些药。
   */

  async function importEntitiesToLocal(ents, opts) {
    opts = opts || {};
    var grantId = String(opts.grantId || '');
    var aliasMap = opts.aliasMap || {};
    var stat = { profiles: 0, meds: 0, schedules: 0, intakes: 0 };
    var pidMap = {};          // ownerProfileId → 本地 profile id
    var livePid = {};         // 可挂药的本地 profile id

    var profiles = (ents.profiles || []).filter(function (p) { return p && p.deleted !== true; });
    for (var i = 0; i < profiles.length; i++) {
      var p = profiles[i];
      if (!p.id) continue;
      var localProfileId = 'imp-' + String(p.id);
      pidMap[String(p.id)] = localProfileId;
      var existProfile = null;
      try { existProfile = await MD.data.profiles.get(localProfileId); } catch (e) { existProfile = null; }
      if (existProfile && existProfile.deleted !== true) {
        // 已导入过且仍在：保留本地这份（用户可能已经改过名）
        livePid[localProfileId] = true;
        continue;
      }
      if (existProfile && existProfile.deleted === true) {
        // 用户删过这个导入档案，现在又重新导入了 → 复活它（保留用户改过的名字）
        var aliasRevive = aliasMap[String(p.id)] || null;
        await MD.data.profiles.save(Object.assign({}, existProfile, {
          deleted: false,
          name: (aliasRevive && aliasRevive.aliasName) || existProfile.name || p.name || '未命名',
          relation: (aliasRevive && aliasRevive.aliasRelation) || existProfile.relation || p.relation || '',
          importedGrant: grantId,
          ver: (Number(existProfile.ver) || 0) + 500,
          updatedAt: nowMs()
        }));
        livePid[localProfileId] = true;
        stat.profiles++;
        continue;
      }
      var alias = aliasMap[String(p.id)] || null;
      await MD.data.profiles.save({
        id: localProfileId,
        name: (alias && alias.aliasName) || p.name || '未命名',
        relation: (alias && alias.aliasRelation) || p.relation || '',
        note: (alias && alias.aliasNote) || p.note || '',
        color: p.color || '#4f8cff',
        birthDate: p.birthDate || '',
        archived: false,
        importedFrom: String(p.id),
        importedGrant: grantId,
        importedAt: nowMs(),
        createdAt: nowMs(),
        updatedAt: nowMs()
      });
      livePid[localProfileId] = true;
      stat.profiles++;
    }

    // 药物：只导入属于已导入档案的那些；id 原样保留（幂等，且不打断排程/历史的引用）
    var medOk = {};
    var meds = (ents.meds || []).filter(function (m) { return m && m.deleted !== true; });
    for (var j = 0; j < meds.length; j++) {
      var m = meds[j];
      if (!m.id) continue;
      var pid = pidMap[String(m.profileId || '')];
      if (!pid || !livePid[pid]) continue;
      var existMed = null;
      try { existMed = await MD.data.meds.get(m.id); } catch (e) { existMed = null; }
      if (existMed) {
        if (existMed.deleted === true) {
          // 重新导入：把删掉的药复活，并挂回（可能重建的）档案
          await MD.data.meds.save(Object.assign({}, existMed, {
            deleted: false, profileId: pid, importedGrant: grantId,
            ver: (Number(existMed.ver) || 0) + 500, updatedAt: nowMs()
          }));
          stat.meds++;
        }
        medOk[m.id] = true;
        continue;
      }
      var medRec = Object.assign({}, m, {
        profileId: pid,
        importedFrom: String(m.id),
        importedGrant: grantId,
        importedAt: nowMs(),
        updatedAt: nowMs()
      });
      delete medRec.__ver;
      await MD.data.meds.save(medRec);
      medOk[m.id] = true;
      stat.meds++;
    }

    // 排程与服药记录：保留原 id，只把归属改到本地档案；药物没导进来的跳过
    var schedules = (ents.schedules || []).filter(function (x) { return x && x.deleted !== true; });
    for (var k = 0; k < schedules.length; k++) {
      var sch = schedules[k];
      if (!sch.id || (sch.medicationId && !medOk[sch.medicationId])) continue;
      var spid = pidMap[String(sch.profileId || '')];
      if (!spid || !livePid[spid]) continue;
      var existSch = null;
      try { existSch = await MD.data.schedules.get(sch.id); } catch (e) { existSch = null; }
      if (existSch) {
        if (existSch.deleted === true) {
          await MD.data.schedules.save(Object.assign({}, existSch, {
            deleted: false, profileId: spid,
            ver: (Number(existSch.ver) || 0) + 500, updatedAt: nowMs()
          }));
          stat.schedules++;
        }
        continue;
      }
      var schRec = Object.assign({}, sch, { profileId: spid, importedGrant: grantId, updatedAt: nowMs() });
      await MD.data.schedules.save(schRec);
      stat.schedules++;
    }

    var intakes = (ents.intakes || []).filter(function (x) { return x && x.deleted !== true; });
    for (var t = 0; t < intakes.length; t++) {
      var it = intakes[t];
      if (!it.id) continue;
      var ipid = pidMap[String(it.profileId || '')];
      if (!ipid || !livePid[ipid]) continue;
      var existIntake = null;
      try { existIntake = await MD.data.intakes.get(it.id); } catch (e) { existIntake = null; }
      if (existIntake) continue;
      var intakeRec = Object.assign({}, it, { profileId: ipid, importedGrant: grantId, updatedAt: nowMs() });
      await MD.data.intakes.save(intakeRec);
      stat.intakes++;
    }

    return stat;
  }

  // 把现存「关联投影」一次性转成本账号的独立档案，然后删掉授权与密钥。
  //
  // 幂等：转完投影就没了，下次进来直接返回 0。
  // 网络失败时不转换（保留现状），下次启动再试 —— 不能因为一次离线就把档案丢掉。
  async function migrateLinkedToImported() {
    var rows = [];
    try { rows = await listFamilyRows(); } catch (e) { return 0; }
    var idx = classifyRows(rows);
    var linked = (idx.linked || []).filter(function (r) { return r && !r.ownerDeleted; });
    if (!linked.length) return 0;

    var aliasMap = {};
    rows.forEach(function (r) { if (isAliasRow(r) && r.aliasFor) aliasMap[String(r.aliasFor)] = r; });

    var groups = {};
    linked.forEach(function (r) {
      var g = String(r.grantId || '');
      (groups[g] = groups[g] || []).push(r);
    });

    var converted = 0;
    for (var gid in groups) {
      if (!Object.prototype.hasOwnProperty.call(groups, gid)) continue;
      var list = groups[gid];
      var grant = gid ? idx.grants[gid] : null;

      if (grant) {
        try {
          // 重启后内存里没有对方 DEK：先用本账号 DEK 解封存下的那份，否则拉不了数据。
          if (MD.familyLink && typeof MD.familyLink.getDek === 'function' && !MD.familyLink.getDek(gid)) {
            var unlocked = await openOwnerDekOf(grant);
            if (unlocked && typeof MD.familyLink.adoptDek === 'function') {
              MD.familyLink.adoptDek(gid, unlocked);
            }
          }
          var recs = await pullAll(gid);
          await importEntitiesToLocal(recordsToEntities(recs), { grantId: gid, aliasMap: aliasMap });
        } catch (e) {
          continue;   // 网络不可达/密钥缺失：保留投影，下次启动再试
        }
      }

      // 兜底：对方已无档案、或没有可用授权时，至少把投影里的显示信息落成本地档案
      for (var i = 0; i < list.length; i++) {
        var row = list[i];
        var opid = String(row.ownerProfileId || '');
        if (!opid) continue;
        var localId = 'imp-' + opid;
        var exist = null;
        try { exist = await MD.data.profiles.get(localId); } catch (e) { exist = null; }
        if (!exist) {
          var alias = aliasMap[opid] || null;
          try {
            await MD.data.profiles.save({
              id: localId,
              name: (alias && alias.aliasName) || row.name || '未命名',
              relation: (alias && alias.aliasRelation) || row.relation || '',
              note: (alias && alias.aliasNote) || row.note || '',
              color: row.color || '#4f8cff',
              birthDate: row.birthDate || '',
              archived: false,
              importedFrom: opid,
              importedGrant: gid,
              importedAt: nowMs(),
              createdAt: nowMs(),
              updatedAt: nowMs()
            });
          } catch (e) { continue; }
        }
        converted++;
      }

      // 断开跨账号通路：投影、别名、授权、密钥一并删除
      for (var j = 0; j < list.length; j++) {
        try { await dropLinked(list[j].ownerProfileId); } catch (e) { /* 忽略 */ }
      }
      if (grant) { try { await dropGrant(gid); } catch (e) { /* 忽略 */ } }
    }
    return converted;
  }

  // 给「还没有别名行」的投影补一条，值取当前显示名（也就是用户此前改过的结果）。
  //
  // 幂等：已有别名行的投影原样跳过。这样升级后第一次打开时，用户此前辛苦改好的
  // 名字会被冻结进别名行，之后的同步/旧设备重写都不会再影响显示。
  async function migrateAliasRows() {
    var rows = [];
    try { rows = await listFamilyRows(); } catch (e) { return 0; }
    var haveAlias = {};
    rows.forEach(function (r) { if (isAliasRow(r) && r.aliasFor) haveAlias[String(r.aliasFor)] = true; });
    var n = 0;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      if (!r || !r.ownerProfileId) continue;
      var opid = String(r.ownerProfileId);
      if (haveAlias[opid]) continue;
      try {
        await MD.data.family.save({
          id: aliasRowId(opid),
          aliasFor: opid,
          aliasName: r.name || '',
          aliasRelation: r.relation || '',
          aliasNote: r.note || '',
          ver: (Number(r.ver) || 0) + 500,
          updatedAt: nowMs()
        });
        haveAlias[opid] = true;
        n++;
      } catch (e) { /* 单条失败下次再试 */ }
    }
    return n;
  }

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
      el('div', { class: 'fam-title' }, [el('span', { text: title })]),
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
      el('span', { class: 'muted', text: k, style: { fontSize: '13px', letterSpacing: '.2px' } }),
      el('span', { class: 'fam-num', text: v, style: { fontWeight: '600', fontSize: '14px' } })
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

    host.appendChild(el('div', { class: 'row', style: { gap: '6px', alignItems: 'center', marginBottom: '14px' } }, [
      el('button', {
        class: 'btn btn-ghost fam-back', type: 'button', 'aria-label': '返回',
        style: { width: '40px', minWidth: '40px', padding: '0', justifyContent: 'center' },
        onClick: function () { S.view = 'list'; S.memberId = null; paint(); }
      }, [MD.icon('chevron')]),
      el('div', { style: { display: 'flex', flexDirection: 'column', gap: '2px', minWidth: '0' } }, [
        el('div', { text: member.name || '未命名', style: { fontSize: '17px', fontWeight: '700', letterSpacing: '.2px' } }),
        el('span', {
          class: 'muted',
          style: { fontSize: '12px', letterSpacing: '.2px' },
          // 本账号档案的数据就在本地，说"实时"会让人以为在连服务器；
          // 关联档案才是真的每次进页面从对方账号拉一次。
          text: (member.relation || '')
            + (data ? (' · ' + (member.source === 'linked'
              ? '导入副本 · ' + fmtDateTime(data.exportedAt)
              : '本账号数据')) : '')
        })
      ])
    ]));

    if (member.note) {
      host.appendChild(el('p', { class: 'fam-hint', text: member.note, style: { margin: '0 0 12px' } }));
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
        el('div', { class: 'row row--wrap', style: { gap: '10px', marginTop: '12px' } }, [
          el('button', {
            class: 'btn btn-primary', type: 'button',
            onClick: function () {
              if (isLinkedMember) { S.tab = 'link'; S.view = 'list'; paint(); }
              else { MD.router.go('med'); }
            }
          }, [el('span', { text: isLinkedMember ? '去关联' : '去用药提醒' })]),
          el('button', {
            class: 'btn btn-ghost', type: 'button',
            style: { color: 'var(--danger)', gap: '6px' },
            onClick: async function () {
              if (await deleteMember(member)) { S.view = 'list'; S.memberId = null; }
              paint();
            }
          }, [MD.icon('trash'), el('span', { text: '删除此档案' })])
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
        ? '这是尚未导入完成的关联档案：下次打开 meddesk 会自动转成本账号的独立副本，之后数据完全归你。'
        : '数据来自对方主动分享的离线快照，非实时。需要最新数据请让对方重新生成关联码。',
      style: { fontSize: '12px', margin: '4px 0 0' }
    }));

    // 详情页也要能删：看完数据想清掉这个档案是很自然的动作，
    // 否则用户必须退回列表再找一次。
    host.appendChild(el('div', { class: 'row', style: { marginTop: '16px' } }, [
      el('button', {
        class: 'btn btn-ghost', type: 'button',
        style: { color: 'var(--danger)', gap: '6px' },
        onClick: async function () {
          if (await deleteMember(member)) { S.view = 'list'; S.memberId = null; }
          paint();
        }
      }, [MD.icon('trash'), el('span', { text: member.source === 'linked' ? '移除此关联档案' : '删除此档案' })])
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
    // 先数一下这个档案下挂了多少药：删档案不会连带删药（删人 ≠ 删药），
    // 必须在这里说清楚，否则用户会看到"药都在，但哪个档案里都没有"。
    var medCount = 0;
    if (member.source !== 'linked') {
      try {
        var meds = (await MD.data.meds.list()) || [];
        meds.forEach(function (m) {
          if (m && m.deleted !== true && String(m.profileId || '') === String(member.id)) medCount++;
        });
      } catch (e) { medCount = 0; }
    }

    var tail = medCount > 0
      ? ('该档案下有 ' + medCount + ' 味药。删除档案不会删掉这些药，它们会变成「未归类」，'
        + '在「全部」里还能看到，随后可在「家庭关联 → 存储诊断」里一键归位或恢复档案。')
      : '删除后「用药提醒」里也会一起消失。';

    var ok = await MD.ui.confirm({
      title: '删除档案',
      message: member.source === 'linked'
        ? ('确定移除「' + (member.name || '未命名') + '」？只删除本账号里的这份关联副本，'
          + '对方的账号、对方的档案与药物清单都不受影响。')
        : ('确定删除「' + (member.name || '未命名') + '」？这是本账号的档案。' + tail),
      confirmText: '删除',
      danger: true
    });
    if (!ok) return false;

    return purgeMember(member, '已删除');
  }

  /**
   * 真正执行删除，并在删除后**回读校验**。
   *
   * 【为什么必须校验】
   * 上一版对投影档案是按 ownerProfileId 重新拼 id 去删，而不是用列表里那条记录的
   * 真实 id —— 一旦两者不一致（历史数据、字段缺失），删除会静默落空，而函数仍然
   * toast「已移除」并返回 true。用户看到的是"点了几次都没反应"，日志里却一切正常。
   * 现在统一按真实 id 删，删完再读一遍确认它真的不在了，才报成功。
   */
  async function purgeMember(member, successText) {
    if (!member || !member.id) return false;
    var isLinked = member.source === 'linked';
    try {
      // 1) 档案/关联本体
      if (isLinked) {
        try { await MD.data.family.remove(member.id); } catch (e) { /* 下面统一校验 */ }
      } else {
        await MD.data.profiles.remove(member.id);
      }

      // 2) 同一档案可能还挂着别的 family 记录（旧式关联、重复投影），一并清掉，
      //    否则列表里会残留一条读不到数据的空壳。
      var fam = await rawTable('family');
      for (var i = 0; i < fam.length; i++) {
        var r = fam[i];
        if (!r || r.deleted) continue;
        var same = String(r.id) === String(member.id)
          || (r.profileId && String(r.profileId) === String(member.id))
          || (r.ownerProfileId && linkedRowId(r.ownerProfileId) === String(member.id));
        if (!same) continue;
        try { await MD.data.family.remove(r.id); } catch (e) { /* 继续清理其余 */ }
      }

      // 3) 本地快照
      await dropSnapshot(member.id);

      // 4) 回读校验：仍然能读到未删除的同 id 记录，就说明没删掉
      var left = [];
      if (isLinked) {
        left = (await rawTable('family')).filter(function (x) {
          return x && !x.deleted && String(x.id) === String(member.id);
        });
      } else {
        left = (await rawTable('profile')).filter(function (x) {
          return x && !x.deleted && String(x.id) === String(member.id);
        });
      }

      if (left.length) {
        S.err = '删除未生效：记录仍存在（' + member.id + '）。'
          + '可能是浏览器存储异常，请刷新后再试，或到「存储诊断」里强制清理。';
        paint();
        return false;
      }

      S.err = '';
      toast(successText || '已删除');
      return true;
    } catch (e) {
      S.err = '删除失败：' + (e && e.message ? e.message : '未知错误');
      return false;
    }
  }

  /* ==================== 6.5 存储诊断 ==================== */

  // 直接读底层表：MD.data.*.list() 会过滤掉已删除记录，
  // 排查"删了还在"必须看到墓碑（deleted=true）才能判断到底哪一步没生效。
  async function rawTable(name) {
    try { return (await MD.db.list(name)) || []; } catch (e) { return []; }
  }

  /**
   * 体检：把可能造成"看着有、删不掉"的四种情况都找出来。
   *
   *   1. 同名档案   —— 本账号档案 + 投影混在一起时最容易出现，删一个另一个还在
   *   2. 孤儿投影   —— 记录在，但对应的授权行没了，数据永远读不到
   *   3. 孤儿授权   —— 授权在，但没有任何档案用它，纯占位
   *   4. 墓碑堆积   —— 软删记录会一直参与同步，越积越多
   */
  // 药单自检：回答「药到底还在不在、挂给了谁、为什么没显示」。
  //
  // 只读本机 IndexedDB，不写任何东西。输出两段：
  //   · 概览：每个档案下有几味药、有几味药没挂档案（这类在任何档案里都看不到）
  //   · 明细：每条药的 id / 名称 / profileId / ver / 删除标记
  async function medReport() {
    var meds = [], schedules = [], profiles = [];
    try { meds = await MD.db.list('medication'); } catch (e) { meds = []; }
    try { schedules = await MD.db.list('schedule'); } catch (e) { schedules = []; }
    try { profiles = await MD.db.list('profile'); } catch (e) { profiles = []; }

    var liveMeds = (meds || []).filter(function (m) { return m && m.deleted !== true; });
    var liveSched = (schedules || []).filter(function (x) { return x && x.deleted !== true; });
    var liveProfiles = (profiles || []).filter(function (p) { return p && p.deleted !== true; });

    var schedByMed = {};
    liveSched.forEach(function (sc) { if (!schedByMed[sc.medicationId]) schedByMed[sc.medicationId] = sc; });

    var groups = {};
    var orphans = [];
    liveMeds.forEach(function (m) {
      var pid = String(m.profileId || '');
      if (!pid) { orphans.push(m); return; }
      (groups[pid] = groups[pid] || []).push(m);
    });

    // 墓碑分两类：内置示例（sample/_example，删了无所谓）与用户自己的药。
    // 混在一起会让人以为"我的药被删了"——实测用户就被这 4 条示例吓到过。
    var allDead = (meds || []).filter(function (m) { return m && m.deleted === true; });
    var deadMeds = allDead.filter(function (m) { return !(m.sample === true || m._example === true); });
    var deadSamples = allDead.filter(function (m) { return m.sample === true || m._example === true; });
    var deadSchedByMed = {};
    (schedules || []).forEach(function (sc) {
      if (sc && sc.deleted === true && sc.medicationId) deadSchedByMed[sc.medicationId] = sc;
    });

    var lines = [];
    lines.push('药物总数：' + liveMeds.length + ' 条（含已删除墓碑 ' + ((meds || []).length - liveMeds.length) + ' 条）');
    lines.push('用药方案：' + liveSched.length + ' 条');
    lines.push('档案：' + liveProfiles.length + ' 个');
    lines.push('');
    liveProfiles.forEach(function (p) {
      var list = groups[String(p.id)] || [];
      lines.push('· ' + (p.name || '未命名') + '（' + String(p.id).slice(0, 12) + '）：' + list.length + ' 味药');
    });
    if (orphans.length) lines.push('· 未挂档案的药：' + orphans.length + ' 味（这些在任何档案下都不会显示）');
    var noSched = liveMeds.filter(function (m) { return !schedByMed[m.id]; });
    if (noSched.length) lines.push('· 没有用药方案（显示为未设置排程）：' + noSched.length + ' 味');
    if (deadMeds.length) {
      lines.push('');
      lines.push('已删除的药物（你的数据，可恢复）：');
      deadMeds.forEach(function (m) {
        lines.push('  × ' + (m.name || '未命名') + ' | 原档案 ' + String(m.profileId || '无').slice(0, 12)
          + ' | v' + (Number(m.ver) || 0) + (deadSchedByMed[m.id] ? ' | 方案已删' : ''));
      });
    }
    if (deadSamples.length) {
      lines.push('');
      lines.push('内置示例药残留：' + deadSamples.length + ' 条（不是你的数据，可忽略/清理）');
    }
    lines.push('');
    lines.push('—— 明细（名称 | 档案 | 版本 | 方案）——');
    liveMeds.forEach(function (m) {
      var sc = schedByMed[m.id];
      lines.push([
        (m.name || '未命名'),
        String(m.profileId || '无').slice(0, 12),
        'v' + (Number(m.ver) || 0),
        sc ? ((sc.kind || '') + ' ' + ((sc.times || []).join('/'))) : '无方案'
      ].join(' | '));
    });

    return {
      text: lines.join('\n'),
      meds: liveMeds, groups: groups, orphans: orphans,
      deleted: deadMeds.map(function (m) {
        return { id: m.id, name: m.name || '未命名', profileId: m.profileId || '', ver: Number(m.ver) || 0 };
      }),
      sampleTombstones: deadSamples.length
    };
  }

  async function storageReport() {
    var profiles = (await rawTable('profile')).filter(Boolean);
    var family = (await rawTable('family')).filter(Boolean);

    var activeProfiles = profiles.filter(function (p) { return !p.deleted; });
    var activeFamily = family.filter(function (r) { return !r.deleted; });
    var idx = classifyRows(activeFamily);
    var linked = idx.linked || [];
    var grants = idx.grants || {};

    var names = {};
    activeProfiles.forEach(function (p) {
      var k = String(p.name || '未命名').trim();
      (names[k] = names[k] || []).push({ kind: 'profile', id: p.id, label: k });
    });
    linked.forEach(function (r) {
      var k = String(r.name || '未命名').trim();
      (names[k] = names[k] || []).push({ kind: 'link', id: r.id, label: k });
    });
    var dup = Object.keys(names).filter(function (k) { return names[k].length > 1; })
      .map(function (k) { return { name: k, items: names[k] }; });

    var usedGrant = {};
    linked.forEach(function (r) { if (r.grantId) usedGrant[String(r.grantId)] = true; });

    var orphanLinked = linked.filter(function (r) {
      return r.grantId && !grants[String(r.grantId)];
    });
    var orphanGrants = Object.keys(grants).filter(function (g) { return !usedGrant[g]; });

    return {
      activeProfiles: activeProfiles,
      linked: linked,
      grants: grants,
      dup: dup,
      orphanLinked: orphanLinked,
      orphanGrants: orphanGrants,
      tomb: {
        profile: profiles.filter(function (p) { return p.deleted; }).length,
        family: family.filter(function (r) { return r.deleted; }).length
      }
    };
  }

  // 物理删除墓碑记录。软删只把 deleted 置 true，记录会一直参与同步；
  // 这里直接从 IndexedDB 删掉，之后不会再被推送。
  async function purgeTombstones() {
    var n = 0;
    var tables = ['profile', 'family', 'medication', 'schedule', 'intake'];
    for (var t = 0; t < tables.length; t++) {
      var rows = await rawTable(tables[t]);
      for (var i = 0; i < rows.length; i++) {
        if (!rows[i] || !rows[i].deleted) continue;
        try { await MD.db.del(tables[t], rows[i].id); n++; } catch (e) { /* 单条失败不阻断 */ }
      }
    }
    return n;
  }

  // 彻底清理「撞名撞关系的空壳本地档案」（v0.3.21 迁移残留）。
  // 判据与 listMembers 去重一致：本地档案与某条关联投影同名同关系，且自己无用药数据。
  // 软删（deleted=true）：从列表隐去且可恢复；带用药数据的真实家人档案不碰。
  // 返回清理条数。
  // 把一份「与某条关联投影撞名撞关系、且本账号没有用药数据」的本地档案标记为已隐藏。
  // 关键：按调用方传入的 旧名+旧关系 匹配，而不是依赖改名后的当前名字 ——
  // 用户先把关联档案「老爸」改成「老公」，那份仍叫「老爸」的空壳本地档案
  // 也能被精确找到并隐藏，不会因为改名后姓名对不上又冒出来（用户视角：又变 4 条）。
  async function suppressDuplicateLocal(oldName, oldRelation, linkedRowId) {
    if ((oldName || '') === '' && (oldRelation || '') === '') return false;
    var profiles = [];
    try { profiles = await MD.data.profiles.list(); } catch (e) { return false; }
    var usedPids = {};
    try {
      (await MD.data.meds.list() || []).forEach(function (m) { if (m && m.profileId) usedPids[m.profileId] = true; });
    } catch (e) { /* 读不到用药数据就不隐藏，宁多勿漏 */ }
    var probe = { name: oldName || '', relation: oldRelation || '' };
    var hit = null;
    for (var i = 0; i < profiles.length; i++) {
      var pr = profiles[i];
      if (!pr || pr.deleted === true || pr.archived) continue;
      if (usedPids[pr.id]) continue;            // 真实档案（有用药数据）绝不碰
      if (!samePerson(pr, probe)) continue;
      hit = pr; break;
    }
    if (!hit) return false;
    try {
      // ver 预留大安全余量：多设备曾在故障期把该记录的 ver 抬高，
      // 残留的旧副本若 ver 更高会覆盖墓碑（空壳复活）。+500 足够越过历史爬升。
      await MD.data.profiles.save(Object.assign({}, hit, {
        deleted: true,
        ver: (Number(hit.ver) || 0) + 500,
        suppressedByLinked: linkedRowId || '',
        updatedAt: nowMs()
      }));
      return true;
    } catch (e) { return false; }
  }

  // 迁移残留空壳的精确识别（不依赖姓名/关系，改名后依然有效）：
  //   a) 旧成员行被 migrateLegacyMembers 标删后墓碑仍在本库，其 id 就是当时创建的
  //      本地 profile 的 id（迁移用 id: old.id 建 profile）；
  //   b) migrateLegacyGrants 迁移时在 grant 行上写了 legacyProfileId。
  // 两个来源都直接指向「这份本地档案是迁移产物」，比按名字匹配可靠得多 ——
  // 用户把关联档案改名后，名字配对失效，空壳会重新冒出来（实测发生过）。
  async function legacyShellProfileIds() {
    var ids = {};
    try {
      var raw = await MD.db.list('family');
      (raw || []).forEach(function (r) {
        if (!r || r.deleted !== true) return;
        if (r.ownerProfileId) return;                       // 投影墓碑，不是成员墓碑
        if (/^grant:/.test(String(r.id || ''))) return;     // 授权墓碑
        if (r.profileId && String(r.id) !== String(r.profileId)) return; // 旧式 link 行，id 是 uid
        if ((r.name || r.relation) && r.id) ids[String(r.id)] = true;
      });
    } catch (e) { /* 读不到原始行就靠来源 b */ }
    try {
      var idx = await loadIndex();
      Object.keys(idx.grants || {}).forEach(function (k) {
        var g = idx.grants[k];
        if (g && g.legacyProfileId) ids[String(g.legacyProfileId)] = true;
      });
    } catch (e) { /* 忽略 */ }
    return ids;
  }

  async function dedupeLocalProfiles() {
    var profiles = [];
    try { profiles = await MD.data.profiles.list(); } catch (e) { profiles = []; }
    var idx;
    try { idx = await loadIndex(); } catch (e) { idx = { linked: [] }; }
    var linkedRows = (idx.linked || []).filter(function (r) { return r && !r.ownerDeleted; });
    var usedPids = {};
    try {
      (await MD.data.meds.list() || []).forEach(function (m) { if (m && m.profileId) usedPids[m.profileId] = true; });
    } catch (e) { /* 忽略 */ }
    var shellIds = await legacyShellProfileIds();
    var n = 0;
    for (var i = 0; i < profiles.length; i++) {
      var pr = profiles[i];
      if (!pr || pr.deleted === true || pr.archived) continue;
      var sameAsLinked = linkedRows.some(function (r) { return samePerson(pr, r); });
      if (!sameAsLinked && !shellIds[String(pr.id)]) continue;
      if (usedPids[pr.id]) continue;  // 真实档案保留
      try {
        await MD.data.profiles.save(Object.assign({}, pr, {
          deleted: true,
          suppressedByLinked: linkedRows.filter(function (r) {
            return samePerson(pr, r);
          }).map(function (r) { return r.id; })[0] || '',
          updatedAt: nowMs()
        }));
        n++;
      } catch (e) { /* 单条失败不阻断其余 */ }
    }
    return n;
  }

  async function purgeOrphans() {
    var rep = await storageReport();
    var n = 0;
    for (var i = 0; i < rep.orphanLinked.length; i++) {
      try { await MD.db.del('family', rep.orphanLinked[i].id); n++; } catch (e) { /* 忽略 */ }
    }
    for (var j = 0; j < rep.orphanGrants.length; j++) {
      var g = rep.grants[rep.orphanGrants[j]];
      if (!g || !g.id) continue;
      try { await MD.db.del('family', g.id); n++; } catch (e) { /* 忽略 */ }
    }
    return n;
  }

  function openDiagnoseSheet() {
    if (!MD.ui || typeof MD.ui.sheet !== 'function') { toast('当前环境不支持', 'error'); return; }

    MD.ui.sheet({
      title: '存储诊断',
      body: function (host) {
        var box = el('div', { class: 'stack', style: { gap: '10px' } });
        host.appendChild(box);
        box.appendChild(el('p', { class: 'fam-hint', text: '正在读取本机存储…' }));

        storageReport().then(function (rep) {
          clear(box);

          var lines = [];
          lines.push('本账号档案：' + rep.activeProfiles.length + ' 条');
          lines.push('关联投影档案：' + rep.linked.length + ' 条');
          lines.push('授权：' + Object.keys(rep.grants).length + ' 条');
          lines.push('已删除残留：档案 ' + rep.tomb.profile + ' 条 / 关联 ' + rep.tomb.family + ' 条');
          if (rep.dup.length) lines.push('同名档案：' + rep.dup.length + ' 组');
          if (rep.orphanLinked.length) lines.push('孤儿投影：' + rep.orphanLinked.length + ' 条');
          if (rep.orphanGrants.length) lines.push('孤儿授权：' + rep.orphanGrants.length + ' 条');

          box.appendChild(el('pre', {
            class: 'fam-hint',
            text: lines.join('\n'),
            style: { whiteSpace: 'pre-wrap', fontFamily: 'inherit', margin: '0' }
          }));

          // 未归类数据：删档案不删药 → 药会挂在已删除的档案下，任何档案视图里都看不到。
          orphanReport().then(function (orph) {
            if (!orph.meds.length) return;
            var ob = el('div', { class: 'stack', style: { gap: '8px', marginTop: '4px' } });
            ob.appendChild(el('p', {
              class: 'fam-hint',
              style: { color: 'var(--danger)', margin: '0' },
              text: '未归类药物 ' + orph.meds.length + ' 味（' + Object.keys(orph.byPid).length
                + ' 个原档案已不存在）—— 只在「全部」里能看到，各档案下都看不到。'
            }));
            ob.appendChild(el('div', { class: 'row row--wrap', style: { gap: '8px' } }, [
              el('button', {
                class: 'btn btn-primary', type: 'button', text: '恢复原档案',
                onClick: async function () {
                  var n = await reviveOrphanProfiles();
                  MD.toast.show(n > 0 ? ('已恢复 ' + n + ' 个档案，药物归位完成') : '没有可恢复的档案', n > 0 ? 'success' : 'warn');
                  MD.ui.closeSheet();
                  paint();
                }
              }),
              el('button', {
                class: 'btn btn-ghost', type: 'button', text: '全部归到…',
                onClick: function () {
                  var live = orph.liveProfiles;
                  if (!live.length) { MD.toast.show('当前没有可用档案，请先新建一个', 'warn'); return; }
                  MD.ui.sheet({
                    title: '把未归类药物归到',
                    body: function (host) {
                      host.appendChild(el('div', { class: 'stack', style: { gap: '8px' } }, live.map(function (pr) {
                        return el('button', {
                          class: 'btn btn-ghost', type: 'button', text: (pr.name || '未命名') + (pr.relation ? ' · ' + pr.relation : ''),
                          onClick: async function () {
                            var n = await moveOrphansTo(pr.id);
                            MD.toast.show('已归位 ' + n + ' 味药到「' + (pr.name || '未命名') + '」', 'success');
                            MD.ui.closeSheet();
                            MD.ui.closeSheet();
                            paint();
                          }
                        });
                      })));
                    }
                  });
                }
              })
            ]));
            box.appendChild(ob);
          }).catch(function () { /* 诊断失败不影响主体 */ });

          // 药单自检：用户反馈「药超过 10 个会丢」，但服务端显示 0 条墓碑，
          // 需要看到每味药挂在哪个档案、有没有方案，才能判断是丢失还是被归属过滤掉。
          var medBox = el('pre', {
            class: 'fam-hint',
            text: '正在统计药物…',
            style: { whiteSpace: 'pre-wrap', fontFamily: 'inherit', margin: '10px 0 0', maxHeight: '44vh', overflow: 'auto' }
          });
          box.appendChild(medBox);
          medReport().then(function (mr) {
            medBox.textContent = mr.text;
            if (mr.deleted && mr.deleted.length) {
              box.appendChild(el('p', {
                class: 'fam-hint',
                text: '已删除的药物（数据仍在，可一键恢复）：',
                style: { marginTop: '10px' }
              }));
              mr.deleted.forEach(function (m) {
                var row = el('div', {
                  class: 'row',
                  style: { justifyContent: 'space-between', alignItems: 'center', gap: '10px', padding: '4px 0' }
                }, [
                  el('span', {
                    text: m.name + (m.profileId ? ' · ' + String(m.profileId).slice(0, 8) : ' · 无档案'),
                    style: { fontSize: '13px', flex: '1 1 auto', minWidth: '0' }
                  }),
                  el('button', {
                    class: 'btn btn-ghost', type: 'button', text: '恢复',
                    style: { flex: '0 0 auto' },
                    onClick: async function () {
                      try {
                        var okRestore = await restoreMed(m.id);
                        toast(okRestore ? '已恢复「' + m.name + '」' : '恢复失败：记录不存在', okRestore ? 'success' : 'error');
                        if (okRestore) { MD.ui.closeSheet(); openDiagnoseSheet(); }
                      } catch (e) {
                        toast('恢复失败：' + (e && e.message ? e.message : '未知错误'), 'error');
                      }
                    }
                  })
                ]);
                box.appendChild(row);
              });
            }
            box.appendChild(el('button', {
              class: 'btn btn-ghost', type: 'button', text: '复制药物自检信息',
              style: { marginTop: '8px' },
              onClick: function () {
                var text = mr.text;
                try {
                  if (navigator.clipboard && navigator.clipboard.writeText) {
                    navigator.clipboard.writeText(text).then(function () { toast('已复制，可直接粘贴发我'); },
                      function () { toast('复制失败，可长按选中文本', 'warn'); });
                    return;
                  }
                } catch (e) { /* 降级到下面 */ }
                toast('当前环境不支持一键复制，请长按选中文本', 'warn');
              }
            }));
          }).catch(function () { medBox.textContent = '药物统计失败'; });

          // 同名档案逐个列出，每个都能就地强制清理
          if (rep.dup.length) {
            box.appendChild(el('p', {
              class: 'fam-hint',
              text: '同名档案（点右侧可逐个清理）：',
              style: { marginTop: '8px' }
            }));
            rep.dup.forEach(function (group) {
              group.items.forEach(function (item) {
                box.appendChild(el('div', {
                  class: 'row',
                  style: { justifyContent: 'space-between', alignItems: 'center', gap: '8px' }
                }, [
                  el('span', {
                    class: 'fam-hint',
                    text: group.name + ' · ' + (item.kind === 'profile' ? '本账号档案' : '关联投影'),
                    style: { flex: '1 1 auto', minWidth: '0' }
                  }),
                  el('button', {
                    class: 'fam-act fam-act--danger', type: 'button',
                    title: '强制清理', 'aria-label': '强制清理',
                    onClick: async function () {
                      var okDel = await purgeMember({
                        id: item.id,
                        source: item.kind === 'link' ? 'linked' : 'local'
                      }, '已清理');
                      toast(okDel ? '已清理' : '清理未生效', okDel ? 'success' : 'error');
                      MD.ui.closeSheet();
                      paint();
                    }
                  }, [MD.icon('trash')])
                ]));
              });
            });
          }

          var acts = el('div', { class: 'row row--wrap', style: { gap: '8px', marginTop: '10px' } });

          if (rep.tomb.profile + rep.tomb.family > 0) {
            acts.appendChild(el('button', {
              class: 'btn btn-ghost', type: 'button',
              onClick: async function () {
                var n = await purgeTombstones();
                toast('已清理 ' + n + ' 条残留记录');
                MD.ui.closeSheet();
                paint();
              }
            }, [el('span', { text: '清理已删残留' })]));
          }

          if (rep.orphanLinked.length + rep.orphanGrants.length > 0) {
            acts.appendChild(el('button', {
              class: 'btn btn-ghost', type: 'button',
              onClick: async function () {
                var n = await purgeOrphans();
                toast('已清理 ' + n + ' 条孤儿记录');
                MD.ui.closeSheet();
                paint();
              }
            }, [el('span', { text: '清理孤儿关联' })]));
          }

          // 同名档案里同时含「本账号档案」与「关联投影」→ 旧模型迁移残留，可一键清掉空壳本地档案
          var hasDupLocal = rep.dup.some(function (g) {
            var kinds = {};
            g.items.forEach(function (it) { kinds[it.kind] = true; });
            return kinds.profile && kinds.link;
          });
          if (hasDupLocal) {
            acts.appendChild(el('button', {
              class: 'btn btn-ghost', type: 'button',
              onClick: async function () {
                var n = await MD.family.dedupeLocalProfiles();
                toast(n > 0 ? ('已清理 ' + n + ' 条重复档案（关联投影保留）') : '没有可清理的重复档案', n > 0 ? 'success' : 'info');
                MD.ui.closeSheet();
                paint();
              }
            }, [MD.icon('trash'), el('span', { text: '清理重复档案' })]));
          }

          if (acts.children.length) box.appendChild(acts);

          box.appendChild(el('p', {
            class: 'fam-hint',
            text: '清理只影响本机存储，不会删除对方账号里的任何数据。'
              + '已删残留会随同步继续上传，清理后不再参与同步。',
            style: { marginTop: '10px' }
          }));
        }).catch(function (e) {
          clear(box);
          box.appendChild(el('p', { class: 'fam-hint', text: '诊断失败：' + (e && e.message ? e.message : '未知错误') }));
        });
      },
      actions: [
        { label: '关闭', kind: 'ghost', onClick: function () { MD.ui.closeSheet(); } }
      ]
    });
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
      var stockText = m.stock != null
        ? '库存 ' + m.stock + ' ' + (m.dosageUnit || '')
        : '';

      var nameRow = el('div', { class: 'fam-med__name' }, [
        el('span', { text: m.name })
      ]);
      if (m.critical) nameRow.appendChild(el('span', { class: 'badge badge--warn', text: '关键' }));

      var meta = el('div', { class: 'fam-med__meta' }, [
        el('span', { text: sub || DASH })
      ]);
      if (stockText) {
        meta.appendChild(el('span', { text: ' · ' + stockText }));
      }

      // 「改 / 删」改成图标：药名长短不一，两个文字按钮会把剂量与频次挤成两行；
      // 图标 + title/aria-label 既省横向空间，又不丢可访问性与鼠标悬停提示。
      rows.push(el('div', { class: 'fam-med' }, [
        el('div', { class: 'fam-med__body' }, [nameRow, meta]),
        canEdit ? el('div', { class: 'fam-med__acts' }, [
          el('button', {
            class: 'fam-act', type: 'button', title: '编辑', 'aria-label': '编辑药物',
            onClick: function () { openMedForm(member, data, m); }
          }, [MD.icon('edit')]),
          el('button', {
            class: 'fam-act fam-act--danger', type: 'button', title: '删除', 'aria-label': '删除药物',
            onClick: function () { removeSharedMed(member, data, m); }
          }, [MD.icon('trash')])
        ]) : null
      ]));
    });

    if (!rows.length) {
      rows.push(el('p', {
        class: 'fam-hint',
        text: isLinked ? '对方还没有为这个成员录入药物' : '还没有药物，点下面的「添加药物」开始'
      }));
    }

    if (canEdit) {
      rows.push(el('button', {
        class: 'btn btn-ghost', type: 'button',
        style: { marginTop: '10px', gap: '6px' },
        onClick: function () { openMedForm(member, data, null); }
      }, [
        MD.icon('plus'),
        el('span', { text: '添加药物' })
      ]));
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
    var linkedId = linkedRowId(ownerProfileId);
    var aliasId = aliasRowId(ownerProfileId);

    var rows = [];
    try { rows = await listFamilyRows(); } catch (e) { rows = []; }
    var linkedRow = null, aliasRow = null;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].id === linkedId) linkedRow = rows[i];
      if (rows[i].id === aliasId) aliasRow = rows[i];
    }

    // 撞名的空壳本地档案要隐藏：用「投影里的对方原名」和「改名前的别名」两个候选，
    // 因为空壳是按迁移时的名字创建的，两者都可能对上。
    var candidates = [];
    if (linkedRow && (linkedRow.name || linkedRow.relation)) {
      candidates.push([linkedRow.name || '', linkedRow.relation || '']);
    }
    if (aliasRow && (aliasRow.name || aliasRow.relation)) {
      candidates.push([aliasRow.name || '', aliasRow.relation || '']);
    }
    for (var c = 0; c < candidates.length; c++) {
      try { await suppressDuplicateLocal(candidates[c][0], candidates[c][1], linkedId); } catch (e) { /* 忽略 */ }
    }

    // 写别名行：投影行保持不动（对方的名字仍属于对方），本账号的显示名只存这里。
    // 字段名故意用 aliasXxx 而不是 name/relation：
    // 旧版本客户端的 migrateLegacyMembers 会把 family 表里任何「有 name/relation 的行」
    // 提升成 profile，别名行若带 name 就会在那台设备上凭空长出空壳档案。
    var rec2 = Object.assign({}, aliasRow || {}, {
      id: aliasId,
      aliasFor: String(ownerProfileId),
      aliasName: rec.name || '',
      aliasRelation: rec.relation || '',
      aliasNote: rec.note || '',
      updatedAt: nowMs()
    });
    // 防御：清掉可能残留的旧字段，避免被旧代码误识别
    delete rec2.name;
    delete rec2.relation;
    delete rec2.note;
    // ver 仍给余量：别名行也可能被本账号的其他设备并发写，后写优先即可。
    rec2.ver = (Number(aliasRow && aliasRow.ver) || 0) + 500;
    await MD.data.family.save(rec2);
    return rec2;
  }

  // 把一份本账号的迁移空壳标记隐藏（带版本余量，避免被残留旧副本的更高 ver 复活）。
  async function hideLocalShell(id) {
    if (!id) return false;
    try {
      var pr = await MD.data.profiles.get(id);
      if (!pr || pr.deleted === true) return false;
      await MD.data.profiles.save(Object.assign({}, pr, {
        deleted: true,
        ver: (Number(pr.ver) || 0) + 500,
        suppressedByLinked: 'rename-redirect',
        updatedAt: nowMs()
      }));
      return true;
    } catch (e) { return false; }
  }

  // 决定「编辑这份成员时改名该写到哪」。
  //
  // 本账号里可能残留着与某条关联投影「同一个人」的空壳（旧模型迁移产物）。
  // 若把改名写到空壳上：投影纹丝不动、列表看起来没变，而且空壳随后会被同步回来的
  // 旧值覆盖 —— 用户视角就是「改了又变回去」。所以这类编辑一律重定向到投影，
  // 并把空壳隐藏。带用药数据的本地档案是用户真实建的家人，绝不参与重定向。
  async function renameTarget(member) {
    if (!member || member.source !== 'local') return { kind: 'linked' };
    var idx;
    try { idx = await loadIndex(); } catch (e) { return { kind: 'local' }; }
    var linkedRows = (idx.linked || []).filter(function (r) { return r && !r.ownerDeleted; });
    if (!linkedRows.length) return { kind: 'local' };

    var usedPids = {};
    try {
      (await MD.data.meds.list() || []).forEach(function (m) { if (m && m.profileId) usedPids[m.profileId] = true; });
    } catch (e) { /* 读不到用药数据就按空壳处理（与列表去重保持一致） */ }
    if (usedPids[member.id]) return { kind: 'local' };

    for (var i = 0; i < linkedRows.length; i++) {
      if (samePerson(member, linkedRows[i])) return { kind: 'linked', row: linkedRows[i] };
    }
    // 名字也对不上时，只有「墓碑/legacyProfileId 明确指向 + 仅一条投影」才敢认领，
    // 避免多授权场景把改名写到别人家去。
    var shellIds = await legacyShellProfileIds();
    if (shellIds[String(member.id)] && linkedRows.length === 1) {
      return { kind: 'linked', row: linkedRows[0] };
    }
    return { kind: 'local' };
  }

  // 统一的成员保存入口：把「用户改了哪张卡片」翻译成「该写哪条记录」。
  // 返回实际写入目标，便于测试断言与提示文案。
  async function applyMemberEdit(editing, rec) {
    // rec 来自表单，正常带 id；缺失时回退到「正在编辑的这份」，
    // 否则会写成一条没有主键的新记录（实测：改名后原记录没动、凭空多一条）。
    rec = Object.assign({}, rec || {});
    if (!rec.id && editing && editing.id) rec.id = editing.id;
    if (!rec.id) throw new Error('档案标识异常');
    if (editing && editing.source === 'linked') {
      await saveLinkedProfileForm({ id: editing.id, name: rec.name, relation: rec.relation, note: rec.note });
      return 'linked';
    }
    var target = editing ? await renameTarget(editing) : null;
    if (target && target.kind === 'linked' && target.row) {
      await saveLinkedProfileForm({
        id: target.row.id, name: rec.name, relation: rec.relation, note: rec.note
      });
      // 空壳留着会以旧名字继续出现在列表里（改名后姓名匹配失效），顺手隐藏。
      if (editing && editing.source === 'local') await hideLocalShell(editing.id);
      return 'linked-via-shell';
    }
    await saveProfileFromForm(rec);
    return 'local';
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
          text: '这是尚未导入完成的关联档案：下次打开会自动转成本账号的独立副本，'
            + '之后你改任何信息都不会影响对方。',
          style: { fontSize: '12px', margin: '0 0 10px' }
        }));
      }
      memberForm(host, editing, async function (rec) {
        if (!rec) { S.view = 'list'; S.memberId = null; paint(); return; }
        try {
          var where = await applyMemberEdit(editing, rec);
          S.view = 'list'; S.memberId = null;
          toast(where === 'linked-via-shell'
            ? '已更新关联档案的显示名称（本账号里的重复空壳已隐藏）'
            : '已保存');
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
      })
    ]));
    // 「刷新档案」已移除：关联改为一次性导入，导入后数据即独立，
    // 不再需要（也没有）与对方保持一致的动作。存量投影在下次打开时自动转换。

    if (!members.length) {
      host.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('home-heart'),
        el('strong', { text: '还没有家庭档案' }),
        el('p', { class: 'muted', text: '档案与「用药提醒」共用同一份名单，在这里或那里添加都可以。关联对方账号后，对方的全部家庭档案会同步到这里。' })
      ]));
      return;
    }

    // 诊断入口放最底部、小字：它是排障工具，不该出现在主流程里
    var diagFoot = el('div', { class: 'row', style: { justifyContent: 'center', marginTop: '4px' } }, [
      el('button', {
        class: 'btn btn-ghost', type: 'button',
        style: { gap: '6px', fontWeight: '400' },
        onClick: function () { openDiagnoseSheet(); }
      }, [
        MD.icon('warning'),
        el('span', { text: '存储诊断', style: { fontSize: '13px' } })
      ])
    ]);

    members.forEach(function (m) {
      var badge;
      if (m.source === 'linked') {
        // 关联投影只在「尚未完成导入」的瞬间出现（启动时会自动转成本地档案）
        badge = '关联档案 · 待转为本地';
      } else if (m.legacyGrantId) {
        badge = '已关联（旧方式）';
      } else {
        badge = '本账号';
      }

      // 整张卡可点进详情：三个文字按钮里「查看」是最常点的，把它变成整卡点击，
      // 右侧只留编辑/删除图标，卡片从三行宽降到一行宽，视觉上清爽很多。
      var actEdit = el('button', {
        class: 'fam-act', type: 'button', title: '编辑档案', 'aria-label': '编辑档案',
        onClick: function (ev) {
          if (ev && ev.stopPropagation) ev.stopPropagation();
          S.view = 'edit'; S.memberId = m.id; paint();
        }
      }, [MD.icon('edit')]);

      var actDel = el('button', {
        class: 'fam-act fam-act--danger', type: 'button', title: '删除档案', 'aria-label': '删除档案',
        onClick: async function (ev) {
          if (ev && ev.stopPropagation) ev.stopPropagation();
          if (await deleteMember(m)) { S.view = 'list'; S.memberId = null; }
          paint();
        }
      }, [MD.icon('trash')]);

      host.appendChild(el('div', {
        class: 'fam-card',
        style: { marginBottom: '10px' },
        onClick: function () { S.view = 'detail'; S.memberId = m.id; paint(); }
      }, [
        el('div', { class: 'fam-card__body' }, [
          el('div', { class: 'fam-card__name', text: m.name || '未命名' }),
          el('div', {
            class: 'fam-card__meta',
            text: (m.relation || '未填关系') + ' · ' + badge
          })
        ]),
        el('div', { class: 'fam-card__acts' }, [actEdit, actDel]),
        el('div', { class: 'fam-card__chev' }, [MD.icon('chevron')])
      ]));
    });

    host.appendChild(diagFoot);
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
        // 新建投影时就把显示名冻结进别名行：从此刻起，对方的改名不会影响本账号的显示，
        // 本账号的改名也不会写回投影行（避免与同步流程争同一条记录）。
        try {
          await MD.data.family.save({
            id: aliasRowId(opid),
            aliasFor: opid,
            aliasName: p.name || '未命名',
            aliasRelation: p.relation || '',
            aliasNote: p.note || '',
            updatedAt: nowMs()
          });
        } catch (e) { /* 失败则由 listMembers 退回投影值，不影响可用性 */ }
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
        text: '数字只能使用一次。需要重新导入（比如你删掉了导入的档案）时，请让对方重新生成一张码，'
          + '旧档案会被自动复活，不会重复。\n关联成功后，对方的全部家庭档案会**复制**到你的「家庭档案」'
          + '（含药物清单、用药方案与服药记录），之后双方各自独立：你改你的、对方改对方的，'
          + '互不影响。导入完成后关联关系会自动断开，需要更新时再让对方生成一张新码。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '8 位关联码' }), codeIn]),
        el('button', {
          class: 'btn btn-primary', type: 'button', text: '关联并导入',
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

              // 一次性把对方全部记录拉下来，复制成本账号的独立档案（不再建立实时镜像）。
              var recs = await pullAll(grantId);
              var ents = recordsToEntities(recs);
              var ownerProfiles = (ents.profiles || []).filter(function (p) {
                return p && p.deleted !== true && !p.archived;
              });
              var stat = await importEntitiesToLocal(ents, { grantId: grantId });

              // 导入完成即断开：删掉授权与密钥，此后没有任何跨账号通路，
              // 本地怎么改都不会影响对方，对方改什么也不会覆盖你。
              try { await dropGrant(grantId); } catch (e) { /* 忽略：数据已落地 */ }

              if (!ownerProfiles.length) {
                S.msg = '已导入完成，但对方还没有创建家庭档案（在「用药提醒 → 家庭档案」里建一个后，'
                  + '再让对方生成一张新关联码导入即可）。';
              } else {
                S.msg = '已导入对方的 ' + stat.profiles + ' 个家庭档案（'
                  + ownerProfiles.map(function (p) { return p.name || '未命名'; }).join('、')
                  + '），含 ' + stat.meds + ' 种药物、' + stat.schedules + ' 条用药方案、'
                  + stat.intakes + ' 条服药记录。'
                  + '这些现在是本账号的独立副本：你改你的，对方改对方的，互不影响。';
              }
              toast('已导入 ' + stat.profiles + ' 个档案、' + stat.meds + ' 种药物');
              S.tab = 'archive'; S.view = 'list'; S.memberId = null;
              paint();
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
    try { await migrateAliasRows(); } catch (e) { /* 同上 */ }
    // 存量「关联投影」一次性转成本账号独立副本（导入即断开）。
    // 放在启动时做，用户不需要任何操作；离线则保留现状，下次启动再试。
    try { await migrateLinkedToImported(); } catch (e) { /* 同上 */ }
  }

  function render(view) {
    pageRoot = view;
    boot().then(function () { paint(); }).catch(function () { paint(); });
  }

  // 恢复一条被删除的药物（连同它的用药方案）。
  //
  // 墓碑记录本身仍带着全部字段，只是 deleted=true。恢复 = 把标记翻回去，
  // 并把 ver 抬到高位，确保这次恢复能压过任何残留的旧删除副本。
  async function restoreMed(id) {
    if (!id) return false;
    var rec = null;
    try { rec = await MD.db.get('medication', id); } catch (e) { rec = null; }
    if (!rec) return false;
    await MD.data.meds.save(Object.assign({}, rec, {
      deleted: false,
      ver: (Number(rec.ver) || 0) + 500,
      updatedAt: nowMs()
    }));
    try {
      var scheds = await MD.db.list('schedule');
      for (var i = 0; i < scheds.length; i++) {
        var sc = scheds[i];
        if (!sc || sc.deleted !== true || sc.medicationId !== id) continue;
        await MD.data.schedules.save(Object.assign({}, sc, {
          deleted: false,
          ver: (Number(sc.ver) || 0) + 500,
          updatedAt: nowMs()
        }));
      }
    } catch (e) { /* 方案恢复失败不影响药物本身 */ }
    return true;
  }

  /* ------------------------------------------------------------------
   * 未归类数据（孤儿）
   *
   * 【怎么产生的】
   *   删除档案只删档案本身，不会连带删除药物（这是有意的：删人 ≠ 删药）。
   *   于是这些药还指向一个已删除/已不存在的 profileId —— 在「全部」里能看到，
   *   在每个档案下都看不到，用户视角就是"药都在，但哪个档案里都没有"。
   *
   *   另一种来源：对方删掉档案重建，id 变了，重新导入会生成新的 imp-<新id>，
   *   旧的 imp-<旧id> 下那些药就成了孤儿。
   *
   * 【修法】
   *   1) 恢复：把被删的档案翻回来（药还有，说明这个人不该消失）；
   *   2) 归位：把所有孤儿药/方案/记录整体挂到一个现存档案上。
   *   两种都由用户明确点选，不做静默猜测。
   * ------------------------------------------------------------------ */

  async function orphanReport() {
    var profiles = [], meds = [], scheds = [], intakes = [];
    try { profiles = (await MD.data.profiles.list()) || []; } catch (e) { /* 读不到按空处理 */ }
    try { meds = (await MD.data.meds.list()) || []; } catch (e) { /* 同上 */ }
    try { scheds = (await MD.data.schedules.list()) || []; } catch (e) { /* 同上 */ }
    try { intakes = (await MD.data.intakes.list()) || []; } catch (e) { /* 同上 */ }

    var live = {};
    var liveList = [];
    profiles.forEach(function (pr) {
      if (!pr || pr.deleted === true || pr.archived) return;
      live[String(pr.id)] = true;
      liveList.push(pr);
    });

    function isOrphan(rec) {
      if (!rec || rec.deleted === true) return false;
      var pid = String(rec.profileId || '');
      return !pid || !live[pid];
    }

    var orphanMeds = meds.filter(isOrphan);
    var byPid = {};
    orphanMeds.forEach(function (m) {
      var pid = String(m.profileId || '');
      (byPid[pid] = byPid[pid] || []).push(m);
    });

    return {
      meds: orphanMeds,
      schedules: scheds.filter(isOrphan),
      intakes: intakes.filter(isOrphan),
      byPid: byPid,
      liveProfiles: liveList
    };
  }

  // 恢复：为每一组孤儿的原 profileId 把档案翻回来（有墓碑就复活，没有就新建占位）。
  async function reviveOrphanProfiles() {
    var rep = await orphanReport();
    var pids = Object.keys(rep.byPid);
    var n = 0;
    for (var i = 0; i < pids.length; i++) {
      var pid = pids[i];
      if (!pid) continue;   // 连 profileId 都没有的记录无法按档案恢复，只能走"归位"
      var exist = null;
      try { exist = await MD.data.profiles.get(pid); } catch (e) { exist = null; }
      if (exist && exist.deleted !== true) continue;
      try {
        if (exist) {
          await MD.data.profiles.save(Object.assign({}, exist, {
            deleted: false,
            ver: (Number(exist.ver) || 0) + 500,
            updatedAt: nowMs()
          }));
        } else {
          await MD.data.profiles.save({
            id: pid,
            name: '已恢复档案',
            relation: '',
            color: '#4f8cff',
            archived: false,
            restoredAt: nowMs(),
            createdAt: nowMs(),
            updatedAt: nowMs()
          });
        }
        n++;
      } catch (e) { /* 单条失败不阻断其余 */ }
    }
    return n;
  }

  // 归位：把所有孤儿（药/方案/服药记录）整体改挂到 targetProfileId。
  async function moveOrphansTo(targetProfileId) {
    if (!targetProfileId) return 0;
    var rep = await orphanReport();
    var n = 0;
    var i;
    for (i = 0; i < rep.meds.length; i++) {
      try {
        await MD.data.meds.save(Object.assign({}, rep.meds[i], {
          profileId: targetProfileId,
          ver: (Number(rep.meds[i].ver) || 0) + 1,
          updatedAt: nowMs()
        }));
        n++;
      } catch (e) { /* 继续 */ }
    }
    for (i = 0; i < rep.schedules.length; i++) {
      try {
        await MD.data.schedules.save(Object.assign({}, rep.schedules[i], {
          profileId: targetProfileId,
          ver: (Number(rep.schedules[i].ver) || 0) + 1,
          updatedAt: nowMs()
        }));
      } catch (e) { /* 继续 */ }
    }
    for (i = 0; i < rep.intakes.length; i++) {
      try {
        await MD.data.intakes.save(Object.assign({}, rep.intakes[i], {
          profileId: targetProfileId,
          ver: (Number(rep.intakes[i].ver) || 0) + 1,
          updatedAt: nowMs()
        }));
      } catch (e) { /* 继续 */ }
    }
    return n;
  }

  MD.family = {
    // 供测试与「刷新档案」按钮使用
    buildSnapshot: buildSnapshot,
    syncLinkedProfiles: syncLinkedProfiles,
    refreshAllLinked: refreshAllLinked,
    migrateLegacyGrants: migrateLegacyGrants,
    storageReport: storageReport,
    purgeMember: purgeMember,
    purgeTombstones: purgeTombstones,
    purgeOrphans: purgeOrphans,
    migrateLegacyMembers: migrateLegacyMembers,
    listMembers: listMembers,
    dedupeLocalProfiles: dedupeLocalProfiles,
    // 未归类数据（档案被删/重建后遗留的药）：检测 + 两种修复方式
    orphanReport: orphanReport,
    reviveOrphanProfiles: reviveOrphanProfiles,
    moveOrphansTo: moveOrphansTo,
    // 供测试：改名目标的判定与统一保存入口
    renameTarget: renameTarget,
    applyMemberEdit: applyMemberEdit,
    // 供测试：别名行迁移 / 导入即独立的转换 / 药物恢复
    restoreMed: restoreMed,
    medReport: medReport,
    migrateAliasRows: migrateAliasRows,
    migrateLinkedToImported: migrateLinkedToImported,
    importEntitiesToLocal: importEntitiesToLocal,
    aliasRowId: aliasRowId,
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

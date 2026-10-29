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

  // 关联信息表（family）的读取：以 profileId 指向档案。
  // 旧结构（迁移前）用档案 id 直接当记录 id，这里一并兼容，避免迁移未跑完时丢数据。
  async function listLinks() {
    var rows = [];
    try { rows = await MD.data.family.list(); } catch (e) { return {}; }
    var map = {};
    (rows || []).forEach(function (r) {
      if (!r || r.deleted === true) return;
      var pid = r.profileId || r.id;
      if (pid) map[pid] = r;
    });
    return map;
  }

  // 档案列表 = profile（人） + family（对这个人的授权信息）。
  //
  // 【为什么改成读 profile】
  // 用药提醒和家庭健康原本各有一张档案表（profile / family），同一个"母亲"
  // 要在两处各建一遍，改名字要改两遍，数据还会分裂。现在档案主数据只有一份，
  // 家庭健康只是"在档案上多挂了一层关联信息"，两个模块看到的是同一批人。
  async function listMembers() {
    var profiles = [], links = {};
    try { profiles = await MD.data.profiles.list(); } catch (e) { return []; }
    links = await listLinks();

    return (profiles || []).filter(function (p) {
      return p && p.deleted !== true && !p.archived;
    }).map(function (p) {
      var link = links[p.id] || {};
      return {
        id: p.id,
        name: p.name || '未命名',
        relation: p.relation || '',
        note: p.note || '',
        color: p.color || '',
        birthDate: p.birthDate || '',
        createdAt: p.createdAt || 0,
        updatedAt: p.updatedAt || 0,
        // 关联信息挂在同一个对象上，UI 层不需要知道它存在另一张表
        grantId: link.grantId || '',
        ownerId: link.ownerId || '',
        dekIv: link.dekIv,
        dekCt: link.dekCt,
        linkedAt: Number(link.linkedAt) || 0,
        snapshotAt: Number(link.snapshotAt) || Number(link.linkedAt) || 0
      };
    }).sort(function (a, b) { return (a.createdAt || 0) - (b.createdAt || 0); });
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
        if (old.grantId && !(await getLink(old.id))) {
          await saveLink(old.id, {
            grantId: old.grantId,
            ownerId: old.ownerId || '',
            dekIv: old.dekIv,
            dekCt: old.dekCt,
            linkedAt: old.linkedAt || nowMs(),
            snapshotAt: old.snapshotAt || 0
          });
        }
        await MD.data.family.remove(old.id);
        changed++;
      } catch (e) {
        // 单条失败不阻断其余档案，下次进入页面会重试
      }
    }
    return changed;
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

    var meds = [], intakes = [];
    try { meds = await MD.data.meds.list(); } catch (e) { meds = []; }
    try { intakes = await MD.data.intakes.list(); } catch (e) { intakes = []; }

    var medMap = {};
    (meds || []).forEach(function (m) { if (m && m.id) medMap[m.id] = m; });

    var medList = (meds || []).filter(function (m) { return m && m.deleted !== true; })
      .map(function (m) {
        return {
          name: m.name || '未命名药物',
          dosage: m.dosage || m.doseText || m.dose || ''
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
          text: (member.relation || '')
            + (data ? (' · ' + (data.realtime ? '实时' : '快照') + ' ' + fmtDateTime(data.exportedAt)) : '')
        })
      ])
    ]));

    if (member.note) {
      host.appendChild(el('p', { class: 'muted', text: member.note, style: { margin: '0 0 12px', fontSize: '13px' } }));
    }

    if (!data) {
      host.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('home-heart'),
        el('strong', { text: '尚未关联数据' }),
        el('p', {
          class: 'muted',
          text: '请到「家庭关联」页输入对方给的 8 位关联码，即可查看其药物清单、用药历史与依从性。'
        }),
        el('div', { class: 'row', style: { gap: '10px', marginTop: '12px' } }, [
          el('button', {
            class: 'btn btn-primary', type: 'button', text: '去关联',
            onClick: function () { S.tab = 'link'; S.view = 'list'; paint(); }
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

    // 1. 依从性统计
    var adh = data.adherence || {};
    host.appendChild(sectionCard('依从性统计（近 ' + (data.days || 90) + ' 天）', [
      kvRow('依从率', pct(adh.rate)),
      kvRow('应服次数', String(adh.total == null ? DASH : adh.total)),
      kvRow('已服用', String(adh.taken == null ? DASH : adh.taken)),
      kvRow('已跳过', String(adh.skipped == null ? DASH : adh.skipped)),
      kvRow('漏服', String(adh.missed == null ? DASH : adh.missed)),
      kvRow('连续达标', (adh.streakDays == null ? DASH : adh.streakDays) + ' 天')
    ]));

    // 2. 药物清单：关联档案可直接编辑，改动写回数据所有方的账号
    host.appendChild(sharedMedsCard(member, data));

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
      message: '确定删除「' + (member.name || '未命名') + '」？'
        + '只会删除本账号里的这个档案条目；对方的账号、药物清单与授权都不受影响。',
      confirmText: '删除',
      danger: true
    });
    if (!ok) return false;

    try {
      if (member.grantId && MD.familyLink && typeof MD.familyLink.forgetDek === 'function') {
        MD.familyLink.forgetDek(member.grantId);
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

  // 药物清单卡片。canEdit 的条件是「这个档案挂着有效授权」——
  // 只有关联来的档案才有对方账号可写；本地自建、未关联的档案没有可写对象。
  function sharedMedsCard(member, data) {
    var canEdit = Boolean(member && member.grantId && MD.familyLink
      && typeof MD.familyLink.pushRecords === 'function');
    var rows = [];

    (data.meds || []).forEach(function (m) {
      rows.push(el('div', {
        class: 'row',
        style: { justifyContent: 'space-between', alignItems: 'center', gap: '10px' }
      }, [
        el('div', { style: { display: 'flex', flexDirection: 'column', gap: '2px', minWidth: '0' } }, [
          el('div', { text: m.name, style: { fontSize: '13px' } }),
          el('span', { class: 'muted', text: m.dosage || DASH, style: { fontSize: '12px' } })
        ]),
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
        class: 'muted', text: '对方还没有录入药物',
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

    var nameIn = el('input', {
      class: 'input', type: 'text', value: med ? med.name : '',
      placeholder: '例如：二甲双胍'
    });
    var doseIn = el('input', {
      class: 'input', type: 'text', value: med ? med.dosage : '',
      placeholder: '例如：0.5 g / 1 片'
    });

    MD.ui.sheet({
      title: med ? '编辑药物' : '添加药物',
      body: function (host) {
        host.appendChild(el('div', { class: 'stack', style: { gap: '10px' } }, [
          el('div', { class: 'field' }, [el('span', { class: 'label', text: '药物名称' }), nameIn]),
          el('div', { class: 'field' }, [el('span', { class: 'label', text: '剂量' }), doseIn])
        ]));
        host.appendChild(el('p', {
          class: 'muted',
          text: '改动会写进对方的账号，对方下次同步即可看到；双方同时修改同一条时会提示以最新值为准。',
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
              dosage: String(doseIn.value || '').trim()
            });
            return true;
          }
        }
      ]
    });
  }

  // 归属成员（profileId）：优先沿用被编辑那条的原值；新建时沿用该档案下
  // 已有药物的 profileId。都取不到就留空 —— 由数据所有方在自己的账号里
  // 调整归属，比在这里瞎猜一个 id 安全（猜错会把药挂到别人名下）。
  function pickProfileId(med, data) {
    if (med && med.profileId) return med.profileId;
    var list = (data && data.meds) || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i].profileId) return list[i].profileId;
    }
    return '';
  }

  async function submitSharedMed(member, data, med, values) {
    try {
      var id = med ? med.id : uid();
      var res = await MD.familyLink.pushRecords(member.grantId, [{
        id: id,
        ver: med ? (Number(med.ver) || 0) + 1 : 1,
        deleted: false,
        payload: {
          id: id,
          profileId: pickProfileId(med, data),
          name: values.name,
          dosage: values.dosage,
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

  async function removeSharedMed(member, data, med) {
    var ok = await MD.ui.confirm({
      title: '删除药物',
      message: '确定从「' + (member.name || '未命名') + '」的药物清单删除「' + med.name + '」？'
        + '这条药物会从对方账号里一并删除，双方都生效。',
      confirmText: '删除',
      danger: true
    });
    if (!ok) return;

    try {
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

  async function renderArchive(host) {
    clear(host);

    if (S.view === 'new' || S.view === 'edit') {
      var editing = S.view === 'edit' ? await getMember(S.memberId) : null;
      host.appendChild(el('div', { style: { fontWeight: '700', marginBottom: '12px' } }, [
        el('span', { text: editing ? '编辑档案' : '新增档案' })
      ]));
      memberForm(host, editing, async function (rec) {
        if (!rec) { S.view = 'list'; S.memberId = null; paint(); return; }
        try {
          await saveProfileFromForm(rec);
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

    host.appendChild(el('div', { class: 'row', style: { gap: '10px', marginBottom: '14px' } }, [
      el('button', {
        class: 'btn btn-primary', type: 'button', text: '新增档案',
        onClick: function () { S.view = 'new'; S.memberId = null; paint(); }
      }),
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '去关联',
        onClick: function () { S.tab = 'link'; paint(); }
      })
    ]));

    if (!members.length) {
      host.appendChild(el('div', { class: 'card empty' }, [
        MD.icon('home-heart'),
        el('strong', { text: '还没有家庭档案' }),
        el('p', { class: 'muted', text: '档案与「用药提醒」共用同一份名单，在这里或那里添加都可以。关联对方账号后即可共同维护药物清单。' })
      ]));
      return;
    }

    members.forEach(function (m) {
      var linked = m.snapshotAt ? ('已关联 · ' + fmtDate(m.snapshotAt)) : '未关联数据';
      host.appendChild(el('div', { class: 'card', style: { marginBottom: '10px' } }, [
        el('div', { class: 'row', style: { justifyContent: 'space-between', alignItems: 'center', gap: '12px' } }, [
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '3px', minWidth: '0' } }, [
            el('div', { text: m.name || '未命名', style: { fontWeight: '700' } }),
            el('span', { class: 'muted', text: (m.relation || '') + ' · ' + linked, style: { fontSize: '12px' } })
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

  /* ============================ 7. 视图：家庭关联 ============================ */

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
  async function sealOwnerDek(profileId, ownerDek) {
    var myDek = MD.api && MD.api.getDEK ? MD.api.getDEK() : null;
    if (!myDek) throw new Error('账号未解锁，请重新登录');
    var body = await MD.crypto.encrypt(myDek, JSON.stringify(Array.prototype.slice.call(ownerDek)));
    return { dekIv: body.ivB64, dekCt: body.ctB64 };
  }

  async function openOwnerDek(member) {
    if (!member || !member.dekIv || !member.dekCt) return null;
    var myDek = MD.api && MD.api.getDEK ? MD.api.getDEK() : null;
    if (!myDek) return null;
    try {
      var plain = await MD.crypto.decrypt(myDek, member.dekIv, member.dekCt);
      return new Uint8Array(JSON.parse(plain));
    } catch (e) {
      // 换了账号或本地材料损坏时，降级为「需要重新关联」，不要把页面卡死
      return null;
    }
  }

  // 把对方的 sync 记录还原成详情页认识的结构（与快照 data 同形）。
  // 只挑 medication 与 intake 两张表：其余表（schedule/profile）不进入展示，
  // 避免越界暴露与该页面承诺无关的内容。
  function recordsToSnapshot(records, days) {
    var now = nowMs();
    var span = Math.max(1, Math.min(365, Number(days) || 90));
    var fromMs = now - span * DAY_MS;

    var meds = [], intakes = [], medMap = {};
    (records || []).forEach(function (r) {
      if (!r || r.deleted || !r.data) return;
      if (r.table === 'medication') {
        medMap[r.id] = r.data;
        // 必须把记录外层的 id/ver 一起带出来：没有它们就无法构造一次合法的
        // 写回（id 决定改哪一条，ver 决定冲突裁决），结果只能是只读展示。
        meds.push({ id: r.id, ver: Number(r.ver) || 0, data: r.data });
      } else if (r.table === 'intake') {
        intakes.push(r.data);
      }
    });

    var medList = meds.filter(function (x) { return x.data && x.data.deleted !== true; })
      .map(function (x) {
        return {
          id: x.id,
          ver: x.ver,
          profileId: x.data.profileId || '',
          name: x.data.name || '未命名药物',
          dosage: x.data.dosage || x.data.doseText || x.data.dose || ''
        };
      });

    // 先按时间窗筛一次，history 与 adherence 共用。
    // 注意：MD.sched.adherence 内部不会自己剔除越界记录，若把全集传进去，
    // 200 天前的服药记录也会被算进「近 90 天」，依从率随之虚高。
    var windowIntakes = intakes.filter(function (it) {
      if (!it || it.deleted === true) return false;
      var t = Number(it.scheduledFor || 0);
      return t >= fromMs && t <= now;
    });

    var history = windowIntakes.map(function (it) {
      var med = medMap[it.medicationId];
      return {
        t: Number(it.scheduledFor || 0),
        s: String(it.status || 'pending'),
        n: med ? (med.name || '未命名药物') : '未知药物'
      };
    }).sort(function (a, b) { return b.t - a.t; }).slice(0, 400);

    var adherence = { total: 0, taken: 0, skipped: 0, missed: 0, rate: null, streakDays: 0 };
    if (MD.sched && typeof MD.sched.adherence === 'function') {
      adherence = MD.sched.adherence({ intakes: windowIntakes, fromMs: fromMs, toMs: now });
    }

    return {
      v: 2, realtime: true, exportedAt: now, days: span,
      meds: medList, history: history, adherence: adherence
    };
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

  // 详情页取数：实时优先，失败回退上次快照（离线时不至于白屏）
  async function loadMemberData(member) {
    if (!member) return null;
    if (member.grantId && MD.familyLink) {
      var ownerDek = await openOwnerDek(member);
      if (ownerDek) {
        MD.familyLink.adoptDek(member.grantId, ownerDek);
        try {
          var recs = await pullAll(member.grantId);
          return recordsToSnapshot(recs, 90);
        } catch (e) {
          S.err = '实时数据拉取失败，显示的是上次快照：' + (e && e.message ? e.message : '未知错误');
        }
      }
    }
    return await getSnapshot(member.id);
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
    var codeIn = el('input', {
      class: 'input', type: 'text', inputmode: 'numeric', maxlength: '9',
      placeholder: '输入 8 位数字', autocomplete: 'off'
    });
    var targetSelect = el('select', { class: 'select' });

    // 「新增档案」内联表单：只有下拉选中 __new__ 时才显示。
    // 关联本身要求"先有档案才能挂数据"，强迫用户先跑去档案页建一个再回来
    // 是很别扭的路径；既然他人已经在手边（手里有码），就该一步做完。
    var newNameIn = el('input', {
      class: 'input', type: 'text', placeholder: '例如：老妈', autocomplete: 'off'
    });
    var newRelSel = el('select', { class: 'select' }, RELATIONS.map(function (r) {
      var opt = el('option', { value: r, text: r });
      if (r === '母亲') opt.selected = true;
      return opt;
    }));
    var newBox = el('div', { class: 'stack', style: { gap: '10px', display: 'none' } }, [
      el('div', { class: 'field' }, [el('span', { class: 'label', text: '姓名' }), newNameIn]),
      el('div', { class: 'field' }, [el('span', { class: 'label', text: '关系' }), newRelSel])
    ]);

    function syncNewBox() {
      newBox.style.display = targetSelect.value === '__new__' ? '' : 'none';
    }
    targetSelect.addEventListener('change', syncNewBox);

    listMembers().then(function (members) {
      clear(targetSelect);
      members.forEach(function (m) {
        targetSelect.appendChild(el('option', { value: m.id, text: m.name + '（' + (m.relation || '') + '）' }));
      });
      targetSelect.appendChild(el('option', { value: '__new__', text: '＋ 新增档案…' }));
      // 没有档案时直接落到「新增」，少一次无意义的点击
      targetSelect.value = members.length ? members[0].id : '__new__';
      syncNewBox();
    });

    host.appendChild(el('div', { class: 'card', style: { marginBottom: '14px' } }, [
      el('div', { style: { fontWeight: '700', marginBottom: '6px' } }, [el('span', { text: '② 我是查看方：输入关联码' })]),
      el('p', {
        class: 'muted',
        text: '数字只能使用一次。关联成功后，对方后续的用药变化会实时出现在这里。',
        style: { fontSize: '13px', margin: '0 0 10px' }
      }),
      el('div', { class: 'stack', style: { gap: '10px' } }, [
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '8 位关联码' }), codeIn]),
        el('div', { class: 'field' }, [el('span', { class: 'label', text: '保存到哪个档案' }), targetSelect]),
        newBox,
        el('button', {
          class: 'btn btn-primary', type: 'button', text: '关联并查看',
          onClick: async function () {
            S.err = ''; S.msg = '';
            try {
              if (!MD.familyLink) throw new Error('短码模块未加载，请刷新页面');
              var memberId = targetSelect.value;
              var member;

              if (memberId === '__new__') {
                var nm = String(newNameIn.value || '').trim();
                if (!nm) throw new Error('请填写新档案的姓名');
                var rec = {
                  id: uid(),
                  name: nm,
                  relation: newRelSel.value || '其他',
                  note: '',
                  color: '#4f8cff',
                  archived: false,
                  createdAt: nowMs(),
                  updatedAt: nowMs()
                };
                await saveProfileFromForm(rec);   // 档案与用药提醒共用同一张表
                memberId = rec.id;
                member = rec;
              } else {
                if (!memberId) throw new Error('请选择保存到哪个档案');
                member = await getMember(memberId);
                if (!member) throw new Error('目标档案不存在');
              }

              var res = await MD.familyLink.redeem(codeIn.value);
              var ownerDek = MD.familyLink.getDek(res.grantId);
              if (!ownerDek) throw new Error('密钥恢复失败，请让对方重新生成关联码');

              // 短码已作废，所以必须立刻用自己账号的 DEK 把对方 DEK 落盘，
              // 这是之后唯一的解锁途径。档案本身（姓名/关系）不动，
              // 只把关联信息写到 family 表。
              var sealed = await sealOwnerDek(member.id, ownerDek);
              await saveLink(member.id, {
                grantId: res.grantId,
                ownerId: res.ownerId || '',
                dekIv: sealed.dekIv,
                dekCt: sealed.dekCt,
                linkedAt: nowMs()
              });

              var recs = await pullAll(res.grantId);
              var snap = recordsToSnapshot(recs, 90);
              await putSnapshot(member.id, snap);   // 离线兜底
              await saveLink(member.id, { snapshotAt: nowMs() });

              toast('关联成功');
              S.tab = 'archive'; S.view = 'detail'; S.memberId = memberId;
              paint();
            } catch (e) {
              var reason = (e && e.message) ? e.message : '未知错误';
              if (reason.indexOf('短码无效') >= 0) {
                // 统一文案是为了不泄露「这个码是否存在」。但只给一句「无效」会让
                // 用户不知道下一步该做什么，这里补充可能原因与可执行动作。
                reason += '\n可能原因：① 已超过 24 小时未兑换；② 已被兑换过；'
                  + '③ 你与该账号此前已建立关联（同一对账号只允许一条有效授权）。'
                  + '\n下一步：让对方重新生成一张；若此前已关联，直接到「家庭档案」查看即可。';
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

  function render(view) {
    pageRoot = view;
    // 迁移放在渲染前：用户打开页面就会自动完成，不需要单独的操作入口。
    // 幂等，多设备各自跑也不会重复建档案（profile 复用旧档案 id）。
    migrateLegacyMembers().then(function () { paint(); }).catch(function () { paint(); });
  }

  MD.family = {
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

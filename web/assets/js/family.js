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
  var S = { tab: 'archive', view: 'list', memberId: null, msg: '', err: '', busy: false };

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

  async function listMembers() {
    var list = [];
    try { list = await MD.data.family.list(); } catch (e) { return []; }
    return (list || []).filter(function (m) { return m && m.deleted !== true; })
      .sort(function (a, b) { return (a.createdAt || 0) - (b.createdAt || 0); });
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

    var relationSelect = el('select', {
      class: 'select',
      onChange: function (e) { draft.relation = e.target.value; }
    }, RELATIONS.map(function (r) {
      var opt = el('option', { value: r, text: r });
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

    host.appendChild(el('div', { class: 'row', style: { gap: '10px', alignItems: 'center', marginBottom: '14px' } }, [
      el('button', {
        class: 'btn btn-ghost', type: 'button', text: '返回',
        onClick: function () { S.view = 'list'; S.memberId = null; paint(); }
      }),
      el('div', { style: { display: 'flex', flexDirection: 'column', gap: '2px', minWidth: '0' } }, [
        el('div', { text: member.name || '未命名', style: { fontSize: '17px', fontWeight: '700' } }),
        el('span', { class: 'muted', text: (member.relation || '') + (data ? (' · 数据截至 ' + fmtDate(data.exportedAt)) : '') })
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
          text: '请到「家庭关联」页粘贴对方提供的关联码与口令，即可查看其药物清单、用药历史与依从性。'
        }),
        el('button', {
          class: 'btn btn-primary', type: 'button', text: '去关联',
          onClick: function () { S.tab = 'link'; S.view = 'list'; paint(); }
        })
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

    // 2. 药物清单
    var medRows = (data.meds || []).map(function (m) {
      return kvRow(m.name, m.dosage || DASH);
    });
    host.appendChild(sectionCard('药物清单（' + (data.meds || []).length + '）', medRows));

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
      text: '数据来自对方主动分享的快照，非实时同步。需要最新数据请让对方重新生成关联码。',
      style: { fontSize: '12px', margin: '4px 0 0' }
    }));
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
          await MD.data.family.save(rec);
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
      var data = await getSnapshot(member.id);
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
        el('p', { class: 'muted', text: '家庭档案与账号相互独立，可自由添加家人姓名与关系，再通过关联码查看其用药情况。' })
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
            })
          ])
        ])
      ]));
    });
  }

  /* ============================ 7. 视图：家庭关联 ============================ */

  function renderLink(host) {
    clear(host);

    var codeBox = el('textarea', {
      class: 'input',
      placeholder: '生成后此处显示关联码，可复制发给家人',
      rows: '4',
      style: { width: '100%', fontSize: '12px', fontFamily: 'monospace', resize: 'vertical' }
    });

    var passOut = el('input', {
      class: 'input', type: 'text', placeholder: '设置一个至少 6 位的口令',
      autocomplete: 'off'
    });

    var daysIn = el('input', {
      class: 'input', type: 'number', value: '90', min: '7', max: '365', step: '1'
    });

    var msg = el('p', { class: 'muted', style: { minHeight: '18px', margin: '0', fontSize: '13px' } });
    if (S.err) { msg.textContent = S.err; msg.style.color = 'var(--danger)'; }
    else if (S.msg) { msg.textContent = S.msg; }

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
                codeBox.value = code;
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
              if (!codeBox.value) { toast('还没有生成关联码', 'error'); return; }
              try {
                if (navigator.clipboard && navigator.clipboard.writeText) {
                  navigator.clipboard.writeText(codeBox.value);
                  toast('已复制');
                } else {
                  codeBox.select();
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

    host.appendChild(msg);
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
        text: '家庭档案与账号相互独立；关联后可按快照查看家人的药物清单、用药历史与依从性。',
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
    paint();
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

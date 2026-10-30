/* ==========================================================================
 * ledger.js —— MedDesk「记账本」前端（B8）
 *
 * 依赖（只读调用，不修改其文件）：
 *   A6：MD.util / MD.ui / MD.router / MD.api / MD.toast / MD.haptic / MD.icon
 * 与 med.js 保持一致的写法：
 *   模板字符串 + innerHTML 渲染；事件挂在根节点上用 data-act 委托。
 *
 * 零外部依赖：图表全部为手写内联 SVG（CSP 禁止外部脚本，也不引入图表库）。
 * 金额口径：全部以「分」为单位参与计算与绘图，杜绝浮点误差（契约 §1）。
 *
 * 路由：id 'ledger'（标题「记账本」，order 105，排在「日程待办」110 之上）。
 * 本文件自己在加载期注册（defer 脚本先于 app.js 的 DOMContentLoaded 执行），
 * app.js 里的同名占位项已删除，保证全局只有一条 ledger 路由。
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};
  var ledger = MD.ledger = MD.ledger || {};
  var doc = global.document;

  /* ============================ 0. 常量 ============================ */

  var PAGE_SIZE = 20;                       // 账单列表每页条数
  var MAX_IMAGE_BYTES = 2 * 1024 * 1024;    // 单张凭证上限 2MB
  var IMAGE_EXT = ['jpg', 'jpeg', 'png', 'webp'];
  var TOP_N = 8;                            // 饼图最多展示分类数，其余归入「其他」

  var TYPE_TEXT = { income: '收入', expense: '支出' };
  var REIMB_TEXT = { none: '不涉及', pending: '待报销', done: '已报销' };
  var REIMB_TONE = { none: '', pending: 'warn', done: 'ok' };
  var RANGE_TEXT = { month: '本月', last: '上月', custom: '自定义' };

  /* ============================ 1. 样式注入（.ldg- 前缀） ============================ */

  var CSS = [
    '.ldg-wrap{display:flex;flex-direction:column;gap:var(--space,16px);min-width:0}',
    '.ldg-tabs{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:4px;padding:4px;border-radius:12px;background:var(--surface-2)}',
    '.ldg-tabs button{min-height:44px;padding:0 8px;border:0;border-radius:9px;background:transparent;color:var(--text-dim);font-weight:700;cursor:pointer;transition:background var(--dur,180ms),color var(--dur,180ms)}',
    '.ldg-tabs button.is-active{background:var(--surface);color:var(--brand);box-shadow:0 2px 10px rgba(20,55,44,.08)}',
    /* 汇总卡 */
    '.ldg-kpis{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:10px}',
    '@media(max-width:520px){.ldg-kpis{grid-template-columns:1fr}}',
    '.ldg-kpi{min-width:0;padding:12px;border:1px solid var(--border);border-radius:var(--radius,14px);background:var(--surface)}',
    '.ldg-kpi__label{font-size:12px;color:var(--text-dim)}',
    '.ldg-kpi__value{margin-top:4px;font-size:20px;font-weight:800;line-height:1.25;font-variant-numeric:tabular-nums;word-break:break-all}',
    '.ldg-kpi--in .ldg-kpi__value{color:var(--ok)}',
    '.ldg-kpi--out .ldg-kpi__value{color:var(--danger)}',
    '.ldg-kpi--neg{background:color-mix(in srgb,var(--danger) 10%,var(--surface));border-color:color-mix(in srgb,var(--danger) 35%,var(--border))}',
    '.ldg-kpi--neg .ldg-kpi__value{color:var(--danger)}',
    /* 图表 */
    '.ldg-charts{display:grid;grid-template-columns:minmax(0,1fr);gap:var(--space,16px)}',
    '@media(min-width:1024px){.ldg-charts{grid-template-columns:repeat(3,minmax(0,1fr))}}',
    '.ldg-panel{min-width:0;padding:12px;border:1px solid var(--border);border-radius:var(--radius-lg,16px);background:var(--surface);box-shadow:var(--shadow)}',
    '.ldg-panel__head{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:8px}',
    '.ldg-panel__title{flex:1 1 auto;min-width:0;font-size:14px;font-weight:800}',
    '.ldg-panel__meta{flex:0 0 auto;font-size:11px;color:var(--text-dim)}',
    '.ldg-chartbox{min-height:190px;display:flex;align-items:center;justify-content:center}',
    '.ldg-chart{display:block;width:100%;height:auto}',
    '.ldg-scroll{width:100%;overflow-x:auto;-webkit-overflow-scrolling:touch}',
    '.ldg-scroll>svg{width:100%;min-width:340px;height:auto}',
    '.ldg-legend{display:flex;flex-direction:column;gap:6px;margin-top:10px;font-size:12px}',
    '.ldg-legend__row{display:flex;align-items:center;gap:6px;min-width:0}',
    '.ldg-legend__sw{width:12px;height:12px;flex:0 0 auto;border-radius:3px}',
    '.ldg-legend__name{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.ldg-legend__val{flex:0 0 auto;font-variant-numeric:tabular-nums;color:var(--text-dim)}',
    '.ldg-placeholder{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:8px;min-height:190px;color:var(--text-dim);text-align:center;font-size:13px}',
    '.ldg-placeholder .icon{width:30px;height:30px;opacity:.6}',
    '.ldg-skeleton{width:100%;min-height:190px;border-radius:12px;background:var(--surface-2);animation:ldg-pulse 1.2s ease-in-out infinite}',
    '@keyframes ldg-pulse{0%,100%{opacity:1}50%{opacity:.5}}',
    /* 筛选 */
    '.ldg-filters{display:grid;grid-template-columns:minmax(0,1fr);gap:10px}',
    '@media(min-width:768px){.ldg-filters{grid-template-columns:repeat(2,minmax(0,1fr))}}',
    '@media(min-width:1024px){.ldg-filters{grid-template-columns:repeat(4,minmax(0,1fr))}}',
    '.ldg-frow{display:flex;flex-wrap:wrap;align-items:center;gap:8px;min-width:0}',
    '.ldg-flabel{font-size:12px;font-weight:700;color:var(--text-dim)}',
    '.ldg-chips{display:flex;flex-wrap:wrap;gap:8px}',
    ".ldg-chips .chip{min-height:44px;padding:0 12px}",
    '.ldg-date{display:grid;grid-template-columns:1fr 1fr;gap:8px;width:100%}',
    /* 账单列表 */
    '.ldg-rec{display:flex;gap:10px;align-items:flex-start;padding:10px 0;border-bottom:1px solid var(--border);min-width:0}',
    '.ldg-rec:last-child{border-bottom:0}',
    '.ldg-rec__body{flex:1 1 auto;min-width:0}',
    '.ldg-rec__title{font-size:14px;font-weight:700;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.ldg-rec__meta{display:flex;flex-wrap:wrap;gap:6px;margin-top:3px;font-size:12px;color:var(--text-dim)}',
    '.ldg-rec__amt{flex:0 0 auto;font-size:15px;font-weight:800;font-variant-numeric:tabular-nums}',
    '.ldg-amt--in{color:var(--ok)}',
    '.ldg-amt--out{color:var(--danger)}',
    '.ldg-rec__acts{display:flex;gap:2px;flex:0 0 auto}',
    /* 表单 */
    '.ldg-form{display:flex;flex-direction:column;gap:10px;max-height:60vh;overflow:auto;padding-right:2px}',
    '.ldg-catpick{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:6px}',
    '@media(min-width:560px){.ldg-catpick{grid-template-columns:repeat(4,minmax(0,1fr))}}',
    '.ldg-catpick .chip{min-height:44px;justify-content:center;text-align:center}',
    '.ldg-row2{display:grid;grid-template-columns:1fr 1fr;gap:8px}',
    '.ldg-att{display:flex;gap:8px;align-items:center;padding:6px;border:1px solid var(--border);border-radius:10px;background:var(--surface-2)}',
    '.ldg-att+.ldg-att{margin-top:6px}',
    '.ldg-att__thumb{width:56px;height:56px;flex:0 0 auto;padding:0;overflow:hidden;border:1px solid var(--border);border-radius:8px;background:var(--surface-2);cursor:zoom-in}',
    '.ldg-att__thumb img{width:100%;height:100%;object-fit:cover;display:block}',
    '.ldg-strip{display:flex;flex-wrap:wrap;gap:8px}',
    '.ldg-check{display:flex;align-items:center;gap:8px;min-height:44px}',
    '.ldg-check input{width:22px;height:22px;flex:0 0 auto}',
    '.ldg-sumline{display:flex;flex-wrap:wrap;gap:14px;padding:10px 0;font-size:13px}',
    '.ldg-sumline b{font-variant-numeric:tabular-nums}',
    '.ldg-note{display:flex;gap:8px;padding:10px;border:1px dashed var(--border);border-radius:12px;background:var(--surface-2);font-size:12px;color:var(--text-dim);line-height:1.6}'
  ].join('\n');

  function injectStyle() {
    if (!doc || doc.getElementById('ldg-b8-style')) return;
    var st = doc.createElement('style');
    st.id = 'ldg-b8-style';
    st.textContent = CSS;
    doc.head.appendChild(st);
  }

  /* ============================ 2. 工具 ============================ */

  function esc(s) {
    var v = s == null ? '' : String(s);
    if (MD.util && typeof MD.util.escapeHtml === 'function') return MD.util.escapeHtml(v);
    return v.replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function toast(msg, kind) { if (MD.toast && MD.toast.show) MD.toast.show(msg, kind || 'info'); }
  function tap() { if (MD.haptic && MD.haptic.tap) MD.haptic.tap(); }
  function ding() { if (MD.haptic && MD.haptic.success) MD.haptic.success(); }
  function pad2(n) { return (Number(n) < 10 ? '0' : '') + Number(n); }
  function msgOf(e) { return e && e.message ? String(e.message) : '请求失败'; }

  /** 分 → 带千分位的字符串（如 123456 → "1,234.56"），全程整数运算，不用浮点。 */
  function fmtCents(cents) {
    var n = Number(cents) || 0;
    var neg = n < 0;
    var abs = Math.abs(n);
    var yuan = Math.floor(abs / 100);
    var frac = abs % 100;
    var head = String(yuan).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    return (neg ? '-' : '') + head + '.' + (frac < 10 ? '0' + frac : frac);
  }
  function money(cents) { return '¥' + fmtCents(cents); }

  /** 坐标轴用的紧凑金额：万以下直接显示，以上折算成「万」。 */
  function compact(cents) {
    var n = Number(cents) || 0;
    var yuan = n / 100;
    if (Math.abs(yuan) >= 10000) return (yuan / 10000).toFixed(1).replace(/\.0$/, '') + '万';
    if (Math.abs(yuan) >= 1000) return String(Math.round(yuan));
    return String(Math.round(yuan * 100) / 100);
  }

  /** 前端金额校验：必须是正数且最多两位小数，返回分的整数。 */
  function parseAmount(input) {
    var raw = String(input == null ? '' : input).trim().replace(/[,，\s¥￥]/g, '');
    if (!raw) return { ok: false, msg: '请填写金额' };
    if (!/^\d{1,12}(\.\d{1,2})?$/.test(raw)) return { ok: false, msg: '金额只能为数字，最多两位小数' };
    var parts = raw.split('.');
    var yuan = parseInt(parts[0], 10);
    var frac = (parts[1] || '') + '00';
    var cents = yuan * 100 + parseInt(frac.slice(0, 2), 10);
    if (!(cents > 0)) return { ok: false, msg: '金额必须大于 0' };
    return { ok: true, cents: cents };
  }

  /** 兼容后端两种金额形态：{cents} 或 {value,cents} 或纯 "12.34"。 */
  function centsOf(moneyish) {
    if (moneyish == null) return 0;
    if (typeof moneyish === 'number') return Math.round(moneyish);
    if (typeof moneyish === 'string') {
      var p = parseAmount(moneyish);
      return p.ok ? p.cents : 0;
    }
    if (typeof moneyish.cents !== 'undefined' && moneyish.cents !== null) return Number(moneyish.cents) || 0;
    if (typeof moneyish.value !== 'undefined') {
      var q = parseAmount(moneyish.value);
      return q.ok ? q.cents : 0;
    }
    if (typeof moneyish.amountCents !== 'undefined') return Number(moneyish.amountCents) || 0;
    if (typeof moneyish.amount !== 'undefined') { var r = parseAmount(moneyish.amount); return r.ok ? r.cents : 0; }
    return 0;
  }
  function sumCents(list) {
    return (list || []).reduce(function (acc, item) { return acc + centsOf(item); }, 0);
  }
  /** 归一化分类金额列表为 [{id,name,cents}] 并按金额倒序。 */
  function normalizeItems(list) {
    return (list || []).map(function (item) {
      return {
        id: item && (item.categoryId || item.id) ? String(item.categoryId || item.id) : '',
        name: item && item.name ? String(item.name) : '未分类',
        cents: centsOf(item)
      };
    }).filter(function (item) { return item.cents > 0; })
      .sort(function (a, b) { return b.cents - a.cents; });
  }
  /** 只保留前 n 项，其余合并为「其他」，避免饼图被零碎分类淹没。 */
  function topN(list, n) {
    if (list.length <= n) return list;
    var head = list.slice(0, n);
    var rest = list.slice(n);
    head.push({ id: '__other__', name: '其他', cents: sumCents(rest) });
    return head;
  }

  /* ---------- 日期 ---------- */
  function dateStr(d) { return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()); }
  function monthKeyOf(ms) { var d = new Date(ms == null ? Date.now() : ms); return d.getFullYear() + '-' + pad2(d.getMonth() + 1); }
  function monthRange(key) {
    var y = parseInt(String(key).slice(0, 4), 10);
    var m = parseInt(String(key).slice(5, 7), 10) - 1;
    var first = new Date(y, m, 1);
    var last = new Date(y, m + 1, 0);
    return { from: dateStr(first), to: dateStr(last) };
  }
  function shiftMonthKey(key, delta) {
    var y = parseInt(String(key).slice(0, 4), 10);
    var m = parseInt(String(key).slice(5, 7), 10) - 1 + (delta || 0);
    var d = new Date(y, m, 1);
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  }
  /** datetime-local 控件值 ↔ ISO 字符串 */
  function localInputFrom(iso) {
    var d = iso ? new Date(iso) : new Date();
    if (isNaN(d.getTime())) d = new Date();
    return dateStr(d) + 'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }
  function isoFromLocal(value) {
    if (!value) return '';
    var d = new Date(value);
    return isNaN(d.getTime()) ? '' : d.toISOString();
  }
  function fmtOccurred(rec) {
    var src = rec.occurredOn || rec.transactionAt || '';
    if (!src) return '';
    if (/^\d{4}-\d{2}-\d{2}$/.test(String(src))) {
      var p = String(src).split('-');
      return Number(p[1]) + '月' + Number(p[2]) + '日';
    }
    var d = new Date(src);
    if (isNaN(d.getTime())) return String(src);
    return (d.getMonth() + 1) + '月' + d.getDate() + '日 ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  /* ============================ 3. 请求封装 ============================ */

  function enc(v) { return encodeURIComponent(v == null ? '' : v); }

  function qs(params) {
    var parts = [];
    Object.keys(params || {}).forEach(function (key) {
      var value = params[key];
      if (value == null || value === '') return;
      parts.push(enc(key) + '=' + enc(value));
    });
    return parts.length ? '?' + parts.join('&') : '';
  }

  function failOf(res, payload, text) {
    var msg = '';
    if (payload && typeof payload === 'object') msg = payload.message || (payload.error && payload.error.message) || '';
    if (!msg && typeof payload === 'string') msg = payload;
    if (!msg && text) msg = text;
    if (!msg) msg = '请求失败（' + res.status + '）';
    var err = new Error(String(msg).slice(0, 200));
    err.status = res.status;
    return err;
  }

  function readText(res) {
    return (res && typeof res.text === 'function') ? res.text() : Promise.resolve('');
  }

  /**
   * 账本专用请求。不复用 MD.api.request：后者把 body 与鉴权绑得更紧（且带 401 自动刷新），
   * 账本接口部分尚未部署，需要自己区分「404 未启用」与「500 真失败」以便优雅降级。
   * @param {string} path 以 /ledger/... 开头
   * @param {Object} opts {method, body, raw}
   */
  function ledgerApi(path, opts, retried) {
    opts = opts || {};
    var session = MD.api && MD.api.getSession ? MD.api.getSession() : null;
    var access = session && session.access;
    if (!access) {
      toast('请先登录后再使用记账本', 'warn');
      return Promise.reject(new Error('未登录'));
    }
    var headers = { Authorization: 'Bearer ' + access, Accept: 'application/json' };
    var body = opts.body;
    var isForm = (typeof FormData !== 'undefined') && body instanceof FormData;
    if (body != null && !isForm && typeof body !== 'string') body = JSON.stringify(body);
    if (body != null && !isForm) headers['Content-Type'] = 'application/json';

    var init = { method: opts.method || (body != null ? 'POST' : 'GET'), headers: headers };
    if (body != null) init.body = body;

    var fetchFn = global.fetch || (typeof fetch !== 'undefined' ? fetch : null);
    if (typeof fetchFn !== 'function') return Promise.reject(new Error('当前环境不支持网络请求'));

    var pending;
    try {
      // fetch 是外部边界：环境缺少 FormData/Headers 等实现时会同步抛错，
      // 包一层保证调用方拿到的永远是 Promise，不会因为 reject 变 throw 而中断渲染。
      pending = fetchFn('/api/v1' + path, init);
    } catch (error) {
      return Promise.reject(error);
    }
    return Promise.resolve(pending).then(function (res) {
      if (res.status === 401) {
        // 账本不走 MD.api.request（需区分 404 与 500 做降级），续期只能自己做。
        // 旧实现直接弹「登录已失效」：access 过期后只要点一下账本就要求重登，
        // 而其他模块会静默刷新——同一个会话里出现两种表现。
        if (retried) {
          toast('登录已失效，请重新登录', 'warn');
          var expired = new Error('登录已失效，请重新登录');
          expired.authInvalid = true;
          throw expired;
        }
        var refreshed = (MD.api && typeof MD.api.refresh === 'function')
          ? MD.api.refresh()
          : Promise.reject(new Error('登录已失效'));
        // 用 then(onOk, onErr) 而非 then().catch()：避免把重试分支里的业务错误
        // （如 400 参数不合法）也当成登录失效吞掉。
        return Promise.resolve(refreshed).then(function () {
          return ledgerApi(path, opts, true);
        }, function (error) {
          if (error && error.transient) throw error;   // 限流/网络：不清登录态，交上层提示
          toast('登录已失效，请重新登录', 'warn');
          var bad = new Error('登录已失效，请重新登录');
          bad.authInvalid = true;
          throw bad;
        });
      }
      if (opts.raw) {
        if (!res.ok) return readText(res).then(function (t) { throw failOf(res, null, t); });
        return res;
      }
      return readText(res).then(function (text) {
        var payload = null;
        if (text) { try { payload = JSON.parse(text); } catch (_) { payload = null; } }
        if (!res.ok) throw failOf(res, payload, typeof payload === 'string' ? payload : text);
        return payload;
      });
    });
  }

  /** 未部署（404/405）判定：用于「功能未启用」的优雅提示，而不是报红 Error。 */
  function isNotDeployed(err) {
    return err && (err.status === 404 || err.status === 405 || err.status === 501);
  }

  /* ============================ 4. 纯 SVG 图表 ============================ */

  function iconSvg(name, size) {
    return '<svg viewBox="0 0 24 24" width="' + (size || 30) + '" height="' + (size || 30) +
      '" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<use href="#i-' + esc(name) + '"/></svg>';
  }

  /** 分类配色：从 brand 经 warn 渐变到 danger，纯 CSS 变量，自动适配暗色主题。 */
  function sliceColor(i, n) {
    if (n <= 1) return 'var(--brand)';
    var t = i / (n - 1);
    if (t < 0.5) return 'color-mix(in srgb, var(--warn) ' + Math.round(t * 200) + '%, var(--brand))';
    return 'color-mix(in srgb, var(--danger) ' + Math.round((t - 0.5) * 200) + '%, var(--warn))';
  }

  function polar(cx, cy, r, deg) {
    var rad = (deg - 90) * Math.PI / 180;
    return { x: cx + r * Math.cos(rad), y: cy + r * Math.sin(rad) };
  }

  /** 环形饼图：viewBox 固定，宽度自适应父容器，绝不会撑破布局。 */
  function pieSvg(items) {
    var cx = 100, cy = 100, r = 72, hole = 44;
    var total = sumCents(items);
    if (!total) return '';
    if (items.length === 1) {
      return '<svg class="ldg-chart" viewBox="0 0 200 200" role="img" aria-label="支出分类占比">' +
        '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" fill="' + sliceColor(0, 1) + '"/>' +
        '<circle cx="' + cx + '" cy="' + cy + '" r="' + hole + '" fill="var(--surface)"/>' +
        '<text x="100" y="98" text-anchor="middle" font-size="11" fill="var(--text-dim)">合计</text>' +
        '<text x="100" y="114" text-anchor="middle" font-size="14" font-weight="700" fill="var(--text)">' + esc(money(total)) + '</text>' +
        '</svg>';
    }
    var start = 0;
    var paths = items.map(function (item, i) {
      var frac = item.cents / total;
      var end = start + frac * 360;
      var p1 = polar(cx, cy, r, start);
      var p2 = polar(cx, cy, r, end);
      var large = frac > 0.5 ? 1 : 0;
      var d = 'M' + cx + ',' + cy + ' L' + p1.x.toFixed(2) + ',' + p1.y.toFixed(2) +
        ' A' + r + ',' + r + ' 0 ' + large + ' 1 ' + p2.x.toFixed(2) + ',' + p2.y.toFixed(2) + ' Z';
      start = end;
      return '<path d="' + d + '" fill="' + sliceColor(i, items.length) + '" stroke="var(--surface)" stroke-width="1.5"/>';
    }).join('');
    return '<svg class="ldg-chart" viewBox="0 0 200 200" role="img" aria-label="支出分类占比">' + paths +
      '<circle cx="' + cx + '" cy="' + cy + '" r="' + hole + '" fill="var(--surface)"/>' +
      '<text x="100" y="98" text-anchor="middle" font-size="11" fill="var(--text-dim)">合计</text>' +
      '<text x="100" y="114" text-anchor="middle" font-size="14" font-weight="700" fill="var(--text)">' + esc(money(total)) + '</text>' +
      '</svg>';
  }

  function pieLegend(items) {
    var total = sumCents(items) || 1;
    return '<div class="ldg-legend">' + items.map(function (item, i) {
      var pct = Math.round(item.cents * 1000 / total) / 10;
      return '<div class="ldg-legend__row">' +
        '<span class="ldg-legend__sw" style="background:' + sliceColor(i, items.length) + '"></span>' +
        '<span class="ldg-legend__name">' + esc(item.name) + '</span>' +
        '<span class="ldg-legend__val">' + esc(money(item.cents)) + ' · ' + pct + '%</span>' +
        '</div>';
    }).join('') + '</div>';
  }

  /** 折线图：最近 N 个自然日的收入/支出两条线，无数据的日子补 0。 */
  function lineSvg(days) {
    var W = 340, H = 190, padL = 40, padR = 10, padT = 12, padB = 30;
    var innerW = W - padL - padR;
    var innerH = H - padT - padB;
    var n = days.length || 1;
    var max = 0;
    days.forEach(function (d) {
      max = Math.max(max, centsOf(d.income), centsOf(d.expense));
    });
    if (max <= 0) max = 1;
    function x(i) { return padL + (n === 1 ? innerW / 2 : innerW * i / (n - 1)); }
    function y(v) { return padT + innerH * (1 - v / max); }

    var grid = '';
    for (var g = 0; g <= 4; g++) {
      var gy = padT + innerH * g / 4;
      grid += '<line x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + gy.toFixed(1) +
        '" stroke="var(--border)" stroke-width="1"/>';
      grid += '<text x="' + (padL - 6) + '" y="' + (gy + 4).toFixed(1) + '" text-anchor="end" font-size="10" fill="var(--text-dim)">' +
        esc(compact(Math.round(max * (1 - g / 4)))) + '</text>';
    }

    function series(key, color) {
      var pts = days.map(function (d, i) { return x(i).toFixed(1) + ',' + y(centsOf(d[key])).toFixed(1); });
      var dots = days.map(function (d, i) {
        return '<circle cx="' + x(i).toFixed(1) + '" cy="' + y(centsOf(d[key])).toFixed(1) + '" r="2.6" fill="' + color + '">' +
          '<title>' + esc(d.date) + ' ' + esc(key === 'income' ? '收入' : '支出') + ' ' + esc(money(centsOf(d[key]))) + '</title></circle>';
      }).join('');
      return '<polyline points="' + pts.join(' ') + '" fill="none" stroke="' + color + '" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>' + dots;
    }

    var labels = days.map(function (d, i) {
      var tail = String(d.date || '').slice(5);
      return '<text x="' + x(i).toFixed(1) + '" y="' + (H - 10) + '" text-anchor="middle" font-size="10" fill="var(--text-dim)">' + esc(tail) + '</text>';
    }).join('');

    return '<svg class="ldg-chart" viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="近七日收支趋势">' +
      grid +
      series('income', 'var(--ok)') +
      series('expense', 'var(--danger)') +
      labels +
      '</svg>';
  }

  /** 分组柱状图：收入与支出按分类并排，绝不混算成一根柱子。 */
  function barSvg(income, expense) {
    var names = [];
    var map = Object.create(null);
    function collect(list, kind) {
      (list || []).forEach(function (item) {
        var key = String(item.name || '未分类');
        if (!map[key]) { map[key] = { name: key, income: 0, expense: 0 }; names.push(key); }
        map[key][kind] += item.cents;
      });
    }
    collect(income, 'income');
    collect(expense, 'expense');
    if (!names.length) return '';

    var padL = 44, padR = 12, padT = 12, padB = 46, groupW = 56, barW = 18, gap = 4, H = 210;
    var W = Math.max(340, padL + padR + names.length * groupW);
    var innerH = H - padT - padB;
    var max = 0;
    names.forEach(function (key) { max = Math.max(max, map[key].income, map[key].expense); });
    if (max <= 0) max = 1;

    var grid = '';
    for (var g = 0; g <= 4; g++) {
      var val = Math.round(max * (1 - g / 4));
      var gy = padT + innerH * g / 4;
      grid += '<line x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + gy.toFixed(1) + '" stroke="var(--border)" stroke-width="1"/>' +
        '<text x="' + (padL - 6) + '" y="' + (gy + 4).toFixed(1) + '" text-anchor="end" font-size="10" fill="var(--text-dim)">' + esc(compact(val)) + '</text>';
    }

    var bars = '', labels = '';
    names.forEach(function (key, i) {
      var item = map[key];
      var x0 = padL + i * groupW + (groupW - (barW * 2 + gap)) / 2;
      var baseY = padT + innerH;
      [['income', 'var(--ok)', 0], ['expense', 'var(--danger)', barW + gap]].forEach(function (cfg) {
        var h = Math.max(item[cfg[0]] > 0 ? 2 : 0, innerH * (item[cfg[0]] / max));
        if (item[cfg[0]] <= 0) return;
        var bx = x0 + cfg[2];
        bars += '<rect x="' + bx.toFixed(1) + '" y="' + (baseY - h).toFixed(1) + '" width="' + barW + '" height="' + h.toFixed(1) +
          '" rx="3" fill="' + cfg[1] + '"><title>' + esc(key) + ' · ' + esc(cfg[0] === 'income' ? '收入' : '支出') + ' ' +
          esc(money(item[cfg[0]])) + '</title></rect>';
      });
      var short = key.length > 4 ? key.slice(0, 4) + '…' : key;
      labels += '<text x="' + (padL + i * groupW + groupW / 2).toFixed(1) + '" y="' + (H - 26) + '" text-anchor="middle" font-size="10" fill="var(--text-dim)">' +
        esc(short) + '</text>';
    });

    return '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="分类收支对比">' + grid + bars + labels +
      '<g font-size="10" fill="var(--text-dim)">' +
      '<rect x="' + padL + '" y="' + (H - 16) + '" width="10" height="10" rx="2" fill="var(--ok)"/>' +
      '<text x="' + (padL + 14) + '" y="' + (H - 8) + '">收入</text>' +
      '<rect x="' + (padL + 46) + '" y="' + (H - 16) + '" width="10" height="10" rx="2" fill="var(--danger)"/>' +
      '<text x="' + (padL + 60) + '" y="' + (H - 8) + '">支出</text>' +
      '</g></svg>';
  }

  /* ============================ 5. 状态 ============================ */

  var S = {
    root: null,
    el: {},
    tab: 'home',
    monthKey: monthKeyOf(Date.now()),
    busy: false,
    mounted: false,
    filters: {
      type: '',
      categoryIds: [],
      keyword: '',
      range: 'month',
      from: '',
      to: '',
      reimbStatus: ''
    },
    page: 1,
    items: [],
    total: 0,
    listState: 'loading',
    listError: '',
    summary: null,
    summaryState: 'loading',
    summaryError: '',
    categories: { income: [], expense: [] },
    catsLoaded: false,
    charts: {
      pie: { state: 'loading', items: [], meta: '', error: '' },
      line: { state: 'loading', days: [], meta: '', error: '' },
      bar: { state: 'loading', income: [], expense: [], meta: '', error: '' }
    },
    // 报销视图：默认「全部状态 + 最近 30 天」，保证已报销记录一进页面就可见，列表不会因为标记而清空。
    reimb: {
      state: 'loading', items: [], total: 0, page: 1, size: 50,
      pendingTotal: 0, doneTotal: 0, counts: null, range: { from: '', to: '' },
      selected: {}, error: '', dirty: false
    },
    rbFilter: { status: 'all', range: 'd30', from: '', to: '' },
    exp: { preset: 'all', from: '', to: '' },
    backup: { state: 'loading', status: null, error: '' },
    formFiles: [],
    formAttachments: []
  };

  function mountedRoot() { return S.root && S.root.parentNode ? S.root : null; }
  function $(id) { return S.root ? S.root.querySelector('#' + id) : null; }
  function setHtml(node, html) {
    if (node) node.innerHTML = String(html == null ? '' : html);
  }
  function paint(id, html) { setHtml($(id), html); }

  /** 当前筛选对应的日期区间 */
  function activeRange() {
    if (S.filters.range === 'custom') return { from: S.filters.from, to: S.filters.to };
    return monthRange(S.filters.range === 'last' ? shiftMonthKey(S.monthKey, -1) : S.monthKey);
  }

  /** 列表请求的公共查询参数（不含分页与分类） */
  function baseQuery() {
    var range = activeRange();
    return {
      type: S.filters.type,
      keyword: S.filters.keyword,
      from: range.from,
      to: range.to,
      reimbStatus: S.filters.reimbStatus
    };
  }

  /** 今天往前第 n 天（本地时区，n=0 即今天）。 */
  function daysAgo(n) {
    var d = new Date();
    d.setDate(d.getDate() - (Number(n) || 0));
    return dateStr(d);
  }

  /**
   * 报销视图的时间区间：d30（最近 30 天，含今天）/ month / last / custom。
   * 与「工作台」的 month/last/custom 语义一致，复用 monthRange + shiftMonthKey。
   */
  function reimbRange() {
    var f = S.rbFilter;
    if (f.range === 'custom') return { from: f.from, to: f.to };
    if (f.range === 'month') return monthRange(S.monthKey);
    if (f.range === 'last') return monthRange(shiftMonthKey(S.monthKey, -1));
    return { from: daysAgo(29), to: daysAgo(0) };
  }

  /** 报销状态筛选值 → 接口 status 参数（all 表示不限）。 */
  function rbStatusParam() {
    var v = S.rbFilter.status;
    // 只认参与报销的两个状态；none 已不属于报销流程，传下去只会拿到空列表。
    return v === 'pending' || v === 'done' ? v : 'all';
  }

  /* ============================ 6. 数据加载 ============================ */

  function loadSummary() {
    S.summaryState = 'loading';
    S.summaryError = '';
    paintSummary();
    return ledgerApi('/ledger/summary?month=' + enc(S.monthKey)).then(function (res) {
      S.summary = res || null;
      S.summaryState = 'ready';
    }).catch(function (error) {
      S.summaryState = 'error';
      S.summaryError = msgOf(error);
    }).then(paintSummary);
  }

  function loadCategories() {
    if (S.catsLoaded) return Promise.resolve(S.categories);
    return Promise.all([
      ledgerApi('/ledger/categories?kind=income').catch(function () { return []; }),
      ledgerApi('/ledger/categories?kind=expense').catch(function () { return []; })
    ]).then(function (pair) {
      S.categories = {
        income: normalizeCats(pair[0]),
        expense: normalizeCats(pair[1])
      };
      S.catsLoaded = true;
      paintFilters();
      return S.categories;
    });
  }

  function normalizeCats(payload) {
    var list = payload && payload.items ? payload.items : payload;
    return (list || []).filter(function (c) { return c && c.id; }).map(function (c) {
      return { id: String(c.id), name: String(c.name || '未命名'), kind: c.kind || '' };
    });
  }

  function loadPie() {
    S.charts.pie = { state: 'loading', items: [], meta: '', error: '' };
    paintChart('pie');
    var range = monthRange(S.monthKey);
    return ledgerApi('/ledger/breakdown?from=' + enc(range.from) + '&to=' + enc(range.to)).then(function (res) {
      var list = topN(normalizeItems(res && res.expense), TOP_N);
      S.charts.pie = {
        state: list.length ? 'ready' : 'empty',
        items: list,
        meta: range.from + ' ~ ' + range.to,
        error: ''
      };
    }).catch(function (error) {
      S.charts.pie = { state: 'error', items: [], meta: '', error: msgOf(error) };
    }).then(function () { paintChart('pie'); });
  }

  function loadTrend() {
    S.charts.line = { state: 'loading', days: [], meta: '', error: '' };
    paintChart('line');
    return ledgerApi('/ledger/trend?days=7').then(function (res) {
      var days = normalizeDays(res);
      var has = days.some(function (d) { return centsOf(d.income) > 0 || centsOf(d.expense) > 0; });
      S.charts.line = {
        state: has ? 'ready' : 'empty',
        days: days,
        meta: days.length ? days[0].date + ' ~ ' + days[days.length - 1].date : '',
        error: ''
      };
    }).catch(function (error) {
      S.charts.line = { state: 'error', days: [], meta: '', error: msgOf(error) };
    }).then(function () { paintChart('line'); });
  }

  /** 趋势接口缺失时本地补 7 个自然日（全部为 0），保证图表骨架不塌陷。 */
  function normalizeDays(res) {
    var raw = (res && res.days) || [];
    if (!raw.length) {
      var out = [];
      for (var i = 6; i >= 0; i--) {
        var d = new Date();
        d.setDate(d.getDate() - i);
        out.push({ date: dateStr(d), income: { value: '0.00', cents: 0 }, expense: { value: '0.00', cents: 0 } });
      }
      return out;
    }
    return raw.map(function (d) {
      return {
        date: String(d.date || ''),
        income: d.income || { value: '0.00', cents: 0 },
        expense: d.expense || { value: '0.00', cents: 0 }
      };
    });
  }

  function loadBar() {
    S.charts.bar = { state: 'loading', income: [], expense: [], meta: '', error: '' };
    paintChart('bar');
    var q = baseQuery();
    return ledgerApi('/ledger/breakdown' + qs(q)).then(function (res) {
      var income = normalizeItems(res && res.income);
      var expense = normalizeItems(res && res.expense);
      S.charts.bar = {
        state: (income.length || expense.length) ? 'ready' : 'empty',
        income: income,
        expense: expense,
        meta: (res && res.from ? res.from + ' ~ ' + res.to : (q.from || '') + ' ~ ' + (q.to || '')),
        error: ''
      };
    }).catch(function (error) {
      S.charts.bar = { state: 'error', income: [], expense: [], meta: '', error: msgOf(error) };
    }).then(function () { paintChart('bar'); });
  }

  function loadRecords(reset) {
    if (reset) S.page = 1;
    S.listState = 'loading';
    S.listError = '';
    paintList();
    return fetchRecords().then(function (res) {
      S.items = (res && res.items) || [];
      S.total = Number(res && res.total) || S.items.length;
      S.listState = 'ready';
    }).catch(function (error) {
      S.items = [];
      S.total = 0;
      S.listState = 'error';
      S.listError = msgOf(error);
    }).then(paintList);
  }

  /**
   * 分类多选：契约的 /records 只有单值 categoryId，多选时按每个分类各取一页再归并，
   * 保证多选语义正确（而不是悄悄只按第一个分类过滤）。
   */
  function fetchRecords() {
    var q = baseQuery();
    var ids = S.filters.categoryIds;
    var take = PAGE_SIZE * S.page;
    if (ids.length <= 1) {
      // 「加载更多」取的是第 1 页到第 N 页的完整区间（page 恒为 1、size 递增），
      // 而不是只取第 N 页 —— 否则翻页会把前面几页从列表里抹掉。
      var single = Object.assign({}, q, { categoryId: ids[0] || '', page: 1, size: take });
      return ledgerApi('/ledger/records' + qs(single));
    }
    return Promise.all(ids.map(function (cid) {
      var per = Object.assign({}, q, { categoryId: cid, page: 1, size: take });
      return ledgerApi('/ledger/records' + qs(per)).catch(function () { return { items: [], total: 0 }; });
    })).then(function (list) {
      var all = [];
      list.forEach(function (res) { all = all.concat((res && res.items) || []); });
      all.sort(function (a, b) { return timeMs(b) - timeMs(a); });
      return { items: all, total: all.length, page: 1, size: take };
    });
  }

  function timeMs(rec) {
    var t = rec && (rec.transactionAt || rec.occurredOn);
    if (!t) return 0;
    var ms = new Date(t).getTime();
    return isNaN(ms) ? 0 : ms;
  }

  /**
   * 报销列表：默认 status=all（含已报销），因此「标记已报销」后记录仍留在列表里，只是徽标变了。
   * append=true 时按 page 追加下一页（勾选状态保留）。
   */
  function loadReimbursements(append) {
    var page = append ? (S.reimb.page || 1) + 1 : 1;
    var range = reimbRange();
    if (!append) {
      S.reimb.state = 'loading';
      S.reimb.error = '';
      S.reimb.page = 1;
    }
    paintReimb();
    var query = {
      status: rbStatusParam(),
      from: range.from,
      to: range.to,
      page: page,
      size: S.reimb.size
    };
    return ledgerApi('/ledger/reimbursements' + qs(query)).then(function (res) {
      var list = (res && res.items) || [];
      S.reimb.items = append ? S.reimb.items.concat(list) : list;
      S.reimb.page = Number(res && res.page) || page;
      S.reimb.total = Number(res && res.total);
      if (isNaN(S.reimb.total)) S.reimb.total = S.reimb.items.length;
      var sum = (res && res.summary) || {};
      S.reimb.counts = sum.counts || null;
      S.reimb.doneTotal = centsOf(sum.doneTotal);
      S.reimb.pendingTotal = res && res.pendingTotal != null
        ? centsOf(res.pendingTotal)
        : centsOf(sum.pendingTotal);
      S.reimb.range = (res && res.range && res.range.from) ? res.range : range;
      // 只保留仍在当前列表里的勾选，避免刷新后勾选错位（不整体清空，列表因此不会「塌陷」）
      var alive = {};
      S.reimb.items.forEach(function (item) { alive[item.id] = true; });
      Object.keys(S.reimb.selected).forEach(function (id) {
        if (!alive[id]) delete S.reimb.selected[id];
      });
      S.reimb.state = 'ready';
    }).catch(function (error) {
      if (!append) S.reimb.items = [];
      S.reimb.state = 'error';
      S.reimb.error = msgOf(error);
    }).then(function () { paintReimb(); });
  }

  function loadBackup() {
    S.backup.state = 'loading';
    S.backup.error = '';
    paintBackup();
    return ledgerApi('/ledger/backup/status').then(function (res) {
      S.backup.status = res || {};
      S.backup.state = 'ready';
    }).catch(function (error) {
      S.backup.status = null;
      // 后端未部署时不当作故障：显示「功能未启用」
      S.backup.state = isNotDeployed(error) ? 'disabled' : 'error';
      S.backup.error = msgOf(error);
    }).then(paintBackup);
  }

  function reloadAll() {
    loadSummary();
    loadPie();
    loadTrend();
    loadBar();
    loadRecords(true);
    loadCategories();
  }

  /* ============================ 7. 渲染 ============================ */

  function render(root) {
    injectStyle();
    S.root = root;
    S.mounted = true;
    S.el = {};
    root.innerHTML =
      '<div class="ldg-wrap">' +
      '<div class="ldg-tabs" id="ldg-tabs" role="tablist">' +
      tabBtn('home', '工作台') + tabBtn('reimb', '报销管理') + tabBtn('backup', '导出与备份') +
      '</div>' +
      '<div id="ldg-body"></div>' +
      '</div>';
    if (S.root && !S.root.__ldgBound) {
      S.root.__ldgBound = true;
      S.root.addEventListener('click', onClick);
      S.root.addEventListener('change', onChange);
      S.root.addEventListener('submit', onSubmit);
    }
    paintTabs();
    paintBody();
  }

  function tabBtn(id, label) {
    return '<button class="' + (S.tab === id ? 'is-active' : '') + '" type="button" role="tab" data-act="tab" data-tab="' + id + '">' + esc(label) + '</button>';
  }

  function paintTabs() {
    var box = $('ldg-tabs');
    if (!box) return;
    [].slice.call(box.querySelectorAll('button')).forEach(function (btn) {
      btn.classList.toggle('is-active', btn.getAttribute('data-tab') === S.tab);
    });
  }

  function paintBody() {
    var body = $('ldg-body');
    if (!body) return;
    if (S.tab === 'reimb') {
      body.innerHTML =
        '<section class="card">' +
        '<div class="ldg-panel__head"><span class="ldg-panel__title">报销管理</span>' +
        '<span class="ldg-panel__meta" id="ldg-rb-range"></span></div>' +
        '<div id="ldg-rb-filters"></div>' +
        '<div id="ldg-rb-sum"></div>' +
        '<div id="ldg-rb-list"></div>' +
        '</section>';
      paintReimbFilters();
      if (S.reimb.state === 'loading' && !S.reimb.items.length) loadReimbursements(); else paintReimb();
      return;
    }
    if (S.tab === 'backup') {
      body.innerHTML =
        '<section class="card">' +
        '<div class="ldg-panel__head"><span class="ldg-panel__title">导出与恢复</span></div>' +
        '<div id="ldg-exp"></div>' +
        '</section>' +
        '<section class="card">' +
        '<div class="ldg-panel__head"><span class="ldg-panel__title">自动备份</span>' +
        '<button class="btn btn-ghost" type="button" data-act="bk-refresh">刷新</button></div>' +
        '<div id="ldg-bk"></div>' +
        '</section>' +
        '<div class="ldg-note">记账数据以明文存储于服务器（便于统计与自动备份）；健康数据仍为端到端加密。</div>';
      paintExport();
      if (S.backup.state === 'loading' && !S.backup.status) loadBackup(); else paintBackup();
      return;
    }
    body.innerHTML =
      '<div class="ldg-kpis" id="ldg-kpis"></div>' +
      '<div class="ldg-charts">' +
      chartPanel('pie', '当月支出分类') +
      chartPanel('line', '近 7 日收支趋势') +
      chartPanel('bar', '分类金额对比') +
      '</div>' +
      '<section class="card"><div class="ldg-panel__head"><span class="ldg-panel__title">筛选</span>' +
      '<button class="btn btn-ghost" type="button" data-act="flt-reset">重置</button></div>' +
      '<div id="ldg-filters"></div></section>' +
      '<section class="card">' +
      '<div class="ldg-panel__head"><span class="ldg-panel__title">账单明细</span>' +
      '<span class="ldg-panel__meta" id="ldg-list-meta"></span>' +
      '<button class="btn btn-primary" type="button" data-act="rec-new">' + iconSvg('plus', 16) + '记一笔</button></div>' +
      '<div id="ldg-list"></div></section>';
    paintSummary();
    paintChart('pie');
    paintChart('line');
    paintChart('bar');
    paintFilters();
    paintList();
    if (S.summaryState === 'loading' && !S.summary) loadSummary();
    if (S.charts.pie.state === 'loading') loadPie();
    if (S.charts.line.state === 'loading') loadTrend();
    if (S.charts.bar.state === 'loading') loadBar();
    if (S.listState === 'loading' && !S.items.length) loadRecords(true);
    if (!S.catsLoaded) loadCategories();
  }

  function chartPanel(key, title) {
    return '<section class="ldg-panel">' +
      '<div class="ldg-panel__head"><span class="ldg-panel__title">' + esc(title) + '</span>' +
      '<span class="ldg-panel__meta" id="ldg-' + key + '-meta"></span>' +
      '<button class="btn btn-ghost" type="button" data-act="chart-retry" data-target="' + key + '" style="display:none">重试</button></div>' +
      '<div id="ldg-' + key + '"></div>' +
      '</section>';
  }

  function paintSummary() {
    var box = $('ldg-kpis');
    if (!box) return;
    if (S.summaryState === 'loading') {
      box.innerHTML = kpi('income', '当月收入', '<span style="color:var(--text-dim)">加载中…</span>') +
        kpi('expense', '当月支出', '<span style="color:var(--text-dim)">加载中…</span>') +
        kpi('balance', '当月结余', '<span style="color:var(--text-dim)">加载中…</span>');
      return;
    }
    if (S.summaryState === 'error') {
      box.innerHTML =
        '<div class="ldg-kpi"><div class="ldg-kpi__label">当月汇总</div>' +
        '<div class="ldg-kpi__value">—</div>' +
        '<div class="ldg-strip" style="margin-top:8px"><button class="btn btn-ghost" type="button" data-act="sum-retry">重试</button></div>' +
        '<div class="ldg-kpi__label" style="margin-top:6px;color:var(--danger)">' + esc(S.summaryError) + '</div></div>';
      return;
    }
    var income = centsOf(S.summary && S.summary.income);
    var expense = centsOf(S.summary && S.summary.expense);
    var balance = (S.summary && typeof S.summary.balance !== 'undefined') ? centsOf(S.summary.balance) : income - expense;
    box.innerHTML =
      kpi('income', '当月收入 · ' + esc(S.monthKey), money(income), 'ldg-kpi--in') +
      kpi('expense', '当月支出 · ' + esc(S.monthKey), money(expense), 'ldg-kpi--out') +
      kpi('balance', '当月结余', balance < 0 ? '-' + money(Math.abs(balance)) : money(balance), balance < 0 ? 'ldg-kpi--neg' : '');
  }

  function kpi(stat, label, valueHtml, cls) {
    return '<div class="ldg-kpi ' + (cls || '') + '" data-stat="' + stat + '">' +
      '<div class="ldg-kpi__label">' + esc(label) + '</div>' +
      '<div class="ldg-kpi__value">' + valueHtml + '</div>' +
      '</div>';
  }

  function loadingBox() { return '<div class="ldg-skeleton" role="status" aria-label="加载中"></div>'; }

  function errorBox(message, act, target) {
    return '<div class="ldg-placeholder">' + iconSvg('warning', 30) +
      '<span style="color:var(--danger)">' + esc(message || '加载失败') + '</span>' +
      '<button class="btn btn-ghost" type="button" data-act="' + act + '"' +
      (target ? ' data-target="' + esc(target) + '"' : '') + '>重试</button>' +
      '</div>';
  }

  function emptyBox() {
    return '<div class="ldg-placeholder" data-empty="1">' + iconSvg('wallet', 30) + '<span>暂无数据</span></div>';
  }

  function paintChart(key) {
    if (!mountedRoot()) return;
    var st = S.charts[key];
    var meta = $('ldg-' + key + '-meta');
    if (meta) meta.textContent = st.meta || '';
    var btn = S.root.querySelector('[data-act="chart-retry"][data-target="' + key + '"]');
    if (btn) btn.style.display = st.state === 'error' ? '' : 'none';
    var host = $('ldg-' + key);
    if (!host) return;

    if (st.state === 'loading') { host.innerHTML = loadingBox(); return; }
    if (st.state === 'error') { host.innerHTML = errorBox(st.error, 'chart-retry', key); return; }
    if (st.state === 'empty') { host.innerHTML = emptyBox(); return; }

    if (key === 'pie') {
      host.innerHTML = '<div class="ldg-chartbox">' + pieSvg(st.items) + '</div>' + pieLegend(st.items);
    } else if (key === 'line') {
      host.innerHTML = '<div class="ldg-chartbox">' + lineSvg(st.days) + '</div>' +
        '<div class="ldg-strip" style="font-size:12px;color:var(--text-dim)">' +
        '<span style="display:inline-flex;align-items:center;gap:6px"><i style="width:12px;height:12px;border-radius:3px;background:var(--ok);display:inline-block"></i>收入</span>' +
        '<span style="display:inline-flex;align-items:center;gap:6px"><i style="width:12px;height:12px;border-radius:3px;background:var(--danger);display:inline-block"></i>支出</span>' +
        '</div>';
    } else {
      host.innerHTML = '<div class="ldg-scroll">' + barSvg(st.income, st.expense) + '</div>';
    }
  }

  function paintFilters() {
    var box = $('ldg-filters');
    if (!box) return;
    var f = S.filters;
    var types = [['', '全部'], ['income', '收入'], ['expense', '支出']];
    var ranges = [['month', '本月'], ['last', '上月'], ['custom', '自定义']];
    var reimbs = [['', '全部报销状态'], ['pending', '待报销'], ['done', '已报销'], ['none', '不涉及']];

    box.innerHTML =
      '<div class="ldg-filters">' +
      '<div class="field"><span class="ldg-flabel">类型</span><div class="ldg-chips">' +
      types.map(function (t) {
        return '<button class="chip' + (f.type === t[0] ? ' chip--active' : '') + '" type="button" data-act="flt-type" data-value="' + t[0] + '">' + t[1] + '</button>';
      }).join('') + '</div></div>' +

      '<div class="field"><span class="ldg-flabel">时间范围</span><div class="ldg-chips">' +
      ranges.map(function (t) {
        return '<button class="chip' + (f.range === t[0] ? ' chip--active' : '') + '" type="button" data-act="flt-range" data-value="' + t[0] + '">' + t[1] + '</button>';
      }).join('') + '</div>' +
      '<div class="ldg-date"' + (f.range === 'custom' ? '' : ' hidden') + '>' +
      '<input class="input" type="date" id="ldg-from" value="' + esc(f.from) + '" aria-label="开始日期">' +
      '<input class="input" type="date" id="ldg-to" value="' + esc(f.to) + '" aria-label="结束日期">' +
      '</div>' +
      (f.range === 'custom' ? '<button class="btn btn-primary" type="button" data-act="flt-custom">应用范围</button>' : '') +
      '</div>' +

      '<div class="field"><span class="ldg-flabel">分类（可多选）</span>' +
      '<button class="btn btn-ghost" type="button" data-act="cat-open" style="min-height:44px;justify-content:flex-start">' +
      '<span>' + (f.categoryIds.length ? '已选 ' + f.categoryIds.length + ' 个分类' : '全部分类') + '</span></button></div>' +

      '<div class="field"><span class="ldg-flabel">报销状态</span>' +
      '<select class="select" data-act="sel-reimb" aria-label="报销状态">' +
      reimbs.map(function (t) {
        return '<option value="' + t[0] + '"' + (f.reimbStatus === t[0] ? ' selected' : '') + '>' + t[1] + '</option>';
      }).join('') + '</select></div>' +

      '<div class="field" style="grid-column:1/-1"><span class="ldg-flabel">关键词</span>' +
      '<div class="ldg-frow">' +
      '<input class="input" type="search" id="ldg-keyword" value="' + esc(f.keyword) + '" placeholder="备注/分类关键词" style="flex:1 1 160px">' +
      '<button class="btn btn-primary" type="button" data-act="search" style="min-height:44px">' + iconSvg('toolbox', 16) + '搜索</button>' +
      '</div></div>' +
      '</div>';
  }

  function paintList() {
    if (!mountedRoot()) return;
    var host = $('ldg-list');
    if (!host) return;
    var meta = $('ldg-list-meta');
    if (meta) meta.textContent = (S.listState === 'ready' && S.total) ? ('共 ' + S.total + ' 条 · 已显示 ' + S.items.length) : '';

    if (S.listState === 'loading' && !S.items.length) { host.innerHTML = loadingBox(); return; }
    if (S.listState === 'error' && !S.items.length) { host.innerHTML = errorBox(S.listError, 'list-retry'); return; }
    if (!S.items.length) { host.innerHTML = emptyBox(); return; }

    host.innerHTML = '<div>' + S.items.map(recordRow).join('') + '</div>' +
      (S.items.length < S.total ? '<button class="btn btn-ghost" type="button" data-act="rec-more" style="min-height:44px;margin-top:10px;width:100%">加载更多</button>' : '');
  }

  function recordRow(rec) {
    var income = rec.type === 'income';
    var hasAtt = Number(rec.attachmentCount) > 0;
    return '<div class="ldg-rec" data-id="' + esc(rec.id) + '">' +
      '<div class="ldg-rec__body" data-act="rec-edit" data-id="' + esc(rec.id) + '" role="button" tabindex="0">' +
      '<div class="ldg-rec__title">' + esc(rec.note || rec.categoryName || (income ? '收入' : '支出')) + '</div>' +
      '<div class="ldg-rec__meta">' +
      '<span>' + esc(fmtOccurred(rec)) + '</span>' +
      '<span class="badge">' + esc(rec.categoryName || '未分类') + '</span>' +
      (rec.reimbursementStatus && rec.reimbursementStatus !== 'none'
        ? '<span class="badge badge--' + (REIMB_TONE[rec.reimbursementStatus] || '') + '">' + esc(REIMB_TEXT[rec.reimbursementStatus] || rec.reimbursementStatus) + '</span>'
        : '') +
      (hasAtt ? '<span class="badge">' + iconSvg('camera', 12) + ' ' + Number(rec.attachmentCount) + '</span>' : '') +
      '</div></div>' +
      '<div class="ldg-rec__amt ' + (income ? 'ldg-amt--in' : 'ldg-amt--out') + '">' +
      (income ? '+' : '-') + money(centsOf(rec)) +
      '</div>' +
      '<div class="ldg-rec__acts">' +
      '<button class="btn-icon" type="button" data-act="rec-edit" data-id="' + esc(rec.id) + '" aria-label="编辑">' + iconSvg('edit', 18) + '</button>' +
      '<button class="btn-icon" type="button" data-act="rec-del" data-id="' + esc(rec.id) + '" aria-label="删除">' + iconSvg('trash', 18) + '</button>' +
      '</div>' +
      '</div>';
  }

  /* ---------- 报销管理 ---------- */

  // 状态筛选四档：全部 / 待报销 / 已报销 / 不涉及。默认 all，已报销记录才不会「凭空消失」。
  // 「不涉及」不放进筛选项：不涉及报销的记录不属于报销流程，
  // 后端同样已把 all 的语义收窄为 pending + done（ReimbExcludeNone）。
  var RB_STATUS_OPTIONS = [['all', '全部'], ['pending', '待报销'], ['done', '已报销']];
  var RB_RANGE_OPTIONS = [['d30', '最近 30 天'], ['month', '本月'], ['last', '上月'], ['custom', '自定义']];

  function selectedIds() {
    return Object.keys(S.reimb.selected).filter(function (id) { return S.reimb.selected[id]; });
  }

  /** 报销状态徽标：三种状态都渲染，已报销用 ok 色，不再是「有就显示、没有就消失」。 */
  function reimbBadge(status) {
    var st = status || 'none';
    var tone = REIMB_TONE[st] || '';
    return '<span class="badge' + (tone ? ' badge--' + tone : '') + '">' + esc(REIMB_TEXT[st] || st) + '</span>';
  }

  function paintReimbFilters() {
    var host = $('ldg-rb-filters');
    if (!host) return;
    var f = S.rbFilter;
    host.innerHTML =
      '<div class="ldg-frow" style="margin-bottom:8px">' +
      '<span class="ldg-flabel">状态</span>' +
      '<select class="select" data-act="rb-status" aria-label="报销状态筛选" style="flex:0 1 180px">' +
      RB_STATUS_OPTIONS.map(function (opt) {
        return '<option value="' + opt[0] + '"' + (f.status === opt[0] ? ' selected' : '') + '>' + esc(opt[1]) + '</option>';
      }).join('') +
      '</select></div>' +
      '<div class="ldg-frow">' +
      '<span class="ldg-flabel">时间</span><div class="ldg-chips">' +
      RB_RANGE_OPTIONS.map(function (opt) {
        return '<button class="chip' + (f.range === opt[0] ? ' chip--active' : '') + '" type="button" data-act="rb-range" data-value="' + opt[0] + '">' + esc(opt[1]) + '</button>';
      }).join('') +
      '</div></div>' +
      (f.range === 'custom'
        ? '<div class="ldg-frow" style="margin-top:8px">' +
        '<input class="input" type="date" id="ldg-rb-from" value="' + esc(f.from) + '" aria-label="开始日期" style="flex:1 1 140px">' +
        '<input class="input" type="date" id="ldg-rb-to" value="' + esc(f.to) + '" aria-label="结束日期" style="flex:1 1 140px">' +
        '<button class="btn btn-primary" type="button" data-act="rb-custom" style="min-height:44px">应用</button>' +
        '</div>'
        : '');
  }

  function paintReimbSum() {
    var host = $('ldg-rb-sum');
    if (!host) return;
    var ids = selectedIds();
    var byId = {};
    S.reimb.items.forEach(function (item) { byId[item.id] = item; });
    var selAmount = 0;
    var canDone = false;
    var canPending = false;
    ids.forEach(function (id) {
      var hit = byId[id];
      if (!hit) return;
      selAmount += centsOf(hit);
      var st = hit.reimbursementStatus || 'none';
      // 只统计参与报销的状态：none 若出现在列表里（理论不会）也不该让
      // "标记已报销"按钮变为可用。
      if (st === 'pending') canDone = true;
      if (st === 'done') canPending = true;
    });
    var allChecked = S.reimb.items.length > 0 && ids.length === S.reimb.items.length;
    var counts = S.reimb.counts || {};
    host.innerHTML =
      '<div class="ldg-sumline">' +
      '<span>待报销 <b style="color:var(--warn)">' + esc(money(S.reimb.pendingTotal)) + '</b>' +
      (counts.pending != null ? ' · ' + Number(counts.pending) + ' 条' : '') + '</span>' +
      '<span>已报销 <b style="color:var(--ok)">' + esc(money(S.reimb.doneTotal)) + '</b>' +
      (counts.done != null ? ' · ' + Number(counts.done) + ' 条' : '') + '</span>' +
      '<span>已选 <b id="ldg-rb-sel-amount" style="color:var(--brand)">' + esc(money(selAmount)) + '</b>' +
      ' · <b id="ldg-rb-sel-count">' + ids.length + '</b> 条</span>' +
      '</div>' +
      '<div class="ldg-strip">' +
      '<label class="ldg-check"><input type="checkbox" data-act="rb-all"' + (allChecked ? ' checked' : '') + '><span>全选</span></label>' +
      '<button class="btn btn-primary" type="button" data-act="rb-mark" data-status="done"' + (canDone ? '' : ' disabled') + '>标记已报销</button>' +
      '<button class="btn btn-ghost" type="button" data-act="rb-mark" data-status="pending"' + (canPending ? '' : ' disabled') + '>退回待报销</button>' +
      '<button class="btn btn-ghost" type="button" data-act="rb-export"' + (ids.length ? '' : ' disabled') + '>导出勾选项</button>' +
      '</div>';
  }

  function reimbRow(rec) {
    var st = rec.reimbursementStatus || 'none';
    // 只有 pending / done 属于报销流程；「不涉及」的记录正常不会出现在本页
    // （后端 all 已排除），万一出现也不给勾选与标记入口 —— 否则可以把
    // 一条与报销无关的记录标成"已报销"，这是用户明确反馈的问题。
    var actionable = st === 'pending' || st === 'done';
    var next = st === 'done'
      ? { status: 'pending', label: '退回待报销' }
      : { status: 'done', label: '标记已报销' };
    return '<div class="ldg-rec" data-id="' + esc(rec.id) + '">' +
      '<label class="ldg-check" style="flex:0 0 auto"><input type="checkbox" data-act="rb-check" data-id="' + esc(rec.id) + '"' +
      (actionable && S.reimb.selected[rec.id] ? ' checked' : '') + (actionable ? '' : ' disabled') +
      ' aria-label="选择"></label>' +
      '<div class="ldg-rec__body">' +
      '<div class="ldg-rec__title">' + esc(rec.note || rec.categoryName || '支出') + '</div>' +
      '<div class="ldg-rec__meta"><span>' + esc(fmtOccurred(rec)) + '</span>' +
      '<span class="badge">' + esc(rec.categoryName || '未分类') + '</span>' +
      reimbBadge(st) +
      '</div></div>' +
      '<div class="ldg-rec__amt ldg-amt--out">-' + money(centsOf(rec)) + '</div>' +
      (actionable
        ? '<button class="btn btn-ghost" type="button" data-act="rb-one" data-id="' + esc(rec.id) +
        '" data-status="' + next.status + '" style="min-height:36px;flex:0 0 auto">' + next.label + '</button>'
        : '') +
      '</div>';
  }

  function paintReimb() {
    if (!mountedRoot()) return;
    if (S.tab !== 'reimb') return;
    paintReimbFilters();
    paintReimbSum();
    var rangeHost = $('ldg-rb-range');
    if (rangeHost) {
      var r = S.reimb.range || {};
      rangeHost.textContent = (r.from || '不限') + ' ~ ' + (r.to || '不限');
    }
    var host = $('ldg-rb-list');
    if (!host) return;
    // 刷新期间保留上一版列表，避免每次标记都闪一次骨架屏（观感上像「列表被清空」）
    if (S.reimb.state === 'loading' && !S.reimb.items.length) { host.innerHTML = loadingBox(); return; }
    if (S.reimb.state === 'error' && !S.reimb.items.length) { host.innerHTML = errorBox(S.reimb.error, 'rb-retry'); return; }
    if (!S.reimb.items.length) {
      host.innerHTML = '<div class="ldg-placeholder">' + iconSvg('wallet', 30) +
        '<span>当前筛选范围内没有报销记录</span></div>';
      return;
    }
    var stale = (S.reimb.state === 'error' && S.reimb.error)
      ? '<p class="muted" style="font-size:12px;color:var(--danger);margin:0 0 8px">刷新失败：' + esc(S.reimb.error) + '（以下为上次结果）</p>'
      : '';
    host.innerHTML = stale + '<div>' + S.reimb.items.map(reimbRow).join('') + '</div>' +
      (S.reimb.items.length < S.reimb.total
        ? '<button class="btn btn-ghost" type="button" data-act="rb-more" style="min-height:44px;margin-top:10px;width:100%">加载更多（共 ' + Number(S.reimb.total) + ' 条）</button>'
        : '');
  }

  /* ---------- 导出与备份 ---------- */

  function paintExport() {
    var host = $('ldg-exp');
    if (!host) return;
    var e = S.exp;
    var presets = [['all', '全部'], ['thisMonth', '本月'], ['lastMonth', '上月'], ['custom', '自定义']];
    host.innerHTML =
      '<div class="field"><span class="ldg-flabel">导出时间范围</span>' +
      '<div class="ldg-chips">' +
      presets.map(function (p) {
        return '<button class="chip' + (e.preset === p[0] ? ' chip--active' : '') + '" type="button" data-act="exp-range" data-value="' + p[0] + '">' + esc(p[1]) + '</button>';
      }).join('') +
      '</div>' +
      (e.preset === 'custom'
        ? '<div class="ldg-frow" style="margin-top:8px">' +
        '<input class="input" type="date" id="ldg-exp-from" value="' + esc(e.from) + '" aria-label="导出开始日期" style="flex:1 1 140px">' +
        '<input class="input" type="date" id="ldg-exp-to" value="' + esc(e.to) + '" aria-label="导出结束日期" style="flex:1 1 140px">' +
        '</div>'
        : '') +
      '</div>' +
      '<div class="ldg-strip" style="margin-top:10px">' +
      '<button class="btn btn-primary" type="button" data-act="exp-html">HTML 报告</button>' +
      '<button class="btn btn-ghost" type="button" data-act="exp-zip">ZIP 数据包</button>' +
      '<button class="btn btn-ghost" type="button" data-act="exp-json">JSON 备份下载</button>' +
      '</div>' +
      '<div class="divider"></div>' +
      '<div class="ldg-frow">' +
      '<input class="input" type="file" id="ldg-import-file" accept="application/json,.json" style="flex:1 1 220px">' +
      '<button class="btn btn-ghost" type="button" data-act="imp-run" data-mode="merge">合并导入</button>' +
      '<button class="btn btn-danger" type="button" data-act="imp-run" data-mode="overwrite">覆盖导入</button>' +
      '</div>' +
      '<p class="muted" style="font-size:12px;margin:8px 0 0">当前导出范围：' + esc(describeRange()) + '。' +
      '「覆盖导入」会先自动备份现有数据再整体替换。</p>';
  }

  /** 导出区间的可读文案（与请求体里的 preset 保持一致）。 */
  function describeRange() {
    var e = S.exp;
    if (e.preset === 'thisMonth') { var m = monthRange(S.monthKey); return '本月 ' + m.from + ' ~ ' + m.to; }
    if (e.preset === 'lastMonth') { var l = monthRange(shiftMonthKey(S.monthKey, -1)); return '上月 ' + l.from + ' ~ ' + l.to; }
    if (e.preset === 'custom') return (e.from || '未选') + ' ~ ' + (e.to || '未选');
    return '全部时间';
  }

  /** 组 ExportFilter 请求体；自定义区间非法时返回 null（已 toast），调用方不再发请求。 */
  function exportFilter() {
    var e = S.exp;
    var preset = e.preset || 'all';
    if (preset === 'custom') {
      // 直接以输入框为准：用户填完日期就该能导，不必再点「应用」
      var fromNode = $('ldg-exp-from');
      var toNode = $('ldg-exp-to');
      var from = String(fromNode ? fromNode.value : (e.from || ''));
      var to = String(toNode ? toNode.value : (e.to || ''));
      e.from = from;
      e.to = to;
      if (!from || !to) { toast('请选择完整的导出开始与结束日期', 'warn'); return null; }
      if (from > to) { toast('开始日期不能晚于结束日期', 'warn'); return null; }
      return { preset: 'custom', from: from, to: to };
    }
    return { preset: preset };
  }

  // 凭据表单的「本机缓存」。
  // 后端只回传脱敏信息（主机名 + 掩码账号），完整地址和用户名无从回填，
  // 用户每打开一次表单就要重敲一遍。缓存在本机 localStorage 后，同一台设备
  // 打开即可自动带出上次保存的值；口令永不缓存、永不回显。
  // 换设备或清过缓存时表单为空，但状态行仍显示"已配置 · q*** @ webdav.xxx"，
  // 用户据此知道配置没丢，不必反复重填。
  var DAV_CACHE = 'meddesk.ledger.dav';

  function readDavCache() {
    try {
      var raw = global.localStorage.getItem(DAV_CACHE);
      return raw ? (JSON.parse(raw) || {}) : {};
    } catch (e) { return {}; }
  }

  function writeDavCache(value) {
    try { global.localStorage.setItem(DAV_CACHE, JSON.stringify(value)); } catch (e) { /* 隐私模式忽略 */ }
  }

  function clearDavCache() {
    try { global.localStorage.removeItem(DAV_CACHE); } catch (e) { /* 忽略 */ }
  }

  /** 备份时间：后端给 RFC3339(UTC)，转成本地 "YYYY-MM-DD HH:mm"。 */
  function backupTimeText(run) {
    var raw = String((run && (run.updatedAt || run.date)) || '');
    if (!raw) return '—';
    var d = new Date(raw);
    if (isNaN(d.getTime())) return raw;
    function pad(n) { return (n < 10 ? '0' : '') + n; }
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  /** 备份结果码 → 中文（后端只有 success / failed / skipped 三种）。 */
  function backupResultText(status) {
    var s = String(status || '');
    if (s === 'success') return '成功';
    if (s === 'failed' || s === 'error') return '失败';
    if (s === 'skipped') return '跳过';
    return s || '—';
  }

  function paintBackup() {
    var host = $('ldg-bk');
    if (!host) return;
    if (S.backup.state === 'loading') { host.innerHTML = loadingBox(); return; }
    if (S.backup.state === 'disabled') {
      host.innerHTML = '<div class="ldg-placeholder">' + iconSvg('lock', 30) +
        '<span>功能未启用</span><span style="font-size:12px">服务端尚未提供记账本备份接口</span></div>';
      return;
    }
    if (S.backup.state === 'error') { host.innerHTML = errorBox(S.backup.error, 'bk-refresh'); return; }

    var st = S.backup.status || {};
    // 字段名必须对齐后端 BackupStatus：configured / runs / nextRunAt / credential。
    // 这里历史上读的是 hasCredentials / lastRunAt / lastFileName —— 后端从来没有这些字段，
    // 于是「凭据明明已配置、备份明明成功过，界面却永远显示未配置 / 从未备份」，
    // 用户据此判断凭据丢了，然后反复重填。
    var runs = Array.isArray(st.runs) ? st.runs : [];
    var last = runs.length ? runs[0] : null;
    var cred = st.credential || null;
    var configured = st.configured === true;
    var cached = readDavCache();
    var rows = [
      ['最近备份时间', last ? backupTimeText(last) : '从未备份'],
      ['最近结果', last ? backupResultText(last.status) : '—'],
      ['备份文件', last && last.remotePath ? last.remotePath : '—'],
      ['WebDAV 凭据', !configured ? '未配置'
        : (cred ? '已配置' : '已配置（无法读取，请重新填写）')],
      // 只显示脱敏信息：主机名 + 掩码账号，足以辨认"是哪台网盘、哪个账号"，
      // 又不把完整凭据放进响应体/页面 DOM。
      ['WebDAV 地址', configured && cred
        ? (cred.usernameMasked ? cred.usernameMasked + ' @ ' : '') + (cred.host || '')
        : '—'],
      ['下次计划', st.nextRunAt || '待调度']
    ];
    host.innerHTML =
      '<div>' + rows.map(function (r) {
        return '<div class="setting-item"><div class="setting-item__copy">' +
          '<span class="setting-item__title">' + esc(r[0]) + '</span>' +
          '<span class="setting-item__desc">' + esc(String(r[1])) + '</span>' +
          '</div></div>';
      }).join('') + '</div>' +
      '<div class="divider"></div>' +
      '<form id="ldg-bk-form" class="stack">' +
      // 回填已保存的地址 / 用户名 / 目录：口令是唯一不回传的字段。
      // 不回填的后果是「每次打开都是空白 → 以为凭据丢了 → 重填时口令又留空 → 保存失败」。
      (configured && !cred
        ? '<p class="muted" style="font-size:12px;margin:0 0 8px">已保存的凭据无法解密（服务端加密密钥可能更换过），请重新填写完整信息。</p>'
        : '') +
      '<div class="ldg-row2">' +
      '<div class="field"><span class="ldg-flabel">WebDAV 地址</span>' +
      '<input class="input" name="url" type="url" placeholder="' +
      esc(configured && cred && cred.host
        ? '当前已配置 ' + cred.host + '，留空沿用'
        : 'https://webdav.example.com/meddesk') +
      '" value="' + esc(cached.baseURL || '') + '" autocomplete="off"></div>' +
      '<div class="field"><span class="ldg-flabel">用户名</span>' +
      '<input class="input" name="user" type="text" placeholder="' +
      esc(configured && cred && cred.usernameMasked ? '当前账号 ' + cred.usernameMasked + '，留空沿用' : '') +
      '" value="' + esc(cached.username || '') + '" autocomplete="off"></div>' +
      '</div>' +
      '<div class="ldg-row2">' +
      '<div class="field"><span class="ldg-flabel">密码 / 应用口令</span>' +
      '<input class="input" name="pass" type="password" autocomplete="new-password" placeholder="' +
      (configured && cred && cred.hasPassword ? '已保存，留空不修改' : '请填写密码') + '"></div>' +
      // 备份目录：地址只填到账号根，文件统一落到这个子目录，
      // 避免 ledger-backup-*.zip 与网盘里其它文件混在同一层。
      '<div class="field"><span class="ldg-flabel">备份目录</span>' +
      '<input class="input" name="dir" type="text" value="' +
      esc(cached.pathPrefix || (cred && cred.pathPrefix) || 'backup') +
      '" placeholder="backup" autocomplete="off"></div>' +
      '</div>' +
      '<div class="ldg-strip">' +
      '<button class="btn btn-primary" type="submit">保存凭据</button>' +
      '<button class="btn btn-danger" type="button" data-act="bk-cred-del">删除凭据</button>' +
      '<button class="btn btn-ghost" type="button" data-act="bk-run">立即备份</button>' +
      '</div>' +
      '</form>';
  }

  /* ============================ 8. 事件 ============================ */

  function onClick(event) {
    var node = event.target && event.target.closest ? event.target.closest('[data-act]') : null;
    if (!node || !S.root || !S.root.contains(node)) return;
    var act = node.getAttribute('data-act');
    if (!act) return;
    tap();

    switch (act) {
      case 'tab':
        S.tab = node.getAttribute('data-tab') || 'home';
        paintTabs();
        paintBody();
        // 报销列表有自己的状态筛选与分页，不属于 reloadAll() 的刷新范围。
        // 在别的页新增/编辑账单后它是过期的：这里按脏标记重拉，
        // 否则用户切过来看到的是上次的快照，会以为"新增没生效"而去手动刷新页面。
        if (S.tab === 'reimb' && S.reimb.dirty) {
          S.reimb.dirty = false;
          // state 仍是 loading 时，paintBody 内部已经会触发首次加载，不必再发一次；
          // 只有"已有数据但已过期"才需要在这里补一次刷新。
          if (S.reimb.state !== 'loading') loadReimbursements();
        }
        return;
      case 'flt-type':
        S.filters.type = node.getAttribute('data-value') || '';
        paintFilters();
        loadRecords(true).then(function () { loadBar(); });
        return;
      case 'flt-range':
        S.filters.range = node.getAttribute('data-value') || 'month';
        paintFilters();
        loadRecords(true).then(loadBar);
        return;
      case 'flt-custom': {
        var from = $('ldg-from'), to = $('ldg-to');
        S.filters.from = from ? from.value : '';
        S.filters.to = to ? to.value : '';
        if (!S.filters.from || !S.filters.to) { toast('请选择完整的开始与结束日期', 'warn'); return; }
        if (S.filters.from > S.filters.to) { toast('开始日期不能晚于结束日期', 'warn'); return; }
        loadRecords(true).then(loadBar);
        return;
      }
      case 'flt-reset':
        S.filters = { type: '', categoryIds: [], keyword: '', range: 'month', from: '', to: '', reimbStatus: '' };
        paintFilters();
        reloadAll();
        return;
      case 'search': {
        var kw = $('ldg-keyword');
        S.filters.keyword = kw ? String(kw.value || '').trim() : '';
        loadRecords(true).then(loadBar);
        return;
      }
      case 'cat-open':
        openCategorySheet();
        return;
      case 'sum-retry':
        loadSummary();
        return;
      case 'list-retry':
        loadRecords(true);
        return;
      case 'chart-retry': {
        var target = node.getAttribute('data-target');
        if (target === 'pie') loadPie();
        else if (target === 'line') loadTrend();
        else loadBar();
        return;
      }
      case 'rec-new':
        openRecordForm(null);
        return;
      case 'rec-edit':
        openRecordForm(findRecord(node.getAttribute('data-id')));
        return;
      case 'rec-del':
        confirmDelete(node.getAttribute('data-id'));
        return;
      case 'rec-more':
        S.page += 1;
        loadRecords(false);
        return;
      case 'rb-retry':
        loadReimbursements();
        return;
      case 'rb-more':
        loadReimbursements(true);
        return;
      case 'rb-range': {
        var prevFrom = $('ldg-rb-from'), prevTo = $('ldg-rb-to');
        if (prevFrom || prevTo) {
          S.rbFilter.from = prevFrom ? prevFrom.value : S.rbFilter.from;
          S.rbFilter.to = prevTo ? prevTo.value : S.rbFilter.to;
        }
        S.rbFilter.range = node.getAttribute('data-value') || 'd30';
        paintReimbFilters();
        if (S.rbFilter.range === 'custom') {
          // 自定义：等用户点「应用」再查，避免半截日期就去请求
          toast('请选择起止日期后点「应用」', 'info');
          return;
        }
        loadReimbursements();
        return;
      }
      case 'rb-custom': {
        var rbFrom = $('ldg-rb-from');
        var rbTo = $('ldg-rb-to');
        S.rbFilter.from = rbFrom ? rbFrom.value : '';
        S.rbFilter.to = rbTo ? rbTo.value : '';
        if (!S.rbFilter.from || !S.rbFilter.to) { toast('请选择完整的开始与结束日期', 'warn'); return; }
        if (S.rbFilter.from > S.rbFilter.to) { toast('开始日期不能晚于结束日期', 'warn'); return; }
        loadReimbursements();
        return;
      }
      case 'rb-mark':
        markReimbursements(node.getAttribute('data-status') || 'done');
        return;
      case 'rb-one':
        markReimbursements(node.getAttribute('data-status') || 'pending', [node.getAttribute('data-id')]);
        return;
      case 'rb-export':
        exportSelectedReimbursements();
        return;
      case 'exp-range': {
        var preset = node.getAttribute('data-value') || 'all';
        var efCur = $('ldg-exp-from'), etCur = $('ldg-exp-to');
        if (efCur || etCur) {
          // 切档前先记住已填的日期，避免在档位间来回切时丢失输入
          S.exp.from = efCur ? efCur.value : S.exp.from;
          S.exp.to = etCur ? etCur.value : S.exp.to;
        }
        S.exp.preset = preset;
        paintExport();
        return;
      }
      case 'bk-refresh':
        loadBackup();
        return;
      case 'bk-run':
        runTask('备份', function () {
          return ledgerApi('/ledger/backup/run', { method: 'POST' }).then(function () {
            return loadBackup().then(function () { return '已触发备份'; });
          });
        });
        return;
      case 'bk-cred-del':
        MD.ui.confirm({
          title: '删除 WebDAV 凭据',
          message: '删除后服务端将停止每日自动备份，确定继续？',
          confirmText: '删除',
          danger: true
        }).then(function (yes) {
          if (!yes) return;
          runTask('删除凭据', function () {
            return ledgerApi('/ledger/backup/credentials', { method: 'DELETE' }).then(function () {
              // 主动删除时本机缓存也一并清掉，否则下次打开还会带出已失效的旧值。
              clearDavCache();
              return loadBackup().then(function () { return '凭据已删除'; });
            });
          });
        });
        return;
      case 'exp-html':
        downloadExport('/ledger/export/html', 'html', '记账本报告');
        return;
      case 'exp-zip':
        downloadExport('/ledger/export/zip', 'zip', '记账本数据包');
        return;
      case 'exp-json':
        downloadExport('/ledger/export/json', 'json', '记账本备份');
        return;
      case 'imp-run':
        runImport(node.getAttribute('data-mode') || 'merge');
        return;
      default:
        return;
    }
  }

  function onChange(event) {
    var node = event.target;
    if (!node || !node.getAttribute) return;
    var act = node.getAttribute('data-act');
    if (act === 'sel-reimb') {
      S.filters.reimbStatus = node.value || '';
      loadRecords(true).then(loadBar);
      return;
    }
    if (act === 'rb-status') {
      S.rbFilter.status = node.value || 'all';
      loadReimbursements();
      return;
    }
    if (act === 'rb-all') {
      var checked = !!node.checked;
      S.reimb.selected = {};
      if (checked) S.reimb.items.forEach(function (item) { S.reimb.selected[item.id] = true; });
      [].slice.call(S.root.querySelectorAll('[data-act="rb-check"]')).forEach(function (cb) { cb.checked = checked; });
      paintReimbSum();
      return;
    }
    if (act === 'rb-check') {
      var id = node.getAttribute('data-id');
      if (node.checked) S.reimb.selected[id] = true; else delete S.reimb.selected[id];
      var all = S.root.querySelector('[data-act="rb-all"]');
      if (all) all.checked = selectedIds().length === S.reimb.items.length && S.reimb.items.length > 0;
      paintReimbSum();
      return;
    }
  }

  function onSubmit(event) {
    if (!event.target || event.target.id !== 'ldg-bk-form') return;
    event.preventDefault();
    var form = event.target;
    var url = String((form.elements.url && form.elements.url.value) || '').trim();
    var user = String((form.elements.user && form.elements.user.value) || '').trim();
    var pass = String((form.elements.pass && form.elements.pass.value) || '');
    // 目录留空时回落到 backup：备份文件不该散落在账号根目录。
    var dir = String((form.elements.dir && form.elements.dir.value) || '').trim() || 'backup';
    var st = S.backup.status || {};
    var configured = st.configured === true;
    // 已配置、可解密、且确实有口令 → 三个字段都允许留空沿用旧值（后端支持）。
    // 场景：换设备后本机没有缓存，用户只想改口令或只调备份目录，
    // 不该被迫把地址和账号重敲一遍。
    var canReuse = configured && !!st.credential && st.credential.hasPassword === true;
    if (!url && !canReuse) { toast('请填写 WebDAV 地址', 'warn'); return; }
    if (url && !/^https?:\/\//i.test(url)) { toast('WebDAV 地址需以 http(s):// 开头', 'warn'); return; }
    if (!user && !canReuse) { toast('请填写用户名', 'warn'); return; }
    if (!pass && !canReuse) { toast('请填写 WebDAV 密码', 'warn'); return; }
    runTask('保存凭据', function () {
      return ledgerApi('/ledger/backup/credentials', {
        method: 'PUT',
        // 字段名必须与后端 CredentialsInput 的 json tag 一致（baseURL/username/password）。
        // 曾经这里写成 url，后端解析到空字符串，于是无论填什么都报「请填写 WebDAV 服务地址」。
        body: { baseURL: url, username: user, password: pass, pathPrefix: dir }
      }).then(function () {
        // 记住本机填写过的完整值（口令不入缓存），下次打开表单自动回填。
        // 留空沿用的字段不覆盖已有缓存，避免把上次记住的地址冲成空串。
        var next = readDavCache();
        if (url) next.baseURL = url;
        if (user) next.username = user;
        next.pathPrefix = dir;
        writeDavCache(next);
        // 不调 form.reset()：它只回退到渲染时的旧值。交给 loadBackup 重新拉状态并
        // 重建表单，这样新地址/目录会立刻回填，用户能确认"确实保存进去了"。
        return loadBackup().then(function () { return '凭据已保存'; });
      });
    });
  }

  function findRecord(id) {
    var all = S.items.concat(S.reimb.items);
    for (var i = 0; i < all.length; i++) if (all[i].id === id) return all[i];
    return null;
  }

  function runTask(label, fn) {
    if (S.busy) { toast('还有操作正在进行，请稍候', 'warn'); return; }
    S.busy = true;
    toast(label + '中…', 'info');
    return Promise.resolve().then(fn).then(function (msg) {
      ding();
      toast(msg || (label + '成功'), 'success');
    }).catch(function (error) {
      toast(label + '失败：' + msgOf(error), 'error');
    }).then(function () { S.busy = false; });
  }

  /* ============================ 9. 账单表单 ============================ */

  function openRecordForm(record) {
    var editing = !!record;
    var form = {
      id: editing ? record.id : '',
      type: editing ? record.type : 'expense',
      amount: editing ? fmtCents(centsOf(record)) : '',
      categoryId: editing ? (record.categoryId || '') : '',
      note: editing ? (record.note || '') : '',
      at: localInputFrom(editing ? (record.transactionAt || record.occurredOn) : ''),
      reimb: editing ? (record.reimbursementStatus || 'none') : 'none'
    };
    releaseThumbs();
    S.formFiles = [];
    S.formAttachments = [];

    MD.ui.sheet({
      title: editing ? '编辑账单' : '记一笔',
      body: function (el) {
        var box = MD.ui.el('div', { html: formHtml(form, editing) });
        el.appendChild(box);
        bindFormEvents(box, form, editing);
        if (editing) loadFormAttachments(box, record.id);
      },
      actions: [
        { label: '取消', kind: 'ghost' },
        {
          label: editing ? '保存' : '新增',
          kind: 'primary',
          onClick: function () { return submitForm(form, editing); }
        }
      ]
    });
  }

  function formHtml(form, editing) {
    var cats = S.categories[form.type] || [];
    return '<div class="ldg-form">' +
      '<div class="field"><span class="ldg-flabel">类型</span><div class="ldg-chips">' +
      ['expense', 'income'].map(function (t) {
        return '<button class="chip' + (form.type === t ? ' chip--active' : '') + '" type="button" data-fact="type" data-value="' + t + '">' + TYPE_TEXT[t] + '</button>';
      }).join('') + '</div></div>' +

      '<div class="field"><span class="ldg-flabel">金额（元）</span>' +
      '<input class="input" id="ldg-f-amount" type="text" inputmode="decimal" placeholder="0.00" value="' + esc(form.amount) + '"></div>' +

      '<div class="field"><span class="ldg-flabel">分类</span>' +
      '<div class="ldg-catpick" id="ldg-f-cats">' + catChips(cats, form.categoryId) + '</div>' +
      // 后端要求 categoryName 非空（缺了直接 400「请选择分类」），
      // 所以这里不能说「可先按无分类保存」——那会把用户引到一个必然失败的提交上。
      (cats.length ? '' : '<p class="muted" style="font-size:12px;margin:0">分类加载失败，请退出重进后再试。</p>') +
      '</div>' +

      '<div class="field"><span class="ldg-flabel">备注</span>' +
      '<input class="input" id="ldg-f-note" type="text" maxlength="80" placeholder="买了什么" value="' + esc(form.note) + '"></div>' +

      '<div class="ldg-row2">' +
      '<div class="field"><span class="ldg-flabel">发生时间</span>' +
      '<input class="input" id="ldg-f-at" type="datetime-local" value="' + esc(form.at) + '"></div>' +
      '<div class="field"' + (form.type === 'income' ? ' hidden' : '') + ' id="ldg-f-reimb-box"><span class="ldg-flabel">报销状态</span>' +
      '<select class="select" id="ldg-f-reimb">' +
      ['none', 'pending', 'done'].map(function (t) {
        return '<option value="' + t + '"' + (form.reimb === t ? ' selected' : '') + '>' + REIMB_TEXT[t] + '</option>';
      }).join('') + '</select></div>' +
      '</div>' +

      (editing
        ? '<div class="field"><span class="ldg-flabel">凭证（JPG / PNG / WebP，单张 ≤ 2MB）</span>' +
        '<div class="ldg-strip">' +
        '<button class="btn btn-ghost" type="button" data-fact="att-pick" style="min-height:44px">' + iconSvg('upload', 16) + '选择图片</button>' +
        '<span class="muted" style="font-size:12px" id="ldg-f-att-hint"></span>' +
        '</div>' +
        '<input type="file" id="ldg-f-file" accept="image/jpeg,image/png,image/webp" multiple hidden>' +
        '<div id="ldg-f-atts"></div></div>'
        : '<p class="muted" style="font-size:12px;margin:0">新增模式下可在保存后立即补充凭证。</p>') +
      '</div>';
  }

  function catChips(cats, activeId) {
    if (!cats.length) return '<span class="muted" style="font-size:12px">暂无分类</span>';
    return cats.map(function (c) {
      return '<button class="chip' + (c.id === activeId ? ' chip--active' : '') + '" type="button" data-fact="cat" data-id="' + esc(c.id) + '">' + esc(c.name) + '</button>';
    }).join('');
  }

  function bindFormEvents(box, form, editing) {
    box.addEventListener('click', function (event) {
      var node = event.target && event.target.closest ? event.target.closest('[data-fact]') : null;
      if (!node) return;
      var fact = node.getAttribute('data-fact');
      if (fact === 'type') {
        form.type = node.getAttribute('data-value') || 'expense';
        form.categoryId = '';
        repaintForm(box, form, editing);
        return;
      }
      if (fact === 'cat') {
        var id = node.getAttribute('data-id') || '';
        form.categoryId = form.categoryId === id ? '' : id;
        [].slice.call(box.querySelectorAll('[data-fact="cat"]')).forEach(function (btn) {
          btn.classList.toggle('chip--active', btn.getAttribute('data-id') === form.categoryId);
        });
        return;
      }
      if (fact === 'att-pick') {
        var file = box.querySelector('#ldg-f-file');
        if (file) file.click();
        return;
      }
      if (fact === 'att-view') {
        viewAttachment(node.getAttribute('data-id'));
        return;
      }
      if (fact === 'att-del') {
        removeAttachment(box, form, node.getAttribute('data-id'));
        return;
      }
    });

    box.addEventListener('change', function (event) {
      if (event.target && event.target.id === 'ldg-f-reimb') form.reimb = event.target.value || 'none';
      if (event.target && event.target.id === 'ldg-f-file') {
        var files = [].slice.call(event.target.files || []);
        event.target.value = '';
        uploadFiles(box, form, files);
      }
    });
  }

  /** 重画表单（切换收支类型时重新拉取该类型分类）。 */
  function repaintForm(box, form, editing) {
    form.amount = val(box, '#ldg-f-amount');
    form.note = val(box, '#ldg-f-note');
    form.at = val(box, '#ldg-f-at');
    var sel = box.querySelector('#ldg-f-reimb');
    if (sel) form.reimb = sel.value || 'none';
    box.innerHTML = formHtml(form, editing);
    paintAttachmentList(box);
  }

  function val(box, selector) {
    var node = box.querySelector(selector);
    return node ? String(node.value || '') : '';
  }

  /**
   * 校验失败后把视图定位到第一个出问题的字段。
   * 提示看得到却找不到位置，体验和没提示一样——分类是多选 chip，不在首屏时尤其明显。
   */
  function focusProblemField(box, problems) {
    var joined = (problems || []).join('；');
    var selector = joined.indexOf('金额') >= 0 ? '#ldg-f-amount'
      : joined.indexOf('分类') >= 0 ? '#ldg-f-cats'
        : joined.indexOf('时间') >= 0 ? '#ldg-f-at' : '';
    var target = selector ? box.querySelector(selector) : null;
    if (!target) return;
    if (typeof target.scrollIntoView === 'function') {
      try { target.scrollIntoView({ block: 'center', behavior: 'smooth' }); } catch (e) { target.scrollIntoView(false); }
    }
    var input = target.tagName === 'INPUT' ? target : target.querySelector('input');
    if (input && typeof input.focus === 'function') {
      try { input.focus({ preventScroll: true }); } catch (e) { input.focus(); }
    }
  }

  function submitForm(form, editing) {
    var box = doc.querySelector('.sheet__body');
    if (!box) return Promise.reject(new Error('表单已关闭'));
    form.amount = val(box, '#ldg-f-amount');
    form.note = val(box, '#ldg-f-note');
    form.at = val(box, '#ldg-f-at');
    var sel = box.querySelector('#ldg-f-reimb');
    if (sel) form.reimb = sel.value || 'none';

    // 一次性收集所有必填项问题，而不是「修一个再报下一个」。
    // 分类此前完全没有前端校验，直接发空值给后端兜底 400「请选择分类」：
    // 用户先补金额 → 再被后端退一次才能选分类，往返两轮，且两处提示级别不一致
    // （前端 toast warn / 后端 error）。这里统一到提交前、同一级别提示。
    var problems = [];
    var parsed = parseAmount(form.amount);
    if (!parsed.ok) problems.push(parsed.msg);
    // 分类接口不可用时（降级态）不强制要求，否则用户完全无法记账。
    var hasCats = (S.categories[form.type] || []).length > 0;
    if (hasCats && !form.categoryId) problems.push('请选择分类');
    var transactionAt = isoFromLocal(form.at);
    if (!transactionAt) problems.push('请选择发生时间');

    if (problems.length) {
      toast(problems.join('；'), 'warn');
      focusProblemField(box, problems);
      // false = 保持表单打开：已填的金额、备注、时间全部保留，补选分类后可直接再提交。
      return Promise.resolve(false);
    }

    var payload = {
      type: form.type,
      amount: fmtCents(parsed.cents),
      categoryId: form.categoryId || '',
      note: form.note,
      transactionAt: transactionAt,
      reimbursementStatus: form.type === 'income' ? 'none' : form.reimb
    };

    return runTask(editing ? '修改' : '新增', function () {
      var req = editing
        ? ledgerApi('/ledger/records/' + encodeURIComponent(form.id), { method: 'PATCH', body: payload })
        : ledgerApi('/ledger/records', { method: 'POST', body: payload });
      return req.then(function (res) {
        var newId = editing ? form.id : (res && (res.id || (res.record && res.record.id)));
        // 新增时先把暂存的图片补传上去（此刻才拿到 recordId）
        if (!editing && newId && S.formFiles.length) return uploadAll(newId).then(function () { return '已新增'; });
        return editing ? '已保存' : '已新增';
      }).then(function (msg) {
        reloadAll();
        // 新增/编辑会改变报销页内容（新记录默认 pending）：在报销页就立刻重拉，
        // 在别的页只置脏、切过去时再拉，避免每次记账都白打一次报销列表请求。
        if (S.tab === 'reimb') {
          S.reimb.dirty = false;
          loadReimbursements();
        } else {
          S.reimb.dirty = true;
        }
        return msg;
      });
    }).then(function () { return true; });
  }

  /* ---------- 凭证 ---------- */

  function attHost(box) { return box.querySelector('#ldg-f-atts'); }

  function loadFormAttachments(box, recordId) {
    var hint = box.querySelector('#ldg-f-att-hint');
    if (hint) hint.textContent = '正在读取…';
    // 契约仅有 GET /attachments/:id（单个），缺少按账单列举的入口 →
    // 这里尝试约定俗成的查询形态，404 时不影响表单其它功能。
    ledgerApi('/ledger/attachments?recordId=' + enc(recordId)).then(function (res) {
      var list = (res && (res.items || res.attachments)) || (Array.isArray(res) ? res : []);
      S.formAttachments = (list || []).map(function (a) {
        return { id: String(a.id), name: String(a.name || a.filename || '凭证'), bytes: Number(a.bytes || 0) };
      });
      paintAttachmentList(box);
      if (hint) hint.textContent = S.formAttachments.length ? ('已有 ' + S.formAttachments.length + ' 张') : '暂无凭证';
    }).catch(function (error) {
      if (isNotDeployed(error)) {
        if (hint) hint.textContent = '凭证接口尚未启用';
      } else if (hint) {
        hint.textContent = '凭证读取失败';
      }
    });
  }

  function paintAttachmentList(box) {
    var host = attHost(box);
    if (!host) return;
    if (!S.formAttachments.length) { host.innerHTML = ''; return; }
    host.innerHTML = S.formAttachments.map(function (a) {
      return '<div class="ldg-att">' +
        '<button class="ldg-att__thumb" type="button" data-fact="att-view" data-id="' + esc(a.id) + '" aria-label="预览凭证">' +
        '<img data-thumb="' + esc(a.id) + '" alt="凭证缩略图"' + (a.url ? ' src="' + esc(a.url) + '"' : '') + '>' +
        '</button>' +
        '<div style="flex:1 1 auto;min-width:0">' +
        '<div style="font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(a.name) + '</div>' +
        '<div class="muted" style="font-size:12px">' + (a.bytes ? (Math.round(a.bytes / 1024) + ' KB') : '—') + '</div>' +
        '</div>' +
        '<button class="btn-icon" type="button" data-fact="att-del" data-id="' + esc(a.id) + '" aria-label="删除凭证">' + iconSvg('trash', 18) + '</button>' +
        '</div>';
    }).join('');
    // 已取到 objectURL 的不再重复下载：表单每次重绘都重新拉一遍会把缩略图流量放大。
    S.formAttachments.forEach(function (a) { if (!a.url) renderThumb(host, a.id); });
  }

  function findAttachment(id) {
    var list = S.formAttachments;
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  /** 凭证是私有资源，必须带 Authorization 拉取后再转 objectURL，不能直接塞进 <img src>。 */
  function renderThumb(host, id) {
    var img = host.querySelector('[data-thumb="' + id + '"]');
    if (!img || !global.URL || !global.URL.createObjectURL) return;
    ledgerApi('/ledger/attachments/' + encodeURIComponent(id), { raw: true })
      .then(function (res) { return res.blob ? res.blob() : null; })
      .then(function (blob) {
        if (!blob || !host.isConnected) return;
        var url = global.URL.createObjectURL(blob);
        var att = findAttachment(id);
        if (att) att.url = url;
        img.src = url;
      })
      .catch(function () { /* 缩略图失败不打扰主流程 */ });
  }

  /** 预览：拿到 blob 后在新的标签页打开原图（blob URL 无法被服务端鉴权，故不复用于 <img src> 之外）。 */
  function viewAttachment(id) {
    var att = findAttachment(id);
    if (att && att.url) { openBlob(att.url); return; }
    ledgerApi('/ledger/attachments/' + encodeURIComponent(id), { raw: true })
      .then(function (res) { return res.blob ? res.blob() : null; })
      .then(function (blob) {
        if (!blob) { toast('凭证预览失败', 'error'); return; }
        var url = global.URL.createObjectURL(blob);
        if (att) att.url = url;
        openBlob(url);
      })
      .catch(function () { toast('凭证预览失败，请稍后重试', 'error'); });
  }

  function openBlob(url) {
    var opened = global.open ? global.open(url, '_blank') : null;
    if (opened) return;
    var a = doc.createElement('a');
    a.href = url;
    a.target = '_blank';
    a.rel = 'noopener';
    a.style.display = 'none';
    doc.body.appendChild(a);
    a.click();
    doc.body.removeChild(a);
  }

  /** 回收 blob URL：表单反复开关会不断创建对象 URL，不回收等于持续占内存。 */
  function releaseThumbs() {
    if (!S.formAttachments.length) return;
    S.formAttachments.forEach(function (a) { releaseThumb(a.id); });
  }

  function releaseThumb(id) {
    var att = findAttachment(id);
    if (!att || !att.url) return;
    if (global.URL && global.URL.revokeObjectURL) {
      try { global.URL.revokeObjectURL(att.url); } catch (_) { /* 忽略回收失败 */ }
    }
    delete att.url;
  }

  function pickValidFiles(files) {
    var okFiles = [];
    files.forEach(function (file) {
      var name = String(file.name || '');
      var ext = (name.split('.').pop() || '').toLowerCase();
      if (IMAGE_EXT.indexOf(ext) < 0) { toast('不支持的文件格式：' + name, 'error'); return; }
      if (file.size && file.size > MAX_IMAGE_BYTES) { toast(name + ' 超过 2MB', 'error'); return; }
      okFiles.push(file);
    });
    return okFiles;
  }

  function uploadFiles(box, form, rawFiles) {
    var files = pickValidFiles(rawFiles || []);
    if (!files.length) return;
    if (!form.id) {
      // 新增尚未保存：先暂存，保存成功后统一上传
      S.formFiles = S.formFiles.concat(files);
      toast('已暂存 ' + files.length + ' 张，保存后自动上传', 'info');
      return;
    }
    runTask('凭证上传', function () { return uploadAll(form.id); })
      .then(function () { loadFormAttachments(box, form.id); });
  }

  function uploadAll(recordId) {
    var queue = S.formFiles.slice();
    S.formFiles = [];
    if (!queue.length) return Promise.resolve(0);
    return queue.reduce(function (chain, file) {
      return chain.then(function (count) {
        var fd = new FormData();
        fd.append('file', file);
        fd.append('recordId', recordId);
        return ledgerApi('/ledger/attachments', { method: 'POST', body: fd }).then(function () { return count + 1; });
      });
    }, Promise.resolve(0));
  }

  function removeAttachment(box, form, id) {
    if (!id) return;
    MD.ui.confirm({
      title: '删除凭证',
      message: '删除后无法恢复，确定删除这张凭证？',
      confirmText: '删除',
      danger: true
    }).then(function (yes) {
      if (!yes) return;
      runTask('删除凭证', function () {
        return ledgerApi('/ledger/attachments/' + encodeURIComponent(id), { method: 'DELETE' }).then(function () {
          releaseThumb(id);
          S.formAttachments = S.formAttachments.filter(function (a) { return a.id !== id; });
          paintAttachmentList(box);
          reloadAll();
          return '凭证已删除';
        });
      });
    });
  }

  function confirmDelete(id) {
    var rec = findRecord(id);
    var label = rec ? (rec.note || rec.categoryName || '这笔账单') : '这笔账单';
    MD.ui.confirm({
      title: '删除账单',
      message: '确定删除「' + label + '」吗？删除后不可恢复，关联凭证也会一并删除。',
      confirmText: '删除',
      danger: true
    }).then(function (yes) {
      if (!yes) return;
      runTask('删除', function () {
        return ledgerApi('/ledger/records/' + encodeURIComponent(id), { method: 'DELETE' }).then(function () {
          reloadAll();
          if (S.tab === 'reimb') loadReimbursements();
          return '账单已删除';
        });
      });
    });
  }

  /* ============================ 10. 分类多选 ============================ */

  function openCategorySheet() {
    loadCategories().then(function () {
      var picked = S.filters.categoryIds.slice();
      var container = MD.ui.el('div', { class: 'stack' });
      MD.ui.sheet({
        title: '选择分类',
        body: function (el) {
          el.appendChild(container);
          paintCatSheet(container, picked);
          var add = MD.ui.el('div', { class: 'ldg-frow', style: { marginTop: '12px' } }, [
            MD.ui.el('input', { class: 'input', id: 'ldg-new-cat', type: 'text', placeholder: '新建分类名', style: { flex: '1 1 160px' } }),
            MD.ui.el('select', { class: 'select', id: 'ldg-new-kind', style: { flex: '0 0 96px' } }, [
              MD.ui.el('option', { value: 'expense', text: '支出' }),
              MD.ui.el('option', { value: 'income', text: '收入' })
            ]),
            MD.ui.el('button', {
              class: 'btn btn-ghost', type: 'button', style: { minHeight: '44px' },
              onClick: function () {
                var input = doc.getElementById('ldg-new-cat');
                var kind = doc.getElementById('ldg-new-kind');
                var name = input ? String(input.value || '').trim() : '';
                if (!name) { toast('请输入分类名', 'warn'); return; }
                runTask('新建分类', function () {
                  return ledgerApi('/ledger/categories', { method: 'POST', body: { kind: kind ? kind.value : 'expense', name: name } })
                    .then(function () {
                      S.catsLoaded = false;
                      if (input) input.value = '';
                      return loadCategories().then(function () {
                        paintCatSheet(container, picked);
                        return '分类已创建';
                      });
                    });
                });
              }
            }, MD.icon ? MD.icon('plus') : null)
          ]);
          add.lastChild.appendChild(doc.createTextNode('新建'));
          el.appendChild(add);
        },
        actions: [
          { label: '清空', kind: 'ghost', onClick: function () { picked = []; paintCatSheet(container, picked); } },
          {
            label: '应用',
            kind: 'primary',
            onClick: function () {
              S.filters.categoryIds = picked.slice();
              paintFilters();
              loadRecords(true).then(loadBar);
            }
          }
        ]
      });

      function paintCatSheet(host, selected) {
        MD.ui.clear(host);
        ['expense', 'income'].forEach(function (kind) {
          var cats = S.categories[kind] || [];
          if (!cats.length) return;
          host.appendChild(MD.ui.el('div', { class: 'fam-title', text: TYPE_TEXT[kind] + '分类' }));
          var wrap = MD.ui.el('div', { class: 'ldg-catpick' });
          cats.forEach(function (c) {
            var on = selected.indexOf(c.id) >= 0;
            wrap.appendChild(MD.ui.el('button', {
              class: 'chip' + (on ? ' chip--active' : ''),
              type: 'button',
              dataset: { cat: c.id },
              onClick: function () {
                var i = selected.indexOf(c.id);
                if (i >= 0) selected.splice(i, 1); else selected.push(c.id);
                paintCatSheet(host, selected);
              }
            }, c.name));
          });
          host.appendChild(wrap);
        });
      }
    });
  }

  /* ============================ 11. 报销批量操作 ============================ */

  /**
   * 批量/单条改报销状态。ids 省略时取勾选项。
   * 刷新后沿用当前筛选（默认 all），该记录仍在列表里，只是徽标变了 —— 列表不会「清空」。
   */
  function markReimbursements(status, ids) {
    var targets = (ids && ids.length ? ids : selectedIds()).filter(function (id) { return !!id; });
    if (!targets.length) { toast('请先勾选账单', 'warn'); return Promise.resolve(); }
    var label = status === 'done' ? '标记已报销' : (status === 'none' ? '标记不涉及' : '退回待报销');
    return runTask(label, function () {
      return ledgerApi('/ledger/reimbursements/mark', { method: 'POST', body: { ids: targets, status: status } })
        .then(function (res) {
          return loadReimbursements().then(function () {
            reloadAll();
            var n = res && typeof res.updated === 'number' ? res.updated : targets.length;
            return (res && res.message) || (label + '：已更新 ' + n + ' 条');
          });
        });
    });
  }

  function exportSelectedReimbursements() {
    var ids = selectedIds();
    if (!ids.length) { toast('请先勾选账单', 'warn'); return; }
    // 导出只是把勾选项打包下载，不能顺手改状态 —— 状态变更只有「标记已报销」负责。
    runTask('导出', function () {
      return ledgerApi('/ledger/export/html', {
        method: 'POST',
        body: { ids: ids, title: '报销清单' },
        raw: true
      }).then(function (res) {
        return saveResponse(res, '报销清单', 'html');
      }).then(function () { return '报销清单已导出'; });
    });
  }

  /* ============================ 12. 导出 / 导入 ============================ */

  /** 导出响应落盘：缺 blob 视为服务端没给文件，而不是假装成功。 */
  function saveResponse(res, baseName, ext) {
    return (res && typeof res.blob === 'function' ? res.blob() : Promise.resolve(null)).then(function (blob) {
      if (!blob) throw new Error('服务端未返回文件内容');
      return saveBlob(blob, fileNameOf(res, baseName, ext));
    });
  }

  /**
   * 下载导出文件。时间范围走 ExportFilter：
   *   HTML/ZIP 是 POST，范围放请求体；JSON 是 GET，范围放查询串（后端 bindExportFilter 两条路都支持）。
   */
  function downloadExport(path, ext, baseName) {
    var filter = exportFilter();
    if (!filter) return;
    var opts = { raw: true };
    if (ext === 'json') {
      path += qs({ preset: filter.preset, from: filter.from, to: filter.to });
    } else {
      opts.method = 'POST';
      opts.body = filter;
    }
    runTask('导出', function () {
      return ledgerApi(path, opts).then(function (res) {
        return saveResponse(res, baseName, ext);
      }).then(function () { return '导出成功（' + describeRange() + '）'; });
    });
  }

  function fileNameOf(res, baseName, ext) {
    var cd = res && res.headers && res.headers.get ? res.headers.get('Content-Disposition') : '';
    var m = cd && cd.match(/filename\*?=(?:UTF-8''|"?)([^";]+)"?/i);
    if (m && m[1]) {
      try { return decodeURIComponent(m[1]); } catch (_) { return m[1]; }
    }
    return baseName + '-' + dateStr(new Date()) + '.' + ext;
  }

  function saveBlob(blob, filename) {
    if (!global.URL || !global.URL.createObjectURL) throw new Error('当前环境不支持文件下载');
    var url = global.URL.createObjectURL(blob);
    var a = doc.createElement('a');
    a.href = url;
    a.download = filename;
    a.style.display = 'none';
    doc.body.appendChild(a);
    a.click();
    doc.body.removeChild(a);
    global.setTimeout(function () {
      try { global.URL.revokeObjectURL(url); } catch (_) { /* 忽略回收失败 */ }
    }, 4000);
    return filename;
  }

  function runImport(mode) {
    var fileNode = doc.getElementById('ldg-import-file');
    var file = fileNode && fileNode.files && fileNode.files[0];
    if (!file) { toast('请先选择 JSON 备份文件', 'warn'); return; }
    if (!global.FileReader) { toast('当前环境不支持读取本地文件', 'error'); return; }

    var go = function () {
      var reader = new global.FileReader();
      reader.onerror = function () { toast('文件读取失败', 'error'); };
      reader.onload = function () {
        var parsed;
        try { parsed = JSON.parse(String(reader.result)); }
        catch (error) { toast('不是合法的 JSON 文件', 'error'); return; }
        runTask('导入', function () {
          return ledgerApi('/ledger/import', {
            method: 'POST',
            body: { mode: mode, payload: parsed }
          }).then(function (res) {
            S.catsLoaded = false;
            reloadAll();
            return (res && res.message) || (mode === 'overwrite' ? '已覆盖导入' : '已合并导入');
          });
        });
      };
      reader.readAsText(file);
    };

    if (mode === 'overwrite') {
      MD.ui.confirm({
        title: '覆盖导入',
        message: '覆盖导入会清空当前全部账单与分类并替换为备份内容。服务端会先自动备份现有数据，但仍建议确认备份已下载。确定继续？',
        confirmText: '覆盖导入',
        danger: true
      }).then(function (yes) { if (yes) go(); });
      return;
    }
    MD.ui.confirm({
      title: '合并导入',
      message: '合并导入会按 id 跳过已存在的账单，只补充备份中缺少的数据。确定继续？',
      confirmText: '合并导入'
    }).then(function (yes) { if (yes) go(); });
  }

  /* ============================ 13. 对外接口与路由注册 ============================ */

  function register() {
    if (!MD.router || typeof MD.router.register !== 'function') return;
    MD.router.register({
      id: 'ledger',
      title: '记账本',
      subtitle: '收支记账与报销',
      icon: 'wallet',
      group: '更多',
      // 侧栏第 4 位：排在「文件存储」之后、「日程待办」之前。
      order: 4,
      // 底部栏独立排序：侧栏里「设置」排最后，但底部栏仍需「设置」在第 3 位，
      // 故这里显式声明 tabOrder，避免随 order 一起被挪走。
      tabOrder: 4,
      comingSoon: false,
      render: render
    });
  }

  ledger.register = register;
  ledger.reload = reloadAll;
  ledger._state = S;

  injectStyle();
  if (MD.router && typeof MD.router.register === 'function') register();
})(typeof window !== 'undefined' ? window : this);

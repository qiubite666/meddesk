/* ==========================================================================
 * export.js —— MedDesk 导出模块（A8）
 *
 * 依赖（只读调用，不修改其文件）：
 *   A6：MD.data（profiles/meds/intakes）、MD.api.postExport、MD.util
 *   A8：med.js 仅通过 MD.export.rows/csv/pdf 调用本模块
 *
 * 隐私说明：MedDesk 为端到端加密，服务端只能看到密文；因此导出由客户端把
 * 已解密数据一次性 POST 给服务端，**仅在内存中渲染，不落盘、不写日志**。
 * CSV 则完全在本机生成，不经过网络。
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};
  var exp = MD['export'] = MD['export'] || {};

  // 状态中文映射（与服务端 internal/export 保持一致）
  var STATUS_CN = {
    taken: '已服用',
    skipped: '已跳过',
    snoozed: '已推迟',
    pending: '待服用',
    missed: '已漏服'
  };
  var HEADER = ['日期', '时间', '成员', '药物', '剂量', '状态', '备注'];

  /* ============================ 工具 ============================ */

  function pad(n) { return (n < 10 ? '0' : '') + n; }

  function dayKey(ms) {
    if (MD.util && typeof MD.util.dayKey === 'function') return MD.util.dayKey(ms);
    var d = new Date(ms);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }

  function timeKey(ms) {
    var d = new Date(ms);
    return pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  /** 入参 from/to 支持毫秒时间戳或 'YYYY-MM-DD' 字符串 */
  function toMs(v, fallback) {
    if (v == null || v === '') return fallback;
    if (typeof v === 'number' && isFinite(v)) return v;
    if (typeof v === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(v)) return new Date(v + 'T00:00:00').getTime();
    var n = Number(v);
    return isFinite(n) ? n : fallback;
  }

  function startOfDay(ms) {
    if (MD.util && typeof MD.util.startOfDay === 'function') return MD.util.startOfDay(ms);
    var d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime();
  }

  function tz() {
    try {
      if (global.Intl && Intl.DateTimeFormat) {
        return Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai';
      }
    } catch (e) { /* 忽略 */ }
    return 'Asia/Shanghai';
  }

  function legacyDownload(blob, filename) {
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 4000);
  }

  // 原生环境（Capacitor WebView）里 <a download> 没有任何组件接管，点击后静默失败；
  // 故改走 MD.exportShare 的系统分享面板，浏览器行为保持原样。
  function download(blob, filename) {
    if (window.MD && MD.exportShare && MD.exportShare.isNative()) {
      MD.exportShare.saveBlob(blob, filename).catch(function () { legacyDownload(blob, filename); });
      return;
    }
    legacyDownload(blob, filename);
  }

  function msgOf(e) {
    if (!e) return '未知错误';
    return e.message || e.msg || (typeof e === 'string' ? e : '未知错误');
  }

  /** 漏服判定：计划时间已过 1 小时仍为 pending */
  function normalizeStatus(it, now) {
    var st = it.status || 'pending';
    if (st === 'pending' && (it.scheduledFor || 0) + 3600000 < now) return 'missed';
    return st;
  }

  /* ============================ 1. 组装导出行 ============================ */

  /**
   * 读取指定时间范围与档案的服药记录，组装为 ExportRow[]。
   * @param {{from:number|string, to:number|string, profileId?:string|null}} req
   * @returns {Promise<Array<{date,time,profile,medication,dosage,status,note}>>}
   */
  function rows(req) {
    req = req || {};
    var now = MD.util && MD.util.nowMs ? MD.util.nowMs() : Date.now();
    var from = toMs(req.from, startOfDay(now));
    var to = toMs(req.to, startOfDay(now) + 86400000);
    if (to <= from) to = from + 86400000;
    var pid = req.profileId || null;

    return Promise.all([
      MD.data.intakes.list(),
      MD.data.meds.list(),
      MD.data.profiles.list()
    ]).then(function (r) {
      var intakes = (r[0] || []).filter(function (x) { return x && !x.deleted; });
      var medMap = {}, profMap = {};
      (r[1] || []).forEach(function (m) { if (m && !m.deleted) medMap[m.id] = m; });
      (r[2] || []).forEach(function (p) { if (p && !p.deleted) profMap[p.id] = p; });

      return intakes.filter(function (i) {
        var t = i.scheduledFor || 0;
        if (t < from || t >= to) return false;
        if (!pid) return true;
        return i.profileId === pid || (medMap[i.medicationId] && medMap[i.medicationId].profileId === pid);
      }).sort(function (a, b) { return (a.scheduledFor || 0) - (b.scheduledFor || 0); })
        .map(function (i) {
          var med = medMap[i.medicationId];
          var prof = profMap[i.profileId] || (med ? profMap[med.profileId] : null);
          return {
            date: dayKey(i.scheduledFor),
            time: timeKey(i.scheduledFor),
            profile: prof ? prof.name : '',
            medication: med ? med.name : '（已删除药物）',
            dosage: i.doseText || (med ? (med.dosageAmount != null ? med.dosageAmount + ' ' + (med.dosageUnit || '') : '') : ''),
            status: normalizeStatus(i, now),
            note: i.note || ''
          };
        });
    });
  }

  /* ============================ 2. 请求体与统计 ============================ */

  function statsOf(rowList) {
    var s = { total: rowList.length, taken: 0, skipped: 0, missed: 0, adherence: 0 };
    rowList.forEach(function (r) {
      if (r.status === 'taken') s.taken++;
      else if (r.status === 'skipped') s.skipped++;
      else if (r.status === 'missed') s.missed++;
    });
    s.adherence = s.total ? s.taken / s.total : 0;
    return s;
  }

  /**
   * 组装服务端导出请求体（model.ExportRequest）。
   * @param {Array} rowList ExportRow[]
   * @param {{title?,profile?,rangeFrom?,rangeTo?}} meta 可选元信息
   * @param {string} format 'csv' | 'pdf'
   */
  function buildRequest(rowList, meta, format) {
    meta = meta || {};
    var list = rowList || [];
    var from = meta.rangeFrom || (list.length ? list[0].date : dayKey(Date.now()));
    var to = meta.rangeTo || (list.length ? list[list.length - 1].date : from);
    return {
      format: format,
      title: meta.title || ('用药记录 ' + from + ' 至 ' + to),
      profile: meta.profile || '',
      rangeFrom: from,
      rangeTo: to,
      timezone: tz(),
      rows: list,
      stats: statsOf(list)
    };
  }

  /* ============================ 3. CSV（纯前端生成） ============================ */

  function csvCell(v) {
    var s = (v == null ? '' : String(v));
    if (/[",\r\n]/.test(s)) s = '"' + s.replace(/"/g, '""') + '"';
    return s;
  }

  function csvText(rowList) {
    var lines = [HEADER.map(csvCell).join(',')];
    (rowList || []).forEach(function (r) {
      lines.push([
        r.date, r.time, r.profile, r.medication, r.dosage,
        STATUS_CN[r.status] || r.status, r.note
      ].map(csvCell).join(','));
    });
    return lines.join('\r\n');
  }

  /**
   * 本机生成 CSV（UTF-8 BOM + CRLF）并触发下载。
   * @param {Array} rowList ExportRow[]
   * @param {{title?,profile?,rangeFrom?,rangeTo?}} [meta]
   */
  function csv(rowList, meta) {
    var req = buildRequest(rowList, meta, 'csv');
    if (!req.rows.length) throw new Error('没有可导出的记录');
    // BOM 保证 Excel 正确识别 UTF-8；CRLF 与服务端 CSV 保持一致
    var blob = new Blob(['\ufeff' + csvText(req.rows)], { type: 'text/csv;charset=utf-8' });
    download(blob, filename(req, 'csv'));
    return Promise.resolve(true);
  }

  /* ============================ 4. PDF（走服务端渲染） ============================ */

  /**
   * 提交服务端渲染 PDF 并下载。
   * @param {Array} rowList ExportRow[]
   * @param {{title?,profile?,rangeFrom?,rangeTo?}} [meta]
   * @returns {Promise<boolean>}
   */
  function pdf(rowList, meta) {
    var req = buildRequest(rowList, meta, 'pdf');
    if (!req.rows.length) return Promise.reject(new Error('没有可导出的记录'));
    if (!MD.api || typeof MD.api.postExport !== 'function') {
      return Promise.reject(new Error('服务端导出接口不可用'));
    }
    return Promise.resolve(MD.api.postExport('pdf', req))
      .then(function (blob) {
        if (!blob) throw new Error('服务端返回空文件');
        download(blob, filename(req, 'pdf'));
        return true;
      })
      .catch(function (e) {
        // 服务端最常见的失败原因是缺少 CJK 字体（apierr.CodeUnavailable）
        throw new Error('服务端未安装中文字体，请先安装 CJK 字体或配置 MEDDESK_PDF_FONT_PATH（' + msgOf(e) + '）');
      });
  }

  function filename(req, ext) {
    return 'meddesk-用药记录-' + req.rangeFrom + '-' + req.rangeTo + '.' + ext;
  }

  /* ============================ 5. 对外接口 ============================ */

  exp.rows = rows;
  exp.csv = csv;
  exp.pdf = pdf;
  exp.buildRequest = buildRequest;
  exp.statusText = function (s) { return STATUS_CN[s] || s; };
})(typeof window !== 'undefined' ? window : this);

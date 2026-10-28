/* ==========================================================================
 * export-share.js —— 导出的原生分享降级（Contract C）
 *
 * 背景：浏览器里导出的最后一步是 URL.createObjectURL + <a download>.click()，
 * 而在 Capacitor 的 WebView 中没有任何组件接管这个动作，结果是静默失败——
 * 用户点了导出，既没下载也没分享，界面上完全无反馈。因此原生环境改为：
 * 先把文件写进应用私有的 Documents/MedDesk/ 目录，再交给系统分享面板。
 *
 * 依赖（只读，不修改其文件）：
 *   core.js：MD.toast.show（失败提示，不存在则静默忽略）
 *   export.js：只被其 download() 调用，本模块不反向依赖 export.js
 *
 * 约束：
 *   - 零构建步骤：只读 window.Capacitor，不 require/import 任何 npm 包
 *   - 非原生环境必须完全等价于原有浏览器下载，不引入任何可见行为差异
 *   - 原生链路任何一步失败都退回浏览器下载路径，且不向调用方抛异常
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};
  var exportShare = MD.exportShare = MD.exportShare || {};

  // 统一放在 Documents/MedDesk/ 下：Android 10+ 分区存储只允许通过 FileProvider
  // 暴露私有目录文件，getUri() 才能拿到可供分享面板读取的 content:// uri。
  var SUB_DIR = 'MedDesk';
  // Capacitor Filesystem 的 Directory.Documents 在桥接层对应的字符串值。
  var DIRECTORY_DOCUMENTS = 'DOCUMENTS';

  /** 取到可用的 Capacitor 根对象；无插件注册表时返回 null */
  function capacitor() {
    return (global.Capacitor && global.Capacitor.Plugins) ? global.Capacitor : null;
  }

  function isNative() {
    var cap = capacitor();
    if (!cap) return false;
    if (!cap.Plugins.Filesystem || !cap.Plugins.Share) return false;
    // isNativePlatform() 存在时以它为准：插件齐全但仍跑在普通浏览器里
    // （如 cap serve 调试）时，分享面板并不存在，必须继续走浏览器下载。
    if (typeof cap.isNativePlatform === 'function') return cap.isNativePlatform() === true;
    return true;
  }

  /** 失败提示：toast 尚未加载或不可用时静默忽略，不能因此中断导出 */
  function toast(message, kind) {
    try {
      if (MD.toast && typeof MD.toast.show === 'function') MD.toast.show(message, kind || 'error');
    } catch (e) { /* 提示只是辅助，失败不影响主流程 */ }
  }

  /**
   * 与 export.js 原有实现等价的浏览器下载。
   * @returns {{ok:boolean, via:string}} 环境不支持对象 URL 时 ok 为 false
   */
  function browserSave(blob, filename) {
    if (typeof document === 'undefined' || typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') {
      return { ok: false, via: 'browser' };
    }
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    // 延后回收：立即 revoke 会让部分浏览器来不及取走数据
    setTimeout(function () { URL.revokeObjectURL(url); }, 4000);
    return { ok: true, via: 'browser' };
  }

  /**
   * 把字节序列编码为 base64。
   * String.fromCharCode.apply 有实参个数上限，按块拼接避免大文件抛栈溢出。
   */
  function bytesToBase64(bytes) {
    var CHUNK = 0x8000;
    var binary = '';
    for (var i = 0; i < bytes.length; i += CHUNK) {
      binary += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
    }
    if (typeof btoa !== 'function') throw new Error('当前环境不支持 base64 编码');
    return btoa(binary);
  }

  /**
   * Blob 转 base64 纯文本（不含 data: 前缀，Filesystem 插件只认裸 base64）。
   * blob.type 为空时 FileReader 产出 "data:base64,..."，非空时产出
   * "data:<mime>;base64,..."，两种前缀都要剥掉。
   * @param {Blob} blob
   * @returns {Promise<string>}
   */
  function toBase64(blob) {
    return new Promise(function (resolve, reject) {
      if (!blob) { reject(new Error('缺少待导出的文件内容')); return; }
      if (typeof FileReader === 'function') {
        var reader = new FileReader();
        reader.onload = function () {
          resolve(String(reader.result || '').replace(/^data:(?:[^;,]*;)?base64,/, ''));
        };
        reader.onerror = function () { reject(reader.error || new Error('读取文件内容失败')); };
        reader.readAsDataURL(blob);
        return;
      }
      // 没有 FileReader 的环境（老 WebView / 无 DOM 的自检脚本）退回字节遍历
      if (typeof blob.arrayBuffer === 'function') {
        Promise.resolve(blob.arrayBuffer()).then(function (buf) {
          resolve(bytesToBase64(new Uint8Array(buf)));
        }, reject);
        return;
      }
      reject(new Error('当前环境无法读取 Blob 内容'));
    });
  }

  /** 用户主动取消分享面板：Capacitor 以 reject 表达，不能当成失败去兜底 */
  function isCancelled(e) {
    var text = (e && (e.message || e)) + '';
    return /cancel/i.test(text);
  }

  function msgOf(e) {
    if (!e) return '未知错误';
    return e.message || (typeof e === 'string' ? e : '未知错误');
  }

  /**
   * 保存（原生环境为「保存并分享」）导出文件。
   * @param {Blob} blob 文件内容
   * @param {string} filename 文件名，将写入 Documents/MedDesk/ 下
   * @returns {Promise<{ok:boolean, via:'native'|'browser', path?:string}>}
   */
  function saveBlob(blob, filename) {
    var name = filename || ('meddesk-export-' + Date.now());
    if (!isNative()) return Promise.resolve(browserSave(blob, name));

    var cap = capacitor();
    var Filesystem = cap.Plugins.Filesystem;
    var Share = cap.Plugins.Share;
    var relative = SUB_DIR + '/' + name;

    return toBase64(blob).then(function (base64) {
      return Filesystem.writeFile({
        path: relative,
        data: base64,
        directory: DIRECTORY_DOCUMENTS,
        // 首次导出时 MedDesk/ 目录尚不存在，不递归创建会直接失败
        recursive: true
      });
    }).then(function (written) {
      var path = (written && written.uri) || relative;
      return Filesystem.getUri({ path: relative, directory: DIRECTORY_DOCUMENTS })
        .then(function (res) {
          var uri = res && res.uri;
          if (!uri) throw new Error('未能取得文件的分享 uri');
          return { uri: uri, path: path };
        });
    }).then(function (info) {
      return Share.share({ files: [info.uri] }).then(function () { return info; });
    }).then(function (info) {
      return { ok: true, via: 'native', path: info.path };
    }).catch(function (e) {
      if (isCancelled(e)) return { ok: true, via: 'native', path: relative };
      // 原生链路失败必须留一条可见的出路，不能让用户在 App 里点了导出毫无反应
      toast('原生分享失败，已改用浏览器下载（' + msgOf(e) + '）');
      return browserSave(blob, name);
    });
  }

  exportShare.isNative = isNative;
  exportShare.saveBlob = saveBlob;
  exportShare.toBase64 = toBase64;
})(typeof window !== 'undefined' ? window : this);

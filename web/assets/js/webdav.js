/* ==========================================================================
 * webdav.js —— MedDesk「文件存储」模块（WebDAV 网盘 + 设置页配置区）
 *
 * 依赖（只读调用，不修改这些文件）：
 *   core.js   ：MD.ui / MD.icon / MD.router / MD.util / MD.toast / MD.haptic / MD.bus
 *   api.js    ：MD.api.request（统一鉴权 + refresh）/ MD.api.getDEK（会话密钥）
 *   crypto.js ：MD.crypto.encrypt / MD.crypto.decrypt（AES-GCM）
 *   db.js     ：MD.db.metaGet / metaSet / metaDel（IndexedDB）
 *
 * 后端契约（全部 POST + JSON，凭据放 body，绝不放 URL / query）：
 *   /api/v1/webdav/test     {baseURL,user,pass}                 → {ok,exists}
 *   /api/v1/webdav/list     {baseURL,user,pass,path}            → {items:[{name,path,isDir,size,modified}]}
 *   /api/v1/webdav/mkdir    {baseURL,user,pass,path}            → {ok}
 *   /api/v1/webdav/delete   {baseURL,user,pass,path}            → {ok}
 *   /api/v1/webdav/upload   multipart：config(JSON 字符串) + file → {ok,path}
 *   /api/v1/webdav/download {baseURL,user,pass,path}            → 文件二进制流
 *   path 为相对 baseURL 的路径（如 docs/a.txt），空串或 / 表示根目录；
 *   list 返回的 item.path 同样是相对根目录的完整路径。
 *
 * 后端约定（与 internal/webdav/webdav.go 对齐）：
 *   user 必填、pass 可空串；baseURL 必须 https 且不得写成 https://账号:密码@域名/ 的形式；
 *   items 恒为数组；mkdir 幂等、upload 自动创建父目录（无需先 mkdir）；
 *   错误信封为 {code,message}，只读 code 分支（bad_request / unauthorized / not_found /
 *   conflict / payload_too_large / unavailable），不解析 message；
 *   远程请求超时 15s、单文件下载上限 200MB、单次列目录上限 2000 项（超限为 payload_too_large）。
 *
 * 凭据安全约定（改动时务必保持）：
 *   1. 配置 {baseURL,user,pass} 永不以明文写入 localStorage / sessionStorage；
 *      一律用会话 DEK 做 AES-GCM 加密，密文放 IndexedDB 的 meta 存储（键 webdav.cfg）。
 *   2. DEK 不可用（未解锁 / 密钥丢失 / IndexedDB 不可用）时降级为「仅内存保存」，
 *      并在界面上明确提示「当前无法加密保存，凭据仅本次会话有效」。
 *   3. 密码输入框默认 type=password，提供「显示 / 隐藏」切换。
 *   4. 所有错误提示、状态文案、Toast 都经 scrub() 脱敏，绝不回显密码或账号。
 *   5. 代码、注释、示例文案中不含任何真实 WebDAV 地址 / 账号 / 密码，
 *      占位符统一写作 https://你的域名/路径/ 。
 *
 * 分层约定：存储层 getConfig/saveConfig → 接口层 callXxx() → 渲染层 renderXxx()
 * 零外部依赖：无 CDN、无第三方库。
 * ========================================================================== */
(function (global) {
  'use strict';

  var MD = global.MD = global.MD || {};
  var doc = global.document;

  /* ============================ 0. 常量 ============================ */

  var CFG_KEY = 'webdav.cfg';            // 密文配置在 IndexedDB meta 存储中的键名
  var API = '/webdav';                   // MD.api.request 已带 /api/v1 前缀
  var MASK = '••••';                     // 脱敏替换符号
  var STYLE_ID = 'md-webdav-style';
  var PLACEHOLDER_URL = 'https://你的域名/路径/';

  /* ============================ 1. 通用小工具 ============================ */

  function toast(message, kind) {
    if (MD.toast && MD.toast.show) MD.toast.show(message, kind || 'info');
  }

  function tap() { if (MD.haptic && MD.haptic.tap) MD.haptic.tap(); }
  function success() { if (MD.haptic && MD.haptic.success) MD.haptic.success(); }

  // 任何面向用户的文案都要过一遍这里：把记忆中的凭据片段替换成掩码，
  // 避免后端错误信息里回显账号 / 密码。
  function scrub(value) {
    var text = String(value == null ? '' : value);
    if (!text || !memoryConfig) return text;
    return [memoryConfig.pass, memoryConfig.user].reduce(function (acc, secret) {
      if (!secret || String(secret).length < 2) return acc;
      return acc.split(String(secret)).join(MASK);
    }, text);
  }

  // 后端错误信封的 code → 中文提示。
  // 约定：只按 code 分支，不解析 message（message 是服务端文案且可能变化）。
  var ERROR_TEXT = {
    bad_request: '请求被拒绝：请检查服务器地址是否需要 https、账号是否填写、路径是否合法',
    unauthorized: '凭据无效或权限不足：请到「设置 · 文件存储」重新填写账号与密码',
    not_found: '远程路径不存在：它可能已被移动或删除',
    conflict: '目标路径存在冲突：同名项目已存在，或上级目录尚未创建',
    payload_too_large: '内容过大：单文件上限 200MB，单目录最多 2000 项',
    unavailable: '无法连接 WebDAV 服务器或请求超时，请稍后重试'
  };

  function readableError(error, fallback) {
    if (error && error.network) return '网络异常，请检查连接后重试';
    if (error && error.code && ERROR_TEXT[error.code]) return ERROR_TEXT[error.code];
    var message = error && error.message ? String(error.message) : '';
    return scrub(message || fallback || '操作失败');
  }

  function humanSize(bytes) {
    var size = Number(bytes);
    if (!isFinite(size) || size < 0) return '—';
    if (size < 1024) return size + ' B';
    var units = ['KB', 'MB', 'GB', 'TB'];
    var index = -1;
    var value = size;
    while (value >= 1024 && index < units.length - 1) {
      value /= 1024;
      index += 1;
    }
    return (value >= 100 ? value.toFixed(0) : value.toFixed(1)) + ' ' + units[index];
  }

  function humanTime(value) {
    if (!value) return '';
    var ms = Date.parse(value);
    if (!isFinite(ms)) return String(value);   // 后端返回未知时间格式：原样显示（走 textContent，无注入风险）
    return MD.util && MD.util.fmtDateTime ? MD.util.fmtDateTime(ms) : String(value);
  }

  function isSafeName(name) {
    var value = String(name == null ? '' : name);
    if (!value || value === '.' || value === '..') return false;
    return !/[/\\]/.test(value);
  }

  // 始终基于「当前目录 + 名称」拼出目标路径，不直接信任后端返回的 path，
  // 前端与后端双重保证无法越出根目录。
  function joinPath(base, name) {
    if (!isSafeName(name)) return null;
    return base ? base + '/' + name : name;
  }

  // list 返回的 item.path 是「相对根目录的规范化路径」（无前导斜杠、目录无尾斜杠），
  // 可直接回填给 download/delete/mkdir（文件名含空格 / 中文 / 特殊字符时的 URL 转义由后端负责）。
  // 这里仍做一次兜底校验：带前导斜杠、含反斜杠或含 . / .. 段时退回本地拼接的 fallback，
  // 保证任何异常数据都不会越出根目录。
  function itemPath(value, fallback) {
    var text = String(value == null ? '' : value);
    if (!text || /^\//.test(text) || text.indexOf('\\') >= 0) return fallback || null;
    var segments = text.split('/');
    for (var index = 0; index < segments.length; index += 1) {
      var segment = segments[index];
      if (!segment || segment === '.' || segment === '..') return fallback || null;
    }
    return text;
  }

  function parentPath(path) {
    var segments = String(path || '').split('/').filter(Boolean);
    return segments.slice(0, -1).join('/');
  }

  function normalizePath(path) {
    return String(path || '')
      .split('/')
      .filter(function (segment) {
        return segment && segment !== '.' && segment !== '..';
      })
      .join('/');
  }

  /* ============================ 2. 样式注入（.wd- 前缀） ============================ */

  var CSS = [
    '.wd-wrap{display:block}',
    '.wd-toolbar{display:flex;flex-wrap:wrap;gap:8px}',
    '.wd-toolbar .btn{min-height:44px}',
    '.wd-crumbs{display:flex;flex-wrap:wrap;align-items:center;gap:2px;min-height:44px;margin:0}',
    '.wd-crumb{max-width:190px;padding:8px 8px;border:0;border-radius:9px;background:transparent;color:var(--brand);font:inherit;font-weight:600;cursor:pointer;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.wd-crumb:hover{background:var(--brand-weak)}',
    '.wd-crumb[aria-current="true"]{color:var(--text);cursor:default}',
    '.wd-crumb[aria-current="true"]:hover{background:transparent}',
    '.wd-crumb__sep{color:var(--text-dim)}',
    '.wd-list{display:flex;flex-direction:column;margin:0;padding:0;list-style:none}',
    '.wd-row{display:flex;align-items:center;gap:10px;border-bottom:1px solid var(--border)}',
    '.wd-row:last-child{border-bottom:0}',
    '.wd-row__main{flex:1 1 auto;min-width:0;display:flex;align-items:center;gap:10px;min-height:56px;padding:12px 0;background:none;border:0;color:inherit;font:inherit;text-align:left;cursor:pointer}',
    '.wd-row__main:disabled{opacity:.5;cursor:not-allowed}',
    '.wd-row__body{min-width:0;flex:1 1 auto}',
    '.wd-row__name{display:block;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.wd-row__meta{display:block;margin-top:2px;color:var(--text-dim);font-size:12px}',
    '.wd-note{margin:10px 0 0;font-size:13px;color:var(--text-dim)}',
    '.wd-note[data-tone="error"]{color:var(--danger)}',
    '.wd-note[data-tone="warn"]{color:var(--warn)}',
    '.wd-form{display:flex;flex-direction:column;gap:12px}',
    '.wd-form .input{min-height:46px;width:100%;font-size:16px}',
    '.wd-secret{display:flex;align-items:center;gap:8px}',
    '.wd-secret .input{flex:1 1 auto}',
    '.wd-secret .btn{flex:0 0 auto;min-height:46px}',
    '.wd-host{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}'
  ].join('\n');

  function ensureStyle() {
    if (!doc || !doc.head || doc.getElementById(STYLE_ID)) return;
    var style = doc.createElement('style');
    style.id = STYLE_ID;
    style.textContent = CSS;
    doc.head.appendChild(style);
  }

  /* ============================ 3. 存储层（加密持久化） ============================ */

  var memoryConfig = null;   // 明文配置的内存副本；解密成功或降级保存后才有值
  var memoryOnly = false;    // true = 无法加密落盘，凭据仅本次会话有效

  function cloneConfig(cfg) {
    return cfg ? { baseURL: cfg.baseURL, user: cfg.user, pass: cfg.pass } : null;
  }

  function normalizeConfig(raw) {
    if (!raw) return null;
    var baseURL = String(raw.baseURL == null ? '' : raw.baseURL).trim();
    if (!baseURL) return null;   // 没有服务器地址视为未配置
    return {
      baseURL: baseURL,
      user: String(raw.user == null ? '' : raw.user).trim(),
      pass: String(raw.pass == null ? '' : raw.pass)
    };
  }

  function currentDEK() {
    return MD.api && typeof MD.api.getDEK === 'function' ? MD.api.getDEK() : null;
  }

  function readBox() {
    if (!MD.db || typeof MD.db.metaGet !== 'function') return Promise.resolve(null);
    return MD.db.metaGet(CFG_KEY).catch(function () { return null; });   // 数据库不可用时视作未配置
  }

  function writeBox(box) {
    if (!MD.db || typeof MD.db.metaSet !== 'function') throw new Error('本地数据库不可用');
    return MD.db.metaSet(CFG_KEY, box);
  }

  function dropBox() {
    if (!MD.db || typeof MD.db.metaDel !== 'function') return Promise.resolve(null);
    return MD.db.metaDel(CFG_KEY).catch(function () { return null; });
  }

  function announce() {
    if (!MD.bus || !MD.bus.emit) return;
    MD.bus.emit('webdav:changed', { configured: Boolean(memoryConfig), memoryOnly: memoryOnly });
  }

  // 退出登录 / 会话失效（team-lead 负责的「忘记此设备」等场景）→ 立即丢弃内存凭据，
  // 避免 DEK 已作废后仍用旧明文配置发起请求。
  if (MD.bus && MD.bus.on) {
    MD.bus.on('auth:changed', function (session) {
      if (session) return;
      memoryConfig = null;
      memoryOnly = false;
      announce();
    });
  }

  /**
   * 读取配置：内存 → IndexedDB 密文（用 DEK 解密）。未配置返回 null。
   * @returns {Promise<{baseURL:string,user:string,pass:string}|null>}
   */
  async function getConfig() {
    if (memoryConfig) return cloneConfig(memoryConfig);
    var box = await readBox();
    if (!box || !box.ctB64 || !box.ivB64) return null;
    var dek = currentDEK();
    if (!dek) return null;                       // 未解锁：不返回任何内容，也不报错
    if (!MD.crypto || !MD.crypto.decrypt) return null;
    var plain;
    try {
      plain = await MD.crypto.decrypt(dek, box.ivB64, box.ctB64);
    } catch (_) {
      return null;                               // DEK 已轮换导致解密失败：视作未配置
    }
    var cfg = normalizeConfig(parseJSON(plain));
    if (cfg) memoryConfig = cfg;
    return cloneConfig(memoryConfig);
  }

  function parseJSON(text) {
    try { return JSON.parse(text); } catch (_) { return null; }
  }

  /**
   * 保存配置：优先 AES-GCM 加密落盘；失败则降级为仅内存保存。
   * @param {{baseURL:string,user?:string,pass?:string}} cfg
   * @returns {Promise<{persisted:boolean,memoryOnly:boolean}>}
   */
  async function saveConfig(cfg) {
    cfg = normalizeConfig(cfg);
    if (!cfg) throw new Error('请先填写服务器地址');
    memoryConfig = cfg;                          // 无论能否落盘，本次会话立即可用
    var persisted = false;
    try {
      var dek = currentDEK();
      if (!dek) throw new Error('会话密钥不可用');
      if (!MD.crypto || !MD.crypto.encrypt) throw new Error('浏览器加密能力不可用');
      var box = await MD.crypto.encrypt(dek, JSON.stringify(cfg));
      await writeBox({ ivB64: box.ivB64, ctB64: box.ctB64 });
      persisted = true;
      memoryOnly = false;
    } catch (_) {
      // 降级：凭据只留在内存，由上层提示「仅本次会话有效」。
      memoryOnly = true;
    }
    announce();
    return { persisted: persisted, memoryOnly: memoryOnly };
  }

  /** 清除配置（含「忘记此设备」场景）：删除密文 + 丢弃内存副本。 */
  async function clearConfig() {
    await dropBox();
    memoryConfig = null;
    memoryOnly = false;
    announce();
  }

  /* ============================ 4. 接口层 ============================ */

  function credentialsBody(cfg, path) {
    var body = { baseURL: cfg.baseURL, user: cfg.user, pass: cfg.pass };
    if (path != null) body.path = path;
    return body;
  }

  // 所有请求统一走 MD.api.request：复用现有的 Bearer 鉴权与 401 refresh 机制。
  function call(name, body, options) {
    if (!MD.api || !MD.api.request) throw new Error('数据接口未就绪');
    var init = { method: 'POST', body: JSON.stringify(body) };
    if (options) Object.keys(options).forEach(function (key) { init[key] = options[key]; });
    return MD.api.request(API + '/' + name, init);
  }

  function apiTest(cfg) {
    return call('test', credentialsBody(cfg));
  }

  function apiList(cfg, path) {
    return call('list', credentialsBody(cfg, path || ''));
  }

  function apiMkdir(cfg, path) {
    return call('mkdir', credentialsBody(cfg, path));
  }

  function apiDelete(cfg, path) {
    return call('delete', credentialsBody(cfg, path));
  }

  function apiDownload(cfg, path) {
    return call('download', credentialsBody(cfg, path), { responseType: 'blob' });
  }

  /** 上传走 multipart：字段 config（JSON 字符串）+ file，Authorization 头由 MD.api 统一带上。 */
  function apiUpload(cfg, path, file) {
    if (!MD.api || !MD.api.request) throw new Error('数据接口未就绪');
    var form = new FormData();
    form.append('config', JSON.stringify(credentialsBody(cfg, path)));
    form.append('file', file, file && file.name ? file.name : 'upload.bin');
    return MD.api.request(API + '/upload', { method: 'POST', body: form });
  }

  function saveBlobToDevice(name, blob) {
    if (!doc || !global.URL || !global.URL.createObjectURL) throw new Error('当前浏览器不支持下载');
    var url = global.URL.createObjectURL(blob);
    var link = MD.ui.el('a', { href: url, download: name || 'download' });
    doc.body.appendChild(link);
    link.click();
    link.remove();
    global.setTimeout(function () { global.URL.revokeObjectURL(url); }, 4000);
  }

  /* ============================ 5. 渲染：未配置引导 ============================ */

  function renderGuide(host) {
    var goButton = MD.ui.el('button', {
      class: 'btn btn-primary',
      type: 'button',
      text: '去设置',
      onClick: function () {
        tap();
        if (MD.router && MD.router.go) MD.router.go('settings');
      }
    });
    host.appendChild(MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('div', { class: 'row' }, [
        MD.icon('upload-cloud'),
        MD.ui.el('strong', { text: '连接你自己的网盘' })
      ]),
      MD.ui.el('p', {
        class: 'muted',
        text: '在「设置 · 文件存储」中填写 WebDAV 服务器地址、账号与密码后，即可在这里上传、下载与管理文件。'
      }),
      MD.ui.el('p', {
        class: 'muted',
        style: { margin: '0', fontSize: '12px' },
        text: '凭据经加密后保存在本机浏览器中，请求由本站后端转发，后端不保存你的凭据。'
      }),
      MD.ui.el('div', { class: 'wd-toolbar' }, [goButton])
    ]));
  }

  /* ============================ 6. 渲染：设置页配置区 ============================ */

  function settingsSection() {
    ensureStyle();

    var status = MD.ui.el('p', { class: 'wd-note', 'aria-live': 'polite' });

    var baseInput = MD.ui.el('input', {
      class: 'input',
      type: 'url',
      inputMode: 'url',
      autocomplete: 'off',
      spellcheck: 'false',
      placeholder: PLACEHOLDER_URL
    });
    var userInput = MD.ui.el('input', {
      class: 'input',
      type: 'text',
      autocomplete: 'off',
      spellcheck: 'false',
      placeholder: '网盘账号（示例占位，请填写你自己的）'
    });
    var passInput = MD.ui.el('input', {
      class: 'input',
      type: 'password',
      autocomplete: 'off',
      spellcheck: 'false',
      placeholder: '密码或应用专用口令'
    });
    var toggleButton = MD.ui.el('button', {
      class: 'btn btn-ghost',
      type: 'button',
      text: '显示',
      'aria-label': '切换密码显示',
      onClick: function () {
        tap();
        var masked = passInput.type === 'password';
        passInput.type = masked ? 'text' : 'password';
        toggleButton.textContent = masked ? '隐藏' : '显示';
      }
    });

    var saveButton = MD.ui.el('button', { class: 'btn btn-primary', type: 'button', text: '保存' });
    var testButton = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button', text: '测试连接' });
    var clearButton = MD.ui.el('button', { class: 'btn btn-danger', type: 'button', text: '清除配置' });

    function setBusy(busy, text) {
      [baseInput, userInput, passInput, saveButton, testButton, clearButton].forEach(function (node) {
        node.disabled = Boolean(busy);
      });
      if (text) setStatus(text, '');
    }

    function setStatus(text, tone) {
      status.textContent = text || '';
      if (tone) status.dataset.tone = tone;
      else delete status.dataset.tone;
    }

    function currentInputConfig() {
      return normalizeConfig({
        baseURL: baseInput.value,
        user: userInput.value,
        pass: passInput.value
      });
    }

    // 后端约束：baseURL 必须 https（http 会被 400 bad_request 拒绝）、地址中不得内嵌凭据、
    // 账号必填而密码可为空。这里先做前端校验，避免每次都靠错误码才发现。
    function validateInput() {
      var cfg = currentInputConfig();
      if (!cfg) return { error: '请填写服务器地址（形如 ' + PLACEHOLDER_URL + '）' };
      if (!/^https:\/\//i.test(cfg.baseURL)) return { error: '服务器地址必须以 https:// 开头（服务端不接受 http 地址）' };
      if (cfg.baseURL.indexOf('@') >= 0) return { error: '地址中不要内嵌账号密码，请分别填写下方账号与密码' };
      if (!cfg.user) return { error: '请填写账号：服务端要求账号必填（无密码的服务可留空密码）' };
      return { cfg: cfg };
    }

    function refreshStatus() {
      if (memoryOnly) {
        setStatus('当前无法加密保存，凭据仅本次会话有效：刷新页面后需要重新填写。', 'warn');
        return;
      }
      if (memoryConfig) {
        setStatus('凭据已用本机会话密钥加密保存。', '');
        return;
      }
      setStatus('尚未配置文件存储。', '');
    }

    saveButton.addEventListener('click', function () {
      tap();
      var check = validateInput();
      if (check.error) {
        setStatus(check.error, 'error');
        return;
      }
      setBusy(true, '正在保存…');
      saveConfig(check.cfg).then(function (result) {
        setBusy(false, '');
        if (result && result.persisted) {
          toast('已加密保存', 'success');
          setStatus('凭据已用本机会话密钥加密保存。', '');
        } else {
          toast('已保存，但无法加密落盘', 'warn');
          setStatus('当前无法加密保存，凭据仅本次会话有效：刷新页面后需要重新填写。', 'warn');
        }
      }).catch(function (error) {
        setBusy(false, '');
        setStatus(readableError(error, '保存失败'), 'error');
        toast('保存失败', 'error');
      });
    });

    testButton.addEventListener('click', function () {
      tap();
      var check = validateInput();
      if (check.error) {
        setStatus(check.error, 'error');
        return;
      }
      setBusy(true, '正在测试连接…');
      apiTest(check.cfg).then(function (payload) {
        setBusy(false, '');
        var exists = !payload || payload.exists !== false;
        if (!exists) {
          setStatus('连接成功，但根路径不存在：请在服务器侧创建该目录后重试。', 'warn');
          toast('连接成功但路径不存在', 'warn');
          return;
        }
        success();
        setStatus('连接成功。', '');
        toast('连接成功', 'success');
      }).catch(function (error) {
        setBusy(false, '');
        var message = readableError(error, '连接失败');
        setStatus(message, 'error');
        toast('连接失败', 'error');
      });
    });

    clearButton.addEventListener('click', function () {
      tap();
      if (!MD.ui || !MD.ui.confirm) {
        clearConfig();
        return;
      }
      MD.ui.confirm({
        title: '清除配置',
        message: '将删除本机保存的 WebDAV 凭据，之后需要重新填写。已上传到网盘的文件不受影响。',
        confirmText: '清除',
        danger: true
      }).then(function (confirmed) {
        if (!confirmed) return;
        setBusy(true, '正在清除…');
        clearConfig().then(function () {
          baseInput.value = '';
          userInput.value = '';
          passInput.value = '';
          passInput.type = 'password';
          toggleButton.textContent = '显示';
          setBusy(false, '');
          toast('已清除配置', 'success');
          setStatus('尚未配置文件存储。', '');
        }).catch(function (error) {
          setBusy(false, '');
          setStatus(readableError(error, '清除失败'), 'error');
        });
      });
    });

    var card = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('h2', { class: 'section-title', text: '文件存储' }),
      MD.ui.el('p', {
        class: 'muted',
        style: { margin: '0', fontSize: '13px' },
        text: '凭据（服务器地址、账号、密码）使用本机会话密钥 AES-GCM 加密后保存在本机浏览器数据库中，不会写入 localStorage，也不会上传明文；每次请求经本站后端转发到你的 WebDAV 服务器，后端不保存凭据。'
      }),
      // 用 form 包裹填写区：① 消除 Chrome「Password field is not contained in a form」提示；
      // ② 让在任一输入框回车等价于点「保存」。
      MD.ui.el('form', {
        class: 'wd-form',
        onSubmit: function (event) {
          if (event && typeof event.preventDefault === 'function') event.preventDefault();
          if (saveButton) saveButton.click();
        }
      }, [
        MD.ui.el('div', { class: 'field' }, [
          MD.ui.el('label', { class: 'muted', style: { fontSize: '13px', fontWeight: '600' }, text: '服务器地址' }),
          baseInput,
          MD.ui.el('span', { class: 'muted', style: { fontSize: '12px' }, text: '形如 ' + PLACEHOLDER_URL + '（占位示例）；必须是 https 地址，账号密码不要写在地址里' })
        ]),
        MD.ui.el('div', { class: 'field' }, [
          MD.ui.el('label', { class: 'muted', style: { fontSize: '13px', fontWeight: '600' }, text: '账号' }),
          userInput,
          MD.ui.el('span', { class: 'muted', style: { fontSize: '12px' }, text: '账号必填；只需用户名/令牌的服务可将密码留空' })
        ]),
        MD.ui.el('div', { class: 'field' }, [
          MD.ui.el('label', { class: 'muted', style: { fontSize: '13px', fontWeight: '600' }, text: '密码' }),
          MD.ui.el('div', { class: 'wd-secret' }, [passInput, toggleButton]),
          MD.ui.el('span', { class: 'muted', style: { fontSize: '12px' }, text: '建议使用网盘的应用专用口令；密码默认掩码显示。' })
        ])
      ]),
      status,
      MD.ui.el('div', { class: 'wd-toolbar' }, [saveButton, testButton, clearButton])
    ]);

    // 已有配置时回填（账号不全、密码留空需重填也能保存）。
    getConfig().then(function (cfg) {
      if (!cfg) return;
      baseInput.value = cfg.baseURL;
      userInput.value = cfg.user;
      passInput.value = cfg.pass;
    }).catch(function () { /* 读取失败保持空表单 */ }).then(refreshStatus);

    return card;
  }

  /* ============================ 7. 渲染：文件浏览器 ============================ */

  function normalizeItems(items) {
    if (!Array.isArray(items)) return [];
    return items.filter(function (item) {
      return item && item.name;
    }).map(function (item) {
      return {
        name: String(item.name),
        path: String(item.path == null ? '' : item.path),
        isDir: Boolean(item.isDir),
        size: Number(item.size) || 0,
        modified: item.modified || ''
      };
    }).sort(function (a, b) {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
      return a.name.localeCompare(b.name, 'zh-CN');
    });
  }

  function renderBrowser(host, cfg) {
    var state = { path: '', items: [], loading: false, busy: false };

    var fileInput = MD.ui.el('input', {
      type: 'file',
      multiple: true,
      style: { display: 'none' },
      tabIndex: -1
    });

    var note = MD.ui.el('p', { class: 'wd-note', 'aria-live': 'polite' });
    var crumbs = MD.ui.el('nav', { class: 'wd-crumbs', 'aria-label': '目录层级' });
    var listHost = MD.ui.el('ul', { class: 'wd-list' });

    var uploadButton = MD.ui.el('button', { class: 'btn btn-primary', type: 'button' }, [
      MD.icon('upload'), '上传文件'
    ]);
    var mkdirButton = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button' }, [
      MD.icon('plus'), '新建目录'
    ]);
    var reloadButton = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button' }, [
      MD.icon('sync'), '刷新'
    ]);
    var upButton = MD.ui.el('button', { class: 'btn btn-ghost', type: 'button', text: '返回上级' });

    upButton.addEventListener('click', function () {
      tap();
      if (!state.path) return;                       // 根目录没有上级
      navigate(parentPath(state.path));
    });

    uploadButton.addEventListener('click', function () {
      tap();
      fileInput.value = '';
      fileInput.click();
    });

    fileInput.addEventListener('change', function () {
      var files = Array.prototype.slice.call(fileInput.files || []);
      uploadFiles(files);
    });

    mkdirButton.addEventListener('click', function () {
      tap();
      openMkdirDialog();
    });

    reloadButton.addEventListener('click', function () {
      tap();
      load();
    });

    /* ---- 7.1 状态与提示 ---- */

    function setBusy(busy, text) {
      state.busy = Boolean(busy);
      [uploadButton, mkdirButton, reloadButton, fileInput].forEach(function (node) {
        node.disabled = state.busy;
      });
      listHost.querySelectorAll('.wd-row__main, .btn-icon').forEach(function (node) {
        node.disabled = state.busy;
      });
      if (text) setNote(text, '');
      else if (!state.busy) setNote(idleNote(), '');
    }

    function setNote(text, tone) {
      note.textContent = text || '';
      if (tone) note.dataset.tone = tone;
      else delete note.dataset.tone;
    }

    function idleNote() {
      if (state.loading) return '正在加载…';
      if (!state.items.length) return '';
      return '共 ' + state.items.length + ' 项';
    }

    /* ---- 7.2 面包屑（只能逐级返回，无越界入口） ---- */

    function crumbButton(label, target, isCurrent) {
      return MD.ui.el('button', {
        class: 'wd-crumb',
        type: 'button',
        text: label,
        'aria-current': isCurrent ? 'true' : null,
        onClick: isCurrent ? null : function () {
          tap();
          navigate(target);
        }
      });
    }

    function paintCrumbs() {
      MD.ui.clear(crumbs);
      var segments = String(state.path || '').split('/').filter(Boolean);
      upButton.disabled = state.busy || !segments.length;
      crumbs.appendChild(upButton);
      crumbs.appendChild(MD.ui.el('span', { class: 'wd-crumb__sep', text: '|' }));
      crumbs.appendChild(crumbButton('根目录', '', !segments.length));
      var acc = [];
      segments.forEach(function (segment, index) {
        acc.push(segment);
        crumbs.appendChild(MD.ui.el('span', { class: 'wd-crumb__sep', text: '/' }));
        crumbs.appendChild(crumbButton(segment, acc.join('/'), index === segments.length - 1));
      });
    }

    function navigate(path) {
      var next = normalizePath(path);
      if (next === state.path) {
        load();
        return;
      }
      state.path = next;
      paintCrumbs();
      load();
    }

    /* ---- 7.3 列表 ---- */

    function paintLoading() {
      MD.ui.clear(listHost);
      listHost.appendChild(MD.ui.el('li', { class: 'wd-row' }, [
        MD.ui.el('span', { class: 'muted', text: '正在加载…' })
      ]));
    }

    function paintList() {
      MD.ui.clear(listHost);
      if (!state.items.length) {
        listHost.appendChild(MD.ui.el('li', { class: 'empty' }, [
          MD.icon('folder'),
          MD.ui.el('span', { text: '这个目录还没有文件' })
        ]));
        return;
      }
      state.items.forEach(function (item) {
        listHost.appendChild(rowFor(item));
      });
    }

    function rowFor(item) {
      // 优先用后端给的 item.path，缺失或不合法时退回「当前目录 + 名称」的本地拼接。
      var target = itemPath(item.path, joinPath(state.path, item.name));
      var meta = humanTime(item.modified);
      var main = MD.ui.el('button', {
        class: 'wd-row__main',
        type: 'button',
        disabled: target == null || state.busy,
        onClick: function () {
          tap();
          if (target == null) return;
          if (item.isDir) navigate(target);
          else downloadItem(item, target);
        }
      }, [
        MD.icon(item.isDir ? 'folder' : 'download'),
        MD.ui.el('span', { class: 'wd-row__body' }, [
          MD.ui.el('span', { class: 'wd-row__name', text: item.name }),
          MD.ui.el('span', {
            class: 'wd-row__meta',
            text: (item.isDir ? '文件夹' : humanSize(item.size)) + (meta ? ' · ' + meta : '')
          })
        ])
      ]);
      var remove = MD.ui.el('button', {
        class: 'btn btn-icon',
        type: 'button',
        disabled: target == null || state.busy,
        'aria-label': '删除 ' + item.name,
        onClick: function () {
          tap();
          if (target == null) return;
          removeItem(item, target);
        }
      }, MD.icon('trash'));
      return MD.ui.el('li', { class: 'wd-row' }, [main, remove]);
    }

    async function load() {
      if (state.loading) return;
      state.loading = true;
      paintLoading();
      setNote(idleNote(), '');
      try {
        var payload = await apiList(cfg, state.path);
        state.items = normalizeItems(payload && payload.items);
        state.loading = false;
        paintList();
        setNote(idleNote(), '');
      } catch (error) {
        state.loading = false;
        state.items = [];
        paintList();
        setNote(readableError(error, '加载失败'), 'error');
      }
    }

    /* ---- 7.4 动作 ---- */

    async function uploadFiles(files) {
      if (!files.length || state.busy) return;
      var okCount = 0;
      var failCount = 0;
      var lastError = '';
      setBusy(true, '准备上传…');
      try {
        for (var index = 0; index < files.length; index += 1) {
          var file = files[index];
          if (!file) continue;
          if (!isSafeName(file.name)) {             // 浏览器一般已限制，这里再兜一层
            failCount += 1;
            lastError = '文件名不合法：' + String(file.name);
            continue;
          }
          setBusy(true, '正在上传 ' + (index + 1) + '/' + files.length + '：' + file.name);
          try {
            await apiUpload(cfg, state.path, file);
            okCount += 1;
          } catch (error) {
            failCount += 1;
            lastError = readableError(error, '上传失败');
          }
        }
      } finally {
        setBusy(false, '');
      }
      if (!failCount) {
        success();
        toast('已上传 ' + okCount + ' 个文件', 'success');
      } else {
        setNote(failCount + ' 个文件上传失败：' + lastError, 'error');
        toast(failCount + ' 个文件上传失败', 'error');
      }
      if (fileInput.value) fileInput.value = '';
      await load();
    }

    async function downloadItem(item, target) {
      if (state.busy) return;
      setBusy(true, '正在下载 ' + item.name + '…');
      try {
        var blob = await apiDownload(cfg, target);
        if (!blob || typeof blob.size !== 'number') throw new Error('下载内容为空');
        saveBlobToDevice(item.name, blob);
        success();
        toast('已保存 ' + item.name, 'success');
      } catch (error) {
        setNote(readableError(error, '下载失败'), 'error');
        toast('下载失败', 'error');
      } finally {
        setBusy(false, '');
      }
    }

    function removeItem(item, target) {
      if (!MD.ui || !MD.ui.confirm) {
        toast('当前环境不支持确认弹窗', 'error');
        return;
      }
      MD.ui.confirm({
        title: '删除「' + item.name + '」',
        message: item.isDir
          ? '目录及其中的文件都会被删除，此操作不可恢复。'
          : '文件将从网盘中删除，此操作不可恢复。',
        confirmText: '删除',
        danger: true
      }).then(function (confirmed) {
        if (!confirmed) return;
        setBusy(true, '正在删除…');
        apiDelete(cfg, target).then(function () {
          setBusy(false, '');
          success();
          toast('已删除 ' + item.name, 'success');
          return load();
        }).catch(function (error) {
          setBusy(false, '');
          setNote(readableError(error, '删除失败'), 'error');
          toast('删除失败', 'error');
        });
      });
    }

    function openMkdirDialog() {
      if (!MD.ui || !MD.ui.sheet) {
        toast('当前环境不支持弹窗', 'error');
        return;
      }
      var input = MD.ui.el('input', {
        class: 'input',
        type: 'text',
        placeholder: '新目录名称',
        maxLength: 120
      });
      var error = MD.ui.el('p', { class: 'wd-note', dataset: { tone: 'error' } });
      MD.ui.sheet({
        title: '新建目录',
        body: function (body) {
          body.appendChild(MD.ui.el('p', {
            class: 'muted',
            text: '目录将创建在：' + (state.path || '根目录')
          }));
          body.appendChild(MD.ui.el('div', { class: 'field' }, [input]));
          body.appendChild(error);
        },
        actions: [
          { label: '取消', kind: 'ghost', onClick: function () { } },
          {
            label: '创建',
            kind: 'primary',
            onClick: function () {
              var name = String(input.value || '').trim();
              error.textContent = '';
              if (!isSafeName(name)) {
                error.textContent = '目录名称不能为空，且不能包含 / 或 \\';
                return false;                       // 阻止 MD.ui.sheet 关闭
              }
              var target = joinPath(state.path, name);
              if (target == null) {
                error.textContent = '目录名称不合法';
                return false;
              }
              setBusy(true, '正在创建…');
              apiMkdir(cfg, target).then(function () {
                setBusy(false, '');
                success();
                toast('已创建目录', 'success');
                return load();
              }).catch(function (apiError) {
                setBusy(false, '');
                setNote(readableError(apiError, '创建失败'), 'error');
                toast('创建目录失败', 'error');
              });
              return true;
            }
          }
        ]
      });
      global.setTimeout(function () { input.focus(); }, 60);
    }

    /* ---- 7.5 组装 ---- */

    var toolbar = MD.ui.el('div', { class: 'wd-toolbar' }, [uploadButton, mkdirButton, reloadButton]);

    var connectionCard = MD.ui.el('section', { class: 'card stack' }, [
      MD.ui.el('div', { class: 'row' }, [
        MD.icon('upload-cloud'),
        MD.ui.el('strong', { text: '文件存储' })
      ]),
      MD.ui.el('p', {
        class: 'muted wd-host',
        style: { margin: '0', fontSize: '12px' },
        text: '服务器：' + cfg.baseURL + (cfg.user ? ' · 账号：' + cfg.user : '')
      }),
      memoryOnly
        ? MD.ui.el('p', {
          class: 'wd-note',
          dataset: { tone: 'warn' },
          text: '当前无法加密保存，凭据仅本次会话有效：刷新页面后需要到设置里重新填写。'
        })
        : null,
      toolbar,
      fileInput
    ]);

    var listCard = MD.ui.el('section', { class: 'card' }, [
      crumbs,
      listHost,
      note
    ]);

    host.appendChild(connectionCard);
    host.appendChild(listCard);

    paintCrumbs();
    load();
  }

  /* ============================ 8. 路由渲染 ============================ */

  function render(view) {
    ensureStyle();
    var wrap = MD.ui.el('div', { class: 'stack wd-wrap' });
    wrap.appendChild(MD.ui.el('p', { class: 'muted', text: '正在读取本地配置…' }));
    view.appendChild(wrap);

    getConfig().then(function (cfg) {
      MD.ui.clear(wrap);
      if (!cfg) renderGuide(wrap);
      else renderBrowser(wrap, cfg);
    }).catch(function () {
      MD.ui.clear(wrap);
      renderGuide(wrap);
    });
  }

  /* ============================ 9. 导出与注册 ============================ */

  MD.webdav = {
    getConfig: getConfig,
    saveConfig: saveConfig,
    clearConfig: clearConfig,
    settingsSection: settingsSection
  };

  if (MD.router && typeof MD.router.register === 'function') {
    MD.router.register({
      id: 'files',
      title: '文件存储',
      subtitle: 'WebDAV 上传与下载',
      icon: 'folder',
      group: '工具箱',
      order: 60,
      // 退出手机底部标签栏（让位给「记账本」），侧栏入口与 order 均不变：
      // 文件存储是低频配置项，记账本是天天要记的，底部栏位置应给它。
      tab: false,
      render: render
    });
  }
})(typeof window !== 'undefined' ? window : this);

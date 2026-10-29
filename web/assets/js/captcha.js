(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  // 图形验证码的前端门面：取图、渲染、取值、刷新。
  // 服务端只返回 SVG 源码，这里用 innerHTML 直接插入，避免额外一次 base64 解码。
  var state = {
    id: '',       // 当前这张图对应的 captchaId，提交时必须原样带回
    box: null,    // 容器
    image: null,  // 图片区（点击即刷新）
    input: null   // 验证码输入框
  };

  // inFlight 是连点保护：刷新过程中再次点击直接忽略。
  // 没有它的话，用户双击「看不清，换一张」会在毫秒内打两次接口，
  // 而且后到的响应可能覆盖先到的，导致展示的图和 state.id 不是同一张。
  var inFlight = false;

  function request() {
    // 复用 api.js 的 request：基址、鉴权头、错误结构都已在那边统一处理，
    // 这里再拼一次 '/api/v1' 会在安卓壳（页面源是 https://localhost）里打错地址。
    if (MD.api && typeof MD.api.request === 'function') {
      return MD.api.request('/auth/captcha', { skipAuthRefresh: true, skipUnauthorized: true });
    }
    return Promise.reject(new Error('网络模块未就绪'));
  }

  // load 取一张新验证码，返回 { captchaId, svg }。
  function load() {
    return request().then(function (payload) {
      var id = payload && (payload.captchaId || payload.CaptchaID);
      var svg = payload && (payload.svg || payload.SVG);
      if (!id || !svg) throw new Error('验证码数据不完整，请重试');
      return { captchaId: id, svg: svg };
    }).catch(function (error) {
      // 统一换成可读文案：底层可能是网络异常（无 message）或后端错误结构。
      throw new Error((error && error.message) || '验证码加载失败，请检查网络后重试');
    });
  }

  // setFailure 在图片区显示一句失败提示，点一下可重试。
  function setFailure(message) {
    state.id = '';
    if (state.image) state.image.textContent = message;
  }

  // refresh 重新取一张：清空已输入的码，因为新图对应的字符已经变了。
  function refresh() {
    if (inFlight) return Promise.resolve(null);
    inFlight = true;
    if (state.image) state.image.textContent = '加载中…';
    return load().then(function (data) {
      state.id = data.captchaId;
      if (state.image) state.image.innerHTML = data.svg;
      if (state.input) state.input.value = '';
      return data;
    }).catch(function (error) {
      setFailure('加载失败，点击重试');
      throw error;
    }).then(function (data) {
      inFlight = false;
      return data;
    }, function (error) {
      inFlight = false;
      throw error;
    });
  }

  // render 在 host 内追加：图片 + 输入框 + 换一张。
  // 只追加不清空，是为了让调用方能在同一个 field 里先放 label 再嵌验证码。
  function render(host) {
    if (!host || !MD.ui || typeof MD.ui.el !== 'function') return host;
    state.box = host;
    state.image = MD.ui.el('div', {
      class: 'captcha__image',
      role: 'button',
      tabIndex: 0,
      title: '看不清？点击换一张',
      'aria-label': '图形验证码，点击刷新',
      style: {
        width: '120px', height: '40px', flex: '0 0 auto', cursor: 'pointer',
        border: '1px solid var(--line, #d8dee9)', borderRadius: '8px',
        overflow: 'hidden', background: '#f4f6f8', lineHeight: '40px',
        textAlign: 'center', fontSize: '12px', color: '#636e72'
      },
      onClick: function () { refresh().catch(function () {}); }
    }, '加载中…');
    state.input = MD.ui.el('input', {
      class: 'input captcha__input',
      type: 'text',
      inputMode: 'text',
      maxLength: 6,   // 与服务端 captchaLength 一致（6 位纯数字）
      autocomplete: 'off',
      autocapitalize: 'off',
      spellcheck: false,
      required: true,
      placeholder: '验证码',
      'aria-label': '图形验证码',
      style: { flex: '1 1 auto', minWidth: '0', letterSpacing: '2px' }
    });
    var change = MD.ui.el('button', {
      class: 'btn btn-ghost captcha__refresh',
      type: 'button',
      text: '看不清，换一张',
      onClick: function () { refresh().catch(function () {}); }
    });
    host.appendChild(MD.ui.el('div', {
      class: 'captcha',
      style: { display: 'flex', alignItems: 'center', gap: '8px' }
    }, [state.image, state.input, change]));
    // 首帧就取图；失败由 refresh 内部把错误显示在图片区，这里不必再弹 toast。
    refresh().catch(function () {});
    return host;
  }

  // value 返回提交时并入注册请求体的两个字段。
  function value() {
    return {
      captchaId: state.id,
      captchaCode: state.input ? String(state.input.value || '').trim() : ''
    };
  }

  MD.captcha = { load: load, render: render, value: value, refresh: refresh };
})(window);

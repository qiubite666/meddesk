(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};
  var webCrypto = window.crypto;
  var subtle = webCrypto && webCrypto.subtle;
  var encoder = new TextEncoder();
  var decoder = new TextDecoder();

  function requireCrypto() {
    if (!webCrypto || !subtle) throw new Error('当前浏览器不支持 WebCrypto');
  }

  function toBytes(value) {
    if (value instanceof Uint8Array) return value;
    if (value instanceof ArrayBuffer) return new Uint8Array(value);
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    if (typeof value === 'string') return b64ToBytes(value);
    throw new TypeError('无法转换为字节数组');
  }

  function bytesToB64(value) {
    var bytes = toBytes(value);
    var binary = '';
    var size = 0x8000;
    for (var index = 0; index < bytes.length; index += size) {
      binary += String.fromCharCode.apply(null, bytes.subarray(index, index + size));
    }
    return window.btoa(binary);
  }

  function b64ToBytes(value) {
    var normalized = String(value || '').replace(/-/g, '+').replace(/_/g, '/');
    while (normalized.length % 4) normalized += '=';
    var binary = window.atob(normalized);
    var bytes = new Uint8Array(binary.length);
    for (var index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
    return bytes;
  }

  function toBase64Url(value) {
    return bytesToB64(value).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
  }

  function fromBase64Url(value) {
    return b64ToBytes(value);
  }

  async function deriveBytes(password, salt) {
    requireCrypto();
    var key = await subtle.importKey('raw', encoder.encode(String(password)), 'PBKDF2', false, ['deriveBits']);
    var bits = await subtle.deriveBits({
      name: 'PBKDF2',
      hash: 'SHA-256',
      salt: toBytes(salt),
      iterations: 210000
    }, key, 256);
    return new Uint8Array(bits);
  }

  async function deriveKEK(password, salt) {
    var authHash = await deriveBytes(password, salt);
    var material = await subtle.importKey('raw', authHash, 'HKDF', false, ['deriveKey']);
    return subtle.deriveKey({
      name: 'HKDF',
      hash: 'SHA-256',
      salt: toBytes(salt),
      info: encoder.encode('meddesk-kek-v1')
    }, material, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  }

  async function importDEK(dek, usage) {
    requireCrypto();
    return subtle.importKey('raw', toBytes(dek), { name: 'AES-GCM' }, false, usage);
  }

  MD.crypto = {
    bytesToB64: bytesToB64,
    b64ToBytes: b64ToBytes,
    toBase64Url: toBase64Url,
    fromBase64Url: fromBase64Url,
    generateSalt: function () {
      requireCrypto();
      return webCrypto.getRandomValues(new Uint8Array(16));
    },
    derive: async function (password, salt) {
      var hash = await deriveBytes(password, salt);
      return { hashB64: bytesToB64(hash) };
    },
    newDEK: function () {
      requireCrypto();
      return webCrypto.getRandomValues(new Uint8Array(32));
    },
    wrapDEK: async function (dek, password, salt) {
      var kek = await deriveKEK(password, salt);
      var iv = webCrypto.getRandomValues(new Uint8Array(12));
      var wrapped = await subtle.encrypt({ name: 'AES-GCM', iv: iv }, kek, toBytes(dek));
      return { wrappedB64: bytesToB64(wrapped), ivB64: bytesToB64(iv) };
    },
    unwrapDEK: async function (wrappedB64, ivB64, password, salt) {
      var kek = await deriveKEK(password, salt);
      var plain = await subtle.decrypt({ name: 'AES-GCM', iv: b64ToBytes(ivB64) }, kek, b64ToBytes(wrappedB64));
      return new Uint8Array(plain);
    },
    // 附件通道：本地加密后上传密文，服务端永不接触明文图片。
    encryptBlob: async function (dek, blob) {
      requireCrypto();
      var buffer = new Uint8Array(await blob.arrayBuffer());
      var key = await importDEK(dek, ['encrypt']);
      var iv = webCrypto.getRandomValues(new Uint8Array(12));
      var ciphertext = await subtle.encrypt({ name: 'AES-GCM', iv: iv }, key, buffer);
      return {
        ivB64: bytesToB64(iv),
        blob: new Blob([ciphertext], { type: 'application/octet-stream' }),
        name: blob.name || 'attachment.bin'
      };
    },
    decryptBlob: async function (dek, ivB64, blob) {
      requireCrypto();
      var buffer = new Uint8Array(await blob.arrayBuffer());
      var key = await importDEK(dek, ['decrypt']);
      var plaintext = await subtle.decrypt({ name: 'AES-GCM', iv: b64ToBytes(ivB64) }, key, buffer);
      return new Uint8Array(plaintext);
    },
    encrypt: async function (dek, plain) {
      var key = await importDEK(dek, ['encrypt']);
      var iv = webCrypto.getRandomValues(new Uint8Array(12));
      var ciphertext = await subtle.encrypt({ name: 'AES-GCM', iv: iv }, key, encoder.encode(String(plain)));
      return { ivB64: bytesToB64(iv), ctB64: bytesToB64(ciphertext) };
    },
    decrypt: async function (dek, ivB64, ctB64) {
      var key = await importDEK(dek, ['decrypt']);
      var plain = await subtle.decrypt({ name: 'AES-GCM', iv: b64ToBytes(ivB64) }, key, b64ToBytes(ctB64));
      return decoder.decode(plain);
    }
  };
})(window);

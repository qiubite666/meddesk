'use strict';

// BUILD_ID 由 Go 服务端在启动时注入（内嵌前端全部文件的内容哈希）。
// 每次部署新版本二进制后它必然变化 → 缓存名变化 → activate 自动清理旧缓存，
// 从根本上杜绝「发版后页面仍用旧 JS」的问题。
const BUILD_ID = '__BUILD_ID__';
// 哨兵拆写（'__'+'BUILD_ID__'）：服务端会把连续的 __BUILD_ID__ 整串替换成真实指纹，
// 若比较表达式里也写占位符会被一并替换导致条件恒 false、V 恒空串——
// 后果：SHELL 预缓存键丢失 ?v= 指纹，与页面实际请求（带指纹）键不一致，
// SW 缓存永不命中，离线 PWA 加载不到 JS。拆写后 dev 模式（无注入）才回退空 V。
const DEV_SENTINEL = '__' + 'BUILD_ID__';
const V = BUILD_ID === DEV_SENTINEL ? '' : '?v=' + BUILD_ID;
const CACHE_NAME = 'meddesk-shell-' + (BUILD_ID || 'dev');

// 预缓存列表：静态资源统一带 ?v= 指纹参数，与服务端注入到 index.html 的引用一致，
// 保证 SW 缓存键与页面请求键完全相同。
const SHELL_ASSETS = [
  '/manifest.webmanifest',
  '/assets/css/app.css',
  '/assets/js/core.js',
  '/assets/js/crypto.js',
  '/assets/js/db.js',
  '/assets/js/api.js',
  '/assets/js/data.js',
  '/assets/js/sync.js',
  '/assets/js/schedule.js',
  '/assets/js/remind.js',
  '/assets/js/med.js',
  '/assets/js/export.js',
  '/assets/js/app.js'
].map(function (u) { return u + V; });
const SHELL = ['/', '/index.html'].concat(SHELL_ASSETS);

self.addEventListener('install', function (event) {
  event.waitUntil(caches.open(CACHE_NAME).then(function (cache) {
    return cache.addAll(SHELL);
  }).then(function () {
    return self.skipWaiting();
  }));
});

self.addEventListener('activate', function (event) {
  event.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (key) {
      return key !== CACHE_NAME;
    }).map(function (key) {
      return caches.delete(key);
    }));
  }).then(function () {
    return self.clients.claim();
  }));
});

self.addEventListener('fetch', function (event) {
  const request = event.request;
  const url = new URL(request.url);

  if (url.origin !== self.location.origin || url.pathname.startsWith('/api/')) {
    return;
  }
  if (request.method !== 'GET') {
    return;
  }

  // 页面导航（HTML）走 network-first：每次打开优先拿最新 index.html
  // （含服务端注入的版本号与 ?v= 指纹引用），失败才回退缓存 —— 保证发版后
  // 打开即加载新外壳，杜绝「新 JS + 旧 HTML」的混合缓存导致的版本显示错误。
  // 带 ?v= 指纹的静态资源 URL 永不变化地对应同一内容，cache-first 依然安全。
  if (request.mode === 'navigate' || url.pathname === '/' || url.pathname === '/index.html') {
    event.respondWith(fetch(request).then(function (response) {
      if (response && response.ok) {
        const copy = response.clone();
        caches.open(CACHE_NAME).then(function (cache) { cache.put('/index.html', copy); });
      }
      return response;
    }).catch(function () {
      return caches.match('/index.html').then(function (cached) {
        return cached || caches.match('/');
      });
    }));
    return;
  }

  event.respondWith(caches.match(request).then(function (cached) {
    if (cached) {
      return cached;
    }
    return fetch(request).then(function (response) {
      if (!response || !response.ok) {
        return response;
      }
      const copy = response.clone();
      caches.open(CACHE_NAME).then(function (cache) {
        cache.put(request, copy);
      });
      return response;
    });
  }));
});


// 点击通知：聚焦已打开的应用窗口，否则新开一个（移动端 PWA 点击提醒直达页面）。
self.addEventListener('notificationclick', function (event) {
  event.notification.close();
  event.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (list) {
    for (var i = 0; i < list.length; i++) {
      var client = list[i];
      if ('focus' in client) return client.focus();
    }
    return self.clients.openWindow('/');
  }));
});

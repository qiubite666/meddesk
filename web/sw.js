'use strict';

// BUILD_ID 由 Go 服务端在启动时注入（内嵌前端全部文件的内容哈希）。
// 每次部署新版本二进制后它必然变化 → 缓存名变化 → activate 自动清理旧缓存，
// 从根本上杜绝「发版后页面仍用旧 JS」的问题。
const BUILD_ID = '__BUILD_ID__';
const V = BUILD_ID && BUILD_ID !== '__BUILD_ID__' ? '?v=' + BUILD_ID : '';
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

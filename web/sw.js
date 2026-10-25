'use strict';

const CACHE_NAME = 'meddesk-shell-v1';
const SHELL = [
  '/',
  '/index.html',
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
];

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

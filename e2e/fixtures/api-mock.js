// Route-mock для /api/* — детерминированный бэк без token-svc/LiveKit.
// Используется в MOCK-режиме. Позволяет настраивать:
//   - gate: открыт (auth 200) / закрыт (auth 401)
//   - список палат
//   - ответ /api/token (валидный токен → join дойдёт до LiveKit и упадёт;
//     это ожидаемый негативный путь, т.к. живого LiveKit в mock нет)

import { test as base, expect } from '@playwright/test';

/**
 * installApiMock навешивает перехват /api/* на page.
 * @param {import('@playwright/test').Page} page
 * @param {object} opts
 * @param {boolean} [opts.gateOpen=true] - true: /api/auth GET→200 (сайт открыт)
 * @param {string}  [opts.password]      - если задан, POST /api/auth сверяет пароль
 * @param {Array}   [opts.rooms]         - список палат для /api/rooms
 */
export async function installApiMock(page, opts = {}) {
  const gateOpen = opts.gateOpen !== false;
  const password = opts.password;
  const rooms = opts.rooms || [];

  await page.route('**/api/**', async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const path = url.pathname;
    const method = req.method();

    // --- gate ---
    if (path === '/api/auth' && method === 'GET') {
      return route.fulfill({ status: gateOpen ? 200 : 401, body: '' });
    }
    if (path === '/api/auth' && method === 'POST') {
      let body = {};
      try { body = JSON.parse(req.postData() || '{}'); } catch { /* ignore */ }
      const ok = password == null || body.password === password;
      return route.fulfill({
        status: ok ? 200 : 401,
        headers: ok ? { 'set-cookie': 'meetler_auth=mock; Path=/' } : {},
        body: '',
      });
    }

    // --- список палат ---
    if (path === '/api/rooms' && method === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(rooms),
      });
    }

    // --- token: отдаём фейковый токен. join() дойдёт до room.connect() и
    //     упадёт (нет LiveKit) — это ожидаемый негативный сценарий. ---
    if (path === '/api/token' && method === 'POST') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ token: 'mock.jwt.token' }),
      });
    }

    // --- per-room ресурсы: пустые, но валидные ---
    if (/\/votes$/.test(path)) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
    }
    if (/\/(sounds|avatars)$/.test(path) && method === 'GET') {
      return route.fulfill({ status: 200, contentType: 'application/json', body: '[]' });
    }
    if (/\/(welcome|farewell)$/.test(path)) {
      return route.fulfill({ status: 404, body: '' }); // дефолтный звук
    }

    // всё прочее — 404 (фронт молча проглатывает)
    return route.fulfill({ status: 404, body: '' });
  });
}

// расширенный test с авто-установкой дефолтного mock (gate открыт, палат нет).
export const test = base.extend({
  apiMock: [async ({ page }, use) => {
    await installApiMock(page);
    await use(installApiMock);
  }, { auto: false }],
});

export { expect };

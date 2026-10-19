// Playwright config для meetler web-клиента.
//
// Две модели прогона:
//   1) MOCK (по умолчанию): статика web/ раздаётся локальным http-сервером,
//      все /api/* перехватываются route-mock'ом (fixtures/api-mock.js). Живой
//      LiveKit/token-svc НЕ нужен — детерминированно, быстро, годится для CI
//      на каждый push. Покрывает gate, приёмное, рендер палат, негативный join.
//   2) LIVE (E2E_BASE_URL задан): гоним против уже поднятого полного стека
//      (docker compose / NixOS). Тогда webServer не стартуем, mock не вешаем —
//      тесты, помеченные @live, работают против реального бэка.
//
// Браузер: только chromium — WebRTC-поведение (getUserMedia, fake media)
// стабильнее всего воспроизводится в chromium с fake-device флагами.

import { defineConfig, devices } from '@playwright/test';

const LIVE = !!process.env.E2E_BASE_URL;
const PORT = process.env.E2E_PORT || '4173';
const baseURL = process.env.E2E_BASE_URL || `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  timeout: 30_000,
  expect: { timeout: 5_000 },

  use: {
    baseURL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    // secure-context флаги: сайт требует getUserMedia; на http://127.0.0.1
    // chromium считает loopback secure, но даём fake-устройства без запроса прав.
    launchOptions: {
      args: [
        '--use-fake-ui-for-media-stream',
        '--use-fake-device-for-media-stream',
        '--autoplay-policy=no-user-gesture-required',
      ],
    },
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  // MOCK-режим: поднимаем статику web/ локально. В LIVE-режиме не трогаем.
  webServer: LIVE
    ? undefined
    : {
        command: `node serve-static.js ${PORT}`,
        url: baseURL,
        reuseExistingServer: !process.env.CI,
        timeout: 15_000,
      },
});

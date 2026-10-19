// Join: валидация формы + негативный путь без LiveKit.
// В MOCK-режиме /api/token отдаёт токен, но room.connect() упадёт (нет SFU) —
// проверяем, что UI корректно обрабатывает сбой: остаётся в приёмном, #err.

import { test, expect, installApiMock } from '../fixtures/api-mock.js';

test.describe('вход в палату', () => {
  test.beforeEach(async ({ page }) => {
    await installApiMock(page, { gateOpen: true });
  });

  test('пустая палата → ошибка валидации, не уходим в stage', async ({ page }) => {
    await page.goto('/');
    await page.locator('#room').fill('');
    await page.locator('#joinBtn').click();
    await expect(page.locator('#err')).toContainText(/палат/i);
    await expect(page.locator('#stage')).toBeHidden();
    await expect(page.locator('#join')).toBeVisible();
  });

  test('ввод имени палаты меняет URL (replaceState) при попытке входа', async ({ page }) => {
    await page.goto('/');
    await page.locator('#room').fill('test-room');
    await page.locator('#joinBtn').click();
    // join() делает history.replaceState('/test-room') до connect
    await expect(page).toHaveURL(/\/test-room$/);
  });

  test('сбой LiveKit-коннекта → остаёмся в приёмном с ошибкой', async ({ page }) => {
    await page.goto('/');
    await page.locator('#room').fill('test-room');
    await page.locator('#name').fill('Tester');
    await page.locator('#joinBtn').click();
    // room.connect к несуществующему SFU упадёт → #err "Ошибка входа"
    await expect(page.locator('#err')).toContainText(/ошибк/i, { timeout: 15_000 });
    await expect(page.locator('#stage')).toBeHidden();
    await expect(page.locator('#joinBtn')).toBeEnabled();
  });
});

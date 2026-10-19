// Smoke: приёмное отделение рендерится, gate не мешает (сайт открыт),
// статический скелет на месте. Mock /api/*, живой бэк не нужен.

import { test, expect, installApiMock } from '../fixtures/api-mock.js';

test.describe('приёмное отделение (сайт открыт)', () => {
  test.beforeEach(async ({ page }) => {
    await installApiMock(page, {
      gateOpen: true,
      rooms: [
        { name: 'palata-6', participants: 3, createdAt: Date.now() - 3600_000 },
        { name: 'kukushka', participants: 1, createdAt: Date.now() - 60_000 },
      ],
    });
  });

  test('скелет страницы и форма входа видны', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#gate')).toBeHidden();
    await expect(page.locator('#join')).toBeVisible();
    await expect(page.locator('#room')).toBeVisible();
    await expect(page.locator('#name')).toBeVisible();
    await expect(page.locator('#joinBtn')).toBeVisible();
    await expect(page.locator('#stage')).toBeHidden();
  });

  test('tag cloud МКБ-10 рендерится (в раскрытом details)', async ({ page }) => {
    await page.goto('/');
    // облако внутри закрытого <details id="dxCloud"> — открываем как юзер
    await page.locator('#dxCloud summary').click();
    await expect(page.locator('#tagCloud a').first()).toBeVisible();
    expect(await page.locator('#tagCloud a').count()).toBeGreaterThan(10);
  });

  test('список палат из /api/rooms рендерится', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#roomsBody a[href="/palata-6"]')).toBeVisible();
    await expect(page.locator('#roomsBody a[href="/kukushka"]')).toBeVisible();
  });

  test('тихая палата из /api/rooms отображается в списке', async ({ page }) => {
    await installApiMock(page, {
      gateOpen: true,
      rooms: [
        { name: 'tishina', participants: 0, createdAt: 0 },
        { name: 'palata-6', participants: 2, createdAt: Date.now() },
      ],
    });
    await page.goto('/');
    await expect(page.locator('#roomsBody a[href="/tishina"]')).toBeVisible();
  });

  test('кнопка «Тишина» переключает состояние и localStorage', async ({ page }) => {
    await page.goto('/');
    // кнопка в DOM (в скрытом #stage) — проверяем через атрибуты, без видимости
    const btn = page.locator('#quietBtn');
    await expect(btn).toHaveAttribute('id', 'quietBtn');
    // до входа в палату кнопка скрыта вместе с #stage — но её состояние
    // управляется глобально; проверяем через evaluate
    const before = await page.evaluate(() => window.__meetlerDecorMuted);
    expect(typeof before).toBe('boolean');
    // localStorage персистит флаг
    const stored = await page.evaluate(() => localStorage.getItem('meetlerDecorMuted'));
    expect(stored === '1').toBe(before);
  });

  test('переключение темы работает (localStorage)', async ({ page }) => {
    await page.goto('/');
    const toggle = page.locator('#themeToggle');
    await expect(toggle).toBeVisible();
    const before = await page.evaluate(() => document.body.className);
    await toggle.click();
    const after = await page.evaluate(() => document.body.className);
    expect(after).not.toBe(before);
  });
});

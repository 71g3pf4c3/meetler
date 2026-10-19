// Gate: центральный пароль. GET /api/auth 401 → показ gate; POST с верным
// паролем → скрытие; с неверным → ошибка.

import { test, expect, installApiMock } from '../fixtures/api-mock.js';

test.describe('gate (общий пароль)', () => {
  test('gate показывается при закрытом сайте (/api/auth 401)', async ({ page }) => {
    await installApiMock(page, { gateOpen: false, password: 'hunter2' });
    await page.goto('/');
    await expect(page.locator('#gate')).toBeVisible();
    await expect(page.locator('#gatePass')).toBeVisible();
    await expect(page.locator('#gateBtn')).toBeVisible();
    // приёмное за гейтом перекрыто
    await expect(page.locator('#gate')).toBeVisible();
  });

  test('верный пароль скрывает gate', async ({ page }) => {
    await installApiMock(page, { gateOpen: false, password: 'hunter2' });
    await page.goto('/');
    await page.locator('#gatePass').fill('hunter2');
    await page.locator('#gateBtn').click();
    await expect(page.locator('#gate')).toBeHidden();
    await expect(page.locator('#join')).toBeVisible();
  });

  test('неверный пароль показывает ошибку, gate остаётся', async ({ page }) => {
    await installApiMock(page, { gateOpen: false, password: 'hunter2' });
    await page.goto('/');
    await page.locator('#gatePass').fill('wrong');
    await page.locator('#gateBtn').click();
    await expect(page.locator('#gateErr')).toContainText(/неверный/i);
    await expect(page.locator('#gate')).toBeVisible();
  });
});

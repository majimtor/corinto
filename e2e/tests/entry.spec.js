// The demo entry, in every browser engine and with scripts off: the panel
// has none, and what it needs of a browser is that it keeps one cookie.
import { expect, test } from '@playwright/test';
import { text, workspace } from './panel.js';

test.use({ javaScriptEnabled: false });

test('the demo entry opens a session the browser keeps, and leaving ends it', async ({ page, context, baseURL }) => {
  await page.goto('/es/');
  await page.getByRole('button', { name: text('entry.enter', { name: workspace }) }).click();
  await expect(page).toHaveURL(/\/es\/links$/);
  await expect(page.getByRole('heading', { level: 1, name: text('links.title') })).toBeVisible();

  // One cookie, which a script cannot read and which goes with the
  // browser. Over HTTPS it is the one with the prefix; the local run has
  // the form that a loopback address alone is given.
  const secure = new URL(baseURL).protocol === 'https:';
  const cookies = await context.cookies();
  expect(cookies).toHaveLength(1);
  expect(cookies[0]).toMatchObject({
    name: secure ? '__Host-session' : 'session', path: '/', httpOnly: true, secure, sameSite: 'Lax', expires: -1,
  });

  // The browser sends it back: another page behind the session answers.
  await page.goto('/es/keys');
  await expect(page.getByRole('heading', { level: 1, name: text('keys.title') })).toBeVisible();

  await page.getByRole('button', { name: text('layout.leave') }).click();
  await expect(page).toHaveURL(/\/es\/$/);
  expect(await context.cookies()).toHaveLength(0);
  await page.goto('/es/links');
  await expect(page).toHaveURL(/\/es\/$/);
});

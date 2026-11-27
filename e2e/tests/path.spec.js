// The product as a whole (PLAN.md section 12): enter, create a link, follow
// it, see it counted; and the rest of what the panel has, with axe on
// every page. It leaves the demo workspace as it found it.
import { expect, test } from '@playwright/test';
import { accessible, counted, redirect, text, workspace } from './panel.js';

// figure returns where a page of a link shows one of its figures.
function figure(page, key) {
  return page.locator('.figure').filter({ has: page.getByText(text(key), { exact: true }) }).locator('.value');
}

test('a link from the door to its figures, the keys and the status', async ({ page, context, request }) => {
  const stamp = Date.now();
  const destination = `https://www.example.com/e2e/${stamp}`;
  const title = `Browser test ${stamp}`;
  let identifier;

  await test.step('enter a demo workspace', async () => {
    await page.goto('/');
    await expect(page).toHaveURL(/\/es\/$/);
    await expect(page.getByRole('heading', { level: 1, name: text('entry.title') })).toBeVisible();
    await accessible(page, '01-door');
    await page.getByRole('button', { name: text('entry.enter', { name: workspace }) }).click();
    await expect(page).toHaveURL(/\/es\/links$/);
    await accessible(page, '02-links');
  });

  await test.step('create a link, after a form that is wrong', async () => {
    await page.getByRole('link', { name: text('links.new') }).click();
    await accessible(page, '03-link-form');
    await page.getByRole('button', { name: text('form.create') }).click();
    await expect(page.getByRole('alert')).toContainText(text('error.destination.required'));
    await expect(page).toHaveTitle(new RegExp(`^${text('form.error_mark')}`));
    await accessible(page, '04-link-form-wrong');
    await page.getByLabel(text('form.destination.label'), { exact: true }).fill(destination);
    await page.getByLabel(text('form.title.label'), { exact: true }).fill(title);
    await page.getByRole('button', { name: text('form.create') }).click();
    await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible();
    await expect(figure(page, 'metric.requests')).toHaveText('0');
    const short = (await page.locator('dl.facts dd.address').first().innerText()).trim();
    identifier = short.slice(short.indexOf('/') + 1);
    expect(identifier).toMatch(/^[a-z0-9-]{3,40}$/);
  });

  await test.step('follow the short link and see it counted', async () => {
    // As a client follows it, without going where it leads. The redirect
    // answers from a copy of the links that takes a change within a couple
    // of seconds: until then it does not know the link, and an unknown
    // link leaves no event.
    let followed;
    await expect(async () => {
      followed = await request.get(`${redirect}/${identifier}`, { maxRedirects: 0 });
      expect(followed.status()).toBe(302);
    }).toPass({ timeout: 15_000 });
    expect(followed.headers().location).toBe(destination);
    // The worker counts it within a few seconds.
    await expect(async () => {
      await page.reload();
      await expect(figure(page, 'metric.requests')).toHaveText('1', { timeout: 1000 });
    }).toPass({ timeout: 30_000 });
    await expect(figure(page, 'metric.estimated_human_clicks')).toHaveText('1');
    await accessible(page, '05-link');
    await page.getByRole('link', { name: counted('period.days', 90), exact: true }).click();
    await expect(page).toHaveURL(/\?days=90$/);
    await expect(figure(page, 'metric.requests')).toHaveText('1');
    await accessible(page, '06-link-ninety-days');
  });

  await test.step('change the link, disable it and delete it', async () => {
    await page.getByRole('link', { name: text('link.edit'), exact: true }).click();
    await accessible(page, '07-link-edit');
    await page.getByLabel(text('form.title.label'), { exact: true }).fill(`${title}, changed`);
    await page.getByRole('button', { name: text('form.save') }).click();
    await expect(page.getByRole('heading', { level: 1, name: `${title}, changed` })).toBeVisible();

    await page.getByRole('button', { name: text('link.disable'), exact: true }).click();
    await expect(page.getByText(text('link.state.disabled'), { exact: true })).toBeVisible();
    // The copy of the redirect takes this change too, within its seconds.
    await expect(async () => {
      const refused = await request.get(`${redirect}/${identifier}`, { maxRedirects: 0 });
      expect(refused.status()).toBe(404);
    }).toPass({ timeout: 15_000 });

    await page.getByRole('link', { name: text('link.delete'), exact: true }).click();
    await accessible(page, '08-link-delete');
    await page.getByRole('button', { name: text('delete.confirm') }).click();
    await expect(page).toHaveURL(/\/es\/links$/);
    await expect(page.getByRole('link', { name: `${title}, changed` })).toHaveCount(0);
  });

  await test.step('issue a key, use it and revoke it', async () => {
    const name = `Browser test ${stamp}`;
    await page.getByRole('link', { name: text('layout.keys'), exact: true }).click();
    await expect(page.getByRole('heading', { level: 1, name: text('keys.title') })).toBeVisible();
    await accessible(page, '09-keys');
    await page.getByRole('link', { name: text('keys.new') }).click();
    await accessible(page, '10-key-form');
    await page.getByLabel(text('keys.name'), { exact: true }).fill(name);
    await page.getByRole('checkbox', { name: /links:read/ }).check();
    await page.getByRole('button', { name: text('keyform.create') }).click();
    await expect(page.getByRole('heading', { level: 1, name: text('issued.title') })).toBeVisible();
    const shown = page.getByLabel(text('issued.label'), { exact: true });
    const credential = await shown.inputValue();
    expect(credential).toMatch(/^cor_[0-9a-f]{32}_[a-z2-7]{52}$/);
    await accessible(page, '11-key-issued', { hide: [shown] });

    // The key reads the links of the workspace, and cannot write them.
    const authorization = { Authorization: `Bearer ${credential}` };
    expect((await request.get('/v1/links', { headers: authorization })).status()).toBe(200);
    expect((await request.post('/v1/links', { headers: authorization, data: { destination } })).status()).toBe(403);

    await page.getByRole('link', { name: text('issued.back') }).click();
    await expect(page.getByRole('heading', { level: 2, name })).toBeVisible();
    await page.getByRole('link', { name: text('keys.revoke_named', { name }) }).click();
    await accessible(page, '12-key-revoke');
    await page.getByRole('button', { name: text('revoke.confirm') }).click();
    await expect(page).toHaveURL(/\/es\/keys$/);
    await expect(page.getByRole('heading', { level: 2, name })).toHaveCount(0);
    expect((await request.get('/v1/links', { headers: authorization })).status()).toBe(401);
  });

  await test.step('read what became of the events', async () => {
    await page.getByRole('link', { name: text('layout.status'), exact: true }).click();
    await expect(page.getByRole('heading', { level: 1, name: text('status.title') })).toBeVisible();
    await expect(page.getByRole('table', { name: text('status.caption') })).toBeVisible();
    await accessible(page, '13-status');
    await page.getByRole('link', { name: counted('period.days', 31), exact: true }).click();
    await expect(page).toHaveURL(/\?days=31$/);
    await expect(page.locator('tbody tr')).toHaveCount(31);
    await accessible(page, '14-status-thirty-one-days');
  });

  await test.step('leave', async () => {
    await page.getByRole('button', { name: text('layout.leave') }).click();
    await expect(page).toHaveURL(/\/es\/$/);
    expect(await context.cookies()).toHaveLength(0);
    await page.goto('/es/links');
    await expect(page).toHaveURL(/\/es\/$/);
  });
});

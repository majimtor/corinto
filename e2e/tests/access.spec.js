// What the review of accessibility looked at that a test can keep looking
// at (docs/accessibility.md): the keyboard and its focus, the titles, the
// language, what is not told by colour alone, what is wrong with a form
// and the narrow screen. One session, as one person going through the
// panel.
import { expect, test } from '@playwright/test';
import { accessible, counted, text, workspace } from './panel.js';

// What the keyboard stops at.
const stops = 'a[href], button, input:not([type="hidden"]), select, textarea, summary, [tabindex="0"]';

// keyboard goes through a page with the Tab key alone, from its top. It
// fails unless the focus visits every stop of the page once, in the order
// of the document, which is also how it shows that nothing holds the
// keyboard (2.1.1, 2.1.2); unless each stop shows the ring of the focus
// (2.4.7); and unless each is in the window when it has the focus (2.4.11):
// no part of a page stays in place over the rest, which the last step of
// the test checks, so what is in the window is in sight.
async function keyboard(page, name) {
  // A page that was just loaded: the keyboard starts at its top.
  await page.reload();
  const expected = await page.locator(stops).evaluateAll((all) => all.filter((element) => element.checkVisibility()).length);
  expect(expected, `the page ${name} has stops for the keyboard`).toBeGreaterThan(1);
  const seen = [];
  // A field of date and time is one stop with several parts, and the
  // keyboard goes through each: so there are more presses than stops.
  for (let presses = 0; presses < expected + 40; presses += 1) {
    await page.keyboard.press('Tab');
    const at = await page.evaluate((selector) => {
      const all = [...document.querySelectorAll(selector)].filter((element) => element.checkVisibility());
      const focused = document.activeElement;
      const style = getComputedStyle(focused);
      const box = focused.getBoundingClientRect();
      return {
        index: all.indexOf(focused),
        ring: style.outlineStyle !== 'none' && parseFloat(style.outlineWidth) >= 2,
        inSight: box.bottom > 0 && box.top < window.innerHeight && box.right > 0 && box.left < window.innerWidth,
      };
    }, stops);
    if (seen.length === 0 || seen[seen.length - 1].index !== at.index) {
      seen.push(at);
    }
    if (at.index === expected - 1) {
      break;
    }
  }
  expect(seen.map((stop) => stop.index), `the order of the keyboard on the page ${name}`).toEqual([...Array(expected).keys()]);
  expect(seen.filter((stop) => !stop.ring).map((stop) => stop.index), `stops without the ring of the focus on the page ${name}`).toEqual([]);
  expect(seen.filter((stop) => !stop.inSight).map((stop) => stop.index), `stops out of the window on the page ${name}`).toEqual([]);
}

test('the keyboard, the titles, the language, colour and the narrow screen', async ({ page }) => {
  const site = ' · Corinto';
  let link;

  await test.step('the door: its language, its title and the link that skips to the content', async () => {
    await page.goto('/es/');
    await expect(page.locator('html')).toHaveAttribute('lang', 'es');
    await expect(page).toHaveTitle(text('entry.title') + site);
    // The other language is offered in that language, and says so (3.1.2).
    const other = page.locator('.language a');
    await expect(other).toHaveAttribute('lang', 'en');
    await expect(other).toHaveAttribute('hreflang', 'en');
    // The first stop of the keyboard leads past the bar, to the content.
    await page.keyboard.press('Tab');
    await expect(page.getByRole('link', { name: text('layout.skip') })).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(page.locator('main')).toBeFocused();
    await keyboard(page, 'door');
    // A page says the language it is in (3.1.1), and its title is in it.
    await other.click();
    await expect(page.locator('html')).toHaveAttribute('lang', 'en');
    await expect(page).not.toHaveTitle(text('entry.title') + site);
    await page.locator('.language a').click();
    await expect(page.locator('html')).toHaveAttribute('lang', 'es');
  });

  await test.step('the list and the page of a link: the title names the view and its period', async () => {
    await page.getByRole('button', { name: text('entry.enter', { name: workspace }) }).click();
    await expect(page).toHaveTitle(text('links.title') + site);
    await keyboard(page, 'links');
    const first = page.locator('tbody th a').first();
    link = (await first.innerText()).trim();
    await first.click();
    await expect(page).toHaveTitle(text('link.page_title', { name: link, days: 30 }) + site);
    await keyboard(page, 'link');
    const thirty = await page.title();
    await page.getByRole('link', { name: counted('period.days', 90), exact: true }).click();
    await expect(page).toHaveTitle(text('link.page_title', { name: link, days: 90 }) + site);
    // The titles are put together from the catalogue, which could leave
    // the period out of both: the title has the number of its days in it,
    // and is not the one of the other period.
    expect(thirty).toContain('30');
    expect(await page.title()).toContain('90');
    expect(await page.title()).not.toBe(thirty);
  });

  await test.step('nothing on the page of a link is told by its colour alone', async () => {
    // The two series differ in their dashes and in the shape of their
    // markers, in the drawing and in its legend (1.4.1).
    const dashes = await page.locator('svg.chart polyline').evaluateAll((lines) => lines.map((line) => getComputedStyle(line).strokeDasharray));
    expect(dashes).toHaveLength(2);
    expect(dashes[0]).toBe('none');
    expect(dashes[1]).not.toBe('none');
    expect(await page.locator('svg.chart circle.marker-1').count()).toBeGreaterThan(0);
    expect(await page.locator('svg.chart rect.marker-2').count()).toBeGreaterThan(0);
    expect(await page.locator('.legend circle').count()).toBe(1);
    expect(await page.locator('.legend rect').count()).toBe(1);
    // The period that is shown is heavier than the others and has a rule
    // under it, and a screen reader is told which it is.
    const shown = page.locator('.periods a[aria-current="page"]');
    await expect(shown).toHaveCount(1);
    const marks = await page.locator('.periods a').evaluateAll((all) => all.map((choice) => {
      const style = getComputedStyle(choice);
      return { current: choice.hasAttribute('aria-current'), heavy: Number(style.fontWeight) >= 600, ruled: style.borderBottomColor !== 'rgba(0, 0, 0, 0)' };
    }));
    expect(marks.filter((mark) => mark.current !== mark.heavy || mark.current !== mark.ruled)).toEqual([]);
    // A link in a sentence is underlined.
    expect(await page.locator('nav.actions a').first().evaluate((a) => getComputedStyle(a).textDecorationLine)).toBe('underline');
    // The table of the series opens with the keyboard.
    const table = page.locator('details');
    await table.locator('summary').focus();
    await page.keyboard.press('Enter');
    await expect(table.getByRole('table')).toBeVisible();
    await expect(table.locator('tbody tr')).toHaveCount(90);
  });

  await test.step('what is wrong with a form is said in text, next to its field and to a screen reader', async () => {
    await page.goto('/es/links/new');
    await keyboard(page, 'link-form');
    await page.getByRole('button', { name: text('form.create') }).click();
    const wrong = text('error.destination.required');
    const field = page.getByLabel(text('form.destination.label'), { exact: true });
    await expect(field).toHaveAttribute('aria-invalid', 'true');
    await expect(field).toHaveAccessibleDescription(new RegExp(wrong.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    await expect(page.locator('#destination-error')).toHaveText(wrong);
    // The summary at the top leads to the field (3.3.1).
    await page.getByRole('alert').getByRole('link', { name: wrong }).click();
    await expect(field).toBeFocused();
  });

  await test.step('the keys and the status', async () => {
    await page.goto('/es/keys');
    await expect(page).toHaveTitle(text('keys.title') + site);
    await keyboard(page, 'keys');
    await page.goto('/es/keys/new');
    await keyboard(page, 'key-form');
    await page.goto('/es/status');
    await expect(page).toHaveTitle(text('status.page_title', { days: 7 }) + site);
    await keyboard(page, 'status');
    const week = await page.title();
    await page.getByRole('link', { name: counted('period.days', 31), exact: true }).click();
    await expect(page).toHaveTitle(text('status.page_title', { days: 31 }) + site);
    expect(week).toContain('7');
    expect(await page.title()).toContain('31');
    expect(await page.title()).not.toBe(week);
  });

  await test.step('a screen 320 pixels wide: no page is wider, and what is wider than it scrolls in a region of its own', async () => {
    await page.setViewportSize({ width: 320, height: 640 });
    for (const [name, path] of [
      ['narrow-links', '/es/links'], ['narrow-link-form', '/es/links/new'], ['narrow-keys', '/es/keys'],
      ['narrow-key-form', '/es/keys/new'], ['narrow-status', '/es/status?days=31'],
    ]) {
      await page.goto(path);
      await accessible(page, name);
      await keyboard(page, name);
    }
    await page.goto('/es/links');
    await page.locator('tbody th a').first().click();
    await page.getByRole('link', { name: counted('period.days', 90), exact: true }).click();
    await page.locator('details summary').click();
    await accessible(page, 'narrow-link');
    // A table or a drawing wider than the screen is inside a region that
    // scrolls by itself, has a name and takes the keyboard (1.4.10).
    const wide = await page.locator('table, svg.chart').evaluateAll((all) => all
      .filter((element) => element.getBoundingClientRect().width > window.innerWidth)
      .map((element) => {
        const region = element.closest('.scroll');
        return Boolean(region) && region.tabIndex === 0 && region.getAttribute('role') === 'group' && region.hasAttribute('aria-labelledby');
      }));
    expect(wide.length).toBeGreaterThan(0);
    expect(wide.filter((inside) => !inside)).toEqual([]);
    // No part of a page stays in place over the rest, where it could hide
    // what has the focus (2.4.11).
    expect(await page.locator('body *').evaluateAll((all) => all.filter((element) => ['fixed', 'sticky'].includes(getComputedStyle(element).position)).length)).toBe(0);
  });
});

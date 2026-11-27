// What the browser tests share: the texts of the panel, the check of
// accessibility and the screenshot of each page.
import { readFileSync } from 'node:fs';
import { AxeBuilder } from '@axe-core/playwright';
import { expect } from '@playwright/test';

// The tests find what they press and read by the texts of the Spanish
// catalogue, which they read from the file the panel embeds: a text that
// changes there changes here with it, and no test holds a word of its own.
const catalogue = JSON.parse(readFileSync(new URL('../../internal/panel/catalog/es.json', import.meta.url), 'utf8'));

// The first demo workspace of the seed, which the tests enter.
export const workspace = 'Lighthouse Books';

// Where the short links are served.
export const redirect = process.env.REDIRECT_URL ?? 'http://127.0.0.1:8082';

// text returns a text of the catalogue with its values in their places.
export function text(key, values = {}) {
  const found = catalogue[key];
  if (typeof found !== 'string') {
    throw new Error(`the catalogue has no text ${key}, or it is one that counts`);
  }
  return found.replace(/\{([a-z_]+)\}/g, (_, name) => {
    if (!(name in values)) {
      throw new Error(`the text ${key} was given no value for ${name}`);
    }
    return String(values[name]);
  });
}

// counted returns a text of the catalogue that counts something, in the
// form of its number.
export function counted(key, n) {
  const found = catalogue[key];
  if (typeof found !== 'object') {
    throw new Error(`the catalogue has no text ${key} that counts`);
  }
  return (n === 1 ? found.one : found.other).replace('{n}', String(n));
}

// accessible fails unless axe finds no violation on a page, and keeps a
// screenshot of the page under a name. axe runs every rule it runs unasked,
// the ones of WCAG at levels A and AA and its best practices, and the rule
// of the size of a target, the only one of WCAG 2.2 it has, which is off
// unless it is asked for (PLAN.md section 16). What a screenshot must not
// show, such as a key, is covered in it.
export async function accessible(page, name, { hide = [] } = {}) {
  const results = await new AxeBuilder({ page })
    .options({ rules: { 'target-size': { enabled: true } } })
    .analyze();
  await page.screenshot({ path: `screenshots/${name}.png`, fullPage: true, mask: hide });
  const found = results.violations.map((violation) => `${violation.id}: ${violation.nodes.map((node) => node.target.join(' ')).join(', ')}`);
  expect(found, `axe on the page ${name}`).toEqual([]);
  // A page that is wider than its screen is not one axe looks for.
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over, `the page ${name} is wider than its screen`).toBeLessThanOrEqual(0);
}

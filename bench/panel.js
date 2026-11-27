// The pages of the panel under a light and steady load, over the history
// of the seed: what PLAN.md section 15 asks of them. It is run through
// `make bench-panel`, which starts the stack this script reaches and seeds
// it with its ninety days.
//
// What is measured is the whole answer to a page, from the request to the
// last byte, as this generator sees it from inside the network of the
// containers: the queries of the page, its rendering and the hop between
// two containers. The objective is of the queries alone, so a page that
// meets it this way meets it.
import http from 'k6/http';
import exec from 'k6/execution';
import { check, fail } from 'k6';

const RATE = Number(__ENV.RATE || 10);
const DURATION = __ENV.DURATION || '60s';
const P95_MS = Number(__ENV.PANEL_P95_MS || 200);
const PANEL = __ENV.PANEL_URL || 'http://api:8080';

// The pages, each asked for as often as the others: the list of links,
// the page of the busiest link at its three periods, the keys and the
// status at its two.
const pages = ['links', 'link-7', 'link-30', 'link-90', 'keys', 'status-7', 'status-31'];

function seconds(duration) {
  const found = /^(\d+)(s|m)$/.exec(duration);
  if (!found) {
    fail(`the duration ${duration} is not a number of seconds or of minutes`);
  }
  return Number(found[1]) * (found[2] === 'm' ? 60 : 1);
}

// A run in which the generator leaves out more than one request in a
// thousand is not a measurement (PLAN.md section 15).
const MAY_DROP = Math.floor(RATE * seconds(DURATION) / 1000);

const thresholds = { checks: ['rate==1'], dropped_iterations: [`count<=${MAY_DROP}`] };
for (const page of pages) {
  thresholds[`http_req_duration{page:${page}}`] = [`p(95)<${P95_MS}`];
}

export const options = {
  scenarios: {
    pages: { executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 20, maxVUs: 100 },
  },
  thresholds,
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

// setup enters the first demo workspace as a browser does, and finds its
// first link, which is the one the seed gives the most traffic. The
// session is one for the whole run: the demo entry opens ten a minute for
// an address.
export function setup() {
  const door = http.get(`${PANEL}/en/`);
  const workspace = /name="workspace" value="([0-9a-f-]{36})"/.exec(door.body);
  if (door.status !== 200 || !workspace) {
    fail(`the door of the panel answered ${door.status}, or offers no demo workspace: was the stack seeded?`);
  }
  const entered = http.post(`${PANEL}/en/session`, { workspace: workspace[1] }, { redirects: 0 });
  const cookie = entered.cookies['__Host-session'];
  if (entered.status !== 303 || !cookie) {
    fail(`the demo entry answered ${entered.status} and no session: the stack of a run issues the cookie of an installation behind HTTPS`);
  }
  // The cookie is for HTTPS alone, and this network has none: it is sent
  // by hand, as the proxy of an installation would pass it on.
  const session = `__Host-session=${cookie[0].value}`;
  const list = http.get(`${PANEL}/en/links`, { headers: { Cookie: session }, redirects: 0 });
  const link = /href="\/en\/links\/([0-9a-f-]{36})"/.exec(list.body);
  if (list.status !== 200 || !link) {
    fail(`the list of links answered ${list.status}, or has no link`);
  }
  return { session, link: link[1] };
}

function path(page, link) {
  switch (page) {
    case 'links': return '/en/links';
    case 'link-7': return `/en/links/${link}?days=7`;
    case 'link-30': return `/en/links/${link}`;
    case 'link-90': return `/en/links/${link}?days=90`;
    case 'keys': return '/en/keys';
    case 'status-7': return '/en/status';
    default: return '/en/status?days=31';
  }
}

export default function (data) {
  const page = pages[exec.scenario.iterationInTest % pages.length];
  const answer = http.get(PANEL + path(page, data.link), { headers: { Cookie: data.session }, redirects: 0, tags: { page } });
  check(answer, { 'the page is served': (r) => r.status === 200 });
}

// The figures of the run, a line for each page, and what was missed.
export function handleSummary(data) {
  const lines = [];
  const ms = (value) => `${value.toFixed(1)} ms`;
  for (const page of pages) {
    const metric = data.metrics[`http_req_duration{page:${page}}`];
    if (!metric) {
      lines.push(`figure: page ${page}: not asked for`);
      continue;
    }
    const v = metric.values;
    lines.push(`figure: page ${page}: p50 ${ms(v.med)}, p95 ${ms(v['p(95)'])}, p99 ${ms(v['p(99)'])}, p99.9 ${ms(v['p(99.9)'])}, max ${ms(v.max)}`);
  }
  const sent = data.metrics.http_reqs ? data.metrics.http_reqs.values.count : 0;
  const dropped = data.metrics.dropped_iterations ? data.metrics.dropped_iterations.values.count : 0;
  lines.push(`figure: requests ${sent}, of which the run left out ${dropped}`);
  for (const [name, metric] of Object.entries(data.metrics)) {
    for (const [threshold, result] of Object.entries(metric.thresholds || {})) {
      if (!result.ok) {
        lines.push(`missed: ${name} ${threshold}`);
      }
    }
  }
  return { stdout: `${lines.join('\n')}\n` };
}

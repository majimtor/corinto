// The load scenario of the redirect (PLAN.md section 15, ADR 0020). It is
// run by bench/run.sh, inside the network of deploy/compose.bench.yaml,
// against a stack that was just seeded.
//
// Requests arrive at a constant rate whatever the server does with them:
// when it slows down, the load does not slow down with it. They are spread
// over many client addresses, each far under the rate limit of the
// redirect, which stays in the path. Meanwhile a link is disabled and
// enabled again and again, to measure how long it keeps redirecting.
//
// What the scenario is judged by is the service time the server measures
// of itself, read from its histogram before and after: the time the load
// generator measures has the network of the containers in it, and is
// reported next to it for what it is.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Gauge, Rate, Trend } from 'k6/metrics';

// What a run is told. The defaults are the burst of the plan.
const MODE = __ENV.MODE || 'burst';
const RATE = Number(__ENV.RATE || 300);
const DURATION = __ENV.DURATION || '10m';
const ADDRESSES = Number(__ENV.ADDRESSES || 1000);
const SWITCHES = Number(__ENV.SWITCHES || 100);
const SWITCH_EVERY = Number(__ENV.SWITCH_EVERY || 5);
// The objectives a run is held to. A smoke run widens them.
const SERVICE_P99_MS = Number(__ENV.SERVICE_P99_MS || 10);
const DEACTIVATION_P99_MS = Number(__ENV.DEACTIVATION_P99_MS || 5000);
// The break test raises the rate by a step every so long, up to a ceiling.
const BREAK_STEP = Number(__ENV.BREAK_STEP || 300);
const BREAK_EVERY = Number(__ENV.BREAK_EVERY || 30);
const BREAK_CEILING = Number(__ENV.BREAK_CEILING || 6000);
const WINDOW = 10;

const REDIRECT = __ENV.REDIRECT_URL || 'http://redirect:8082';
const STATUS = __ENV.STATUS_URL || 'http://redirect:8081/statusz';
const API = __ENV.API_URL || 'http://api:8080';
const KEY = __ENV.CORINTO_KEY;

// The links of the seed (internal/seed/data.go). One has expired: it owes
// an event like the others and answers 404.
const ACTIVE = [
  'spring-reading', 'spring-reading-ig', 'spring-reading-pod', 'staff-picks', 'signed-copies',
  'author-tour', 'author-tour-poster', 'tour-tickets', 'opening-hours',
  'summer-sale', 'summer-sale-ig', 'summer-sale-yt', 'tune-up-offer', 'ride-map', 'workshop-booking',
];
const EXPIRED = 'winter-clearance';
const SWITCHED = 'bench-switch';

// What the requests say they are: mostly browsers, with the previewers,
// crawlers and libraries that follow any public link.
const AGENTS = [
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36',
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15',
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1',
  'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36',
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:144.0) Gecko/20100101 Firefox/144.0',
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0',
  'Mozilla/5.0 (iPad; CPU OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1',
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36',
  'WhatsApp/2.23.20.0',
  'Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)',
  'curl/8.11.0',
];
const REFERRERS = [
  'https://news.example.com/today', 'https://blog.example.org/reading', 'https://forum.example.net/thread',
  'https://mail.example.com/', 'https://search.example.org/results',
];

const asExpected = new Rate('answered_as_the_contract_says');
const deactivation = new Trend('deactivation', true);
const serviceP50 = new Gauge('server_service_time_p50_ms');
const serviceP95 = new Gauge('server_service_time_p95_ms');
const serviceP99 = new Gauge('server_service_time_p99_ms');
const serviceP999 = new Gauge('server_service_time_p999_ms');
const windowP99 = new Trend('server_service_time_window_p99_ms');

// seconds reads a duration written as k6 takes it, in seconds or minutes.
function seconds(duration) {
  const written = /^(\d+)(s|m)$/.exec(duration);
  if (!written) {
    throw new Error(`DURATION is written like 20s or 10m, not ${duration}`);
  }
  return Number(written[1]) * (written[2] === 'm' ? 60 : 1);
}

// The generator has a second of requests in hand before it needs anything
// new: a machine that stops for a moment does not make it fall behind the
// rate it was told.
const steady = {
  executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s', duration: DURATION,
  preAllocatedVUs: RATE, maxVUs: RATE * 4,
};
// A run in which the generator did not send more than one request in a
// thousand of the ones it was told is not a measurement of that rate.
const MAY_DROP = Math.floor(RATE * seconds(DURATION) / 1000);
// The break test: the rate goes up a step at a time until the service time
// of a window misses the objective, which ends the run.
const stages = [];
for (let rate = RATE; rate <= BREAK_CEILING; rate += BREAK_STEP) {
  stages.push({ target: rate, duration: '1s' }, { target: rate, duration: `${BREAK_EVERY - 1}s` });
}
const rising = {
  executor: 'ramping-arrival-rate', startRate: RATE, timeUnit: '1s', stages,
  preAllocatedVUs: Math.ceil(RATE / 2), maxVUs: BREAK_CEILING,
};

export const options = MODE === 'break' ? {
  scenarios: {
    load: rising,
    watch: { executor: 'constant-vus', vus: 1, duration: `${stages.length / 2 * BREAK_EVERY}s`, exec: 'watch' },
  },
  thresholds: {
    server_service_time_window_p99_ms: [{ threshold: `max<=${SERVICE_P99_MS}`, abortOnFail: true }],
  },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
} : {
  scenarios: {
    load: steady,
    switches: { executor: 'per-vu-iterations', vus: 1, iterations: SWITCHES, exec: 'switches', maxDuration: DURATION },
  },
  thresholds: {
    // The server, by its own histogram.
    server_service_time_p99_ms: [`value<=${SERVICE_P99_MS}`],
    deactivation: [`p(99)<${DEACTIVATION_P99_MS}`],
    // The generator kept the rate it was told, and every answer was the
    // one the contract of the redirect gives: no request was refused.
    dropped_iterations: [`count<=${MAY_DROP}`],
    answered_as_the_contract_says: ['rate==1'],
  },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

// address returns the nth simulated client, in the range kept for
// benchmarks (198.18.0.0/15).
function address(n) {
  return `198.18.${Math.floor(n / 256) % 256}.${n % 256}`;
}

function pick(list) {
  return list[Math.floor(Math.random() * list.length)];
}

// histogram reads the service time the redirect has measured so far.
function histogram() {
  const response = http.get(STATUS);
  if (response.status !== 200) {
    throw new Error(`GET ${STATUS} = ${response.status}`);
  }
  return response.json('service_time');
}

// between returns the requests of each bucket between two readings.
function between(before, after) {
  return {
    requests: after.requests - before.requests,
    over: after.over - before.over,
    buckets: after.buckets.map((bucket, i) => ({ up_to_ms: bucket.up_to_ms, requests: bucket.requests - before.buckets[i].requests })),
  };
}

// percentile returns the upper bound of the bucket a percentile falls in,
// which is how a figure of a histogram is given, or Infinity when it is
// past the last bucket.
function percentile(measured, share) {
  let seen = 0;
  for (const bucket of measured.buckets) {
    seen += bucket.requests;
    if (seen >= measured.requests * share) {
      return bucket.up_to_ms;
    }
  }
  return Infinity;
}

function api(method, path, body) {
  const response = http.request(method, API + path, body ? JSON.stringify(body) : null, {
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
  });
  if (response.status >= 300) {
    throw new Error(`${method} ${path} = ${response.status}`);
  }
  return response;
}

// follow asks for a short link as one client and returns the status.
function follow(alias, client, tags) {
  return http.get(`${REDIRECT}/${alias}`, {
    redirects: 0, headers: { 'X-Forwarded-For': client, 'User-Agent': AGENTS[0] }, tags,
    responseCallback: http.expectedStatuses(302, 404),
  }).status;
}

// until asks for a link ten times a second, half of what one address may,
// until it answers with a status, and returns how long that took.
function until(alias, client, status) {
  const started = Date.now();
  while (follow(alias, client, { kind: 'switch' }) !== status) {
    if (Date.now() - started > 60000) {
      throw new Error(`${alias} never answered ${status}`);
    }
    sleep(0.1);
  }
  return Date.now() - started;
}

export function setup() {
  if (MODE === 'break') {
    return { before: histogram() };
  }
  // The link that is switched on and off, created like any other.
  const created = api('POST', '/v1/links', { destination: 'https://www.example.com/bench/switch', alias: SWITCHED }).json();
  until(SWITCHED, address(ADDRESSES), 302);
  return { before: histogram(), switched: created.id };
}

export default function () {
  const draw = Math.random();
  let alias = pick(ACTIVE);
  let expected = 302;
  if (draw < 0.03) {
    alias = EXPIRED;
    expected = 404;
  } else if (draw < 0.04) {
    alias = 'nobody-has-this';
    expected = 404;
  }
  const headers = { 'X-Forwarded-For': address(Math.floor(Math.random() * ADDRESSES)), 'User-Agent': pick(AGENTS) };
  if (Math.random() < 0.4) {
    headers.Referer = pick(REFERRERS);
  }
  const response = http.get(`${REDIRECT}/${alias}`, {
    redirects: 0, headers, tags: { kind: 'load' }, responseCallback: http.expectedStatuses(302, 404),
  });
  asExpected.add(check(response, { 'the answer of the contract': (r) => r.status === expected }));
}

// switches disables the link and measures how long it keeps redirecting:
// from the answer of the API, when the change is committed, to the first
// 404 of the redirect. Then it enables it again and waits for it.
export function switches(data) {
  const started = Date.now();
  const client = address(ADDRESSES);
  api('POST', `/v1/links/${data.switched}/disable`);
  deactivation.add(until(SWITCHED, client, 404));
  api('POST', `/v1/links/${data.switched}/enable`);
  until(SWITCHED, client, 302);
  // One switch every so long, at a moment that does not follow the polls
  // of the redirect.
  sleep(Math.max(0, SWITCH_EVERY - (Date.now() - started) / 1000) * (0.5 + Math.random()));
}

// watch reads the histogram every window and gives the 99th percentile of
// that window alone: the break test ends at the first that misses.
export function watch() {
  const before = histogram();
  sleep(WINDOW);
  const measured = between(before, histogram());
  const p99 = percentile(measured, 0.99);
  console.log(`window: ${Math.round(measured.requests / WINDOW)} requests a second, service time p99 up to ${p99} ms`);
  windowP99.add(Number.isFinite(p99) ? p99 : 1e9);
}

export function teardown(data) {
  const measured = between(data.before, histogram());
  for (const [gauge, share, name] of [[serviceP50, 0.5, 'p50'], [serviceP95, 0.95, 'p95'], [serviceP99, 0.99, 'p99'], [serviceP999, 0.999, 'p99.9']]) {
    const bound = percentile(measured, share);
    gauge.add(Number.isFinite(bound) ? bound : 1e9);
    console.log(`figure: server service time ${name} up to ${bound} ms`);
  }
  console.log(`figure: requests measured by the server ${measured.requests}, of which over the last bucket ${measured.over}`);
}

// Single-link capacity and contention test: races ATTEMPTS distinct customers against ONE reusable
// link to verify MaxLinkEnrollments (100) enforcement, resume-while-full, and latency/error SLOs.
// Never POSTs /payments (Stripe).
// Env: BASE_URL, TOKEN (or TOKENS_FILE + OFFSET), CAP=100, ATTEMPTS=120, VUS=120, OUT.
import http from 'k6/http';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import {
  ORIGIN, OFFSET, baseOptions, call, completeStarted, expect, expectPlain, expectStep, has, hdrEnds, intEnv, loadTokens, spec,
  summarize, tokenAt,
} from './lib.js';

const CAP = intEnv('CAP', 100);
const ATTEMPTS = intEnv('ATTEMPTS', CAP + 20);
const mainIterations = ATTEMPTS > CAP ? ATTEMPTS - 1 : ATTEMPTS;
const VUS = Math.min(intEnv('VUS', mainIterations), mainIterations);
const THINK = __ENV.THINK !== '0';

// Global expected statuses: 200-399. Entry GET and /start explicitly accept 404 once the link fills,
// so real 5xx or mid-journey 4xx errors still fail http_req_failed.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 399 }));
const allowFullWelcome = http.expectedStatuses(200, 404);
const allowFullStart = http.expectedStatuses(303, 404);
const expectFull = http.expectedStatuses(404);

const tokens = __ENV.TOKEN ? null : loadTokens();
const TOKEN = __ENV.TOKEN || tokenAt(tokens, 0);
if (!TOKEN) throw new Error('capacity.js needs TOKEN or a token at TOKENS_FILE[OFFSET]');

const NOT_FOUND = 'Este enlace ya no es válido';
const WELCOME = 'Bienvenidos a Masterminds';
const ASSETS = ['/static/customer.css', '/static/logo-mark.webp', '/favicon.ico'];

const linkStarted = new Counter('link_started');
const linkRejected = new Counter('link_rejected');
const linkCompleted = new Counter('link_completed');
const journeyDuration = new Trend('journey_duration', true);

export const options = {
  ...baseOptions,
  scenarios: {
    capacity: {
      executor: 'shared-iterations',
      vus: VUS,
      iterations: mainIterations,
      maxDuration: '5m',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate==1'],
    ...(ATTEMPTS > CAP && {
      link_started: [`count==${CAP}`],
      link_completed: [`count==${CAP}`],
      link_rejected: [`count==${ATTEMPTS - CAP}`],
    }),
  },
};

// When ATTEMPTS > CAP, reserve 1 session up front so teardown can verify resume-while-full after
// the remaining CAP - 1 slots fill and reject new visitors.
export function setup() {
  if (ATTEMPTS <= CAP) return { reservedCookie: '' };
  http.cookieJar().clear(ORIGIN);
  const path = `/customer/${TOKEN}`;
  const welcome = call('GET', path);
  if (!expectStep('welcome', welcome, 'setup: GET /customer/:token', 200, { 'has welcome marker': has(WELCOME) })) {
    exec.test.abort('capacity.js setup: link is not open (expected 200 on welcome)');
  }
  const started = call('POST', `${path}/start`);
  if (!expectStep('start', started, 'setup: POST /customer/:token/start', 303, { 'Location ends /forms': hdrEnds('Location', '/forms') })) {
    exec.test.abort('capacity.js setup: failed to start reserved session');
  }
  const jar = http.cookieJar().cookiesForURL(`${ORIGIN}${path}`);
  const cookie = (jar.enrollment_session || [])[0] || '';
  if (!cookie) exec.test.abort('capacity.js setup: missing enrollment_session cookie');
  linkStarted.add(1);
  return { reservedCookie: cookie };
}

export default function () {
  http.cookieJar().clear(ORIGIN);
  const i = exec.scenario.iterationInTest;
  const idx = OFFSET + (ATTEMPTS > CAP ? 1 : 0) + i;
  const path = `/customer/${TOKEN}`;
  const startMs = Date.now();

  const [welcome, ...rest] = http.batch([
    spec('GET', path, { params: { responseCallback: allowFullWelcome } }),
    ...ASSETS.map((p) => spec('GET', p, { params: { responseType: 'none' } })),
  ]);
  const welcomeOk = expectStep('welcome', welcome, 'GET /customer/:token', [200, 404], {
    'welcome or branded 404': (r) => (r.status === 200 ? has(WELCOME)(r) : has(NOT_FOUND)(r)),
  });
  let assetsOk = true;
  rest.forEach((r, k) => {
    assetsOk = expectPlain(r, `GET ${ASSETS[k]}`, 200) && assetsOk;
  });
  if (!welcomeOk || !assetsOk) return;
  if (welcome.status === 404) {
    linkRejected.add(1);
    return;
  }

  if (THINK) sleep(1 + Math.random() * 2);

  const started = call('POST', `${path}/start`, { params: { responseCallback: allowFullStart } });
  const startOk = expectStep('start', started, 'POST /customer/:token/start', [303, 404], {
    'redirects to /forms or branded 404': (r) => (r.status === 303 ? hdrEnds('Location', '/forms')(r) : has(NOT_FOUND)(r)),
  });
  if (!startOk) return;
  if (started.status === 404) {
    linkRejected.add(1);
    return;
  }

  linkStarted.add(1);
  if (completeStarted(TOKEN, idx, { think: THINK })) {
    linkCompleted.add(1);
    journeyDuration.add(Date.now() - startMs);
  }
}

export function teardown(data) {
  if (!data || !data.reservedCookie) return;
  const path = `/customer/${TOKEN}`;

  // 1. Fresh visitor with no cookie must still be refused once full.
  http.cookieJar().clear(ORIGIN);
  expect(call('GET', path, { params: { responseCallback: expectFull } }), 'full link refuses fresh GET with 404', 404, {
    'has branded 404': has(NOT_FOUND),
  });
  expect(call('POST', `${path}/start`, { params: { responseCallback: expectFull } }), 'full link refuses fresh POST /start with 404', 404, {
    'has branded 404': has(NOT_FOUND),
  });

  // 2. Existing unpaid session must still resume and finish without adding a 101st row.
  http.cookieJar().set(`${ORIGIN}${path}`, 'enrollment_session', data.reservedCookie, { path });
  expect(call('GET', path), 'full link lets existing live session view welcome', 200, {
    'shows Continuar': has('>Continuar</a>'),
  });
  expect(call('POST', `${path}/start`), 'full link lets existing live session resume via POST /start', 303, {
    'Location ends /forms': hdrEnds('Location', '/forms'),
  });
  if (completeStarted(TOKEN, OFFSET, { think: false })) {
    linkCompleted.add(1);
  }
}

export const handleSummary = (data) => summarize('capacity', data);

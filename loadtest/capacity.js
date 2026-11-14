// Reusable-link capacity and contention test: ATTEMPTS distinct customers arrive at one or more reusable
// links to verify MaxLinkEnrollments (100) enforcement, resume-while-full (one link) and latency/error SLOs.
// Customers arrive one after another, evenly spaced over WINDOW seconds (WINDOW=0 sends them all at once:
// the worst-case burst), round-robin over the links. A full link turns latecomers away with the branded 404.
// 1000 customers, one every quarter second, on 10 links: ATTEMPTS=1000 WINDOW=250 TOKENS=<10 tokens>.
// Every journey ends with POST /payments: a real Stripe sandbox Checkout Session per customer. CHECKOUT=0 skips it.
// Env: BASE_URL, TOKEN or TOKENS (comma-separated; or TOKENS_FILE + OFFSET for one), CAP=100,
// ATTEMPTS=CAP*links+20, WINDOW=60, VUS=ATTEMPTS-1 (burst only), THINK=0 to skip think time, CHECKOUT=1 (0: no Stripe),
// OFFSET (PIN base; random per run when unset), OUT.
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import {
  NOT_FOUND, ORIGIN, OFFSET, PLANS, STRIPE_CHECKOUT, WELCOME, baseOptions, call, completeStarted, encode, expect, expectStep, has, hdrEnds,
  intEnv, journeyMetrics, loadTokens, newSession, start, stepThresholds, summarize, target, think, tokenAt, welcome,
} from './lib.js';

const CAP = intEnv('CAP', 100);
const tokens = __ENV.TOKENS || __ENV.TOKEN ? null : loadTokens();
const TOKENS = (__ENV.TOKENS || __ENV.TOKEN || tokenAt(tokens, 0) || '').split(',').filter(Boolean);
if (!TOKENS.length) throw new Error('capacity.js needs TOKEN, TOKENS or a token at TOKENS_FILE[OFFSET]');
const LINKS = TOKENS.length;
const ATTEMPTS = intEnv('ATTEMPTS', CAP * LINKS + 20);
// One link only: reserve a session up front so teardown can check resume-while-full after the link fills.
const RESERVE = LINKS === 1 && ATTEMPTS > CAP;
const mainIterations = ATTEMPTS - (RESERVE ? 1 : 0);
// Customer i goes to link i % LINKS; each link takes CAP and turns the rest away.
const STARTED = TOKENS.reduce((n, _, k) => n + Math.min(CAP, Math.floor(ATTEMPTS / LINKS) + (k < ATTEMPTS % LINKS ? 1 : 0)), 0);
const VUS = Math.min(intEnv('VUS', mainIterations), mainIterations);
const WINDOW = Math.max(0, intEnv('WINDOW', 60));
const CHECKOUT = __ENV.CHECKOUT !== '0';
const THINK = __ENV.THINK !== '0';

// Expected statuses: 200-399. Entry GET and /start explicitly accept 404 once the link fills,
// so real 5xx or mid-journey 4xx errors still fail http_req_failed.
const journeyDuration = journeyMetrics();
const allowFullWelcome = http.expectedStatuses(200, 404);
const allowFullStart = http.expectedStatuses(303, 404);
const expectFull = http.expectedStatuses(404);

const TOKEN = TOKENS[0]; // setup and teardown (the single-link checks) only

const linkStarted = new Counter('link_started');
const linkRejected = new Counter('link_rejected');
const linkCompleted = new Counter('link_completed');

// WINDOW>0: customers start at a steady rate for WINDOW seconds, so exactly mainIterations arrive.
// Each journey outlives the window by its think time, hence the long gracefulStop. A journey takes ~20 s with
// think time; preAllocatedVUs assumes up to JOURNEY_SECONDS, and the dropped_iterations threshold says if that was too low.
const JOURNEY_SECONDS = 40;
const scenario = WINDOW
  ? {
      executor: 'constant-arrival-rate',
      rate: mainIterations,
      timeUnit: `${WINDOW}s`,
      duration: `${WINDOW}s`,
      preAllocatedVUs: Math.min(mainIterations + 1, Math.ceil((mainIterations / WINDOW) * JOURNEY_SECONDS)),
      maxVUs: mainIterations + 1, // k6 sometimes fires one arrival past rate x duration (the guard in the default function drops it); it needs a free, preallocated VU
      gracefulStop: '2m',
    }
  : { executor: 'shared-iterations', vus: VUS, iterations: mainIterations, maxDuration: '5m' };

export const options = {
  ...baseOptions,
  scenarios: { capacity: scenario },
  thresholds: {
    ...(!CHECKOUT && { http_req_duration: ['p(95)<500', 'p(99)<1000'] }),
    ...stepThresholds(CHECKOUT),
    http_req_failed: ['rate<0.01'],
    checks: ['rate==1'],
    ...(WINDOW && { dropped_iterations: ['count==0'] }),
    link_started: [`count==${STARTED}`],
    link_completed: [`count==${STARTED}`],
    link_rejected: [`count==${ATTEMPTS - STARTED}`],
  },
};

// Picks the PIN base once per run: PINs are 100000 + base + i and must be unique in the database, so a
// fixed base collides with the previous run's leftover rows. A random one rarely does; fail_pin_taken says when.
// On a single link with ATTEMPTS > CAP, reserves 1 session up front so teardown can verify resume-while-full
// after the link fills and turns new visitors away.
export function setup() {
  const base = __ENV.OFFSET ? OFFSET : Math.floor(Math.random() * (900000 - ATTEMPTS));
  console.log(`PIN base ${base}: PINs ${100000 + base}..${100000 + base + ATTEMPTS}`);
  if (!RESERVE) return { base, reservedCookie: '' };
  const jar = newSession('setup');
  const path = `/customer/${TOKEN}`;
  if (!expectStep('welcome', call('GET', path), 'setup: GET /customer/:token', 200, { 'has welcome marker': has(WELCOME) })) {
    exec.test.abort('capacity.js setup: link is not open (expected 200 on welcome)');
  }
  if (!start(path, { label: 'setup: POST /customer/:token/start' })) {
    exec.test.abort('capacity.js setup: failed to start reserved session');
  }
  const cookie = (jar.cookiesForURL(`${ORIGIN}${path}`).enrollment_session || [])[0] || '';
  if (!cookie) exec.test.abort('capacity.js setup: missing enrollment_session cookie');
  linkStarted.add(1);
  return { base, reservedCookie: cookie };
}

export default function (data) {
  const i = exec.scenario.iterationInTest;
  // constant-arrival-rate can start one iteration more than rate x duration (seen on Railway: 120 for 119).
  if (i >= mainIterations) return;
  const idx = data.base + (RESERVE ? 1 : 0) + i;
  newSession(idx);
  const token = TOKENS[i % LINKS];
  const path = `/customer/${token}`;
  const startMs = Date.now();

  const page = welcome(path, {
    status: [200, 404],
    extra: { 'welcome or branded 404': (r) => (r.status === 200 ? has(WELCOME)(r) : has(NOT_FOUND)(r)) },
    params: { responseCallback: allowFullWelcome },
  });
  if (!page) return;
  if (page.status === 404) {
    linkRejected.add(1);
    return;
  }

  if (THINK) think(1, 3);

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
  if (completeStarted(token, idx, { think: THINK }) && (!CHECKOUT || startCheckout(token, idx))) {
    linkCompleted.add(1);
    journeyDuration.add(Date.now() - startMs);
  }
}

export function teardown(data) {
  if (!data || !data.reservedCookie) return;
  const path = `/customer/${TOKEN}`;

  // 1. Fresh visitor with no cookie must still be refused once full.
  newSession('teardown fresh');
  expect(call('GET', path, { params: { responseCallback: expectFull } }), 'full link refuses fresh GET with 404', 404, {
    'has branded 404': has(NOT_FOUND),
  });
  expect(call('POST', `${path}/start`, { params: { responseCallback: expectFull } }), 'full link refuses fresh POST /start with 404', 404, {
    'has branded 404': has(NOT_FOUND),
  });

  // 2. Existing unpaid session must still resume and finish without adding a 101st row.
  newSession('teardown reserved').set(`${ORIGIN}${path}`, 'enrollment_session', data.reservedCookie, { path });
  expect(call('GET', path), 'full link lets existing live session view welcome', 200, {
    'shows Continuar': has('>Continuar</a>'),
  });
  start(path, { label: 'full link lets existing live session resume via POST /start', step: false });
  if (completeStarted(TOKEN, data.base, { think: false }) && (!CHECKOUT || startCheckout(TOKEN, data.base))) {
    linkCompleted.add(1);
  }
}

function startCheckout(token, idx) {
  const plan = PLANS[idx % PLANS.length];
  const res = call('POST', `/customer/${token}/payments`, { body: encode({ payment_plan: plan }) });
  const ok = expectStep('checkout', res, 'POST /customer/:token/payments', 303, {
    'targets https://checkout.stripe.com/': (r) => target(r).startsWith(STRIPE_CHECKOUT),
  });
  if (ok) console.log(`CHECKOUT ${idx} ${plan} ${target(res)}`);
  return ok;
}

export const handleSummary = (data) => summarize('capacity', data);

// Stripe checkout starter + duplicate-POST race. NOT a load test: every token creates (or resumes) a real
// Stripe Checkout Session. Env: BASE_URL, PLAN=full|three|six, TOKENS_FILE, OFFSET, COUNT (<=20, default 1),
// RACE (identical concurrent POSTs per token, default 10), CONCURRENT_TOKENS (tokens raced together, default 1), OUT.
// Prints one parseable line per token: CHECKOUT <token> <plan> <url-or-NONE> <distinct-url-count>
import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import {
  ORIGIN, PLANS, OFFSET, baseOptions, call, completeToPayments, customerHeaderChecks, encode, expect, has, intEnv, loadTokens, spec,
  summarize, target, tokenAt,
} from './lib.js';

const PLAN = __ENV.PLAN || 'full';
if (!PLANS.includes(PLAN)) throw new Error(`PLAN must be one of ${PLANS.join('|')}, got "${PLAN}"`);
const RACE = intEnv('RACE', 10);
const PARALLEL = intEnv('CONCURRENT_TOKENS', 1);
const N = Math.min(intEnv('COUNT', 1), 20);
const STRIPE = 'https://checkout.stripe.com/';
const LABEL = 'POST /customer/:token/payments';

export const options = {
  ...baseOptions,
  scenarios: { checkout: { executor: 'shared-iterations', vus: 1, iterations: Math.ceil(N / PARALLEL), maxDuration: '10m' } },
  batch: RACE * PARALLEL,
  batchPerHost: RACE * PARALLEL,
  thresholds: { checks: ['rate==1'] },
};

const tokens = loadTokens();
const payload = encode({ payment_plan: PLAN });

// /payments/return is token-free; it must not invent outcomes for unknown input.
function returnPageCases() {
  expect(call('GET', '/payments/return'), 'GET /payments/return without params is 404', 404);
  expect(call('GET', '/payments/return?cancelled=1'), 'GET /payments/return?cancelled=1 shows the cancelled outcome', 200, {
    'has No completado': has('No completado'),
  });
  expect(call('GET', '/payments/return?session_id=cs_test_unknown'), 'GET /payments/return with an unknown session is 404', 404);
}

// One token's race: all POSTs are accepted (303 to Stripe, or 202 while the first one still creates the
// Session), never 5xx, and every redirect (race + a later sequential replay) is the same Checkout URL.
function settle(token, responses) {
  responses.forEach((r) => customerHeaderChecks(r, LABEL));
  const redirects = responses.filter((r) => r.status === 303).map(target);
  check(responses, {
    'checkout race: no 5xx': (rs) => rs.every((r) => r.status < 500),
    'checkout race: every response is a 303 to Stripe or a 202 pending resume': (rs) => rs.every((r) => r.status === 303 || r.status === 202),
    'checkout race: every redirect targets https://checkout.stripe.com/': () => redirects.every((u) => u.startsWith(STRIPE)),
    'checkout race: at least one POST got a Checkout URL': () => redirects.length > 0,
    'checkout race: all racing redirects share one Checkout URL': () => new Set(redirects).size <= 1,
  });

  // Duplicate POSTs must resume the open attempt, not create another Session.
  const replay = call('POST', `/customer/${token}/payments`, { body: payload });
  const urls = [...redirects, ...(replay.status === 303 ? [target(replay)] : [])];
  expect(replay, 'sequential replay after the race resumes the open Checkout attempt', 303, {
    'targets https://checkout.stripe.com/': (r) => target(r).startsWith(STRIPE),
  });
  check(urls, { 'race and replay together yield exactly one Checkout URL': (u) => new Set(u).size === 1 });
  console.log(`CHECKOUT ${token} ${PLAN} ${urls[0] || 'NONE'} ${new Set(urls).size}`);
}

export default function () {
  http.cookieJar().clear(ORIGIN);
  const it = exec.scenario.iterationInTest;
  if (it === 0) returnPageCases();

  const racers = [];
  for (let k = 0; k < PARALLEL; k++) {
    const i = it * PARALLEL + k;
    if (i >= N) break;
    const token = tokenAt(tokens, i);
    if (!token) exec.test.abort(`checkout.js: token slice too short, no token at slice index ${i}`);
    if (completeToPayments(token, OFFSET + i, { think: false })) racers.push(token);
    else console.log(`CHECKOUT ${token} ${PLAN} NONE 0`);
  }
  if (!racers.length) return;

  const requests = racers.flatMap((t) => Array.from({ length: RACE }, () => spec('POST', `/customer/${t}/payments`, { body: payload })));
  const responses = http.batch(requests);
  racers.forEach((token, k) => settle(token, responses.slice(k * RACE, (k + 1) * RACE)));
}

export const handleSummary = (data) => summarize('checkout', data);

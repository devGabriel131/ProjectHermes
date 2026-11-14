// Load test: one customer per iteration, welcome page to plan selection. Never POSTs /payments (Stripe).
// Env: BASE_URL, PROFILE=100|1000, TOKENS_FILE, OFFSET, COUNT, OUT.
import http from 'k6/http';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { ORIGIN, baseOptions, call, completeToPayments, expect, has, loadTokens, OFFSET, randomToken, summarize, tokenAt } from './lib.js';

// Only real failures count in http_req_failed; 404/409/422 are not expected anywhere in this journey.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 399 }));
const notFound = http.expectedStatuses(404);

const STAGES = {
  100: [
    { duration: '30s', target: 100 },
    { duration: '2m', target: 100 },
    { duration: '15s', target: 0 },
  ],
  1000: [
    { duration: '60s', target: 250 },
    { duration: '60s', target: 500 },
    { duration: '60s', target: 1000 },
    { duration: '3m', target: 1000 },
    { duration: '30s', target: 0 },
  ],
};
const PROFILE = __ENV.PROFILE || '100';
if (!STAGES[PROFILE]) throw new Error(`PROFILE must be 100 or 1000, got "${PROFILE}"`);

export const options = {
  ...baseOptions,
  scenarios: { journey: { executor: 'ramping-vus', startVUs: 0, stages: STAGES[PROFILE], gracefulRampDown: '30s', gracefulStop: '30s' } },
  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
    http_req_failed: ['rate<0.01'],
    journey_success: ['rate>0.99'],
  },
};

const tokens = loadTokens();
const journeySuccess = new Rate('journey_success');
const journeyDuration = new Trend('journey_duration', true);
const tokensExhausted = new Counter('tokens_exhausted');

// Read-only repeat traffic: HEAD must not consume the link; unknown tokens look like the branded 404.
function repeatTraffic(token) {
  const head = call('HEAD', `/customer/${token}`);
  const miss = call('GET', `/customer/${randomToken()}`, { name: 'GET /customer/:unknown', params: { responseCallback: notFound } });
  const a = expect(head, 'HEAD /customer/:token', 200, { 'has empty body': (r) => !r.body });
  const b = expect(miss, 'GET /customer/:unknown', 404, { 'has branded 404 marker': has('Este enlace ya no es válido') });
  return a && b;
}

export default function () {
  // Clear VU cookies so a reusable token never resumes a prior customer's session.
  http.cookieJar().clear(ORIGIN);
  const i = exec.scenario.iterationInTest;
  const token = tokenAt(tokens, i);
  if (!token) {
    tokensExhausted.add(1);
    sleep(1);
    return;
  }
  const start = Date.now();
  let ok = true;
  if (Math.random() < 0.1) ok = repeatTraffic(token);
  ok = completeToPayments(token, OFFSET + i) && ok;
  journeySuccess.add(ok);
  journeyDuration.add(Date.now() - start);
}

export const handleSummary = (data) => summarize('journey', data);

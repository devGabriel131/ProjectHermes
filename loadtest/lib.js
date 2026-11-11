// Shared helpers for the ProjectHermes k6 scripts. Offline-safe: no remote imports.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Trend } from 'k6/metrics';

export const BASE = (__ENV.BASE_URL || 'https://paymentwebsite-production.up.railway.app').replace(/\/+$/, '');
export const ORIGIN = BASE.match(/^https?:\/\/[^/]+/)[0];
export const intEnv = (key, fallback) => (__ENV[key] ? parseInt(__ENV[key], 10) : fallback);
export const OFFSET = intEnv('OFFSET', 0);
export const COUNT = intEnv('COUNT', Infinity);

// Draftable fields in form order; keys match students.Fields() and the /draft/{field} route.
// The PIN is not one: it is never autosaved, only sent with the final submit.
export const FIELDS = ['name', 'date_of_birth', 'gender', 'email', 'phone', 'municipality'];
export const PLANS = ['full', 'three', 'six']; // internal/payments/plans.go IDs

export const baseOptions = {
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
  // The url tag carries customer tokens; dropping it bounds cardinality to the name tag.
  systemTags: ['status', 'method', 'name', 'group', 'check', 'error', 'error_code', 'expected_response', 'scenario'],
};

// Call from the init context only. Tokens are 43-char base64url strings.
export const loadTokens = () => new SharedArray('tokens', () => JSON.parse(open(__ENV.TOKENS_FILE || '/tmp/k6/tokens.json')));
// The i-th token of the OFFSET..OFFSET+COUNT slice, or undefined when the slice is spent.
export const tokenAt = (tokens, i) => (i < COUNT ? tokens[OFFSET + i] : undefined);

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';
export const randomToken = () => Array.from({ length: 43 }, () => B64[Math.floor(Math.random() * 64)]).join('');

// All values are valid per internal/students/fields.go; email_confirm and pin are only used by the final submit.
// PINs must be unique across the database, so the token index makes each one: keep OFFSET slices disjoint.
export const formValues = (idx) => ({
  name: 'María José Pérez Rivera',
  date_of_birth: '2000-01-15',
  gender: 'Female',
  email: `k6.${idx}@example.com`,
  email_confirm: `k6.${idx}@example.com`,
  phone: '(787) 234-5678',
  municipality: 'Río Grande',
  pin: String(100000 + idx),
});

// application/x-www-form-urlencoded; an array value repeats the key.
export const encode = (obj) =>
  Object.entries(obj)
    .flatMap(([k, v]) => [].concat(v).map((x) => `${encodeURIComponent(k)}=${encodeURIComponent(x)}`))
    .join('&');

// Request spec [method, url, body, params]: usable with http.request(...) and http.batch([...]).
// Redirects are never followed so 303/HX-Redirect responses can be asserted. The name tag masks
// tokens and field names.
export function spec(method, path, { body, htmx = false, headers = {}, name, params = {} } = {}) {
  const label = path.replace(/^\/customer\/[^/?]+/, '/customer/:token').replace(/\/draft\/[^/?]+/, '/draft/:field');
  return [
    method,
    BASE + path,
    body === undefined ? null : body,
    {
      headers: {
        ...(body !== undefined && { 'Content-Type': 'application/x-www-form-urlencoded' }),
        ...(htmx && { 'HX-Request': 'true' }),
        ...headers,
      },
      redirects: 0,
      tags: { name: name || `${method} ${label}` },
      ...params,
    },
  ];
}
export const call = (method, path, opts) => http.request(...spec(method, path, opts));

// Case-insensitive response header ('' when absent).
export function hdr(res, name) {
  const want = name.toLowerCase();
  for (const key in res.headers) if (key.toLowerCase() === want) return res.headers[key];
  return '';
}
export const target = (res) => hdr(res, 'Location') || hdr(res, 'HX-Redirect');
export const has = (text) => (r) => typeof r.body === 'string' && r.body.includes(text);
export const hdrEnds = (name, suffix) => (r) => hdr(r, name).endsWith(suffix);

// withHeaders (customer.go) on every customer outcome + X-Request-ID from logging.Requests.
export function customerHeaderChecks(res, label) {
  return check(res, {
    [`${label}: Cache-Control no-store`]: (r) => hdr(r, 'Cache-Control').includes('no-store'),
    [`${label}: Referrer-Policy no-referrer`]: (r) => hdr(r, 'Referrer-Policy') === 'no-referrer',
    [`${label}: Content-Security-Policy set`]: (r) => hdr(r, 'Content-Security-Policy') !== '',
    [`${label}: X-Request-Id set`]: (r) => hdr(r, 'X-Request-Id') !== '',
  });
}

// Asserts status (number or list) plus named extra predicates; `label` should name the contract.
function verify(res, label, status, extra, customer) {
  const want = [].concat(status);
  const checks = { [`${label} → ${want.join('|')}`]: (r) => want.includes(r.status) };
  for (const [name, fn] of Object.entries(extra)) checks[`${label}: ${name}`] = fn;
  const ok = check(res, checks);
  return (!customer || customerHeaderChecks(res, label)) && ok;
}
export const expect = (res, label, status, extra = {}) => verify(res, label, status, extra, true);
// For responses not produced by the customer handlers (static, management, webhook, CSRF 403, mux 405).
export const expectPlain = (res, label, status, extra = {}) => verify(res, label, status, extra, false);

// Per-step latency (response time only, no think time) and journey failure counters.
const S = Object.fromEntries(
  ['welcome', 'forms', 'draft', 'submit', 'agreement', 'accept', 'payments'].map((n) => [n, new Trend(`step_${n}`, true)]),
);
const agreementDateMissing = new Counter('agreement_date_missing');

// Walks one customer from the welcome page to the plan-selection page. Never POSTs /payments
// (that calls Stripe). Returns true only if every step's checks passed.
export function completeToPayments(token, idx, { think = true } = {}) {
  const path = `/customer/${token}`;
  const form = formValues(idx);
  const pause = (lo, hi) => {
    if (think) sleep(lo + Math.random() * (hi - lo));
  };
  const asset = (p) => spec('GET', p, { params: { responseType: 'none' } });
  let ok = true;
  const step = (trend, res, label, status, extra) => {
    trend.add(res.timings.duration);
    ok = expect(res, label, status, extra) && ok;
  };
  const staticOk = (res, p) => {
    ok = expectPlain(res, `GET ${p}`, 200) && ok;
  };

  // 1. welcome page + the assets it references (index.html welcome view)
  const assets = ['/static/customer.css', '/static/logo-mark.webp', '/favicon.ico'];
  const [welcome, ...rest] = http.batch([spec('GET', path), ...assets.map(asset)]);
  step(S.welcome, welcome, 'GET /customer/:token', 200, { 'has welcome marker': has('Bienvenidos a Masterminds') });
  rest.forEach((r, i) => staticOk(r, assets[i]));
  if (!ok) return false;
  pause(1, 3);

  // 2. form page + its script
  const [forms, script] = http.batch([spec('GET', `${path}/forms`), asset('/static/customer.js')]);
  step(S.forms, forms, 'GET /customer/:token/forms', 200, { 'has form marker': has('Agrega la información del estudiante') });
  staticOk(script, '/static/customer.js');
  if (!ok) return false;
  pause(1, 3);

  // 3. autosave on blur, one field per request
  for (const field of FIELDS) {
    const res = call('POST', `${path}/draft/${field}`, { body: encode({ [field]: form[field] }), htmx: true });
    step(S.draft, res, 'POST /customer/:token/draft/:field', 200, { 'says Guardado.': has('Guardado.') });
    pause(0.3, 1);
  }
  if (!ok) return false;
  pause(1, 3);

  // 4. final submit: htmx answers 200 + HX-Redirect to the agreement
  const submit = call('POST', path, { body: encode(form), htmx: true });
  step(S.submit, submit, 'POST /customer/:token', 200, { 'HX-Redirect ends /agreement': hdrEnds('HX-Redirect', '/agreement') });
  if (!ok) return false;
  pause(1, 3);

  // 5. agreement page carries the program start date the customer must accept
  const agreement = call('GET', `${path}/agreement`);
  step(S.agreement, agreement, 'GET /customer/:token/agreement', 200, { 'has agreement marker': has('Revisa y acepta el acuerdo') });
  const date = (/name="program_start_date"\s+value="([^"]*)"/.exec(String(agreement.body)) || [])[1];
  if (!date) agreementDateMissing.add(1);
  ok = check(agreement, { 'GET /customer/:token/agreement: program_start_date present': () => Boolean(date) }) && ok;
  if (!ok) return false;
  pause(1, 3);

  // 6. accept terms (plain form post): 303 to payments
  const accept = call('POST', `${path}/agreement`, { body: encode({ accept_terms: 'yes', program_start_date: date }) });
  step(S.accept, accept, 'POST /customer/:token/agreement', 303, { 'Location ends /payments': hdrEnds('Location', '/payments') });
  if (!ok) return false;
  pause(1, 3);

  // 7. plan selection
  const payments = call('GET', `${path}/payments`);
  step(S.payments, payments, 'GET /customer/:token/payments', 200, {
    'has plan-selection marker': has('Elige tu plan de pago'),
    'lists Pago Único': has('Pago Único'),
  });
  return ok;
}

// handleSummary body: compact table on stdout, full JSON to OUT (default /tmp/k6/out/<script>.json;
// the directory must exist).
export function summarize(script, data) {
  const val = (name, key) => {
    const m = data.metrics[name];
    return m && (m.values ? m.values[key] : m[key]);
  };
  const num = (x, d = 1) => (Number.isFinite(x) ? x.toFixed(d) : '-');
  const lines = [
    `== ${script} ==`,
    `checks       ${num(val('checks', 'rate') * 100, 2)}% passed (${val('checks', 'passes')} ok / ${val('checks', 'fails')} failed)`,
    `http_reqs    ${val('http_reqs', 'count')} (${num(val('http_reqs', 'rate'))}/s)   iterations ${val('iterations', 'count')} (${num(val('iterations', 'rate'))}/s)   tokens_exhausted ${val('tokens_exhausted', 'count') || 0}   agreement_date_missing ${val('agreement_date_missing', 'count') || 0}`,
    `${'latency ms'.padEnd(24)}${'p95'.padStart(9)}${'p99'.padStart(9)}${'max'.padStart(9)}`,
  ];
  for (const name of Object.keys(data.metrics)) {
    if (!(name === 'http_req_duration' || name === 'journey_duration' || name.startsWith('step_')) || !val(name, 'max')) continue;
    lines.push(`${name.padEnd(24)}${num(val(name, 'p(95)')).padStart(9)}${num(val(name, 'p(99)')).padStart(9)}${num(val(name, 'max')).padStart(9)}`);
  }
  for (const [name, m] of Object.entries(data.metrics)) {
    for (const [expr, t] of Object.entries(m.thresholds || {})) {
      lines.push(`${(t && typeof t === 'object' ? t.ok : t) ? 'PASS' : 'FAIL'}  ${name} ${expr}`);
    }
  }
  const failed = [];
  const walk = (g) => {
    [].concat(g.checks || []).forEach((c) => c.fails > 0 && failed.push(`${c.name} (x${c.fails})`));
    [].concat(g.groups || []).forEach(walk);
  };
  if (data.root_group) walk(data.root_group);
  lines.push(failed.length ? `FAILED CHECKS (${failed.length}): ${failed.join(' | ')}` : 'FAILED CHECKS: none');
  return { stdout: `${lines.join('\n')}\n`, [__ENV.OUT || `/tmp/k6/out/${script}.json`]: JSON.stringify(data, null, 2) };
}

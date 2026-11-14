// Shared helpers for the ProjectHermes k6 scripts. Offline-safe: no remote imports.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Rate, Trend } from 'k6/metrics';

if (!__ENV.BASE_URL) throw new Error('BASE_URL is required, for example BASE_URL=http://host.docker.internal:3000');
export const BASE = __ENV.BASE_URL.replace(/\/+$/, '');
export const ORIGIN = BASE.match(/^https?:\/\/[^/]+/)[0];
export const intEnv = (key, fallback) => (__ENV[key] ? parseInt(__ENV[key], 10) : fallback);
export const OFFSET = intEnv('OFFSET', 0);
export const COUNT = intEnv('COUNT', Infinity);

// Every customer needs a fresh cookie jar: cookieJar().clear() does not drop the path-scoped
// enrollment_session cookie, so a VU's next iteration would resume the previous customer.
// Module state is per VU in k6, so one jar and label per VU is safe. `who` tags FAIL lines.
let jar = null;
let who = '';
export function newSession(label = '') {
  who = label;
  return (jar = new http.CookieJar());
}

// Draftable fields in form order; keys match students.Fields() and the /draft/{field} route.
// The PIN is not one: it is never autosaved. It is checked live (POST /pin) and sent with the final submit.
export const FIELDS = ['name', 'date_of_birth', 'gender', 'email', 'phone', 'municipality'];
export const PLANS = ['full', 'three', 'six']; // internal/payments/plans.go IDs

// Response markers, from internal/customer/templates.
export const WELCOME = 'Bienvenidos a Masterminds';
export const NOT_FOUND = 'Este enlace ya no es válido'; // not-found.html
export const AGREEMENT = 'Revisa y acepta el acuerdo';
export const PLAN_SELECTION = 'Elige tu plan de pago';
export const SAVED = 'Guardado.';
export const STRIPE_CHECKOUT = 'https://checkout.stripe.com/';
// What the welcome page references; welcome() fetches them with it, as a browser does.
const ASSETS = ['/static/customer.css', '/static/logo-mark.webp', '/favicon.ico'];

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
const FIRST_NAMES = [
  ['María José', 'Female'], ['Carlos Antonio', 'Male'], ['Sofía Isabel', 'Female'], ['Diego Alejandro', 'Male'],
  ['Valentina Luz', 'Female'], ['Sebastián Luis', 'Male'], ['Camila Andrea', 'Female'], ['Mateo Gabriel', 'Male'],
  ['Lucía Carmen', 'Female'], ['Andrés Felipe', 'Male'], ['Paula Elena', 'Female'], ['Javier Ignacio', 'Male'],
];
const LAST_NAMES = ['Pérez', 'Rivera', 'Rodríguez', 'González', 'Torres', 'Vargas', 'Morales', 'Ortiz', 'Cruz', 'Díaz', 'Reyes', 'Santos'];
const MUNICIPALITIES = ['Río Grande', 'San Juan', 'Bayamón', 'Ponce', 'Caguas', 'Carolina', 'Mayagüez', 'Arecibo', 'Guaynabo', 'Trujillo Alto'];

export const formValues = (idx) => {
  const [first, gender] = FIRST_NAMES[idx % FIRST_NAMES.length];
  const last1 = LAST_NAMES[Math.floor(idx / FIRST_NAMES.length) % LAST_NAMES.length];
  const last2 = LAST_NAMES[(Math.floor(idx / (FIRST_NAMES.length * LAST_NAMES.length)) + idx + 3) % LAST_NAMES.length];
  const year = 1996 + (idx % 10);
  const month = String((idx % 12) + 1).padStart(2, '0');
  const day = String((idx % 28) + 1).padStart(2, '0');
  return {
    name: `${first} ${last1} ${last2}`,
    date_of_birth: `${year}-${month}-${day}`,
    gender,
    email: `k6.${idx}@example.com`,
    email_confirm: `k6.${idx}@example.com`,
    phone: '(787) 234-5678',
    municipality: MUNICIPALITIES[idx % MUNICIPALITIES.length],
    pin: String(100000 + idx),
  };
};

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
      ...(jar && { jar }),
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

const REASONS = ['pin_taken', 'validation', 'rate_limited', 'not_found', 'forbidden', 'server_error', 'network', 'wrong_body', 'other'];
const FAILS = Object.fromEntries(REASONS.map((r) => [r, new Counter(`fail_${r}`)]));
const PIN_TAKEN = 'Este PIN ya está en uso'; // pinTakenMessage in internal/customer/customer.go

// Why a response failed: the status was expected (a body or header predicate failed) or it was not.
function classify(res, want) {
  const s = res.status;
  if (want.includes(s)) return 'wrong_body';
  if (s === 0) return 'network';
  if (s === 422) return String(res.body).includes(PIN_TAKEN) ? 'pin_taken' : 'validation';
  if (s === 429) return 'rate_limited';
  if (s === 404) return 'not_found';
  if (s === 403) return 'forbidden';
  return s >= 500 ? 'server_error' : 'other';
}

// Asserts status (number or list) plus named extra predicates; `label` should name the contract.
// A failure also counts under fail_<reason> and logs one parseable line (run.fish summarizes them):
//   FAIL <who> <label> status=<n> reason=<reason> [alert text]
function verify(res, label, status, extra, customer) {
  const want = [].concat(status);
  const checks = { [`${label} → ${want.join('|')}`]: (r) => want.includes(r.status) };
  for (const [name, fn] of Object.entries(extra)) checks[`${label}: ${name}`] = fn;
  const ok = check(res, checks);
  const pass = (!customer || customerHeaderChecks(res, label)) && ok;
  if (!pass) {
    const reason = classify(res, want);
    FAILS[reason].add(1);
    const alert = (/role="alert"[^>]*>([^<]{1,120})/.exec(String(res.body)) || [])[1];
    console.warn(`FAIL ${who} ${label} status=${res.status} reason=${reason}${alert ? ` alert="${alert.trim()}"` : ''}`);
  }
  return pass;
}
export const expect = (res, label, status, extra = {}) => verify(res, label, status, extra, true);
// For responses not produced by the customer handlers (static, management, webhook, CSRF 403, mux 405).
export const expectPlain = (res, label, status, extra = {}) => verify(res, label, status, extra, false);

// Per-step latency (response time only, no think time) and pass rate (n/fail per step in the summary).
const STEP_NAMES = ['welcome', 'start', 'forms', 'draft', 'pin', 'submit', 'agreement', 'accept', 'payments', 'checkout'];
export const STEPS = Object.fromEntries(STEP_NAMES.map((n) => [n, new Trend(`step_${n}`, true)]));
const STEP_OK = Object.fromEntries(STEP_NAMES.map((n) => [n, new Rate(`step_${n}_ok`)]));
const agreementDateMissing = new Counter('agreement_date_missing');

export function expectStep(trendName, res, label, status, extra = {}) {
  STEPS[trendName].add(res.timings.duration);
  const pass = expect(res, label, status, extra);
  STEP_OK[trendName].add(pass);
  return pass;
}

// Per-step latency budget: the journey methodology (p95<500, p99<1000). Checkout calls Stripe, so it gets its own
// budget, and only scripts that reach it pass checkout=true.
export const stepThresholds = (checkout = false) =>
  Object.fromEntries(
    STEP_NAMES.filter((n) => checkout || n !== 'checkout').map((n) => [
      `step_${n}`,
      n === 'checkout' ? ['p(95)<1500', 'p(99)<3000'] : ['p(95)<500', 'p(99)<1000'],
    ]),
  );

// For journey scripts, init context only: http_req_failed counts anything outside 200-399 (a request
// expecting otherwise passes its own responseCallback), and journey_duration times each finished journey.
export function journeyMetrics() {
  http.setResponseCallback(http.expectedStatuses({ min: 200, max: 399 }));
  return new Trend('journey_duration', true);
}

// A customer's pause between steps, uniform between lo and hi seconds.
export const think = (lo, hi) => sleep(lo + Math.random() * (hi - lo));

const asset = (p) => spec('GET', p, { params: { responseType: 'none' } });

// Loads the welcome page with its ASSETS in one batch. status, extra and params default to an open link.
// Returns the page, or null when it or an asset failed its checks.
export function welcome(path, { status = 200, extra = { 'has welcome marker': has(WELCOME) }, params } = {}) {
  const [page, ...assets] = http.batch([spec('GET', path, { params }), ...ASSETS.map(asset)]);
  let ok = expectStep('welcome', page, 'GET /customer/:token', status, extra);
  assets.forEach((r, i) => {
    ok = expectPlain(r, `GET ${ASSETS[i]}`, 200) && ok;
  });
  return ok ? page : null;
}

// POST /start: 303 to /forms with a new or resumed enrollment_session cookie. step=false checks it without
// timing the start step.
export function start(path, { label = 'POST /customer/:token/start', step = true } = {}) {
  const res = call('POST', `${path}/start`);
  const extra = { 'Location ends /forms': hdrEnds('Location', '/forms') };
  return step ? expectStep('start', res, label, 303, extra) : expect(res, label, 303, extra);
}

// Walks a started enrollment from /forms to plan selection. Never POSTs /payments (that calls Stripe).
// Returns true only if every step's checks passed.
export function completeStarted(token, idx, { think: pauses = true } = {}) {
  const path = `/customer/${token}`;
  const form = formValues(idx);
  const pause = (lo, hi) => pauses && think(lo, hi);
  let ok = true;
  const step = (trendName, res, label, status, extra) => {
    ok = expectStep(trendName, res, label, status, extra) && ok;
  };
  const staticOk = (res, p) => {
    ok = expectPlain(res, `GET ${p}`, 200) && ok;
  };

  // 3. form page + its script
  const [forms, script] = http.batch([spec('GET', `${path}/forms`), asset('/static/customer.js')]);
  step('forms', forms, 'GET /customer/:token/forms', 200, { 'has form marker': has('Agrega la información del estudiante') });
  staticOk(script, '/static/customer.js');
  if (!ok) return false;
  pause(1, 3);

  // 4. autosave on blur, one field per request
  for (const field of FIELDS) {
    const res = call('POST', `${path}/draft/${field}`, { body: encode({ [field]: form[field] }), htmx: true });
    step('draft', res, 'POST /customer/:token/draft/:field', 200, { 'says Guardado.': has(SAVED) });
    pause(0.3, 1);
  }
  if (!ok) return false;
  pause(1, 3);

  // 5. live PIN check when the field changes: hx-params="pin" sends only the PIN
  const pin = call('POST', `${path}/pin`, { body: encode({ pin: form.pin }), htmx: true });
  step('pin', pin, 'POST /customer/:token/pin', 200, { 'says PIN disponible.': has('PIN disponible.') });
  if (!ok) return false;
  pause(0.3, 1);

  // 6. final submit: htmx answers 200 + HX-Redirect to the agreement
  const submit = call('POST', path, { body: encode(form), htmx: true });
  step('submit', submit, 'POST /customer/:token', 200, { 'HX-Redirect ends /agreement': hdrEnds('HX-Redirect', '/agreement') });
  if (!ok) return false;
  pause(1, 3);

  // 7. agreement page carries the program start date the customer must accept
  const agreement = call('GET', `${path}/agreement`);
  step('agreement', agreement, 'GET /customer/:token/agreement', 200, { 'has agreement marker': has(AGREEMENT) });
  const date = (/name="program_start_date"\s+value="([^"]*)"/.exec(String(agreement.body)) || [])[1];
  if (!date) agreementDateMissing.add(1);
  ok = check(agreement, { 'GET /customer/:token/agreement: program_start_date present': () => Boolean(date) }) && ok;
  if (!ok) return false;
  pause(1, 3);

  // 8. accept terms (plain form post): 303 to payments
  const accept = call('POST', `${path}/agreement`, { body: encode({ accept_terms: 'yes', program_start_date: date }) });
  step('accept', accept, 'POST /customer/:token/agreement', 303, { 'Location ends /payments': hdrEnds('Location', '/payments') });
  if (!ok) return false;
  pause(1, 3);

  // 9. plan selection
  const payments = call('GET', `${path}/payments`);
  step('payments', payments, 'GET /customer/:token/payments', 200, {
    'has plan-selection marker': has(PLAN_SELECTION),
    'lists Pago Único': has('Pago Único'),
  });
  return ok;
}

// Walks one customer from the welcome page to the plan-selection page. Never POSTs /payments
// (that calls Stripe). Returns true only if every step's checks passed.
export function completeToPayments(token, idx, { think: pauses = true } = {}) {
  const path = `/customer/${token}`;
  // 1. welcome page + the assets it references (index.html welcome view)
  if (!welcome(path)) return false;
  if (pauses) think(1, 3);
  // 2. start enrollment session -> 303 to /forms + enrollment_session cookie
  if (!start(path)) return false;
  return completeStarted(token, idx, { think: pauses });
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
    `http_reqs    ${val('http_reqs', 'count')} (${num(val('http_reqs', 'rate'))}/s)   iterations ${val('iterations', 'count')} (${num(val('iterations', 'rate'))}/s)   dropped ${val('dropped_iterations', 'count') || 0}   tokens_exhausted ${val('tokens_exhausted', 'count') || 0}   agreement_date_missing ${val('agreement_date_missing', 'count') || 0}`,
  ];
  const reasons = REASONS.filter((r) => val(`fail_${r}`, 'count')).map((r) => `${r}=${val(`fail_${r}`, 'count')}`);
  if (reasons.length) lines.push(`failures     ${reasons.join(' ')}`);
  lines.push(`${'latency ms'.padEnd(26)}${'n'.padStart(6)}${'fail'.padStart(6)}${'p95'.padStart(9)}${'p99'.padStart(9)}${'max'.padStart(9)}`);
  const steps = STEP_NAMES.map((n) => `step_${n}`).filter((m) => data.metrics[m]);
  for (const name of ['http_req_duration', 'http_req_waiting', 'http_req_connecting', 'http_req_tls_handshaking', 'journey_duration', ...steps]) {
    if (!val(name, 'max')) continue;
    const ok = `${name}_ok`;
    const n = steps.includes(name) ? val(ok, 'passes') + val(ok, 'fails') : '';
    const fails = steps.includes(name) ? val(ok, 'fails') : '';
    lines.push(`${name.padEnd(26)}${String(n).padStart(6)}${String(fails).padStart(6)}${num(val(name, 'p(95)')).padStart(9)}${num(val(name, 'p(99)')).padStart(9)}${num(val(name, 'max')).padStart(9)}`);
  }
  const verdicts = [];
  for (const [name, m] of Object.entries(data.metrics)) {
    for (const [expr, t] of Object.entries(m.thresholds || {})) {
      verdicts.push(`${(t && typeof t === 'object' ? t.ok : t) ? 'PASS' : 'FAIL'}  ${name} ${expr}`);
    }
  }
  lines.push(...verdicts.sort()); // FAIL sorts before PASS
  const failed = [];
  const walk = (g) => {
    [].concat(g.checks || []).forEach((c) => c.fails > 0 && failed.push(`${c.name} (x${c.fails})`));
    [].concat(g.groups || []).forEach(walk);
  };
  if (data.root_group) walk(data.root_group);
  lines.push(failed.length ? `FAILED CHECKS (${failed.length}): ${failed.join(' | ')}` : 'FAILED CHECKS: none');
  return { stdout: `${lines.join('\n')}\n`, [__ENV.OUT || `/tmp/k6/out/${script}.json`]: JSON.stringify(data, null, 2) };
}

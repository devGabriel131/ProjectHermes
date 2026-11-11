// Safety/contract checks, one VU, one pass. Each group uses its own fresh token (6 needed from OFFSET..).
// The login limiter group runs LAST: it spends the process-wide limiter (burst 2, then 1 per 12 s),
// so staff cannot sign in for ~25 s afterwards. Env: BASE_URL, TOKENS_FILE, OFFSET, COUNT, OUT.
import http from 'k6/http';
import exec from 'k6/execution';
import { check, group } from 'k6';
import {
  ORIGIN, OFFSET, baseOptions, call, encode, expect, expectPlain, formValues, has, hdr, hdrEnds, loadTokens, randomToken, spec,
  summarize, target, tokenAt,
} from './lib.js';

export const options = {
  ...baseOptions,
  scenarios: { safety: { executor: 'shared-iterations', vus: 1, iterations: 1, maxDuration: '5m' } },
  batch: 20,
  batchPerHost: 20,
  thresholds: { checks: ['rate==1'] },
};

const tokens = loadTokens();
const NOT_FOUND = 'Este enlace ya no es válido'; // not-found.html
const WELCOME = 'Bienvenidos a Masterminds';
const EVIL = { Origin: 'https://evil.example', 'Sec-Fetch-Site': 'cross-site' };
const SAME_ORIGIN = { Origin: ORIGIN, 'Sec-Fetch-Site': 'same-origin' };
const JSON_BODY = { 'Content-Type': 'application/json' };
const redirectsTo = (suffix) => (r) => target(r).endsWith(suffix);

let used = 0;
function fresh() {
  const i = used++;
  const token = tokenAt(tokens, i);
  if (!token) exec.test.abort(`safety.js needs a token at slice index ${i}; widen OFFSET/COUNT`);
  const form = formValues(OFFSET + i);
  return { token, path: `/customer/${token}`, form, body: encode(form) };
}
const submit = (c, opts = {}) => call('POST', c.path, { body: c.body, htmx: true, ...opts });

export default function () {
  group('1 token boundaries', () => {
    const c = fresh();
    const variants = {
      'short token': 'short',
      '43 chars containing !': `!${'a'.repeat(42)}`,
      'unknown well-formed token': randomToken(),
      'valid token with one altered char': `${c.token.slice(0, 20)}${c.token[20] === 'A' ? 'B' : 'A'}${c.token.slice(21)}`,
    };
    const lengths = new Set();
    for (const [kind, token] of Object.entries(variants)) {
      const path = `/customer/${token}`;
      const get = call('GET', path);
      const post = call('POST', path, { body: c.body, htmx: true });
      const draft = call('POST', `${path}/draft/name`, { body: encode({ name: 'Ana Pérez' }), htmx: true });
      [get, post, draft].forEach((r) => lengths.add(r.body.length));
      expect(get, `${kind}: GET is the branded 404`, 404, { 'has branded body': has(NOT_FOUND) });
      expect(post, `${kind}: final submit is the branded 404 and creates nothing`, 404, { 'has branded body': has(NOT_FOUND) });
      expect(draft, `${kind}: autosave is the branded 404 and asks htmx to refresh`, 404, {
        'HX-Refresh true': (r) => hdr(r, 'HX-Refresh') === 'true',
      });
    }
    check(lengths, { 'every unusable-token response has the same body length (no existence oracle)': (s) => s.size === 1 });

    expect(call('HEAD', c.path), 'HEAD on an unused valid token reports usable', 200, { 'has empty body': (r) => !r.body });
    expect(call('HEAD', `${c.path}/forms`), 'HEAD /forms on an unused valid token reports usable', 200, { 'has empty body': (r) => !r.body });
    for (const method of ['PUT', 'DELETE']) {
      expectPlain(call(method, c.path), `${method} /customer/{token} is not routable`, 405);
    }
    expect(call('GET', c.path), 'probing (HEAD, PUT, DELETE, forged variants) never submits the valid link', 200, {
      'still shows the welcome page': has(WELCOME),
    });
  });

  group('2 draft endpoint', () => {
    const c = fresh();
    const draft = (field, body) => call('POST', `${c.path}/draft/${field}`, { body, htmx: true });
    expect(draft('nope', encode({ nope: 'x' })), 'autosave of an unknown field is refused', 404);
    expect(draft('name', encode({ name: ['Ana Pérez', 'Luis Soto'] })), 'autosave with duplicate values is rejected', 400);
    expect(draft('name', ''), 'autosave without any value is rejected', 400);
    expect(draft('name', encode({ other: 'x' })), 'autosave whose body lacks the field key is rejected', 400);
    expect(draft('name', `name=${'a'.repeat(4092)}`), 'autosave body over the 4 KiB cap is rejected', 413);
    expect(draft('email', encode({ email: 'not-an-email' })), 'autosave of an invalid email is refused, not saved', 422, {
      'says not saved': has('No se guardó'),
    });
    expect(draft('name', encode({ name: c.form.name })), 'autosave of a valid value saves', 200, { 'says Guardado.': has('Guardado.') });
    expect(draft('name', encode({ name: c.form.name })), 'autosave replay of the same value is idempotent', 200, {
      'says Guardado.': has('Guardado.'),
    });
    expect(call('GET', `${c.path}/forms`), 'a saved draft is restored on the form page', 200, {
      'has the saved name': has(`value="${c.form.name}"`),
    });
  });

  group('3 final submit', () => {
    const c = fresh();
    const invalid = {
      'email mismatch': { email_confirm: 'other@example.com' },
      'one-word name': { name: 'María' },
      'phone 123': { phone: '123' },
      'forged municipality': { municipality: 'Atlantis' },
      'future date_of_birth': { date_of_birth: '2999-12-31' },
      'forged gender': { gender: 'Other' },
      'short pin': { pin: '123' },
    };
    for (const [what, override] of Object.entries(invalid)) {
      const r = call('POST', c.path, { body: encode({ ...c.form, ...override }), htmx: true });
      expect(r, `final submit with ${what} is rejected with a field error`, 422, { 'flags the field': has('aria-invalid="true"') });
    }
    const allWrong = encode({ name: 'x', email: 'bad', email_confirm: '', phone: '1', municipality: 'z', date_of_birth: 'nope', gender: 'z', pin: '1' });
    expect(call('POST', c.path, { body: allWrong, htmx: true }), 'final submit collects every field error, not only the first', 422, {
      'flags at least 7 fields': (r) => (r.body.match(/aria-invalid="true"/g) || []).length >= 7,
    });
    expect(call('GET', c.path), 'rejected submits leave the link unsubmitted', 200, { 'still shows the welcome page': has(WELCOME) });

    expect(submit(c), 'valid submit after rejections succeeds and redirects to the agreement', 200, {
      'HX-Redirect ends /agreement': hdrEnds('HX-Redirect', '/agreement'),
    });
    expect(submit(c), 'replaying the valid submit is an idempotent redirect', 200, { 'HX-Redirect ends /agreement': hdrEnds('HX-Redirect', '/agreement') });
    expect(submit(c, { htmx: false }), 'replaying the valid submit without htmx is a 303 to the agreement', 303, {
      'Location ends /agreement': redirectsTo('/agreement'),
    });
    expect(call('GET', c.path), 'GET of a submitted link continues to the agreement', 303, { 'Location ends /agreement': redirectsTo('/agreement') });
    expect(call('GET', `${c.path}/forms`), 'GET /forms of a submitted link continues to the agreement', 303, {
      'Location ends /agreement': redirectsTo('/agreement'),
    });
    expect(call('HEAD', c.path), 'HEAD of a submitted link still reports usable', 200);
    const stale = (htmx) => call('POST', `${c.path}/draft/name`, { body: encode({ name: 'Otra Persona' }), htmx });
    expect(stale(true), 'stale autosave after submit is 409 and asks htmx to refresh', 409, { 'HX-Refresh true': (r) => hdr(r, 'HX-Refresh') === 'true' });
    expect(stale(false), 'stale autosave after submit without htmx is 409', 409);
  });

  group('4 concurrent final submit', () => {
    const c = fresh();
    const responses = http.batch(Array.from({ length: 20 }, () => spec('POST', c.path, { body: c.body, htmx: true })));
    for (const r of responses) {
      expect(r, '20 racing identical submits are each accepted or replayed as a redirect, never 4xx/5xx', [200, 303], {
        'redirects to the agreement': redirectsTo('/agreement'),
      });
    }
    expect(call('GET', `${c.path}/agreement`), 'the agreement is reachable after the submit race', 200, { 'has agreement marker': has('Revisa y acepta el acuerdo') });
  });

  group('5 agreement and payment gating', () => {
    const c = fresh();
    const agreementPost = (form) => call('POST', `${c.path}/agreement`, { body: encode(form) });
    expect(call('GET', `${c.path}/agreement`), 'agreement before submit sends the customer back to the form', 303, { 'Location ends /forms': redirectsTo('/forms') });
    expect(call('GET', `${c.path}/payments`), 'payments before submit sends the customer back to the form', 303, { 'Location ends /forms': redirectsTo('/forms') });
    expect(call('GET', `${c.path}/edit`), 'edit before submit sends the customer back to the form', 303, { 'Location ends /forms': redirectsTo('/forms') });
    expect(agreementPost({ accept_terms: 'yes', program_start_date: '2030-01-01' }), 'accepting terms before submit is refused', 303, {
      'Location ends /forms': redirectsTo('/forms'),
    });

    expect(submit(c), 'submit succeeds', 200, { 'HX-Redirect ends /agreement': hdrEnds('HX-Redirect', '/agreement') });
    expect(call('GET', `${c.path}/payments`), 'payments before accepting terms sends the customer to the agreement', 303, {
      'Location ends /agreement': redirectsTo('/agreement'),
    });
    expect(call('POST', `${c.path}/payments`, { body: encode({ payment_plan: 'full' }) }), 'starting checkout before accepting terms is refused without reaching billing', 303, {
      'Location ends /agreement': redirectsTo('/agreement'),
    });
    expect(call('GET', `${c.path}/edit`), 'contact edit works after submit', 200, { 'has edit form': has('Edita la información del estudiante') });

    const page = call('GET', `${c.path}/agreement`);
    const date = (/name="program_start_date"\s+value="([^"]*)"/.exec(String(page.body)) || [])[1];
    expect(page, 'agreement after submit shows the terms form', 200, { 'has the hidden program_start_date': () => Boolean(date) });

    expect(agreementPost({ program_start_date: date }), 'acceptance without accept_terms is refused', 422, { 'explains consent is required': has('Debes aceptar los términos') });
    expect(agreementPost({ accept_terms: 'no', program_start_date: date }), 'acceptance with accept_terms=no is refused', 422);
    expect(agreementPost({ accept_terms: 'yes' }), 'acceptance without program_start_date is rejected', 400);
    expect(agreementPost({ accept_terms: ['yes', 'yes'], program_start_date: date }), 'acceptance with duplicate accept_terms is rejected', 400);
    expect(agreementPost({ accept_terms: 'yes', program_start_date: '1999-01-01' }), 'acceptance bound to a start date other than the current one is refused', 422, {
      'explains the date changed': has('La fecha de inicio cambió'),
    });
    expect(call('GET', `${c.path}/payments`), 'refused acceptances leave terms unaccepted', 303, { 'Location ends /agreement': redirectsTo('/agreement') });

    expect(agreementPost({ accept_terms: 'yes', program_start_date: date }), 'acceptance with the displayed date succeeds and goes to payments', 303, {
      'Location ends /payments': redirectsTo('/payments'),
    });
    expect(agreementPost({ accept_terms: 'yes', program_start_date: date }), 'replaying acceptance redirects to payments without error', 303, {
      'Location ends /payments': redirectsTo('/payments'),
    });
    expect(agreementPost({ accept_terms: 'yes', program_start_date: '1999-01-01' }), 'replaying acceptance keeps the first agreement even with another date', 303, {
      'Location ends /payments': redirectsTo('/payments'),
    });
    expect(call('GET', `${c.path}/agreement`), 'agreement after acceptance shows it as accepted', 200, { 'says already accepted': has('Ya aceptaste este acuerdo') });
    expect(call('GET', `${c.path}/payments`), 'payments after acceptance shows plan selection', 200, { 'has plan-selection marker': has('Elige tu plan de pago') });
    // Rejected before StartCheckout, so no Stripe call is made.
    expect(call('POST', `${c.path}/payments`, { body: encode({ payment_plan: 'bogus' }) }), 'unknown payment plan is refused without starting checkout', 422);
    expect(call('POST', `${c.path}/payments`, { body: encode({ payment_plan: ['full', 'six'] }) }), 'two payment plans in one request are rejected', 400);
  });

  group('6 cross-origin protection', () => {
    const c = fresh();
    const blocked = (what, r) => expectPlain(r, `cross-origin ${what} is blocked before any handler runs`, 403, { 'says cross-origin': has('cross-origin request detected') });
    blocked('autosave', call('POST', `${c.path}/draft/name`, { body: encode({ name: 'Evil Name' }), htmx: true, headers: EVIL }));
    blocked('final submit', submit(c, { headers: EVIL }));
    blocked('autosave with Origin only (no Sec-Fetch-Site)', call('POST', `${c.path}/draft/name`, { body: encode({ name: 'Evil Name' }), htmx: true, headers: { Origin: EVIL.Origin } }));
    blocked('staff login', call('POST', '/management/login', { body: encode({ username: 'x', password: 'y' }), headers: EVIL }));
    // main.go wraps every route in CrossOriginProtection with no bypass: browser headers block even the webhook.
    blocked('webhook post with browser headers', call('POST', '/webhooks/stripe', { body: '{}', headers: { ...EVIL, ...JSON_BODY } }));
    expectPlain(call('POST', '/webhooks/stripe', { body: '{}', headers: JSON_BODY }), 'webhook post without browser headers (as Stripe sends) reaches the signature check', 400);

    expect(call('GET', c.path, { headers: EVIL }), 'safe methods are not subject to the cross-origin guard', 200, { 'has welcome page': has(WELCOME) });
    expect(call('GET', `${c.path}/forms`), 'blocked cross-origin writes saved nothing', 200, { 'no evil draft stored': (r) => !r.body.includes('Evil Name') });
    expect(call('GET', c.path), 'blocked cross-origin submit did not submit the link', 200, { 'still shows the welcome page': has(WELCOME) });
    expect(call('POST', `${c.path}/draft/name`, { body: encode({ name: c.form.name }), htmx: true, headers: SAME_ORIGIN }), 'same-origin writes pass the guard', 200, {
      'says Guardado.': has('Guardado.'),
    });
  });

  group('7 staff area signed out', () => {
    const targets = [
      ['GET', '/management/events'],
      ['GET', '/management/students'],
      ['GET', '/management/trash'],
      ['GET', '/management/settings'],
      ['POST', '/management/links'],
    ];
    for (const [method, path] of targets) {
      expectPlain(call(method, path, method === 'POST' ? { body: '' } : {}), `signed-out ${method} ${path} redirects to sign-in`, 303, {
        'Location is /management/': hdrEnds('Location', '/management/'),
        'leaks no student/link registry data': (r) => !/student|link/i.test(r.body || ''),
      });
    }
    expectPlain(call('GET', '/management/students', { htmx: true }), 'signed-out htmx request gets 401, not a swapped-in page', 401, {
      'HX-Redirect is /management/': hdrEnds('HX-Redirect', '/management/'),
    });
    expectPlain(call('GET', '/management/'), 'GET /management/ signed out serves the login form uncached', 200, {
      'Cache-Control no-store': (r) => hdr(r, 'Cache-Control').includes('no-store'),
      'has the username field': has('name="username"'),
      'has the password field': has('name="password"'),
    });
  });

  group('8 stripe webhook', () => {
    const hook = (headers, body = '{}') => call('POST', '/webhooks/stripe', { body, headers: { ...JSON_BODY, ...headers } });
    expectPlain(hook({}), 'webhook without Stripe-Signature is rejected', 400);
    expectPlain(hook({ 'Stripe-Signature': 't=1,v1=deadbeef' }), 'webhook with a garbage Stripe-Signature is rejected', 400);
    // Exactly the 1 MiB cap + 1 byte so the server consumes the whole body before answering.
    expectPlain(hook({}, 'x'.repeat((1 << 20) + 1)), 'webhook body over 1 MiB is rejected before signature checks', 413);
    expectPlain(call('GET', '/webhooks/stripe'), 'GET /webhooks/stripe is not allowed', 405);
  });

  group('9 static files', () => {
    expectPlain(call('GET', '/static/customer.css'), 'static customer.css is served', 200, { 'has a css content type': (r) => hdr(r, 'Content-Type').includes('css') });
    for (const path of ['/static/../go.mod', '/static/%2e%2e/go.mod', '/static/..%2fgo.mod', '/static/%2e%2e%2fgo.mod', '/static/../main.go']) {
      const r = call('GET', path);
      check(r, {
        [`traversal ${path} is not served`]: (res) => res.status !== 200,
        [`traversal ${path} leaks no Go source`]: (res) => !String(res.body).includes('module projecthermes') && !String(res.body).includes('package main'),
      });
    }
  });

  group('10 login limiter (last)', () => {
    const body = encode({ username: 'k6-nobody', password: 'wrong-password' });
    const attempts = Array.from({ length: 6 }, () => call('POST', '/management/login', { body, headers: SAME_ORIGIN }));
    attempts.forEach((r) => expectPlain(r, 'wrong credentials never sign in and never 5xx', [401, 429]));
    const codes = attempts.map((r) => r.status);
    const firstLimited = codes.indexOf(429);
    check(codes, {
      'login limiter: at most 3 of 6 rapid attempts reach the password check (burst 2 + one refill)': (c) => c.filter((s) => s === 401).length <= 3,
      'login limiter: once limited the following attempts stay limited': () => firstLimited === -1 || codes.slice(firstLimited).every((s) => s === 429),
    });
    expectPlain(attempts[5], 'login limiter answers 429 with Retry-After after the burst', 429, {
      'Retry-After is positive': (r) => parseInt(hdr(r, 'Retry-After'), 10) > 0,
      'says too many attempts': has('Too many sign-in attempts'),
    });
  });
}

export const handleSummary = (data) => summarize('safety', data);

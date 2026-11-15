// Pays the hosted Checkout Sessions a capacity.js run created, in a real browser (Playwright, run by pay.fish).
// k6 cannot do this: the card is typed on Stripe's page. Reads the `CHECKOUT <idx> <plan> <url>` lines from LOG and
// plays each customer a scenario from SCENARIOS (by idx): card brands, 3-D Secure, declines the customer fixes with
// another card, declines the customer gives up on, and cards Stripe disputes after paying. Failures are retried one at
// a time (parallel tabs time out under load; the same sessions pay when retried serially).
// Prints PAID / DECLINED (a Session the scenario leaves open and unpaid on purpose) / FAILED per customer.
// Env: LOG (default /tmp/k6/out/capacity.log), CONCURRENCY (default 3).
const fs = require('fs');
const { chromium } = require('playwright');

const LOG = process.env.LOG || '/tmp/k6/out/capacity.log';
const CONCURRENCY = Number(process.env.CONCURRENCY || 3);

// cards are typed in order: every card but the last must be declined on the page, the last pays, or (gaveUp) is declined too.
const ok = (name, card, cvc = '424') => ({ name, cards: [card], cvc });
const fixed = (name, declined) => ({ name, cards: [declined, '4242424242424242'], cvc: '424' });
const gaveUp = (name, declined) => ({ name, cards: [declined], cvc: '424', gaveUp: true });
// The length must not be a multiple of 3: capacity.js picks the plan by idx % 3, and each scenario must meet every plan.
const SCENARIOS = [
  // brands and funding types
  ok('visa', '4242424242424242'),
  ok('visa debit', '4000056655665556'),
  ok('visa instant balance', '4000000000000077'),
  ok('mastercard', '5555555555554444'),
  ok('mastercard 2-series', '2223003122003222'),
  ok('mastercard debit', '5200828282828210'),
  ok('mastercard prepaid', '5105105105105100'),
  ok('amex', '378282246310005', '4242'),
  ok('amex 2', '371449635398431', '4242'),
  ok('discover', '6011111111111117'),
  ok('discover 2', '6011000990139424'),
  ok('discover debit', '6011981111111113'),
  ok('diners', '3056930009020004'),
  ok('diners 14 digits', '36227206271667'),
  ok('jcb', '3566002020360505'),
  ok('unionpay', '6200000000000005'),
  // 3-D Secure: the challenge is completed on the page
  ok('3ds required once (off-session charges work after)', '4000002500003155'),
  ok('3ds always required (later installments need authentication)', '4000000000003220'),
  ok('3ds optional', '4000000000003055'),
  fixed('3ds authenticated, then declined', '4000008400001629'),
  // declines the customer fixes with a visa
  fixed('generic decline', '4000000000000002'),
  fixed('insufficient funds', '4000000000009995'),
  fixed('lost card', '4000000000009987'),
  fixed('stolen card', '4000000000009979'),
  fixed('expired card', '4000000000000069'),
  fixed('incorrect cvc', '4000000000000127'),
  fixed('processing error', '4000000000000119'),
  fixed('attaches, but charges fail', '4000000000000341'),
  // declines the customer gives up on: the Session stays open and unpaid
  gaveUp('generic decline, gave up', '4000000000000002'),
  gaveUp('insufficient funds, gave up', '4000000000009995'),
  gaveUp('radar block, gave up', '4100000000000019'),
  // paid, then disputed or flagged by Stripe shortly after
  ok('fraudulent dispute', '4061219946393810'),
  ok('dispute inquiry', '4000000000001976'),
  ok('early fraud warning', '4000000000005423'),
  ok('product-not-received dispute', '4000000000002685'),
];

const sessions = [...fs.readFileSync(LOG, 'utf8').matchAll(/CHECKOUT (\d+) (full|three|six) (https:\/\/checkout\.stripe\.com\/[^"\s]+)/g)].map(
  ([, idx, plan, url]) => ({ idx, plan, url, scenario: SCENARIOS[Number(idx) % SCENARIOS.length] }),
);
if (!sessions.length) {
  console.error(`no CHECKOUT lines in ${LOG}: run capacity.js first (CHECKOUT is on by default)`);
  process.exit(2);
}

// Stripe's hosted page renders the payment form inside iframes, so a locator is looked up in whichever frame has it.
async function find(page, locate) {
  const deadline = Date.now() + 30000;
  for (;;) {
    for (const frame of page.frames()) {
      const found = locate(frame);
      if (await found.count().catch(() => 0)) return found.first();
    }
    if (Date.now() > deadline) throw new Error('payment form element not found');
    await page.waitForTimeout(250);
  }
}

async function anyFrame(page, locate, keep = () => true) {
  for (const frame of page.frames().filter(keep)) {
    const found = locate(frame);
    if (await found.count().catch(() => 0)) return found.first();
  }
  return null;
}

// What the page does after Pay: completes a 3-D Secure challenge when one opens, then ends in the return page
// ('paid') or an inline error ('declined'; editing the card clears it).
async function settle(page) {
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    if (/\/payments\/return/.test(page.url())) return 'paid';
    const authorize = await anyFrame(page, (f) => f.locator('#test-source-authorize-3ds'));
    if (authorize) await authorize.click().catch(() => {});
    // hCaptcha's frame always carries an alert of its own, so only Stripe's frames can say the card was declined.
    else if (await anyFrame(page, (f) => f.locator('[role="alert"]'), (f) => f.url().startsWith('https://js.stripe.com/'))) return 'declined';
    await page.waitForTimeout(250);
  }
  throw new Error('no result after Pay');
}

async function pay(browser, { idx, url, scenario }) {
  const context = await browser.newContext({ locale: 'es-419' });
  try {
    const page = await context.newPage();
    await page.goto(url, { waitUntil: 'domcontentloaded' });
    await (await find(page, (f) => f.getByText('Tarjeta', { exact: true }))).click();
    for (const [n, card] of scenario.cards.entries()) {
      await (await find(page, (f) => f.getByLabel('Número de tarjeta'))).fill(card);
      await (await find(page, (f) => f.getByLabel('Vencimiento'))).fill('12/30');
      await (await find(page, (f) => f.getByLabel('Código de seguridad'))).fill(scenario.cvc);
      await (await find(page, (f) => f.getByLabel('Nombre del titular'))).fill(`K6 ${idx}`);
      await (await find(page, (f) => f.getByRole('button', { name: 'Pago', exact: true }))).click();
      const outcome = await settle(page);
      const wantPaid = n === scenario.cards.length - 1 && !scenario.gaveUp;
      if (outcome !== (wantPaid ? 'paid' : 'declined')) throw new Error(`card ${card} ${outcome}, expected ${wantPaid ? 'paid' : 'declined'}`);
    }
    return scenario.gaveUp ? 'DECLINED' : 'PAID';
  } finally {
    await context.close();
  }
}

async function runAll(browser, list, workers) {
  const failed = [];
  const queue = [...list];
  await Promise.all(
    Array.from({ length: workers }, async () => {
      for (let s; (s = queue.shift()); ) {
        try {
          console.log(`${await pay(browser, s)} ${s.idx} ${s.plan} ${s.scenario.name}`);
        } catch (err) {
          failed.push(s);
          console.log(`FAILED ${s.idx} ${s.plan} ${s.scenario.name}: ${String(err.message).split('\n')[0]}`);
        }
      }
    }),
  );
  return failed;
}

(async () => {
  const browser = await chromium.launch();
  let failed = await runAll(browser, sessions, CONCURRENCY);
  for (let pass = 0; pass < 2 && failed.length; pass++) {
    console.log(`retrying ${failed.length} one at a time`);
    failed = await runAll(browser, failed, 1);
  }
  await browser.close();
  const byPlan = {};
  for (const s of sessions) byPlan[s.plan] = (byPlan[s.plan] || 0) + 1;
  const left = sessions.filter((s) => s.scenario.gaveUp).length;
  console.log(`done ${sessions.length - failed.length}/${sessions.length} (${Object.entries(byPlan).map(([p, n]) => `${p} ${n}`).join(', ')}); ${left} left unpaid on purpose`);
  process.exit(failed.length ? 1 : 0);
})();

// Pays the hosted Checkout Sessions a capacity.js run created, in a real browser (Playwright, run by pay.fish).
// k6 cannot do this: the card is typed on Stripe's page. Reads the `CHECKOUT <idx> <plan> <url>` lines from LOG,
// pays each with a sandbox card rotated by customer index, then retries failures one at a time (parallel tabs
// time out under load; the same sessions pay when retried serially). Prints PAID / FAILED per customer.
// Env: LOG (default /tmp/k6/out/capacity.log), CONCURRENCY (default 3), CARDS (comma-separated numbers).
const fs = require('fs');
const { chromium } = require('playwright');

const LOG = process.env.LOG || '/tmp/k6/out/capacity.log';
const CONCURRENCY = Number(process.env.CONCURRENCY || 3);
const CARDS = (process.env.CARDS || '4242424242424242,5555555555554444,4000056655665556').split(',');

const sessions = [...fs.readFileSync(LOG, 'utf8').matchAll(/CHECKOUT (\d+) (full|three|six) (https:\/\/checkout\.stripe\.com\/[^"\s]+)/g)].map(
  ([, idx, plan, url]) => ({ idx, plan, url }),
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

async function pay(browser, { idx, url }) {
  const context = await browser.newContext({ locale: 'es-419' });
  try {
    const page = await context.newPage();
    await page.goto(url, { waitUntil: 'domcontentloaded' });
    await (await find(page, (f) => f.getByText('Tarjeta', { exact: true }))).click();
    await (await find(page, (f) => f.getByLabel('Número de tarjeta'))).fill(CARDS[idx % CARDS.length]);
    await (await find(page, (f) => f.getByLabel('Vencimiento'))).fill('12/30');
    await (await find(page, (f) => f.getByLabel('Código de seguridad'))).fill('424');
    await (await find(page, (f) => f.getByLabel('Nombre del titular'))).fill(`K6 ${idx}`);
    await (await find(page, (f) => f.getByRole('button', { name: 'Pago', exact: true }))).click();
    await page.waitForURL(/\/payments\/return/, { timeout: 60000 });
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
          await pay(browser, s);
          console.log(`PAID ${s.idx} ${s.plan}`);
        } catch (err) {
          failed.push(s);
          console.log(`FAILED ${s.idx} ${s.plan} ${String(err.message).split('\n')[0]}`);
        }
      }
    }),
  );
  return failed;
}

(async () => {
  const browser = await chromium.launch();
  let failed = await runAll(browser, sessions, CONCURRENCY);
  if (failed.length) {
    console.log(`retrying ${failed.length} one at a time`);
    failed = await runAll(browser, failed, 1);
  }
  await browser.close();
  const byPlan = {};
  for (const s of sessions) byPlan[s.plan] = (byPlan[s.plan] || 0) + 1;
  console.log(`paid ${sessions.length - failed.length}/${sessions.length} (${Object.entries(byPlan).map(([p, n]) => `${p} ${n}`).join(', ')})`);
  process.exit(failed.length ? 1 : 0);
})();

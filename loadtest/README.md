# k6 load and safety scripts

**Warning:** `BASE_URL` is required and a run fails without it; `checkout.js`, and `capacity.js` unless `CHECKOUT=0`, create real Stripe sandbox Checkout Sessions through whatever host they name. Point them at a local server, or set `CHECKOUT=0`, to avoid that.

Scripts for the app at `BASE_URL` (required, no default). `journey.js`, `safety.js` and `checkout.js` take tokens from a JSON
array of 43-char link tokens at `TOKENS_FILE` (default `/tmp/k6/tokens.json`); each uses `OFFSET`..`OFFSET+COUNT`.
Tokens are only revealed when staff create a link (POST /management/links) and this repository has no generator, so build the file from links you created
(`link.fish seed` below makes one).
Each reusable link supports up to 100 enrollments (`POST /customer/{token}/start` issues an `enrollment_session` cookie per customer); use disjoint `OFFSET` slices across runs so PINs never collide.
PINs come from the token index (`100000 + OFFSET + i`) and must be unique in the database, so slices never overlap, not even across token files.
The summary JSON goes to `OUT` (default `/tmp/k6/out/<script>.json`).

- `journey.js` load test: welcome page to plan selection, `PROFILE=100|1000`. Never POSTs `/payments`. ~850 / ~16000 tokens.
- `safety.js` single-pass contract checks (tokens, drafts, submit, gating, cross-origin, staff, webhook, static, login limiter). Needs 6 tokens.
  The login limiter group runs last and blocks staff sign-in for ~25 s afterwards.
- `capacity.js` reusable-link capacity and arrival test: `ATTEMPTS` customers (default `CAP * links + 20`) arrive one after another, evenly spaced over
  `WINDOW` seconds (default 60), round-robin over the links in `TOKENS` (comma-separated; `TOKEN` for one, or `tokens[OFFSET]`).
  `WINDOW=0` sends them all at once, the worst-case burst (`VUS` caps concurrency there). Verifies each link takes `CAP` (default 100)
  customers through to plan selection and turns the rest away with the branded 404; on a single link it also checks that an existing unpaid session resumes once full.
  1000 customers, one every quarter second, on 10 links: `ATTEMPTS=1000 WINDOW=250 TOKENS=<10 tokens>`.
  The PIN base is random per run unless `OFFSET` is set. `THINK=0` skips think time. Every journey ends with `POST /payments`, a real Checkout Session per customer
  (plan `full`/`three`/`six` by index; prints `CHECKOUT <idx> <plan> <url>`); `CHECKOUT=0` skips it. Each journey also fires the live `POST /pin` check before submit, as the form does.
- `checkout.js` starts real Stripe Checkout Sessions and races duplicate `POST /payments`: `PLAN=full|three|six`, `COUNT<=20` (default 1),
  `RACE` (default 10), `CONCURRENT_TOKENS` (default 1). Prints `CHECKOUT <token> <plan> <url-or-NONE> <distinct-url-count>` per token.
- `lib.js` shared helpers: request specs and checks, the page markers (`WELCOME`, `NOT_FOUND`, `AGREEMENT`, `PLAN_SELECTION`, `SAVED`, `STRIPE_CHECKOUT`),
  the journey steps (`welcome` with its assets, `start`, `completeStarted`, `completeToPayments`), `think` time, `journeyMetrics` (the 200-399 response
  callback plus `journey_duration`), `stepThresholds` and the summary.
- `run.fish` runs a script in Docker with the live dashboard and a failure-cause summary; `link.fish` seeds, verifies and cleans a link;
  `pay.fish` (with `pay.cjs`) pays the Checkout Sessions a `capacity.js` run created, by typing sandbox cards into Stripe's hosted page in a headless browser (Playwright in Docker).

## Reading a run

- Every failed check logs `FAIL <customer index> <step> status=<n> reason=<reason> [alert="..."]` and counts under `fail_<reason>`:
  `pin_taken`, `validation`, `rate_limited`, `not_found`, `forbidden`, `server_error`, `network`, `wrong_body` (right status, wrong body or headers), `other`.
  `run.fish` keeps these lines in the log and prints them grouped by cause.
- Latency is budgeted per step (p95<500, p99<1000; checkout, which calls Stripe, p95<1500, p99<3000) as well as globally. The summary table lists each step's
  requests, failures and percentiles, plus `http_req_waiting` (server time), `http_req_connecting` and `http_req_tls_handshaking`.
  `http_req_duration` excludes connecting and TLS, so a slow start shows in those rows or in `http_req_waiting`, not in the duration.
  Unless `CHECKOUT=0`, the global `http_req_duration` threshold is dropped (it would include the Stripe call); the per-step ones stay.
- Each iteration gets its own cookie jar (`newSession` in `lib.js`). `http.cookieJar().clear()` does not remove the path-scoped `enrollment_session` cookie, so clearing would resume the previous customer.

## Running

Run from the repository root. `run.fish` writes `/tmp/k6/out/<script>.{json,log,html}` and serves the dashboard at <http://127.0.0.1:5665> during the run.

```fish
token=(loadtest/link.fish seed "k6 capacity")
loadtest/run.fish capacity.js BASE_URL=http://127.0.0.1:3000 TOKEN=$token                       # arrivals over 60 s
loadtest/run.fish capacity.js BASE_URL=http://127.0.0.1:3000 TOKEN=$token WINDOW=0              # all at once
loadtest/link.fish verify $token                                                                # what the database holds
loadtest/link.fish clean $token

loadtest/run.fish journey.js BASE_URL=... PROFILE=100 OFFSET=0 COUNT=1500
loadtest/run.fish safety.js BASE_URL=... OFFSET=2000 COUNT=40
loadtest/run.fish checkout.js BASE_URL=... PLAN=full OFFSET=2100 COUNT=5 RACE=10
```

`link.fish` runs psql inside the Compose `db` service against `DATABASE_URL` (default: `.env`). For Railway, open a temporary TCP proxy on the Postgres service,
export a `DATABASE_URL` that points at it (`?sslmode=require`), and delete the proxy afterwards. `clean` refuses links not seeded by `link.fish`,
removes the link's enrollments, students and billing rows, and leaves Stripe sandbox objects alone.

### Paying the sessions: 100 customers, all three plans

`capacity.js` only creates the Checkout Sessions; k6 cannot type a card. `pay.fish` pays every `CHECKOUT` line in the log (`LOG`, default `/tmp/k6/out/capacity.log`), three tabs at a time,
rotating the sandbox cards by customer index and retrying failures one at a time. The plan is already spread by index (`full`, `three`, `six`), so 100 customers give 34 / 33 / 33.
The first run pulls the Playwright image (about 2 GB). Pay against sessions a deployed app created: Stripe sends its webhooks to the app's `PUBLIC_BASE_URL`, so a local app never hears the payment.

```fish
token=(loadtest/link.fish seed "k6 payments")
loadtest/run.fish capacity.js BASE_URL=https://<app> TOKEN=$token ATTEMPTS=100 WINDOW=60   # 100 customers, one link
loadtest/pay.fish                                                                          # PAID / FAILED per customer
loadtest/link.fish verify $token                                                           # paid, students, checkout states
```

With `INSTALLMENT_SPACING=15m` set on the app (sandbox only, see [Configuration](../docs/operations/configuration.md)), the `three` and `six` customers' later installments fall due 15, 30, ... minutes after they pay,
so running `link.fish verify` again after that shows whether those installments were charged.

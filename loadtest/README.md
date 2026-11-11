# k6 load and safety scripts

**Warning:** `BASE_URL` is required and a run fails without it; checkout.js creates real Stripe sandbox Checkout Sessions through whatever host it names. Point it at a local server to avoid that.

Scripts for the app at `BASE_URL` (required, no default). Tokens come from a JSON
array of 43-char link tokens at `TOKENS_FILE` (default `/tmp/k6/tokens.json`); each script uses `OFFSET`..`OFFSET+COUNT`.
Tokens are only revealed when staff create a link (POST /management/links) and this repository has no generator, so build the file from links you created.
One link is one customer and can be submitted once, so use disjoint slices per script and never reuse a token across runs.
PINs come from the token index (`100000 + OFFSET + i`) and must be unique in the database, so slices never overlap, not even across token files.
The summary JSON goes to `OUT` (default `/tmp/k6/out/<script>.json`); run `mkdir -p /tmp/k6/out` first.

- `journey.js` load test: welcome page to plan selection, `PROFILE=100|1000`. Never POSTs `/payments`. ~850 / ~16000 tokens.
- `safety.js` single-pass contract checks (tokens, drafts, submit, gating, cross-origin, staff, webhook, static, login limiter). Needs 6 tokens.
  The login limiter group runs last and blocks staff sign-in for ~25 s afterwards.
- `checkout.js` starts real Stripe Checkout Sessions and races duplicate `POST /payments`: `PLAN=full|three|six`, `COUNT<=20` (default 1),
  `RACE` (default 10), `CONCURRENT_TOKENS` (default 1). Prints `CHECKOUT <token> <plan> <url-or-NONE> <distinct-url-count>` per token.
- `lib.js` shared helpers and the journey steps.

Run from the repository root.

```sh
k6() { docker run --rm --network host --ulimit nofile=1048576:1048576 -v "$PWD/loadtest:/loadtest" -v /tmp/k6:/tmp/k6 "$@"; }
k6 -e PROFILE=100 -e OFFSET=0 -e COUNT=1500 grafana/k6:latest run /loadtest/journey.js
k6 -e OFFSET=2000 -e COUNT=40 grafana/k6:latest run /loadtest/safety.js
k6 -e PLAN=full -e OFFSET=2100 -e COUNT=5 -e RACE=10 grafana/k6:latest run /loadtest/checkout.js
```

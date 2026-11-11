# Testing

How the app is verified: the automated layers and their commands, the helpers that isolate them (`testdb`, `billingtest`), what stays manual, and what no test covers today. Load scripts are documented in [loadtest/README.md](../loadtest/README.md); local setup is in [Local development](operations/local-development.md).

## Layers

```mermaid
flowchart LR
    unit["Unit tests\ngo test ./...\nno database"] --> integration["Integration tests\ngo test -tags=integration ./...\none testdb schema per test"]
    integration --> manual["Manual browser checks\nlisted below"]
    manual --> k6["k6 scripts in loadtest/"]
    integration --- pg[("hermes_test\nTEST_DATABASE_URL")]
    integration --- fake["billingtest\nfake Stripe over httptest"]
    k6 -->|"BASE_URL"| deployed["Deployed app\nreal Stripe sandbox"]
```

| Layer | Selected by | Needs | Proves |
|---|---|---|---|
| Unit | no build tag | nothing | Pure logic: Stripe config and catalog validation, log shapes, plan schedules, field parsers, SSE stream timing, the shutdown sequence |
| Integration | `-tags=integration` | PostgreSQL at `TEST_DATABASE_URL` | Stores, handlers, billing worker and webhook against a real database and a fake Stripe, and the real route wiring in `main` |
| Manual browser | a person | a running app, sandbox Stripe | Behavior no Go test can see; list [below](#not-proven-by-tests) |
| k6 | `docker run grafana/k6` | tokens file, reachable `BASE_URL` | Throughput and single-pass contract checks against a running server |

Untagged (unit) test files: `main_test.go` (`TestServeLifecycle`: in-flight request drains on shutdown; a blocked request is force-closed after the drain timeout), `internal/logging/{logging_test.go,http_test.go,database_test.go}` (JSON shape, error classification and privacy, request IDs, slow-query summaries; `http_integration_test.go` carries `integration`), `internal/live` (stream catch-up, stalled reader, shutdown budget, heartbeats), `internal/payments/plans_test.go`, `internal/students/fields_test.go`, `internal/settings/parse_test.go` and `internal/billing/stripe_test.go`. Every other test file carries the `integration` tag.

## Commands

Integration tests need a dedicated database. Create it once, then run the checks:

```fish
docker compose up --detach --wait --wait-timeout 90 db
docker compose exec -T db createdb -U postgres hermes_test
set -gx TEST_DATABASE_URL 'postgres://postgres:postgres@127.0.0.1:5432/hermes_test?sslmode=disable'
go build ./...
go vet -tags=integration ./...
go test -count=1 ./...  # unit tests, no database
go test -tags=integration -count=1 ./...
go test -race -tags=integration ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/tools/cmd/deadcode@latest -test -tags=integration ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

- The test database is separate from the development database (`hermes`) so tests never touch dev data. The credentials are the Compose ones ([Local development](operations/local-development.md)).
- With `TEST_DATABASE_URL` unset or not a PostgreSQL URL, an integration test **fails** (`testdb.Open` calls `t.Fatal`); it never silently skips.
- `deadcode` runs with `-test -tags=integration` so helpers used only by integration tests (`billingtest`, `testdb`) count as used.
- The `@latest` tools are fetched on first run.

## `testdb`: one schema per test

`testdb.Open(t, models...)` gives each test an isolated database view on the one `TEST_DATABASE_URL` database:

1. It generates a schema name `pwtest_<16 hex digits>` and connects with `search_path` set to it, so every query, including those made through `db.DB()`, lands in that schema.
2. It runs `CREATE SCHEMA`, then `AutoMigrate(models...)` within a 10 s budget.
3. Cleanup runs in reverse registration order: it drops the schema first (`DROP SCHEMA ... CASCADE`, 5 s budget, a failure fails the test), then closes the pool.

Tests therefore run in parallel without sharing rows. Nothing sweeps schemas left by a killed test run, so stray `pwtest_*` schemas can accumulate in the test database. `testdb.AwaitBlocked` supports lock-contention tests: it polls `pg_blocking_pids` until a session is really waiting on the blocker's open transaction, then lets the test release the lock, so those tests act on observed database state instead of elapsed time. It fails the test if the call returns before blocking.

## `billingtest`: fake Stripe

`internal/billing/billingtest` builds a real `billing.Service` for other packages' tests without touching Stripe:

| Function | Provides |
|---|---|
| `Config` | a valid sandbox `billing.Config` (`BaseURL` `http://127.0.0.1:3000`, test webhook secret and configuration IDs) |
| `Catalog` | a fresh price map covering every plan installment and the reinstatement fee |
| `Client` | a Stripe SDK client pointed at an `httptest` server running the test's own `http.Handler` (404 for everything when the handler is `nil`); no retries, no SDK logging |
| `New` | `billing.New` over a test database with `Config`, `Catalog`, that client, and no live feed or logger |

The fake is whatever handler a test supplies: billing worker and checkout tests register the Stripe API paths they expect and assert the resulting rows; tests that must not reach Stripe pass `nil` so any call fails with 404. No Go test contacts Stripe.

## Wiring tests

`main_wiring_integration_test.go` runs the real `newServer` handler over a `testdb` database with `billingtest.New(t, db, nil)`:

| Test | Guards |
|---|---|
| `TestRoutesAreMounted` | `/` redirects to `/management/`; `/static/styles.css` and `/favicon.ico` are public; `/management/events` requires sign-in; `POST /webhooks/stripe` bypasses sign-in and rejects unsigned events with 400 |
| `TestPublicCustomerIgnoresStaffSessions` | with the `sessions` table made unavailable, `HEAD /customer/{token}` for an unknown token still answers 404 with the customer headers, an empty body, no `Vary: Cookie` and no cookie, while `/management/` answers 500 |
| `TestCrossOriginWritesAreRejected` | management, customer and login `POST`s with cross-site `Sec-Fetch-Site` or foreign `Origin` get 403; without either header they reach their handlers |

Helpers: `newTestServer` builds the handler, `signInStaff` stores an account as the CLI does and signs in through the real form, `send` serves one request in memory. The route table itself is documented in [Architecture](architecture.md).

## Deliberate choices

- **`testdb` repeats the production pool limits** (10 open, 10 idle, as in `main.go`), so concurrency tests run under the same connection pressure as the server.
- **`billingtest` is untagged but test-only.** Importing it from other packages' integration tests needs no build tag; `deadcode` reports it clean because it runs with the integration tag.
- **Lock tests wait on database state, not time** (`AwaitBlocked`), so they are neither flaky nor slow.
- **The webhook route is tested through the real handler**, not just the billing package, so a mounting mistake in `main.go` cannot pass unseen.

## Not proven by tests

Tests do not prove randomness statistically. No test proves these browser checks, which a person runs after relevant changes:

- Native dialog focus after an out-of-band row replacement; clipboard copy toast and failure recovery.
- Live updates across tabs, and reconnect after a restart with URL and focus kept; the 24-hour archive cutoff on the next load.
- Layouts at 320x568, 390x844 and 1440x900 with no horizontal overflow.
- JavaScript-disabled flows through submit, agreement and payments.
- Hosted Checkout round trip with Stripe sandbox cards, and wallet display on a real device.

## Load tests

`loadtest/` holds three k6 scripts (`journey.js`, `safety.js`, `checkout.js`) and shared helpers; usage, token files and slices are in [loadtest/README.md](../loadtest/README.md). Two facts matter here:

- "No test calls Stripe" holds for Go tests only. `checkout.js` creates **real Stripe sandbox Checkout Sessions** through the app it targets.
- `BASE_URL` defaults to the deployed Railway URL (`loadtest/lib.js`), so a run without `BASE_URL` hits the deployed service ([Railway](operations/railway.md)).

## Known test gaps

| Area | What is untested | Covered elsewhere |
|---|---|---|
| Student list filters | the handler's parsing of `?status`, `?cohort` and `?group` (valid values kept, unknown values ignored) | the `students.Filter` query itself is tested in `internal/students` |
| `HX-Replace-Url` | the header `GET /management/students` sets so the address bar keeps the active filters | none |
| `GET /management/dashboard` | the handler and its page: no test requests it | `billing.Service.Overview`, which computes the figures, is tested in `internal/billing` |

## Source

- `main_test.go`: `TestServeLifecycle`
- `main_wiring_integration_test.go`: `TestRoutesAreMounted`, `TestPublicCustomerIgnoresStaffSessions`, `TestCrossOriginWritesAreRejected`, `newTestServer`, `signInStaff`, `send`
- `internal/testdb/testdb.go`: `Open`, `AwaitBlocked`, `TEST_DATABASE_URL`
- `internal/billing/billingtest/billingtest.go`: `Config`, `Catalog`, `Client`, `New`
- `internal/management/students.go`: `HX-Replace-Url`
- `internal/management/dashboard.go`: `registerDashboardRoutes`
- `internal/billing/overview.go`: `Overview`
- `internal/students/students_test.go`: `Filter`
- `loadtest/lib.js`: `BASE_URL`, `BASE`
- `compose.yaml`: `POSTGRES_DB`

# Configuration

Every environment variable the code reads, with its default, validation and the startup stage that fails when it is wrong. Startup order is in [Startup and shutdown](startup-shutdown.md); Stripe dashboard objects these variables point at are in [Stripe setup](stripe-setup.md); local and Railway values are in [Local development](local-development.md) and [Railway](railway.md).

The application never loads a `.env` file itself; locally, Air injects `.env` (`env_files` in `.air.toml`) and variables already set in the shell win. There are no price-ID variables: prices are found in Stripe by lookup key ([Stripe setup](stripe-setup.md)).

## Variables

| Variable | Used by | Default | Validation | Failure stage |
|---|---|---|---|---|
| `DATABASE_URL` | server, `user` CLI | none; required | Non-empty, then parsed by the pgx driver | `configuration` (empty), `database_open` (unparseable), `database_connect` (unreachable or 10 s spent), `database_baseline`, `database_migrate` |
| `PORT` | server only | unset: listen on `127.0.0.1:3000`. Set: listen on `:<PORT>` (all interfaces) | None; a bad value fails when binding | `listen` |
| `STRIPE_API_KEY` | server only | none; required | Non-empty; prefix `rk_test_` or `sk_test_` when `STRIPE_MODE=sandbox`, `rk_live_` or `sk_live_` when `live` | `billing_configuration` |
| `STRIPE_MODE` | server only | none; required | Exactly `sandbox` or `live` | `billing_configuration` |
| `PUBLIC_BASE_URL` | server only | none; required | Absolute URL with a host; no userinfo, query, fragment or path prefix (a lone `/` is allowed); `https` in live mode; in sandbox `https`, or `http` only for `localhost`, `127.0.0.1` or `::1`. Its scheme also decides `Secure` on both session cookies | `billing_configuration` |
| `STRIPE_WEBHOOK_SECRET` | server only | none; required | Not blank (whitespace-only fails) | `billing_configuration` |
| `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID` | server only | none; required | Not blank | `billing_configuration` |
| `STRIPE_PORTAL_CONFIGURATION_ID` | server only | none; required | Not blank | `billing_configuration` |
| `INSTALLMENT_SPACING` | server only | unset: monthly due dates | Optional. A positive Go duration such as `15m`; refused when `STRIPE_MODE=live`. Sandbox testing only: installment *i* falls due `i × INSTALLMENT_SPACING` after funding instead of on monthly midnights ([Billing overview](../billing/overview.md#due-dates)) | `configuration` |
| `INSTALLMENT_RETRY_DELAY` | server only | unset: 24 h | Optional. A positive Go duration such as `24h`: how long after a failure, and between retries, the app pays a failed later installment again, at most twice ([Invoices](../billing/invoices.md#retrying-a-failed-installment)) | `billing_configuration` |
| `TEST_DATABASE_URL` | integration tests only (`-tags=integration`) | none; the test fails | Must parse as a URL with a host and scheme `postgres` or `postgresql`; use a dedicated database | none (test failure) |
| `RAILWAY_SERVICE_NAME` | server, `user` CLI (log field `service`) | `Apollo` | None | never fails |
| `RAILWAY_ENVIRONMENT_NAME` | server, `user` CLI (log field `environment`) | `local` | None | never fails |
| `RAILWAY_GIT_COMMIT_SHA` | server, `user` CLI (log field `version`) | `unknown` | None | never fails |
| `RAILWAY_DEPLOYMENT_ID` | server, `user` CLI (log field `deployment_id`) | field omitted when unset | None | never fails |
| `RAILWAY_REPLICA_ID` | server, `user` CLI (log field `replica_id`) | field omitted when unset | None | never fails |
| `SENTRY_DSN` | server, `user` CLI, `sentry-check` | unset: Sentry off, no Sentry code runs | Optional; seal it in Railway. Parsed by sentry-go when set ([Observability](observability.md#sentry)) | `sentry_configuration` |

Notes on the table:

- The `user` CLI needs only `DATABASE_URL`. It runs before the Stripe variables are read, so those may be absent ([Startup and shutdown](startup-shutdown.md) shows where the branch splits). The `RAILWAY_*` variables and `SENTRY_DSN` are read for every command except help paths and an unknown first argument, so they label CLI logs too, and a CLI error event reaches Sentry like a server one. `RAILWAY_ENVIRONMENT_NAME` and `RAILWAY_GIT_COMMIT_SHA` are also the Sentry environment and release.
- Stripe variables are validated in this order and the first failure is reported: `STRIPE_API_KEY` empty, `STRIPE_MODE`, key prefix, then `PUBLIC_BASE_URL`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`. The API key and `PUBLIC_BASE_URL` are not trimmed; the webhook secret and the two configuration IDs fail when blank after trimming whitespace.
- Sandbox versus live is also enforced against Stripe itself: every catalog price's `livemode` must equal the configured mode (stage `billing_catalog`), and each webhook event's `livemode` must too ([Webhooks](../billing/webhooks.md)).
- `PUBLIC_BASE_URL` builds Stripe return URLs and the customer-portal return URL ([Checkout](../billing/checkout.md)). It also sets `Secure` on the staff `management_session` and customer `enrollment_session` cookies exactly when its parsed scheme is `https` (so `HTTPS://` counts). `Config.Validate` allows a non-`https` origin only in sandbox mode for `http` on `localhost`, `127.0.0.1` or `::1`, so only such an origin, wherever the app runs, sends session cookies without `Secure`.
- `.env.example` documents the local sandbox set; it holds only the Compose database credentials and placeholder Stripe values.

### Webhook secret: local versus deployed

`STRIPE_WEBHOOK_SECRET` must be the signing secret of whatever delivers events to this process:

| Where | Source of the secret |
|---|---|
| Local development | The secret printed by `stripe listen`, which forwards events to `/webhooks/stripe` ([Local development](local-development.md)) |
| Deployed | The signing secret of the webhook endpoint registered in Stripe for `<PUBLIC_BASE_URL>/webhooks/stripe` [EXTERNAL] |

A mismatch is not a startup error: every delivery fails signature verification, is logged as `stripe_webhook_rejected` and answered 400 ([Webhooks](../billing/webhooks.md)).

## Startup failure stages

A startup failure logs `startup_failed` with a `stage` field, then the process exits 1 ([Observability](observability.md) lists the other fields).

| `stage` | Raised by | Typical cause |
|---|---|---|
| `sentry_configuration` | `sentrylog.New` in `main` | `SENTRY_DSN` set but not a valid DSN; checked before `run`, so no database is touched |
| `configuration` | `run` | `DATABASE_URL` empty; `INSTALLMENT_SPACING` not a positive duration or set with `STRIPE_MODE=live` |
| `database_open` | `db.Open` | DSN does not parse. No connection is attempted here: the pool connects on first use |
| `database_connect` | `Pool.Ping` | Server unreachable, rejected credentials, the shared 10 s budget spent, or a signal during startup |
| `database_baseline` | `db.Migrate` | The schema holds some but not all of the baseline tables; nothing was changed ([Data model](../data-model.md#migrations)) |
| `database_migrate` | `db.Migrate` | A migration failed and rolled back, or the shared 10 s budget was spent, waiting for the migration lock included |
| `billing_configuration` | `billing.LoadConfigFromEnv` | Any Stripe variable above; `INSTALLMENT_RETRY_DELAY` not a positive duration |
| `billing_catalog` | `billing.LoadCatalog` | Price missing, duplicated, inactive, wrong mode, currency, billing scheme, type or amount; Stripe unreachable within 15 s |
| `billing_initialize` | `billing.New` | Repeats the `Config.Validate` that `LoadConfigFromEnv` already passed, so it cannot fail after `billing_configuration` succeeds |
| `auth_initialize` | `auth.New` | Argon2 dummy-hash creation or `db.DB()`; the login page is templ, checked at compile time |
| `management_routes` | `management.RegisterRoutes` | Template parse error |
| `customer_routes` | `customer.RegisterRoutes` | Template parse error |
| `listen` | `net.Listen` | Port in use or invalid `PORT` |

Failures after `listen` succeeds are `server_failed` or `shutdown_failed`, not startup stages.

## Source

- `main.go`: `main`, `run`, `startupError`, `INSTALLMENT_SPACING`, `DATABASE_URL`, `PORT`, `SENTRY_DSN`, `RAILWAY_SERVICE_NAME`, `RAILWAY_ENVIRONMENT_NAME`, `RAILWAY_GIT_COMMIT_SHA`, `RAILWAY_DEPLOYMENT_ID`, `RAILWAY_REPLICA_ID`
- `internal/logging/sentrylog/sentrylog.go`: `New`
- `internal/billing/stripe.go`: `Config.Validate`, `Config.SecureOrigin`, `LoadConfigFromEnv`, `LoadCatalog`, `STRIPE_API_KEY`, `STRIPE_MODE`, `PUBLIC_BASE_URL`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`, `INSTALLMENT_RETRY_DELAY`
- `internal/billing/billing.go`: `New`
- `internal/db/migrate.go`: `Migrate`, `ErrPartialSchema`
- `internal/testdb/testdb.go`: `Open`, `TEST_DATABASE_URL`
- `.air.toml`: `env_files`
- `.env.example`: `STRIPE_MODE`, `PUBLIC_BASE_URL`, `INSTALLMENT_SPACING`, `INSTALLMENT_RETRY_DELAY`

# Configuration

Every environment variable the code reads, with its default, validation and the startup stage that fails when it is wrong. Startup order is in [Startup and shutdown](startup-shutdown.md); Stripe dashboard objects these variables point at are in [Stripe setup](stripe-setup.md); local and Railway values are in [Local development](local-development.md) and [Railway](railway.md).

The application never loads a `.env` file itself; locally, Air injects `.env` (`env_files` in `.air.toml`) and variables already set in the shell win. There are no price-ID variables: prices are found in Stripe by lookup key ([Stripe setup](stripe-setup.md)).

## Variables

| Variable | Used by | Default | Validation | Failure stage |
|---|---|---|---|---|
| `DATABASE_URL` | server, `user` CLI | none; required | Non-empty, then parsed by the pgx driver | `configuration` (empty), `database_open` (unparseable), `database_connect` (unreachable or 10 s spent), `database_migrate` |
| `SESSION_COOKIE_SECURE` | server only | `true` when unset | `strconv.ParseBool`: `1 t T TRUE true True 0 f F FALSE false False`. Set but empty fails | `configuration` |
| `PORT` | server only | unset: listen on `127.0.0.1:3000`. Set: listen on `:<PORT>` (all interfaces) | None; a bad value fails when binding | `listen` |
| `STRIPE_API_KEY` | server only | none; required | Non-empty; prefix `rk_test_` or `sk_test_` when `STRIPE_MODE=sandbox`, `rk_live_` or `sk_live_` when `live` | `billing_configuration` |
| `STRIPE_MODE` | server only | none; required | Exactly `sandbox` or `live` | `billing_configuration` |
| `PUBLIC_BASE_URL` | server only | none; required | Absolute URL with a host; no userinfo, query, fragment or path prefix (a lone `/` is allowed); `https` in live mode; in sandbox `https`, or `http` only for `localhost`, `127.0.0.1` or `::1` | `billing_configuration` |
| `STRIPE_WEBHOOK_SECRET` | server only | none; required | Not blank (whitespace-only fails) | `billing_configuration` |
| `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID` | server only | none; required | Not blank | `billing_configuration` |
| `STRIPE_PORTAL_CONFIGURATION_ID` | server only | none; required | Not blank | `billing_configuration` |
| `TEST_DATABASE_URL` | integration tests only (`-tags=integration`) | none; the test fails | Must parse as a URL with a host and scheme `postgres` or `postgresql`; use a dedicated database | none (test failure) |
| `RAILWAY_SERVICE_NAME` | server, `user` CLI (log field `service`) | `ProjectHermes` | None | never fails |
| `RAILWAY_ENVIRONMENT_NAME` | server, `user` CLI (log field `environment`) | `local` | None | never fails |
| `RAILWAY_GIT_COMMIT_SHA` | server, `user` CLI (log field `version`) | `unknown` | None | never fails |
| `RAILWAY_DEPLOYMENT_ID` | server, `user` CLI (log field `deployment_id`) | field omitted when unset | None | never fails |
| `RAILWAY_REPLICA_ID` | server, `user` CLI (log field `replica_id`) | field omitted when unset | None | never fails |

Notes on the table:

- The `user` CLI needs only `DATABASE_URL`. It runs before the cookie and Stripe variables are read, so those may be absent ([Startup and shutdown](startup-shutdown.md) shows where the branch splits). The `RAILWAY_*` variables are read for every command except help paths and an unknown first argument, so they label CLI logs too.
- Stripe variables are validated in this order and the first failure is reported: `STRIPE_API_KEY` empty, `STRIPE_MODE`, key prefix, then `PUBLIC_BASE_URL`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`. The API key and `PUBLIC_BASE_URL` are not trimmed; the webhook secret and the two configuration IDs fail when blank after trimming whitespace.
- Sandbox versus live is also enforced against Stripe itself: every catalog price's `livemode` must equal the configured mode (stage `billing_catalog`), and each webhook event's `livemode` must too ([Webhooks](../billing/webhooks.md)).
- `PUBLIC_BASE_URL` builds Stripe return URLs and the customer-portal return URL ([Checkout](../billing/checkout.md)).
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
| `configuration` | `run` | `DATABASE_URL` empty; `SESSION_COOKIE_SECURE` not a boolean |
| `database_open` | `gorm.Open` | DSN does not parse. No connection is attempted here: `DisableAutomaticPing` is set |
| `database_pool` | `db.DB()` | GORM holds no `*sql.DB`; not reachable with the PostgreSQL driver in use [INFERENCE] |
| `database_connect` | `PingContext` | Server unreachable, rejected credentials, the shared 10 s budget spent, or a signal during startup |
| `database_migrate` | `AutoMigrate(models()...)` | DDL failure or the shared 10 s budget spent |
| `billing_configuration` | `billing.LoadConfigFromEnv` | Any Stripe variable above |
| `billing_catalog` | `billing.LoadCatalog` | Price missing, duplicated, inactive, wrong mode, currency, billing scheme, type or amount; Stripe unreachable within 15 s |
| `billing_initialize` | `billing.New` | Repeats the `Config.Validate` that `LoadConfigFromEnv` already passed, so it cannot fail after `billing_configuration` succeeds |
| `auth_initialize` | `auth.New` | Login template parse, Argon2 dummy-hash creation, or `db.DB()` |
| `management_routes` | `management.RegisterRoutes` | Template parse error |
| `customer_routes` | `customer.RegisterRoutes` | Template parse error |
| `listen` | `net.Listen` | Port in use or invalid `PORT` |

Failures after `listen` succeeds are `server_failed` or `shutdown_failed`, not startup stages.

## Source

- `main.go`: `run`, `startupError`, `SESSION_COOKIE_SECURE`, `DATABASE_URL`, `PORT`, `RAILWAY_SERVICE_NAME`, `RAILWAY_ENVIRONMENT_NAME`, `RAILWAY_GIT_COMMIT_SHA`, `RAILWAY_DEPLOYMENT_ID`, `RAILWAY_REPLICA_ID`
- `internal/billing/stripe.go`: `Config.Validate`, `LoadConfigFromEnv`, `LoadCatalog`, `STRIPE_API_KEY`, `STRIPE_MODE`, `PUBLIC_BASE_URL`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`
- `internal/billing/billing.go`: `New`
- `internal/testdb/testdb.go`: `Open`, `TEST_DATABASE_URL`
- `.air.toml`: `env_files`
- `.env.example`: `STRIPE_MODE`, `PUBLIC_BASE_URL`

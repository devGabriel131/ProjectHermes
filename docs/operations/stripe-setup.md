# Stripe Setup

What must exist in the Stripe account before the app can start, and which parts the code enforces. Variable validation and startup failure stages are in [Configuration](configuration.md); the Checkout, invoice and refund behavior that depends on these settings is in [Checkout](../billing/checkout.md), [Webhooks](../billing/webhooks.md), [Invoices](../billing/invoices.md) and [Withdrawal](../billing/withdrawal.md); open questions before live mode are in [Go-live](../go-live.md).

Do the setup once per mode. Sandbox and live each need their own key, prices, payment-method configuration, portal configuration and webhook endpoint; the code compares every price's `livemode` and every event's `livemode` with `STRIPE_MODE`. Settings that exist only in the Stripe dashboard, and so cannot be proven from the repository, are marked `[EXTERNAL]`.

## Checklist

| # | Setting | What to set | Code enforces? |
|---|---|---|---|
| 1 | API key and mode | `STRIPE_API_KEY` for the mode (`rk_test_`/`sk_test_` for sandbox, `rk_live_`/`sk_live_` for live) and `STRIPE_MODE` exactly `sandbox` or `live` | Partly. The prefix must match the mode, at startup. Restricted keys are advice only: an `sk_` key is accepted |
| 2 | Public origin | `PUBLIC_BASE_URL`: origin only, `https` (plain `http` only for `localhost`, `127.0.0.1` or `::1` in sandbox). Builds the Checkout return URLs and the portal return URL | Yes, at startup |
| 3 | Prices | Six one-time USD prices found by lookup key ([table](#price-lookup-keys)) | Yes, at startup |
| 4 | Webhook endpoint | `<PUBLIC_BASE_URL>/webhooks/stripe`, created with API version `2026-09-30.endive`, subscribed to the 23 types in [Webhooks](../billing/webhooks.md) `[EXTERNAL]` | Partly. Signature, 300 s tolerance, mode and API release train are checked per event (400 on failure). The endpoint's existence and its subscription list are not checked |
| 5 | Webhook secret | `STRIPE_WEBHOOK_SECRET`: the registered endpoint's signing secret when deployed, the secret printed by `stripe listen` locally `[EXTERNAL]` | Presence at startup; correctness shows up as 400s on every delivery |
| 6 | Payment-method configuration | `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`; contents: Card, Apple Pay, Google Pay, Link and Klarna on, everything else off `[EXTERNAL]` | ID must be non-empty. It is sent only for the single payment (`full`); its contents are never read |
| 7 | Customer portal configuration | `STRIPE_PORTAL_CONFIGURATION_ID`; contents: invoice history and payment-method update only `[EXTERNAL]` | ID must be non-empty; contents are never read |
| 8 | Adaptive Pricing | Off, including on Hosted Invoice Pages `[EXTERNAL]` | Checkout Sessions send `adaptive_pricing.enabled=false`. Hosted Invoice Pages are dashboard-only |
| 9 | Automatic tax | Off `[EXTERNAL]` | Not set by code. An invoice with tax, a discount, a customer balance debit, another currency or an unexpected line is held for staff review (`invoice_mismatch`) |
| 10 | Smart Retries | On for one-off invoices with a 2-week window, matching the 14-day suspension rule `[EXTERNAL]` | No |
| 11 | Customer emails | See [below](#dashboard-only-settings) `[EXTERNAL]` | No |
| 12 | Locale | Stripe pages, emails and PDFs in `es-419` | Yes: `stripeLocale` is sent on the Customer, every Checkout Session and every portal session |

Startup fails with `startup_failed` at stage `billing_configuration` (rows 1, 2, 5, 6, 7), `billing_catalog` (row 3) or `billing_initialize`; the `user` CLI and `--help` need none of this.

### Price lookup keys

Every price must be one-time, `per_unit`, USD, active, and in the configured mode, with the exact amount from `internal/payments/plans.go`. `LoadCatalog` fetches all six with one batched `lookup_keys` request (15 s budget in `main.go`) and stops startup on a missing, duplicate, inactive, wrong-mode, wrong-currency, wrong-scheme, wrong-type or wrong-amount price. There is no fallback price.

| Lookup key | Used for | Cents | Amount |
|---|---|---|---|
| `asvab_full_initial_usd` | `full`, its only payment | 60000 | $600.00 |
| `asvab_three_initial_usd` | `three`, first payment | 20834 | $208.34 |
| `asvab_three_remaining_usd` | `three`, payments 2 and 3 | 20833 | $208.33 |
| `asvab_six_initial_usd` | `six`, first payment | 10835 | $108.35 |
| `asvab_six_remaining_usd` | `six`, payments 2 to 6 | 10833 | $108.33 |
| `asvab_reinstatement_usd` | Reinstatement fee (`reinstatementFeeCents`) | 2500 | $25.00 |

All later installments of a plan share one `remaining` price. The catalog also refuses a plan whose installments share a key but differ in amount, so editing one amount in `plans.go` needs a new price. Plan amounts are in [Billing overview](../billing/overview.md).

### API surface of the key

Startup exercises only the price list; every other call first runs when a customer or staff action needs it, so a key missing a permission fails then. Stripe's permission names for these resources are `[EXTERNAL]`.

| Resource | Calls | Used for |
|---|---|---|
| Prices | list | Startup catalog |
| Customers | create, retrieve, update | Customer at the first Checkout start; name and email sync; default payment method for later invoices |
| Checkout Sessions | create, retrieve, list, expire | [Checkout](../billing/checkout.md) |
| Invoices | create, retrieve, list, update, finalize, send, void, delete | Later installments and the reinstatement fee; cancelling a corrected fee or a withdrawn contract's invoices |
| Invoice items | create, list | Attaching the quoted price to a draft invoice |
| Invoice payments | list | Proving the first payment before funding |
| PaymentIntents, PaymentMethods, Charges | retrieve | Payment rows and the card check before arming |
| Refunds | create, retrieve | Withdrawal refunds on the PaymentIntent ([Withdrawal](../billing/withdrawal.md#refunds)); finding a refund's payment |
| Disputes, Radar early fraud warnings | retrieve, list | Payment-review state |
| Billing portal sessions | create | Staff portal link |

A restricted (`rk_`) key therefore needs write access to Refunds (Stripe's permission name is `[EXTERNAL]`). Without it every withdrawal refund fails: the staff request answers 500, and every later sync of the withdrawn contract fails and retries a minute later.

## Payment methods

- **Single payment (`full`)** uses the payment-method configuration, so Card, Apple Pay, Google Pay, Link and Klarna are available as the dashboard allows.
- **Installment plans (`three`, `six`)** send `allowed_payment_method_types=["card"]` and no configuration. Apple Pay, Google Pay and Link ride on card, and Klarna is absent. The first payment must leave a saved card; otherwise the contract is flagged `unsupported_payment_method` and no later invoice is armed ([Invoices](../billing/invoices.md)).
- **Every invoice the app creates** is limited to `card`, so Klarna never appears on later installments or the reinstatement fee.

## Customer portal

`PortalURL` creates a short-lived portal session for the contract's own Stripe Customer with `configuration` set to `STRIPE_PORTAL_CONFIGURATION_ID`, `locale` `es-419` and a `return_url` of `<PUBLIC_BASE_URL>/management/students/{id}`. It needs a contract that already has a Customer, never marks anything paid and never reactivates a student. The link is a payer credential, created on demand for staff and never stored. The portal configuration should offer invoice history and payment-method update only `[EXTERNAL]`; the app cannot check this. The staff button is described in [Students](../management/students.md).

## Dashboard payment links

The staff billing block links every payment to the Stripe Dashboard ("Open in Stripe", [Staff students](../management/students.md#billing-block)), where staff make refunds the app does not. `dashboardURL` builds the link from the PaymentIntent ID and the mode (`Config.Live`, from `STRIPE_MODE`):

| Mode | Link |
|---|---|
| `live` | `https://dashboard.stripe.com/payments/<PaymentIntent ID>` |
| `sandbox` | `https://dashboard.stripe.com/test/payments/<PaymentIntent ID>` |

The link carries no credential: staff must be signed in to that Stripe account `[EXTERNAL]`.

## Dashboard-only settings

All of these are `[EXTERNAL]`; the app neither reads nor verifies them.

| Setting | Where in the Stripe dashboard |
|---|---|
| Smart Retries on, 2-week window, for one-off invoices | Settings > Billing > Invoices > Advanced invoicing features |
| Receipt email for successful payments | Settings > Emails > "Successful payments" |
| Email when a card payment fails | Settings > Billing > Subscriptions and emails (Automatic collection) > "Send emails when card payments fail" |
| Hosted link for cardholders to authenticate (3D Secure) | Same page > "Send a Stripe-hosted link for cardholders to authenticate when required" |
| Adaptive Pricing off for Hosted Invoice Pages; automatic tax off | Location not recorded in the repository |

The `stripeLocale` comment records how Stripe renders amounts `[EXTERNAL]`: `es` prints Spain-style `600,00 US$`, `es-419` prints `USD 600.00`, and only `en` prints a bare `$600.00`, in English. Stripe has no Puerto Rico locale, which is why `es-419` is used.

If webhook delivery is missing or the endpoint is not subscribed to a type, the worker still catches up: attempts in flight and overdue invoices are re-synced every 15 minutes, and every contract at least every 24 hours ([Worker](../billing/worker.md)).

## Deliberate choices

- **The catalog is checked at startup with no fallback price.** A wrong amount, mode or currency stops the deploy instead of charging a customer the wrong quote.
- **One batched lookup.** Stripe caps `lookup_keys` at 10 and the app needs six (`ponytail:` comment in `LoadCatalog`); a catalog beyond 10 keys would need more than one request.
- **Shared-key amount check.** It guards the assumption in `priceKey` that every later installment of a plan has one price.

## Source

- `internal/billing/stripe.go`: `Config`, `Live`, `Validate`, `LoadConfigFromEnv`, `LoadCatalog`, `priceKey`, `reinstatementFeeCents`, `reinstatementKey`, `stripeLocale`, `NewStripeClient`
- `internal/billing/checkout.go`: `sessionParams`, `AdaptivePricing`, `AllowedPaymentMethodTypes`, `PaymentMethodConfiguration`
- `internal/billing/billing.go`: `PortalURL`, `New`
- `internal/billing/webhook.go`: `webhookEvents`, `Webhook`
- `internal/billing/invoices.go`: `createInvoice`, `invoiceMatches`, `ensureDefaultPaymentMethod`
- `internal/billing/withdraw.go`: `refundPayments`
- `internal/billing/summary.go`: `dashboardURL`
- `internal/billing/worker.go`: `nextSyncAt`
- `internal/payments/plans.go`: `Plans`
- `main.go`: `LoadCatalog`, `LoadConfigFromEnv`, `billing_catalog`, `billing_configuration`
- `.env.example`: `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_MODE`, `PUBLIC_BASE_URL`, `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`

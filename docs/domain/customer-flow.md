# Customer flow

This page covers what the customer's browser can do at `/customer/{token}` and `/payments/return`: routes, check precedence, contact edit, plan choice, the return page, headers and limits. The state behind it is in [Links](links.md), [Students](students.md) and [Agreement](agreement.md); the Stripe side is in [Checkout](../billing/checkout.md); script and style behavior is in [Frontend](../frontend.md).

The `customer` package holds handlers only. Every write goes through `links.Store` or `billing.Service`. Customer pages are Spanish; all customer text below is quoted as in code.

## Routes

The link token in the path and the path-scoped `enrollment_session` cookie identify the browser's enrollment. There is no sign-in.

| Method | Path | Behavior |
|---|---|---|
| GET | `/customer/{token}` | Read-only welcome page (`welcome`). Creates no rows. Resumes `Awaiting payment` to `/agreement`, shows "Tu matrícula ya fue completada." (+ "Comenzar nueva matrícula" if the link is still usable) for `Finished`, "Continuar" to `/forms` for `In progress`, or "Comenzar" (`POST /start`) for new visitors. |
| POST | `/customer/{token}/start` | Calls `StartEnrollment`, sets the `enrollment_session` cookie (`HttpOnly`, `SameSite=Lax`, `Path: "/customer/{token}"`) if a new enrollment started, and redirects 303 (or `HX-Redirect`) to `/forms` (or `/agreement` if already submitted). |
| GET | `/customer/{token}/forms` | Resolves session (`resolveSession`). Missing/expired session on a still-usable link redirects 303 to `/customer/{token}`. Otherwise renders the contact form with drafts prefilled and autosave wired. |
| POST | `/customer/{token}` | Final submit: six contact fields, `email_confirm` and PIN. Success: 303 to `/agreement`. |
| POST | `/customer/{token}/draft/{field}` | Autosave one registry field. Answers only a status fragment ("Guardado.") and never redraws the form. |
| POST | `/customer/{token}/pin` | Advisory PIN check, saves nothing ("PIN disponible."). A POST keeps the PIN out of URLs and access logs. Each answer, and each valid final submit, spends one of `links.MaxPinChecks` (30) per enrollment; past that the answer is 429. |
| GET, POST | `/customer/{token}/edit` | Reopens the contact form of a submitted enrollment before payment. |
| GET, POST | `/customer/{token}/agreement` | Terms and acceptance ([Agreement](agreement.md)). |
| GET, POST | `/customer/{token}/payments` | Plan choice and Checkout start; after funding, the persisted plan and schedule. |
| GET | `/payments/return` | Token-free outcome of a hosted Checkout. |

Every GET route also answers `HEAD`: same checks, no body, and never writes a row. On an `Awaiting payment` session `HEAD /customer/{token}` returns 200 where GET redirects. Route registration and the surrounding middleware are in [Architecture](../architecture.md).
Step order is enforced by redirect, not by state flags:

| Page | Not submitted | Terms not accepted |
|---|---|---|
| `/agreement` | 303 to `/forms` | page shows the form |
| `/payments` | 303 to `/forms` | 303 to `/agreement` |
| `/edit` | 303 to `/forms` | allowed |

## Request sequence

```mermaid
sequenceDiagram
    actor Browser
    participant H as customer handlers
    participant L as links.Store
    participant B as billing.Service
    participant DB as PostgreSQL
    participant S as Stripe
    Browser->>H: GET /customer/{token}
    H->>L: LookupSession
    H-->>Browser: welcome page (read-only)
    Browser->>H: POST /customer/{token}/start
    H->>L: StartEnrollment
    L->>DB: insert enrollment under MaxLinkEnrollments
    H-->>Browser: Set-Cookie enrollment_session + 303 to /forms
    Browser->>H: POST /customer/{token}/draft/{field} on change
    H->>L: SaveDraft
    L->>DB: guarded UPDATE of one column
    H-->>Browser: Guardado. or 422 or 409
    Browser->>H: POST /customer/{token}/pin
    H->>DB: pinHeld on students and enrollments
    Browser->>H: POST /customer/{token}
    H->>L: Submit
    L->>DB: guarded UPDATE enrollments
    H-->>Browser: 303 to /agreement
    Browser->>H: POST /customer/{token}/agreement
    H->>L: AcceptTerms
    H-->>Browser: 303 to /payments
    Browser->>H: GET /customer/{token}/payments
    H->>B: ForEnrollment
    H-->>Browser: three plans with schedule from Plan.Schedule(now)
    Browser->>H: POST /customer/{token}/payments
    H->>B: StartCheckout
    B->>S: Customer and Checkout Session
    H-->>Browser: 303 to checkout.stripe.com
    Browser->>S: pays
    S-->>Browser: redirect to /payments/return?session_id
    Browser->>H: GET /payments/return (polls)
    H->>B: ReturnStatus
```

Funding happens afterwards in the worker, not in any customer request ([Lifecycle](lifecycle.md)).

## Check precedence

Submit, draft and PIN answer the same questions in the same order:

```mermaid
flowchart TD
    A["Request"] --> U{"Session usable?"}
    U -->|"no"| N["404 branded page, HX-Refresh for HTMX (GET/HEAD redirects to welcome if link usable)"]
    U -->|"yes"| S{"Already submitted?"}
    S -->|"submit"| ADV["303 or HX-Redirect to /agreement"]
    S -->|"draft or pin"| LK["409, HX-Refresh for HTMX"]
    S -->|"no"| B{"Body parses within 4 KiB?"}
    B -->|"no"| E1["413 too large or 400 unreadable"]
    B -->|"yes"| P{"Field count and values valid?"}
    P -->|"no"| E2["400 wrong field count or 422 invalid value"]
    P -->|"yes"| W["write result"]
    W -->|"Saved"| OK["200, or 303 for submit"]
    W -->|"PinTaken"| E3["422 on pin"]
    W -->|"error"| E5["500 with a retry message"]
```

Implementation notes that keep this order observable:

- An unknown `{field}` in `/draft/{field}` is refused with 404 before the body is read or the database is touched.
- With valid input, `Submit` and `SaveDraft` run the guarded UPDATE first. On zero rows, `saveResult` re-reads the enrollment to separate unusable (404) from submitted (advance or 409). With rejected input, the handler looks the session up itself (`resolveSession`) and applies the same order.
- `/pin` looks the session up first, then reads the body, then `ParsePin` (422), then `pinHeld` (422 "Este PIN ya está en uso. Elige otro.").
- A 422 on submit redraws the attempted values; any other rejection shows what is stored.
- Draft outcomes: `Saved` 200; submitted 409; unusable 404. A submitted enrollment that gets a draft or PIN POST is a stale page, so HTMX reloads it.

## Contact edit

`/edit` rewrites the Link's contact columns through `billing.UpdateContact`, because no Student exists before payment ([Students](students.md)). After funding, edits are refused and the Student becomes canonical.

| Step | Behavior |
|---|---|
| Link unusable | 404 |
| Link not submitted | Redirect to `/forms` |
| `ContactEditable` false | Redirect to `/payments`, for `GET` and `POST` |
| GET | Form without autosave and without PIN |
| POST unreadable or invalid | 413, 400 or 422, the same parsers as submit |
| POST unchanged | Redirect to `/agreement`; no write, no refresh, no provider sync |
| POST changed | `UpdateContact`, then redirect to `/agreement`. Submission time, expiry and accepted terms never change. |

`ContactEditable` is a lock-free pre-check, also used to show or hide "Editar información del estudiante" on the agreement. The authority is `UpdateContact`, which locks Link, then Contract, then the in-flight attempt, and returns `ErrContactLocked` (redirect to `/payments`) when the contract is funded or an attempt is `processing`, `uncertain` or `expiring`. Attempts in `creating` or `open` still allow edits. An unusable link returns `ErrUnavailable` (404). A change marks the contract due for sync, which has effect only once a Stripe customer exists, and publishes a live event.

## Payments page

`GET` calls `ForEnrollment` and, until funded, lists the three plans with `Plan.Schedule(now)` (dates anchored to today in Puerto Rico; real dates are fixed at funding, see [Billing overview](../billing/overview.md)). A funded contract shows its persisted plan, paid amount and installments instead of a way to pay again.

`POST /payments` outcomes:

| Condition | Response |
|---|---|
| Body unreadable or oversized | 413 or 400, "No pudimos leer tu selección." |
| `payment_plan` repeated | 400 "Selecciona una sola opción de pago." |
| missing or unknown plan | 422 "Selecciona una de las tres opciones de pago." |
| `StartCheckout` returns a URL | 303 (or `HX-Redirect`) to `checkout.stripe.com` |
| already funded with the same plan | redirect to `/payments` |
| funded with a different plan (`ErrPlanLocked`) | 409 "Tu plan de pago ya está confirmado y no se puede cambiar." |
| attempt in flight (`CheckoutResult{Pending: true}`: `creating`, `processing`, `expiring`, `uncertain`, or open without URL) | 202 with the "Estamos preparando tu pago seguro" notice |
| `ErrUnavailable` | 404 |
| other error | 500 "No pudimos iniciar el pago seguro. Intenta de nuevo." |

Before funding the customer may switch plans; [Checkout](../billing/checkout.md) owns the rules.

## Payment return

`GET /payments/return` needs no token. It reads one persisted attempt through `ReturnStatus` and exposes only status and amount, never identity, contact or payment history. Visiting it never changes financial state.

| Input | Result |
|---|---|
| `?session_id=` known | `paid`, `pending` or `not_completed` ([Checkout](../billing/checkout.md) maps attempt states) |
| `?session_id=` unknown | 404; HTMX reloads the page |
| `?cancelled=1` | `not_completed` without a lookup |
| neither | 404 |

While `pending`, the status block polls itself with `hx-trigger="every 1.5s"`, carrying `polls`. The counter is clamped to 0 to 80 (`maxReturnPolls`), about two minutes; at 80 the block stops and asks the customer to refresh. A failed poll swaps in a retry block that keeps polling until the cap. Without JavaScript a `<noscript>` meta refresh reloads every 3 seconds with the same counter.

## Headers and CSP

`withHeaders` sets these before the handler runs, so 404, 422 and 500 carry them too: `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `Vary: HX-Request` and `Content-Type: text/html; charset=utf-8`. The Content-Security-Policy, HSTS and `nosniff` come from the server-wide `secureHeaders` middleware ([Frontend](../frontend.md)): scripts only from `'self'` and the pinned htmx file, no inline script or style, `frame-ancestors 'none'`. A `htmx-config` meta tag sets `includeIndicatorStyles` to false, so htmx injects no inline indicator style. `form-action` allows the hosted Checkout redirect and nothing else from Stripe: no Stripe.js, no framing.

Redirects carry no contact data: targets are paths only, plus Stripe's URL. An HTMX `POST` gets 200 with `HX-Redirect` instead of 303.

## Limits and timeouts

| Limit | Value |
|---|---|
| Request body | 4 KiB (`http.MaxBytesReader`) on every customer POST; larger is 413, unparsable is 400 |
| Database work per handler | 5 s (`dbTimeout`) |
| `StartCheckout` | its own 8 s `providerBudget`, so Stripe calls never inherit the 5 s bound. The summary that follows gets a fresh 5 s. |
| Server `WriteTimeout` | 10 s ([Startup and shutdown](../operations/startup-shutdown.md)) |

## Bearer token and messaging

Anyone holding a link can read the customer's submitted details, edit them while editable, accept terms, choose a plan and start Checkout, for as long as the link is usable (initial lifetime, then 168 hours from submit). The app sends no email or SMS: the customer reaches the pages only through the URL staff pass on. Anything Stripe sends is configured in Stripe `[EXTERNAL]`.

## Deliberate choices

- `withHeaders` wraps every route, not selected ones, so headers apply to every outcome.
- `dbTimeout` and `providerBudget` stay separate: provider work must not be cut at 5 s, and DB work must not wait 8 s.
- `LookupSession` versus `StartEnrollment`: `GET` and `HEAD` never create rows, so link previews cannot spend the 100-enrollment cap.
- One enrollment per browser per link: the `enrollment_session` cookie is scoped to `/customer/{token}` and holds one session. While that enrollment is `In progress` or `Awaiting payment`, `POST /start` resumes it and the welcome page offers no second start. Other browsers on the same link start their own enrollments, up to `MaxLinkEnrollments`. Several enrollments in one browser would need one cookie per enrollment and a chooser on the welcome page.
- The `ContactEditable` pre-check is lock-free for speed; `UpdateContact` rechecks under locks, so the pre-check can be stale without harm.
- The PIN check is a POST so the PIN never reaches URLs or access logs.

## Source

- `internal/customer/customer.go`: `RegisterRoutes`, `withHeaders`, `dbTimeout`, `site.render`, `site.resolveSession`, `redirectCustomer`, `site.welcome`, `site.start`, `site.forms`, `site.submit`, `site.draft`, `site.edit`, `parseCustomerForm`, `readSubmission`, `site.checkPin`, `pinTakenMessage`, `readSessionCookie`, `setSessionCookie`, `sessionCookieName`
- `internal/customer/payments.go`: `site.payments`, `site.paymentReturn`, `maxReturnPolls`
- `internal/customer/agreement.go`: `agreement`
- `internal/customer/templates/index.html`: `htmx-config`, `includeIndicatorStyles`, `customer-content`
- `internal/customer/templates/payment-return.html`: `payment-return-status`, `noscript`
- `internal/links/links.go`: `Store.LookupSession`, `Store.StartEnrollment`, `Store.SaveDraft`, `Store.Submit`, `saveResult`
- `internal/billing/contact.go`: `UpdateContact`, `ContactEditable`, `inFlightAttemptStates`, `ErrContactLocked`
- `internal/billing/checkout.go`: `StartCheckout`
- `internal/billing/summary.go`: `ForEnrollment`, `ReturnStatus`
- `internal/billing/billing.go`: `providerBudget`, `ErrPlanLocked`, `ErrUnavailable`

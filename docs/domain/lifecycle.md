# Lifecycle

This page defines the vocabulary every other page uses and tells the whole story once: from staff issuing a link to the 14-day payment sweep. Each stage links to the page that owns its detail; the index of all pages is [docs/README](../README.md).

The central fact: a Student does not exist until the first payment is confirmed. Staff issue a reusable Link, each visitor starts an Enrollment tied to a session cookie, and submitting writes only that Enrollment. The Student is inserted by `links.Enroll` inside billing's funding transaction.

## Vocabulary

| Term | Meaning |
|---|---|
| **submitted** | `enrollments.submitted_at` is set by `links.Store.Submit`. No Student and no money exist yet. |
| **paid / funded** | `enrollments.paid_at` and `billing_contracts.funded_at` are set, to the same instant: the paid time of the first Stripe invoice. Set once, in one transaction. |
| **enrolled** | A `students` row exists. For an enrollment it is created only at funding, by `links.Enroll`; staff can also add students directly (`students.Store.Add`), with no enrollment, contract or cohort. |
| **Finished** | The Enrollment status once it is paid. |
| **withdrawn** | `students.withdrawn_at` and `billing_contracts.withdrawn_at` are set by a staff withdrawal: every undisputed payment refunded, every unpaid installment cancelled, the Student inactive for good. Irreversible; the person re-enrolls through a new link. [Withdrawal](../billing/withdrawal.md) |
| Link | One reusable customer URL in `links`: name, token, lifetime, audit. [Links](links.md) |
| Enrollment | One customer's session in `enrollments`: session digest, drafts, PIN, agreement snapshot. [Links](links.md) |
| Student | The enrolled person: canonical contact details, PIN, group, cohort, `is_active`, `withdrawn_at`. [Students](students.md) |
| Contract | `billing_contracts`: the financial agreement for one Enrollment (1:1 by `enrollment_id`). Created at the first Checkout start, before any Student. [Billing overview](../billing/overview.md) |
| Obligation | `billing_obligations`: one tuition installment, or one reinstatement fee. [Invoices](../billing/invoices.md) |
| Attempt | `billing_checkout_attempts`: one hosted Checkout try for the first payment. [Checkout](../billing/checkout.md) |
| Suspension / episode | `billing_suspensions`: one continuous period of missed-deadline suspension, with one fee. [Suspension](../billing/suspension.md) |
| review reason | A short code on a contract or obligation meaning staff must look at the money state before automation continues. [Reviews](../billing/reviews.md) |
| cohort | `students.cohort`, `YYYY-MM`: the month of the start date the customer accepted. Set at enrollment, never edited. |
| group | `students.group`, `Básico` or `Regular`: the program stage. Starts at `Básico`; staff change it. |

## End to end

```mermaid
sequenceDiagram
    actor Staff
    actor Customer
    participant App
    participant PostgreSQL
    participant Stripe
    participant Worker
    Staff->>App: POST /management/links
    App->>PostgreSQL: Store.CreateLink stores token and expires_at
    App-->>Staff: reusable URL /customer/{token}
    Staff-->>Customer: shares the URL outside the app
    Customer->>App: GET /customer/{token}
    App-->>Customer: welcome page (read-only)
    Customer->>App: POST /customer/{token}/start
    App->>PostgreSQL: Store.StartEnrollment creates enrollment + session digest
    App-->>Customer: Set-Cookie enrollment_session, 303 to /forms
    Customer->>App: POST /customer/{token}/draft/{field}
    App->>PostgreSQL: Store.SaveDraft
    Customer->>App: POST /customer/{token}
    App->>PostgreSQL: Store.Submit sets submitted_at, pin, expires_at = now() + 168 hours
    App->>PostgreSQL: Store.AcceptTerms snapshots the start date
    Customer->>App: POST /customer/{token}/payments
    App->>PostgreSQL: StartCheckout writes contract, obligations, attempt
    App->>Stripe: create Customer and Checkout Session
    App-->>Customer: 303 to checkout.stripe.com
    Customer->>Stripe: pays the first installment
    Stripe->>App: POST /webhooks/stripe
    App->>PostgreSQL: store event id and type, wake the worker
    Worker->>PostgreSQL: claim event, lease the contract
    Worker->>Stripe: read Session, Invoice, PaymentIntent
    Note over Worker,PostgreSQL: fund runs one transaction
    Worker->>PostgreSQL: links.Enroll inserts the Student, sets paid_at, clears enrollments.pin
    Worker->>PostgreSQL: funded_at, due dates, payment row
    Worker->>Stripe: scheduleInvoices creates later installment invoices
    loop every contract sync
        Worker->>PostgreSQL: evaluateDeadlines checks due_at plus 14 days
        Worker->>PostgreSQL: unpaid past deadline opens an episode, is_active = false
    end
```

| Stage | Detail page |
|---|---|
| Issue link, token, expiry | [Links](links.md) |
| Open, drafts, submit, edit | [Customer flow](customer-flow.md) |
| Field rules, PIN | [Students](students.md) |
| Agreement, start date | [Agreement](agreement.md) |
| Plan choice, Checkout | [Checkout](../billing/checkout.md) |
| Webhook intake | [Webhooks](../billing/webhooks.md) |
| Worker, funding, retries | [Worker](../billing/worker.md) |
| Later installments | [Invoices](../billing/invoices.md) |
| 14-day sweep, reinstatement | [Suspension](../billing/suspension.md) |
| Withdrawal and full refund | [Withdrawal](../billing/withdrawal.md) |

Only the worker (event handling or contract sync) first-funds a contract. Staff Reconcile, Reinstatement and Withdrawal run the same sync, but they find the contract through the `students` row, which for an enrollment exists only after funding (a staff-added student has no contract), so in production they re-sync an already funded contract ([Billing overview](../billing/overview.md) lists who can reach `fund`). Stripe collects the invoices; the app charges a card itself only to retry a failed later installment ([Invoices](../billing/invoices.md#retrying-a-failed-installment)), and refunds only when staff withdraw the student ([Withdrawal](../billing/withdrawal.md)).

## Who owns which state

| State | Where it lives | Written by |
|---|---|---|
| Link and enrollment rows, status, drafts, PIN reservation, agreement snapshot | `links`, `enrollments` | `internal/links` ([Links](links.md)) |
| Enrollment contact snapshot edit before payment | `enrollments` contact columns | `billing.UpdateContact`, which locks the Enrollment, then the Contract |
| `paid_at`, Student row | `enrollments.paid_at`, `students` | `links.Enroll`, called by `billing.Service.fund` |
| Student contact, PIN, group | `students` | `students.Store.UpdateField` (staff) |
| `students.is_active` | `students` | `internal/billing` suspension and withdrawal code |
| `withdrawn_at` and `withdrawn_by` | `students`, `billing_contracts` | `internal/billing` withdrawal (`withdraw`) |
| Plan quote, contract, obligations | `billing_contracts`, `billing_obligations` | `internal/billing` |
| Checkout attempt | `billing_checkout_attempts` | `internal/billing` |
| Received payments | `billing_payments` | `internal/billing` worker |
| Episodes and fees | `billing_suspensions`, fee obligations | `internal/billing` suspension code |
| Review reasons | `review_reason` on contracts and obligations | `internal/billing` |
| Stripe events | `billing_webhook_events` | `internal/billing` webhook intake |
| Start date, link lifetime | `settings` | `internal/settings` ([Agreement](agreement.md)) |
| Staff accounts and sessions | `users`, `sessions` | `internal/auth` ([Staff auth](../staff-auth.md)) |
| Live refresh signal | in memory | `internal/live` ([Live updates](../live-updates.md)) |
| Plans, schedules, money format | code only | `internal/payments` |

Every table with its writer and database rules is in [Data model](../data-model.md).

## A person's journey

```mermaid
stateDiagram-v2
    state "In progress" as InProgress
    state "Awaiting payment" as AwaitingPayment
    state Finished {
        [*] --> Active
        Active --> Suspended : unpaid installment passes 14 days
        Suspended --> Active : reinstatement requested and fee and due installments paid
        Active --> Withdrawn : staff withdraw and refund
        Suspended --> Withdrawn : staff withdraw and refund
    }
    [*] --> InProgress : customer POST /start
    InProgress --> AwaitingPayment : submit
    InProgress --> Canceled : staff cancel enrollment or parent link
    InProgress --> Expired : expiry passes
    AwaitingPayment --> Expired : 168 hours unpaid
    AwaitingPayment --> Finished : funding creates the Student
    Expired --> Finished : late funding
```

Inside Finished the Enrollment status stays Finished while the Student alternates between active and suspended. Reactivation also needs staff to have requested reinstatement; an episode later proven on time is corrected automatically ([Suspension](../billing/suspension.md)). Withdrawn is final: nothing leaves it, and no route undoes it ([Withdrawal](../billing/withdrawal.md)).

| State | Page |
|---|---|
| In progress, Expired, Canceled | [Links](links.md) |
| Awaiting payment | [Customer flow](customer-flow.md), [Agreement](agreement.md) |
| Finished, Student active | [Students](students.md) |
| Student suspended | [Suspension](../billing/suspension.md) |
| Student withdrawn | [Withdrawal](../billing/withdrawal.md) |

## Reading the rest of the docs

| Area | Pages |
|---|---|
| System | [Architecture](../architecture.md), [Data model](../data-model.md) |
| Domain | [Links](links.md), [Customer flow](customer-flow.md), [Students](students.md), [Agreement](agreement.md) |
| Billing | [Overview](../billing/overview.md), [Checkout](../billing/checkout.md), [Webhooks](../billing/webhooks.md), [Worker](../billing/worker.md), [Invoices](../billing/invoices.md), [Suspension](../billing/suspension.md), [Withdrawal](../billing/withdrawal.md), [Reviews](../billing/reviews.md) |
| Staff | [Staff auth](../staff-auth.md), [Live updates](../live-updates.md), [Management overview](../management/overview.md), [Management students](../management/students.md), [Dashboard](../management/dashboard.md), [Frontend](../frontend.md) |
| Operations | [Configuration](../operations/configuration.md), [Startup and shutdown](../operations/startup-shutdown.md), [Observability](../operations/observability.md), [Stripe setup](../operations/stripe-setup.md), [Railway](../operations/railway.md), [Local development](../operations/local-development.md) |
| Project | [Testing](../testing.md), [Go-live](../go-live.md) |

## Source

- `internal/links/links.go`: `Enroll`, `Store.CreateLink`, `Store.StartEnrollment`, `Store.SaveDraft`, `Store.Submit`, `Store.AcceptTerms`, `LinkEntry`, `EnrollmentEntry`
- `internal/students/students.go`: `Student`, `Store.UpdateField`
- `internal/customer/customer.go`: `RegisterRoutes`
- `internal/billing/checkout.go`: `StartCheckout`
- `internal/billing/contact.go`: `UpdateContact`
- `internal/billing/webhook.go`: `Webhook`
- `internal/billing/checkout.go`: `fund`
- `internal/billing/worker.go`: `reconcileContract`
- `internal/billing/invoices.go`: `scheduleInvoices`
- `internal/billing/suspension.go`: `evaluateDeadlines`, `evaluateReactivation`, `deadlineWindow`
- `internal/billing/withdraw.go`: `Withdraw`, `withdraw`
- `internal/billing/models.go`: `Contract`, `Obligation`, `CheckoutAttempt`, `Suspension`, `ReviewReason`
- `internal/settings/settings.go`: `Settings`
- `internal/payments/plans.go`: `Plan.Schedule`

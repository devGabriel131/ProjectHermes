# Documentation index

This directory contains the architecture, domain, billing, management, and operations documentation for the tuition payment and enrollment system.

## Reading order for newcomers

1. [System lifecycle](domain/lifecycle.md) — The vocabulary, entity states, and complete customer-to-student journey.
2. [Architecture](architecture.md) — System context, mounted routes, middleware chain, and package dependencies.
3. [Data model](data-model.md) — The 11 PostgreSQL tables, keys, and database-enforced constraints.
4. **Domain pages**: [Links](domain/links.md) -> [Customer flow](domain/customer-flow.md) -> [Students](domain/students.md) -> [Agreement](domain/agreement.md).
5. **Billing pages**: [Billing overview](billing/overview.md) -> [Checkout](billing/checkout.md) -> [Webhooks](billing/webhooks.md) -> [Worker](billing/worker.md) -> [Invoices](billing/invoices.md) -> [Suspension](billing/suspension.md) -> [Reviews](billing/reviews.md).
6. **Management and UI**: [Management overview](management/overview.md) -> [Students management](management/students.md) -> [Dashboard](management/dashboard.md) -> [Frontend](frontend.md) -> [Live updates](live-updates.md) -> [Staff auth](staff-auth.md).
7. **Operations and Testing**: [Configuration](operations/configuration.md) -> [Startup & shutdown](operations/startup-shutdown.md) -> [Local development](operations/local-development.md) -> [Stripe setup](operations/stripe-setup.md) -> [Railway](operations/railway.md) -> [Observability](operations/observability.md) -> [Testing](testing.md) -> [Go-live](go-live.md).

## Page index

| Area | Page | Key question answered |
|---|---|---|
| Overview | [architecture.md](architecture.md) | How are packages, routes, and middleware assembled? |
| Overview | [data-model.md](data-model.md) | How are tables structured and what rules does the DB enforce? |
| Overview | [testing.md](testing.md) | How is code verified across unit, integration, and load tests? |
| Overview | [go-live.md](go-live.md) | What gaps and open decisions remain before production? |
| Overview | [staff-auth.md](staff-auth.md) | How do staff authenticate, manage sessions, and use the CLI? |
| Overview | [live-updates.md](live-updates.md) | How does the server push live event updates to staff browsers? |
| Overview | [frontend.md](frontend.md) | How do JavaScript, styles, CSP, and HTMX behave in browsers? |
| Domain | [domain/lifecycle.md](domain/lifecycle.md) | What are the core states and the end-to-end journey of a student? |
| Domain | [domain/links.md](domain/links.md) | How are private customer links issued, drafted, and expired? |
| Domain | [domain/customer-flow.md](domain/customer-flow.md) | How does the customer fill details, accept terms, and pay? |
| Domain | [domain/students.md](domain/students.md) | How are student records created, validated, and updated? |
| Domain | [domain/agreement.md](domain/agreement.md) | How are terms accepted and linked to start dates? |
| Billing | [billing/overview.md](billing/overview.md) | How do tuition plans, invoices, and the funding model work? |
| Billing | [billing/checkout.md](billing/checkout.md) | How are Stripe Checkout sessions initiated and tracked? |
| Billing | [billing/webhooks.md](billing/webhooks.md) | How are incoming Stripe webhooks validated and stored? |
| Billing | [billing/worker.md](billing/worker.md) | How does the leased background worker synchronize Stripe state? |
| Billing | [billing/invoices.md](billing/invoices.md) | How are later tuition and reinstatement invoices armed and paid? |
| Billing | [billing/suspension.md](billing/suspension.md) | When are students suspended for missed tuition and reinstated? |
| Billing | [billing/reviews.md](billing/reviews.md) | What causes review flags and how do they block automation? |
| Management | [management/overview.md](management/overview.md) | How do staff views, HTMX fragments, and navigation operate? |
| Management | [management/students.md](management/students.md) | How do staff inspect student details and perform billing actions? |
| Management | [management/dashboard.md](management/dashboard.md) | How are financial and operational metrics calculated? |
| Operations | [operations/configuration.md](operations/configuration.md) | What environment variables exist and how are they validated? |
| Operations | [operations/startup-shutdown.md](operations/startup-shutdown.md) | In what order does the system start, migrate, and shut down? |
| Operations | [operations/observability.md](operations/observability.md) | What structured log events and slow-query metrics are emitted? |
| Operations | [operations/local-development.md](operations/local-development.md) | How do engineers run Compose, Air, and Stripe CLI locally? |
| Operations | [operations/railway.md](operations/railway.md) | How is the application deployed and configured on Railway? |
| Operations | [operations/stripe-setup.md](operations/stripe-setup.md) | What Stripe dashboard configuration and pricing are required? |

## Documentation conventions

- **One home per fact**: Each behavior or concept is owned by a single document. Other pages link to the owning document rather than restating rules.
- **Source citations**: Each document concludes with a machine-verifiable `## Source` section citing repo-relative source files and symbols without line numbers.
- **Mermaid diagrams**: Architectural flows, state machines, and schemas use Mermaid syntax (`flowchart`, `sequenceDiagram`, `stateDiagram-v2`, `erDiagram`).
- **Synchronized updates**: Any pull request or commit that changes application behavior must update the matching documentation page in the same change.

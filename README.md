# ProjectHermes

Enrollment, agreement, and tuition-payment web application. Staff manage customer links, student records, and billing settings; customers open a reusable link that staff share; each browser gets its own private enrollment session to submit contact information, accept the agreement, and pay tuition through Stripe.

## Status

All data is mock and the application is **not live-ready** (see [AGENTS.md](AGENTS.md)). Losing links, sessions, or rows in a migration is acceptable.

## Quick start

```bash
test -f .env || cp .env.example .env
docker compose up --detach --wait --wait-timeout 90 db
go tool air
# Forward webhooks in a separate terminal:
stripe listen --forward-to http://127.0.0.1:3000/webhooks/stripe
```

## Stack

Go 1.27, PostgreSQL 17 via GORM, Stripe Go SDK v87, Argon2id, and HTMX.

## Where to look

| Area | Path | Description |
|---|---|---|
| Index & conventions | [docs/README.md](docs/README.md) | Full 29-page catalog, reading order, and documentation conventions |
| Architecture | [docs/architecture.md](docs/architecture.md) | Package graph, middleware pipeline, mounted route table |
| Data model | [docs/data-model.md](docs/data-model.md) | 12 PostgreSQL tables, ER diagram, DB-enforced constraints |
| Domain | [docs/domain/lifecycle.md](docs/domain/lifecycle.md) | Vocabulary, links lifecycle, customer forms, student registry |
| Billing | [docs/billing/overview.md](docs/billing/overview.md) | Hosted Checkout, one-off invoices, leased worker, suspension rules |
| Management | [docs/management/overview.md](docs/management/overview.md) | Staff interface, student roster, live SSE feed, frontend scripts |
| Operations | [docs/operations/configuration.md](docs/operations/configuration.md) | Environment variables, startup stages, Railway deployment |
| Testing & Go-live | [docs/testing.md](docs/testing.md) | Integration test layers, commands, verification checklist, open gaps |

## Verification

Run `go build ./... && go test -tags=integration ./...`. See the [Testing guide](docs/testing.md) for dedicated test database configuration, race detector flags, and static analysis.

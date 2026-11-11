# Go-live

Decisions the owner must make and gaps the code knowingly carries before the app handles real customers or live payments. [AGENTS.md](../AGENTS.md#production-ready-false) records `Production ready: false`: all data is mock and the Railway `production` environment is test-only ([Railway](operations/railway.md)). Nothing here blocks the current sandbox use.

## Open decisions

| Decision | What is open | Related pages |
|---|---|---|
| Data policy | While production-ready is false a migration may drop rows and the schema may change without backfills. That rule must be revisited the moment the flag flips. | [Data model](data-model.md) |
| Link reissue | A new link never revokes older ones, and links are not tied to a person. Decide whether re-sending a link should revoke the earlier one. | [Links](domain/links.md) |
| Customer verification and identity | Links are bearer credentials and valid contact syntax is not identity. Every paid link creates its own Student and contract; nothing deduplicates by email or phone. | [Customer flow](domain/customer-flow.md), [Students](domain/students.md) |
| Smart Retries | There is no proof yet that Stripe itself ran a retry on a one-off invoice (not a manual pay, not just a scheduled next attempt), nor of retry exhaustion. Treat retry behavior as unverified. The 2-week window is a dashboard-only setting the app cannot check. | [Stripe setup](operations/stripe-setup.md), [Suspension](billing/suspension.md) |
| Money | Tax treatment and registrations (automatic tax stays off), Adaptive Pricing on the live account, and the actual Stripe fees. | [Stripe setup](operations/stripe-setup.md) |
| Live Stripe account | The live catalog, a restricted key, the payment-method and portal configurations and the webhook endpoint; Apple Pay and Google Pay on a real device; live email delivery (sandbox suppresses automatic emails, so verify the dashboard toggles for successful-payment receipts, failed-payment alerts and 3D Secure links). | [Stripe setup](operations/stripe-setup.md), [Configuration](operations/configuration.md) |
| Staff passwords | The 4-character minimum in `readNewPassword` is marked as a temporary development value; raise it before production. | [Staff auth](staff-auth.md) |

## Known gaps

| Gap | Effect today | Upgrade path |
|---|---|---|
| Submit-vs-Enroll PIN race | `Submit` refuses a PIN held by an enrolled Student, but a `Submit` racing a concurrent `Enroll` can still see the PIN free (READ COMMITTED). The unique index `idx_student_pin` then rejects the second Student when that link is funded. The funding transaction rolls back, so Stripe has the payment but the app has no Student and no funded contract. Funding retries forever: through webhook events with backoff (5 s doubling to 15 min; `billing_reconcile_failed`, stage set to the event type) or through contract sync every 1 min (`stage contract_sync`). Changing the other Student's PIN in the staff dialog frees the index, after which a retry can complete funding. | One PIN-reservation table or an advisory lock (noted on `Submit`). |
| No staff action releases a review hold | A held contract or obligation stays held until a later sync recomputes its reason as resolved; staff Reconcile only re-runs that sync. Some reasons have no clearing path in code (`unresolved_provider_create` is only ever set). While any reason is set, `RequestReinstatement` and automatic reactivation refuse with `ErrNeedsReview`, so a suspended student cannot be reinstated. | A staff "resolve review" action with an audit entry. Reasons and clearing rules: [Reviews](billing/reviews.md). |
| Session SQL has no timeout | The SCS PostgreSQL store reads, writes and deletes sessions through context-free `database/sql` calls on the shared pool. Every request that loads or saves a session (sign-in, sign-out, any `/management/` request carrying a session cookie) therefore waits on a pooled connection with no deadline when the database stalls. The account lookup (`findUser`) and the 5-minute expired-session cleanup do use the 5 s `dbTimeout`. | A context-aware session store, or a per-request deadline around `LoadAndSave`. |
| Student field edits are not attributed | Link creation and cancellation (`CreatedBy`, `CanceledBy`), settings changes (`UpdatedBy`) and reinstatement requests (`RequestedBy`) record the staff username. A Student has no editor or edit-time column and the edit handler logs nothing, so who changed a name, email, phone, PIN or group is not recoverable. | An `updated_by` column or an audit table written in `UpdateField`'s transaction. See [Management students](management/students.md). |
| Lists are unpaginated | `links.Store.List` loads every link and `students.Store.List` every student matching the filter; the pages render them all. Fine for two staff and mock data. | Pagination or cursors before sustained growth. See [Management overview](management/overview.md). |
| Single replica | `live.Feed` fans change events out in process, so a page connected to one replica never hears about writes made through another. The login limiter is also per process, which multiplies the attempt budget by the replica count. The billing worker is replica-safe (leases and claims). | PostgreSQL `LISTEN/NOTIFY` for the feed and a shared limiter. See [Live updates](live-updates.md). |

## Source

- `AGENTS.md`: `Production ready: false`
- `internal/auth/cli.go`: `readNewPassword`
- `internal/auth/auth.go`: `dbTimeout`, `findUser`
- `internal/auth/http.go`: `NewWithCleanupInterval`, `deleteExpiredSessions`, `limiter`, `LoadAndSave`
- `internal/links/links.go`: `Submit`, `Enroll`, `pinIndex`, `Store.List`, `CreatedBy`, `CanceledBy`
- `internal/students/students.go`: `Student`, `Store.List`, `UpdateField`
- `internal/settings/settings.go`: `UpdatedBy`
- `internal/billing/worker.go`: `contractRetry`, `syncContract`, `logFailure`, `reviewFor`, `markContract`
- `internal/billing/suspension.go`: `hasReview`, `reinstatable`, `RequestReinstatement`
- `internal/billing/billing.go`: `ErrNeedsReview`
- `internal/billing/models.go`: `RequestedBy`
- `internal/live/live.go`: `Feed`

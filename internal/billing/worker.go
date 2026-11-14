package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
	"projecthermes/internal/students"
)

const (
	workerTick      = 5 * time.Second
	eventBatch      = 25
	contractBatch   = 10
	retryMin        = 5 * time.Second
	retryMax        = 15 * time.Minute
	activeSyncEvery = 15 * time.Minute
	auditEvery      = 24 * time.Hour

	leaseSQL     = "now() + interval '60 seconds'"
	eventsDue    = "processed_at IS NULL AND (retry_at IS NULL OR retry_at <= now()) AND (lease_expires_at IS NULL OR lease_expires_at <= now())"
	contractsDue = "(next_sync_at IS NULL OR next_sync_at <= now()) AND (lease_expires_at IS NULL OR lease_expires_at <= now())"
)

// ponytail: contracts carry no retry counter, so failed syncs retry at a fixed delay;
// add a retry_count column for the exponential schedule events already use.
const contractRetry = time.Minute

var (
	skipLocked = clause.Locking{Strength: clause.LockingStrengthUpdate, Options: clause.LockingOptionsSkipLocked}

	errLeaseLost    = errors.New("billing: contract lease lost")
	errContractBusy = errors.New("billing: contract is leased by another worker")
	errNotReady     = errors.New("billing: provider data is not ready")
	// errReviewed stops one obligation's workflow after its review reason is persisted.
	errReviewed = errors.New("billing: review recorded")
)

// lease proves one worker owns a contract's provider workflow.
type lease struct {
	id    uint64
	token string
}

// ---- worker loop and queues

// Run drains due webhook events, then due contracts, every workerTick or as soon as a webhook is stored, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	ctx = logging.WithOperation(ctx, "billing_worker")
	ticker := time.NewTicker(workerTick)
	defer ticker.Stop()
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
			ticker.Reset(workerTick)
		}
	}
}

// drain runs one worker pass: due events, then due contracts.
func (s *Service) drain(ctx context.Context) {
	s.drainEvents(ctx)
	s.drainContracts(ctx)
}

// claimEvents leases up to eventBatch due events with FOR UPDATE SKIP LOCKED in one short
// transaction; provider work happens after it commits.
func (s *Service) claimEvents(ctx context.Context) (events []WebhookEvent, token string, err error) {
	token = rand.Text()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(skipLocked).Where(eventsDue).Order("created_at ASC").Limit(eventBatch).Find(&events).Error; err != nil || len(events) == 0 {
			return err
		}
		ids := make([]string, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		return tx.Model(&WebhookEvent{}).Where("id IN ?", ids).
			Updates(map[string]any{"lease_token": token, "lease_expires_at": gorm.Expr(leaseSQL)}).Error
	})
	return events, token, err
}

func (s *Service) drainEvents(ctx context.Context) {
	events, token, err := s.claimEvents(ctx)
	if err != nil {
		s.logFailure(ctx, "event_claim", "", err)
		return
	}
	for _, e := range events {
		if ctx.Err() != nil {
			return
		}
		s.processEvent(ctx, e, token)
	}
}

func (s *Service) drainContracts(ctx context.Context) {
	var due []Contract
	if err := s.db.WithContext(ctx).Where(contractsDue).Order("next_sync_at ASC NULLS FIRST").Limit(contractBatch).Find(&due).Error; err != nil {
		s.logFailure(ctx, "contract_claim", "", err)
		return
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		s.syncContract(ctx, c)
	}
}

// logFailure logs only an opaque reference, a bounded stage and the error class; shutdown is silent.
func (s *Service) logFailure(ctx context.Context, stage, ref string, err error) {
	if ctx.Err() != nil {
		return
	}
	s.log.LogAttrs(ctx, slog.LevelWarn, "billing_reconcile_failed",
		append(logging.ErrorAttrs(err), slog.String("stage", stage), slog.String("ref", ref))...)
}

// processEvent runs one claimed event and records its outcome with the claim's token.
func (s *Service) processEvent(ctx context.Context, e WebhookEvent, token string) {
	mine := func(ctx context.Context) *gorm.DB {
		return s.db.WithContext(ctx).Model(&WebhookEvent{}).Where("id = ? AND lease_token = ?", e.ID, token)
	}
	// Events later in a batch may have waited long; renew before the provider calls.
	renewed := mine(ctx).Where("processed_at IS NULL").Update("lease_expires_at", gorm.Expr(leaseSQL))
	if renewed.Error != nil {
		s.logFailure(ctx, "event_lease", e.ID, renewed.Error)
		return
	}
	if renewed.RowsAffected == 0 {
		return
	}

	err := s.handleEvent(ctx, e)

	updates := map[string]any{"lease_token": nil, "lease_expires_at": nil}
	switch {
	case err == nil:
		updates["processed_at"] = gorm.Expr("now()")
		updates["retry_at"] = nil
	case ctx.Err() != nil:
		// Shutting down: hand the event back untouched.
	case errors.Is(err, errContractBusy):
		updates["retry_at"] = gorm.Expr("now() + make_interval(secs => ?)", retryMin.Seconds())
	default:
		updates["retry_count"] = e.RetryCount + 1
		updates["retry_at"] = gorm.Expr("now() + make_interval(secs => ?)", backoff(e.RetryCount+1).Seconds())
		s.logFailure(ctx, e.EventType, e.ID, err)
	}
	saveCtx, cancel := persistCtx(ctx)
	defer cancel()
	if err := mine(saveCtx).Updates(updates).Error; err != nil {
		s.logFailure(ctx, "event_finish", e.ID, err)
	}
}

// backoff doubles from retryMin to retryMax.
func backoff(attempt int) time.Duration {
	return min(retryMax, retryMin<<min(attempt-1, 10))
}

func isNotFound(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) && stripeErr.HTTPStatusCode == http.StatusNotFound
}

// ignoreMissing drops a vanished resource: unrelated or deleted objects need no work.
func ignoreMissing(err error) error {
	if isNotFound(err) {
		return nil
	}
	return err
}

// ---- webhook event routing

// handleEvent maps an event to its contract and refreshes that resource from Stripe; the
// event body is never applied, so replays and out-of-order delivery converge.
func (s *Service) handleEvent(ctx context.Context, e WebhookEvent) error {
	db := s.db.WithContext(ctx)
	switch {
	case strings.HasPrefix(e.EventType, "checkout.session."):
		session, err := s.retrieveSession(ctx, e.ResourceID)
		if err != nil {
			return ignoreMissing(err)
		}
		var attempt CheckoutAttempt
		// An unbound attempt is matched by the opaque reference sent as client_reference_id.
		found, err := firstRow(db, &attempt, "stripe_session_id = ? OR (stripe_session_id IS NULL AND reference = ?)", session.ID, session.ClientReferenceID)
		if err != nil || !found {
			return err
		}
		return s.withContract(ctx, attempt.ContractID, func(l lease) error {
			return s.syncAttempt(ctx, l, attempt.ID, session)
		})

	case strings.HasPrefix(e.EventType, "invoice."):
		var o Obligation
		found, err := firstRow(db, &o, "stripe_invoice_id = ?", e.ResourceID)
		if err != nil || !found {
			return err
		}
		return s.withContract(ctx, o.ContractID, func(l lease) error {
			return s.syncInvoice(ctx, l, o.ID)
		})

	case e.EventType == "customer.updated":
		var c Contract
		found, err := firstRow(db, &c, "stripe_customer_id = ?", e.ResourceID)
		if err != nil || !found {
			return err
		}
		return s.withContract(ctx, c.ID, func(l lease) error {
			return s.scheduleInvoices(ctx, l)
		})
	}

	// charge.*, refund.updated, radar.*: routed through the mapped PaymentIntent.
	intentID, err := s.paymentIntentOf(ctx, e)
	if err != nil || intentID == "" {
		return ignoreMissing(err)
	}
	var p Payment
	found, err := firstRow(db, &p, "id = ?", intentID)
	if err != nil || !found {
		// Not recorded yet: the invoice that pays it records current charge facts when it syncs.
		return err
	}
	return s.withContract(ctx, p.ContractID, func(l lease) error {
		return s.syncPayment(ctx, l, p.ID)
	})
}

// paymentIntentOf resolves the PaymentIntent behind a charge, refund, dispute or fraud warning.
func (s *Service) paymentIntentOf(ctx context.Context, e WebhookEvent) (string, error) {
	var intent *stripe.PaymentIntent
	charge := &stripe.Charge{ID: e.ResourceID}
	switch {
	case e.EventType == "refund.updated":
		r, err := s.client.V1Refunds.Retrieve(ctx, e.ResourceID, nil)
		if err != nil {
			return "", err
		}
		intent, charge = r.PaymentIntent, r.Charge
	case strings.HasPrefix(e.EventType, "charge.dispute."):
		d, err := s.client.V1Disputes.Retrieve(ctx, e.ResourceID, nil)
		if err != nil {
			return "", err
		}
		intent, charge = d.PaymentIntent, d.Charge
	case strings.HasPrefix(e.EventType, "radar."):
		w, err := s.client.V1RadarEarlyFraudWarnings.Retrieve(ctx, e.ResourceID, nil)
		if err != nil {
			return "", err
		}
		intent, charge = w.PaymentIntent, w.Charge
	}
	if intent != nil && intent.ID != "" {
		return intent.ID, nil
	}
	if charge == nil || charge.ID == "" {
		return "", nil
	}
	c, err := s.client.V1Charges.Retrieve(ctx, charge.ID, nil)
	if err != nil || c.PaymentIntent == nil {
		return "", err
	}
	return c.PaymentIntent.ID, nil
}

// ---- leases

// withContract runs fn while holding the contract's workflow lease; a contract another
// caller holds is reported busy so the event is retried shortly without counting a failure.
func (s *Service) withContract(ctx context.Context, id uint64, fn func(lease) error) error {
	l := lease{id: id, token: rand.Text()}
	res := s.db.WithContext(ctx).Model(&Contract{}).
		Where("id = ? AND (lease_expires_at IS NULL OR lease_expires_at <= now())", id).
		Updates(map[string]any{"lease_token": l.token, "lease_expires_at": gorm.Expr(leaseSQL)})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errContractBusy
	}
	defer s.release(ctx, l)
	return fn(l)
}

// release gives the lease back; a failed release only delays the contract until it expires.
func (s *Service) release(ctx context.Context, l lease) {
	ctx, cancel := persistCtx(ctx)
	defer cancel()
	err := s.db.WithContext(ctx).Model(&Contract{}).Where("id = ? AND lease_token = ?", l.id, l.token).
		Updates(map[string]any{"lease_token": nil, "lease_expires_at": nil}).Error
	if err != nil {
		s.logFailure(ctx, "lease_release", "", err)
	}
}

// renew extends a still-held lease between persisted provider stages.
func (s *Service) renew(ctx context.Context, l lease) error {
	res := s.db.WithContext(ctx).Model(&Contract{}).
		Where("id = ? AND lease_token = ? AND lease_expires_at > now()", l.id, l.token).
		Update("lease_expires_at", gorm.Expr(leaseSQL))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errLeaseLost
	}
	return nil
}

// leased runs fn in a transaction holding the Link then the Contract row lock, only while l still owns
// the lease. It outlives ctx so a provider outcome already obtained is still recorded.
func (s *Service) leased(ctx context.Context, l lease, fn func(tx *gorm.DB, c *Contract) error) error {
	ctx, cancel := persistCtx(ctx)
	defer cancel()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Enrollment before Contract, the order checkout and contact edits take; funding writes the Enrollment.
		if err := tx.Exec("SELECT 1 FROM enrollments WHERE id = (SELECT enrollment_id FROM billing_contracts WHERE id = ?) FOR UPDATE", l.id).Error; err != nil {
			return err
		}
		var c Contract
		found, err := firstRow(tx.Clauses(forUpdate), &c, "id = ? AND lease_token = ? AND lease_expires_at > now()", l.id, l.token)
		if err != nil {
			return err
		}
		if !found {
			return errLeaseLost
		}
		return fn(tx, &c)
	})
}

// leasedStudent is leased plus the Student row lock, ordered Link, Contract, then Student.
func (s *Service) leasedStudent(ctx context.Context, l lease, fn func(tx *gorm.DB, c *Contract, student *students.Student) error) error {
	return s.leased(ctx, l, func(tx *gorm.DB, c *Contract) error {
		var student students.Student
		if err := require(firstRow(tx.Clauses(forUpdate), &student, "enrollment_id = ?", c.EnrollmentID)); err != nil {
			return err
		}
		return fn(tx, c, &student)
	})
}

// markContract sets or clears one contract-level reason. The column holds a single reason,
// so one owned by another workflow keeps the slot; the next sync re-detects whatever lost it.
func markContract(tx *gorm.DB, c *Contract, reason string, set bool) (bool, error) {
	switch current := c.ReviewReason; {
	case set && current == nil:
		c.ReviewReason = &reason
	case !set && current != nil && *current == reason:
		c.ReviewReason = nil
	default:
		return false, nil
	}
	return true, tx.Model(&Contract{}).Where("id = ?", c.ID).Update("review_reason", nullable(deref(c.ReviewReason))).Error
}

// mark persists markContract under the lease and announces a visible change.
func (s *Service) mark(ctx context.Context, l lease, reason string, set bool) error {
	var did bool
	var ref string
	err := s.leased(ctx, l, func(tx *gorm.DB, c *Contract) (err error) {
		ref = c.Reference
		did, err = markContract(tx, c, reason, set)
		return err
	})
	if err == nil && did {
		if set {
			s.log.LogAttrs(ctx, slog.LevelWarn, "billing_review_required",
				slog.String("contract", ref), slog.String("reason", reason))
		}
		s.changed()
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullable maps "" to SQL NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- checkout attempts and funding

func (s *Service) retrieveSession(ctx context.Context, id string) (*stripe.CheckoutSession, error) {
	params := &stripe.CheckoutSessionRetrieveParams{}
	params.AddExpand("payment_intent")
	return s.client.V1CheckoutSessions.Retrieve(ctx, id, params)
}

// sessionMatches reports whether session is exactly the Checkout quoted for the attempt.
func sessionMatches(c Contract, a CheckoutAttempt, session *stripe.CheckoutSession) bool {
	return session.Currency == stripe.CurrencyUSD &&
		session.Mode == stripe.CheckoutSessionModePayment &&
		session.Customer != nil && c.StripeCustomerID != nil && session.Customer.ID == *c.StripeCustomerID &&
		session.AmountTotal == a.FirstAmountCents
}

// syncAttempt applies the Session's current state to its attempt. A paid Session funds the
// contract; completed-but-unpaid stays processing; expiry or failed async payment ends it.
func (s *Service) syncAttempt(ctx context.Context, l lease, attemptID uint64, session *stripe.CheckoutSession) error {
	db := s.db.WithContext(ctx)
	var c Contract
	var a CheckoutAttempt
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	if err := require(firstRow(db, &a, "id = ? AND contract_id = ?", attemptID, l.id)); err != nil {
		return err
	}
	if a.StripeSessionID == nil {
		// Recovering a Session whose ID was never recorded: only an exact same-Customer match binds.
		if !sessionMatches(c, a, session) {
			return s.mark(ctx, l, reviewConfigurationMismatch, true)
		}
		err := s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
			return tx.Model(&CheckoutAttempt{}).Where("id = ?", a.ID).Update("stripe_session_id", session.ID).Error
		})
		if err != nil {
			return err
		}
		a.StripeSessionID = &session.ID
	}

	switch {
	case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid:
		if c.FundedAt == nil {
			return s.fund(ctx, l, c, a, session)
		}
		return s.scheduleInvoices(ctx, l) // crash recovery after funding
	case session.Status == stripe.CheckoutSessionStatusExpired:
		return s.moveAttempt(ctx, l, a.ID, "expired")
	case session.Status == stripe.CheckoutSessionStatusComplete:
		to := "processing"
		if pi := session.PaymentIntent; pi != nil &&
			(pi.Status == stripe.PaymentIntentStatusRequiresPaymentMethod || pi.Status == stripe.PaymentIntentStatusCanceled) {
			to = "failed"
		}
		return s.moveAttempt(ctx, l, a.ID, to)
	case session.Status == stripe.CheckoutSessionStatusOpen && session.URL != "" &&
		(a.State == "creating" || a.State == "uncertain"):
		// An adopted Session the browser never received becomes the attempt's payable link.
		return s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
			return tx.Model(&CheckoutAttempt{}).Where("id = ? AND state = ?", a.ID, a.State).Updates(map[string]any{
				"state":        "open",
				"checkout_url": session.URL,
			}).Error
		})
	case a.State == "expiring":
		_, err := s.expireAttempt(ctx, &a) // finishes an abandoned plan's Expire call
		return err
	}
	return nil
}

// checkoutKeyRetention is how long a Checkout create may be replayed under its original
// idempotency key; Stripe may prune keys after 24 hours.
const checkoutKeyRetention = 23 * time.Hour

// resumeCreate settles an attempt whose Session ID was never recorded (browser gone, timeout,
// crash). A Session already created for it is adopted by client_reference_id; otherwise the
// create is replayed under the same idempotency key while Stripe still honors it. Past that
// window the consistent list proves no Session exists, so the attempt fails and frees the contract.
func (s *Service) resumeCreate(ctx context.Context, l lease, a CheckoutAttempt) error {
	if a.State == "creating" && time.Since(a.CreationStartedAt) < 15*time.Second {
		// Fresh attempt actively in-flight from web request; let the request finish.
		return nil
	}
	db := s.db.WithContext(ctx)
	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	if c.StripeCustomerID == nil {
		// Unreachable: the Customer is saved in the same transaction as the attempt.
		return s.moveAttempt(ctx, l, a.ID, "failed")
	}
	var found []string
	sessions := s.client.V1CheckoutSessions.List(ctx, &stripe.CheckoutSessionListParams{Customer: c.StripeCustomerID})
	for session, err := range sessions.All(ctx) {
		if err != nil {
			return err
		}
		if session.ClientReferenceID == a.Reference {
			found = append(found, session.ID)
		}
	}
	switch {
	case len(found) == 1:
		session, err := s.retrieveSession(ctx, found[0])
		if err != nil {
			return err
		}
		return s.syncAttempt(ctx, l, a.ID, session)
	case len(found) > 1:
		return s.mark(ctx, l, reviewUnresolvedProviderCreate, true)
	case time.Since(a.CreationStartedAt) < checkoutKeyRetention:
		var first Obligation
		if err := require(firstRow(db, &first, "contract_id = ? AND kind = 'tuition' AND number = 1", c.ID)); err != nil {
			return err
		}
		_, err := s.createSession(ctx, c, first, a)
		return err
	}
	return s.moveAttempt(ctx, l, a.ID, "failed")
}

// moveAttempt moves a nonterminal attempt to another state under the lease.
func (s *Service) moveAttempt(ctx context.Context, l lease, id uint64, to string) error {
	var moved bool
	err := s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
		res := tx.Model(&CheckoutAttempt{}).Where("id = ? AND state IN ? AND state <> ?", id, activeAttemptStates, to).Update("state", to)
		moved = res.RowsAffected > 0
		return res.Error
	})
	if err == nil && moved {
		s.changed()
	}
	return err
}

// fund records the first payment once the Session is paid and its generated invoice confirms
// it, then schedules the remaining invoices. Every provider read happens before the single
// funding transaction, and FundedAt is anchored to the invoice's paid_at, never a local clock.
func (s *Service) fund(ctx context.Context, l lease, c Contract, a CheckoutAttempt, session *stripe.CheckoutSession) error {
	if session.PaymentIntent == nil || session.Invoice == nil || session.Invoice.ID == "" {
		// Invoice linkage can lag a paid Session: stay processing so no other Checkout or quote change slips in.
		return errors.Join(s.moveAttempt(ctx, l, a.ID, "processing"), errNotReady)
	}
	var first Obligation
	if err := require(firstRow(s.db.WithContext(ctx), &first, "contract_id = ? AND kind = 'tuition' AND number = 1", c.ID)); err != nil {
		return err
	}
	inv, err := s.client.V1Invoices.Retrieve(ctx, session.Invoice.ID, nil)
	if err != nil {
		return err
	}
	if inv.Status != stripe.InvoiceStatusPaid || inv.StatusTransitions == nil || inv.StatusTransitions.PaidAt == 0 {
		return errors.Join(s.moveAttempt(ctx, l, a.ID, "processing"), errNotReady)
	}
	if inv.AmountPaid != a.FirstAmountCents || !invoiceMatches(first, inv) {
		return errors.Join(s.moveAttempt(ctx, l, a.ID, "processing"), s.mark(ctx, l, reviewInvoiceMismatch, true))
	}
	fundedAt := time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()

	intentID := session.PaymentIntent.ID
	paid, err := s.paidInvoicePayments(ctx, inv.ID)
	if err != nil {
		return err
	}
	var initial *stripe.InvoicePayment
	for _, ip := range paid {
		if ip.Payment.PaymentIntent != nil && ip.Payment.PaymentIntent.ID == intentID {
			initial = ip
		}
	}
	if initial == nil {
		return errNotReady
	}
	if initial.AmountPaid != a.FirstAmountCents {
		return errors.Join(s.moveAttempt(ctx, l, a.ID, "processing"), s.mark(ctx, l, reviewInvoiceMismatch, true))
	}
	pay, err := s.paymentFrom(ctx, c.ID, intentID, fundedAt)
	if err != nil {
		return err
	}

	var funded bool
	err = s.leased(ctx, l, func(tx *gorm.DB, cur *Contract) error {
		if cur.FundedAt != nil {
			return nil
		}
		var obligations []Obligation
		if err := tx.Where("contract_id = ? AND kind = 'tuition'", cur.ID).Order("number ASC").Find(&obligations).Error; err != nil {
			return err
		}
		// Dates come from the contract's frozen plan and persisted amounts, never today's registry.
		amounts := make([]int64, len(obligations))
		for i, o := range obligations {
			amounts[i] = o.AmountCents
		}
		schedule := payments.Plan{ID: cur.PlanID, Name: cur.PlanName, Amounts: amounts}.Schedule(fundedAt)
		for i, o := range obligations {
			updates := map[string]any{"due_at": schedule[i].Date, "amount_remaining_cents": o.AmountCents}
			if i == 0 {
				updates = map[string]any{
					"due_at":                 schedule[0].Date,
					"stripe_invoice_id":      inv.ID,
					"latest_stripe_status":   string(inv.Status),
					"amount_paid_cents":      inv.AmountPaid,
					"amount_remaining_cents": 0,
					"first_settled_at":       fundedAt,
				}
			}
			if err := tx.Model(&Obligation{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		if _, err := savePayment(tx, pay); err != nil {
			return err
		}
		if err := tx.Model(&CheckoutAttempt{}).Where("id = ?", a.ID).Update("state", "paid").Error; err != nil {
			return err
		}
		for _, reason := range []string{reviewInvoiceMismatch, reviewConfigurationMismatch} {
			if _, err := markContract(tx, cur, reason, false); err != nil {
				return err
			}
		}
		if err := links.Enroll(tx, cur.EnrollmentID, fundedAt); err != nil {
			return err
		}
		funded = true
		return tx.Model(&Contract{}).Where("id = ?", cur.ID).Updates(map[string]any{"funded_at": fundedAt, "next_sync_at": nil}).Error
	})
	if err != nil {
		return err
	}
	if funded {
		s.changed()
	}
	return s.scheduleInvoices(ctx, l)
}

// paidInvoicePayments lists every paid InvoicePayment of an invoice, following pagination.
func (s *Service) paidInvoicePayments(ctx context.Context, invoiceID string) ([]*stripe.InvoicePayment, error) {
	var paid []*stripe.InvoicePayment
	list := s.client.V1InvoicePayments.List(ctx, &stripe.InvoicePaymentListParams{Invoice: stripe.String(invoiceID)})
	for ip, err := range list.All(ctx) {
		if err != nil {
			return nil, err
		}
		if ip.Status == "paid" && ip.Payment != nil && ip.AmountPaid > 0 {
			paid = append(paid, ip)
		}
	}
	return paid, nil
}

func paidAt(ip *stripe.InvoicePayment) time.Time {
	if t := ip.StatusTransitions; t != nil && t.PaidAt > 0 {
		return time.Unix(t.PaidAt, 0).UTC()
	}
	return time.Unix(ip.Created, 0).UTC()
}

// ---- payments

// paymentFrom reads a succeeded PaymentIntent as the Payment row to store: gross received,
// refunded cents, dispute/fraud state and any balance-transaction fee/net Stripe has settled.
func (s *Service) paymentFrom(ctx context.Context, contractID uint64, intentID string, at time.Time) (Payment, error) {
	params := &stripe.PaymentIntentRetrieveParams{}
	params.AddExpand("latest_charge.balance_transaction")
	pi, err := s.client.V1PaymentIntents.Retrieve(ctx, intentID, params)
	if err != nil {
		return Payment{}, err
	}
	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		return Payment{}, errNotReady
	}
	p := Payment{ID: pi.ID, ContractID: contractID, GrossReceivedCents: pi.AmountReceived, PaymentTime: at}
	ch := pi.LatestCharge
	if ch == nil || ch.ID == "" {
		return p, nil
	}
	p.StripeChargeID = &ch.ID
	p.GrossRefundedCents = ch.AmountRefunded
	if bt := ch.BalanceTransaction; bt != nil && bt.ID != "" {
		p.BalanceTransactionID, p.FeeCents, p.NetCents = &bt.ID, &bt.Fee, &bt.Net
	}
	p.DisputeOrFraudReviewState, err = s.reviewState(ctx, ch)
	return p, err
}

// reviewState summarizes disputes and early fraud warnings filtered by the Charge. A lost
// dispute outranks an open one, which outranks a warning; won/closed ones need no review.
func (s *Service) reviewState(ctx context.Context, ch *stripe.Charge) (*string, error) {
	state := ""
	if ch.Disputed {
		for d, err := range s.client.V1Disputes.List(ctx, &stripe.DisputeListParams{Charge: stripe.String(ch.ID)}).All(ctx) {
			if err != nil {
				return nil, err
			}
			switch d.Status {
			case stripe.DisputeStatusLost:
				state = "dispute_lost"
			case stripe.DisputeStatusWon, stripe.DisputeStatusWarningClosed, stripe.DisputeStatusPrevented:
			default:
				if state != "dispute_lost" {
					state = "dispute_open"
				}
			}
		}
	}
	if state == "" {
		warnings := s.client.V1RadarEarlyFraudWarnings.List(ctx, &stripe.RadarEarlyFraudWarningListParams{Charge: stripe.String(ch.ID)})
		for _, err := range warnings.All(ctx) {
			if err != nil {
				return nil, err
			}
			state = "fraud_warning"
			break
		}
	}
	if state == "" {
		return nil, nil
	}
	return &state, nil
}

type paymentFacts struct {
	charge, balance, review *string
	fee, net                *int64
	refunded                int64
}

func (p Payment) facts() paymentFacts {
	return paymentFacts{p.StripeChargeID, p.BalanceTransactionID, p.DisputeOrFraudReviewState, p.FeeCents, p.NetCents, p.GrossRefundedCents}
}

// savePayment inserts a PaymentIntent once and afterwards only refreshes mutable Stripe facts.
func savePayment(tx *gorm.DB, p Payment) (bool, error) {
	var old Payment
	found, err := firstRow(tx, &old, "id = ?", p.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return true, tx.Create(&p).Error
	}
	if reflect.DeepEqual(old.facts(), p.facts()) {
		return false, nil
	}
	return true, tx.Model(&Payment{}).Where("id = ?", p.ID).
		Select("stripe_charge_id", "gross_refunded_cents", "dispute_or_fraud_review_state", "balance_transaction_id", "fee_cents", "net_cents").
		Updates(p).Error
}

// syncPayment refreshes one recorded PaymentIntent.
func (s *Service) syncPayment(ctx context.Context, l lease, id string) error {
	var old Payment
	if err := require(firstRow(s.db.WithContext(ctx), &old, "id = ? AND contract_id = ?", id, l.id)); err != nil {
		return err
	}
	pay, err := s.paymentFrom(ctx, l.id, id, old.PaymentTime)
	if err != nil {
		return err
	}
	return s.store(ctx, l, nil, []Payment{pay})
}

// ---- invoices

// invoiceState is the part of an Obligation that follows its Stripe invoice.
type invoiceState struct {
	Status, Review         string
	Paid, Remaining, Other int64
	Failed, Settled        bool
}

// invoiceRead is one provider invoice mapped onto its obligation.
type invoiceRead struct {
	o         Obligation
	st        invoiceState
	settledAt time.Time
}

func (o Obligation) state() invoiceState {
	return invoiceState{
		Status:    deref(o.LatestStripeStatus),
		Review:    deref(o.ReviewReason),
		Paid:      o.AmountPaidCents,
		Remaining: o.AmountRemainingCents,
		Other:     o.OtherSettlementCents,
		Failed:    o.FailureOrActionRequired,
		Settled:   o.FirstSettledAt != nil,
	}
}

// invoiceMatches reports whether inv is exactly the quoted one-line USD invoice for o, with
// no tax, discount or customer debit that would change what is collected.
func invoiceMatches(o Obligation, inv *stripe.Invoice) bool {
	if inv.Currency != stripe.CurrencyUSD || inv.StartingBalance > 0 ||
		len(inv.Discounts) > 0 || len(inv.TotalDiscountAmounts) > 0 || len(inv.TotalTaxes) > 0 ||
		inv.Lines == nil || len(inv.Lines.Data) != 1 {
		return false
	}
	line := inv.Lines.Data[0]
	return line.Amount == o.AmountCents && line.Currency == stripe.CurrencyUSD &&
		len(line.Taxes) == 0 && len(line.DiscountAmounts) == 0
}

// reviewFor keeps a reason owned by another workflow; otherwise the invoice-derived one wins.
func reviewFor(o Obligation, computed string) string {
	switch current := deref(o.ReviewReason); current {
	case "", reviewInvoiceMismatch, reviewMissingInvoice:
		return computed
	default:
		return current
	}
}

// invoiceStateOf maps the current invoice onto the quoted obligation. Drafts owe the quoted
// cents; void or uncollectible invoices never forgive signed tuition; credits and off-Stripe
// settlement are kept apart from Stripe cash.
func invoiceStateOf(o Obligation, inv *stripe.Invoice) (st invoiceState, settledAt time.Time) {
	st = invoiceState{
		Status:  string(inv.Status),
		Paid:    inv.AmountPaid,
		Other:   max(0, inv.Total-inv.AmountDue) + inv.AmountPaidOffStripe,
		Settled: o.FirstSettledAt != nil,
	}
	computed := ""
	switch inv.Status {
	case stripe.InvoiceStatusDraft:
		st.Paid, st.Other, st.Remaining = 0, 0, o.AmountCents
	case stripe.InvoiceStatusPaid:
		if inv.StatusTransitions != nil && inv.StatusTransitions.PaidAt > 0 {
			st.Settled = true
			settledAt = time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()
		}
		if o.CollectionDisposition == "cancel" {
			computed = reviewPaymentReview // a corrected fee was collected anyway: staff refund review
		}
	case stripe.InvoiceStatusOpen:
		st.Remaining = inv.AmountRemaining
		st.Failed = inv.Attempted
	default: // void, uncollectible
		if o.CollectionDisposition != "cancel" { // a corrected fee is meant to be void
			st.Remaining = max(0, o.AmountCents-st.Paid-st.Other)
			computed = reviewInvoiceMismatch
		}
	}
	if !invoiceMatches(o, inv) {
		computed = reviewInvoiceMismatch
	}
	st.Review = reviewFor(o, computed)
	return st, settledAt
}

// syncInvoice refreshes one obligation from its current invoice: it records each paid
// InvoicePayment's PaymentIntent as a payment, then stores the invoice-derived state and
// review flag on the obligation.
func (s *Service) syncInvoice(ctx context.Context, l lease, obligationID uint64) error {
	var o Obligation
	found, err := firstRow(s.db.WithContext(ctx), &o, "id = ? AND contract_id = ?", obligationID, l.id)
	if err != nil || !found || o.StripeInvoiceID == nil {
		return err
	}
	if (o.Kind == "reinstatement" || o.Number > 1) && o.StripeInvoiceItemID == nil {
		return nil // the creating flow owns the draft until its item exists; an empty draft is not a mismatch
	}
	inv, err := s.client.V1Invoices.Retrieve(ctx, *o.StripeInvoiceID, nil)
	if isNotFound(err) || (err == nil && inv.Deleted) {
		// A known invoice that vanished is reviewed, never replaced.
		st := o.state()
		st.Status, st.Review = "deleted", reviewFor(o, reviewMissingInvoice)
		if o.CollectionDisposition == "cancel" { // a corrected fee's invoice is meant to vanish
			st.Review, st.Remaining = reviewFor(o, ""), 0
		}
		return s.store(ctx, l, &invoiceRead{o: o, st: st}, nil)
	}
	if err != nil {
		return err
	}
	paid, err := s.paidInvoicePayments(ctx, inv.ID)
	if err != nil {
		return err
	}
	var pays []Payment
	seen := map[string]bool{}
	for _, ip := range paid {
		pi := ip.Payment.PaymentIntent
		if pi == nil || seen[pi.ID] {
			continue
		}
		seen[pi.ID] = true
		pay, err := s.paymentFrom(ctx, l.id, pi.ID, paidAt(ip))
		if err != nil {
			return err
		}
		pays = append(pays, pay)
	}
	st, settledAt := invoiceStateOf(o, inv)
	if err := s.store(ctx, l, &invoiceRead{o, st, settledAt}, pays); err != nil {
		return err
	}
	// The review is persisted first; only then stop an unexpected invoice from collecting.
	if !invoiceMatches(o, inv) && inv.AutoAdvance && (inv.Status == stripe.InvoiceStatusDraft || inv.Status == stripe.InvoiceStatusOpen) {
		return s.holdInvoice(ctx, l, inv.ID)
	}
	return nil
}

func (s *Service) holdInvoice(ctx context.Context, l lease, id string) error {
	if err := s.renew(ctx, l); err != nil {
		return err
	}
	_, err := s.client.V1Invoices.Update(ctx, id, &stripe.InvoiceUpdateParams{AutoAdvance: stripe.Bool(false)})
	return err
}

// store applies one provider read under the lease in a single transaction and announces
// only changes a viewer can see. read is nil for payment-only refreshes.
func (s *Service) store(ctx context.Context, l lease, read *invoiceRead, pays []Payment) error {
	var changed bool
	var obligationRef, contractRef string
	err := s.leased(ctx, l, func(tx *gorm.DB, c *Contract) error {
		contractRef = c.Reference
		if read != nil && read.st != read.o.state() {
			st := read.st
			updates := map[string]any{
				"latest_stripe_status":       st.Status,
				"amount_paid_cents":          st.Paid,
				"amount_remaining_cents":     st.Remaining,
				"other_settlement_cents":     st.Other,
				"failure_or_action_required": st.Failed,
				"review_reason":              nullable(st.Review),
			}
			if st.Settled && read.o.FirstSettledAt == nil {
				updates["first_settled_at"] = read.settledAt // earliest authoritative settlement is kept
			}
			if err := tx.Model(&Obligation{}).Where("id = ?", read.o.ID).Updates(updates).Error; err != nil {
				return err
			}
			changed = true
			if st.Review != "" && st.Review != read.o.state().Review {
				obligationRef = read.o.Reference
			}
		}
		for _, p := range pays {
			did, err := savePayment(tx, p)
			if err != nil {
				return err
			}
			changed = changed || did
		}
		// Refunds, disputes and fraud warnings are surfaced for staff; nothing is rebilled or refunded.
		var flagged int64
		if err := tx.Model(&Payment{}).
			Where("contract_id = ? AND (gross_refunded_cents > 0 OR dispute_or_fraud_review_state IS NOT NULL)", c.ID).
			Count(&flagged).Error; err != nil {
			return err
		}
		did, err := markContract(tx, c, reviewPaymentReview, flagged > 0)
		changed = changed || did
		return err
	})
	if err != nil {
		return err
	}
	if obligationRef != "" {
		s.log.LogAttrs(ctx, slog.LevelWarn, "billing_review_required",
			slog.String("contract", contractRef), slog.String("obligation", obligationRef))
	}
	if changed {
		s.changed()
	}
	return nil
}

// ---- invoice creation (later tuition, reinstatement fee)

// scheduleInvoices creates and arms the later one-off invoices, resuming whatever stage a
// crash left behind. Only the persisted N-1 later obligations are ever created, each guarded
// by its stored provider IDs and a deterministic idempotency key, so a funded contract ends
// with exactly zero, two or five scheduled invoices.
func (s *Service) scheduleInvoices(ctx context.Context, l lease) error {
	db := s.db.WithContext(ctx)
	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	if c.FundedAt == nil || c.StripeCustomerID == nil {
		return nil
	}
	// Reviewed obligations are never re-armed merely because a setup stage is incomplete.
	var pending []Obligation
	if err := db.Where("contract_id = ? AND kind = 'tuition' AND number > 1 AND collection_disposition = 'collect' "+
		"AND review_reason IS NULL AND due_at IS NOT NULL "+
		"AND (stripe_invoice_id IS NULL OR stripe_invoice_item_id IS NULL OR scheduled_finalization_at IS NULL)", c.ID).
		Order("number ASC").Find(&pending).Error; err != nil || len(pending) == 0 {
		return err
	}
	if ok, err := s.ensureDefaultPaymentMethod(ctx, l, c); err != nil || !ok {
		return err
	}
	for i := range pending {
		if err := s.scheduleOne(ctx, l, c, &pending[i]); err != nil && !errors.Is(err, errReviewed) {
			return err
		}
	}
	return nil
}

// scheduleOne runs one obligation's three persisted stages: draft invoice, its item, arming.
func (s *Service) scheduleOne(ctx context.Context, l lease, c Contract, o *Obligation) error {
	if o.StripeInvoiceID == nil {
		id, err := s.createInvoice(ctx, l, c, o)
		if err != nil {
			return err
		}
		if err := s.saveObligation(ctx, l, o.ID, map[string]any{"stripe_invoice_id": id}); err != nil {
			return err
		}
		o.StripeInvoiceID = &id
	}
	if o.StripeInvoiceItemID == nil {
		id, err := s.createItem(ctx, l, c, o)
		if err != nil {
			return err
		}
		if err := s.saveObligation(ctx, l, o.ID, map[string]any{"stripe_invoice_item_id": id}); err != nil {
			return err
		}
		o.StripeInvoiceItemID = &id
	}
	if o.ScheduledFinalizationAt == nil {
		return s.arm(ctx, l, o)
	}
	return nil
}

// createInvoice is stage one: a noncollecting draft. A retry first adopts any draft an
// earlier attempt created, so an expired idempotency key cannot duplicate it.
func (s *Service) createInvoice(ctx context.Context, l lease, c Contract, o *Obligation) (string, error) {
	if o.InvoiceCreationStartedAt != nil {
		found, err := s.invoicesOf(ctx, *c.StripeCustomerID, o.Reference)
		if err != nil {
			return "", err
		}
		if id, err := s.adopt(ctx, l, o, found); err != nil || id != "" {
			return id, err
		}
	} else {
		now := time.Now().UTC()
		if err := s.saveObligation(ctx, l, o.ID, map[string]any{"invoice_creation_started_at": now}); err != nil {
			return "", err
		}
		o.InvoiceCreationStartedAt = &now
	}
	if err := s.renew(ctx, l); err != nil {
		return "", err
	}
	params := &stripe.InvoiceCreateParams{
		Customer:                    stripe.String(*c.StripeCustomerID),
		Currency:                    stripe.String("usd"),
		CollectionMethod:            stripe.String(string(stripe.InvoiceCollectionMethodChargeAutomatically)),
		AutoAdvance:                 stripe.Bool(false),
		PendingInvoiceItemsBehavior: stripe.String("exclude"),
		Metadata:                    map[string]string{"tuition_contract": c.Reference, "tuition_obligation": o.Reference},
		// Card covers Apple Pay and Google Pay; Klarna is for the single payment only, so the
		// account's invoice defaults never add it here.
		PaymentSettings: &stripe.InvoiceCreatePaymentSettingsParams{PaymentMethodTypes: stripe.StringSlice([]string{"card"})},
	}
	if o.Kind == "reinstatement" {
		// The student pays a fee on Stripe's hosted page by the next day; it is never charged to a saved card.
		params.CollectionMethod = stripe.String(string(stripe.InvoiceCollectionMethodSendInvoice))
		params.DaysUntilDue = stripe.Int64(1)
	}
	params.SetIdempotencyKey("tuition:" + o.Reference + ":inv")
	inv, err := s.client.V1Invoices.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return inv.ID, nil
}

// invoicesOf lists the Customer's invoices created for one obligation reference.
func (s *Service) invoicesOf(ctx context.Context, customer, reference string) ([]string, error) {
	var found []string
	for inv, err := range s.client.V1Invoices.List(ctx, &stripe.InvoiceListParams{Customer: stripe.String(customer)}).All(ctx) {
		if err != nil {
			return nil, err
		}
		if inv.Metadata["tuition_obligation"] == reference {
			found = append(found, inv.ID)
		}
	}
	return found, nil
}

// createItem is stage two: the quoted Price attached to that exact draft.
func (s *Service) createItem(ctx context.Context, l lease, c Contract, o *Obligation) (string, error) {
	if o.ItemCreationStartedAt != nil {
		var found []string
		items := s.client.V1InvoiceItems.List(ctx, &stripe.InvoiceItemListParams{Invoice: o.StripeInvoiceID})
		for item, err := range items.All(ctx) {
			if err != nil {
				return "", err
			}
			if item.Metadata["tuition_obligation"] == o.Reference {
				found = append(found, item.ID)
			}
		}
		if id, err := s.adopt(ctx, l, o, found); err != nil || id != "" {
			return id, err
		}
	} else {
		now := time.Now().UTC()
		if err := s.saveObligation(ctx, l, o.ID, map[string]any{"item_creation_started_at": now}); err != nil {
			return "", err
		}
		o.ItemCreationStartedAt = &now
	}
	if err := s.renew(ctx, l); err != nil {
		return "", err
	}
	description := fmt.Sprintf("Pago %d de %d — %s", o.Number, c.InstallmentCount, c.PlanName)
	if o.Kind == "reinstatement" {
		description = "Reinstalación"
	}
	params := &stripe.InvoiceItemCreateParams{
		Customer:    stripe.String(*c.StripeCustomerID),
		Invoice:     stripe.String(*o.StripeInvoiceID),
		Pricing:     &stripe.InvoiceItemCreatePricingParams{Price: stripe.String(o.StripePriceID)},
		Quantity:    stripe.Int64(1),
		Description: stripe.String(description),
		Metadata:    map[string]string{"tuition_obligation": o.Reference},
	}
	params.SetIdempotencyKey("tuition:" + o.Reference + ":item")
	item, err := s.client.V1InvoiceItems.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return item.ID, nil
}

// adopt picks the one provider resource an earlier crashed attempt created; several are a
// review case, and none lets the caller create it.
func (s *Service) adopt(ctx context.Context, l lease, o *Obligation, found []string) (string, error) {
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", s.reviewObligation(ctx, l, o, reviewUnresolvedProviderCreate)
}

// arm is stage three: validate the exact quote, then let Stripe finalize on the due date.
// Dates already past keep their original due date and finalize now with automatic collection.
// A reinstatement fee has no due date: it is finalized and emailed at once (sendFee).
func (s *Service) arm(ctx context.Context, l lease, o *Obligation) error {
	inv, err := s.client.V1Invoices.Retrieve(ctx, *o.StripeInvoiceID, nil)
	if isNotFound(err) {
		return s.reviewObligation(ctx, l, o, reviewMissingInvoice)
	}
	if err != nil {
		return err
	}
	if !invoiceMatches(*o, inv) || (o.Kind == "reinstatement" && inv.Total != o.AmountCents) {
		if err := s.reviewObligation(ctx, l, o, reviewInvoiceMismatch); !errors.Is(err, errReviewed) {
			return err
		}
		if inv.AutoAdvance {
			if err := s.holdInvoice(ctx, l, inv.ID); err != nil {
				return err
			}
		}
		return errReviewed
	}
	if o.Kind == "reinstatement" {
		return s.sendFee(ctx, l, o, inv)
	}
	status := inv.Status
	if inv.Status == stripe.InvoiceStatusDraft {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		var armed *stripe.Invoice
		if o.DueAt.After(time.Now()) {
			params := &stripe.InvoiceUpdateParams{
				AutoAdvance:              stripe.Bool(true),
				AutomaticallyFinalizesAt: stripe.Int64(o.DueAt.Unix()),
			}
			params.SetIdempotencyKey("tuition:" + o.Reference + ":arm")
			armed, err = s.client.V1Invoices.Update(ctx, inv.ID, params)
		} else {
			params := &stripe.InvoiceFinalizeInvoiceParams{AutoAdvance: stripe.Bool(true)}
			params.SetIdempotencyKey("tuition:" + o.Reference + ":finalize")
			armed, err = s.client.V1Invoices.FinalizeInvoice(ctx, inv.ID, params)
		}
		if err != nil {
			return err
		}
		status = armed.Status
	}
	return s.saveObligation(ctx, l, o.ID, map[string]any{
		"scheduled_finalization_at": *o.DueAt,
		"latest_stripe_status":      string(status),
	})
}

// sendFee finalizes the fee invoice with automatic advancement, then emails it, each call under
// a stable key. The stage is recorded only afterwards, so a lost response retries under the
// same key instead of creating another fee.
// ponytail: past the keys' 24h retention a retried send would email again; the fee is never duplicated.
func (s *Service) sendFee(ctx context.Context, l lease, o *Obligation, inv *stripe.Invoice) error {
	status := inv.Status
	if status == stripe.InvoiceStatusDraft {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		params := &stripe.InvoiceFinalizeInvoiceParams{AutoAdvance: stripe.Bool(true)}
		params.SetIdempotencyKey("reinstatement:" + o.Reference + ":finalize")
		finalized, err := s.client.V1Invoices.FinalizeInvoice(ctx, inv.ID, params)
		if err != nil {
			return err
		}
		status = finalized.Status
	}
	if status == stripe.InvoiceStatusOpen {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		params := &stripe.InvoiceSendInvoiceParams{}
		params.SetIdempotencyKey("reinstatement:" + o.Reference + ":send")
		if _, err := s.client.V1Invoices.SendInvoice(ctx, inv.ID, params); err != nil {
			return err
		}
	}
	return s.saveObligation(ctx, l, o.ID, map[string]any{
		"scheduled_finalization_at": time.Now().UTC(),
		"latest_stripe_status":      string(status),
	})
}

func (s *Service) saveObligation(ctx context.Context, l lease, id uint64, updates map[string]any) error {
	return s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
		return tx.Model(&Obligation{}).Where("id = ?", id).Updates(updates).Error
	})
}

// reviewObligation persists a bounded reason before any provider action and always returns
// errReviewed on success, which stops that obligation's workflow without a retry.
func (s *Service) reviewObligation(ctx context.Context, l lease, o *Obligation, reason string) error {
	if err := s.saveObligation(ctx, l, o.ID, map[string]any{"review_reason": reason}); err != nil {
		return err
	}
	o.ReviewReason = &reason
	s.log.LogAttrs(ctx, slog.LevelWarn, "billing_review_required",
		slog.String("obligation", o.Reference), slog.String("reason", reason))
	s.changed()
	return errReviewed
}

// ensureDefaultPaymentMethod makes the first payment's card the Customer's invoice default
// before any invoice is armed. An already selected default is never overwritten, which also
// makes crash recovery safe. ok is false when the method cannot pay future invoices.
func (s *Service) ensureDefaultPaymentMethod(ctx context.Context, l lease, c Contract) (ok bool, err error) {
	customer, err := s.client.V1Customers.Retrieve(ctx, *c.StripeCustomerID, nil)
	if err != nil {
		return false, err
	}
	if is := customer.InvoiceSettings; is != nil && is.DefaultPaymentMethod != nil && is.DefaultPaymentMethod.ID != "" {
		return true, s.mark(ctx, l, reviewUnsupportedPaymentMethod, false)
	}
	var first Payment
	if err := require(firstRow(s.db.WithContext(ctx).Order("payment_time ASC"), &first, "contract_id = ?", c.ID)); err != nil {
		return false, err
	}
	pi, err := s.client.V1PaymentIntents.Retrieve(ctx, first.ID, nil)
	if err != nil {
		return false, err
	}
	if pi.PaymentMethod == nil || pi.PaymentMethod.ID == "" {
		return false, s.mark(ctx, l, reviewUnsupportedPaymentMethod, true)
	}
	pm, err := s.client.V1PaymentMethods.Retrieve(ctx, pi.PaymentMethod.ID, nil)
	if err != nil {
		return false, err
	}
	if pm.Type != stripe.PaymentMethodTypeCard || pm.Customer == nil || pm.Customer.ID != *c.StripeCustomerID {
		return false, s.mark(ctx, l, reviewUnsupportedPaymentMethod, true)
	}
	if err := s.renew(ctx, l); err != nil {
		return false, err
	}
	params := &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{DefaultPaymentMethod: stripe.String(pm.ID)},
	}
	params.SetIdempotencyKey("tuition:" + c.Reference + ":default_pm")
	if _, err := s.client.V1Customers.Update(ctx, *c.StripeCustomerID, params); err != nil {
		return false, err
	}
	return true, s.mark(ctx, l, reviewUnsupportedPaymentMethod, false)
}

// ---- contract reconciliation

// syncContract reconciles one due contract under its workflow lease.
func (s *Service) syncContract(ctx context.Context, c Contract) {
	err := s.withContract(ctx, c.ID, func(l lease) error {
		err := s.reconcileContract(ctx, l)
		if err == nil {
			err = s.markSynced(ctx, l)
		}
		if err != nil && ctx.Err() == nil { // retry later; token-guarded so a lost lease writes nothing
			saveCtx, cancel := persistCtx(ctx)
			defer cancel()
			if res := s.db.WithContext(saveCtx).Model(&Contract{}).Where("id = ? AND lease_token = ?", l.id, l.token).
				Update("next_sync_at", time.Now().UTC().Add(contractRetry)); res.Error != nil {
				s.logFailure(ctx, "contract_retry", c.Reference, res.Error)
			}
		}
		return err
	})
	if err != nil && !errors.Is(err, errContractBusy) && ctx.Err() == nil {
		s.logFailure(ctx, "contract_sync", c.Reference, err)
	}
}

// reconcileContract refreshes everything Stripe owns for one contract: in-flight Checkout
// attempts, the later invoices, and every invoice with its payments, charges, refunds and
// disputes. The same pass is the daily audit that finds events nobody delivered.
// Last it applies the school's deadline rules to what Stripe just reported: suspension,
// cleanup of corrected fees and reactivation.
// ponytail: every sync re-reads all of the contract's invoices (at most six); narrow it to
// changed invoices if per-contract request volume ever matters.
func (s *Service) reconcileContract(ctx context.Context, l lease) error {
	db := s.db.WithContext(ctx)
	var attempts []CheckoutAttempt
	if err := db.Where("contract_id = ? AND state IN ?", l.id, activeAttemptStates).
		Order("id ASC").Find(&attempts).Error; err != nil {
		return err
	}
	for _, a := range attempts {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		if a.StripeSessionID == nil {
			if err := s.resumeCreate(ctx, l, a); err != nil {
				return err
			}
			continue
		}
		session, err := s.retrieveSession(ctx, *a.StripeSessionID)
		if err != nil {
			return err
		}
		if err := s.syncAttempt(ctx, l, a.ID, session); err != nil {
			return err
		}
	}

	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	if err := s.syncCustomerContact(ctx, l, c); err != nil {
		return err
	}
	if c.FundedAt == nil {
		return nil
	}
	if err := s.scheduleInvoices(ctx, l); err != nil {
		return err
	}
	var obligations []Obligation
	if err := db.Where("contract_id = ? AND stripe_invoice_id IS NOT NULL", l.id).Order("number ASC").Find(&obligations).Error; err != nil {
		return err
	}
	for _, o := range obligations {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		if err := s.syncInvoice(ctx, l, o.ID); err != nil {
			return err
		}
	}
	if err := s.evaluateDeadlines(ctx, l); err != nil {
		return err
	}
	if err := s.cancelFees(ctx, l); err != nil {
		return err
	}
	return s.evaluateReactivation(ctx, l)
}

// contactOf returns the details a contract bills against: the Enrollment's until payment is confirmed, the Student's after.
func contactOf(db *gorm.DB, c Contract) (students.Details, error) {
	if c.FundedAt == nil {
		var link links.Enrollment
		err := require(firstRow(db, &link, "id = ?", c.EnrollmentID))
		return link.Details, err
	}
	var student students.Student
	err := require(firstRow(db, &student, "enrollment_id = ?", c.EnrollmentID))
	return student.Details, err
}

// syncCustomerContact updates Stripe's customer name and email when they differ from the last synced stamp.
func (s *Service) syncCustomerContact(ctx context.Context, l lease, c Contract) error {
	if c.StripeCustomerID == nil {
		return nil
	}
	contact, err := contactOf(s.db.WithContext(ctx), c)
	if err != nil {
		return err
	}
	if contact.Name == c.SyncedCustomerName && contact.Email == c.SyncedCustomerEmail {
		return nil
	}
	if err := s.renew(ctx, l); err != nil {
		return err
	}
	_, err = s.client.V1Customers.Update(ctx, *c.StripeCustomerID, &stripe.CustomerUpdateParams{
		Name:  stripe.String(contact.Name),
		Email: stripe.String(contact.Email),
	})
	if err != nil {
		return err
	}
	return s.leased(ctx, l, func(tx *gorm.DB, cur *Contract) error {
		return tx.Model(&Contract{}).Where("id = ?", cur.ID).Updates(map[string]any{
			"synced_customer_name":  contact.Name,
			"synced_customer_email": contact.Email,
		}).Error
	})
}

// markSynced records a finished contract sync and schedules the next one. The worker calls it
// after every reconcile; RequestReinstatement and ReconcileStudent call it under their staff
// lease so the next scheduled sync accounts for what the action created.
func (s *Service) markSynced(ctx context.Context, l lease) error {
	ctx, cancel := persistCtx(ctx)
	defer cancel()
	now := time.Now().UTC()
	next, err := s.nextSync(ctx, l.id, now)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&Contract{}).Where("id = ? AND lease_token = ?", l.id, l.token).
		Updates(map[string]any{"last_synced_at": now, "next_sync_at": next}).Error
}

// nextSync loads what nextSyncAt needs.
func (s *Service) nextSync(ctx context.Context, id uint64, now time.Time) (time.Time, error) {
	db := s.db.WithContext(ctx)
	var c Contract
	var obligations []Obligation
	var pays []Payment
	var inFlight int64
	err := errors.Join(
		require(firstRow(db, &c, "id = ?", id)),
		db.Where("contract_id = ?", id).Find(&obligations).Error,
		db.Where("contract_id = ?", id).Find(&pays).Error,
		db.Model(&CheckoutAttempt{}).Where("contract_id = ? AND state IN ?", id, activeAttemptStates).Count(&inFlight).Error,
	)
	if err != nil {
		return time.Time{}, err
	}
	return nextSyncAt(now, c, obligations, pays, inFlight > 0), nil
}

// nextSyncAt picks the earliest of: the daily audit; a quarter-hour recheck while a Checkout
// is in flight, tuition is overdue, an open fee (no DueAt, amount remaining) awaits payment,
// or a payment still lacks fee/net data; the nearest unsettled installment date; and its
// not-yet-handled 14-day deadline.
func nextSyncAt(now time.Time, c Contract, obligations []Obligation, pays []Payment, inFlight bool) time.Time {
	next := now.Add(auditEvery)
	soonest := func(t time.Time) {
		if t.Before(next) {
			next = t
		}
	}
	soon := now.Add(activeSyncEvery)
	if inFlight {
		soonest(soon)
	}
	if c.FundedAt == nil {
		return next
	}
	for _, o := range obligations {
		if o.AmountRemainingCents == 0 {
			continue
		}
		if o.DueAt == nil || !o.DueAt.After(now) {
			soonest(soon)
		} else {
			soonest(*o.DueAt)
		}
		if o.DueAt != nil && o.DeadlineEpisodeID == nil {
			if deadline := o.DueAt.Add(deadlineWindow); deadline.After(now) {
				soonest(deadline)
			}
		}
	}
	for _, p := range pays {
		if p.BalanceTransactionID == nil {
			soonest(soon)
		}
	}
	return next
}

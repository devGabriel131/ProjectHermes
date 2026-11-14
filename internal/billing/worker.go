package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/logging"
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

// claimEvent leases the oldest due event, skipping events another worker holds.
func (s *Service) claimEvent(ctx context.Context) (e WebhookEvent, token string, found bool, err error) {
	token = rand.Text()
	res := s.db.WithContext(ctx).Raw(`
UPDATE billing_webhook_events SET lease_token = ?, lease_expires_at = `+leaseSQL+`, updated_at = now()
 WHERE id = (SELECT id FROM billing_webhook_events WHERE `+eventsDue+` ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *`, token).Scan(&e)
	return e, token, res.RowsAffected == 1, res.Error
}

// drainEvents runs up to eventBatch due events, each claimed just before it runs, so no lease
// ages while earlier events are handled.
func (s *Service) drainEvents(ctx context.Context) {
	for range eventBatch {
		if ctx.Err() != nil {
			return
		}
		e, token, found, err := s.claimEvent(ctx)
		if err != nil {
			s.logFailure(ctx, "event_claim", "", err)
			return
		}
		if !found {
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
	if err := s.db.WithContext(saveCtx).Model(&WebhookEvent{}).Where("id = ? AND lease_token = ?", e.ID, token).Updates(updates).Error; err != nil {
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
		return s.syncSession(ctx, e.ResourceID)

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

	case e.EventType == "payment_intent.payment_failed":
		// Checkout keeps its Session open after a decline, so no checkout.session event follows;
		// the Session behind this PaymentIntent records it. Invoice PaymentIntents have none.
		var sessionID string
		params := &stripe.CheckoutSessionListParams{PaymentIntent: stripe.String(e.ResourceID)}
		for session, err := range s.client.V1CheckoutSessions.List(ctx, params).All(ctx) {
			if err != nil {
				return ignoreMissing(err)
			}
			sessionID = session.ID
		}
		if sessionID == "" {
			return nil
		}
		return s.syncSession(ctx, sessionID)
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

// syncSession refreshes the attempt behind a Checkout Session from Stripe.
func (s *Service) syncSession(ctx context.Context, sessionID string) error {
	session, err := s.retrieveSession(ctx, sessionID)
	if err != nil {
		return ignoreMissing(err)
	}
	var attempt CheckoutAttempt
	// An unbound attempt is matched by the opaque reference sent as client_reference_id.
	found, err := firstRow(s.db.WithContext(ctx), &attempt, "stripe_session_id = ? OR (stripe_session_id IS NULL AND reference = ?)", session.ID, session.ClientReferenceID)
	if err != nil || !found {
		return err
	}
	return s.withContract(ctx, attempt.ContractID, func(l lease) error {
		return s.syncAttempt(ctx, l, attempt.ID, session)
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

// leased runs fn in a transaction holding the Enrollment then the Contract row lock, only while l still owns
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

// leasedStudent is leased plus the Student row lock, ordered Enrollment, Contract, then Student.
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
	return true, tx.Model(&Contract{}).Where("id = ?", c.ID).Update("review_reason", c.ReviewReason).Error
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
		var enrollment links.Enrollment
		err := require(firstRow(db, &enrollment, "id = ?", c.EnrollmentID))
		return enrollment.Details, err
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

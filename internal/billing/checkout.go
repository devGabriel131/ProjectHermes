package billing

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
)

// integrationIdentifier is Stripe's non-unique grouping label for this integration's Checkout
// Sessions; a constant keeps idempotent create replays byte-identical.
const integrationIdentifier = "tuition_checkout"

// activeAttemptStates are the nonterminal states guarded by idx_active_checkout_attempt.
var activeAttemptStates = []string{"creating", "open", "processing", "expiring", "uncertain"}

var errCheckoutMismatch = errors.New("billing: checkout session does not match its attempt")

// CheckoutResult returns the outcome of initiating or resuming a checkout attempt.
type CheckoutResult struct {
	URL     string
	Pending bool
	Funded  bool
}

// checkoutStep is what one start transaction decided.
type checkoutStep struct {
	done     *CheckoutResult  // set when no provider call remains
	expire   *CheckoutAttempt // another plan's open attempt, claimed as "expiring"
	changed  bool             // the contract's visible quote changed
	contract Contract
	first    Obligation
	attempt  CheckoutAttempt // new "creating" attempt
}

// StartCheckout creates or resumes the single hosted first-payment attempt for
// enrollmentID. Before funding it may switch plans, but only after the previous
// plan's open Session is confirmed expired.
func (s *Service) StartCheckout(ctx context.Context, enrollmentID uint64, planID string) (CheckoutResult, error) {
	plan, ok := payments.Find(planID)
	if !ok {
		return CheckoutResult{}, ErrUnknownPlan
	}
	callCtx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()

	// Each pass claims at most one stale open attempt; a competing start can only
	// create another by winning a race, and callCtx bounds the retries.
	for {
		step, err := s.beginCheckout(callCtx, enrollmentID, plan)
		if err != nil {
			return CheckoutResult{}, err
		}
		if step.changed {
			s.changed()
		}
		if step.expire != nil {
			expired, err := s.expireAttempt(callCtx, step.expire)
			if err != nil {
				return CheckoutResult{}, err
			}
			if !expired {
				return CheckoutResult{Pending: true}, nil
			}
			continue
		}
		if step.done != nil {
			return *step.done, nil
		}
		return s.createSession(callCtx, step.contract, step.first, step.attempt)
	}
}

// beginCheckout runs the locked decision transaction: Enrollment, then Contract, then attempt.
// It persists the new attempt's identity before any Checkout call is made.
func (s *Service) beginCheckout(ctx context.Context, enrollmentID uint64, plan payments.Plan) (step checkoutStep, err error) {
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var enrollment links.Enrollment
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &enrollment,
			"id = ? AND submitted_at IS NOT NULL AND terms_accepted_at IS NOT NULL", enrollmentID)); err != nil {
			return err
		}
		var contract Contract
		var columns []string // contract columns this start changed, written once below
		exists, err := firstRow(tx.Clauses(forUpdate), &contract, "enrollment_id = ?", enrollmentID)
		if err != nil {
			return err
		}
		if !exists {
			syncGrace := time.Now().UTC().Add(1 * time.Minute)
			contract = Contract{
				EnrollmentID:     enrollmentID,
				Reference:        rand.Text(),
				PlanID:           plan.ID,
				PlanName:         plan.Name,
				InstallmentCount: len(plan.Amounts),
				TotalCents:       plan.TotalCents(),
				NextSyncAt:       &syncGrace,
			}
			if err := tx.Create(&contract).Error; err != nil {
				return err
			}
			if err := s.createObligations(tx, contract.ID, plan); err != nil {
				return err
			}
			step.changed = true
		} else if contract.FundedAt != nil {
			if contract.PlanID != plan.ID {
				return ErrPlanLocked
			}
			step.done = &CheckoutResult{Funded: true}
			return nil
		} else {
			var attempt CheckoutAttempt
			active, err := firstRow(tx.Clauses(forUpdate), &attempt, "contract_id = ? AND state IN ?", contract.ID, activeAttemptStates)
			if err != nil {
				return err
			}
			if active {
				open := attempt.State == "open"
				switch {
				case open && attempt.PlanID == plan.ID && attempt.CheckoutURL != nil:
					step.done = &CheckoutResult{URL: *attempt.CheckoutURL}
				case open && attempt.PlanID != plan.ID && attempt.StripeSessionID != nil:
					if err := claimExpiringAttempt(tx, &attempt); err != nil {
						return err
					}
					step.expire = &attempt
				default:
					step.done = &CheckoutResult{Pending: true}
				}
				return nil
			}
			if contract.PlanID != plan.ID {
				// Every earlier attempt is terminal, so no Session can still collect the old quote.
				// Unfunded contracts have no provider invoices, so the old obligations are plain rows.
				if err := tx.Where("contract_id = ? AND kind = 'tuition'", contract.ID).Delete(&Obligation{}).Error; err != nil {
					return err
				}
				contract.PlanID, contract.PlanName = plan.ID, plan.Name
				contract.InstallmentCount, contract.TotalCents = len(plan.Amounts), plan.TotalCents()
				columns = append(columns, "plan_id", "plan_name", "installment_count", "total_cents")
				if err := s.createObligations(tx, contract.ID, plan); err != nil {
					return err
				}
				step.changed = true
			}
		}

		var first Obligation
		if err := require(firstRow(tx, &first, "contract_id = ? AND kind = 'tuition' AND number = 1", contract.ID)); err != nil {
			return err
		}

		if contract.StripeCustomerID == nil {
			// ponytail: the Enrollment and Contract locks stay held across this once-per-enrollment
			// Stripe call so concurrent starts share one Customer; the stable key makes a retry
			// safe, move it after commit if lock waits ever matter.
			params := &stripe.CustomerCreateParams{
				Name:             stripe.String(enrollment.Name),
				Email:            stripe.String(enrollment.Email),
				PreferredLocales: []*string{stripe.String(stripeLocale)},
				Metadata:         map[string]string{"tuition_contract": contract.Reference},
			}
			params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:customer", contract.Reference))
			customer, err := s.client.V1Customers.Create(ctx, params)
			if err != nil {
				return fmt.Errorf("billing: create stripe customer: %w", err)
			}
			contract.StripeCustomerID = &customer.ID
			contract.SyncedCustomerName, contract.SyncedCustomerEmail = enrollment.Name, enrollment.Email
			columns = append(columns, "stripe_customer_id", "synced_customer_name", "synced_customer_email")
		}

		attempt := CheckoutAttempt{
			ContractID:        contract.ID,
			Reference:         rand.Text(),
			PlanID:            plan.ID,
			FirstAmountCents:  first.AmountCents,
			StripePriceID:     first.StripePriceID,
			CreationStartedAt: time.Now().UTC(),
			State:             "creating",
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		if len(columns) > 0 {
			// Select, never Save: Save would also rewrite the lease, sync and review columns other workflows own.
			if err := tx.Select(columns).Updates(&contract).Error; err != nil {
				return err
			}
		}
		step.contract, step.first, step.attempt = contract, first, attempt
		return nil
	})
	return step, err
}

// claimExpiringAttempt marks an open attempt "expiring" inside the caller's locking transaction,
// so no other start or cancel acts on its Session while expireAttempt runs.
func claimExpiringAttempt(tx *gorm.DB, attempt *CheckoutAttempt) error {
	if err := tx.Model(&CheckoutAttempt{}).Where("id = ?", attempt.ID).Update("state", "expiring").Error; err != nil {
		return err
	}
	attempt.State = "expiring"
	return nil
}

// expireAttempt expires the claimed open Session of an abandoned plan. expired is true only
// once the Session is terminally unpaid; otherwise the caller reports the checkout as pending.
// Unresolved outcomes stay "expiring" for the worker, never unlocking a possibly paid quote.
func (s *Service) expireAttempt(ctx context.Context, attempt *CheckoutAttempt) (expired bool, err error) {
	session, err := s.client.V1CheckoutSessions.Expire(ctx, *attempt.StripeSessionID, nil)
	if err != nil {
		// Sessions that already ended reject Expire; their current state decides.
		session, err = s.client.V1CheckoutSessions.Retrieve(ctx, *attempt.StripeSessionID, nil)
	}
	if err == nil {
		saveCtx, cancel := persistCtx(ctx)
		defer cancel()
		switch {
		case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid || session.Status == stripe.CheckoutSessionStatusComplete:
			return false, s.setAttempt(saveCtx, attempt.ID, "expiring", "processing", nil)
		case session.Status == stripe.CheckoutSessionStatusExpired:
			return true, s.setAttempt(saveCtx, attempt.ID, "expiring", "expired", nil)
		}
	}
	s.log.LogAttrs(ctx, slog.LevelWarn, "checkout_expire_unresolved",
		append(logging.ErrorAttrs(err), slog.String("attempt", attempt.Reference))...)
	return false, nil
}

// createSession calls Stripe for the already persisted attempt and records the outcome.
func (s *Service) createSession(ctx context.Context, contract Contract, first Obligation, attempt CheckoutAttempt) (CheckoutResult, error) {
	session, err := s.client.V1CheckoutSessions.Create(ctx, s.sessionParams(contract, first, attempt))
	// Record the outcome even if the browser went away or the budget just ran out.
	saveCtx, cancel := persistCtx(ctx)
	defer cancel()

	if err != nil {
		if outcomeUnknown(err) {
			// A failed write leaves the attempt as it was; the worker resumes it the same way.
			_ = s.setAttempt(saveCtx, attempt.ID, attempt.State, "uncertain", nil)
			s.log.LogAttrs(ctx, slog.LevelWarn, "checkout_create_uncertain",
				append(logging.ErrorAttrs(err), slog.String("attempt", attempt.Reference))...)
			return CheckoutResult{Pending: true}, nil
		}
		return CheckoutResult{}, errors.Join(
			fmt.Errorf("billing: checkout session rejected: %w", err),
			s.setAttempt(saveCtx, attempt.ID, attempt.State, "failed", nil))
	}

	if !sessionMatches(contract, attempt, session) || session.URL == "" {
		// The URL is never exposed, so nobody can pay this Session; keep its ID for staff and webhooks.
		return CheckoutResult{}, errors.Join(errCheckoutMismatch,
			s.setAttempt(saveCtx, attempt.ID, attempt.State, "failed", map[string]any{"stripe_session_id": session.ID}))
	}

	if err := s.setAttempt(saveCtx, attempt.ID, attempt.State, "open", map[string]any{
		"stripe_session_id": session.ID,
		"checkout_url":      session.URL,
	}); err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{URL: session.URL}, nil
}

// sessionParams builds the hosted Checkout request from persisted attempt values only.
func (s *Service) sessionParams(contract Contract, first Obligation, attempt CheckoutAttempt) *stripe.CheckoutSessionCreateParams {
	metadata := map[string]string{
		"tuition_contract":   contract.Reference,
		"tuition_obligation": first.Reference,
		"tuition_attempt":    attempt.Reference,
	}
	base := strings.TrimSuffix(s.cfg.BaseURL, "/")
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                  stripe.String(string(stripe.CheckoutSessionModePayment)),
		Currency:              stripe.String("usd"),
		Customer:              stripe.String(*contract.StripeCustomerID),
		ClientReferenceID:     stripe.String(attempt.Reference),
		IntegrationIdentifier: stripe.String(integrationIdentifier),
		Locale:                stripe.String(stripeLocale),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Price:    stripe.String(attempt.StripePriceID),
			Quantity: stripe.Int64(1),
		}},
		InvoiceCreation: &stripe.CheckoutSessionCreateInvoiceCreationParams{
			Enabled:     stripe.Bool(true),
			InvoiceData: &stripe.CheckoutSessionCreateInvoiceCreationInvoiceDataParams{Metadata: metadata},
		},
		AdaptivePricing: &stripe.CheckoutSessionCreateAdaptivePricingParams{Enabled: stripe.Bool(false)},
		SuccessURL:      stripe.String(base + "/payments/return?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:       stripe.String(base + "/payments/return?attempt=" + attempt.Reference),
	}
	if contract.InstallmentCount > 1 {
		// Later invoices charge the saved card, so installment plans take cards only; Apple Pay,
		// Google Pay and Link ride on card. Stripe rejects an allow-list next to a payment method
		// configuration, so these Sessions skip it and the extra methods it enables (Klarna).
		params.AllowedPaymentMethodTypes = stripe.StringSlice([]string{"card"})
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			SetupFutureUsage: stripe.String(string(stripe.PaymentIntentSetupFutureUsageOffSession)),
			Metadata:         metadata,
		}
		params.CustomText = &stripe.CheckoutSessionCreateCustomTextParams{
			Submit: &stripe.CheckoutSessionCreateCustomTextSubmitParams{
				Message: stripe.String("Primer pago de matrícula. Los pagos restantes se procesarán automáticamente en las fechas acordadas."),
			},
		}
	} else {
		params.PaymentMethodConfiguration = stripe.String(s.cfg.PaymentMethodConfigurationID)
	}
	params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:checkout", attempt.Reference))
	return params
}

// createObligations quotes one tuition obligation per installment with its catalog price.
func (s *Service) createObligations(tx *gorm.DB, contractID uint64, plan payments.Plan) error {
	obligations := make([]Obligation, len(plan.Amounts))
	for i, amount := range plan.Amounts {
		price := s.catalog[priceKey(plan.ID, i)]
		if price == "" {
			return fmt.Errorf("billing: catalog has no price for plan %q installment %d", plan.ID, i+1)
		}
		obligations[i] = Obligation{
			ContractID:            contractID,
			Reference:             rand.Text(),
			Kind:                  "tuition",
			Number:                i + 1,
			AmountCents:           amount,
			AmountRemainingCents:  amount,
			StripePriceID:         price,
			CollectionDisposition: "collect",
		}
	}
	return tx.Create(&obligations).Error
}

// setAttempt moves an attempt between states; from keeps a newer state from being overwritten.
func (s *Service) setAttempt(ctx context.Context, id uint64, from, to string, extra map[string]any) error {
	updates := map[string]any{"state": to}
	maps.Copy(updates, extra)
	return s.db.WithContext(ctx).Model(&CheckoutAttempt{}).Where("id = ? AND state = ?", id, from).Updates(updates).Error
}

// outcomeUnknown reports whether Stripe may have acted despite err: transport failures,
// timeouts, cancellation, 5xx, rate limits and in-flight idempotency conflicts.
func outcomeUnknown(err error) bool {
	var stripeErr *stripe.Error
	if !errors.As(err, &stripeErr) {
		return true
	}
	code := stripeErr.HTTPStatusCode
	rejected := code >= http.StatusBadRequest && code < http.StatusInternalServerError &&
		code != http.StatusTooManyRequests && code != http.StatusConflict
	return !rejected
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
	if pi := session.PaymentIntent; pi != nil && pi.LastPaymentError != nil {
		if err := s.recordDecline(ctx, l, a, pi.LastPaymentError); err != nil {
			return err
		}
	}

	switch {
	case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid:
		if c.FundedAt == nil {
			return s.fund(ctx, l, c, a, session)
		}
		return nil // a replay after funding: the funding commit left the contract due, so its sync schedules invoices
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

const (
	// checkoutKeyRetention is how long a Checkout create may be replayed under its original
	// idempotency key; Stripe may prune keys after 24 hours.
	checkoutKeyRetention = 23 * time.Hour
	// creatingGrace leaves a fresh "creating" attempt to the request that inserted it: that request
	// spends at most providerBudget on Stripe, then persistBudget recording the outcome; 2 s covers commit lag.
	creatingGrace = providerBudget + persistBudget + 2*time.Second
)

// resumeCreate settles an attempt whose Session ID was never recorded (browser gone, timeout,
// crash). A Session already created for it is adopted by client_reference_id; otherwise the
// create is replayed under the same idempotency key while Stripe still honors it. Past that
// window the consistent list proves no Session exists, so the attempt fails and frees the contract.
func (s *Service) resumeCreate(ctx context.Context, l lease, a CheckoutAttempt) error {
	if a.State == "creating" && time.Since(a.CreationStartedAt) < creatingGrace {
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

// recordDecline stores the latest failed try's Stripe code; Checkout keeps an open Session
// payable after a decline, so the attempt's state is left alone.
func (s *Service) recordDecline(ctx context.Context, l lease, a CheckoutAttempt, e *stripe.Error) error {
	code := cmp.Or(string(e.DeclineCode), string(e.Code), string(e.Type))
	if code == deref(a.DeclineCode) {
		return nil
	}
	err := s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
		return tx.Model(&CheckoutAttempt{}).Where("id = ?", a.ID).Update("decline_code", code).Error
	})
	if err == nil {
		s.changed() // live refresh of staff lists
	}
	return err
}

// fund records the first payment once the Session is paid and its generated invoice confirms
// it. Every provider read happens before the single funding transaction, and FundedAt is anchored
// to the invoice's paid_at, never a local clock. Funding leaves next_sync_at NULL, so the contract
// sync that follows (in the same worker pass, or the caller's reconcileContract) schedules the later invoices.
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
	return nil
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

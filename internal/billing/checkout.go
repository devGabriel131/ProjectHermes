package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
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
	if !validID(enrollmentID) {
		return CheckoutResult{}, ErrUnavailable
	}
	plan, ok := payments.Find(planID)
	if !ok {
		return CheckoutResult{}, payments.ErrUnknownPlan
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
		var link links.Enrollment
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &link,
			"id = ? AND submitted_at IS NOT NULL AND terms_accepted_at IS NOT NULL", enrollmentID)); err != nil {
			return err
		}
		var contract Contract
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
				if err := tx.Model(&Contract{}).Where("id = ?", contract.ID).Updates(map[string]any{
					"plan_id":           plan.ID,
					"plan_name":         plan.Name,
					"installment_count": len(plan.Amounts),
					"total_cents":       plan.TotalCents(),
				}).Error; err != nil {
					return err
				}
				contract.PlanID, contract.PlanName = plan.ID, plan.Name
				contract.InstallmentCount, contract.TotalCents = len(plan.Amounts), plan.TotalCents()
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

		contractUpdates := map[string]any{}
		if contract.StripeCustomerID == nil {
			// ponytail: the Link and Contract locks stay held across this once-per-enrollment
			// Stripe call so concurrent starts share one Customer; the stable key makes a retry
			// safe, move it after commit if lock waits ever matter.
			params := &stripe.CustomerCreateParams{
				Name:             stripe.String(link.Name),
				Email:            stripe.String(link.Email),
				PreferredLocales: []*string{stripe.String(stripeLocale)},
				Metadata:         map[string]string{"tuition_contract": contract.Reference},
			}
			params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:customer", contract.Reference))
			customer, err := s.client.V1Customers.Create(ctx, params)
			if err != nil {
				return fmt.Errorf("billing: create stripe customer: %w", err)
			}
			contract.StripeCustomerID = &customer.ID
			contract.SyncedCustomerName = link.Name
			contract.SyncedCustomerEmail = link.Email
			contractUpdates["stripe_customer_id"] = customer.ID
			contractUpdates["synced_customer_name"] = link.Name
			contractUpdates["synced_customer_email"] = link.Email
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
		if len(contractUpdates) > 0 {
			if err := tx.Model(&Contract{}).Where("id = ?", contract.ID).Updates(contractUpdates).Error; err != nil {
				return err
			}
		}
		step.contract, step.first, step.attempt = contract, first, attempt
		return nil
	})
	return step, err
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
		CancelURL:       stripe.String(base + "/payments/return?cancelled=1"),
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

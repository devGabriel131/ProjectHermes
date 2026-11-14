package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
)

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
// payment_review on an obligation is invoice-derived too, since only invoiceStateOf sets it, so a
// withdrawal can clear it.
func reviewFor(o Obligation, computed string) string {
	switch current := deref(o.ReviewReason); current {
	case "", reviewInvoiceMismatch, reviewMissingInvoice, reviewPaymentReview:
		return computed
	default:
		return current
	}
}

// invoiceStateOf maps the current invoice onto the quoted obligation. Drafts owe the quoted
// cents; void or uncollectible invoices never forgive signed tuition; credits and off-Stripe
// settlement are kept apart from Stripe cash. A cancelled invoice that was paid anyway is a
// review case, unless the contract is withdrawn: the withdrawal refunds it.
func invoiceStateOf(o Obligation, inv *stripe.Invoice, withdrawn bool) (st invoiceState, settledAt time.Time) {
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
		if o.CollectionDisposition == "cancel" && !withdrawn {
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
	var c Contract
	if err := require(firstRow(s.db.WithContext(ctx), &c, "id = ?", l.id)); err != nil {
		return err
	}
	st, settledAt := invoiceStateOf(o, inv, c.WithdrawnAt != nil)
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
			switch {
			case !st.Failed:
				updates["next_retry_at"] = nil
			case !read.o.FailureOrActionRequired: // newly failed: schedule the first retry
				updates["next_retry_at"] = s.firstRetryAt(read.o, time.Now().UTC())
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
		// Refunds, disputes and fraud warnings are surfaced for staff; a withdrawn contract's refunds are
		// its own, so only disputes and fraud warnings flag it.
		var flagged int64
		if err := tx.Model(&Payment{}).
			Where("contract_id = ? AND ((? AND gross_refunded_cents > 0) OR dispute_or_fraud_review_state IS NOT NULL)", c.ID, c.WithdrawnAt == nil).
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
			slog.String("contract", contractRef), slog.String("obligation", obligationRef), slog.String("reason", read.st.Review))
	}
	if changed {
		s.changed()
	}
	return nil
}

// nullable maps "" to SQL NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// ---- cancellation (corrected fees, withdrawn contracts)

// cancelPending reports whether the obligation's invoice still has to be cancelled at Stripe:
// its collection is cancelled and an invoice exists or may exist, and it is not already void,
// deleted or paid.
func (o Obligation) cancelPending() bool {
	return o.CollectionDisposition == "cancel" && (o.StripeInvoiceID != nil || o.InvoiceCreationStartedAt != nil) &&
		!slices.Contains([]string{"void", "deleted", "paid"}, deref(o.LatestStripeStatus))
}

// cancelObligations finishes the Stripe side of every obligation whose collection was cancelled:
// fees of corrected suspension episodes and each unsettled obligation of a withdrawn contract.
// The intent is already persisted as CollectionDisposition=cancel, so every retry only continues cancelling.
func (s *Service) cancelObligations(ctx context.Context, l lease) error {
	db := s.db.WithContext(ctx)
	var cancelled []Obligation
	if err := db.Where("contract_id = ? AND collection_disposition = 'cancel'", l.id).
		Order("id ASC").Find(&cancelled).Error; err != nil {
		return err
	}
	cancelled = slices.DeleteFunc(cancelled, func(o Obligation) bool { return !o.cancelPending() })
	if len(cancelled) == 0 {
		return nil
	}
	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	for i := range cancelled {
		if err := s.cancelObligation(ctx, l, c, &cancelled[i]); err != nil {
			return err
		}
	}
	return nil
}

// cancelObligation deletes a draft invoice or voids a finalized unpaid one. A paid one is left
// alone: a withdrawal refunds it, otherwise syncInvoice flags it for staff refund review. Nothing
// is ever recharged here, and an uncertain create is only located so it can be cancelled, never
// finished or sent.
// ponytail: an uncertain create that never shows at the provider is re-listed every sync; add
// a terminal marker if that ever matters.
func (s *Service) cancelObligation(ctx context.Context, l lease, c Contract, o *Obligation) error {
	if err := s.renew(ctx, l); err != nil {
		return err
	}
	if o.StripeInvoiceID == nil {
		if c.StripeCustomerID == nil {
			return nil
		}
		found, err := s.invoicesOf(ctx, *c.StripeCustomerID, o.Reference)
		if err != nil {
			return err
		}
		id, err := s.adopt(ctx, l, o, found)
		if errors.Is(err, errReviewed) {
			return nil // several candidates: staff review, never cancel one blindly
		}
		if err != nil || id == "" {
			return err
		}
		if err := s.saveObligation(ctx, l, o.ID, map[string]any{"stripe_invoice_id": id}); err != nil {
			return err
		}
		o.StripeInvoiceID = &id
	}
	status := "deleted"
	inv, err := s.client.V1Invoices.Retrieve(ctx, *o.StripeInvoiceID, nil)
	switch {
	case isNotFound(err), err == nil && inv.Deleted:
	case err != nil:
		return err
	case inv.Status == stripe.InvoiceStatusDraft:
		if _, err := s.client.V1Invoices.Delete(ctx, inv.ID, nil); err != nil && !isNotFound(err) {
			return err
		}
	case inv.Status == stripe.InvoiceStatusOpen || inv.Status == stripe.InvoiceStatusUncollectible:
		if _, err := s.client.V1Invoices.VoidInvoice(ctx, inv.ID, nil); err != nil {
			return err
		}
		status = "void"
	default: // paid or already void: syncInvoice reports what remains
		return nil
	}
	return s.saveObligation(ctx, l, o.ID, map[string]any{"latest_stripe_status": status, "amount_remaining_cents": 0})
}

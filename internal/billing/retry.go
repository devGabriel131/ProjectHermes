package billing

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
)

// ---- installment retries
//
// Stripe makes the first attempt when an armed invoice finalizes. When that attempt fails the app pays
// the same invoice again after retryDelay, at most maxRetries times, so a card that was briefly short of
// funds or hit an issuer hiccup is collected without the payer doing anything. This is the one place the
// app itself charges a card; Stripe's own Smart Retries, if enabled, run independently.

const (
	maxRetries        = 2
	defaultRetryDelay = 24 * time.Hour
)

// retryableDeclines are the soft declines a later attempt can clear. Everything else (lost or stolen
// card, expired card, authentication required, no reason on record) needs the payer, so it is never
// charged again off-session.
var retryableDeclines = []string{
	"generic_decline", "insufficient_funds", "card_velocity_exceeded",
	"processing_error", "try_again_later", "issuer_not_available",
}

func (s *Service) retryDelay() time.Duration { return cmp.Or(s.cfg.RetryDelay, defaultRetryDelay) }

// firstRetryAt is when a newly failed installment is paid again; nil when no retry applies.
func (s *Service) firstRetryAt(o Obligation, now time.Time) any {
	if o.Kind != "tuition" || o.RetryCount >= maxRetries {
		return nil
	}
	return now.Add(s.retryDelay())
}

// retryFailed pays again every failed, unreviewed installment whose retry is due.
func (s *Service) retryFailed(ctx context.Context, l lease) error {
	var due []Obligation
	if err := s.db.WithContext(ctx).Where("contract_id = ? AND kind = 'tuition' AND stripe_invoice_id IS NOT NULL "+
		"AND failure_or_action_required AND review_reason IS NULL AND retry_count < ? AND next_retry_at <= now()",
		l.id, maxRetries).Order("number ASC").Find(&due).Error; err != nil {
		return err
	}
	for _, o := range due {
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		if err := s.retryInvoice(ctx, l, o); err != nil {
			return err
		}
	}
	return nil
}

// retryInvoice pays one failed installment again unless its last decline needs the payer. The attempt
// is counted only after Stripe answered, so a crash or an unknown outcome replays the same idempotency
// key instead of charging twice; an invoice already paid cannot be paid again.
func (s *Service) retryInvoice(ctx context.Context, l lease, o Obligation) error {
	code, err := s.lastDecline(ctx, *o.StripeInvoiceID)
	if err != nil {
		return err
	}
	retried := slices.Contains(retryableDeclines, code)
	count, next := maxRetries, any(nil) // a decline only the payer can fix ends the retries
	if retried {
		params := &stripe.InvoicePayParams{}
		params.SetIdempotencyKey(fmt.Sprintf("tuition:%s:retry:%d", o.Reference, o.RetryCount+1))
		// Stripe's answer, a decline included, consumes the attempt; the sync below reads the outcome.
		if _, err := s.client.V1Invoices.Pay(ctx, *o.StripeInvoiceID, params); err != nil && outcomeUnknown(err) {
			return err
		}
		count = o.RetryCount + 1
		if count < maxRetries {
			next = time.Now().UTC().Add(s.retryDelay())
		}
	}
	var contractRef string
	err = s.leased(ctx, l, func(tx *gorm.DB, c *Contract) error {
		contractRef = c.Reference
		return tx.Model(&Obligation{}).Where("id = ?", o.ID).Updates(map[string]any{"retry_count": count, "next_retry_at": next}).Error
	})
	if err != nil {
		return err
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "installment_retry", slog.String("contract", contractRef),
		slog.String("obligation", o.Reference), slog.Int("attempt", o.RetryCount+1), slog.String("decline", code), slog.Bool("retried", retried))
	return s.syncInvoice(ctx, l, o.ID)
}

// lastDecline is the Stripe code of the invoice's latest failed payment try, "" when none is on record.
func (s *Service) lastDecline(ctx context.Context, invoiceID string) (string, error) {
	params := &stripe.InvoicePaymentListParams{Invoice: stripe.String(invoiceID)}
	params.AddExpand("data.payment.payment_intent")
	for ip, err := range s.client.V1InvoicePayments.List(ctx, params).All(ctx) {
		if err != nil {
			return "", err
		}
		if ip.Payment != nil && ip.Payment.PaymentIntent != nil && ip.Payment.PaymentIntent.LastPaymentError != nil {
			return declineCode(ip.Payment.PaymentIntent.LastPaymentError), nil
		}
	}
	return "", nil
}

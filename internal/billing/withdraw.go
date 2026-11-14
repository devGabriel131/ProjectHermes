package billing

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"projecthermes/internal/students"
)

// Withdraw ends a funded contract for staff, irreversibly: it records the intent (cancel every
// unsettled obligation, close any open episode, deactivate and mark the student withdrawn), then runs
// the contract sync, which voids or deletes those invoices and refunds every undisputed payment in full.
// Repeating it resumes unfinished cancels and refunds and never changes who withdrew or when.
func (s *Service) Withdraw(ctx context.Context, studentID uint64, staff string) (*Summary, error) {
	if strings.TrimSpace(staff) == "" {
		return nil, errStaffRequired
	}
	ctx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return nil, err
	}
	if c.FundedAt == nil {
		return nil, ErrUnavailable
	}
	err = s.withStaffContract(ctx, c.ID, func(l lease) error {
		if err := s.withdraw(ctx, l, staff); err != nil {
			return err
		}
		if err := s.reconcileContract(ctx, l); err != nil {
			return err
		}
		return s.markSynced(ctx, l)
	})
	if err != nil {
		return nil, err
	}
	return s.ForStudent(ctx, studentID)
}

// withdraw persists the withdrawal in one transaction. A contract already withdrawn is left
// untouched, so the first withdrawal's author and time are kept.
func (s *Service) withdraw(ctx context.Context, l lease, staff string) error {
	var ref string
	var did bool
	err := s.leasedStudent(ctx, l, func(tx *gorm.DB, c *Contract, student *students.Student) error {
		if c.WithdrawnAt != nil {
			return nil
		}
		ref, did = c.Reference, true
		now := time.Now().UTC()
		// next_sync_at: if the staff request dies after this commit, the worker picks the contract up at once.
		if err := tx.Model(&Contract{}).Where("id = ?", c.ID).
			Updates(map[string]any{"withdrawn_at": now, "withdrawn_by": staff, "next_sync_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Obligation{}).Where("contract_id = ? AND first_settled_at IS NULL", c.ID).
			Updates(map[string]any{"collection_disposition": "cancel", "next_retry_at": nil}).Error; err != nil {
			return err
		}
		// close_reason stays NULL: the contract's withdrawn_at explains the closure.
		if err := tx.Model(&Suspension{}).Where("contract_id = ? AND closed_at IS NULL", c.ID).
			Update("closed_at", now).Error; err != nil {
			return err
		}
		return tx.Model(&students.Student{}).Where("id = ?", student.ID).
			Updates(map[string]any{"is_active": false, "withdrawn_at": now}).Error
	})
	if err != nil || !did {
		return err
	}
	s.changed()
	s.log.LogAttrs(ctx, slog.LevelInfo, "student_withdrawn", slog.String("contract", ref))
	return nil
}

// refundPayments gives back, in full, what each undisputed payment of a withdrawn contract still
// holds. Stripe refuses any amount above what is left, so a stale read can never refund twice;
// a failed refund is retried by the next sync.
// ponytail: a refund that fails at the bank within 24h reuses its key, so it is re-sent only once the key expires.
func (s *Service) refundPayments(ctx context.Context, l lease) error {
	db := s.db.WithContext(ctx)
	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	var payments []Payment
	if err := db.Where("contract_id = ?", l.id).Order("payment_time ASC, id ASC").Find(&payments).Error; err != nil {
		return err
	}
	for _, p := range payments {
		amount := p.refundableCents()
		if amount <= 0 {
			continue
		}
		if err := s.renew(ctx, l); err != nil {
			return err
		}
		params := &stripe.RefundCreateParams{
			PaymentIntent: stripe.String(p.ID),
			Amount:        stripe.Int64(amount),
			Reason:        stripe.String(string(stripe.RefundReasonRequestedByCustomer)),
		}
		params.AddMetadata("contract", c.Reference)
		params.AddMetadata("withdrawn_by", deref(c.WithdrawnBy))
		params.SetIdempotencyKey("withdrawal:" + c.Reference + ":" + p.ID + ":" + strconv.FormatInt(p.GrossRefundedCents, 10))
		_, err := s.client.V1Refunds.Create(ctx, params)
		switch {
		case err == nil:
			s.log.LogAttrs(ctx, slog.LevelInfo, "withdrawal_refund",
				slog.String("contract", c.Reference), slog.Int64("amount_cents", amount))
		case !refundRefused(err):
			return err
		}
		if err := s.syncPayment(ctx, l, p.ID); err != nil {
			return err
		}
	}
	return nil
}

// refundRefused reports a refund Stripe refuses because nothing is left to give back: the charge
// is already fully refunded or disputed.
func refundRefused(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) &&
		(stripeErr.Code == stripe.ErrorCodeChargeAlreadyRefunded || stripeErr.Code == stripe.ErrorCodeChargeDisputed)
}

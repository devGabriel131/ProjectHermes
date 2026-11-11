package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"projecthermes/internal/students"
)

// deadlineWindow is how long a due tuition installment may stay unpaid before the student is suspended.
const deadlineWindow = 14 * 24 * time.Hour

// reinstatable returns the contract's open episode with its contract and obligations, or
// ErrNotSuspended when none is open, or ErrNeedsReview while any review reason is unresolved.
func (s *Service) reinstatable(ctx context.Context, contractID uint64) (ep Suspension, c Contract, obligations []Obligation, err error) {
	db := s.db.WithContext(ctx)
	found, err := firstRow(db, &ep, "contract_id = ? AND closed_at IS NULL", contractID)
	if err != nil {
		return
	}
	if !found {
		err = ErrNotSuspended
		return
	}
	if err = require(firstRow(db, &c, "id = ?", contractID)); err != nil {
		return
	}
	if err = db.Where("contract_id = ?", contractID).Find(&obligations).Error; err != nil {
		return
	}
	if hasReview(c, obligations) {
		err = ErrNeedsReview
	}
	return
}

var errStaffRequired = errors.New("staff identity is required")

// RequestReinstatement creates, or resumes, the open episode's single 2500-cent fee invoice for
// staff. Original overdue invoices are refreshed first and are never duplicated onto the fee.
func (s *Service) RequestReinstatement(ctx context.Context, studentID uint64, staff string) (*Summary, error) {
	if strings.TrimSpace(staff) == "" {
		return nil, errStaffRequired
	}
	ctx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return nil, err
	}
	// Refuse early, before any provider work; the decision is repeated after the refresh.
	if _, _, _, err := s.reinstatable(ctx, c.ID); err != nil {
		return nil, err
	}
	err = s.withStaffContract(ctx, c.ID, func(l lease) error {
		if err := s.reconcileContract(ctx, l); err != nil {
			return err
		}
		if err := s.requestFee(ctx, l, staff); err != nil {
			return err
		}
		return s.markSynced(ctx, l)
	})
	if err != nil {
		return nil, err
	}
	return s.ForStudent(ctx, studentID)
}

// requestFee reuses the episode's fee obligation or inserts it, runs the persisted invoice
// stages, then records who requested it first. A repeated request resumes where one stopped
// and never changes RequestedBy or the request time.
func (s *Service) requestFee(ctx context.Context, l lease, staff string) error {
	ep, c, obligations, err := s.reinstatable(ctx, l.id)
	if err != nil {
		return err
	}
	if c.StripeCustomerID == nil {
		return ErrUnavailable
	}
	var fee Obligation
	if i := slices.IndexFunc(obligations, func(o Obligation) bool {
		return o.Kind == "reinstatement" && o.SuspensionID != nil && *o.SuspensionID == ep.ID
	}); i >= 0 {
		fee = obligations[i]
	} else {
		price := s.catalog[reinstatementKey]
		if price == "" {
			return errors.New("billing: catalog has no reinstatement price")
		}
		fee = Obligation{
			ContractID:            c.ID,
			Reference:             rand.Text(),
			Kind:                  "reinstatement",
			AmountCents:           ReinstatementFeeCents,
			StripePriceID:         price,
			SuspensionID:          &ep.ID,
			CollectionDisposition: "collect",
			AmountRemainingCents:  ReinstatementFeeCents,
		}
		if err := s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error { return tx.Create(&fee).Error }); err != nil {
			return err
		}
	}
	if err := s.scheduleOne(ctx, l, c, &fee); err != nil {
		if errors.Is(err, errReviewed) {
			return ErrNeedsReview
		}
		return err
	}
	err = s.leased(ctx, l, func(tx *gorm.DB, _ *Contract) error {
		return tx.Model(&Suspension{}).Where("id = ? AND reinstatement_requested_at IS NULL", ep.ID).
			Updates(map[string]any{"reinstatement_requested_at": time.Now().UTC(), "requested_by": staff}).Error
	})
	if err == nil {
		s.changed()
	}
	return err
}

// missedDeadline reports whether a tuition installment missed its 14-day window: still unpaid,
// or first fully settled after the deadline. DueAt must be set.
func missedDeadline(o Obligation) bool {
	return o.AmountRemainingCents > 0 || (o.FirstSettledAt != nil && o.FirstSettledAt.After(o.DueAt.Add(deadlineWindow)))
}

// hasReview reports whether the contract or any of its obligations still awaits staff review.
func hasReview(c Contract, obligations []Obligation) bool {
	return c.ReviewReason != nil || slices.ContainsFunc(obligations, func(o Obligation) bool { return o.ReviewReason != nil })
}

// episodeFor picks the episode a missed deadline belongs to: one whose started-to-closed
// interval contains it, otherwise the current open one, otherwise none.
func episodeFor(episodes []Suspension, deadline time.Time) *Suspension {
	var open *Suspension
	for i := range episodes {
		e := &episodes[i]
		if e.ClosedAt == nil {
			open = e
		}
		if !e.StartedAt.After(deadline) && (e.ClosedAt == nil || !deadline.After(*e.ClosedAt)) {
			return e
		}
	}
	return open
}

// falseEpisode reports whether an episode's triggering installment is authoritatively settled
// on time, which proves the suspension was detected falsely.
func falseEpisode(e Suspension, tuition []Obligation) bool {
	i := slices.IndexFunc(tuition, func(o Obligation) bool { return o.Number == e.TriggeringTuitionInstallmentNumber })
	return i >= 0 && tuition[i].FirstSettledAt != nil && !missedDeadline(tuition[i])
}

// correctEpisode closes a falsely detected episode, releases its deadline attributions and
// persists the cancel intent on its fee, all before cancelFees touches Stripe.
func correctEpisode(tx *gorm.DB, id uint64, now time.Time) error {
	if err := tx.Model(&Suspension{}).Where("id = ?", id).Updates(map[string]any{
		"close_reason": "corrected",
		"closed_at":    gorm.Expr("COALESCE(closed_at, ?)", now),
	}).Error; err != nil {
		return err
	}
	if err := tx.Model(&Obligation{}).Where("deadline_episode_id = ?", id).Update("deadline_episode_id", nil).Error; err != nil {
		return err
	}
	return tx.Model(&Obligation{}).Where("kind = 'reinstatement' AND suspension_id = ?", id).Update("collection_disposition", "cancel").Error
}

// evaluateDeadlines applies the 14-day rule to a funded contract whose invoices were just
// refreshed, in one decision under leasedStudent's Link, Contract and Student locks. Episodes whose triggering
// installment is proven on time are corrected first; then every missed deadline is attributed
// once to the episode covering it or the open one, else it opens a new episode and deactivates
// the student. Late installments of one continuous suspension share one episode, hence one fee.
func (s *Service) evaluateDeadlines(ctx context.Context, l lease) error {
	var changed bool
	err := s.leasedStudent(ctx, l, func(tx *gorm.DB, c *Contract, student *students.Student) error {
		now := time.Now().UTC()
		var tuition []Obligation
		if err := tx.Where("contract_id = ? AND kind = 'tuition' AND due_at IS NOT NULL", c.ID).
			Order("due_at ASC, number ASC").Find(&tuition).Error; err != nil {
			return err
		}
		var episodes []Suspension
		if err := tx.Where("contract_id = ? AND (close_reason IS NULL OR close_reason <> 'corrected')", c.ID).
			Order("started_at ASC, id ASC").Find(&episodes).Error; err != nil {
			return err
		}

		var corrected, correctedOpen, created bool
		live := episodes[:0]
		for _, e := range episodes {
			if !falseEpisode(e, tuition) {
				live = append(live, e)
				continue
			}
			if err := correctEpisode(tx, e.ID, now); err != nil {
				return err
			}
			for i := range tuition {
				if o := &tuition[i]; o.DeadlineEpisodeID != nil && *o.DeadlineEpisodeID == e.ID {
					o.DeadlineEpisodeID = nil
				}
			}
			corrected, correctedOpen = true, correctedOpen || e.ClosedAt == nil
		}

		for i := range tuition {
			o := &tuition[i]
			deadline := o.DueAt.Add(deadlineWindow)
			if deadline.After(now) || o.DeadlineEpisodeID != nil || !missedDeadline(*o) {
				continue
			}
			ep := episodeFor(live, deadline)
			if ep == nil {
				fresh := Suspension{
					ContractID:                         c.ID,
					TriggeringTuitionInstallmentNumber: o.Number,
					StartedAt:                          deadline,
				}
				if err := tx.Create(&fresh).Error; err != nil {
					return err
				}
				live = append(live, fresh)
				ep, created = &live[len(live)-1], true
			}
			if err := tx.Model(&Obligation{}).Where("id = ?", o.ID).Update("deadline_episode_id", ep.ID).Error; err != nil {
				return err
			}
		}

		// A corrected open episode reactivates unless another valid one remains; a new episode
		// deactivates; evaluateReactivation flips it back after an approved reinstatement.
		open := slices.ContainsFunc(live, func(e Suspension) bool { return e.ClosedAt == nil })
		if (created || correctedOpen) && student.IsActive == open {
			if err := tx.Model(&students.Student{}).Where("id = ?", student.ID).Update("is_active", !open).Error; err != nil {
				return err
			}
		}
		changed = created || corrected
		return nil
	})
	if err == nil && changed {
		s.changed()
	}
	return err
}

// cancelFees finishes the Stripe side of fees whose episode was corrected. The intent is
// already persisted as CollectionDisposition=cancel, so every retry only continues cancelling.
func (s *Service) cancelFees(ctx context.Context, l lease) error {
	db := s.db.WithContext(ctx)
	var fees []Obligation
	if err := db.Where("contract_id = ? AND kind = 'reinstatement' AND collection_disposition = 'cancel' "+
		"AND (stripe_invoice_id IS NOT NULL OR invoice_creation_started_at IS NOT NULL) "+
		"AND COALESCE(latest_stripe_status, '') NOT IN ('void', 'deleted', 'paid')", l.id).
		Order("id ASC").Find(&fees).Error; err != nil || len(fees) == 0 {
		return err
	}
	var c Contract
	if err := require(firstRow(db, &c, "id = ?", l.id)); err != nil {
		return err
	}
	for i := range fees {
		if err := s.cancelFee(ctx, l, c, &fees[i]); err != nil {
			return err
		}
	}
	return nil
}

// cancelFee deletes a draft fee invoice or voids a finalized unpaid one. A paid fee is left
// for staff refund review (syncInvoice flags it); nothing is ever recharged or refunded here,
// and an uncertain create is only located so it can be cancelled, never finished or sent.
// ponytail: an uncertain create that never shows at the provider is re-listed every sync; add
// a terminal marker if that ever matters.
func (s *Service) cancelFee(ctx context.Context, l lease, c Contract, o *Obligation) error {
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

// evaluateReactivation reinstates the student exactly once, under leasedStudent's Link, Contract
// and Student locks, when the open episode's requested fee is confirmed settled, every currently due
// installment is settled and no review is unresolved.
func (s *Service) evaluateReactivation(ctx context.Context, l lease) error {
	var changed bool
	err := s.leasedStudent(ctx, l, func(tx *gorm.DB, c *Contract, student *students.Student) error {
		var ep Suspension
		if found, err := firstRow(tx, &ep, "contract_id = ? AND closed_at IS NULL AND reinstatement_requested_at IS NOT NULL", c.ID); err != nil || !found {
			return err
		}
		var obligations []Obligation
		if err := tx.Where("contract_id = ?", c.ID).Find(&obligations).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		var feeSettled bool
		for _, o := range obligations {
			switch {
			case o.Kind == "reinstatement" && o.SuspensionID != nil && *o.SuspensionID == ep.ID:
				feeSettled = o.CollectionDisposition == "collect" && o.AmountRemainingCents == 0 && o.FirstSettledAt != nil
			case o.Kind == "tuition" && o.DueAt != nil && !o.DueAt.After(now) && o.AmountRemainingCents > 0:
				return nil
			}
		}
		if !feeSettled || hasReview(*c, obligations) {
			return nil
		}
		if err := tx.Model(&Suspension{}).Where("id = ?", ep.ID).
			Updates(map[string]any{"closed_at": now, "close_reason": "reinstated"}).Error; err != nil {
			return err
		}
		if err := tx.Model(&students.Student{}).Where("id = ?", student.ID).Update("is_active", true).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err == nil && changed {
		s.changed()
	}
	return err
}

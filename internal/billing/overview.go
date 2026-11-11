package billing

import (
	"context"
	"database/sql"
	"time"
)

// Overview is the business-wide billing standing staff scan on the dashboard.
// Tuition figures count only funded contracts: an abandoned Checkout leaves unpaid
// obligations that nobody owes yet. Overdue, failed and in-review follow the same rules
// as a student's Summary, so the totals add up to what each student's billing shows.
// Cash is gross of Stripe fees and net of refunds.
type Overview struct {
	CollectedCents      int64
	OutstandingCents    int64 // tuition still unpaid, due or not
	OverdueCents        int64
	OverdueInstallments int
	DueSoonCents        int64 // tuition falling due within the window after now
	DueSoonInstallments int
	FailedInstallments  int // a failed charge or customer action blocks payment
	ContractsInReview   int // a contract or installment awaits staff review
	PaymentsInReview    int // disputes and fraud warnings
	FundedRecently      int // contracts funded within the window before now
}

// Overview totals billing at now. The window looks back for newly funded contracts
// and ahead for installments coming due.
func (s *Service) Overview(ctx context.Context, now time.Time, window time.Duration) (Overview, error) {
	var o Overview
	db := s.db.WithContext(ctx)
	at := sql.Named("now", now)
	until := sql.Named("until", now.Add(window))
	since := sql.Named("since", now.Add(-window))

	if err := db.Raw(`
		SELECT
			COALESCE(SUM(o.amount_remaining_cents), 0)::bigint,
			COALESCE(SUM(o.amount_remaining_cents) FILTER (WHERE o.due_at < @now), 0)::bigint,
			count(*) FILTER (WHERE o.due_at < @now AND o.amount_remaining_cents > 0),
			COALESCE(SUM(o.amount_remaining_cents) FILTER (WHERE o.due_at >= @now AND o.due_at < @until), 0)::bigint,
			count(*) FILTER (WHERE o.due_at >= @now AND o.due_at < @until AND o.amount_remaining_cents > 0),
			count(*) FILTER (WHERE o.failure_or_action_required AND o.first_settled_at IS NULL AND o.amount_remaining_cents > 0)
		FROM billing_obligations o
		JOIN billing_contracts c ON c.id = o.contract_id
		WHERE o.kind = 'tuition' AND c.funded_at IS NOT NULL`, at, until).Row().Scan(
		&o.OutstandingCents, &o.OverdueCents, &o.OverdueInstallments,
		&o.DueSoonCents, &o.DueSoonInstallments, &o.FailedInstallments); err != nil {
		return Overview{}, err
	}
	if err := db.Raw(`
		SELECT
			COALESCE(SUM(gross_received_cents - gross_refunded_cents), 0)::bigint,
			count(*) FILTER (WHERE COALESCE(dispute_or_fraud_review_state, '') <> '')
		FROM billing_payments`).Row().Scan(&o.CollectedCents, &o.PaymentsInReview); err != nil {
		return Overview{}, err
	}
	if err := db.Raw(`
		SELECT
			count(*) FILTER (WHERE c.funded_at >= @since),
			count(*) FILTER (WHERE c.review_reason IS NOT NULL OR EXISTS (
				SELECT 1 FROM billing_obligations o WHERE o.contract_id = c.id AND o.review_reason IS NOT NULL))
		FROM billing_contracts c`, since).Row().Scan(&o.FundedRecently, &o.ContractsInReview); err != nil {
		return Overview{}, err
	}
	return o, nil
}

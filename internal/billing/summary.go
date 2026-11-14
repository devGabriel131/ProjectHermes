package billing

import (
	"context"
	"slices"
	"time"

	"projecthermes/internal/students"
)

// ForEnrollment returns the summary for enrollmentID, or nil before checkout creates its contract.
func (s *Service) ForEnrollment(ctx context.Context, enrollmentID uint64) (*Summary, error) {
	if !validID(enrollmentID) {
		return nil, ErrUnavailable
	}
	var contract Contract
	found, err := firstRow(s.db.WithContext(ctx), &contract, "enrollment_id = ?", enrollmentID)
	if err != nil || !found {
		return nil, err
	}
	var student students.Student
	hasStudent, err := firstRow(s.db.WithContext(ctx), &student, "enrollment_id = ?", enrollmentID)
	if err != nil {
		return nil, err
	}
	return s.summarize(ctx, contract, hasStudent && student.IsActive)
}

// ForStudent returns the financial summary, or nil when the student or contract does not exist.
func (s *Service) ForStudent(ctx context.Context, studentID uint64) (*Summary, error) {
	if !validID(studentID) {
		return nil, nil
	}
	var student students.Student
	if found, err := firstRow(s.db.WithContext(ctx), &student, "id = ?", studentID); err != nil || !found {
		return nil, err
	}
	if student.EnrollmentID == nil {
		return nil, nil
	}
	var contract Contract
	if found, err := firstRow(s.db.WithContext(ctx), &contract, "enrollment_id = ?", *student.EnrollmentID); err != nil || !found {
		return nil, err
	}
	return s.summarize(ctx, contract, student.IsActive)
}

func (s *Service) summarize(ctx context.Context, contract Contract, active bool) (*Summary, error) {
	var obligations []Obligation
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Order("number ASC").Find(&obligations).Error; err != nil {
		return nil, err
	}
	var payments []Payment
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Find(&payments).Error; err != nil {
		return nil, err
	}
	var suspension Suspension
	suspended, err := firstRow(s.db.WithContext(ctx), &suspension, "contract_id = ? AND closed_at IS NULL", contract.ID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	summary := &Summary{
		PlanID:       contract.PlanID,
		PlanName:     contract.PlanName,
		TotalCents:   contract.TotalCents,
		Funded:       contract.FundedAt != nil,
		FundedAt:     contract.FundedAt,
		IsActive:     active,
		IsSuspended:  suspended,
		LastSyncedAt: contract.LastSyncedAt,
	}
	for _, p := range payments {
		summary.CashPaidCents += p.GrossReceivedCents - p.GrossRefundedCents
	}
	addReason := func(reason *string) {
		if reason != nil && !slices.Contains(summary.ReviewReasons, *reason) {
			summary.ReviewReasons = append(summary.ReviewReasons, *reason)
		}
	}
	addReason(contract.ReviewReason)
	for _, o := range obligations {
		summary.OtherSettlementCents += o.OtherSettlementCents
		addReason(o.ReviewReason)
		overdue := o.DueAt != nil && o.DueAt.Before(now)
		switch o.Kind {
		case "reinstatement":
			if o.CollectionDisposition == "collect" { // a corrected fee is no longer owed
				summary.ReinstatementFeeRemainingCents += o.AmountRemainingCents
				if o.ID > summary.ReinstatementFeeID { // the newest fee belongs to the current episode
					summary.ReinstatementFeeID, summary.ReinstatementInvoiceReady = o.ID, o.invoiceReady()
				}
			}
		case "tuition":
			summary.RemainingTuitionCents += o.AmountRemainingCents
			summary.TuitionPaidCents += o.AmountPaidCents
			if overdue {
				summary.OverdueTuitionCents += o.AmountRemainingCents
			}
			status := "scheduled"
			switch {
			case o.FirstSettledAt != nil || o.AmountRemainingCents == 0:
				status = "paid"
			case o.FailureOrActionRequired:
				status = "action_required"
			case overdue:
				status = "overdue"
			}
			summary.Installments = append(summary.Installments, SummaryInstallment{
				ID:           o.ID,
				Number:       o.Number,
				AmountCents:  o.AmountCents,
				DueAt:        o.DueAt,
				Status:       status,
				PaidAt:       o.FirstSettledAt,
				InvoiceReady: o.invoiceReady(),
			})
		}
	}
	return summary, nil
}

// invoiceReady reports whether the obligation has a finalized Stripe invoice with a hosted page.
func (o Obligation) invoiceReady() bool {
	return o.StripeInvoiceID != nil && !slices.Contains([]string{"", "draft", "deleted"}, deref(o.LatestStripeStatus))
}

// ReturnStatus maps a Checkout session to a customer-safe outcome. It exposes only status and
// amount, never student identity, contact, or payment history.
func (s *Service) ReturnStatus(ctx context.Context, sessionID string) (ReturnStatus, error) {
	if sessionID == "" {
		return ReturnStatus{}, ErrUnavailable
	}
	var attempt CheckoutAttempt
	found, err := firstRow(s.db.WithContext(ctx), &attempt, "stripe_session_id = ?", sessionID)
	if err != nil {
		return ReturnStatus{}, err
	}
	if !found {
		return ReturnStatus{}, ErrUnavailable
	}
	status := "pending"
	switch attempt.State {
	case "paid":
		status = "paid"
	case "expired", "failed":
		status = "not_completed"
	}
	return ReturnStatus{Status: status, AmountCents: attempt.FirstAmountCents}, nil
}

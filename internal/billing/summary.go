package billing

import (
	"context"
	"slices"
	"time"

	"projecthermes/internal/students"
)

// SummaryInstallment represents the status of one tuition installment in Summary.
type SummaryInstallment struct {
	ID           uint64
	Number       int
	AmountCents  int64
	DueAt        *time.Time
	Status       string
	PaidAt       *time.Time
	InvoiceReady bool
}

// SummaryPayment is one received payment in Summary, with where staff can refund part of it.
type SummaryPayment struct {
	PaidAt                     time.Time
	AmountCents, RefundedCents int64
	Review                     string // dispute or fraud state, "" when none
	DashboardURL               string
}

// Summary exposes financial standing, suspension and review reasons.
type Summary struct {
	PlanID                         string
	PlanName                       string
	TotalCents                     int64
	Funded                         bool
	FundedAt                       *time.Time
	CashPaidCents                  int64
	TuitionPaidCents               int64
	OtherSettlementCents           int64
	RemainingTuitionCents          int64
	OverdueTuitionCents            int64
	ReinstatementFeeRemainingCents int64
	ReinstatementFeeID             uint64
	ReinstatementInvoiceReady      bool
	Installments                   []SummaryInstallment
	IsSuspended                    bool
	WithdrawnAt                    *time.Time
	WithdrawnBy                    string
	RefundableCents                int64 // undisputed cash a withdrawal refunds, or still has to
	Payments                       []SummaryPayment
	LastSyncedAt                   *time.Time
	ReviewReasons                  []string
	Decline                        string // set while the unfunded contract's newest attempt ended its latest try in a decline; same groups as ReturnStatus.Decline
}

// ReturnStatus holds customer-facing status returned from Stripe-hosted checkout.
// Contains only payment status, amount, and the decline reason group; never leaks student identity, contact, or history.
type ReturnStatus struct {
	Status      string
	AmountCents int64
	Decline     string // reason group when Status is declined: funds, card_details, authentication or generic
}

// ForEnrollment returns the summary for enrollmentID, or nil before checkout creates its contract.
func (s *Service) ForEnrollment(ctx context.Context, enrollmentID uint64) (*Summary, error) {
	var contract Contract
	found, err := firstRow(s.db.WithContext(ctx), &contract, "enrollment_id = ?", enrollmentID)
	if err != nil || !found {
		return nil, err
	}
	return s.summarize(ctx, contract)
}

// ForStudent returns the financial summary, or nil when the student or contract does not exist.
func (s *Service) ForStudent(ctx context.Context, studentID uint64) (*Summary, error) {
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
	return s.summarize(ctx, contract)
}

func (s *Service) summarize(ctx context.Context, contract Contract) (*Summary, error) {
	var obligations []Obligation
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Order("number ASC").Find(&obligations).Error; err != nil {
		return nil, err
	}
	var payments []Payment
	if err := s.db.WithContext(ctx).Where("contract_id = ?", contract.ID).Order("payment_time ASC, id ASC").Find(&payments).Error; err != nil {
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
		IsSuspended:  suspended,
		LastSyncedAt: contract.LastSyncedAt,
		WithdrawnAt:  contract.WithdrawnAt,
		WithdrawnBy:  deref(contract.WithdrawnBy),
	}
	for _, p := range payments {
		summary.CashPaidCents += p.GrossReceivedCents - p.GrossRefundedCents
		summary.RefundableCents += p.refundableCents()
		summary.Payments = append(summary.Payments, SummaryPayment{
			PaidAt:        p.PaymentTime,
			AmountCents:   p.GrossReceivedCents,
			RefundedCents: p.GrossRefundedCents,
			Review:        deref(p.DisputeOrFraudReviewState),
			DashboardURL:  s.dashboardURL(p.ID),
		})
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
			summary.TuitionPaidCents += o.AmountPaidCents
			status := "scheduled"
			switch {
			case o.CollectionDisposition == "cancel" && o.FirstSettledAt == nil:
				status = "cancelled" // a withdrawal stopped it: never remaining or overdue
			case o.FirstSettledAt != nil || o.AmountRemainingCents == 0:
				status = "paid"
			case o.FailureOrActionRequired:
				status = "action_required"
			case overdue:
				status = "overdue"
			}
			if status != "cancelled" {
				summary.RemainingTuitionCents += o.AmountRemainingCents
				if overdue {
					summary.OverdueTuitionCents += o.AmountRemainingCents
				}
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
	if contract.FundedAt == nil {
		var last CheckoutAttempt
		found, err := firstRow(s.db.WithContext(ctx).Order("id DESC"), &last, "contract_id = ?", contract.ID)
		if err != nil {
			return nil, err
		}
		if found && last.declined() {
			summary.Decline = declineReason(*last.DeclineCode)
		}
	}
	return summary, nil
}

// invoiceReady reports whether the obligation has a finalized Stripe invoice with a hosted page.
func (o Obligation) invoiceReady() bool {
	return o.StripeInvoiceID != nil && !slices.Contains([]string{"", "draft", "deleted"}, deref(o.LatestStripeStatus))
}

// dashboardURL is where staff open one PaymentIntent in the Stripe Dashboard, for refunds the app does not make.
func (s *Service) dashboardURL(paymentIntentID string) string {
	if s.cfg.Live {
		return "https://dashboard.stripe.com/payments/" + paymentIntentID
	}
	return "https://dashboard.stripe.com/test/payments/" + paymentIntentID
}

// ReturnStatus maps the Checkout Session Stripe redirected to after completion (success_url) to a
// customer-safe outcome.
func (s *Service) ReturnStatus(ctx context.Context, sessionID string) (ReturnStatus, error) {
	return s.attemptOutcome(ctx, "stripe_session_id = ?", sessionID, false)
}

// CancelStatus maps the attempt whose Checkout back link (cancel_url) the customer followed to a
// customer-safe outcome.
func (s *Service) CancelStatus(ctx context.Context, attemptRef string) (ReturnStatus, error) {
	return s.attemptOutcome(ctx, "reference = ?", attemptRef, true)
}

// attemptOutcome loads one attempt and exposes only status, amount and the decline reason group,
// never student identity, contact, or payment history.
func (s *Service) attemptOutcome(ctx context.Context, query, value string, cancelled bool) (ReturnStatus, error) {
	if value == "" {
		return ReturnStatus{}, ErrUnavailable
	}
	var a CheckoutAttempt
	found, err := firstRow(s.db.WithContext(ctx), &a, query, value)
	if err != nil {
		return ReturnStatus{}, err
	}
	if !found {
		return ReturnStatus{}, ErrUnavailable
	}
	r := ReturnStatus{Status: "pending", AmountCents: a.FirstAmountCents}
	switch {
	case a.State == "paid":
		r.Status = "paid"
	case a.State == "open" && !cancelled:
		// Checkout redirects to success_url only after completion, which the worker hasn't synced yet.
		// This guard stops a decline followed by a successful retry from showing "declined".
	case a.declined():
		r.Status, r.Decline = "declined", declineReason(*a.DeclineCode)
	case slices.Contains(idleAttemptStates, a.State):
		r.Status = "not_completed"
	}
	return r, nil
}

// idleAttemptStates are where nothing is paying: the Session waits for a try (open) or the attempt
// ended unpaid. Only here is a recorded decline the attempt's latest word.
var idleAttemptStates = []string{"open", "failed", "expired"}

// declined reports whether a's latest payment try was declined and nothing newer superseded it.
func (a CheckoutAttempt) declined() bool {
	return a.DeclineCode != nil && slices.Contains(idleAttemptStates, a.State)
}

// declineReason groups a Stripe decline or error code into what a customer can act on.
// Lost, stolen and fraud codes stay generic, as Stripe advises.
func declineReason(code string) string {
	switch code {
	case "insufficient_funds", "card_velocity_exceeded":
		return "funds"
	case "expired_card", "incorrect_cvc", "invalid_cvc", "incorrect_number", "invalid_number",
		"incorrect_zip", "invalid_expiry_month", "invalid_expiry_year":
		return "card_details"
	case "authentication_required", "payment_intent_authentication_failure":
		return "authentication"
	}
	return "generic"
}

// DeclinedEnrollments returns the enrollments whose unfunded contract's newest checkout attempt
// ended its latest try in a decline.
func (s *Service) DeclinedEnrollments(ctx context.Context) (map[uint64]bool, error) {
	var ids []uint64
	err := s.db.WithContext(ctx).Raw(`
		SELECT c.enrollment_id FROM billing_contracts c
		JOIN LATERAL (SELECT state, decline_code FROM billing_checkout_attempts
		              WHERE contract_id = c.id ORDER BY id DESC LIMIT 1) a ON true
		WHERE c.funded_at IS NULL AND a.decline_code IS NOT NULL AND a.state IN ?`, idleAttemptStates).Scan(&ids).Error
	declined := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		declined[id] = true
	}
	return declined, err
}

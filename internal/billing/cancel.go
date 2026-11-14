package billing

import (
	"context"
	"strings"

	"gorm.io/gorm"

	"projecthermes/internal/links"
)

// CancelEnrollment revokes an InProgress or AwaitingPayment enrollment or clears an Expired
// submission's PIN, guarded against funding and active Stripe Checkout Sessions.
// Any open Checkout Session is expired with Stripe first; in-flight or unresolved attempts
// return ErrCancelBlocked so no paid enrollment or held PIN can be lost.
func (s *Service) CancelEnrollment(ctx context.Context, enrollmentID uint64, staff string) (*links.EnrollmentEntry, bool, error) {
	if strings.TrimSpace(staff) == "" {
		return nil, false, errStaffRequired
	}
	callCtx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()

	for {
		var (
			expire  *CheckoutAttempt
			entry   *links.EnrollmentEntry
			changed bool
		)
		err := s.db.WithContext(callCtx).Transaction(func(tx *gorm.DB) error {
			var e links.Enrollment
			found, err := firstRow(tx.Clauses(forUpdate), &e, "id = ?", enrollmentID)
			if err != nil || !found {
				return err
			}
			var contract Contract
			hasContract, err := firstRow(tx.Clauses(forUpdate), &contract, "enrollment_id = ?", enrollmentID)
			if err != nil {
				return err
			}
			if hasContract && contract.FundedAt == nil {
				var attempt CheckoutAttempt
				active, err := firstRow(tx.Clauses(forUpdate), &attempt,
					"contract_id = ? AND state IN ?", contract.ID, activeAttemptStates)
				if err != nil {
					return err
				}
				if active {
					if attempt.State == "open" && attempt.StripeSessionID != nil {
						if err := claimExpiringAttempt(tx, &attempt); err != nil {
							return err
						}
						expire = &attempt
						return nil
					}
					return ErrCancelBlocked
				}
			}
			entry, changed, err = links.CancelEnrollmentTx(tx, enrollmentID, staff)
			return err
		})
		if err != nil {
			return nil, false, err
		}
		if expire != nil {
			expired, err := s.expireAttempt(callCtx, expire)
			if err != nil {
				return nil, false, err
			}
			if !expired {
				return nil, false, ErrCancelBlocked
			}
			continue
		}
		if changed {
			s.changed()
		}
		return entry, changed, nil
	}
}

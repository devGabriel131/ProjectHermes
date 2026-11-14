package billing

import (
	"context"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/students"
)
// touch marks the contract due for sync now.
func touch(db *gorm.DB, enrollmentID uint64) error {
	return db.Model(&Contract{}).Where("enrollment_id = ? AND stripe_customer_id IS NOT NULL", enrollmentID).Update("next_sync_at", time.Now().UTC()).Error
}

// TouchContract marks the contract behind enrollmentID due now and wakes the
// billing worker so contact updates sync promptly. Nil is a no-op for staff-added students.
func (s *Service) TouchContract(ctx context.Context, enrollmentID *uint64) {
	if enrollmentID == nil || !validID(*enrollmentID) {
		return
	}
	_ = touch(s.db.WithContext(ctx), *enrollmentID)
	s.wakeWorker()
}

// inFlightAttemptStates are the active states where money may be in transit, blocking contact edits.
var inFlightAttemptStates = []string{"processing", "uncertain", "expiring"}

// UpdateContact updates the Enrollment's contact snapshot, guarded against concurrent checkouts
// and blocked once funded (the Student is then canonical and staff-edited) or while payment is in flight.
func (s *Service) UpdateContact(ctx context.Context, enrollmentID uint64, details students.Details) error {
	if !validID(enrollmentID) {
		return ErrUnavailable
	}
	contact := map[string]any{
		"name": details.Name, "email": details.Email, "phone": details.Phone,
		"municipality": details.Municipality, "date_of_birth": details.DateOfBirth, "gender": details.Gender,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link links.Enrollment
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &link,
			"id = ? AND submitted_at IS NOT NULL", enrollmentID)); err != nil {
			return err
		}
		var contract Contract
		exists, err := firstRow(tx.Clauses(forUpdate), &contract, "enrollment_id = ?", enrollmentID)
		if err != nil {
			return err
		}
		if exists {
			if contract.FundedAt != nil {
				return ErrContactLocked
			}
			var attempt CheckoutAttempt
			inFlight, err := firstRow(tx.Clauses(forUpdate), &attempt,
				"contract_id = ? AND state IN ?", contract.ID, inFlightAttemptStates)
			if err != nil {
				return err
			}
			if inFlight {
				return ErrContactLocked
			}
			if contract.StripeCustomerID != nil {
				if err := touch(tx, enrollmentID); err != nil {
					return err
				}
			}
		}
		return tx.Model(&links.Enrollment{}).Where("id = ?", enrollmentID).Updates(contact).Error
	})
	if err != nil {
		return err
	}
	s.changed()
	s.wakeWorker()
	return nil
}

// ContactEditable reports whether the customer's contact details can still be edited.
// Once funded or while a payment is in flight (processing, uncertain, expiring), edits are locked.
func (s *Service) ContactEditable(ctx context.Context, enrollmentID uint64) (bool, error) {
	if !validID(enrollmentID) {
		return false, nil
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&Contract{}).
		Where("enrollment_id = ? AND (funded_at IS NOT NULL OR EXISTS (SELECT 1 FROM billing_checkout_attempts a WHERE a.contract_id = billing_contracts.id AND a.state IN ?))", enrollmentID, inFlightAttemptStates).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

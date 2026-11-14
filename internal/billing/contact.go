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
	if enrollmentID == nil {
		return
	}
	_ = touch(s.db.WithContext(ctx), *enrollmentID)
	s.wakeWorker()
}

// inFlightAttemptStates are the active states where money may be in transit, blocking contact edits.
var inFlightAttemptStates = []string{"processing", "uncertain", "expiring"}

// contactEditable is the contact lock: the enrollment's contact may change until its contract is funded
// (the Student is then canonical and staff-edited), except while a checkout attempt has money in flight.
func contactEditable(db *gorm.DB, enrollmentID uint64) (bool, error) {
	var locked int64
	err := db.Model(&Contract{}).
		Where("enrollment_id = ? AND (funded_at IS NOT NULL OR EXISTS (SELECT 1 FROM billing_checkout_attempts a WHERE a.contract_id = billing_contracts.id AND a.state IN ?))", enrollmentID, inFlightAttemptStates).
		Count(&locked).Error
	return locked == 0, err
}

// ContactEditable reports whether the customer's contact details can still be edited. Before the terms are
// accepted it answers without a query: checkout requires them, so no contract can exist to lock the contact.
func (s *Service) ContactEditable(ctx context.Context, e *links.Enrollment) (bool, error) {
	if e.TermsAcceptedAt == nil {
		return true, nil
	}
	return contactEditable(s.db.WithContext(ctx), e.ID)
}

// UpdateContact updates the Enrollment's contact snapshot unless the contact is locked. The Enrollment row
// lock serializes it with checkout and funding, which decide under the same lock.
func (s *Service) UpdateContact(ctx context.Context, enrollmentID uint64, details students.Details) error {
	contact := map[string]any{
		"name": details.Name, "email": details.Email, "phone": details.Phone,
		"municipality": details.Municipality, "date_of_birth": details.DateOfBirth, "gender": details.Gender,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var enrollment links.Enrollment
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &enrollment,
			"id = ? AND submitted_at IS NOT NULL", enrollmentID)); err != nil {
			return err
		}
		editable, err := contactEditable(tx, enrollmentID)
		if err != nil {
			return err
		}
		if !editable {
			return ErrContactLocked
		}
		if err := touch(tx, enrollmentID); err != nil {
			return err
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

package billing

import (
	"context"
	"crypto/sha256"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/students"
)

// touch marks the contract due for sync now.
func touch(db *gorm.DB, sourceLinkHash []byte) error {
	return db.Model(&Contract{}).Where("source_link_hash = ? AND stripe_customer_id IS NOT NULL", sourceLinkHash).Update("next_sync_at", time.Now().UTC()).Error
}

// TouchContract marks the contract behind sourceLinkHash due now and wakes the
// billing worker so contact updates sync promptly.
func (s *Service) TouchContract(ctx context.Context, sourceLinkHash []byte) {
	_ = touch(s.db.WithContext(ctx), sourceLinkHash)
	s.wakeWorker()
}

// inFlightAttemptStates are the active states where money may be in transit, blocking contact edits.
var inFlightAttemptStates = []string{"processing", "uncertain", "expiring"}

// UpdateContact updates the Link's contact snapshot, guarded against concurrent checkouts
// and blocked once funded (the Student is then canonical and staff-edited) or while payment is in flight.
func (s *Service) UpdateContact(ctx context.Context, sourceLinkHash []byte, details students.Details) error {
	if len(sourceLinkHash) != sha256.Size {
		return ErrUnavailable
	}
	contact := map[string]any{
		"name": details.Name, "email": details.Email, "phone": details.Phone,
		"municipality": details.Municipality, "date_of_birth": details.DateOfBirth, "gender": details.Gender,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link links.Link
		if err := require(firstRow(tx.Clauses(forUpdate).Scopes(links.Usable), &link,
			"token_hash = ? AND submitted_at IS NOT NULL", sourceLinkHash)); err != nil {
			return err
		}
		var contract Contract
		exists, err := firstRow(tx.Clauses(forUpdate), &contract, "source_link_hash = ?", sourceLinkHash)
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
				if err := touch(tx, sourceLinkHash); err != nil {
					return err
				}
			}
		}
		return tx.Model(&links.Link{}).Where("token_hash = ?", sourceLinkHash).Updates(contact).Error
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
func (s *Service) ContactEditable(ctx context.Context, sourceLinkHash []byte) (bool, error) {
	if len(sourceLinkHash) != sha256.Size {
		return false, nil
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&Contract{}).
		Where("source_link_hash = ? AND (funded_at IS NOT NULL OR EXISTS (SELECT 1 FROM billing_checkout_attempts a WHERE a.contract_id = billing_contracts.id AND a.state IN ?))", sourceLinkHash, inFlightAttemptStates).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

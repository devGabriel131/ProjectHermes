//go:build integration

package billing_test

import (
	"crypto/rand"
	"testing"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

func TestBillingModelsMigration(t *testing.T) {
	allModels := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	// Verify we can insert a student and contract
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	details := students.Details{
		Name:         "Juan del Pueblo",
		Email:        "juan@example.com",
		Phone:        "+17875551234",
		Municipality: "San Juan",
	}
	link := links.Enrollment{
		LinkID:      seedLink(t, db),
		SessionHash: hash,
		Details:     details,
		ExpiresAt:   time.Now().Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}
	student := students.Student{
		EnrollmentID: &link.ID,
		Details:      details,
		IsActive:     true,
		Cohort:       "2026-11",
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}

	contractRef := rand.Text()
	contract := billing.Contract{
		EnrollmentID:     link.ID,
		Reference:        contractRef,
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       62500,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}

	// Verify obligation creation
	obligation := billing.Obligation{
		ContractID:    contract.ID,
		Reference:     rand.Text(),
		Kind:          "tuition",
		Number:        1,
		AmountCents:   20834,
		StripePriceID: "price_test_three_first",
	}
	if err := db.Create(&obligation).Error; err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	// Verify checkout attempt creation with nonterminal state
	attempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "price_test_three_first",
		CreationStartedAt: time.Now().UTC(),
		State:             "creating",
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("create checkout attempt: %v", err)
	}

	// Verify partial unique index prevents second active attempt for same contract
	competingAttempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "price_test_three_first",
		CreationStartedAt: time.Now().UTC(),
		State:             "open",
	}
	if err := db.Create(&competingAttempt).Error; err == nil {
		t.Fatalf("expected error creating second nonterminal attempt on same contract, got nil")
	}

	// Verify suspension partial index
	suspension := billing.Suspension{
		ContractID:                         contract.ID,
		TriggeringTuitionInstallmentNumber: 1,
		StartedAt:                          time.Now().UTC(),
	}
	if err := db.Create(&suspension).Error; err != nil {
		t.Fatalf("create suspension: %v", err)
	}
	secondActiveSuspension := billing.Suspension{
		ContractID:                         contract.ID,
		TriggeringTuitionInstallmentNumber: 2,
		StartedAt:                          time.Now().UTC(),
	}
	if err := db.Create(&secondActiveSuspension).Error; err == nil {
		t.Fatalf("expected error creating second open suspension on same contract, got nil")
	}
}

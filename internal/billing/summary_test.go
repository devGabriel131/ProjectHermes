//go:build integration

package billing_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"projecthermes/internal/billing"
)

func TestSummaryForLinkAndStudent(t *testing.T) {
	mock := newMockStripeServer()
	db, svc, fundedHash, fundedID := setupTestEnvironment(t, mock)
	ctx := context.Background()

	fundedAt := time.Date(2026, 10, 1, 15, 4, 5, 0, time.UTC)
	contract := billing.Contract{
		SourceLinkHash:   fundedHash,
		FundedAt:         &fundedAt,
		Reference:        rand.Text(),
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       62500,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}

	sumLink, err := svc.ForLink(ctx, fundedHash)
	if err != nil || sumLink == nil {
		t.Fatalf("ForLink = %v, %v; want non-nil summary", sumLink, err)
	}
	if !sumLink.Funded || !sumLink.IsActive {
		t.Fatalf("ForLink: Funded=%v, IsActive=%v; want true, true", sumLink.Funded, sumLink.IsActive)
	}

	sumStudent, err := svc.ForStudent(ctx, fundedID)
	if err != nil || sumStudent == nil {
		t.Fatalf("ForStudent = %v, %v; want non-nil summary", sumStudent, err)
	}
	if !sumStudent.Funded || !sumStudent.IsActive {
		t.Fatalf("ForStudent: Funded=%v, IsActive=%v; want true, true", sumStudent.Funded, sumStudent.IsActive)
	}
}

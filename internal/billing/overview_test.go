//go:build integration

package billing_test

import (
	"crypto/rand"
	"testing"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/students"
)

// Dashboard money must count only what funded contracts owe, once, and agree with
// the per-student rules for overdue, failed and in-review.
func TestOverviewCountsOnlyFundedTuition(t *testing.T) {
	db, svc, idA, _ := setupTestEnvironment(t, newMockStripeServer())
	now := time.Now().UTC()
	const day = 24 * time.Hour
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	create := func(row any) {
		t.Helper()
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	student := func() uint64 {
		t.Helper()
		hash := make([]byte, 32)
		rand.Read(hash)
		details := students.Details{Name: "Extra Student"}
		e := links.Enrollment{LinkID: seedLink(t, db), SessionHash: hash, Details: details, ExpiresAt: now.Add(7 * day)}
		create(&e)
		s := students.Student{EnrollmentID: &e.ID, Details: details, IsActive: true, Cohort: "2026-11"}
		create(&s)
		return e.ID
	}
	contract := func(enrollmentID uint64, fundedAt *time.Time, attention *string) uint64 {
		t.Helper()
		c := billing.Contract{EnrollmentID: enrollmentID, Reference: rand.Text(), PlanID: "three", PlanName: "Plan de 3 Pagos Mensuales",
			InstallmentCount: 3, TotalCents: 62500, FundedAt: fundedAt, ReviewReason: attention}
		create(&c)
		return c.ID
	}
	obligation := func(contractID uint64, kind string, number int, dueAt *time.Time, remaining int64, set func(*billing.Obligation)) {
		t.Helper()
		o := billing.Obligation{ContractID: contractID, Reference: rand.Text(), Kind: kind, Number: number, AmountCents: 20000,
			StripePriceID: "price", DueAt: dueAt, CollectionDisposition: "collect", AmountRemainingCents: remaining}
		if set != nil {
			set(&o)
		}
		create(&o)
	}
	review := func(o *billing.Obligation) { r := "invoice_mismatch"; o.ReviewReason = &r }
	settled := func(o *billing.Obligation) { o.FirstSettledAt = at(-35 * day) }

	// A: funded recently. #2 is overdue and failed, #3 is due soon and in review.
	a := contract(idA, at(-5*day), nil)
	obligation(a, "tuition", 1, at(-35*day), 0, settled)
	obligation(a, "tuition", 2, at(-10*day), 20000, func(o *billing.Obligation) { o.FailureOrActionRequired = true })
	obligation(a, "tuition", 3, at(10*day), 20000, review)
	obligation(a, "reinstatement", 0, nil, 2500, nil) // fees are not tuition
	// B: funded long ago, next installment outside the window; flagged on both levels, counted once.
	attention := "payment_review"
	b := contract(student(), at(-60*day), &attention)
	obligation(b, "tuition", 1, at(-60*day), 0, settled)
	obligation(b, "tuition", 2, at(40*day), 30000, review)
	// C: an abandoned Checkout owes nothing yet.
	c := contract(student(), nil, nil)
	obligation(c, "tuition", 1, nil, 20000, nil)

	create(&billing.Payment{ID: "pi_a", ContractID: a, GrossReceivedCents: 25000, GrossRefundedCents: 5000, PaymentTime: now})
	flagged := "dispute_open"
	create(&billing.Payment{ID: "pi_b", ContractID: b, GrossReceivedCents: 100, PaymentTime: now, DisputeOrFraudReviewState: &flagged})

	got, err := svc.Overview(t.Context(), now, 30*day)
	if err != nil {
		t.Fatal(err)
	}
	want := billing.Overview{
		CollectedCents: 20100, OutstandingCents: 70000,
		OverdueCents: 20000, OverdueInstallments: 1,
		DueSoonCents: 20000, DueSoonInstallments: 1,
		FailedInstallments: 1, ContractsInReview: 2, PaymentsInReview: 1, FundedRecently: 1,
	}
	if got != want {
		t.Fatalf("Overview = %+v, want %+v", got, want)
	}
}

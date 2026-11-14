//go:build integration

package customer

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/students"
)

func TestPaymentReturnRoute(t *testing.T) {
	db := customerDatabase(t)
	handler := newMux(t, db)

	// 1. Missing query params returns 404
	rec := customerRequest(t, handler, http.MethodGet, "/payments/return", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for empty query, got %d", rec.Code)
	}

	// 2. Unknown attempt reference returns 404
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?attempt=UNKNOWNREF", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown attempt, got %d", rec.Code)
	}

	// 3. Unknown session_id returns 404
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?session_id=cs_unknown_999", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown session_id, got %d", rec.Code)
	}

	// 4. Known session_id returns 200 with verified status and amount
	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	now := time.Now().UTC()
	parent := links.Link{
		Name:      "2026-11-01",
		Token:     strings.Repeat("R", 43),
		CreatedBy: "staff",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatalf("create parent link: %v", err)
	}
	link := links.Enrollment{
		LinkID:      parent.ID,
		SessionHash: tokenHash,
		Details: students.Details{
			Name:  "Return Student",
			Email: "return@example.com",
		},
		CreatedAt:        now,
		SubmittedAt:      &now,
		TermsAcceptedAt:  &now,
		ProgramStartDate: "2026-11-01",
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}
	student := students.Student{
		EnrollmentID: &link.ID,
		Details:      link.Details,
		IsActive:     true,
		Cohort:       "2026-11",
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create student: %v", err)
	}
	contract := billing.Contract{
		EnrollmentID:     link.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		TotalCents:       60000,
	}
	if err := db.Create(&contract).Error; err != nil {
		t.Fatalf("create contract: %v", err)
	}
	sessID := "cs_return_test_123"
	attempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "full",
		FirstAmountCents:  60000,
		StripePriceID:     "pr_full",
		CreationStartedAt: now,
		StripeSessionID:   &sessID,
		State:             "paid",
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?session_id="+sessID, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid session_id, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Pagado") || !strings.Contains(body, "$600.00") {
		t.Fatalf("expected status 'Pagado' and amount '$600.00' in body: %s", body)
	}
	// Never leaks student identity or contact
	if strings.Contains(body, "Return Student") || strings.Contains(body, "return@example.com") {
		t.Fatalf("return page leaked student details: %s", body)
	}

	// 5. Pending attempt renders polling trigger, spinner, and meta refresh
	pendingSessID := "cs_return_pending_456"
	pendingAttempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "full",
		FirstAmountCents:  60000,
		StripePriceID:     "pr_full",
		CreationStartedAt: now,
		StripeSessionID:   &pendingSessID,
		State:             "processing",
	}
	if err := db.Create(&pendingAttempt).Error; err != nil {
		t.Fatalf("create pending attempt: %v", err)
	}

	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?session_id="+pendingSessID, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for pending session_id, got %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "payment-spinner") {
		t.Fatalf("expected spinner in pending body: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 1.5s"`) || !strings.Contains(body, `polls=1`) {
		t.Fatalf("expected htmx trigger and polls=1 in pending body: %s", body)
	}
	if !strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected noscript refresh in pending body: %s", body)
	}

	// 5b. Checkout's back link names the attempt: a declined try shows its reason group, never polls,
	// and never leaks student details; an unpaid end without a decline reads as not completed.
	declineCode := "insufficient_funds"
	declined := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "full",
		FirstAmountCents:  60000,
		StripePriceID:     "pr_full",
		CreationStartedAt: now,
		State:             "failed",
		DeclineCode:       &declineCode,
	}
	expired := declined
	expired.Reference, expired.State, expired.DeclineCode = rand.Text(), "expired", nil
	if err := db.Create(&[]billing.CheckoutAttempt{declined, expired}).Error; err != nil {
		t.Fatalf("create declined and expired attempts: %v", err)
	}
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?attempt="+declined.Reference, "", false)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Pago rechazado") || !strings.Contains(body, "fondos") {
		t.Fatalf("declined attempt: status %d, want 200 with decline reason: %s", rec.Code, body)
	}
	if strings.Contains(body, "hx-trigger") || strings.Contains(body, "Return Student") {
		t.Fatalf("declined page must not poll or leak student details: %s", body)
	}
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?attempt="+expired.Reference, "", false)
	if body = rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "No completado") {
		t.Fatalf("expired attempt: status %d, want 200 No completado: %s", rec.Code, body)
	}

	// 6. HX-Request returns only the partial
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?session_id="+pendingSessID+"&polls=1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for HX-Request, got %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, "<!doctype") || strings.Contains(body, "<html") {
		t.Fatalf("HX-Request should return only partial, got full page: %s", body)
	}
	if !strings.Contains(body, `polls=2`) {
		t.Fatalf("expected next poll count polls=2 in partial: %s", body)
	}

	// 7. Cap at polls >= 80 stops polling
	rec = customerRequest(t, handler, http.MethodGet, "/payments/return?session_id="+pendingSessID+"&polls=80", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for polls=80, got %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, "hx-trigger") {
		t.Fatalf("polls=80 should not have hx-trigger: %s", body)
	}
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("polls=80 should not have meta refresh: %s", body)
	}

	// 8. Failing DB with HX-Request returns 500 + retry fragment with polls+1
	failingHandler := newMux(t, failingDB(t, db))

	rec = customerRequest(t, failingHandler, http.MethodGet, "/payments/return?session_id=cs_failing_1&polls=1", "", true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for failing DB HX-Request, got %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, "<!doctype") || strings.Contains(body, "<html") {
		t.Fatalf("HX-Request should return only partial, got full page: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 1.5s"`) {
		t.Fatalf("expected hx-trigger in retry fragment: %s", body)
	}
	if !strings.Contains(body, `polls=2`) {
		t.Fatalf("expected polls=2 in retry fragment: %s", body)
	}

	// 9. Failing DB with HX-Request at polls=80 returns 500 without hx-trigger
	rec = customerRequest(t, failingHandler, http.MethodGet, "/payments/return?session_id=cs_failing_1&polls=80", "", true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for failing DB HX-Request at cap, got %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, "hx-trigger") {
		t.Fatalf("polls=80 error fragment should not have hx-trigger: %s", body)
	}
	if !strings.Contains(body, "Actualiza esta página") {
		t.Fatalf("expected refresh copy in capped error fragment: %s", body)
	}

	// 10. Failing DB with non-HX request returns 500 full page
	rec = customerRequest(t, failingHandler, http.MethodGet, "/payments/return?session_id=cs_failing_1&polls=1", "", false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for failing DB non-HX request, got %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `id="payment-error"`) {
		t.Fatalf("expected full-page payment-error callout in non-HX error: %s", body)
	}
}

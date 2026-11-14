//go:build integration

package billing_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/links"
	"projecthermes/internal/payments"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

type customerUpdate struct{ ID, Name, Email string }

type mockStripeServer struct {
	mux         *http.ServeMux
	sessionReqs int64
	expireReqs  int64

	mu                        sync.Mutex
	expireReturnsPaid         bool
	createSessionError        bool
	createSessionLoseResponse bool // Stripe creates the Session, but the response never arrives
	receivedCurrency          string
	receivedCustomer          string
	receivedPMC               string
	receivedAllowed           []string
	customersCreated          map[string]string         // contractRef -> customerID
	sessionsCreated           map[string]string         // attemptRef -> sessionID
	sessions                  map[string]map[string]any // sessionID -> stored Session
	customerUpdateError       bool
	customerUpdates           []customerUpdate // guarded by mu
}

func newMockStripeServer() *mockStripeServer {
	m := &mockStripeServer{
		customersCreated: make(map[string]string),
		sessionsCreated:  make(map[string]string),
		sessions:         make(map[string]map[string]any),
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/customers", func(w http.ResponseWriter, r *http.Request) {
		idemp := r.Header.Get("Idempotency-Key")
		m.mu.Lock()
		defer m.mu.Unlock()

		cusID := "cus_mock_" + rand.Text()[:8]
		if idemp != "" && m.customersCreated[idemp] != "" {
			cusID = m.customersCreated[idemp]
		} else if idemp != "" {
			m.customersCreated[idemp] = cusID
		}

		resp := map[string]any{
			"id":       cusID,
			"object":   "customer",
			"livemode": false,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		if m.customerUpdateError {
			m.mu.Unlock()
			http.Error(w, `{"error":{"message":"customer update error"}}`, http.StatusInternalServerError)
			return
		}
		_ = r.ParseForm()
		m.customerUpdates = append(m.customerUpdates, customerUpdate{r.PathValue("id"), r.FormValue("name"), r.FormValue("email")})
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "object": "customer", "livemode": false})
	})

	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.sessionReqs, 1)
		m.mu.Lock()
		_ = r.ParseForm()
		m.receivedCurrency = r.FormValue("currency")
		m.receivedCustomer = r.FormValue("customer")
		m.receivedPMC = r.FormValue("payment_method_configuration")
		m.receivedAllowed = m.receivedAllowed[:0]
		for key, values := range r.Form {
			if strings.HasPrefix(key, "allowed_payment_method_types[") {
				m.receivedAllowed = append(m.receivedAllowed, values...)
			}
		}
		if m.createSessionError {
			m.mu.Unlock()
			http.Error(w, `{"error":{"message":"internal error"}}`, http.StatusInternalServerError)
			return
		}

		sessID := "cs_mock_" + rand.Text()[:8]
		idemp := r.Header.Get("Idempotency-Key")
		if idemp != "" && m.sessionsCreated[idemp] != "" {
			sessID = m.sessionsCreated[idemp]
		} else if idemp != "" {
			m.sessionsCreated[idemp] = sessID
		}
		status := "open"
		price := r.FormValue("line_items[0][price]")
		amountTotal := int64(20834)
		switch price {
		case "pr_full":
			amountTotal = 60000
		case "pr_three_1":
			amountTotal = 20834
		case "pr_six_1":
			amountTotal = 10835
		}

		resp := map[string]any{
			"id":                  sessID,
			"object":              "checkout.session",
			"url":                 "https://checkout.stripe.com/pay/" + sessID,
			"currency":            "usd",
			"mode":                "payment",
			"customer":            m.receivedCustomer,
			"client_reference_id": r.FormValue("client_reference_id"),
			"livemode":            false,
			"status":              status,
			"payment_status":      "unpaid",
			"amount_total":        amountTotal,
		}
		m.sessions[sessID] = resp
		lose := m.createSessionLoseResponse
		m.mu.Unlock()
		if lose {
			http.Error(w, `{"error":{"message":"internal error"}}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /v1/checkout/sessions/{id}/expire", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.expireReqs, 1)
		m.mu.Lock()
		defer m.mu.Unlock()
		status := "expired"
		paymentStatus := "unpaid"
		if m.expireReturnsPaid {
			status = "complete"
			paymentStatus = "paid"
		}
		resp := map[string]any{
			"id":             r.PathValue("id"),
			"object":         "checkout.session",
			"status":         status,
			"payment_status": paymentStatus,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v1/checkout/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		resp, ok := m.sessions[r.PathValue("id")]
		if !ok {
			resp = map[string]any{
				"id":             r.PathValue("id"),
				"object":         "checkout.session",
				"status":         "open",
				"payment_status": "unpaid",
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		customer := r.URL.Query().Get("customer")
		m.mu.Lock()
		defer m.mu.Unlock()
		data := []map[string]any{}
		for _, sess := range m.sessions {
			if customer == "" || sess["customer"] == customer {
				data = append(data, sess)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":   "list",
			"url":      "/v1/checkout/sessions",
			"has_more": false,
			"data":     data,
		})
	})

	m.mux = mux
	return m
}

// with runs f holding the mock lock, for reading or flipping fields the handlers share.
func (m *mockStripeServer) with(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f()
}

// setupTestEnvironment seeds a Link and, only so staff-keyed entry points (ForStudent, ReconcileStudent)
// can reach the contract, a Student. Production creates the Student at funding (links.Enroll), so a
// contract never has one before funding.
func seedLink(t *testing.T, db *gorm.DB) uint64 {
	t.Helper()
	var b [32]byte
	rand.Read(b[:])
	l := links.Link{
		Name:      "2026-11-01",
		Token:     base64.RawURLEncoding.EncodeToString(b[:]),
		CreatedBy: "staff",
		ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&l).Error; err != nil {
		t.Fatalf("create test parent link: %v", err)
	}
	return l.ID
}

func setupTestEnvironment(t *testing.T, mock *mockStripeServer) (*gorm.DB, *billing.Service, uint64, uint64) {
	t.Helper()
	allModels := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)

	now := time.Now().UTC()
	link := links.Enrollment{
		LinkID:      seedLink(t, db),
		SessionHash: tokenHash,
		Details: students.Details{
			Name:         "Maria Santos",
			Email:        "maria@example.com",
			Phone:        "+17875559876",
			Municipality: "Ponce",
		},
		CreatedAt:        now,
		SubmittedAt:      &now,
		TermsAcceptedAt:  &now,
		ProgramStartDate: "2026-11-01",
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("create test link: %v", err)
	}

	student := students.Student{
		EnrollmentID: &link.ID,
		Details:        link.Details,
		CreatedAt:      now,
		IsActive:       true,
		Cohort:         "2026-11",
	}
	if err := db.Create(&student).Error; err != nil {
		t.Fatalf("create test student: %v", err)
	}

	svc := billingtest.New(t, db, mock.mux)

	return db, svc, link.ID, student.ID
}

func TestCheckoutConcurrencyAndRecovery(t *testing.T) {
	mock := newMockStripeServer()

	db, svc, enrollmentID, _ := setupTestEnvironment(t, mock)

	// 1. Invalid or expired source/consent cannot start payment
	t.Run("invalid_source_rejected", func(t *testing.T) {
		_, err := svc.StartCheckout(context.Background(), 0, "three")
		if err != billing.ErrUnavailable {
			t.Fatalf("expected ErrUnavailable for zero id, got %v", err)
		}

		_, err = svc.StartCheckout(context.Background(), 999999, "three")
		if err != billing.ErrUnavailable {
			t.Fatalf("expected ErrUnavailable for unknown id, got %v", err)
		}
	})

	t.Run("unknown_plan_rejected", func(t *testing.T) {
		_, err := svc.StartCheckout(context.Background(), enrollmentID, "invalid_plan")
		if err != payments.ErrUnknownPlan {
			t.Fatalf("expected ErrUnknownPlan, got %v", err)
		}
	})

	// 2. Concurrent starts produce exactly one contract, one customer, and one open attempt
	t.Run("concurrent_starts", func(t *testing.T) {
		const concurrency = 5
		var wg sync.WaitGroup
		results := make([]billing.CheckoutResult, concurrency)
		errs := make([]error, concurrency)

		for i := range concurrency {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				results[idx], errs[idx] = svc.StartCheckout(context.Background(), enrollmentID, "three")
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("checkout %d failed: %v", i, err)
			}
			if results[i].URL == "" && !results[i].Pending {
				t.Fatalf("checkout %d returned empty URL and not pending", i)
			}
		}

		// Verify database state: exactly 1 contract, 1 active attempt, 3 obligations
		var contractCount int64
		db.Model(&billing.Contract{}).Where("enrollment_id = ?", enrollmentID).Count(&contractCount)
		if contractCount != 1 {
			t.Fatalf("expected 1 contract, got %d", contractCount)
		}

		var attemptCount int64
		db.Model(&billing.CheckoutAttempt{}).Count(&attemptCount)
		if attemptCount != 1 {
			t.Fatalf("expected 1 attempt, got %d", attemptCount)
		}

		var obligationCount int64
		db.Model(&billing.Obligation{}).Count(&obligationCount)
		if obligationCount != 3 {
			t.Fatalf("expected 3 obligations, got %d", obligationCount)
		}

		// Verify Stripe request was explicitly in USD
		mock.mu.Lock()
		if mock.receivedCurrency != "usd" {
			t.Errorf("expected Stripe currency 'usd', got %q", mock.receivedCurrency)
		}
		if mock.receivedPMC != "" || !slices.Equal(mock.receivedAllowed, []string{"card"}) {
			t.Errorf("installment Checkout must allow only card and skip the payment method configuration, got pmc %q, allowed %v", mock.receivedPMC, mock.receivedAllowed)
		}
		mock.mu.Unlock()
	})

	// 3. Replay with same plan returns the existing open attempt URL without a second charge attempt
	t.Run("same_plan_replay", func(t *testing.T) {
		res, err := svc.StartCheckout(context.Background(), enrollmentID, "three")
		if err != nil {
			t.Fatalf("unexpected error on replay: %v", err)
		}
		if res.URL == "" {
			t.Fatal("expected non-empty URL on replay")
		}

		var attemptCount int64
		db.Model(&billing.CheckoutAttempt{}).Count(&attemptCount)
		if attemptCount != 1 {
			t.Fatalf("expected attemptCount to stay 1, got %d", attemptCount)
		}
	})

	// 4. Provider failure marks attempt uncertain and returns Pending
	t.Run("provider_failure_recovery", func(t *testing.T) {
		// Create another student to test failure isolation
		otherHash := make([]byte, 32)
		rand.Read(otherHash)
		now := time.Now().UTC()
		link := links.Enrollment{
			LinkID:      seedLink(t, db),
			SessionHash: otherHash,
			Details: students.Details{
				Name:  "Carlos Gomez",
				Email: "carlos@example.com",
			},
			SubmittedAt:      &now,
			TermsAcceptedAt:  &now,
			ProgramStartDate: "2026-11-01",
			ExpiresAt:        now.Add(7 * 24 * time.Hour),
		}
		db.Create(&link)
		student := students.Student{
			EnrollmentID: &link.ID,
			Details:        link.Details,
			IsActive:       true,
			Cohort:         "2026-11",
		}
		db.Create(&student)

		mock.mu.Lock()
		mock.createSessionError = true
		mock.mu.Unlock()

		res, err := svc.StartCheckout(context.Background(), link.ID, "full")
		if err != nil {
			t.Fatalf("expected nil error (graceful pending), got %v", err)
		}
		if !res.Pending {
			t.Fatal("expected Pending=true on provider failure")
		}
		mock.mu.Lock()
		if mock.receivedPMC != "pmc_test" || len(mock.receivedAllowed) != 0 {
			t.Errorf("single-payment Checkout must use the payment method configuration and no allow-list, got pmc %q, allowed %v", mock.receivedPMC, mock.receivedAllowed)
		}
		mock.mu.Unlock()

		var failedAttempt billing.CheckoutAttempt
		err = db.Where("plan_id = 'full'").First(&failedAttempt).Error
		if err != nil {
			t.Fatalf("failed to find attempt: %v", err)
		}
		if failedAttempt.State != "uncertain" {
			t.Fatalf("expected state 'uncertain', got %q", failedAttempt.State)
		}
	})

	// 5. Funded contract cannot pay first installment twice
	t.Run("funded_cannot_repay", func(t *testing.T) {
		now := time.Now().UTC()
		db.Model(&billing.Contract{}).Where("enrollment_id = ?", enrollmentID).Update("funded_at", &now)
		// Same plan -> returns Funded: true without creating attempts
		res, err := svc.StartCheckout(context.Background(), enrollmentID, "three")
		if err != nil {
			t.Fatalf("unexpected error for funded contract replay: %v", err)
		}
		if !res.Funded {
			t.Fatal("expected Funded=true")
		}

		// Different plan -> ErrPlanLocked
		_, err = svc.StartCheckout(context.Background(), enrollmentID, "six")
		if err != billing.ErrPlanLocked {
			t.Fatalf("expected ErrPlanLocked, got %v", err)
		}
	})
}

func TestPlanChangeExpiresPreviousSession(t *testing.T) {
	mock := newMockStripeServer()

	db, svc, enrollmentID, _ := setupTestEnvironment(t, mock)

	// 1. Initial checkout with "three" plan
	res1, err := svc.StartCheckout(context.Background(), enrollmentID, "three")
	if err != nil {
		t.Fatalf("initial checkout: %v", err)
	}
	if !strings.Contains(res1.URL, "cs_mock_") {
		t.Fatalf("expected mock session URL, got %s", res1.URL)
	}

	// 2. Change plan to "six"
	res2, err := svc.StartCheckout(context.Background(), enrollmentID, "six")
	if err != nil {
		t.Fatalf("plan change to six: %v", err)
	}
	if !strings.Contains(res2.URL, "cs_mock_") {
		t.Fatalf("expected new session URL, got %s", res2.URL)
	}

	// Verify expire was called on Stripe for the previous attempt
	if atomic.LoadInt64(&mock.expireReqs) < 1 {
		t.Fatalf("expected Stripe expire to be called, got %d", mock.expireReqs)
	}

	// Verify contract updated to six
	var contract billing.Contract
	if err := db.Where("enrollment_id = ?", enrollmentID).First(&contract).Error; err != nil {
		t.Fatalf("find contract: %v", err)
	}
	if contract.PlanID != "six" || contract.InstallmentCount != 6 {
		t.Fatalf("expected plan six with 6 installments, got %s (%d)", contract.PlanID, contract.InstallmentCount)
	}

	// Verify old attempt is terminal "expired"
	var oldAttempt billing.CheckoutAttempt
	if err := db.Where("contract_id = ? AND plan_id = 'three'", contract.ID).First(&oldAttempt).Error; err != nil {
		t.Fatalf("find old attempt: %v", err)
	}
	if oldAttempt.State != "expired" {
		t.Fatalf("expected old attempt state 'expired', got %q", oldAttempt.State)
	}

	// Verify 6 obligations exist
	var obligations []billing.Obligation
	db.Where("contract_id = ?", contract.ID).Find(&obligations)
	if len(obligations) != 6 {
		t.Fatalf("expected 6 obligations, got %d", len(obligations))
	}

	// 3. Race won by payment completion prevents replacement
	t.Run("race_won_by_completion_prevents_replacement", func(t *testing.T) {
		mock.mu.Lock()
		mock.expireReturnsPaid = true
		mock.mu.Unlock()

		// Attempting to change to "full" when Stripe session says it was paid
		res, err := svc.StartCheckout(context.Background(), enrollmentID, "full")
		if err != nil {
			t.Fatalf("start checkout: %v", err)
		}
		if !res.Pending {
			t.Fatal("expected Pending=true when session turns out to be paid")
		}

		// Verify contract remains on "six"
		db.Where("enrollment_id = ?", enrollmentID).First(&contract)
		if contract.PlanID != "six" {
			t.Fatalf("expected contract to remain 'six', got %s", contract.PlanID)
		}
	})
}

func TestSummaryAndReturnStatus(t *testing.T) {
	mock := newMockStripeServer()

	db, svc, enrollmentID, studentID := setupTestEnvironment(t, mock)

	// Summary before contract creation: returns nil, nil
	summary, err := svc.ForEnrollment(context.Background(), enrollmentID)
	if err != nil {
		t.Fatalf("ForEnrollment error: %v", err)
	}
	if summary != nil {
		t.Fatalf("expected nil summary before checkout, got %+v", summary)
	}

	// Start checkout
	res, err := svc.StartCheckout(context.Background(), enrollmentID, "three")
	if err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if res.URL == "" {
		t.Fatal("expected non-empty URL from StartCheckout")
	}
	// Summary after attempt created: returns populated summary with 3 installments
	summary, err = svc.ForStudent(context.Background(), studentID)
	if err != nil {
		t.Fatalf("ForStudent error: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
	if summary.PlanID != "three" || summary.TotalCents != 62500 || len(summary.Installments) != 3 {
		t.Fatalf("unexpected summary values: %+v", summary)
	}
	if summary.Funded {
		t.Fatal("expected Funded=false")
	}
	if summary.RemainingTuitionCents != summary.TotalCents {
		t.Fatalf("expected RemainingTuitionCents == TotalCents (%d), got %d", summary.TotalCents, summary.RemainingTuitionCents)
	}
	for i, inst := range summary.Installments {
		if inst.Status != "scheduled" {
			t.Fatalf("installment %d: expected status 'scheduled', got %q", i+1, inst.Status)
		}
	}

	// Test ReturnStatus
	attempt := latestAttempt(t, db)
	if attempt.StripeSessionID == nil {
		t.Fatal("attempt stripe session ID is nil")
	}

	returnStatus, err := svc.ReturnStatus(context.Background(), *attempt.StripeSessionID)
	if err != nil {
		t.Fatalf("ReturnStatus error: %v", err)
	}
	if returnStatus.Status != "pending" && returnStatus.Status != "paid" {
		t.Fatalf("unexpected return status: %s", returnStatus.Status)
	}
	if returnStatus.AmountCents != 20834 {
		t.Fatalf("expected 20834 cents, got %d", returnStatus.AmountCents)
	}

	// Unknown session ID returns ErrUnavailable
	_, err = svc.ReturnStatus(context.Background(), "cs_unknown_123")
	if err != billing.ErrUnavailable {
		t.Fatalf("expected ErrUnavailable for unknown session, got %v", err)
	}
}

func latestAttempt(t *testing.T, db *gorm.DB) billing.CheckoutAttempt {
	t.Helper()
	var a billing.CheckoutAttempt
	if err := db.Order("id DESC").First(&a).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}
	return a
}

func attemptCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&billing.CheckoutAttempt{}).Count(&n).Error; err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return n
}

// A Checkout create whose Session ID was never recorded must be settled by the worker,
// never left blocking the contract.
func TestCheckoutCreateResumes(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown_create_is_replayed", func(t *testing.T) {
		mock := newMockStripeServer()
		db, svc, enrollmentID, studentID := setupTestEnvironment(t, mock)

		mock.with(func() { mock.createSessionError = true })
		res, err := svc.StartCheckout(ctx, enrollmentID, "three")
		if err != nil || !res.Pending {
			t.Fatalf("expected pending without error, got %+v, %v", res, err)
		}
		a := latestAttempt(t, db)
		if a.State != "uncertain" || a.StripeSessionID != nil {
			t.Fatalf("expected uncertain attempt without session, got %q %v", a.State, a.StripeSessionID)
		}

		mock.with(func() { mock.createSessionError = false })
		if _, err := svc.ReconcileStudent(ctx, studentID); err != nil {
			t.Fatalf("ReconcileStudent: %v", err)
		}
		a = latestAttempt(t, db)
		if a.State != "open" || a.StripeSessionID == nil || a.CheckoutURL == nil {
			t.Fatalf("expected open attempt with session and URL, got %+v", a)
		}
		key := "tuition:" + a.Reference + ":checkout"
		mock.with(func() {
			if _, ok := mock.sessionsCreated[key]; !ok || len(mock.sessionsCreated) != 1 {
				t.Fatalf("expected exactly one Session created under %q, got %v", key, mock.sessionsCreated)
			}
		})

		res, err = svc.StartCheckout(ctx, enrollmentID, "three")
		if err != nil || res.URL != *a.CheckoutURL {
			t.Fatalf("expected stored URL %q, got %+v, %v", *a.CheckoutURL, res, err)
		}
		if n := attemptCount(t, db); n != 1 {
			t.Fatalf("expected 1 attempt, got %d", n)
		}
	})

	t.Run("landed_create_is_adopted", func(t *testing.T) {
		mock := newMockStripeServer()
		db, svc, enrollmentID, studentID := setupTestEnvironment(t, mock)

		mock.with(func() { mock.createSessionLoseResponse = true })
		res, err := svc.StartCheckout(ctx, enrollmentID, "three")
		if err != nil || !res.Pending {
			t.Fatalf("expected pending without error, got %+v, %v", res, err)
		}
		a := latestAttempt(t, db)
		if a.State != "uncertain" || a.StripeSessionID != nil {
			t.Fatalf("expected uncertain attempt without session, got %q %v", a.State, a.StripeSessionID)
		}

		mock.with(func() { mock.createSessionLoseResponse = false })
		before := atomic.LoadInt64(&mock.sessionReqs)
		if _, err := svc.ReconcileStudent(ctx, studentID); err != nil {
			t.Fatalf("ReconcileStudent: %v", err)
		}
		a = latestAttempt(t, db)
		if a.State != "open" || a.StripeSessionID == nil || a.CheckoutURL == nil {
			t.Fatalf("expected open attempt with session and URL, got %+v", a)
		}
		mock.with(func() {
			if want := mock.sessionsCreated["tuition:"+a.Reference+":checkout"]; *a.StripeSessionID != want {
				t.Fatalf("expected adopted Session %q, got %q", want, *a.StripeSessionID)
			}
		})
		if got := atomic.LoadInt64(&mock.sessionReqs); got != before {
			t.Fatalf("expected no new Session create requests, got %d", got-before)
		}
	})

	t.Run("expired_key_fails_and_frees_contract", func(t *testing.T) {
		mock := newMockStripeServer()
		db, svc, enrollmentID, studentID := setupTestEnvironment(t, mock)

		mock.with(func() { mock.createSessionError = true })
		res, err := svc.StartCheckout(ctx, enrollmentID, "three")
		if err != nil || !res.Pending {
			t.Fatalf("expected pending without error, got %+v, %v", res, err)
		}
		a := latestAttempt(t, db)
		if a.State != "uncertain" {
			t.Fatalf("expected uncertain attempt, got %q", a.State)
		}
		// Age the attempt past the idempotency-key retention window.
		if err := db.Model(&billing.CheckoutAttempt{}).Where("id = ?", a.ID).
			Update("creation_started_at", time.Now().UTC().Add(-24*time.Hour)).Error; err != nil {
			t.Fatalf("age attempt: %v", err)
		}

		mock.with(func() { mock.createSessionError = false })
		before := atomic.LoadInt64(&mock.sessionReqs)
		if _, err := svc.ReconcileStudent(ctx, studentID); err != nil {
			t.Fatalf("ReconcileStudent: %v", err)
		}
		if a = latestAttempt(t, db); a.State != "failed" {
			t.Fatalf("expected failed attempt, got %q", a.State)
		}
		if got := atomic.LoadInt64(&mock.sessionReqs); got != before {
			t.Fatalf("expected no replay past the key window, got %d create requests", got-before)
		}

		res, err = svc.StartCheckout(ctx, enrollmentID, "three")
		if err != nil || res.URL == "" {
			t.Fatalf("expected a fresh checkout URL, got %+v, %v", res, err)
		}
		if n := attemptCount(t, db); n != 2 {
			t.Fatalf("expected 2 attempts, got %d", n)
		}
	})
}

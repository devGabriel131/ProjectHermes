//go:build integration

package billing_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/links"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

func mustCreate(t *testing.T, db *gorm.DB, value any) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}

type workerMockServer struct {
	mux           *http.ServeMux
	initialAmount int64 // fixed at construction

	mu                    sync.Mutex
	paidAtUnix            int64
	invoicesCreated       int64
	invoiceItemsCreated   int64
	refundedCents         int64
	earlyWarningsReturned []map[string]any
	invoicesByID          map[string]map[string]any
	cancels               []string          // "void <id>" or "delete <id>", in call order
	declines              map[string]string // failed invoice id -> decline code on its open InvoicePayment
	payCalls              []string          // idempotency key of each invoice pay, in call order
	payPays               bool              // an invoice pay succeeds instead of being declined
	refunds               []string          // "<payment intent> <cents> <idempotency key>" of each refund created, in call order
}

// registerInvoice adds a one-line invoice of the customer, as Stripe holds it.
func (wms *workerMockServer) registerInvoice(id string, amount int64, status, customer string) {
	paid, remaining := int64(0), amount
	if status == "paid" {
		paid, remaining = amount, 0
	}
	wms.mu.Lock()
	defer wms.mu.Unlock()
	wms.invoicesByID[id] = map[string]any{
		"id":               id,
		"object":           "invoice",
		"status":           status,
		"currency":         "usd",
		"total":            amount,
		"amount_due":       amount,
		"amount_paid":      paid,
		"amount_remaining": remaining,
		"customer":         customer,
		"lines": map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "il_" + id, "amount": amount, "currency": "usd"}},
		},
	}
}

// patchInvoice overwrites fields of a known invoice, as a change at Stripe would.
func (wms *workerMockServer) patchInvoice(id string, fields map[string]any) {
	wms.mu.Lock()
	defer wms.mu.Unlock()
	maps.Copy(wms.invoicesByID[id], fields)
}

func newWorkerMockServer(paidAt time.Time, initialAmount int64) *workerMockServer {
	wms := &workerMockServer{
		paidAtUnix:    paidAt.Unix(),
		initialAmount: initialAmount,
		invoicesByID:  make(map[string]map[string]any),
		declines:      make(map[string]string),
	}

	mux := http.NewServeMux()

	// 1. GET /v1/checkout/sessions/{id}
	mux.HandleFunc("GET /v1/checkout/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		resp := map[string]any{
			"id":             id,
			"object":         "checkout.session",
			"currency":       "usd",
			"mode":           "payment",
			"payment_status": "paid",
			"status":         "complete",
			"amount_total":   wms.initialAmount,
			"customer":       "cus_worker_test",
			"invoice": map[string]any{
				"id": "in_initial_paid",
			},
			"payment_intent": map[string]any{
				"id":     "pi_worker_test",
				"status": "succeeded",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 2. GET /v1/invoices/{id}
	mux.HandleFunc("GET /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv, exists := wms.invoicesByID[id]
		wms.mu.Unlock()

		if exists {
			_ = json.NewEncoder(w).Encode(inv)
			return
		}

		if id == "in_initial_paid" {
			resp := map[string]any{
				"id":          "in_initial_paid",
				"object":      "invoice",
				"status":      "paid",
				"currency":    "usd",
				"amount_paid": wms.initialAmount,
				"customer":    "cus_worker_test",
				"status_transitions": map[string]any{
					"paid_at": wms.paidAtUnix,
				},
				"lines": map[string]any{
					"object": "list",
					"data": []map[string]any{
						{
							"amount":   wms.initialAmount,
							"currency": "usd",
							"metadata": map[string]any{
								"tuition_obligation": "ref_first_obl",
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		http.Error(w, `{"error":{"message":"invoice not found"}}`, http.StatusNotFound)
	})

	// 3. GET /v1/invoice_payments
	mux.HandleFunc("GET /v1/invoice_payments", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		invID := r.FormValue("invoice")
		wms.mu.Lock()
		inv, exists := wms.invoicesByID[invID]
		code := wms.declines[invID]
		wms.mu.Unlock()
		if !exists && invID != "in_initial_paid" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": []any{}})
			return
		}
		if exists && inv["status"] != "paid" {
			data := []map[string]any{}
			if code != "" {
				data = append(data, map[string]any{
					"id": "invp_" + invID, "object": "invoice_payment", "status": "open",
					"payment": map[string]any{
						"type": "payment_intent",
						"payment_intent": map[string]any{
							"id": "pi_" + invID, "object": "payment_intent", "status": "requires_payment_method",
							"last_payment_error": map[string]any{"type": "card_error", "code": "card_declined", "decline_code": code},
						},
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": data})
			return
		}
		piID := "pi_worker_test"
		if invID != "in_initial_paid" && invID != "" {
			piID = "pi_" + invID
		}
		resp := map[string]any{
			"object":   "list",
			"has_more": false,
			"data": []map[string]any{
				{
					"id":          "invp_" + invID,
					"object":      "invoice_payment",
					"status":      "paid",
					"amount_paid": wms.initialAmount,
					"payment": map[string]any{
						"type": "payment_intent",
						"payment_intent": map[string]any{
							"id": piID,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 4. GET /v1/payment_intents/{id}
	mux.HandleFunc("GET /v1/payment_intents/{id}", func(w http.ResponseWriter, r *http.Request) {
		chID := "ch_mock_1"
		if r.PathValue("id") != "pi_worker_test" {
			chID = "ch_" + r.PathValue("id")
		}
		wms.mu.Lock()
		refunded := wms.refundedCents
		wms.mu.Unlock()
		resp := map[string]any{
			"id":              r.PathValue("id"),
			"object":          "payment_intent",
			"status":          "succeeded",
			"amount_received": wms.initialAmount,
			"currency":        "usd",
			"payment_method": map[string]any{
				"id":       "pm_card_test",
				"customer": "cus_worker_test",
			},
			"latest_charge": map[string]any{
				"id":              chID,
				"amount_refunded": refunded,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	// 5. GET /v1/customers/{id}
	mux.HandleFunc("GET /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
			"invoice_settings": map[string]any{
				"default_payment_method": map[string]any{
					"id": "pm_card_test",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 6. POST /v1/customers/{id}
	mux.HandleFunc("POST /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 7. POST /v1/invoices (Stage 1 creation)
	mux.HandleFunc("POST /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		atomic.AddInt64(&wms.invoicesCreated, 1)
		invID := fmt.Sprintf("in_sched_%d", atomic.LoadInt64(&wms.invoicesCreated))

		inv := map[string]any{
			"id":           invID,
			"object":       "invoice",
			"status":       "draft",
			"currency":     "usd",
			"customer":     r.FormValue("customer"),
			"metadata":     map[string]any{"tuition_obligation": r.FormValue("metadata[tuition_obligation]")},
			"amount_due":   20833,
			"auto_advance": false,
			"lines": map[string]any{
				"object": "list",
				"data":   []map[string]any{},
			},
		}

		wms.mu.Lock()
		wms.invoicesByID[invID] = inv
		wms.mu.Unlock()

		_ = json.NewEncoder(w).Encode(inv)
	})

	// GET /v1/invoices: the customer's invoices, one page.
	mux.HandleFunc("GET /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		customer := r.URL.Query().Get("customer")
		wms.mu.Lock()
		defer wms.mu.Unlock()
		data := []map[string]any{}
		for _, inv := range wms.invoicesByID {
			if inv["customer"] == customer {
				data = append(data, inv)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "url": "/v1/invoices", "has_more": false, "data": data})
	})

	mux.HandleFunc("POST /v1/invoices/{id}/void", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		defer wms.mu.Unlock()
		wms.cancels = append(wms.cancels, "void "+id)
		inv := wms.invoicesByID[id]
		inv["status"] = "void"
		_ = json.NewEncoder(w).Encode(inv)
	})

	mux.HandleFunc("DELETE /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		defer wms.mu.Unlock()
		wms.cancels = append(wms.cancels, "delete "+id)
		delete(wms.invoicesByID, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "invoice", "deleted": true})
	})

	// 8. POST /v1/invoiceitems (Stage 2 creation)
	mux.HandleFunc("POST /v1/invoiceitems", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&wms.invoiceItemsCreated, 1)
		_ = r.ParseForm()
		invID := r.FormValue("invoice")
		amount := int64(20833)
		price := r.FormValue("pricing[price]")
		if price == "" {
			price = r.FormValue("price")
		}
		if price == "pr_reinstatement" {
			amount = 2500
		}

		wms.mu.Lock()
		if inv, ok := wms.invoicesByID[invID]; ok {
			inv["lines"] = map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":       "il_mock",
						"amount":   amount,
						"currency": "usd",
					},
				},
			}
			inv["total"] = amount
			inv["amount_due"] = amount
		}
		wms.mu.Unlock()

		resp := map[string]any{
			"id":      fmt.Sprintf("ii_sched_%d", atomic.LoadInt64(&wms.invoiceItemsCreated)),
			"object":  "invoiceitem",
			"invoice": invID,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 9. POST /v1/invoices/{id} (Stage 3 arming)
	mux.HandleFunc("POST /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		wms.mu.Unlock()

		resp := map[string]any{
			"id":           id,
			"object":       "invoice",
			"status":       "draft",
			"currency":     "usd",
			"auto_advance": true,
			"lines":        inv["lines"],
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 10. GET /v1/radar/early_fraud_warnings
	mux.HandleFunc("GET /v1/radar/early_fraud_warnings", func(w http.ResponseWriter, r *http.Request) {
		wms.mu.Lock()
		data := wms.earlyWarningsReturned
		wms.mu.Unlock()
		if data == nil {
			data = []map[string]any{}
		}
		resp := map[string]any{
			"object":   "list",
			"has_more": false,
			"data":     data,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 11. GET /v1/refunds/{id}
	mux.HandleFunc("GET /v1/refunds/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":     r.PathValue("id"),
			"object": "refund",
			"payment_intent": map[string]any{
				"id": "pi_worker_test",
			},
			"charge": map[string]any{
				"id": "ch_mock_1",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// 12. POST /v1/invoices/{id}/finalize
	mux.HandleFunc("POST /v1/invoices/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		if inv == nil {
			inv = map[string]any{"id": id, "object": "invoice", "currency": "usd"}
		}
		inv["status"] = "open"
		inv["hosted_invoice_url"] = "https://invoice.stripe.com/test_" + id
		wms.invoicesByID[id] = inv
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inv)
	})

	// POST /v1/invoices/{id}/pay: the app's own retry of a failed installment
	mux.HandleFunc("POST /v1/invoices/{id}/pay", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		defer wms.mu.Unlock()
		wms.payCalls = append(wms.payCalls, r.Header.Get("Idempotency-Key"))
		if !wms.payPays {
			w.WriteHeader(http.StatusPaymentRequired)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "card_error", "code": "card_declined", "decline_code": wms.declines[id]}})
			return
		}
		inv := wms.invoicesByID[id]
		maps.Copy(inv, map[string]any{
			"status": "paid", "amount_paid": int64(20833), "amount_remaining": int64(0),
			"status_transitions": map[string]any{"paid_at": time.Now().Unix()},
		})
		_ = json.NewEncoder(w).Encode(inv)
	})

	// 13. POST /v1/invoices/{id}/send
	mux.HandleFunc("POST /v1/invoices/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		wms.mu.Lock()
		inv := wms.invoicesByID[id]
		wms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inv)
	})

	// POST /v1/refunds: the withdrawal's refund of one PaymentIntent
	mux.HandleFunc("POST /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		amount, _ := strconv.ParseInt(r.FormValue("amount"), 10, 64)
		wms.mu.Lock()
		defer wms.mu.Unlock()
		wms.refunds = append(wms.refunds, r.FormValue("payment_intent")+" "+r.FormValue("amount")+" "+r.Header.Get("Idempotency-Key"))
		wms.refundedCents += amount
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("re_w_%d", len(wms.refunds)), "object": "refund", "status": "succeeded",
			"amount": amount, "payment_intent": r.FormValue("payment_intent"),
		})
	})

	wms.mux = mux
	return wms
}

// seedThreePlanCheckout seeds a submitted Enrollment whose unfunded "three" contract has its quoted
// obligations and an open attempt; the worker mock reports that attempt's Session paid.
func seedThreePlanCheckout(t *testing.T) (*gorm.DB, links.Enrollment, billing.Contract, billing.CheckoutAttempt) {
	t.Helper()
	db := testdb.Open(t, append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)...)
	sessionHash := make([]byte, 32)
	rand.Read(sessionHash)
	now := time.Now().UTC()
	enrollment := links.Enrollment{
		LinkID:           seedLink(t, db),
		SessionHash:      sessionHash,
		Details:          students.Details{Name: "Test Student", Email: "test@example.com", Phone: "+17875551234"},
		CreatedAt:        now,
		SubmittedAt:      &now,
		ProgramStartDate: "2026-11-01",
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
	}
	mustCreate(t, db, &enrollment)
	customerID := "cus_worker_test"
	contract := billing.Contract{
		EnrollmentID:     enrollment.ID,
		Reference:        rand.Text(),
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       62500,
		StripeCustomerID: &customerID,
	}
	mustCreate(t, db, &contract)
	for i, amount := range []int64{20834, 20833, 20833} {
		price := "pr_three_2"
		if i == 0 {
			price = "pr_three_1"
		}
		mustCreate(t, db, &billing.Obligation{
			ContractID:            contract.ID,
			Reference:             rand.Text(),
			Kind:                  "tuition",
			Number:                i + 1,
			AmountCents:           amount,
			AmountRemainingCents:  amount,
			StripePriceID:         price,
			CollectionDisposition: "collect",
		})
	}
	sessionID := "cs_" + rand.Text()
	attempt := billing.CheckoutAttempt{
		ContractID:        contract.ID,
		Reference:         rand.Text(),
		PlanID:            "three",
		FirstAmountCents:  20834,
		StripePriceID:     "pr_three_1",
		CreationStartedAt: now,
		StripeSessionID:   &sessionID,
		State:             "open",
	}
	mustCreate(t, db, &attempt)
	return db, enrollment, contract, attempt
}

func TestFundingSchedulesLaterInvoicesOnce(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 20834)

	db, enrollment, contract, attempt := seedThreePlanCheckout(t)
	svc := billingtest.New(t, db, mock.mux)

	// Insert checkout.session.completed webhook event into DB
	evt := billing.WebhookEvent{
		ID:         "evt_checkout_completed",
		EventType:  "checkout.session.completed",
		ResourceID: *attempt.StripeSessionID,
	}
	mustCreate(t, db, &evt)

	var studentCountBefore int64
	db.Model(&students.Student{}).Count(&studentCountBefore)
	if studentCountBefore != 0 {
		t.Fatalf("expected 0 students before funding, got %d", studentCountBefore)
	}

	// One worker pass: the event funds the contract, then the same pass creates the later invoices
	svc.DrainForTest(t.Context())

	var enrolledStudent students.Student
	if err := db.Where("enrollment_id = ?", enrollment.ID).Take(&enrolledStudent).Error; err != nil {
		t.Fatalf("enrolled student: %v", err)
	}
	if enrolledStudent.Name != enrollment.Name || enrolledStudent.Email != enrollment.Email || enrolledStudent.Phone != enrollment.Phone {
		t.Fatalf("student details mismatch: %+v, want %+v", enrolledStudent.Details, enrollment.Details)
	}
	if !enrolledStudent.CreatedAt.Equal(paidAt) {
		t.Fatalf("expected student created_at %v, got %v", paidAt, enrolledStudent.CreatedAt)
	}
	// Verify contract is now funded
	var updatedContract billing.Contract
	db.First(&updatedContract, contract.ID)
	if updatedContract.FundedAt == nil {
		t.Fatal("expected contract.FundedAt to be set")
	}
	if !updatedContract.FundedAt.Equal(paidAt) {
		t.Fatalf("expected FundedAt %v, got %v", paidAt, *updatedContract.FundedAt)
	}

	// Verify attempt is now terminal "paid"
	var updatedAttempt billing.CheckoutAttempt
	db.First(&updatedAttempt, attempt.ID)
	if updatedAttempt.State != "paid" {
		t.Fatalf("expected attempt state 'paid', got %q", updatedAttempt.State)
	}

	// Verify exactly two remaining scheduled invoices were created (for plan "three")
	var remainingObligations []billing.Obligation
	db.Where("contract_id = ? AND number > 1", contract.ID).Order("number ASC").Find(&remainingObligations)
	if len(remainingObligations) != 2 {
		t.Fatalf("expected 2 remaining obligations, got %d", len(remainingObligations))
	}
	for i, o := range remainingObligations {
		if o.StripeInvoiceID == nil {
			t.Errorf("obligation %d has nil StripeInvoiceID", i+2)
		}
		if o.StripeInvoiceItemID == nil {
			t.Errorf("obligation %d has nil StripeInvoiceItemID", i+2)
		}
		if o.ScheduledFinalizationAt == nil {
			t.Errorf("obligation %d has nil ScheduledFinalizationAt", i+2)
		}
		if o.DueAt == nil {
			t.Errorf("obligation %d has nil DueAt", i+2)
		}
	}

	// Verify atomic idempotent recovery: re-running does not create duplicate invoices
	invoicesBefore := atomic.LoadInt64(&mock.invoicesCreated)
	db.Model(&contract).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	invoicesAfter := atomic.LoadInt64(&mock.invoicesCreated)
	if invoicesAfter != invoicesBefore {
		t.Fatalf("re-run created duplicate invoices: before=%d, after=%d", invoicesBefore, invoicesAfter)
	}
}

func TestRecurringPaymentsWebhooks(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 20834)
	secret := billingtest.Config().WebhookSecret

	db, _, contract, attempt := seedThreePlanCheckout(t)
	svc := billingtest.New(t, db, mock.mux)

	postWebhook := func(evtID, evtType, resourceID string) {
		payload := makeStripeEventJSON(evtID, evtType, resourceID, false, stripe.APIVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())
		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()
		svc.Webhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook %s failed (%d): %s", evtType, rec.Code, rec.Body.String())
		}
	}

	// 1. Initial checkout payment arrives via webhook
	postWebhook("evt_checkout_done", "checkout.session.completed", *attempt.StripeSessionID)
	svc.DrainForTest(t.Context())

	var c billing.Contract
	db.First(&c, contract.ID)
	if c.FundedAt == nil {
		t.Fatal("expected contract to be funded")
	}

	var ob2, ob3 billing.Obligation
	if err := db.Where("contract_id = ? AND number = 2", contract.ID).First(&ob2).Error; err != nil {
		t.Fatalf("load obligation 2: %v", err)
	}
	if err := db.Where("contract_id = ? AND number = 3", contract.ID).First(&ob3).Error; err != nil {
		t.Fatalf("load obligation 3: %v", err)
	}

	if ob2.DueAt == nil || ob3.DueAt == nil {
		t.Fatalf("expected DueAt on ob2 and ob3, got ob2=%v, ob3=%v", ob2.DueAt, ob3.DueAt)
	}

	diff2 := ob2.DueAt.Sub(*c.FundedAt)
	if diff2 < 27*24*time.Hour || diff2 > 32*24*time.Hour {
		t.Fatalf("obligation 2 DueAt diff from FundedAt = %v, want ~1 month", diff2)
	}
	diff3 := ob3.DueAt.Sub(*c.FundedAt)
	if diff3 < 57*24*time.Hour || diff3 > 63*24*time.Hour {
		t.Fatalf("obligation 3 DueAt diff from FundedAt = %v, want ~2 months", diff3)
	}

	if ob2.StripeInvoiceID == nil {
		t.Fatal("expected ob2 to have StripeInvoiceID")
	}
	invID2 := *ob2.StripeInvoiceID
	if ob2.ScheduledFinalizationAt == nil || !ob2.ScheduledFinalizationAt.Equal(*ob2.DueAt) {
		t.Fatalf("ob2 ScheduledFinalizationAt = %v, want %v", ob2.ScheduledFinalizationAt, ob2.DueAt)
	}
	if ob2.LatestStripeStatus == nil || *ob2.LatestStripeStatus != "draft" {
		t.Fatalf("ob2 LatestStripeStatus = %v, want draft", ob2.LatestStripeStatus)
	}

	// 2. Stripe finalizes invoice 2 -> webhook invoice.finalized
	mock.patchInvoice(invID2, map[string]any{"status": "open", "amount_remaining": int64(20833)})

	postWebhook("evt_inv2_finalized", "invoice.finalized", invID2)
	svc.DrainForTest(t.Context())

	db.First(&ob2, ob2.ID)
	if ob2.LatestStripeStatus == nil || *ob2.LatestStripeStatus != "open" {
		t.Fatalf("after invoice.finalized: ob2 status = %v, want open", ob2.LatestStripeStatus)
	}
	if ob2.AmountRemainingCents != 20833 {
		t.Fatalf("after invoice.finalized: remaining = %d, want 20833", ob2.AmountRemainingCents)
	}

	// 3. Recurring payment succeeds -> webhook invoice.paid
	paidTime := ob2.DueAt.Unix()
	mock.patchInvoice(invID2, map[string]any{
		"status": "paid", "amount_paid": int64(20833), "amount_remaining": int64(0),
		"status_transitions": map[string]any{"paid_at": paidTime},
	})

	postWebhook("evt_inv2_paid", "invoice.paid", invID2)
	svc.DrainForTest(t.Context())

	db.First(&ob2, ob2.ID)
	if ob2.LatestStripeStatus == nil || *ob2.LatestStripeStatus != "paid" {
		t.Fatalf("after invoice.paid: ob2 status = %v, want paid", ob2.LatestStripeStatus)
	}
	if ob2.AmountRemainingCents != 0 {
		t.Fatalf("after invoice.paid: remaining = %d, want 0", ob2.AmountRemainingCents)
	}
	if ob2.AmountPaidCents != 20833 {
		t.Fatalf("after invoice.paid: paid = %d, want 20833", ob2.AmountPaidCents)
	}
	if ob2.FirstSettledAt == nil || ob2.FirstSettledAt.Unix() != paidTime {
		t.Fatalf("after invoice.paid: FirstSettledAt = %v, want unix %d", ob2.FirstSettledAt, paidTime)
	}

	var payment billing.Payment
	if err := db.Where("contract_id = ? AND id = ?", contract.ID, "pi_"+invID2).First(&payment).Error; err != nil {
		t.Fatalf("expected billing_payment row for %s: %v", invID2, err)
	}
	if payment.ID != "pi_"+invID2 {
		t.Fatalf("payment ID = %s, want %s", payment.ID, "pi_"+invID2)
	}

	// 4. Test idempotency: replay same invoice.paid webhook
	postWebhook("evt_inv2_paid_replay", "invoice.paid", invID2)
	svc.DrainForTest(t.Context())

	var paymentCount int64
	db.Model(&billing.Payment{}).Where("contract_id = ?", contract.ID).Count(&paymentCount)
	if paymentCount != 2 { // 1 from checkout, 1 from recurring payment
		t.Fatalf("replayed webhook changed payment count: count = %d, want 2", paymentCount)
	}

	// 5. Test recurring payment failure on installment 3
	invID3 := *ob3.StripeInvoiceID
	mock.patchInvoice(invID3, map[string]any{"status": "open", "attempted": true, "amount_remaining": int64(20833)})

	postWebhook("evt_inv3_failed", "invoice.payment_failed", invID3)
	svc.DrainForTest(t.Context())

	db.First(&ob3, ob3.ID)
	if !ob3.FailureOrActionRequired {
		t.Fatalf("after invoice.payment_failed: expected FailureOrActionRequired = true")
	}
	if ob3.LatestStripeStatus == nil || *ob3.LatestStripeStatus != "open" {
		t.Fatalf("after invoice.payment_failed: status = %v, want open", ob3.LatestStripeStatus)
	}

	// 6. Verify Overview figures
	overview, err := svc.Overview(t.Context(), time.Now().UTC().Add(6*time.Minute), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if overview.CollectedCents != 41668 { // 20834 (checkout) + 20834 (mock pi #2)
		t.Fatalf("CollectedCents = %d, want 41668", overview.CollectedCents)
	}
	if overview.OutstandingCents != 20833 { // installment #3 still remaining
		t.Fatalf("OutstandingCents = %d, want 20833", overview.OutstandingCents)
	}
	if overview.FailedInstallments != 1 { // installment #3 failed
		t.Fatalf("FailedInstallments = %d, want 1", overview.FailedInstallments)
	}
}

// TestFailedInstallmentIsPaidAgain pins the retry policy: a soft decline is paid again after the
// delay up to the limit, a recovered card settles on the retry, and a decline only the payer can
// fix is never charged again.
func TestFailedInstallmentIsPaidAgain(t *testing.T) {
	type fixture struct {
		mock *workerMockServer
		o    billing.Obligation
		// due makes the retry due now and runs one worker pass, then reloads o.
		due func()
	}
	// failed funds a "three" contract and has Stripe fail installment 2 with decline code.
	failed := func(t *testing.T, code string) *fixture {
		t.Helper()
		mock := newWorkerMockServer(time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC), 20834)
		db, _, contract, attempt := seedThreePlanCheckout(t)
		svc := billingtest.New(t, db, mock.mux)
		mustCreate(t, db, &billing.WebhookEvent{ID: "evt_checkout_completed", EventType: "checkout.session.completed", ResourceID: *attempt.StripeSessionID})
		svc.DrainForTest(t.Context())

		f := &fixture{mock: mock}
		if err := db.Where("contract_id = ? AND number = 2", contract.ID).First(&f.o).Error; err != nil {
			t.Fatalf("load installment 2: %v", err)
		}
		invoice := *f.o.StripeInvoiceID
		mock.patchInvoice(invoice, map[string]any{"status": "open", "attempted": true, "amount_remaining": int64(20833)})
		mock.mu.Lock()
		mock.declines[invoice] = code
		mock.mu.Unlock()
		mustCreate(t, db, &billing.WebhookEvent{ID: "evt_installment_failed", EventType: "invoice.payment_failed", ResourceID: invoice})
		svc.DrainForTest(t.Context())
		db.Take(&f.o, "id = ?", f.o.ID)

		// due fires a pending retry now; with none pending (NULL) nothing is due, as in production.
		f.due = func() {
			db.Model(&billing.Obligation{}).Where("id = ? AND next_retry_at IS NOT NULL", f.o.ID).Update("next_retry_at", time.Now().Add(-time.Minute))
			db.Model(&billing.Contract{}).Where("id = ?", f.o.ContractID).Update("next_sync_at", nil)
			svc.DrainForTest(t.Context())
			var o billing.Obligation // a fresh struct: scanning NULL would not clear a pointer already set
			db.Take(&o, "id = ?", f.o.ID)
			f.o = o
		}
		return f
	}
	keys := func(f *fixture, n int) []string {
		var want []string
		for i := 1; i <= n; i++ {
			want = append(want, fmt.Sprintf("tuition:%s:retry:%d", f.o.Reference, i))
		}
		return want
	}
	payCalls := func(f *fixture) []string {
		f.mock.mu.Lock()
		defer f.mock.mu.Unlock()
		return slices.Clone(f.mock.payCalls)
	}
	inAbout := func(t time.Time, d time.Duration) bool {
		return t.After(time.Now().Add(d-time.Hour)) && t.Before(time.Now().Add(d+time.Hour))
	}

	t.Run("soft decline is retried up to the limit", func(t *testing.T) {
		f := failed(t, "insufficient_funds")
		if !f.o.FailureOrActionRequired || f.o.NextRetryAt == nil || !inAbout(*f.o.NextRetryAt, 24*time.Hour) || len(payCalls(f)) != 0 {
			t.Fatalf("after the failure: failed=%v next=%v pays=%v, want failed, a retry about 24h out, no pay yet", f.o.FailureOrActionRequired, f.o.NextRetryAt, payCalls(f))
		}
		f.due()
		if got := payCalls(f); !slices.Equal(got, keys(f, 1)) || f.o.RetryCount != 1 || f.o.NextRetryAt == nil || !inAbout(*f.o.NextRetryAt, 24*time.Hour) {
			t.Fatalf("after retry 1: pays=%v count=%d next=%v, want %v, 1 and the next retry scheduled", got, f.o.RetryCount, f.o.NextRetryAt, keys(f, 1))
		}
		f.due()
		if got := payCalls(f); !slices.Equal(got, keys(f, 2)) || f.o.RetryCount != 2 || f.o.NextRetryAt != nil {
			t.Fatalf("after retry 2: pays=%v count=%d next=%v, want %v, 2 and no further retry", got, f.o.RetryCount, f.o.NextRetryAt, keys(f, 2))
		}
		f.due()
		if got := payCalls(f); len(got) != 2 {
			t.Fatalf("a spent retry budget paid again: %v", got)
		}
	})

	t.Run("recovered card settles on the retry", func(t *testing.T) {
		f := failed(t, "generic_decline")
		f.mock.mu.Lock()
		f.mock.payPays = true
		f.mock.mu.Unlock()
		f.due()
		if got := payCalls(f); !slices.Equal(got, keys(f, 1)) {
			t.Fatalf("pays = %v, want %v", got, keys(f, 1))
		}
		if f.o.FirstSettledAt == nil || f.o.FailureOrActionRequired || f.o.NextRetryAt != nil || f.o.AmountRemainingCents != 0 {
			t.Fatalf("after the paid retry: settled=%v failed=%v next=%v remaining=%d, want settled, not failed, no retry, nothing remaining",
				f.o.FirstSettledAt, f.o.FailureOrActionRequired, f.o.NextRetryAt, f.o.AmountRemainingCents)
		}
	})

	t.Run("decline only the payer can fix is not charged again", func(t *testing.T) {
		f := failed(t, "stolen_card")
		f.due()
		f.due()
		if got := payCalls(f); len(got) != 0 || f.o.NextRetryAt != nil || !f.o.FailureOrActionRequired {
			t.Fatalf("stolen card: pays=%v next=%v failed=%v, want no pay, no retry, still failed", got, f.o.NextRetryAt, f.o.FailureOrActionRequired)
		}
	})
}

func TestRefundsAndFraudSignalsSurfaceWithoutWebhook(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 60000)
	allModels := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	now := time.Now().UTC()
	enrollment := links.Enrollment{
		LinkID:      seedLink(t, db),
		SessionHash: tokenHash,
		Details: students.Details{
			Name:  "Financial Student",
			Email: "fin@example.com",
		},
		CreatedAt:   now,
		SubmittedAt: &now,
		PaidAt:      &now,
		ExpiresAt:   now.Add(7 * 24 * time.Hour),
	}
	mustCreate(t, db, &enrollment)
	student := students.Student{
		EnrollmentID: &enrollment.ID,
		Details:      enrollment.Details,
		IsActive:     true,
		Cohort:       "2026-11",
	}
	mustCreate(t, db, &student)
	cusID := "cus_worker_test"
	contract := billing.Contract{
		EnrollmentID:     enrollment.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		TotalCents:       60000,
		FundedAt:         &now,
		StripeCustomerID: &cusID,
	}
	mustCreate(t, db, &contract)
	chID := "ch_mock_1"
	payment := billing.Payment{
		ID:                 "pi_worker_test",
		ContractID:         contract.ID,
		StripeChargeID:     &chID,
		GrossReceivedCents: 60000,
		GrossRefundedCents: 0,
		PaymentTime:        now,
	}
	mustCreate(t, db, &payment)

	invID := "in_initial_paid"
	obligation := billing.Obligation{
		ContractID:            contract.ID,
		Reference:             "ref_full_obl",
		Kind:                  "tuition",
		Number:                1,
		AmountCents:           60000,
		AmountPaidCents:       60000,
		AmountRemainingCents:  0,
		StripeInvoiceID:       &invID,
		FirstSettledAt:        &now,
		CollectionDisposition: "collect",
	}
	mustCreate(t, db, &obligation)

	svc := billingtest.New(t, db, mock.mux)

	// Verify initial summary: 60000 paid, 0 remaining
	summary, err := svc.ForStudent(context.Background(), student.ID)
	if err != nil || summary == nil {
		t.Fatalf("ForStudent: %v", err)
	}
	if summary.CashPaidCents != 60000 || summary.RemainingTuitionCents != 0 {
		t.Fatalf("unexpected summary before adjustment: %+v", summary)
	}

	// stored reads back the payment and the contract's attention reason
	stored := func() (p billing.Payment, attention string) {
		db.First(&p, "id = ?", payment.ID)
		var c billing.Contract
		db.First(&c, contract.ID)
		if c.ReviewReason != nil {
			attention = *c.ReviewReason
		}
		return p, attention
	}

	// 1. Stripe reports a partial refund; the refund.updated event makes the worker re-read the payment
	mock.mu.Lock()
	mock.refundedCents = 10000
	mock.mu.Unlock()
	mustCreate(t, db, &billing.WebhookEvent{
		ID:         "evt_refund_1",
		EventType:  "refund.updated",
		ResourceID: "re_test_1",
	})
	svc.DrainForTest(t.Context())

	p, attention := stored()
	if p.GrossRefundedCents != 10000 {
		t.Fatalf("expected GrossRefundedCents 10000 after refund, got %d", p.GrossRefundedCents)
	}
	if attention != "payment_review" {
		t.Fatalf("expected attention reason payment_review after refund, got %q", attention)
	}
	// CashPaid is now 50000 (gross received - refunded)
	summary, err = svc.ForStudent(t.Context(), student.ID)
	if err != nil || summary == nil {
		t.Fatalf("ForStudent: %v", err)
	}
	if summary.CashPaidCents != 50000 {
		t.Fatalf("expected CashPaidCents 50000 after refund, got %d", summary.CashPaidCents)
	}

	// 2. Daily audit discovers early fraud warning without webhook
	mock.mu.Lock()
	mock.earlyWarningsReturned = []map[string]any{
		{
			"id":     "issfr_mock_1",
			"object": "radar.early_fraud_warning",
			"charge": "ch_mock_1",
			"payment_intent": map[string]any{
				"id": "pi_worker_test",
			},
		},
	}
	mock.mu.Unlock()
	db.Model(&contract).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	p, attention = stored()
	if p.DisputeOrFraudReviewState == nil || *p.DisputeOrFraudReviewState != "fraud_warning" {
		t.Fatal("expected DisputeOrFraudReviewState fraud_warning to be recorded by audit")
	}
	if attention != "payment_review" {
		t.Fatalf("expected attention reason to stay payment_review, got %q", attention)
	}
}

// seedFundedStudent seeds a paid Enrollment, its active Student and a funded "three" contract
// on a Stripe Customer of its own.
func seedFundedStudent(t *testing.T, db *gorm.DB, name, email string) (students.Student, billing.Contract) {
	t.Helper()
	now := time.Now().UTC()
	tokenHash := make([]byte, 32)
	rand.Read(tokenHash)
	enrollment := links.Enrollment{
		LinkID:      seedLink(t, db),
		SessionHash: tokenHash,
		Details:     students.Details{Name: name, Email: email},
		CreatedAt:   now, SubmittedAt: &now, PaidAt: &now,
		ExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	mustCreate(t, db, &enrollment)
	student := students.Student{
		EnrollmentID: &enrollment.ID,
		Details:      enrollment.Details,
		IsActive:     true,
		Cohort:       "2026-11",
	}
	mustCreate(t, db, &student)
	cusID := "cus_worker_" + rand.Text()[:8]
	contract := billing.Contract{
		EnrollmentID:     enrollment.ID,
		Reference:        rand.Text(),
		PlanID:           "three",
		PlanName:         "Plan de 3 Pagos Mensuales",
		InstallmentCount: 3,
		TotalCents:       60000,
		FundedAt:         &now,
		StripeCustomerID: &cusID,
	}
	mustCreate(t, db, &contract)
	return student, contract
}

func TestSuspensionAndReinstatement(t *testing.T) {
	paidAt := time.Date(2026, time.November, 15, 4, 0, 0, 0, time.UTC)
	mock := newWorkerMockServer(paidAt, 20834)

	allModels := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)
	db := testdb.Open(t, allModels...)

	svc := billingtest.New(t, db, mock.mux)

	now := time.Now().UTC()

	// 1. Boundary: unpaid obligation 13 days 23 hours past due -> NOT suspended
	student1, contract1 := seedFundedStudent(t, db, "Near Boundary", "near@example.com")
	dueNear := now.Add(-13*24*time.Hour - 23*time.Hour)
	invID1 := "in_boundary_1"
	mock.registerInvoice(invID1, 20833, "open", *contract1.StripeCustomerID)
	obNear := billing.Obligation{
		ContractID:            contract1.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID1,
		DueAt:                 &dueNear,
		CollectionDisposition: "collect",
	}
	mustCreate(t, db, &obNear)

	// Run worker sweep for contract 1
	svc.DrainForTest(t.Context())

	var checkStudent1 students.Student
	db.First(&checkStudent1, student1.ID)
	if !checkStudent1.IsActive {
		t.Fatal("expected student to remain active when 14-day deadline has not passed")
	}
	var suspCount1 int64
	db.Model(&billing.Suspension{}).Where("contract_id = ?", contract1.ID).Count(&suspCount1)
	if suspCount1 != 0 {
		t.Fatalf("expected 0 suspensions, got %d", suspCount1)
	}

	// 2. Just past the 14-day boundary: unpaid obligation 14 days 1 hour past due -> SUSPENDED
	student2, contract2 := seedFundedStudent(t, db, "Past Boundary", "past@example.com")
	duePast := now.Add(-14*24*time.Hour - time.Hour)
	invID2 := "in_boundary_2"
	mock.registerInvoice(invID2, 20833, "open", *contract2.StripeCustomerID)
	obPast := billing.Obligation{
		ContractID:            contract2.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID2,
		DueAt:                 &duePast,
		CollectionDisposition: "collect",
	}
	mustCreate(t, db, &obPast)

	// Also add installment 3 (future invoice) to verify future invoices remain scheduled/armed
	dueFuture := now.Add(15 * 24 * time.Hour)
	invID3 := "in_future_3"
	mock.registerInvoice(invID3, 20833, "draft", *contract2.StripeCustomerID)
	obFuture := billing.Obligation{
		ContractID:            contract2.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                3,
		AmountCents:           20833,
		AmountRemainingCents:  20833,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invID3,
		DueAt:                 &dueFuture,
		CollectionDisposition: "collect",
	}
	mustCreate(t, db, &obFuture)

	svc.DrainForTest(t.Context())

	var checkStudent2 students.Student
	db.First(&checkStudent2, student2.ID)
	if checkStudent2.IsActive {
		t.Fatal("expected student to be suspended (is_active = false) after 14-day deadline passed")
	}
	var openSusp billing.Suspension
	if err := db.Where("contract_id = ? AND closed_at IS NULL", contract2.ID).First(&openSusp).Error; err != nil {
		t.Fatalf("expected open suspension for contract 2: %v", err)
	}
	if openSusp.TriggeringTuitionInstallmentNumber != 2 {
		t.Fatalf("expected triggering installment 2, got %d", openSusp.TriggeringTuitionInstallmentNumber)
	}

	// Verify obligation has DeadlineEpisodeID attributed
	var updatedObPast billing.Obligation
	db.First(&updatedObPast, obPast.ID)
	if updatedObPast.DeadlineEpisodeID == nil || *updatedObPast.DeadlineEpisodeID != openSusp.ID {
		t.Fatalf("expected DeadlineEpisodeID to match suspension ID %d", openSusp.ID)
	}

	// 3. No fee before staff request
	var feeCount int64
	db.Model(&billing.Obligation{}).Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).Count(&feeCount)
	if feeCount != 0 {
		t.Fatalf("expected 0 fee obligations before staff request, got %d", feeCount)
	}

	// 4. Request reinstatement by staff
	summary, err := svc.RequestReinstatement(context.Background(), student2.ID, "director_carmen")
	if err != nil {
		t.Fatalf("RequestReinstatement failed: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary after RequestReinstatement")
	}

	// Verify fee obligation was created and invoiced
	var feeOb billing.Obligation
	if err := db.Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).First(&feeOb).Error; err != nil {
		t.Fatalf("expected fee obligation created: %v", err)
	}
	if feeOb.AmountCents != 2500 || feeOb.AmountRemainingCents != 2500 {
		t.Fatalf("expected fee amount 2500 cents, got %d", feeOb.AmountCents)
	}
	if feeOb.StripeInvoiceID == nil {
		t.Fatal("expected fee obligation to have StripeInvoiceID")
	}

	// Repeated request reuses the episode and single fee obligation (idempotent)
	summary2, err := svc.RequestReinstatement(context.Background(), student2.ID, "director_carmen")
	if err != nil {
		t.Fatalf("repeated RequestReinstatement failed: %v", err)
	}
	if summary2 == nil {
		t.Fatal("expected summary from repeated request")
	}
	db.Model(&billing.Obligation{}).Where("contract_id = ? AND kind = 'reinstatement'", contract2.ID).Count(&feeCount)
	if feeCount != 1 {
		t.Fatalf("expected exactly 1 fee obligation after repeated request, got %d", feeCount)
	}

	// 5. Fee paid alone cannot reactivate if overdue tuition remains unpaid
	mock.patchInvoice(*feeOb.StripeInvoiceID, map[string]any{
		"status": "paid", "amount_paid": int64(2500), "amount_remaining": int64(0),
		"status_transitions": map[string]any{"paid_at": now.Unix()},
	})
	db.Model(&contract2).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	db.First(&checkStudent2, student2.ID)
	if checkStudent2.IsActive {
		t.Fatal("student should NOT reactivate when overdue tuition is still unpaid, even if fee is paid")
	}

	// 6. Full required settlement (overdue tuition + fee) auto-reactivates student
	mock.patchInvoice(invID2, map[string]any{
		"status": "paid", "amount_paid": int64(20833), "amount_remaining": int64(0),
		"status_transitions": map[string]any{"paid_at": now.Unix()},
	})
	db.Model(&contract2).Update("next_sync_at", nil)
	svc.DrainForTest(t.Context())

	db.First(&checkStudent2, student2.ID)
	if !checkStudent2.IsActive {
		t.Fatal("expected student to be reactivated (is_active = true) after all overdue tuition and fee are paid")
	}
	var closedSusp billing.Suspension
	db.First(&closedSusp, openSusp.ID)
	if closedSusp.ClosedAt == nil || closedSusp.CloseReason == nil || *closedSusp.CloseReason != "reinstated" {
		t.Fatalf("expected suspension closed with reason 'reinstated', got %+v", closedSusp)
	}

	// Verify future obligation remains scheduled
	var checkFuture billing.Obligation
	db.First(&checkFuture, obFuture.ID)
	if checkFuture.AmountRemainingCents != 20833 {
		t.Fatal("future installment should remain scheduled and owing")
	}

	// 7. On-time payment with late webhook does not penalize
	student3, contract3 := seedFundedStudent(t, db, "On-time late webhook", "ontime@example.com")
	dueOld := now.Add(-30 * 24 * time.Hour)
	onTimeSettle := dueOld.Add(5 * 24 * time.Hour) // settled 5 days after due, well before 14-day deadline
	invIDOnTime := "in_ontime"
	mock.registerInvoice(invIDOnTime, 20833, "paid", *contract3.StripeCustomerID)
	obOnTime := billing.Obligation{
		ContractID:            contract3.ID,
		Reference:             rand.Text(),
		Kind:                  "tuition",
		Number:                2,
		AmountCents:           20833,
		AmountPaidCents:       20833,
		AmountRemainingCents:  0,
		FirstSettledAt:        &onTimeSettle,
		StripePriceID:         "pr_three_2",
		StripeInvoiceID:       &invIDOnTime,
		DueAt:                 &dueOld,
		CollectionDisposition: "collect",
	}
	mustCreate(t, db, &obOnTime)

	svc.DrainForTest(t.Context())

	var checkStudent3 students.Student
	db.First(&checkStudent3, student3.ID)
	if !checkStudent3.IsActive {
		t.Fatal("on-time payment settled before 14-day deadline should NOT suspend student")
	}
}

// A suspension proves false once Stripe reports its installment paid on time, however late it says
// so. The episode is corrected, the student reactivated and the requested fee cancelled at Stripe
// exactly once. A paid fee is never voided; it is flagged for refund review. An uncertain create is
// located by its reference and cancelled, but never blindly when several invoices match.
func TestCorrectedEpisodeCancelsFee(t *testing.T) {
	// forget is a crash after Stripe created the fee's draft but before its ID was stored.
	forget := func(t *testing.T, db *gorm.DB, mock *workerMockServer, fee billing.Obligation) {
		t.Helper()
		mock.patchInvoice(*fee.StripeInvoiceID, map[string]any{"status": "draft"})
		if err := db.Model(&fee).Updates(map[string]any{
			"stripe_invoice_id": nil, "stripe_invoice_item_id": nil, "item_creation_started_at": nil,
			"scheduled_finalization_at": nil, "latest_stripe_status": nil,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		// prepare moves the requested fee, finalized and emailed, to the state the correction finds.
		prepare        func(t *testing.T, db *gorm.DB, mock *workerMockServer, fee billing.Obligation, customer string)
		cancel         string // Stripe call on the fee invoice: "void", "delete" or none
		status, review string // the fee's stored Stripe status and review reason
	}{{
		name: "open fee is voided",
		prepare: func(_ *testing.T, _ *gorm.DB, mock *workerMockServer, fee billing.Obligation, _ string) {
			mock.patchInvoice(*fee.StripeInvoiceID, map[string]any{"amount_remaining": int64(2500)})
		},
		cancel: "void", status: "void",
	}, {
		name: "uncertain create is found and its draft deleted",
		prepare: func(t *testing.T, db *gorm.DB, mock *workerMockServer, fee billing.Obligation, _ string) {
			forget(t, db, mock, fee)
		},
		cancel: "delete", status: "deleted",
	}, {
		name: "paid fee is kept for refund review",
		prepare: func(_ *testing.T, _ *gorm.DB, mock *workerMockServer, fee billing.Obligation, _ string) {
			mock.patchInvoice(*fee.StripeInvoiceID, map[string]any{
				"status": "paid", "amount_paid": int64(2500), "amount_remaining": int64(0),
				"status_transitions": map[string]any{"paid_at": time.Now().Unix()},
			})
		},
		status: "paid", review: "payment_review",
	}, {
		name: "several uncertain creates are reviewed and none cancelled",
		prepare: func(t *testing.T, db *gorm.DB, mock *workerMockServer, fee billing.Obligation, customer string) {
			mock.registerInvoice("in_fee_twin", 2500, "draft", customer)
			mock.patchInvoice("in_fee_twin", map[string]any{"metadata": map[string]any{"tuition_obligation": fee.Reference}})
			forget(t, db, mock, fee)
		},
		review: "unresolved_provider_create",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newWorkerMockServer(time.Now(), 20834)
			db := testdb.Open(t, append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)...)
			svc := billingtest.New(t, db, mock.mux)
			student, contract := seedFundedStudent(t, db, "Corrected Episode", "corrected@example.com")
			customer := *contract.StripeCustomerID
			sync := func() {
				t.Helper()
				if err := db.Model(&contract).Update("next_sync_at", nil).Error; err != nil {
					t.Fatal(err)
				}
				svc.DrainForTest(t.Context())
			}

			// Installment 2 reads unpaid past its 14-day window: the student is suspended and staff request the fee.
			due := time.Now().UTC().Add(-30 * 24 * time.Hour)
			tuitionInvoice := "in_tuition_2"
			mock.registerInvoice(tuitionInvoice, 20833, "open", customer)
			mustCreate(t, db, &billing.Obligation{
				ContractID:            contract.ID,
				Reference:             rand.Text(),
				Kind:                  "tuition",
				Number:                2,
				AmountCents:           20833,
				AmountRemainingCents:  20833,
				StripePriceID:         "pr_three_2",
				StripeInvoiceID:       &tuitionInvoice,
				DueAt:                 &due,
				CollectionDisposition: "collect",
			})
			sync()
			if _, err := svc.RequestReinstatement(t.Context(), student.ID, "director_carmen"); err != nil {
				t.Fatalf("RequestReinstatement: %v", err)
			}
			var fee billing.Obligation
			if err := db.Where("contract_id = ? AND kind = 'reinstatement'", contract.ID).Take(&fee).Error; err != nil {
				t.Fatal(err)
			}
			feeInvoice := *fee.StripeInvoiceID
			tc.prepare(t, db, mock, fee, customer)

			// Stripe now reports installment 2 paid five days after its due date: the suspension was false.
			mock.patchInvoice(tuitionInvoice, map[string]any{
				"status": "paid", "amount_paid": int64(20833), "amount_remaining": int64(0),
				"status_transitions": map[string]any{"paid_at": due.Add(5 * 24 * time.Hour).Unix()},
			})
			settled := func() {
				t.Helper()
				var episode billing.Suspension
				if err := db.Where("contract_id = ?", contract.ID).Take(&episode).Error; err != nil {
					t.Fatal(err)
				}
				if episode.ClosedAt == nil || episode.CloseReason == nil || *episode.CloseReason != "corrected" {
					t.Fatalf("episode closed %v with reason %v, want corrected", episode.ClosedAt, episode.CloseReason)
				}
				if err := db.First(&student, student.ID).Error; err != nil || !student.IsActive {
					t.Fatalf("student active = %v (err %v), want reactivated", student.IsActive, err)
				}
				if err := db.First(&fee, fee.ID).Error; err != nil {
					t.Fatal(err)
				}
				text := func(p *string) string {
					if p == nil {
						return ""
					}
					return *p
				}
				if fee.CollectionDisposition != "cancel" || text(fee.LatestStripeStatus) != tc.status || text(fee.ReviewReason) != tc.review {
					t.Fatalf("fee disposition/status/review = %s/%q/%q, want cancel/%q/%q",
						fee.CollectionDisposition, text(fee.LatestStripeStatus), text(fee.ReviewReason), tc.status, tc.review)
				}
				var want []string
				if tc.cancel != "" {
					want = []string{tc.cancel + " " + feeInvoice}
					if fee.AmountRemainingCents != 0 {
						t.Fatalf("cancelled fee still owes %d cents", fee.AmountRemainingCents)
					}
				}
				mock.mu.Lock()
				cancels := slices.Clone(mock.cancels)
				mock.mu.Unlock()
				if !slices.Equal(cancels, want) {
					t.Fatalf("Stripe cancels = %v, want %v", cancels, want)
				}
			}
			sync()
			settled() // the correcting pass itself finishes the fee, so staff see a refund review at once
			sync()
			settled() // a later pass changes nothing and never cancels again
		})
	}
}

// TestWithdrawalRefundsAndStopsCollection: a withdrawal refunds what the payer paid, cancels every
// unsettled invoice and ends the student, and repeating it moves no money again.
func TestWithdrawalRefundsAndStopsCollection(t *testing.T) {
	mock := newWorkerMockServer(time.Now(), 20834)
	db := testdb.Open(t, append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)...)
	svc := billingtest.New(t, db, mock.mux)
	student, contract := seedFundedStudent(t, db, "Withdrawn Student", "withdrawn@example.com")
	customer := *contract.StripeCustomerID
	now := time.Now().UTC()

	seed := func(number int, amount int64, invoice, status string, due time.Time) {
		t.Helper()
		item := fmt.Sprintf("ii_w_%d", number)
		o := billing.Obligation{
			ContractID: contract.ID, Reference: rand.Text(), Kind: "tuition", Number: number,
			AmountCents: amount, StripePriceID: "pr_three", StripeInvoiceID: &invoice, StripeInvoiceItemID: &item,
			DueAt: &due, ScheduledFinalizationAt: &due, CollectionDisposition: "collect",
			LatestStripeStatus: &status, AmountRemainingCents: amount,
		}
		mock.registerInvoice(invoice, amount, status, customer)
		if status == "paid" {
			o.AmountPaidCents, o.AmountRemainingCents, o.FirstSettledAt = amount, 0, &due
			mock.patchInvoice(invoice, map[string]any{"status_transitions": map[string]any{"paid_at": due.Unix()}})
		}
		mustCreate(t, db, &o)
	}
	seed(1, 20834, "in_w_1", "paid", now.Add(-30*24*time.Hour))
	seed(2, 20833, "in_w_2", "open", now.Add(-24*time.Hour))
	seed(3, 20833, "in_w_3", "draft", now.Add(30*24*time.Hour))

	summary, err := svc.Withdraw(t.Context(), student.ID, "director_carmen")
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	moved := func() (refunds, cancels []string) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return slices.Clone(mock.refunds), slices.Clone(mock.cancels)
	}
	refunds, cancels := moved()
	if len(refunds) != 1 || !strings.HasPrefix(refunds[0], "pi_in_w_1 20834 withdrawal:") {
		t.Fatalf("refunds = %v, want one full refund of pi_in_w_1", refunds)
	}
	slices.Sort(cancels)
	if want := []string{"delete in_w_3", "void in_w_2"}; !slices.Equal(cancels, want) {
		t.Fatalf("Stripe cancels = %v, want %v", cancels, want)
	}

	var obligations []billing.Obligation
	if err := db.Where("contract_id = ?", contract.ID).Order("number ASC").Find(&obligations).Error; err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ disposition, status string }{{"collect", "paid"}, {"cancel", "void"}, {"cancel", "deleted"}} {
		o := obligations[i]
		if o.CollectionDisposition != want.disposition || *o.LatestStripeStatus != want.status || (i > 0 && o.AmountRemainingCents != 0) {
			t.Fatalf("installment %d = %s/%s remaining %d, want %s/%s", i+1, o.CollectionDisposition, *o.LatestStripeStatus, o.AmountRemainingCents, want.disposition, want.status)
		}
	}
	if err := db.First(&student, student.ID).Error; err != nil || student.IsActive || student.WithdrawnAt == nil {
		t.Fatalf("student active = %v, withdrawn at %v (err %v), want deactivated and withdrawn", student.IsActive, student.WithdrawnAt, err)
	}
	var stored billing.Contract
	if err := db.First(&stored, contract.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.WithdrawnBy == nil || *stored.WithdrawnBy != "director_carmen" || stored.WithdrawnAt == nil || stored.ReviewReason != nil {
		t.Fatalf("contract withdrawn by %v at %v review %v, want director_carmen, set, none", stored.WithdrawnBy, stored.WithdrawnAt, stored.ReviewReason)
	}
	var payment billing.Payment
	if err := db.First(&payment, "id = ?", "pi_in_w_1").Error; err != nil || payment.GrossRefundedCents != 20834 {
		t.Fatalf("payment refunded %d (err %v), want 20834", payment.GrossRefundedCents, err)
	}

	if summary.CashPaidCents != 0 || summary.RefundableCents != 0 || summary.RemainingTuitionCents != 0 || summary.WithdrawnBy != "director_carmen" {
		t.Fatalf("summary cash %d refundable %d remaining %d by %q, want 0, 0, 0, director_carmen",
			summary.CashPaidCents, summary.RefundableCents, summary.RemainingTuitionCents, summary.WithdrawnBy)
	}
	if len(summary.Installments) != 3 || summary.Installments[1].Status != "cancelled" || summary.Installments[2].Status != "cancelled" {
		t.Fatalf("installments = %+v, want 2 and 3 cancelled", summary.Installments)
	}
	if len(summary.Payments) != 1 || summary.Payments[0].DashboardURL != "https://dashboard.stripe.com/test/payments/pi_in_w_1" {
		t.Fatalf("payments = %+v, want pi_in_w_1 with its test Dashboard link", summary.Payments)
	}

	// Repeating the withdrawal, or any sync, moves no money again and keeps who withdrew and when.
	if _, err := svc.ReconcileStudent(t.Context(), student.ID); err != nil {
		t.Fatalf("ReconcileStudent: %v", err)
	}
	if _, err := svc.Withdraw(t.Context(), student.ID, "someone_else"); err != nil {
		t.Fatalf("second Withdraw: %v", err)
	}
	againRefunds, againCancels := moved()
	var after billing.Contract
	if err := db.First(&after, contract.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(againRefunds) != len(refunds) || len(againCancels) != len(cancels) ||
		!after.WithdrawnAt.Equal(*stored.WithdrawnAt) || *after.WithdrawnBy != "director_carmen" {
		t.Fatalf("repeat refunds %v cancels %v withdrawn %v by %q, want nothing new and the first withdrawal kept",
			againRefunds, againCancels, after.WithdrawnAt, *after.WithdrawnBy)
	}
}

func TestWorkerSyncsCustomerContact(t *testing.T) {
	mock := newMockStripeServer()
	db, svc, enrollmentID, _ := setupTestEnvironment(t, mock)
	ctx := context.Background()

	// Start checkout creates the customer in Stripe and stamps synced columns
	res, err := svc.StartCheckout(ctx, enrollmentID, "three")
	if err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if res.URL == "" {
		t.Fatalf("expected checkout URL, got %+v", res)
	}

	var c billing.Contract
	if err := db.Where("enrollment_id = ?", enrollmentID).Take(&c).Error; err != nil {
		t.Fatalf("take contract: %v", err)
	}
	if c.StripeCustomerID == nil || *c.StripeCustomerID == "" {
		t.Fatal("expected StripeCustomerID to be set on contract")
	}
	if c.SyncedCustomerName != "Maria Santos" || c.SyncedCustomerEmail != "maria@example.com" {
		t.Fatalf("initial stamps wrong: %s, %s", c.SyncedCustomerName, c.SyncedCustomerEmail)
	}

	// 1. Unchanged contact => drain produces 0 customer updates
	svc.TouchContract(ctx, &enrollmentID)
	svc.DrainForTest(ctx)
	mock.with(func() {
		if len(mock.customerUpdates) != 0 {
			t.Fatalf("expected 0 customer updates, got %d", len(mock.customerUpdates))
		}
	})

	// 2. Customer changes email on the Enrollment before funding
	// Contract is unfunded, but customer syncs durable to Stripe from the Enrollment
	if err := db.Model(&links.Enrollment{}).Where("id = ?", enrollmentID).Update("email", "maria.updated@example.com").Error; err != nil {
		t.Fatalf("update enrollment: %v", err)
	}
	svc.TouchContract(ctx, &enrollmentID)
	// One worker drain records exactly one customer update with new email
	svc.DrainForTest(ctx)
	var updates []customerUpdate
	mock.with(func() {
		updates = append([]customerUpdate(nil), mock.customerUpdates...)
	})
	if len(updates) != 1 {
		t.Fatalf("expected 1 customer update, got %d", len(updates))
	}
	wantUpdate := customerUpdate{ID: *c.StripeCustomerID, Name: "Maria Santos", Email: "maria.updated@example.com"}
	if updates[0] != wantUpdate {
		t.Fatalf("customer update = %+v, want %+v", updates[0], wantUpdate)
	}

	// DB contract stamp equals the Enrollment's contact
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerEmail != "maria.updated@example.com" {
		t.Fatalf("stamp not updated in DB: %s", c.SyncedCustomerEmail)
	}

	// 3. Mock fails update => contract stamp stays old and next drain succeeds
	if err := db.Model(&links.Enrollment{}).Where("id = ?", enrollmentID).Update("name", "Maria Rivera").Error; err != nil {
		t.Fatalf("update enrollment name: %v", err)
	}
	svc.TouchContract(ctx, &enrollmentID)
	mock.with(func() {
		mock.customerUpdateError = true
	})
	svc.DrainForTest(ctx)

	// Stamp in DB still has old name because update failed
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerName != "Maria Santos" {
		t.Fatalf("stamp should not be updated on failure: %s", c.SyncedCustomerName)
	}

	// Reset mock error and bump next_sync_at to now for next pass
	mock.with(func() {
		mock.customerUpdateError = false
	})
	svc.TouchContract(ctx, &enrollmentID)
	svc.DrainForTest(ctx)

	// Next drain succeeded
	mock.with(func() {
		updates = append([]customerUpdate(nil), mock.customerUpdates...)
	})
	if len(updates) != 2 {
		t.Fatalf("expected 2 total customer updates, got %d", len(updates))
	}
	wantSecond := customerUpdate{ID: *c.StripeCustomerID, Name: "Maria Rivera", Email: "maria.updated@example.com"}
	if updates[1] != wantSecond {
		t.Fatalf("second update = %+v, want %+v", updates[1], wantSecond)
	}
	if err := db.Where("id = ?", c.ID).Take(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.SyncedCustomerName != "Maria Rivera" {
		t.Fatalf("stamp not updated after retry: %s", c.SyncedCustomerName)
	}
}

//go:build integration

package billing_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/links"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

func TestBillingSafeCancelEnrollment(t *testing.T) {
	allModels := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}}, billing.Models()...)
	now := time.Now().UTC()

	setup := func(t *testing.T, handler http.HandlerFunc) (*gorm.DB, *links.Store, *billing.Service, links.LinkEntry) {
		t.Helper()
		db := testdb.Open(t, allModels...)
		store := links.New(db, nil)
		biller := billingtest.New(t, db, handler)
		link, err := store.CreateLink(t.Context(), "staff", "Cohort", now.Add(7*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return db, store, biller, link
	}

	submitWithPin := func(t *testing.T, store *links.Store, linkToken, name, pin string) (uint64, string) {
		t.Helper()
		e, cookie, err := store.StartEnrollment(t.Context(), linkToken, "")
		if err != nil || e == nil {
			t.Fatalf("start: %v, %v", e, err)
		}
		details := students.Details{Name: name, Email: "test@example.com", Phone: "+17875551234", Municipality: "San Juan", DateOfBirth: "2005-06-15", Gender: "Female"}
		if res, err := store.Submit(t.Context(), linkToken, cookie, details, pin); err != nil || res != links.Saved {
			t.Fatalf("submit: %v, %v", res, err)
		}
		return e.ID, cookie
	}

	t.Run("awaiting payment with open checkout expires Stripe session and frees PIN", func(t *testing.T) {
		var expireCalls atomic.Int32
		db, store, biller, link := setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/expire") {
				expireCalls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":             "cs_open_cancel",
					"object":         "checkout.session",
					"status":         "expired",
					"payment_status": "unpaid",
				})
				return
			}
			http.NotFound(w, r)
		})
		eID, _ := submitWithPin(t, store, link.Token, "Open Checkout", "445566")
		sessID := "cs_open_cancel"
		c := billing.Contract{EnrollmentID: eID, Reference: rand.Text(), PlanID: "full", PlanName: "Pago Único", InstallmentCount: 1, TotalCents: 60000}
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
		a := billing.CheckoutAttempt{ContractID: c.ID, Reference: rand.Text(), PlanID: "full", FirstAmountCents: 60000, StripeSessionID: &sessID, State: "open", CreationStartedAt: now}
		if err := db.Create(&a).Error; err != nil {
			t.Fatal(err)
		}

		entry, changed, err := biller.CancelEnrollment(context.Background(), eID, "admin")
		if err != nil || !changed || entry == nil || entry.Status != links.Canceled || entry.HasPin || entry.CanceledBy != "admin" {
			t.Fatalf("CancelEnrollment = %+v, changed=%v, err=%v", entry, changed, err)
		}
		if expireCalls.Load() != 1 {
			t.Fatalf("expected 1 Stripe expire call, got %d", expireCalls.Load())
		}
		var updatedAttempt billing.CheckoutAttempt
		if err := db.Take(&updatedAttempt, a.ID).Error; err != nil || updatedAttempt.State != "expired" {
			t.Fatalf("attempt state = %q, want expired", updatedAttempt.State)
		}
		// Released PIN is reusable on a new enrollment.
		_, _ = submitWithPin(t, store, link.Token, "Reused Pin", "445566")
	})

	t.Run("open checkout already paid on Stripe blocks cancel", func(t *testing.T) {
		db, store, biller, link := setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/expire"):
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "session already complete"}})
			case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":             "cs_paid_race",
					"object":         "checkout.session",
					"status":         "complete",
					"payment_status": "paid",
				})
			}
		})
		eID, _ := submitWithPin(t, store, link.Token, "Paid Race", "778899")
		sessID := "cs_paid_race"
		c := billing.Contract{EnrollmentID: eID, Reference: rand.Text(), PlanID: "full", PlanName: "Pago Único", InstallmentCount: 1, TotalCents: 60000}
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
		a := billing.CheckoutAttempt{ContractID: c.ID, Reference: rand.Text(), PlanID: "full", FirstAmountCents: 60000, StripeSessionID: &sessID, State: "open", CreationStartedAt: now}
		if err := db.Create(&a).Error; err != nil {
			t.Fatal(err)
		}

		_, changed, err := biller.CancelEnrollment(context.Background(), eID, "admin")
		if !errors.Is(err, billing.ErrCancelBlocked) || changed {
			t.Fatalf("expected ErrCancelBlocked, got changed=%v, err=%v", changed, err)
		}
		var e links.Enrollment
		if err := db.Take(&e, eID).Error; err != nil || e.CanceledAt != nil || e.Pin != "778899" {
			t.Fatalf("blocked cancel altered enrollment: %+v, err=%v", e, err)
		}
		var updatedAttempt billing.CheckoutAttempt
		if err := db.Take(&updatedAttempt, a.ID).Error; err != nil || updatedAttempt.State != "processing" {
			t.Fatalf("attempt state = %q, want processing", updatedAttempt.State)
		}
	})

	t.Run("in-flight states block cancel and expired PIN clearing", func(t *testing.T) {
		for _, state := range []string{"creating", "processing", "expiring", "uncertain"} {
			db, store, biller, link := setup(t, http.NotFound)
			eID, _ := submitWithPin(t, store, link.Token, "In Flight", "112233")
			c := billing.Contract{EnrollmentID: eID, Reference: rand.Text(), PlanID: "full", PlanName: "Pago Único", InstallmentCount: 1, TotalCents: 60000}
			if err := db.Create(&c).Error; err != nil {
				t.Fatal(err)
			}
			a := billing.CheckoutAttempt{ContractID: c.ID, Reference: rand.Text(), PlanID: "full", FirstAmountCents: 60000, State: state, CreationStartedAt: now}
			if err := db.Create(&a).Error; err != nil {
				t.Fatal(err)
			}
			if _, changed, err := biller.CancelEnrollment(context.Background(), eID, "admin"); !errors.Is(err, billing.ErrCancelBlocked) || changed {
				t.Fatalf("state %s: changed=%v, err=%v; want ErrCancelBlocked", state, changed, err)
			}
		}
	})

	t.Run("expired submission with PIN clears PIN while staying Expired", func(t *testing.T) {
		db, store, biller, link := setup(t, http.NotFound)
		eID, _ := submitWithPin(t, store, link.Token, "Expired Pin", "334455")
		if err := db.Model(&links.Enrollment{}).Where("id = ?", eID).Update("expires_at", now.Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		entry, changed, err := biller.CancelEnrollment(context.Background(), eID, "admin")
		if err != nil || !changed || entry == nil || entry.Status != links.Expired || entry.HasPin {
			t.Fatalf("expired clear PIN: entry=%+v, changed=%v, err=%v", entry, changed, err)
		}
		// Second call is a no-op.
		entry2, changed2, err := biller.CancelEnrollment(context.Background(), eID, "admin")
		if err != nil || changed2 || entry2 == nil || entry2.Status != links.Expired {
			t.Fatalf("repeat clear PIN: entry=%+v, changed=%v, err=%v", entry2, changed2, err)
		}
		// PIN can now be reused.
		_, _ = submitWithPin(t, store, link.Token, "Next Student", "334455")
	})

	t.Run("funded enrollment stays Finished", func(t *testing.T) {
		db, store, biller, link := setup(t, http.NotFound)
		eID, _ := submitWithPin(t, store, link.Token, "Paid Student", "998877")
		if err := db.Model(&links.Enrollment{}).Where("id = ?", eID).Updates(map[string]any{"terms_accepted_at": now, "program_start_date": "2026-11-01"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			return links.Enroll(tx, eID, now)
		}); err != nil {
			t.Fatal(err)
		}
		entry, changed, err := biller.CancelEnrollment(context.Background(), eID, "admin")
		if err != nil || changed || entry == nil || entry.Status != links.Finished {
			t.Fatalf("funded cancel: entry=%+v, changed=%v, err=%v", entry, changed, err)
		}
	})
}

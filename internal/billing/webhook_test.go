//go:build integration

package billing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/testdb"
)

func signWebhookPayload(t *testing.T, payload []byte, secret string, timestamp time.Time) string {
	t.Helper()
	ts := timestamp.Unix()
	signedPayload := fmt.Sprintf("%d.%s", ts, string(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedPayload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("t=%d,v1=%s", ts, sig)
}

func makeStripeEventJSON(id, eventType, resourceID string, livemode bool, apiVersion string) []byte {
	evt := map[string]any{
		"id":          id,
		"object":      "event",
		"type":        eventType,
		"livemode":    livemode,
		"api_version": apiVersion,
		"created":     time.Now().Unix(),
		"data": map[string]any{
			"object": map[string]any{
				"id":     resourceID,
				"object": "checkout.session",
			},
		},
	}
	b, _ := json.Marshal(evt)
	return b
}

func TestWebhookAuthenticityAndReplay(t *testing.T) {
	secret := billingtest.Config().WebhookSecret

	db := testdb.Open(t, billing.Models()...)
	svc := billingtest.New(t, db, nil)

	const apiVersion = "2026-09-30.endive"

	// 1. Valid signed event inserts into billing_webhook_events and returns 200
	t.Run("valid_event_stored", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_1", "checkout.session.completed", "cs_test_1", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var event billing.WebhookEvent
		if err := db.Where("id = 'evt_test_1'").First(&event).Error; err != nil {
			t.Fatalf("expected event stored in DB: %v", err)
		}
		if event.EventType != "checkout.session.completed" || event.ResourceID != "cs_test_1" {
			t.Fatalf("unexpected stored event fields: %+v", event)
		}
	})

	// 2. Duplicate event delivery returns 200 without error
	t.Run("duplicate_event_returns_200", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_1", "checkout.session.completed", "cs_test_1", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200 on duplicate, got %d", rec.Code)
		}
	})

	// 3. Bad signature rejected with 400
	t.Run("bad_signature_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_bad_sig", "checkout.session.completed", "cs_test_bad", false, apiVersion)
		sig := signWebhookPayload(t, payload, "wrong_secret", time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for bad signature, got %d", rec.Code)
		}
	})

	// 4. Expired signature (> 5 minutes old) rejected with 400
	t.Run("expired_signature_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_expired", "checkout.session.completed", "cs_test_exp", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now().Add(-10*time.Minute))

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for expired signature, got %d", rec.Code)
		}
	})

	// 5. Wrong mode (livemode=true in sandbox) rejected with 400
	t.Run("wrong_mode_rejected", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_test_livemode", "checkout.session.completed", "cs_test_live", true, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 for livemode mismatch, got %d", rec.Code)
		}
	})

	// 6. Oversized payload (> 1 MiB) rejected with 413
	t.Run("oversized_payload_rejected", func(t *testing.T) {
		hugePayload := make([]byte, 1024*1024+10)
		sig := signWebhookPayload(t, hugePayload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(hugePayload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected status 413 for oversized body, got %d", rec.Code)
		}
	})

	// 7. Non-allowlisted event returns 200 without storing in DB
	t.Run("non_allowlisted_event_ignored", func(t *testing.T) {
		payload := makeStripeEventJSON("evt_unrelated_1", "payout.paid", "po_123", false, apiVersion)
		sig := signWebhookPayload(t, payload, secret, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()

		svc.Webhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}

		var count int64
		db.Model(&billing.WebhookEvent{}).Where("id = 'evt_unrelated_1'").Count(&count)
		if count != 0 {
			t.Fatalf("expected unrelated event not to be stored, got count %d", count)
		}
	})
}

// A stored webhook must wake the worker well before the 5s tick. The sentinel shows Run is up
// and idle, so the second event finishing within 3s can only come from the wake.
func TestWebhookWakesWorker(t *testing.T) {
	secret := billingtest.Config().WebhookSecret
	db := testdb.Open(t, billing.Models()...)
	svc := billingtest.New(t, db, nil)

	post := func(id string) {
		t.Helper()
		// An unknown invoice maps to no obligation, so the worker finishes it without Stripe.
		payload := makeStripeEventJSON(id, "invoice.paid", "in_unknown", false, stripe.APIVersion)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", signWebhookPayload(t, payload, secret, time.Now()))
		rec := httptest.NewRecorder()
		svc.Webhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook %s: status %d", id, rec.Code)
		}
	}
	processed := func(id string, within time.Duration) bool {
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			var event billing.WebhookEvent
			if db.Where("id = ? AND processed_at IS NOT NULL", id).Limit(1).Find(&event).RowsAffected > 0 {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	defer func() { cancel(); <-done }()

	post("evt_wake_sentinel")
	if !processed("evt_wake_sentinel", 3*time.Second) {
		t.Fatal("sentinel event was not processed")
	}
	post("evt_wake_second")
	if !processed("evt_wake_second", 3*time.Second) {
		t.Fatal("webhook did not wake the worker before the poll tick")
	}
}

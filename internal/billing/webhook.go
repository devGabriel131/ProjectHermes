package billing

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/stripe/stripe-go/v87/webhook"
	"gorm.io/gorm/clause"

	"projecthermes/internal/logging"
)

// maxWebhookBody bounds the raw payload read before signature verification.
const maxWebhookBody = 1 << 20

// webhookEvents is the exact set of Stripe event types the worker acts on.
var webhookEvents = map[string]struct{}{
	"checkout.session.completed":               {},
	"checkout.session.async_payment_succeeded": {},
	"checkout.session.async_payment_failed":    {},
	"checkout.session.expired":                 {},
	"invoice.created":                          {},
	"invoice.finalized":                        {},
	"invoice.finalization_failed":              {},
	"invoice.paid":                             {},
	"invoice.payment_failed":                   {},
	"invoice.payment_action_required":          {},
	"invoice.updated":                          {},
	"invoice.voided":                           {},
	"invoice.marked_uncollectible":             {},
	"invoice.deleted":                          {},
	"charge.updated":                           {},
	"charge.refunded":                          {},
	"refund.updated":                           {},
	"charge.dispute.created":                   {},
	"charge.dispute.updated":                   {},
	"charge.dispute.closed":                    {},
	"radar.early_fraud_warning.created":        {},
	"customer.updated":                         {},
}

// Webhook verifies a signed Stripe event and durably records allowlisted types for the
// worker. It returns 200 only after insertion or a confirmed duplicate, so Stripe retries
// anything that might have been lost. Body, signature and secret are never logged.
func (s *Service) Webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	event, err := webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), s.cfg.WebhookSecret)
	if err != nil {
		s.rejected.Do(func() {
			s.log.LogAttrs(r.Context(), slog.LevelWarn, "stripe_webhook_rejected", logging.ErrorAttrs(err)...)
		})
		http.Error(w, "invalid webhook", http.StatusBadRequest)
		return
	}
	if event.Livemode != s.cfg.Live {
		http.Error(w, "livemode mismatch", http.StatusBadRequest)
		return
	}
	if _, ok := webhookEvents[string(event.Type)]; !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	var resourceID string
	if event.Data != nil {
		resourceID, _ = event.Data.Object["id"].(string)
	}
	if event.ID == "" || resourceID == "" {
		http.Error(w, "invalid webhook", http.StatusBadRequest)
		return
	}

	record := WebhookEvent{
		ID:         event.ID,
		EventType:  string(event.Type),
		ResourceID: resourceID,
	}
	// A redelivery conflicts on the event ID and inserts nothing; that is confirmed durability.
	err = s.db.WithContext(r.Context()).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		Create(&record).Error
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "stripe_webhook_store_failed",
			append(logging.ErrorAttrs(err), slog.String("event", event.ID))...)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.wakeWorker()
	w.WriteHeader(http.StatusOK)
}

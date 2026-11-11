// Package billingtest builds billing services backed by a fake Stripe for tests in other packages.
package billingtest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"projecthermes/internal/billing"
)

// Config is a valid sandbox configuration.
func Config() billing.Config {
	return billing.Config{
		BaseURL:                      "http://127.0.0.1:3000",
		WebhookSecret:                "whsec_test",
		PaymentMethodConfigurationID: "pmc_test",
		PortalConfigurationID:        "bpc_test",
	}
}

// Catalog returns a fresh map holding a Price ID for every plan installment and the reinstatement fee.
func Catalog() billing.Catalog {
	return billing.Catalog{
		"asvab_full_initial_usd":    "pr_full",
		"asvab_three_initial_usd":   "pr_three_1",
		"asvab_three_remaining_usd": "pr_three_2",
		"asvab_six_initial_usd":     "pr_six_1",
		"asvab_six_remaining_usd":   "pr_six_2",
		"asvab_reinstatement_usd":   "pr_reinstatement",
	}
}

// Client talks to handler (404 for everything when nil) through a server closed with the test.
// It never retries and never logs SDK traffic.
func Client(t testing.TB, handler http.Handler) *stripe.Client {
	t.Helper()
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	maxRetries := int64(0)
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		URL:               stripe.String(server.URL),
		HTTPClient:        server.Client(),
		MaxNetworkRetries: &maxRetries,
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return stripe.NewClient("sk_test_mock", stripe.WithBackends(backends))
}

// New builds a billing service over db with Config, Catalog and a Client for handler.
func New(t testing.TB, db *gorm.DB, handler http.Handler) *billing.Service {
	t.Helper()
	svc, err := billing.New(db, Client(t, handler), Config(), Catalog(), nil, nil)
	if err != nil {
		t.Fatalf("billing.New: %v", err)
	}
	return svc
}

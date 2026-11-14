package billing_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     billing.Config
		wantErr bool
	}{
		{
			name: "valid_live",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: false,
		},
		{
			name: "valid_sandbox_localhost",
			cfg: billing.Config{
				BaseURL:                      "http://127.0.0.1:3000",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         false,
			},
			wantErr: false,
		},
		{
			name: "live_http_rejected",
			cfg: billing.Config{
				BaseURL:                      "http://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "sandbox_remote_http_rejected",
			cfg: billing.Config{
				BaseURL:                      "http://remote.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         false,
			},
			wantErr: true,
		},
		{
			name: "path_prefix_rejected",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com/prefix",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "query_string_rejected",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com?foo=bar",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_webhook_secret",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_payment_method_configuration_id",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "",
				PortalConfigurationID:        "bpc_123",
				Live:                         true,
			},
			wantErr: true,
		},
		{
			name: "missing_portal_configuration_id",
			cfg: billing.Config{
				BaseURL:                      "https://payments.example.com",
				WebhookSecret:                "whsec_test123",
				PaymentMethodConfigurationID: "pmc_123",
				PortalConfigurationID:        "",
				Live:                         true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("cfg.Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	tests := []struct {
		name, apiKey, mode string
		wantErr            bool
	}{
		{"missing_api_key", "", "sandbox", true},
		{"invalid_mode", "sk_test_123", "staging", true},
		{"mismatched_key_prefix_sandbox", "sk_live_123", "sandbox", true},
		{"mismatched_key_prefix_live", "sk_test_123", "live", true},
		{"valid_sandbox_config", "rk_test_abc123", "sandbox", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("STRIPE_API_KEY", tt.apiKey)
			t.Setenv("STRIPE_MODE", tt.mode)
			t.Setenv("PUBLIC_BASE_URL", "http://localhost:3000")
			t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_mock")
			t.Setenv("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID", "pmc_mock")
			t.Setenv("STRIPE_PORTAL_CONFIGURATION_ID", "bpc_mock")
			cfg, key, err := billing.LoadConfigFromEnv()
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadConfigFromEnv() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && (key != tt.apiKey || cfg.Live) {
				t.Fatalf("got key %q, Live=%v; want %q, Live=false", key, cfg.Live, tt.apiKey)
			}
		})
	}
}

func mockStripePrice(id, lookupKey string, cents int64, currency string, active, livemode bool, billingScheme stripe.PriceBillingScheme, priceType stripe.PriceType) map[string]any {
	return map[string]any{
		"id":             id,
		"object":         "price",
		"active":         active,
		"billing_scheme": string(billingScheme),
		"currency":       currency,
		"livemode":       livemode,
		"lookup_key":     lookupKey,
		"type":           string(priceType),
		"unit_amount":    cents,
	}
}

// loadCatalog runs sandbox LoadCatalog against a Stripe whose price list is prices.
func loadCatalog(t *testing.T, prices []map[string]any) (billing.Catalog, error) {
	t.Helper()
	client := billingtest.Client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": prices, "has_more": false})
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	return billing.LoadCatalog(ctx, client, false)
}

func TestLoadCatalogValidation(t *testing.T) {
	validPrices := []map[string]any{
		mockStripePrice("pr_full", "asvab_full_initial_usd", 60000, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_three_1", "asvab_three_initial_usd", 20834, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_three_2", "asvab_three_remaining_usd", 20833, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_six_1", "asvab_six_initial_usd", 10835, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_six_2", "asvab_six_remaining_usd", 10833, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
		mockStripePrice("pr_reinstatement", "asvab_reinstatement_usd", 2500, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime),
	}

	t.Run("valid_catalog", func(t *testing.T) {
		catalog, err := loadCatalog(t, validPrices)
		if err != nil {
			t.Fatalf("unexpected error loading catalog: %v", err)
		}
		want := billing.Catalog{
			"asvab_full_initial_usd":    "pr_full",
			"asvab_three_initial_usd":   "pr_three_1",
			"asvab_three_remaining_usd": "pr_three_2",
			"asvab_six_initial_usd":     "pr_six_1",
			"asvab_six_remaining_usd":   "pr_six_2",
			"asvab_reinstatement_usd":   "pr_reinstatement",
		}
		if !maps.Equal(catalog, want) {
			t.Fatalf("catalog price IDs mismatched: %+v", catalog)
		}
	})

	t.Run("missing_price", func(t *testing.T) {
		if _, err := loadCatalog(t, validPrices[:5]); err == nil { // omit reinstatement
			t.Fatal("expected error when price is missing from catalog")
		}
	})

	t.Run("wrong_unit_amount", func(t *testing.T) {
		prices := slices.Clone(validPrices)
		prices[0] = mockStripePrice("pr_full", "asvab_full_initial_usd", 59999, "usd", true, false, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime)
		if _, err := loadCatalog(t, prices); err == nil {
			t.Fatal("expected error when price amount does not match plan registry")
		}
	})

	t.Run("wrong_livemode", func(t *testing.T) {
		prices := slices.Clone(validPrices)
		prices[0] = mockStripePrice("pr_full", "asvab_full_initial_usd", 60000, "usd", true, true, stripe.PriceBillingSchemePerUnit, stripe.PriceTypeOneTime)
		if _, err := loadCatalog(t, prices); err == nil {
			t.Fatal("expected error when price livemode does not match configuration")
		}
	})
}

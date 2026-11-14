package billing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"

	"projecthermes/internal/payments"
)

const (
	reinstatementFeeCents = 2500
	reinstatementKey      = "asvab_reinstatement_usd"
)

// stripeLocale is the language of every Stripe-rendered page, email and PDF. Stripe has no
// Puerto Rico locale: "es" prints Spain-style amounts ("600,00 US$"), "es-419" prints "USD 600.00".
// Only "en" prints a bare "$600.00", in English.
const stripeLocale = "es-419"

// Config carries server-mode Stripe configuration.
type Config struct {
	BaseURL                      string
	WebhookSecret                string
	PaymentMethodConfigurationID string
	PortalConfigurationID        string
	Live                         bool
}

// Validate validates that Config adheres to trusted base URL and provider requirements.
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("billing: PUBLIC_BASE_URL is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Host == "" {
		return errors.New("billing: PUBLIC_BASE_URL must be an absolute URL with host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("billing: PUBLIC_BASE_URL must not contain userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("billing: PUBLIC_BASE_URL must not contain a path prefix")
	}
	isLocalhost := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if c.Live {
		if parsed.Scheme != "https" {
			return errors.New("billing: PUBLIC_BASE_URL must use https in live mode")
		}
	} else {
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalhost) {
			return errors.New("billing: PUBLIC_BASE_URL must use https except localhost in sandbox mode")
		}
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		return errors.New("billing: STRIPE_WEBHOOK_SECRET is required")
	}
	if strings.TrimSpace(c.PaymentMethodConfigurationID) == "" {
		return errors.New("billing: STRIPE_PAYMENT_METHOD_CONFIGURATION_ID is required")
	}
	if strings.TrimSpace(c.PortalConfigurationID) == "" {
		return errors.New("billing: STRIPE_PORTAL_CONFIGURATION_ID is required")
	}
	return nil
}

// LoadConfigFromEnv reads and validates Stripe configuration from environment variables.
func LoadConfigFromEnv() (Config, string, error) {
	apiKey := os.Getenv("STRIPE_API_KEY")
	if apiKey == "" {
		return Config{}, "", errors.New("billing: STRIPE_API_KEY is required")
	}
	mode := os.Getenv("STRIPE_MODE")
	if mode != "sandbox" && mode != "live" {
		return Config{}, "", errors.New("billing: STRIPE_MODE must be exactly 'sandbox' or 'live'")
	}
	live := mode == "live"
	if live {
		if !strings.HasPrefix(apiKey, "rk_live_") && !strings.HasPrefix(apiKey, "sk_live_") {
			return Config{}, "", errors.New("billing: STRIPE_API_KEY prefix must be rk_live_ or sk_live_ in live mode")
		}
	} else {
		if !strings.HasPrefix(apiKey, "rk_test_") && !strings.HasPrefix(apiKey, "sk_test_") {
			return Config{}, "", errors.New("billing: STRIPE_API_KEY prefix must be rk_test_ or sk_test_ in sandbox mode")
		}
	}

	cfg := Config{
		BaseURL:                      os.Getenv("PUBLIC_BASE_URL"),
		WebhookSecret:                os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PaymentMethodConfigurationID: os.Getenv("STRIPE_PAYMENT_METHOD_CONFIGURATION_ID"),
		PortalConfigurationID:        os.Getenv("STRIPE_PORTAL_CONFIGURATION_ID"),
		Live:                         live,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, "", err
	}
	return cfg, apiKey, nil
}

// Catalog maps price lookup keys to resolved Stripe Price IDs.
type Catalog map[string]string

// priceKey names the lookup key of a plan's i-th installment. Every later installment shares one price.
func priceKey(planID string, i int) string {
	if i == 0 {
		return "asvab_" + planID + "_initial_usd"
	}
	return "asvab_" + planID + "_remaining_usd"
}

// NewStripeClient creates a client configured with 5-second timeouts, at most 2 retries,
// and silent logging so raw SDK payloads never leak into application logs.
func NewStripeClient(apiKey string) *stripe.Client {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}
	maxRetries := int64(2)
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		HTTPClient:        httpClient,
		MaxNetworkRetries: &maxRetries,
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return stripe.NewClient(apiKey, stripe.WithBackends(backends))
}

// LoadCatalog queries Stripe for every plan installment price and the reinstatement fee in a
// single batched lookup and validates currency, unit amount, billing scheme, and livemode.
func LoadCatalog(ctx context.Context, client *stripe.Client, live bool) (Catalog, error) {
	expected := map[string]int64{reinstatementKey: reinstatementFeeCents}
	for _, p := range payments.Plans() {
		for i, amount := range p.Amounts {
			key := priceKey(p.ID, i)
			if prev, ok := expected[key]; ok && prev != amount {
				return nil, fmt.Errorf("billing: plan %q installments share price key %q but differ in amount", p.ID, key)
			}
			expected[key] = amount
		}
	}

	// ponytail: Stripe caps lookup_keys at 10
	params := &stripe.PriceListParams{
		LookupKeys: stripe.StringSlice(slices.Sorted(maps.Keys(expected))),
	}

	iter := client.V1Prices.List(ctx, params)
	resolved := make(map[string]*stripe.Price)
	for price, err := range iter.All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("billing: list prices from stripe: %w", err)
		}
		if price == nil || price.LookupKey == "" {
			continue
		}
		if _, exists := resolved[price.LookupKey]; exists {
			return nil, fmt.Errorf("billing: conflicting duplicate price found for lookup key %q", price.LookupKey)
		}
		resolved[price.LookupKey] = price
	}

	catalog := make(Catalog, len(expected))
	for lookupKey, expectedCents := range expected {
		price, found := resolved[lookupKey]
		if !found {
			return nil, fmt.Errorf("billing: missing stripe price for lookup key %q", lookupKey)
		}
		if !price.Active {
			return nil, fmt.Errorf("billing: stripe price %q (%s) is not active", lookupKey, price.ID)
		}
		if price.Livemode != live {
			return nil, fmt.Errorf("billing: stripe price %q livemode (%t) does not match configured live mode (%t)", lookupKey, price.Livemode, live)
		}
		if price.Currency != stripe.CurrencyUSD {
			return nil, fmt.Errorf("billing: stripe price %q has currency %q, expected usd", lookupKey, price.Currency)
		}
		if price.BillingScheme != stripe.PriceBillingSchemePerUnit {
			return nil, fmt.Errorf("billing: stripe price %q has billing scheme %q, expected per_unit", lookupKey, price.BillingScheme)
		}
		if price.Type != stripe.PriceTypeOneTime {
			return nil, fmt.Errorf("billing: stripe price %q has type %q, expected one_time", lookupKey, price.Type)
		}
		if price.UnitAmount != expectedCents {
			return nil, fmt.Errorf("billing: stripe price %q has unit amount %d cents, expected %d cents", lookupKey, price.UnitAmount, expectedCents)
		}
		catalog[lookupKey] = price.ID
	}
	return catalog, nil
}

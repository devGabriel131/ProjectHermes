// Package billing owns tuition money: the Stripe configuration and price catalog, hosted Checkout for the first payment,
// scheduled one-off invoices for later installments, webhook intake, and the leased background worker that mirrors
// Stripe state into contracts, obligations, attempts and payments. Stripe's current objects are authoritative; stored
// webhook events carry no payload and only wake the worker. Funding is the single moment a Student is created (links.Enroll).
// See docs/billing/overview.md.
package billing

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// providerBudget bounds one call's synchronous provider work and stays under the server's
	// 10 s write timeout; slower outcomes are recorded and resumed rather than holding the browser.
	providerBudget = 8 * time.Second
	// persistBudget bounds the detached write that records a provider outcome (persistCtx).
	persistBudget = 5 * time.Second
)

// Bounded review reasons.
const (
	reviewUnresolvedProviderCreate = "unresolved_provider_create"
	reviewInvoiceMismatch          = "invoice_mismatch"
	reviewMissingInvoice           = "missing_invoice"
	reviewPaymentReview            = "payment_review"
	reviewUnsupportedPaymentMethod = "unsupported_payment_method"
	reviewConfigurationMismatch    = "configuration_mismatch"
)

// Error values.
var (
	ErrUnavailable     = errors.New("billing: resource unavailable")
	ErrPlanLocked      = errors.New("billing: plan is locked")
	ErrPaymentPending  = errors.New("billing: payment is pending")
	ErrNotSuspended    = errors.New("billing: student is not suspended")
	ErrNeedsReview     = errors.New("billing: financial state needs review")
	ErrInvoiceNotReady = errors.New("billing: invoice is not ready")
	ErrContactLocked   = errors.New("billing: contact is locked")
	ErrCancelBlocked   = errors.New("billing: payment in progress")
	ErrUnknownPlan     = errors.New("billing: unknown payment plan")

	errStaffRequired = errors.New("billing: staff identity is required")
)

var forUpdate = clause.Locking{Strength: clause.LockingStrengthUpdate}

// persistCtx outlives the request so work that follows a committed outcome (recording a provider result, syncing a provider update) still runs.
func persistCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistBudget)
}

// firstRow loads the first row matching query; found is false when none match.
func firstRow(db *gorm.DB, dest any, query string, args ...any) (found bool, err error) {
	result := db.Where(query, args...).Limit(1).Find(dest)
	return result.RowsAffected > 0, result.Error
}

// require turns a missing row into ErrUnavailable.
func require(found bool, err error) error {
	if err == nil && !found {
		return ErrUnavailable
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Service manages tuition billing, Stripe operations, webhooks, and background worker reconciliation.
type Service struct {
	db      *gorm.DB
	client  *stripe.Client
	cfg     Config
	catalog Catalog
	changed func()
	log     *slog.Logger
	// wake nudges Run to drain now; buffered 1 so concurrent webhooks coalesce.
	wake chan struct{}
	// rejected keeps unauthenticated bad-signature POSTs from flooding the log; the first still logs at once.
	rejected rate.Sometimes
}

// New creates a verified billing service.
func New(db *gorm.DB, client *stripe.Client, cfg Config, catalog Catalog, changed func(), log *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if changed == nil {
		changed = func() {}
	}
	return &Service{
		db:       db,
		client:   client,
		cfg:      cfg,
		catalog:  catalog,
		changed:  changed,
		log:      log,
		wake:     make(chan struct{}, 1),
		rejected: rate.Sometimes{Interval: time.Minute},
	}, nil
}

func (s *Service) wakeWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// contractFor finds the contract behind a student; an unknown student is unavailable.
func (s *Service) contractFor(ctx context.Context, studentID uint64) (Contract, error) {
	var c Contract
	err := require(firstRow(s.db.WithContext(ctx), &c, "enrollment_id = (SELECT enrollment_id FROM students WHERE id = ?)", studentID))
	return c, err
}

// withStaffContract runs fn under the contract's workflow lease. A contract a worker holds is
// mid-provider-work, so staff get ErrPaymentPending to retry instead of racing it.
func (s *Service) withStaffContract(ctx context.Context, id uint64, fn func(lease) error) error {
	err := s.withContract(ctx, id, fn)
	if errors.Is(err, errContractBusy) {
		return ErrPaymentPending
	}
	return err
}

// InvoiceURL returns the current hosted URL of one of the student's invoices. Links expire,
// so it is fetched on demand and never stored.
func (s *Service) InvoiceURL(ctx context.Context, studentID, obligationID uint64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return "", err
	}
	var o Obligation
	if err := require(firstRow(s.db.WithContext(ctx), &o, "id = ? AND contract_id = ?", obligationID, c.ID)); err != nil {
		return "", err
	}
	if o.StripeInvoiceID == nil {
		return "", ErrInvoiceNotReady
	}
	inv, err := s.client.V1Invoices.Retrieve(ctx, *o.StripeInvoiceID, nil)
	if isNotFound(err) {
		return "", ErrInvoiceNotReady
	}
	if err != nil {
		return "", err
	}
	if inv.Deleted || inv.HostedInvoiceURL == "" {
		return "", ErrInvoiceNotReady
	}
	return inv.HostedInvoiceURL, nil
}

// PortalURL creates a short-lived Stripe customer-portal link for the contract's own Customer.
// It never marks anything paid or reactivates.
func (s *Service) PortalURL(ctx context.Context, studentID uint64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return "", err
	}
	if c.StripeCustomerID == nil {
		return "", ErrUnavailable
	}
	session, err := s.client.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:      stripe.String(*c.StripeCustomerID),
		Configuration: stripe.String(s.cfg.PortalConfigurationID),
		Locale:        stripe.String(stripeLocale),
		ReturnURL:     stripe.String(strings.TrimSuffix(s.cfg.BaseURL, "/") + "/management/students/" + strconv.FormatUint(studentID, 10)),
	})
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// ReconcileStudent runs the worker's full contract sync for staff: it can fund the contract, create or
// arm invoices (a past-due invoice is finalized at once, so Stripe then collects), replay a Checkout
// create, and evaluate deadlines and reactivation. It never charges a card itself.
func (s *Service) ReconcileStudent(ctx context.Context, studentID uint64) (*Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, providerBudget)
	defer cancel()
	c, err := s.contractFor(ctx, studentID)
	if err != nil {
		return nil, err
	}
	err = s.withStaffContract(ctx, c.ID, func(l lease) error {
		if err := s.reconcileContract(ctx, l); err != nil {
			return err
		}
		return s.markSynced(ctx, l)
	})
	if err != nil {
		return nil, err
	}
	return s.ForStudent(ctx, studentID)
}

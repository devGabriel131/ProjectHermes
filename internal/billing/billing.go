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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// providerBudget bounds one call's synchronous provider work and stays under the server's
// 10 s write timeout; slower outcomes are recorded and resumed rather than holding the browser.
const providerBudget = 8 * time.Second

// Bounded review reasons.
const (
	reviewUnresolvedProviderCreate = "unresolved_provider_create"
	reviewInvoiceMismatch          = "invoice_mismatch"
	reviewMissingInvoice           = "missing_invoice"
	reviewPaymentReview            = "payment_review"
	reviewUnsupportedPaymentMethod = "unsupported_payment_method"
	reviewConfigurationMismatch    = "configuration_mismatch"
)

// Exported error values.
var (
	ErrUnavailable     = errors.New("resource unavailable")
	ErrPlanLocked      = errors.New("plan is locked")
	ErrPaymentPending  = errors.New("payment is pending")
	ErrNotSuspended    = errors.New("student is not suspended")
	ErrNeedsReview     = errors.New("financial state needs review")
	ErrInvoiceNotReady = errors.New("invoice is not ready")
	ErrContactLocked   = errors.New("billing: contact is locked")
)

var forUpdate = clause.Locking{Strength: clause.LockingStrengthUpdate}

// persistCtx outlives the request so work that follows a committed outcome (recording a provider result, syncing a provider update) still runs.
func persistCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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

// CheckoutResult returns the outcome of initiating or resuming a checkout attempt.
type CheckoutResult struct {
	URL     string
	Pending bool
	Funded  bool
}

// SummaryInstallment represents the status of one tuition installment in Summary.
type SummaryInstallment struct {
	ID           uint64
	Number       int
	AmountCents  int64
	DueAt        *time.Time
	Status       string
	PaidAt       *time.Time
	InvoiceReady bool
}

// Summary exposes financial standing, active/suspended status, and review reasons.
type Summary struct {
	PlanID                         string
	PlanName                       string
	TotalCents                     int64
	Funded                         bool
	FundedAt                       *time.Time
	CashPaidCents                  int64
	TuitionPaidCents               int64
	OtherSettlementCents           int64
	RemainingTuitionCents          int64
	OverdueTuitionCents            int64
	ReinstatementFeeRemainingCents int64
	ReinstatementFeeID             uint64
	ReinstatementInvoiceReady      bool
	Installments                   []SummaryInstallment
	IsActive                       bool
	IsSuspended                    bool
	LastSyncedAt                   *time.Time
	ReviewReasons                  []string
}

// ReturnStatus holds customer-facing status returned from Stripe-hosted checkout.
// Contains only payment status and amount; never leaks student identity, contact, or history.
type ReturnStatus struct {
	Status      string
	AmountCents int64
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
		db:      db,
		client:  client,
		cfg:     cfg,
		catalog: catalog,
		changed: changed,
		log:     log,
		wake:    make(chan struct{}, 1),
	}, nil
}

func (s *Service) wakeWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// contractFor finds the contract behind a student; unknown or out-of-range IDs are unavailable.
func (s *Service) contractFor(ctx context.Context, studentID uint64) (Contract, error) {
	var c Contract
	if studentID == 0 || studentID > 1<<63-1 {
		return c, ErrUnavailable
	}
	err := require(firstRow(s.db.WithContext(ctx), &c, "source_link_hash = (SELECT source_link_hash FROM students WHERE id = ?)", studentID))
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
	if obligationID == 0 || obligationID > 1<<63-1 {
		return "", ErrUnavailable
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

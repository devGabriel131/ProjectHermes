package billing

import (
	"time"

	"projecthermes/internal/links"
)

// TableName overrides for billing models.
func (Contract) TableName() string        { return "billing_contracts" }
func (Obligation) TableName() string      { return "billing_obligations" }
func (CheckoutAttempt) TableName() string { return "billing_checkout_attempts" }
func (Payment) TableName() string         { return "billing_payments" }
func (Suspension) TableName() string      { return "billing_suspensions" }
func (WebhookEvent) TableName() string    { return "billing_webhook_events" }

// Contract is the financial agreement for an enrollment; it belongs to the Link, and the Student exists only once it is funded.
type Contract struct {
	ID                  uint64      `gorm:"primaryKey"`
	SourceLinkHash      []byte      `gorm:"not null;uniqueIndex;check:contract_source_link_hash_size,octet_length(source_link_hash) = 32"`
	Link                *links.Link `gorm:"foreignKey:SourceLinkHash;references:TokenHash;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	Reference           string      `gorm:"not null;uniqueIndex"`
	PlanID              string      `gorm:"not null"`
	PlanName            string      `gorm:"not null"`
	InstallmentCount    int         `gorm:"not null;check:contract_installment_count,installment_count > 0"`
	TotalCents          int64       `gorm:"not null;check:contract_total_cents,total_cents > 0"`
	FundedAt            *time.Time
	StripeCustomerID    *string `gorm:"uniqueIndex"`
	SyncedCustomerName  string
	SyncedCustomerEmail string
	ReviewReason        *string
	LastSyncedAt        *time.Time
	NextSyncAt          *time.Time
	LeaseToken          *string
	LeaseExpiresAt      *time.Time
	CreatedAt           time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt           time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// Obligation represents a scheduled or completed tuition installment or reinstatement fee.
type Obligation struct {
	ID                       uint64    `gorm:"primaryKey"`
	ContractID               uint64    `gorm:"not null;index;uniqueIndex:idx_tuition_contract_number,where:kind = 'tuition'"`
	Contract                 *Contract `gorm:"foreignKey:ContractID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	Reference                string    `gorm:"not null;uniqueIndex"`
	Kind                     string    `gorm:"not null;check:obligation_kind,kind IN ('tuition', 'reinstatement')"`
	Number                   int       `gorm:"not null;uniqueIndex:idx_tuition_contract_number,where:kind = 'tuition'"`
	AmountCents              int64     `gorm:"not null;check:obligation_amount_cents,amount_cents > 0"`
	StripePriceID            string    `gorm:"not null"`
	DueAt                    *time.Time
	SuspensionID             *uint64 `gorm:"uniqueIndex"`
	DeadlineEpisodeID        *uint64 `gorm:"index"`
	CollectionDisposition    string  `gorm:"not null;default:'collect';check:obligation_disposition,collection_disposition IN ('collect', 'cancel')"`
	StripeInvoiceID          *string `gorm:"uniqueIndex"`
	StripeInvoiceItemID      *string `gorm:"uniqueIndex"`
	InvoiceCreationStartedAt *time.Time
	ItemCreationStartedAt    *time.Time
	ScheduledFinalizationAt  *time.Time
	LatestStripeStatus       *string
	AmountPaidCents          int64 `gorm:"not null;default:0;check:obligation_amount_paid,amount_paid_cents >= 0"`
	AmountRemainingCents     int64 `gorm:"not null;default:0;check:obligation_amount_remaining,amount_remaining_cents >= 0"`
	OtherSettlementCents     int64 `gorm:"not null;default:0;check:obligation_other_settlement,other_settlement_cents >= 0"`
	FirstSettledAt           *time.Time
	FailureOrActionRequired  bool `gorm:"not null;default:false"`
	ReviewReason             *string
	CreatedAt                time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt                time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// CheckoutAttempt tracks a single hosted Checkout session creation and payment attempt.
type CheckoutAttempt struct {
	ID                uint64    `gorm:"primaryKey"`
	ContractID        uint64    `gorm:"not null;index;uniqueIndex:idx_active_checkout_attempt,where:state = 'creating' OR state = 'open' OR state = 'processing' OR state = 'expiring' OR state = 'uncertain'"`
	Contract          *Contract `gorm:"foreignKey:ContractID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	Reference         string    `gorm:"not null;uniqueIndex"`
	PlanID            string    `gorm:"not null"`
	FirstAmountCents  int64     `gorm:"not null;check:attempt_first_amount_cents,first_amount_cents > 0"`
	StripePriceID     string    `gorm:"not null"`
	CreationStartedAt time.Time `gorm:"not null"`
	StripeSessionID   *string   `gorm:"uniqueIndex"`
	CheckoutURL       *string
	State             string    `gorm:"not null;check:attempt_state,state IN ('creating', 'open', 'processing', 'expiring', 'uncertain', 'expired', 'failed', 'paid')"`
	CreatedAt         time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt         time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// Payment records a confirmed Stripe PaymentIntent and balance transaction details.
type Payment struct {
	ID                        string    `gorm:"primaryKey"`
	ContractID                uint64    `gorm:"not null;index"`
	Contract                  *Contract `gorm:"foreignKey:ContractID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	StripeChargeID            *string   `gorm:"uniqueIndex"`
	GrossReceivedCents        int64     `gorm:"not null;check:payment_gross_received,gross_received_cents >= 0"`
	GrossRefundedCents        int64     `gorm:"not null;default:0;check:payment_gross_refunded,gross_refunded_cents >= 0"`
	PaymentTime               time.Time `gorm:"not null"`
	DisputeOrFraudReviewState *string
	BalanceTransactionID      *string
	FeeCents                  *int64
	NetCents                  *int64
	CreatedAt                 time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt                 time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// Suspension represents an active or historic academic suspension due to missed tuition deadline.
type Suspension struct {
	ID                                 uint64    `gorm:"primaryKey"`
	ContractID                         uint64    `gorm:"not null;index;uniqueIndex:idx_active_suspension,where:closed_at IS NULL"`
	Contract                           *Contract `gorm:"foreignKey:ContractID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	TriggeringTuitionInstallmentNumber int       `gorm:"not null"`
	StartedAt                          time.Time `gorm:"not null"`
	ReinstatementRequestedAt           *time.Time
	RequestedBy                        *string
	ClosedAt                           *time.Time
	CloseReason                        *string   `gorm:"check:suspension_close_reason,close_reason IS NULL OR close_reason IN ('reinstated', 'corrected')"`
	CreatedAt                          time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt                          time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// WebhookEvent persists received Stripe events for idempotent asynchronous processing.
type WebhookEvent struct {
	ID             string `gorm:"primaryKey"`
	EventType      string `gorm:"not null"`
	ResourceID     string `gorm:"not null"`
	ProcessedAt    *time.Time
	RetryAt        *time.Time `gorm:"index"`
	RetryCount     int        `gorm:"not null;default:0"`
	LeaseToken     *string
	LeaseExpiresAt *time.Time
	CreatedAt      time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt      time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// Models returns instances of all billing entities for GORM AutoMigrate.
func Models() []any {
	return []any{
		&Contract{},
		&Obligation{},
		&CheckoutAttempt{},
		&Payment{},
		&Suspension{},
		&WebhookEvent{},
	}
}

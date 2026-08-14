package store

import (
	"time"
)

type Checkout struct {
	CheckoutID                  string
	PlanID                      string
	NormalizedReturnURL         string
	ProcessorFamily             string
	RequestFingerprint          []byte
	ProviderIdempotencyKey      string
	ProcessorShopperReference   *string
	Status                      string
	SubscriptionRef             *string
	ProcessorCheckoutID         *string
	ExpiresAt                   time.Time
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type Subscription struct {
	SubscriptionRef             string
	CheckoutID                  string
	PlanID                      string
	Status                      string
	StateVersion                int64
	ProcessorFamily             string
	ProcessorCustomerID         *string
	ProcessorSubscriptionID     *string
	ProcessorInitialPaymentID   *string
	ProcessorShopperReference   *string
	PaymentMethodCiphertext     []byte
	PaymentMethodNonce          []byte
	PaymentMethodKeyVersion     *string
	CurrentPeriodStart          time.Time
	CurrentPeriodEnd            time.Time
	CancelAtPeriodEnd           bool
	StateChangedAt              time.Time
	PastDueSince                *time.Time
	CanceledAt                  *time.Time
	AutomaticChargingBlocked    bool
	ChargingBlockReason         *string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type OutboundEvent struct {
	EventID        string
	EventType      string
	SubscriptionRef string
	CheckoutID     string
	StateVersion   int64
	PayloadBody    []byte
	DeliveryState  string
	AttemptCount   int
	NextAttemptAt  *time.Time
	DeliveredAt    *time.Time
	DeadLetteredAt *time.Time
	AbandonedAt    *time.Time
	LastErrorClass *string
	ClaimToken     *string
	FencingToken   int64
	LeaseUntil     *time.Time
	CreatedAt      time.Time
}

type ProcessorEvent struct {
	ProcessorFamily     string
	ProcessorEventID    string
	ProcessingActionID  string
	ProviderEventType   string
	PayloadHash         []byte
	NormalizedFields    map[string]any
	SensitiveCiphertext []byte
	SensitiveNonce      []byte
	SensitiveKeyVersion *string
	ProcessingState     string
	SubscriptionRef     *string
	ReceivedAt          time.Time
	ProcessedAt         *time.Time
	ClaimToken          *string
	FencingToken        int64
	LeaseUntil          *time.Time
	LastErrorClass      *string
}

type ProcessingLease struct {
	ProcessingKey  string
	Status         string
	ActiveActionID *string
	ClaimToken     *string
	FencingToken   int64
	LeaseUntil     *time.Time
}

type ScheduledAction struct {
	ActionID       string
	ActionKey      string
	SubscriptionRef string
	ActionType     string
	TargetAt       time.Time
	DueAt          time.Time
	Status         string
	ClaimToken     *string
	FencingToken   int64
	LeaseUntil     *time.Time
	LastErrorClass *string
}

type ChargeAttempt struct {
	AttemptID                string
	ActionID                 string
	SubscriptionRef          string
	PeriodStart              time.Time
	PeriodEnd                time.Time
	AttemptNumber            int
	ProviderEndpoint         string
	ProviderAPIVersion       string
	MerchantAccount          string
	AmountMinor              int64
	Currency                 string
	AttemptReference         string
	ShopperReference         string
	ShopperInteraction       string
	RecurringProcessingModel string
	IdempotencyKey           string
	RequestFingerprint       []byte
	RequestCiphertext        []byte
	RequestNonce             []byte
	RequestKeyVersion        string
	ProcessorPaymentID       *string
	Status                   string
	ClaimToken               *string
	FencingToken             int64
	LeaseUntil               *time.Time
	FirstSubmittedAt         *time.Time
	ResolutionDeadline       *time.Time
	RefusalReasonCode        *string
	CompletedAt              *time.Time
}

type Audit struct {
	AuditID    string
	Action     string
	TargetType string
	TargetID   string
	Actor      string
	Reason     string
	Metadata   map[string]any
}

// CloneBytes copies a byte slice, preserving nil.
func CloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// StrPtr returns a pointer to s.
func StrPtr(s string) *string { return &s }

// TimePtr returns a pointer to t.
func TimePtr(t time.Time) *time.Time { return &t }

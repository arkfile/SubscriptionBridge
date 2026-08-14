package adapters

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

var ErrProviderManaged = protocol.ErrProviderManaged

type ProcessorAdapter interface {
	Family() string
	CreateCheckout(ctx context.Context, request CheckoutRequest) (CheckoutResult, error)
	CreatePortalSession(ctx context.Context, processorCustomerID, returnURL string) (string, error)
	CreatePaymentUpdateSession(ctx context.Context, request PaymentUpdateRequest) (PaymentUpdateSession, error)
	ParseWebhook(ctx context.Context, headers http.Header, body []byte) ([]NormalizedEvent, error)
	GetSubscription(ctx context.Context, subscription ProcessorSubscription) (*SubscriptionState, error)
	CancelSubscription(ctx context.Context, subscription ProcessorSubscription, atPeriodEnd bool) error
	ChargeRenewal(ctx context.Context, request RenewalRequest) (RenewalResult, error)
	ResolveRenewalAttempt(ctx context.Context, attempt RenewalAttempt) (RenewalResolution, error)
}

type CheckoutRequest struct {
	CheckoutID      string
	PlanID          string
	ReturnURL       string
	IdempotencyKey  string
	ShopperRef      string
	AmountMinor     int64
	Currency        string
	Interval        string
	StripePriceID   string
	MerchantAccount string
	CountryCode     string
	PublicBaseURL   string
	ExpiresAt       time.Time
}

type CheckoutResult struct {
	RedirectURL         string
	ProcessorCheckoutID string
	ProcessorCustomerID string
	ExpiresAt           time.Time
	Uncertain           bool
}

type PaymentUpdateRequest struct {
	CheckoutID      string
	ShopperRef      string
	ReturnURL       string
	IdempotencyKey  string
	AmountMinor     int64
	Currency        string
	MerchantAccount string
	CountryCode     string
}

type PaymentUpdateSession struct {
	ID          string
	SessionData string
}

type ProcessorSubscription struct {
	Family                  string
	ProcessorCustomerID     string
	ProcessorSubscriptionID string
	ProcessorPaymentID      string
	ShopperReference        string
}

type SubscriptionState struct {
	Status              string
	PlanPriceID         string
	CurrentPeriodStart  time.Time
	CurrentPeriodEnd    time.Time
	CancelAtPeriodEnd   bool
	ProcessorCustomerID string
	ProcessorSubID      string
	ImmediateCancel     bool
	Deleted             bool
}

type NormalizedEvent struct {
	ProcessorFamily   string
	ProcessorEventID  string
	ProviderEventType string
	NormalizedKind    string
	PayloadHash       [32]byte
	OccurredAt        time.Time
	Fields            map[string]any
	SensitivePlain    []byte
}

const (
	KindStripeCheckoutChanged      = "stripe.checkout_changed"
	KindStripeSubscriptionChanged  = "stripe.subscription_changed"
	KindAdyenInitialAuthorisation  = "adyen.initial_authorisation"
	KindAdyenRenewalAuthorisation  = "adyen.renewal_authorisation"
	KindAdyenContractChanged       = "adyen.contract_changed"
	KindAdyenOperationalAdjustment = "adyen.operational_adjustment"
)

type RenewalRequest struct {
	Endpoint         string
	APIVersion       string
	MerchantAccount  string
	AmountMinor      int64
	Currency         string
	AttemptReference string
	ShopperReference string
	PaymentMethodID  string
	IdempotencyKey   string
	CanonicalBody    []byte
}

type RenewalResult struct {
	Status             string
	ProcessorPaymentID string
	RefusalCode        string
	Uncertain          bool
}

type RenewalAttempt struct {
	Endpoint       string
	APIVersion     string
	IdempotencyKey string
	CanonicalBody  []byte
	AttemptRef     string
}

type RenewalResolution struct {
	Status             string
	ProcessorPaymentID string
	RefusalCode        string
	Uncertain          bool
}

// IsTimeout reports whether err is a transport timeout or cancellation.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

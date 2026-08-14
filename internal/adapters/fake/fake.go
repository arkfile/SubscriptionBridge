package fake

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

type Adapter struct {
	mu        sync.Mutex
	FamilyName string
	Sessions  map[string]string
	Subs      map[string]adapters.SubscriptionState
	FailNext  error
	Uncertain bool
}

// New constructs an in-process fake processor for tests.
func New(family string) *Adapter {
	return &Adapter{
		FamilyName: family,
		Sessions:   map[string]string{},
		Subs:       map[string]adapters.SubscriptionState{},
	}
}

// Family returns the configured fake processor family.
func (a *Adapter) Family() string { return a.FamilyName }

// CreateCheckout returns a local fake checkout URL without calling a processor.
func (a *Adapter) CreateCheckout(_ context.Context, request adapters.CheckoutRequest) (adapters.CheckoutResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FailNext != nil {
		err := a.FailNext
		a.FailNext = nil
		return adapters.CheckoutResult{Uncertain: a.Uncertain}, err
	}
	if url, ok := a.Sessions[request.IdempotencyKey]; ok {
		return adapters.CheckoutResult{
			RedirectURL:         url,
			ProcessorCheckoutID: request.CheckoutID,
			ExpiresAt:           request.ExpiresAt,
		}, nil
	}
	url := "https://processor.example/checkout/" + request.CheckoutID
	a.Sessions[request.IdempotencyKey] = url
	return adapters.CheckoutResult{
		RedirectURL:         url,
		ProcessorCheckoutID: request.CheckoutID,
		ExpiresAt:           request.ExpiresAt,
	}, nil
}

// CreatePortalSession returns the given return URL.
func (a *Adapter) CreatePortalSession(_ context.Context, processorCustomerID, returnURL string) (string, error) {
	return returnURL + "/portal", nil
}

// ParseWebhook accepts a simplified JSON fixture as a normalized event.
func (a *Adapter) ParseWebhook(_ context.Context, _ http.Header, body []byte) ([]adapters.NormalizedEvent, error) {
	var payload struct {
		Kind       string `json:"kind"`
		EventID    string `json:"event_id"`
		CheckoutID string `json:"checkout_id"`
		SubID      string `json:"processor_subscription_id"`
		CustomerID string `json:"processor_customer_id"`
		AttemptRef string `json:"attempt_reference"`
		PaymentID  string `json:"processor_payment_id"`
		Success    bool   `json:"success"`
		Status     string `json:"provider_status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	fields := map[string]any{
		"provider_occurred_at": time.Now().UTC().Format(time.RFC3339),
	}
	if payload.CheckoutID != "" {
		fields["checkout_id"] = payload.CheckoutID
	}
	if payload.SubID != "" {
		fields["processor_subscription_id"] = payload.SubID
		fields["authoritative_refresh_required"] = true
	}
	if payload.CustomerID != "" {
		fields["processor_customer_id"] = payload.CustomerID
	}
	if payload.AttemptRef != "" {
		fields["attempt_reference"] = payload.AttemptRef
	}
	if payload.PaymentID != "" {
		fields["processor_payment_id"] = payload.PaymentID
	}
	if payload.Status != "" {
		fields["provider_status"] = payload.Status
	}
	fields["success"] = payload.Success
	kind := payload.Kind
	if kind == "" {
		if a.FamilyName == protocol.ProcessorStripe {
			kind = adapters.KindStripeCheckoutChanged
		} else {
			kind = adapters.KindAdyenInitialAuthorisation
		}
	}
	return []adapters.NormalizedEvent{{
		ProcessorFamily:   a.FamilyName,
		ProcessorEventID:  payload.EventID,
		ProviderEventType: payload.Kind,
		NormalizedKind:    kind,
		PayloadHash:       sum,
		OccurredAt:        time.Now().UTC(),
		Fields:            fields,
	}}, nil
}

// GetSubscription returns injected state or a default active period.
func (a *Adapter) GetSubscription(_ context.Context, subscription adapters.ProcessorSubscription) (*adapters.SubscriptionState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.Subs[subscription.ProcessorSubscriptionID]; ok {
		return &s, nil
	}
	now := time.Now().UTC().Truncate(time.Second)
	st := adapters.SubscriptionState{
		Status:              protocol.StatusActive,
		CurrentPeriodStart:  now,
		CurrentPeriodEnd:    protocol.AddCalendarMonths(now, 1),
		ProcessorSubID:      subscription.ProcessorSubscriptionID,
		ProcessorCustomerID: subscription.ProcessorCustomerID,
	}
	return &st, nil
}

// QA / TODO: no-op fake; does not record cancellation on injected state.
func (a *Adapter) CancelSubscription(context.Context, adapters.ProcessorSubscription, bool) error {
	return nil
}

// ChargeRenewal returns a fake authorised charge, or ErrProviderManaged for Stripe.
func (a *Adapter) ChargeRenewal(context.Context, adapters.RenewalRequest) (adapters.RenewalResult, error) {
	if a.FamilyName == protocol.ProcessorStripe {
		return adapters.RenewalResult{}, adapters.ErrProviderManaged
	}
	return adapters.RenewalResult{Status: "authorized", ProcessorPaymentID: "pay_fake"}, nil
}

// ResolveRenewalAttempt returns a fake authorised resolution, or ErrProviderManaged for Stripe.
func (a *Adapter) ResolveRenewalAttempt(context.Context, adapters.RenewalAttempt) (adapters.RenewalResolution, error) {
	if a.FamilyName == protocol.ProcessorStripe {
		return adapters.RenewalResolution{}, adapters.ErrProviderManaged
	}
	return adapters.RenewalResolution{Status: "authorized", ProcessorPaymentID: "pay_fake"}, nil
}

// SetSub injects authoritative subscription state for tests.
func (a *Adapter) SetSub(id string, st adapters.SubscriptionState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Subs[id] = st
}

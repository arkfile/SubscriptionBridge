package fake

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

type Adapter struct {
	mu                sync.Mutex
	FamilyName        string
	Sessions          map[string]string
	Subs              map[string]adapters.SubscriptionState
	FailNext          error
	Uncertain         bool
	LastCharge        adapters.RenewalRequest
	LastPaymentUpdate adapters.PaymentUpdateRequest
	ChargeResult      *adapters.RenewalResult
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

// CreatePaymentUpdateSession returns a local fake Checkout Session for Drop-in tests.
func (a *Adapter) CreatePaymentUpdateSession(_ context.Context, request adapters.PaymentUpdateRequest) (adapters.PaymentUpdateSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastPaymentUpdate = request
	if a.FailNext != nil {
		err := a.FailNext
		a.FailNext = nil
		return adapters.PaymentUpdateSession{}, err
	}
	if request.ShopperRef == "" || request.CheckoutID == "" {
		return adapters.PaymentUpdateSession{}, fmt.Errorf("missing shopper or checkout")
	}
	return adapters.PaymentUpdateSession{ID: "CS_fake", SessionData: "session_fake"}, nil
}

// PaymentUpdate returns the last Drop-in session request.
func (a *Adapter) PaymentUpdate() adapters.PaymentUpdateRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LastPaymentUpdate
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
		Token      string `json:"token"`
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
		SensitivePlain:    []byte(payload.Token),
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

// CancelSubscription records cancellation on injected fake state.
func (a *Adapter) CancelSubscription(_ context.Context, subscription adapters.ProcessorSubscription, atPeriodEnd bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.Subs[subscription.ProcessorSubscriptionID]; ok {
		st.CancelAtPeriodEnd = atPeriodEnd
		if !atPeriodEnd {
			st.ImmediateCancel = true
			st.Status = protocol.StatusExpired
		} else {
			st.CancelAtPeriodEnd = true
			st.Status = protocol.StatusCanceled
		}
		a.Subs[subscription.ProcessorSubscriptionID] = st
	}
	return nil
}

// ChargeRenewal returns a fake authorised charge, or ErrProviderManaged for Stripe.
func (a *Adapter) ChargeRenewal(_ context.Context, request adapters.RenewalRequest) (adapters.RenewalResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastCharge = request
	if a.FamilyName == protocol.ProcessorStripe {
		return adapters.RenewalResult{}, adapters.ErrProviderManaged
	}
	if a.FailNext != nil {
		err := a.FailNext
		a.FailNext = nil
		return adapters.RenewalResult{Uncertain: a.Uncertain || adapters.IsTimeout(err)}, err
	}
	if a.Uncertain {
		return adapters.RenewalResult{Uncertain: true}, nil
	}
	if a.ChargeResult != nil {
		return *a.ChargeResult, nil
	}
	return adapters.RenewalResult{Status: "authorized", ProcessorPaymentID: "pay_fake"}, nil
}

// ResolveRenewalAttempt replays the last fake charge, or ErrProviderManaged for Stripe.
func (a *Adapter) ResolveRenewalAttempt(ctx context.Context, attempt adapters.RenewalAttempt) (adapters.RenewalResolution, error) {
	res, err := a.ChargeRenewal(ctx, adapters.RenewalRequest{
		Endpoint:         attempt.Endpoint,
		APIVersion:       attempt.APIVersion,
		IdempotencyKey:   attempt.IdempotencyKey,
		CanonicalBody:    attempt.CanonicalBody,
		AttemptReference: attempt.AttemptRef,
	})
	return adapters.RenewalResolution{
		Status:             res.Status,
		ProcessorPaymentID: res.ProcessorPaymentID,
		RefusalCode:        res.RefusalCode,
		Uncertain:          res.Uncertain,
	}, err
}

// SetSub injects authoritative subscription state for tests.
func (a *Adapter) SetSub(id string, st adapters.SubscriptionState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Subs[id] = st
}

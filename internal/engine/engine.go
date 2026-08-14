package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/envelope"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

var errUnknownStripePrice = errors.New("unknown stripe price id")

type Engine struct {
	Store    store.Store
	Config   config.Config
	Keys     hmac.Keys
	Adapters map[string]adapters.ProcessorAdapter
	Box      *envelope.Box
	Log      *slog.Logger
	Clock    func() time.Time
}

// now returns the engine clock truncated to UTC seconds.
func (e *Engine) now() time.Time {
	if e.Clock != nil {
		return protocol.TruncateUTC(e.Clock())
	}
	return e.Store.Now()
}

// logger returns the configured logger or slog.Default.
func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// adapter returns the processor adapter for a trusted family name.
func (e *Engine) adapter(family string) (adapters.ProcessorAdapter, error) {
	a, ok := e.Adapters[family]
	if !ok {
		return nil, fmt.Errorf("no adapter for %s", family)
	}
	return a, nil
}

type StartResult struct {
	RedirectURL string
	Resumed     bool
}

// StartCheckout verifies a start token and creates or resumes checkout at the processor.
func (e *Engine) StartCheckout(ctx context.Context, token string) (StartResult, error) {
	now := e.now()
	claims, payload, err := hmac.ParseStartToken(e.Keys.Token, token, now)
	if err != nil {
		return StartResult{}, err
	}
	plan, family, err := e.Config.ResolvePlan(claims.PlanID)
	if err != nil {
		return StartResult{}, err
	}
	fp := hmac.Fingerprint(payload)
	idemp := "sb_checkout_" + claims.CheckoutID
	var shopper *string
	if family == protocol.ProcessorAdyen {
		ref, err := protocol.NewShopperReference()
		if err != nil {
			return StartResult{}, err
		}
		shopper = &ref
	}
	var existing store.Checkout
	var found bool
	err = e.Store.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckoutForUpdate(claims.CheckoutID)
		if errors.Is(err, store.ErrNotFound) {
			expires := time.Unix(claims.Exp, 0).UTC().Add(protocol.ClockSkew)
			row := store.Checkout{
				CheckoutID:                claims.CheckoutID,
				PlanID:                    claims.PlanID,
				NormalizedReturnURL:       claims.ReturnURL,
				ProcessorFamily:           family,
				RequestFingerprint:        fp[:],
				ProviderIdempotencyKey:    idemp,
				ProcessorShopperReference: shopper,
				Status:                    protocol.CheckoutCreating,
				ExpiresAt:                 protocol.TruncateUTC(expires),
			}
			return tx.InsertCheckout(row)
		}
		if err != nil {
			return err
		}
		found = true
		existing = c
		return nil
	})
	if err != nil {
		return StartResult{}, err
	}
	if found {
		if hex.EncodeToString(existing.RequestFingerprint) != hex.EncodeToString(fp[:]) ||
			existing.PlanID != claims.PlanID ||
			existing.NormalizedReturnURL != claims.ReturnURL ||
			existing.ProcessorFamily != family {
			return StartResult{}, protocol.ErrCheckoutConflict
		}
		if store.IsTerminalCheckout(existing.Status) {
			return StartResult{}, protocol.ErrCheckoutTerminal
		}
		if existing.Status == protocol.CheckoutPending && existing.ProcessorCheckoutID != nil {
			ad, err := e.adapter(family)
			if err != nil {
				return StartResult{}, err
			}
			res, err := ad.CreateCheckout(ctx, e.checkoutRequest(existing, plan, family))
			if err != nil {
				return StartResult{}, err
			}
			if res.RedirectURL != "" {
				return StartResult{RedirectURL: res.RedirectURL, Resumed: true}, nil
			}
		}
	} else {
		existing, err = e.loadCheckout(ctx, claims.CheckoutID)
		if err != nil {
			return StartResult{}, err
		}
	}
	ad, err := e.adapter(family)
	if err != nil {
		return StartResult{}, err
	}
	res, err := ad.CreateCheckout(ctx, e.checkoutRequest(existing, plan, family))
	if err != nil {
		if adapters.IsTimeout(err) || res.Uncertain {
			return StartResult{}, err
		}
		return StartResult{}, err
	}
	if res.Uncertain {
		return StartResult{}, fmt.Errorf("provider timeout")
	}
	err = e.Store.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckoutForUpdate(claims.CheckoutID)
		if err != nil {
			return err
		}
		c.Status = protocol.CheckoutPending
		c.ProcessorCheckoutID = store.StrPtr(res.ProcessorCheckoutID)
		if !res.ExpiresAt.IsZero() {
			c.ExpiresAt = protocol.TruncateUTC(res.ExpiresAt)
		}
		return tx.UpdateCheckout(c)
	})
	if err != nil {
		return StartResult{}, err
	}
	return StartResult{RedirectURL: res.RedirectURL}, nil
}

// checkoutRequest builds the adapter checkout request from bound checkout state.
func (e *Engine) checkoutRequest(c store.Checkout, plan config.Plan, family string) adapters.CheckoutRequest {
	req := adapters.CheckoutRequest{
		CheckoutID:     c.CheckoutID,
		PlanID:         c.PlanID,
		ReturnURL:      c.NormalizedReturnURL,
		IdempotencyKey: c.ProviderIdempotencyKey,
		AmountMinor:    plan.AmountMinor,
		Currency:       plan.Currency,
		Interval:       plan.Interval,
		StripePriceID:  plan.Stripe.PriceID,
		MerchantAccount: plan.Adyen.MerchantAccount,
		CountryCode:    plan.Adyen.CountryCode,
		PublicBaseURL:  e.Config.PublicURL,
		ExpiresAt:      c.ExpiresAt,
	}
	if c.ProcessorShopperReference != nil {
		req.ShopperRef = *c.ProcessorShopperReference
	}
	_ = family
	return req
}

// loadCheckout reads a checkout outside a long-held lock.
func (e *Engine) loadCheckout(ctx context.Context, id string) (store.Checkout, error) {
	var c store.Checkout
	err := e.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		c, err = tx.GetCheckout(id)
		return err
	})
	return c, err
}

// Snapshot returns the current consumer-visible subscription snapshot.
func (e *Engine) Snapshot(ctx context.Context, ref string) (protocol.Snapshot, error) {
	if err := protocol.ValidateSubscriptionRef(ref); err != nil {
		return protocol.Snapshot{}, err
	}
	var sub store.Subscription
	err := e.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		sub, err = tx.GetSubscription(ref)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Snapshot{}, protocol.ErrNotFound
		}
		return protocol.Snapshot{}, err
	}
	return snapshotFrom(sub), nil
}

// snapshotFrom projects stored subscription state into the protocol snapshot.
func snapshotFrom(sub store.Subscription) protocol.Snapshot {
	return protocol.Snapshot{
		Protocol:           protocol.Name,
		Version:            protocol.Version,
		CheckoutID:         sub.CheckoutID,
		SubscriptionRef:    sub.SubscriptionRef,
		PlanID:             sub.PlanID,
		StateVersion:       sub.StateVersion,
		Status:             sub.Status,
		CurrentPeriodStart: protocol.FormatUTC(sub.CurrentPeriodStart),
		CurrentPeriodEnd:   protocol.FormatUTC(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd:  sub.CancelAtPeriodEnd,
		StateChangedAt:     protocol.FormatUTC(sub.StateChangedAt),
	}
}

// commitChange writes subscription state and the matching outbound event in one transaction.
func (e *Engine) commitChange(tx store.Tx, current, next store.Subscription, eventType string) error {
	body, eventID, err := marshalCallback(next, eventType)
	if err != nil {
		return err
	}
	if current.SubscriptionRef == "" {
		if err := tx.InsertSubscription(next); err != nil {
			return err
		}
	} else if err := tx.UpdateSubscription(next); err != nil {
		return err
	}
	now := tx.Now()
	ev := store.OutboundEvent{
		EventID:         eventID,
		EventType:       eventType,
		SubscriptionRef: next.SubscriptionRef,
		CheckoutID:      next.CheckoutID,
		StateVersion:    next.StateVersion,
		PayloadBody:     body,
		DeliveryState:   protocol.DeliveryPending,
		NextAttemptAt:   store.TimePtr(now),
	}
	if err := tx.InsertOutbound(ev); err != nil {
		return err
	}
	if next.ProcessorFamily == protocol.ProcessorAdyen && (eventType == protocol.EventActivated || eventType == protocol.EventRenewed) && next.Status == protocol.StatusActive {
		key := protocol.ActionKey(next.SubscriptionRef, protocol.ActionRenew, next.CurrentPeriodEnd)
		aid, err := newUUID()
		if err != nil {
			return err
		}
		if err := tx.InsertAction(store.ScheduledAction{
			ActionID:        aid,
			ActionKey:       key,
			SubscriptionRef: next.SubscriptionRef,
			ActionType:      protocol.ActionRenew,
			TargetAt:        next.CurrentPeriodEnd,
			DueAt:           next.CurrentPeriodEnd,
			Status:          "pending",
		}); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	if eventType == protocol.EventCanceled {
		key := protocol.ActionKey(next.SubscriptionRef, protocol.ActionExpire, next.CurrentPeriodEnd)
		aid, err := newUUID()
		if err != nil {
			return err
		}
		if err := tx.CancelActionsForSubscription(next.SubscriptionRef, key); err != nil {
			return err
		}
		if err := tx.InsertAction(store.ScheduledAction{
			ActionID:        aid,
			ActionKey:       key,
			SubscriptionRef: next.SubscriptionRef,
			ActionType:      protocol.ActionExpire,
			TargetAt:        next.CurrentPeriodEnd,
			DueAt:           next.CurrentPeriodEnd,
			Status:          "pending",
		}); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	if eventType == protocol.EventExpired || eventType == protocol.EventRenewed {
		if err := tx.CancelActionsForSubscription(next.SubscriptionRef, ""); err != nil {
			return err
		}
	}
	return nil
}

// marshalCallback serializes the immutable outbound callback bytes and event ID.
func marshalCallback(sub store.Subscription, eventType string) ([]byte, string, error) {
	eventID, err := protocol.NewEventID()
	if err != nil {
		return nil, "", err
	}
	cb, err := protocol.NewCallback(
		eventID, eventType, sub.CheckoutID, sub.SubscriptionRef, sub.PlanID,
		sub.StateVersion, sub.Status, sub.CurrentPeriodStart, sub.CurrentPeriodEnd,
		sub.StateChangedAt, sub.CancelAtPeriodEnd,
	)
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(cb)
	if err != nil {
		return nil, "", err
	}
	return body, eventID, nil
}

// newUUID allocates a UUID string for row primary keys.
func newUUID() (string, error) {
	b, err := protocol.RandomClaimToken()
	if err != nil {
		return "", err
	}
	return protocol.UUIDString(b), nil
}

// IngestAndProcess verifies a provider webhook and applies normalized events.
func (e *Engine) IngestAndProcess(ctx context.Context, family string, headers http.Header, body []byte) error {
	ad, err := e.adapter(family)
	if err != nil {
		return err
	}
	events, err := ad.ParseWebhook(ctx, headers, body)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := e.processNormalized(ctx, ad, ev); err != nil {
			return err
		}
	}
	return nil
}

// processNormalized leases a provider event, observes provider state, then commits transitions.
func (e *Engine) processNormalized(ctx context.Context, ad adapters.ProcessorAdapter, ev adapters.NormalizedEvent) error {
	now := e.now()
	actionID, err := newUUID()
	if err != nil {
		return err
	}
	row := store.ProcessorEvent{
		ProcessorFamily:    ev.ProcessorFamily,
		ProcessorEventID:   ev.ProcessorEventID,
		ProcessingActionID: actionID,
		ProviderEventType:  ev.ProviderEventType,
		PayloadHash:        ev.PayloadHash[:],
		NormalizedFields:   ev.Fields,
		ProcessingState:    "pending",
	}
	if len(ev.SensitivePlain) > 0 && e.Box != nil {
		ct, nonce, ver, err := e.Box.Seal(ev.SensitivePlain, envelope.AAD("provider-sensitive", ev.ProcessorEventID))
		if err != nil {
			return err
		}
		row.SensitiveCiphertext = ct
		row.SensitiveNonce = nonce
		row.SensitiveKeyVersion = &ver
	}
	var inserted bool
	if err := e.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		inserted, err = tx.InsertProcessorEvent(row)
		return err
	}); err != nil {
		return err
	}
	if !inserted {
		return nil
	}
	processingKey := processingKeyFor(ev)
	var lease store.ProcessingLease
	if err := e.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		_, lease, err = tx.ClaimProcessorEvent(ev.ProcessorFamily, ev.ProcessorEventID, processingKey, now, 2*time.Minute)
		return err
	}); err != nil {
		return err
	}
	state, err := e.observeProvider(ctx, ad, ev)
	if err != nil {
		e.logger().Info("provider_observe_failed", "family", ev.ProcessorFamily)
		return err
	}
	return e.Store.InTx(ctx, func(tx store.Tx) error {
		cur, err := tx.GetProcessorEvent(ev.ProcessorFamily, ev.ProcessorEventID)
		if err != nil {
			return err
		}
		if cur.ProcessingState != "running" || cur.ClaimToken == nil || lease.ClaimToken == nil || *cur.ClaimToken != *lease.ClaimToken || cur.FencingToken != lease.FencingToken {
			return nil
		}
		subRef, applyErr := e.applyObservation(tx, ev, state, now)
		if applyErr != nil {
			if errors.Is(applyErr, errUnknownStripePrice) {
				if err := tx.FinishProcessorEvent(ev.ProcessorFamily, ev.ProcessorEventID, *cur.ClaimToken, cur.FencingToken, "quarantined", subRef, now); err != nil {
					return err
				}
				return tx.ReleaseProcessorLease(processingKey, *lease.ClaimToken, lease.FencingToken)
			}
			return applyErr
		}
		if err := tx.FinishProcessorEvent(ev.ProcessorFamily, ev.ProcessorEventID, *cur.ClaimToken, cur.FencingToken, "processed", subRef, now); err != nil {
			return err
		}
		return tx.ReleaseProcessorLease(processingKey, *lease.ClaimToken, lease.FencingToken)
	})
}

// processingKeyFor derives the Stripe/Adyen processing-lease key for an event.
func processingKeyFor(ev adapters.NormalizedEvent) string {
	if v, ok := ev.Fields["checkout_id"].(string); ok && v != "" {
		return v
	}
	if v, ok := ev.Fields["processor_subscription_id"].(string); ok && v != "" {
		return v
	}
	if v, ok := ev.Fields["attempt_reference"].(string); ok && v != "" {
		return v
	}
	return ev.ProcessorFamily + ":" + ev.ProcessorEventID
}

// observeProvider retrieves authoritative processor state outside any DB transaction.
func (e *Engine) observeProvider(ctx context.Context, ad adapters.ProcessorAdapter, ev adapters.NormalizedEvent) (*adapters.SubscriptionState, error) {
	if ev.NormalizedKind == adapters.KindStripeCheckoutChanged || ev.NormalizedKind == adapters.KindStripeSubscriptionChanged {
		subID, _ := ev.Fields["processor_subscription_id"].(string)
		if subID == "" {
			return nil, nil
		}
		return ad.GetSubscription(ctx, adapters.ProcessorSubscription{
			Family:                  ev.ProcessorFamily,
			ProcessorSubscriptionID: subID,
		})
	}
	return nil, nil
}

// applyObservation dispatches a normalized event to Stripe or Adyen transition logic.
func (e *Engine) applyObservation(tx store.Tx, ev adapters.NormalizedEvent, state *adapters.SubscriptionState, now time.Time) (*string, error) {
	switch ev.NormalizedKind {
	case adapters.KindStripeCheckoutChanged, adapters.KindStripeSubscriptionChanged:
		return e.applyStripe(tx, ev, state, now)
	case adapters.KindAdyenInitialAuthorisation:
		return e.applyAdyenInitial(tx, ev, now)
	case adapters.KindAdyenRenewalAuthorisation:
		return e.applyAdyenRenewal(tx, ev, now)
	case adapters.KindAdyenContractChanged:
		return e.applyAdyenContract(tx, ev, now)
	default:
		return nil, nil
	}
}

// applyStripe maps authoritative Stripe state onto local subscription transitions.
func (e *Engine) applyStripe(tx store.Tx, ev adapters.NormalizedEvent, state *adapters.SubscriptionState, now time.Time) (*string, error) {
	if state == nil {
		return nil, nil
	}
	checkoutID, _ := ev.Fields["checkout_id"].(string)
	var sub store.Subscription
	var err error
	if state.ProcessorSubID != "" {
		sub, err = tx.GetSubscriptionByProcessor(protocol.ProcessorStripe, state.ProcessorSubID)
	}
	if errors.Is(err, store.ErrNotFound) || sub.SubscriptionRef == "" {
		if checkoutID == "" {
			checkoutID, _ = ev.Fields["checkout_id"].(string)
		}
		if checkoutID != "" {
			sub, err = tx.GetSubscriptionByCheckout(checkoutID)
		}
	}
	if errors.Is(err, store.ErrNotFound) || sub.SubscriptionRef == "" {
		if checkoutID == "" {
			return nil, nil
		}
		co, err := tx.GetCheckoutForUpdate(checkoutID)
		if err != nil {
			return nil, err
		}
		if state.Status != protocol.StatusActive && state.Status != protocol.StatusTrialing {
			return nil, nil
		}
		ref, err := protocol.NewSubscriptionRef()
		if err != nil {
			return nil, err
		}
		cust := state.ProcessorCustomerID
		psub := state.ProcessorSubID
		created := FirstActivation(co, ref, state.Status, state.CurrentPeriodStart, state.CurrentPeriodEnd, now, strPtr(cust), strPtr(psub), nil, nil)
		if err := e.commitChange(tx, store.Subscription{}, created, protocol.EventActivated); err != nil {
			return nil, err
		}
		co.Status = protocol.CheckoutCompleted
		co.SubscriptionRef = &ref
		if err := tx.UpdateCheckout(co); err != nil {
			return nil, err
		}
		return &ref, nil
	}
	obs := Observation{
		Status:            state.Status,
		PeriodStart:       state.CurrentPeriodStart,
		PeriodEnd:         state.CurrentPeriodEnd,
		CancelAtPeriodEnd: state.CancelAtPeriodEnd,
		ImmediateExpire:   state.ImmediateCancel || state.Deleted,
	}
	if state.PlanPriceID != "" {
		if planID, ok := e.Config.StripePriceMap()[state.PlanPriceID]; ok {
			obs.PlanID = planID
			if planID != sub.PlanID {
				obs.EventType = protocol.EventPlanChanged
			}
		} else if state.PlanPriceID != "" {
			return &sub.SubscriptionRef, errUnknownStripePrice
		}
	}
	dec, err := Decide(sub, obs, now)
	if err != nil {
		return nil, err
	}
	if dec.Noop {
		return &sub.SubscriptionRef, nil
	}
	if err := e.commitChange(tx, sub, dec.Next, dec.EventType); err != nil {
		return nil, err
	}
	return &dec.Next.SubscriptionRef, nil
}

// applyAdyenInitial activates a subscription after a successful initial Adyen authorisation.
func (e *Engine) applyAdyenInitial(tx store.Tx, ev adapters.NormalizedEvent, now time.Time) (*string, error) {
	success, _ := ev.Fields["success"].(bool)
	checkoutID, _ := ev.Fields["checkout_id"].(string)
	if !success || checkoutID == "" {
		return nil, nil
	}
	co, err := tx.GetCheckoutForUpdate(checkoutID)
	if err != nil {
		return nil, err
	}
	if co.Status == protocol.CheckoutCompleted && co.SubscriptionRef != nil {
		return co.SubscriptionRef, nil
	}
	ref, err := protocol.NewSubscriptionRef()
	if err != nil {
		return nil, err
	}
	payID, _ := ev.Fields["processor_payment_id"].(string)
	periodStart := now
	periodEnd := protocol.AddCalendarMonths(periodStart, 1)
	created := FirstActivation(co, ref, protocol.StatusActive, periodStart, periodEnd, now, nil, nil, strPtr(payID), co.ProcessorShopperReference)
	if len(ev.SensitivePlain) == 0 && e.Box != nil {
		// token already sealed on the processor event; copy after open is handled by caller storing ciphertext on subscription in a later step
	}
	pe, _ := tx.GetProcessorEvent(ev.ProcessorFamily, ev.ProcessorEventID)
	if len(pe.SensitiveCiphertext) > 0 {
		created.PaymentMethodCiphertext = pe.SensitiveCiphertext
		created.PaymentMethodNonce = pe.SensitiveNonce
		created.PaymentMethodKeyVersion = pe.SensitiveKeyVersion
	}
	if err := e.commitChange(tx, store.Subscription{}, created, protocol.EventActivated); err != nil {
		return nil, err
	}
	co.Status = protocol.CheckoutCompleted
	co.SubscriptionRef = &ref
	if err := tx.UpdateCheckout(co); err != nil {
		return nil, err
	}
	return &ref, nil
}

// QA / TODO: dead attempt lookup; store has no GetAttemptByReference, so correlation is incomplete.
func (e *Engine) applyAdyenRenewal(tx store.Tx, ev adapters.NormalizedEvent, now time.Time) (*string, error) {
	attemptRef, _ := ev.Fields["attempt_reference"].(string)
	success, _ := ev.Fields["success"].(bool)
	if attemptRef == "" {
		return nil, nil
	}
	var att store.ChargeAttempt
	found := false
	// lookup by scanning attempts for the action is done via GetAttempts; memory store has no index by attempt_reference
	_ = att
	_ = found
	subs, err := tx.GetSubscription("")
	_ = subs
	_ = err
	// Find subscription via attempt reference uniqueness in charge attempts: iterate is not on Tx.
	// Scheduler path updates attempts; webhook correlates by attempt_reference stored on the attempt.
	return e.applyAdyenRenewalByRef(tx, attemptRef, success, ev, now)
}

// QA / TODO: ignores attempt_reference and correlates only by processor_payment_id.
func (e *Engine) applyAdyenRenewalByRef(tx store.Tx, attemptRef string, success bool, ev adapters.NormalizedEvent, now time.Time) (*string, error) {
	_ = attemptRef
	payID, _ := ev.Fields["processor_payment_id"].(string)
	if payID != "" {
		sub, err := tx.GetSubscriptionByChargePayment(payID)
		if err == nil {
			obs := Observation{Status: protocol.StatusPastDue}
			if success {
				obs = Observation{
					Status:      protocol.StatusActive,
					PeriodStart: sub.CurrentPeriodEnd,
					PeriodEnd:   protocol.AddCalendarMonths(sub.CurrentPeriodEnd, 1),
				}
			}
			dec, err := Decide(sub, obs, now)
			if err != nil {
				return nil, err
			}
			if dec.Noop {
				return &sub.SubscriptionRef, nil
			}
			if err := e.commitChange(tx, sub, dec.Next, dec.EventType); err != nil {
				return nil, err
			}
			return &dec.Next.SubscriptionRef, nil
		}
	}
	return nil, nil
}

// applyAdyenContract applies Adyen cancellation/contract-change notifications.
func (e *Engine) applyAdyenContract(tx store.Tx, ev adapters.NormalizedEvent, now time.Time) (*string, error) {
	payID, _ := ev.Fields["processor_payment_id"].(string)
	if payID == "" {
		return nil, nil
	}
	sub, err := tx.GetSubscriptionByInitialPayment(protocol.ProcessorAdyen, payID)
	if errors.Is(err, store.ErrNotFound) {
		sub, err = tx.GetSubscriptionByChargePayment(payID)
	}
	if err != nil {
		return nil, nil
	}
	dec, err := Decide(sub, Observation{Status: protocol.StatusCanceled, CancelAtPeriodEnd: true}, now)
	if err != nil {
		return nil, err
	}
	if dec.Noop {
		return &sub.SubscriptionRef, nil
	}
	if err := e.commitChange(tx, sub, dec.Next, dec.EventType); err != nil {
		return nil, err
	}
	return &dec.Next.SubscriptionRef, nil
}

// CancelAtPeriodEnd asks the processor (when applicable) and records canceled-through-period-end.
func (e *Engine) CancelAtPeriodEnd(ctx context.Context, ref string) error {
	now := e.now()
	return e.Store.InTx(ctx, func(tx store.Tx) error {
		sub, err := tx.GetSubscriptionForUpdate(ref)
		if err != nil {
			return err
		}
		dec, err := Decide(sub, Observation{Status: protocol.StatusCanceled, CancelAtPeriodEnd: true}, now)
		if err != nil {
			return err
		}
		if dec.Noop {
			return nil
		}
		return e.commitChange(tx, sub, dec.Next, dec.EventType)
	})
}

// ExpireNow applies an immediate expired transition for scheduler or mock paths.
func (e *Engine) ExpireNow(ctx context.Context, ref string) error {
	now := e.now()
	return e.Store.InTx(ctx, func(tx store.Tx) error {
		sub, err := tx.GetSubscriptionForUpdate(ref)
		if err != nil {
			return err
		}
		dec, err := Decide(sub, Observation{Status: protocol.StatusExpired, ImmediateExpire: true}, now)
		if err != nil {
			return err
		}
		if dec.Noop {
			return nil
		}
		return e.commitChange(tx, sub, dec.Next, dec.EventType)
	})
}

// ActivateCheckout is the mock/dev path that activates without a provider webhook.
func (e *Engine) ActivateCheckout(ctx context.Context, checkoutID string) error {
	now := e.now()
	return e.Store.InTx(ctx, func(tx store.Tx) error {
		co, err := tx.GetCheckoutForUpdate(checkoutID)
		if err != nil {
			return err
		}
		if co.SubscriptionRef != nil {
			return nil
		}
		ref, err := protocol.NewSubscriptionRef()
		if err != nil {
			return err
		}
		created := FirstActivation(co, ref, protocol.StatusActive, now, protocol.AddCalendarMonths(now, 1), now, nil, nil, nil, co.ProcessorShopperReference)
		if err := e.commitChange(tx, store.Subscription{}, created, protocol.EventActivated); err != nil {
			return err
		}
		co.Status = protocol.CheckoutCompleted
		co.SubscriptionRef = &ref
		return tx.UpdateCheckout(co)
	})
}

// SweepCheckouts expires pending checkouts whose TTL has elapsed.
func (e *Engine) SweepCheckouts(ctx context.Context) error {
	return e.Store.InTx(ctx, func(tx store.Tx) error {
		_, err := tx.ExpireDueCheckouts(tx.Now())
		return err
	})
}

// Commit applies a Decision's subscription update and outbound event.
func (e *Engine) Commit(tx store.Tx, current store.Subscription, dec Decision) error {
	if dec.Noop {
		return nil
	}
	return e.commitChange(tx, current, dec.Next, dec.EventType)
}

// strPtr returns a pointer to s.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

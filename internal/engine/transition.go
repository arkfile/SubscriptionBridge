package engine

import (
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

type Observation struct {
	EventType         string
	Status            string
	PlanID            string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	CancelAtPeriodEnd bool
	ImmediateExpire   bool
}

type Decision struct {
	Noop       bool
	EventType  string
	Next       store.Subscription
	CreateExpire bool
	CancelExpire bool
	CreateRenew  bool
	CancelRenew  bool
}

// Decide maps an authoritative observation onto the next protocol state or a no-op.
func Decide(current store.Subscription, obs Observation, now time.Time) (Decision, error) {
	now = protocol.TruncateUTC(now)
	if current.Status == protocol.StatusExpired {
		return Decision{Noop: true}, nil
	}
	next := current
	next.StateChangedAt = now
	next.UpdatedAt = now

	if obs.PlanID != "" {
		next.PlanID = obs.PlanID
	}
	if !obs.PeriodStart.IsZero() {
		next.CurrentPeriodStart = protocol.TruncateUTC(obs.PeriodStart)
	}
	if !obs.PeriodEnd.IsZero() {
		next.CurrentPeriodEnd = protocol.TruncateUTC(obs.PeriodEnd)
	}
	if !next.CurrentPeriodEnd.After(next.CurrentPeriodStart) {
		return Decision{}, protocol.ErrUnorderedPeriod
	}

	switch {
	case obs.ImmediateExpire || obs.Status == protocol.StatusExpired:
		return expire(current, next, now, obs.ImmediateExpire && current.Status != protocol.StatusCanceled)
	case obs.EventType == protocol.EventPlanChanged || (obs.PlanID != "" && obs.PlanID != current.PlanID && obs.Status == protocol.StatusActive):
		if current.Status != protocol.StatusActive {
			return Decision{Noop: true}, nil
		}
		if current.PlanID == next.PlanID && samePeriod(current, next) {
			return Decision{Noop: true}, nil
		}
		next.Status = protocol.StatusActive
		next.CancelAtPeriodEnd = false
		next.StateVersion = current.StateVersion + 1
		if err := protocol.ValidateEventStatus(protocol.EventPlanChanged, next.Status, next.CancelAtPeriodEnd); err != nil {
			return Decision{}, err
		}
		return Decision{EventType: protocol.EventPlanChanged, Next: next}, nil
	case obs.Status == protocol.StatusCanceled || obs.CancelAtPeriodEnd:
		return cancelAtPeriodEnd(current, next, now)
	case obs.Status == protocol.StatusPastDue:
		return pastDue(current, next, now)
	case obs.Status == protocol.StatusActive || obs.Status == protocol.StatusTrialing:
		return activateOrRenew(current, next, obs.Status, now)
	default:
		return Decision{Noop: true}, nil
	}
}

// samePeriod reports whether two subscriptions share the same billing window.
func samePeriod(a, b store.Subscription) bool {
	return a.CurrentPeriodStart.Equal(b.CurrentPeriodStart) && a.CurrentPeriodEnd.Equal(b.CurrentPeriodEnd)
}

// expire transitions to terminal expired status.
func expire(current, next store.Subscription, now time.Time, setCanceledAt bool) (Decision, error) {
	if current.Status == protocol.StatusExpired {
		return Decision{Noop: true}, nil
	}
	next.Status = protocol.StatusExpired
	next.CancelAtPeriodEnd = false
	next.StateVersion = current.StateVersion + 1
	if setCanceledAt && current.CanceledAt == nil {
		next.CanceledAt = &now
	}
	if err := protocol.ValidateEventStatus(protocol.EventExpired, next.Status, next.CancelAtPeriodEnd); err != nil {
		return Decision{}, err
	}
	return Decision{EventType: protocol.EventExpired, Next: next, CancelExpire: true, CancelRenew: true}, nil
}

// cancelAtPeriodEnd transitions to canceled while remaining effective through period end.
func cancelAtPeriodEnd(current, next store.Subscription, now time.Time) (Decision, error) {
	if current.Status == protocol.StatusCanceled {
		return Decision{Noop: true}, nil
	}
	if current.Status == protocol.StatusExpired {
		return Decision{Noop: true}, nil
	}
	next.Status = protocol.StatusCanceled
	next.CancelAtPeriodEnd = true
	next.StateVersion = current.StateVersion + 1
	next.CanceledAt = &now
	if err := protocol.ValidateEventStatus(protocol.EventCanceled, next.Status, next.CancelAtPeriodEnd); err != nil {
		return Decision{}, err
	}
	return Decision{EventType: protocol.EventCanceled, Next: next, CreateExpire: true, CancelRenew: true}, nil
}

// pastDue transitions to past_due after a failed renewal.
func pastDue(current, next store.Subscription, now time.Time) (Decision, error) {
	if current.Status == protocol.StatusPastDue {
		return Decision{Noop: true}, nil
	}
	next.Status = protocol.StatusPastDue
	next.CancelAtPeriodEnd = false
	next.StateVersion = current.StateVersion + 1
	if next.PastDueSince == nil {
		next.PastDueSince = &now
	}
	if err := protocol.ValidateEventStatus(protocol.EventPastDue, next.Status, next.CancelAtPeriodEnd); err != nil {
		return Decision{}, err
	}
	return Decision{EventType: protocol.EventPastDue, Next: next}, nil
}

// activateOrRenew activates a new subscription or records a provider-authoritative renewal.
func activateOrRenew(current, next store.Subscription, status string, now time.Time) (Decision, error) {
	event := protocol.EventActivated
	if current.StateVersion >= 1 && current.Status != "" {
		event = protocol.EventRenewed
		status = protocol.StatusActive
	}
	if current.Status == status && samePeriod(current, next) && current.PlanID == next.PlanID && !current.CancelAtPeriodEnd {
		return Decision{Noop: true}, nil
	}
	if current.Status == protocol.StatusCanceled && event != protocol.EventRenewed {
		return Decision{Noop: true}, nil
	}
	next.Status = status
	next.CancelAtPeriodEnd = false
	next.StateVersion = current.StateVersion + 1
	if next.StateVersion == 1 {
		event = protocol.EventActivated
	}
	if event == protocol.EventRenewed {
		next.PastDueSince = nil
		next.CanceledAt = nil
		next.AutomaticChargingBlocked = false
		next.ChargingBlockReason = nil
	}
	if err := protocol.ValidateEventStatus(event, next.Status, next.CancelAtPeriodEnd); err != nil {
		return Decision{}, err
	}
	d := Decision{EventType: event, Next: next, CancelExpire: event == protocol.EventRenewed, CreateRenew: next.ProcessorFamily == protocol.ProcessorAdyen && event == protocol.EventRenewed || (event == protocol.EventActivated && next.ProcessorFamily == protocol.ProcessorAdyen)}
	if event == protocol.EventActivated && next.ProcessorFamily == protocol.ProcessorAdyen {
		d.CreateRenew = true
	}
	return d, nil
}

// FirstActivation builds the initial subscription row for a completed checkout.
func FirstActivation(checkout store.Checkout, subRef, status string, periodStart, periodEnd, now time.Time, processorCustomer, processorSub, initialPayment, shopper *string) store.Subscription {
	now = protocol.TruncateUTC(now)
	return store.Subscription{
		SubscriptionRef:           subRef,
		CheckoutID:                checkout.CheckoutID,
		PlanID:                    checkout.PlanID,
		Status:                    status,
		StateVersion:              1,
		ProcessorFamily:           checkout.ProcessorFamily,
		ProcessorCustomerID:       processorCustomer,
		ProcessorSubscriptionID:   processorSub,
		ProcessorInitialPaymentID: initialPayment,
		ProcessorShopperReference: shopper,
		CurrentPeriodStart:        protocol.TruncateUTC(periodStart),
		CurrentPeriodEnd:          protocol.TruncateUTC(periodEnd),
		CancelAtPeriodEnd:         false,
		StateChangedAt:            now,
	}
}

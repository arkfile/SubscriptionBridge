package engine_test

import (
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

// TestTransitionMatrix covers past_due, cancel, renew, expire, and expired no-op transitions.
func TestTransitionMatrix(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	base := store.Subscription{
		SubscriptionRef:    "sub_a8f3c1d2",
		CheckoutID:         "subchk_7f3a9c2e",
		PlanID:             "plan_500gb",
		Status:             protocol.StatusActive,
		StateVersion:       1,
		ProcessorFamily:    protocol.ProcessorStripe,
		CurrentPeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CurrentPeriodEnd:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		StateChangedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	d, err := engine.Decide(base, engine.Observation{Status: protocol.StatusPastDue}, now)
	if err != nil || d.Noop || d.EventType != protocol.EventPastDue || d.Next.StateVersion != 2 {
		t.Fatalf("past_due: %+v %v", d, err)
	}

	d, err = engine.Decide(d.Next, engine.Observation{Status: protocol.StatusPastDue}, now)
	if err != nil || !d.Noop {
		t.Fatalf("duplicate past_due should noop: %+v %v", d, err)
	}

	recovered, err := engine.Decide(store.Subscription{
		SubscriptionRef:    base.SubscriptionRef,
		CheckoutID:         base.CheckoutID,
		PlanID:             base.PlanID,
		Status:             protocol.StatusPastDue,
		StateVersion:       2,
		ProcessorFamily:    protocol.ProcessorStripe,
		CurrentPeriodStart: base.CurrentPeriodStart,
		CurrentPeriodEnd:   base.CurrentPeriodEnd,
		PastDueSince:       &now,
		StateChangedAt:     now,
	}, engine.Observation{
		Status:      protocol.StatusActive,
		PeriodStart: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}, now)
	if err != nil || recovered.EventType != protocol.EventRenewed || recovered.Next.PastDueSince != nil {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}

	canceled, err := engine.Decide(base, engine.Observation{Status: protocol.StatusCanceled, CancelAtPeriodEnd: true}, now)
	if err != nil || canceled.EventType != protocol.EventCanceled || !canceled.CreateExpire {
		t.Fatalf("cancel: %+v %v", canceled, err)
	}

	expired, err := engine.Decide(canceled.Next, engine.Observation{Status: protocol.StatusExpired}, now)
	if err != nil || expired.EventType != protocol.EventExpired || expired.Next.StateVersion != 3 {
		t.Fatalf("expire: %+v %v", expired, err)
	}

	late, err := engine.Decide(expired.Next, engine.Observation{Status: protocol.StatusActive}, now)
	if err != nil || !late.Noop {
		t.Fatalf("expired is terminal: %+v %v", late, err)
	}

	plan, err := engine.Decide(base, engine.Observation{Status: protocol.StatusActive, PlanID: "plan_1tb"}, now)
	if err != nil || plan.EventType != protocol.EventPlanChanged {
		t.Fatalf("plan change: %+v %v", plan, err)
	}

	restored, err := engine.Decide(canceled.Next, engine.Observation{Status: protocol.StatusActive}, now)
	if err != nil || restored.EventType != protocol.EventRenewed || restored.Next.CanceledAt != nil {
		t.Fatalf("restore canceled: %+v %v", restored, err)
	}
}

// TestMonotonicVersion checks that the first activation is version 1 and the next change is 2.
func TestMonotonicVersion(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := engine.FirstActivation(store.Checkout{
		CheckoutID:      "subchk_7f3a9c2e",
		PlanID:          "plan_500gb",
		ProcessorFamily: protocol.ProcessorStripe,
	}, "sub_a8f3c1d2", protocol.StatusActive, now, now.AddDate(0, 1, 0), now, nil, nil, nil, nil)
	if sub.StateVersion != 1 {
		t.Fatalf("version %d", sub.StateVersion)
	}
	d, err := engine.Decide(sub, engine.Observation{Status: protocol.StatusPastDue}, now.Add(time.Hour))
	if err != nil || d.Next.StateVersion != 2 {
		t.Fatalf("%+v %v", d, err)
	}
}

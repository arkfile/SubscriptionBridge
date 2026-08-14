package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/scheduler"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

type harness struct {
	clock time.Time
	eng   *engine.Engine
	mem   *store.Memory
	ad    *fake.Adapter
	sched *scheduler.Scheduler
	keys  hmac.Keys
}

// newHarness builds an Adyen engine, memory store, fake adapter, and scheduler on a shared clock.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{clock: time.Unix(1767225605, 0).UTC()}
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	h.keys = keys
	h.mem = store.NewMemory(func() time.Time { return h.clock })
	h.ad = fake.New(protocol.ProcessorAdyen)
	cfg := config.Config{
		PublicURL:               "https://billing.example.com",
		DefaultProcessor:        protocol.ProcessorAdyen,
		RenewalRetryDelays:      []time.Duration{24 * time.Hour, 72 * time.Hour, 120 * time.Hour},
		AdyenResolutionDeadline: 144 * time.Hour,
		Catalog: config.Catalog{DefaultProcessor: protocol.ProcessorAdyen, Plans: map[string]config.Plan{
			"plan_500gb": {Processor: protocol.ProcessorAdyen, Currency: "USD", AmountMinor: 500, Interval: "month", Adyen: config.AdyenPlan{MerchantAccount: "ExampleMerchant", CountryCode: "CH"}},
		}},
	}
	h.eng = &engine.Engine{
		Store:    h.mem,
		Config:   cfg,
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{protocol.ProcessorAdyen: h.ad},
		Clock:    func() time.Time { return h.clock },
	}
	h.sched = &scheduler.Scheduler{Store: h.mem, Engine: h.eng, Adyen: h.ad, Config: cfg, Clock: func() time.Time { return h.clock }}
	return h
}

// activate starts and activates an Adyen checkout, then advances the clock to the renewal due time.
func (h *harness) activate(t *testing.T) string {
	t.Helper()
	fx := testdata.Load(t)
	ctx := context.Background()
	if _, err := h.eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ActivateCheckout(ctx, "subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	var ref string
	_ = h.mem.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			t.Fatal(err)
		}
		ref = *c.SubscriptionRef
		return nil
	})
	h.clock = protocol.AddCalendarMonths(h.clock, 1)
	return ref
}

// sub loads the subscription under test.
func (h *harness) sub(t *testing.T, ref string) store.Subscription {
	t.Helper()
	var sub store.Subscription
	_ = h.mem.InTx(context.Background(), func(tx store.Tx) error {
		var err error
		sub, err = tx.GetSubscription(ref)
		return err
	})
	return sub
}

// attempts lists stored charge attempts.
func (h *harness) attempts(t *testing.T) []store.ChargeAttempt {
	t.Helper()
	var out []store.ChargeAttempt
	_ = h.mem.InTx(context.Background(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListAttempts(nil, 50)
		return err
	})
	return out
}

// TestUncertainExactReplay reuses the original idempotency key on exact replay.
func TestUncertainExactReplay(t *testing.T) {
	h := newHarness(t)
	ref := h.activate(t)
	h.ad.Uncertain = true
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := h.ad.LastCharge.IdempotencyKey
	if first == "" {
		t.Fatal("missing first idempotency key")
	}
	atts := h.attempts(t)
	if len(atts) != 1 || atts[0].Status != "uncertain" {
		t.Fatalf("attempt %+v", atts)
	}
	h.ad.Uncertain = false
	h.clock = h.clock.Add(time.Minute)
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.ad.LastCharge.IdempotencyKey != first {
		t.Fatal("replay must reuse idempotency key")
	}
	if h.sub(t, ref).Status != protocol.StatusActive {
		t.Fatalf("status %s", h.sub(t, ref).Status)
	}
	if h.sub(t, ref).StateVersion != 2 {
		t.Fatalf("version %d", h.sub(t, ref).StateVersion)
	}
}

// TestWebhookCorrelatesUncertainAttempt authorizes an uncertain attempt by attempt_reference.
func TestWebhookCorrelatesUncertainAttempt(t *testing.T) {
	h := newHarness(t)
	ref := h.activate(t)
	h.ad.Uncertain = true
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	att := h.attempts(t)[0]
	body := []byte(`{"kind":"adyen.renewal_authorisation","event_id":"evt_wh_1","attempt_reference":"` + att.AttemptReference + `","processor_payment_id":"psp_1","success":true,"status":"AUTHORISATION"}`)
	if err := h.eng.IngestAndProcess(context.Background(), protocol.ProcessorAdyen, nil, body); err != nil {
		t.Fatal(err)
	}
	if h.sub(t, ref).Status != protocol.StatusActive {
		t.Fatalf("status %s", h.sub(t, ref).Status)
	}
	updated := h.attempts(t)[0]
	if updated.Status != "authorized" {
		t.Fatalf("attempt %s", updated.Status)
	}
}

// TestResolutionDeadlineBlocksCharging moves an overdue uncertain attempt to manual_review.
func TestResolutionDeadlineBlocksCharging(t *testing.T) {
	h := newHarness(t)
	ref := h.activate(t)
	h.ad.Uncertain = true
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(145 * time.Hour)
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := h.sub(t, ref)
	if !sub.AutomaticChargingBlocked || sub.Status != protocol.StatusPastDue {
		t.Fatalf("block %+v", sub)
	}
	if h.attempts(t)[0].Status != "manual_review" {
		t.Fatalf("attempt %s", h.attempts(t)[0].Status)
	}
}

// TestStaleWorkerCannotFinishAction proves an expired worker cannot commit after a newer claim.
func TestStaleWorkerCannotFinishAction(t *testing.T) {
	h := newHarness(t)
	_ = h.activate(t)
	ctx := context.Background()
	var first store.ScheduledAction
	err := h.mem.InTx(ctx, func(tx store.Tx) error {
		var err error
		first, err = tx.ClaimDueAction(h.clock, time.Minute, "pending", protocol.ActionRenew)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(2 * time.Minute)
	var second store.ScheduledAction
	err = h.mem.InTx(ctx, func(tx store.Tx) error {
		var err error
		second, err = tx.ClaimAction(first.ActionID, h.clock, time.Minute)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatalf("fence %d %d", first.FencingToken, second.FencingToken)
	}
	err = h.mem.InTx(ctx, func(tx store.Tx) error {
		return tx.FinishAction(first.ActionID, *first.ClaimToken, first.FencingToken, "completed", nil, nil)
	})
	if !errors.Is(err, store.ErrNotOwned) {
		t.Fatalf("stale worker must not finish: %v", err)
	}
}

// TestRefusalCreatesPastDueAndReschedules keeps the same renewal action after a retryable refusal.
func TestRefusalCreatesPastDueAndReschedules(t *testing.T) {
	h := newHarness(t)
	ref := h.activate(t)
	h.ad.ChargeResult = &adapters.RenewalResult{Status: "refused", RefusalCode: "12"}
	if err := h.sched.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := h.sub(t, ref)
	if sub.Status != protocol.StatusPastDue || sub.StateVersion != 2 {
		t.Fatalf("sub %+v", sub)
	}
	var pending bool
	_ = h.mem.InTx(context.Background(), func(tx store.Tx) error {
		acts, err := tx.ListActions(20)
		for _, a := range acts {
			if a.SubscriptionRef == ref && a.ActionType == protocol.ActionRenew && a.Status == "pending" {
				pending = true
			}
		}
		return err
	})
	if !pending {
		t.Fatal("renewal action should be rescheduled pending")
	}
}

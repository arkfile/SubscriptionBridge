package adapters_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	adyenadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	stripeadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/stripe"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/scheduler"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestConformanceFakeAdapters runs the black-box suite against fake Stripe and Adyen adapters.
func TestConformanceFakeAdapters(t *testing.T) {
	for _, family := range []string{protocol.ProcessorStripe, protocol.ProcessorAdyen} {
		t.Run(family, func(t *testing.T) {
			runConformance(t, family)
		})
	}
}

// runConformance checks checkout, renew, past_due recovery, cancel, expire, and signature reject.
func runConformance(t *testing.T, family string) {
	t.Helper()
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return clock })
	ad := fake.New(family)
	cfg := config.Config{
		PublicURL:          "https://billing.example.com",
		DefaultProcessor:   family,
		RenewalRetryDelays: []time.Duration{24 * time.Hour, 72 * time.Hour, 120 * time.Hour},
		Catalog: config.Catalog{DefaultProcessor: family, Plans: map[string]config.Plan{
			"plan_500gb": {Processor: family, Currency: "USD", AmountMinor: 500, Interval: "month", Stripe: config.StripePlan{PriceID: "price_x"}, Adyen: config.AdyenPlan{MerchantAccount: "ExampleMerchant", CountryCode: "CH"}},
		}},
	}
	eng := &engine.Engine{
		Store:    mem,
		Config:   cfg,
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{family: ad},
		Clock:    func() time.Time { return clock },
	}
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	if err := eng.ActivateCheckout(ctx, "subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	if err := eng.ActivateCheckout(ctx, "subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	ref := ""
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			return err
		}
		ref = *c.SubscriptionRef
		return nil
	})
	snap, err := eng.Snapshot(ctx, ref)
	if err != nil || snap.Status != protocol.StatusActive || snap.StateVersion != 1 {
		t.Fatalf("activate %+v %v", snap, err)
	}

	if family == protocol.ProcessorStripe {
		runStripeLifecycle(t, eng, ad, ref, &clock)
	} else {
		runAdyenLifecycle(t, eng, ad, mem, cfg, ref, &clock)
	}

	if err := eng.CancelAtPeriodEnd(ctx, ref); err != nil {
		t.Fatal(err)
	}
	snap, _ = eng.Snapshot(ctx, ref)
	if snap.Status != protocol.StatusCanceled {
		t.Fatalf("cancel %s", snap.Status)
	}
	if err := eng.ExpireNow(ctx, ref); err != nil {
		t.Fatal(err)
	}
	snap, _ = eng.Snapshot(ctx, ref)
	if snap.Status != protocol.StatusExpired {
		t.Fatalf("expire %s", snap.Status)
	}
	rejectBadSignature(t, family)
}

// runStripeLifecycle renews, marks past_due, recovers, and ignores a duplicate event.
func runStripeLifecycle(t *testing.T, eng *engine.Engine, ad *fake.Adapter, ref string, clock *time.Time) {
	t.Helper()
	ctx := context.Background()
	periodStart := clock.AddDate(0, 1, 0)
	periodEnd := protocol.AddCalendarMonths(periodStart, 1)
	ad.SetSub("sub_conf_1", adapters.SubscriptionState{
		Status:             protocol.StatusActive,
		CurrentPeriodStart: periodStart,
		CurrentPeriodEnd:   periodEnd,
		ProcessorSubID:     "sub_conf_1",
	})
	body := []byte(`{"kind":"stripe.subscription_changed","event_id":"evt_conf_renew","checkout_id":"subchk_7f3a9c2e","processor_subscription_id":"sub_conf_1","success":true}`)
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorStripe, nil, body); err != nil {
		t.Fatal(err)
	}
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorStripe, nil, body); err != nil {
		t.Fatal(err)
	}
	snap, err := eng.Snapshot(ctx, ref)
	if err != nil || snap.Status != protocol.StatusActive || snap.StateVersion != 2 {
		t.Fatalf("renew %+v %v", snap, err)
	}
	ad.SetSub("sub_conf_1", adapters.SubscriptionState{
		Status:             protocol.StatusPastDue,
		CurrentPeriodStart: periodStart,
		CurrentPeriodEnd:   periodEnd,
		ProcessorSubID:     "sub_conf_1",
	})
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorStripe, nil, []byte(`{"kind":"stripe.subscription_changed","event_id":"evt_conf_due","checkout_id":"subchk_7f3a9c2e","processor_subscription_id":"sub_conf_1","success":true}`)); err != nil {
		t.Fatal(err)
	}
	snap, _ = eng.Snapshot(ctx, ref)
	if snap.Status != protocol.StatusPastDue {
		t.Fatalf("past_due %s", snap.Status)
	}
	ad.SetSub("sub_conf_1", adapters.SubscriptionState{
		Status:             protocol.StatusActive,
		CurrentPeriodStart: periodStart,
		CurrentPeriodEnd:   periodEnd,
		ProcessorSubID:     "sub_conf_1",
	})
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorStripe, nil, []byte(`{"kind":"stripe.subscription_changed","event_id":"evt_conf_ok","checkout_id":"subchk_7f3a9c2e","processor_subscription_id":"sub_conf_1","success":true}`)); err != nil {
		t.Fatal(err)
	}
	snap, _ = eng.Snapshot(ctx, ref)
	if snap.Status != protocol.StatusActive {
		t.Fatalf("recover %s", snap.Status)
	}
}

// runAdyenLifecycle refuses a renewal, then recovers on the rescheduled attempt.
func runAdyenLifecycle(t *testing.T, eng *engine.Engine, ad *fake.Adapter, mem *store.Memory, cfg config.Config, ref string, clock *time.Time) {
	t.Helper()
	ctx := context.Background()
	sched := &scheduler.Scheduler{Store: mem, Engine: eng, Adyen: ad, Config: cfg, Clock: func() time.Time { return *clock }}
	*clock = protocol.AddCalendarMonths(*clock, 1)
	ad.ChargeResult = &adapters.RenewalResult{Status: "refused", RefusalCode: "12"}
	if err := sched.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := eng.Snapshot(ctx, ref)
	if err != nil || snap.Status != protocol.StatusPastDue {
		t.Fatalf("past_due %+v %v", snap, err)
	}
	ad.ChargeResult = &adapters.RenewalResult{Status: "authorized", ProcessorPaymentID: "pay_ok"}
	*clock = clock.Add(24 * time.Hour)
	if err := sched.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap, _ = eng.Snapshot(ctx, ref)
	if snap.Status != protocol.StatusActive {
		t.Fatalf("recover %s", snap.Status)
	}
	var attemptRef string
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		atts, err := tx.ListAttempts([]string{"authorized"}, 10)
		if err != nil || len(atts) == 0 {
			return err
		}
		attemptRef = atts[0].AttemptReference
		return nil
	})
	if attemptRef == "" {
		t.Fatal("missing authorized attempt")
	}
	body := []byte(`{"kind":"adyen.renewal_authorisation","event_id":"evt_dup","attempt_reference":"` + attemptRef + `","processor_payment_id":"pay_ok","success":true}`)
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorAdyen, nil, body); err != nil {
		t.Fatal(err)
	}
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorAdyen, nil, body); err != nil {
		t.Fatal(err)
	}
	after, _ := eng.Snapshot(ctx, ref)
	if after.StateVersion != snap.StateVersion {
		t.Fatalf("duplicate webhook consumed a version: %d %d", snap.StateVersion, after.StateVersion)
	}
}

// rejectBadSignature checks that native adapters fail closed on unverified webhooks.
func rejectBadSignature(t *testing.T, family string) {
	t.Helper()
	ctx := context.Background()
	if family == protocol.ProcessorStripe {
		ad := stripeadapter.New("sk_test", "whsec_test")
		_, err := ad.ParseWebhook(ctx, http.Header{}, []byte(`{"id":"evt_x","type":"customer.subscription.updated"}`))
		if !errors.Is(err, protocol.ErrInvalidSignature) {
			t.Fatalf("stripe signature %v", err)
		}
		return
	}
	ad := adyenadapter.New("key", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff", "test", "")
	_, err := ad.ParseWebhook(ctx, nil, []byte(`{"notificationItems":[{"NotificationRequestItem":{"eventCode":"AUTHORISATION","success":"true","pspReference":"p","merchantAccountCode":"m","merchantReference":"r","amount":{"value":0,"currency":"USD"},"additionalData":{"hmacSignature":"AAAA"}}}]}`))
	if !errors.Is(err, protocol.ErrInvalidSignature) {
		t.Fatalf("adyen signature %v", err)
	}
}

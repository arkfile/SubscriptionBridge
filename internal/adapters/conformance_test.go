package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
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

// runConformance checks checkout, activation, and snapshot behavior for one family.
func runConformance(t *testing.T, family string) {
	t.Helper()
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return now })
	ad := fake.New(family)
	eng := &engine.Engine{
		Store: mem,
		Config: config.Config{
			PublicURL:        "https://billing.example.com",
			DefaultProcessor: family,
			Catalog: config.Catalog{DefaultProcessor: family, Plans: map[string]config.Plan{
				"plan_500gb": {Processor: family, Currency: "USD", AmountMinor: 500, Interval: "month", Stripe: config.StripePlan{PriceID: "price_x"}, Adyen: config.AdyenPlan{MerchantAccount: "ExampleMerchant", CountryCode: "CH"}},
			}},
		},
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{family: ad},
		Clock:    func() time.Time { return now },
	}
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
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
	if err != nil || snap.Status != protocol.StatusActive {
		t.Fatalf("activate %+v %v", snap, err)
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
}

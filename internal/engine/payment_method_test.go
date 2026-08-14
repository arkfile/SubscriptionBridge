package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/envelope"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestAdyenPaymentMethodReplacement re-encrypts a new token and clears the charging block.
func TestAdyenPaymentMethodReplacement(t *testing.T) {
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	box, err := envelope.New(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return now })
	ad := fake.New(protocol.ProcessorAdyen)
	eng := &engine.Engine{
		Store: mem,
		Config: config.Config{
			PublicURL:        "https://billing.example.com",
			DefaultProcessor: protocol.ProcessorAdyen,
			Catalog: config.Catalog{DefaultProcessor: protocol.ProcessorAdyen, Plans: map[string]config.Plan{
				"plan_500gb": {Processor: protocol.ProcessorAdyen, Currency: "USD", AmountMinor: 500, Interval: "month", Adyen: config.AdyenPlan{MerchantAccount: "ExampleMerchant", CountryCode: "CH"}},
			}},
		},
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{protocol.ProcessorAdyen: ad},
		Box:      box,
		Clock:    func() time.Time { return now },
	}
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"kind":"adyen.initial_authorisation","event_id":"psp_1:AUTHORISATION:true:","checkout_id":"subchk_7f3a9c2e","processor_payment_id":"psp_1","success":true,"status":"AUTHORISATION","token":"tok_initial"}`)
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorAdyen, nil, first); err != nil {
		t.Fatal(err)
	}
	var ref string
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			return err
		}
		ref = *c.SubscriptionRef
		sub, err := tx.GetSubscriptionForUpdate(ref)
		if err != nil {
			return err
		}
		plain, err := box.Open(sub.PaymentMethodCiphertext, sub.PaymentMethodNonce, envelope.AAD("payment-method", ref), *sub.PaymentMethodKeyVersion)
		if err != nil {
			t.Fatal(err)
		}
		if string(plain) != "tok_initial" {
			t.Fatalf("initial token %q", plain)
		}
		sub.AutomaticChargingBlocked = true
		reason := "manual_review"
		sub.ChargingBlockReason = &reason
		return tx.UpdateSubscription(sub)
	})
	second := []byte(`{"kind":"adyen.initial_authorisation","event_id":"psp_2:AUTHORISATION:true:","checkout_id":"subchk_7f3a9c2e","processor_payment_id":"psp_2","success":true,"status":"AUTHORISATION","token":"tok_replaced"}`)
	if err := eng.IngestAndProcess(ctx, protocol.ProcessorAdyen, nil, second); err != nil {
		t.Fatal(err)
	}
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		sub, err := tx.GetSubscription(ref)
		if err != nil {
			return err
		}
		if sub.StateVersion != 1 {
			t.Fatalf("replacement must not consume state_version: %d", sub.StateVersion)
		}
		if sub.AutomaticChargingBlocked {
			t.Fatal("charging block must clear")
		}
		plain, err := box.Open(sub.PaymentMethodCiphertext, sub.PaymentMethodNonce, envelope.AAD("payment-method", ref), *sub.PaymentMethodKeyVersion)
		if err != nil {
			t.Fatal(err)
		}
		if string(plain) != "tok_replaced" {
			t.Fatalf("replaced token %q", plain)
		}
		return nil
	})
}

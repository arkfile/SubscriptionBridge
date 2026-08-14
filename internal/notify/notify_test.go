package notify_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/notify"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestDeadLetterOnOther4xx dead-letters outbound events after a non-retryable 4xx.
func TestDeadLetterOnOther4xx(t *testing.T) {
	fx := testdata.Load(t)
	keys, _ := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	now := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return now })
	_ = mem.InTx(context.Background(), func(tx store.Tx) error {
		shopper := "sbr_test"
		_ = tx.InsertCheckout(store.Checkout{
			CheckoutID: "subchk_7f3a9c2e", PlanID: "plan_500gb", NormalizedReturnURL: "https://app.example.com/",
			ProcessorFamily: protocol.ProcessorAdyen, RequestFingerprint: bytes32(), ProviderIdempotencyKey: "k",
			ProcessorShopperReference: &shopper, Status: protocol.CheckoutCompleted, ExpiresAt: now.Add(time.Hour),
		})
		_ = tx.InsertSubscription(store.Subscription{
			SubscriptionRef: "sub_a8f3c1d2", CheckoutID: "subchk_7f3a9c2e", PlanID: "plan_500gb",
			Status: protocol.StatusActive, StateVersion: 1, ProcessorFamily: protocol.ProcessorAdyen,
			ProcessorShopperReference: &shopper,
			CurrentPeriodStart: now, CurrentPeriodEnd: now.AddDate(0, 1, 0), StateChangedAt: now,
		})
		return tx.InsertOutbound(store.OutboundEvent{
			EventID: "evt_550e8400-e29b-41d4-a716-446655440000", EventType: protocol.EventActivated,
			SubscriptionRef: "sub_a8f3c1d2", CheckoutID: "subchk_7f3a9c2e", StateVersion: 1,
			PayloadBody: []byte(fx.Callback.BodyJSONUTF8), DeliveryState: protocol.DeliveryPending, NextAttemptAt: &now,
		})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	n := &notify.Notifier{Store: mem, Keys: keys, WebhookURL: srv.URL, Clock: func() time.Time { return now }}
	if err := n.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = mem.InTx(context.Background(), func(tx store.Tx) error {
		ev, err := tx.GetOutbound("evt_550e8400-e29b-41d4-a716-446655440000")
		if err != nil {
			t.Fatal(err)
		}
		if ev.DeliveryState != protocol.DeliveryDeadLettered {
			t.Fatalf("state %s", ev.DeliveryState)
		}
		return nil
	})
}

// bytes32 returns a 32-byte test key.
func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

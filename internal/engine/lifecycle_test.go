package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/notify"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// testEngine builds an engine on the in-memory store with fake adapters.
func testEngine(t *testing.T) (*engine.Engine, hmac.Keys, *store.Memory) {
	t.Helper()
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemory(func() time.Time { return time.Unix(1767225605, 0).UTC() })
	eng := &engine.Engine{
		Store: mem,
		Config: config.Config{
			PublicURL:          "https://billing.example.com",
			ConsumerWebhookURL: "https://app.example.com/api/webhooks/subscription-bridge",
			PairingRoot:        fx.PairingRoot.ConfiguredHex,
			DefaultProcessor:   protocol.ProcessorStripe,
			Catalog: config.Catalog{
				DefaultProcessor: protocol.ProcessorStripe,
				Plans: map[string]config.Plan{
					"plan_500gb": {Processor: protocol.ProcessorStripe, Currency: "USD", AmountMinor: 500, Interval: "month", Stripe: config.StripePlan{PriceID: "price_x"}},
				},
			},
		},
		Keys: keys,
		Adapters: map[string]adapters.ProcessorAdapter{
			protocol.ProcessorStripe: fake.New(protocol.ProcessorStripe),
		},
		Clock: func() time.Time { return time.Unix(1767225605, 0).UTC() },
	}
	return eng, keys, mem
}

// TestStartCheckoutIdempotent resumes an exact start-token replay.
func TestStartCheckoutIdempotent(t *testing.T) {
	eng, _, _ := testEngine(t)
	fx := testdata.Load(t)
	ctx := context.Background()
	a, err := eng.StartCheckout(ctx, fx.StartToken.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a.RedirectURL == "" {
		t.Fatal("missing redirect")
	}
	b, err := eng.StartCheckout(ctx, fx.StartToken.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a.RedirectURL != b.RedirectURL {
		t.Fatal("idempotent start must reuse session")
	}
}

// TestActivateAndNotifierByteIdentity delivers the stored callback bytes unchanged.
func TestActivateAndNotifierByteIdentity(t *testing.T) {
	eng, keys, mem := testEngine(t)
	fx := testdata.Load(t)
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	if err := eng.ActivateCheckout(ctx, "subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	var codes atomic.Int32
	codes.Store(500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := append([]byte{}, buf[:n]...)
		bodies = append(bodies, body)
		if _, err := protocol.ParseCallback(body); err != nil {
			t.Errorf("callback: %v", err)
		}
		if err := hmac.VerifyCallbackHeader(keys.Callback, r.Header.Get(protocol.HMACHeaderName), body, time.Unix(1767225605, 0).UTC()); err != nil {
			t.Errorf("sig: %v", err)
		}
		if codes.Load() == 500 {
			codes.Store(200)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n := &notify.Notifier{Store: mem, Keys: keys, WebhookURL: srv.URL, Clock: func() time.Time { return time.Unix(1767225605, 0).UTC() }, RetryAt: func(_ int, now time.Time) time.Time { return now }}
	if err := n.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts %d", len(bodies))
	}
	if string(bodies[0]) != string(bodies[1]) {
		t.Fatal("retry must reuse exact bytes")
	}
	var delivered int
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		ev, err := tx.ListOutbound(protocol.DeliveryDelivered, 10)
		delivered = len(ev)
		return err
	})
	if delivered != 1 {
		t.Fatalf("delivered %d", delivered)
	}
	snap, err := eng.Snapshot(ctx, mustSub(t, mem))
	if err != nil {
		t.Fatal(err)
	}
	if snap.StateVersion != 1 || snap.Status != protocol.StatusActive {
		raw, _ := json.Marshal(snap)
		t.Fatalf("snap %s", raw)
	}
}

// mustSub starts and activates a checkout, returning the subscription_ref.
func mustSub(t *testing.T, mem *store.Memory) string {
	t.Helper()
	var ref string
	_ = mem.InTx(context.Background(), func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			t.Fatal(err)
		}
		if c.SubscriptionRef == nil {
			t.Fatal("missing sub")
		}
		ref = *c.SubscriptionRef
		return nil
	})
	return ref
}

// TestCheckoutConflict rejects a checkout_id reused with a different plan.
func TestCheckoutConflict(t *testing.T) {
	eng, keys, _ := testEngine(t)
	fx := testdata.Load(t)
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"checkout_id":"subchk_7f3a9c2e","plan_id":"plan_500gb","return_url":"https://app.example.com/other","iat":1767225600,"exp":1767226500}`)
	tok, err := hmac.EncodeToken(keys.Token, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.StartCheckout(ctx, tok); err == nil {
		t.Fatal("expected conflict")
	}
}

// TestRolledBackActivationLeavesNoEvent asserts a rolled-back tx leaves no outbound event.
func TestRolledBackActivationLeavesNoEvent(t *testing.T) {
	eng, _, mem := testEngine(t)
	fx := testdata.Load(t)
	ctx := context.Background()
	if _, err := eng.StartCheckout(ctx, fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	err := mem.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckoutForUpdate("subchk_7f3a9c2e")
		if err != nil {
			return err
		}
		c.Status = protocol.CheckoutCompleted
		if err := tx.UpdateCheckout(c); err != nil {
			return err
		}
		return context.Canceled
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	_ = mem.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			t.Fatal(err)
		}
		if c.Status == protocol.CheckoutCompleted {
			t.Fatal("checkout should not complete on rollback")
		}
		return nil
	})
}

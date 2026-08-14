package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/httpapi"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// signPortal mints a portal token for the given subscription_ref.
func signPortal(t *testing.T, key []byte, ref string, now time.Time) string {
	t.Helper()
	payload := fmt.Sprintf(`{"subscription_ref":%q,"return_url":"https://app.example.com/billing","iat":%d,"exp":%d}`, ref, now.Unix(), now.Unix()+900)
	tok, err := hmac.EncodeToken(key, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestAdyenPortalDropinChrome serves Drop-in mount points without leaking identifiers.
func TestAdyenPortalDropinChrome(t *testing.T) {
	fx := testdata.Load(t)
	keys, _ := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	now := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return now })
	ad := fake.New(protocol.ProcessorAdyen)
	eng := &engine.Engine{
		Store: mem,
		Config: config.Config{
			PublicURL:        "https://billing.example.com",
			DefaultProcessor: protocol.ProcessorAdyen,
			AdyenClientKey:   "test_client_key",
			AdyenEnvironment: "test",
			Catalog: config.Catalog{DefaultProcessor: protocol.ProcessorAdyen, Plans: map[string]config.Plan{
				"plan_500gb": {Processor: protocol.ProcessorAdyen, Currency: "USD", AmountMinor: 500, Interval: "month", Adyen: config.AdyenPlan{MerchantAccount: "ExampleMerchant", CountryCode: "CH"}},
			}},
		},
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{protocol.ProcessorAdyen: ad},
		Clock:    func() time.Time { return now },
	}
	if _, err := eng.StartCheckout(context.Background(), fx.StartToken.Token); err != nil {
		t.Fatal(err)
	}
	if err := eng.ActivateCheckout(context.Background(), "subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	var ref, shopper string
	_ = mem.InTx(context.Background(), func(tx store.Tx) error {
		c, err := tx.GetCheckout("subchk_7f3a9c2e")
		if err != nil {
			return err
		}
		ref = *c.SubscriptionRef
		sub, err := tx.GetSubscription(ref)
		if err != nil {
			return err
		}
		if sub.ProcessorShopperReference != nil {
			shopper = *sub.ProcessorShopperReference
		}
		return nil
	})
	srv := &httpapi.Server{Engine: eng, Store: mem, Keys: keys, Clock: func() time.Time { return now }}
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/portal/assets/portal.css", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "--depth-1") {
		t.Fatalf("css %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/portal?token="+signPortal(t, keys.Token, ref, now), nil))
	if w.Code != 200 {
		t.Fatalf("portal %d", w.Code)
	}
	body := w.Body.String()
	for _, needle := range []string{`id="dropin"`, `class="btn-secondary"`, `class="danger-button"`, `Update payment method`, `adyen.js`, `test_client_key`, `CS_fake`, `session_fake`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("missing %q", needle)
		}
	}
	if strings.Contains(body, ref) {
		t.Fatal("subscription_ref must not appear in portal HTML")
	}
	if shopper != "" && strings.Contains(body, shopper) {
		t.Fatal("shopper reference must not appear in portal HTML")
	}
	if !strings.HasPrefix(ad.PaymentUpdate().ShopperRef, protocol.ShopperPrefix) {
		t.Fatalf("session must reuse shopper, got %q", ad.PaymentUpdate().ShopperRef)
	}
	if ad.PaymentUpdate().AmountMinor != 0 {
		t.Fatalf("replacement amount %d", ad.PaymentUpdate().AmountMinor)
	}
	if ad.PaymentUpdate().ShopperRef != shopper {
		t.Fatalf("shopper mismatch %q %q", ad.PaymentUpdate().ShopperRef, shopper)
	}
	if !strings.HasPrefix(ad.PaymentUpdate().IdempotencyKey, protocol.PaymentUpdatePrefix) {
		t.Fatalf("merchant ref %q", ad.PaymentUpdate().IdempotencyKey)
	}
}

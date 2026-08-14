package httpapi_test

import (
	"net/http"
	"net/http/httptest"
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

// TestHealthAndStart checks /health and a signed /v1/start redirect.
func TestHealthAndStart(t *testing.T) {
	fx := testdata.Load(t)
	keys, _ := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	now := time.Unix(1767225605, 0).UTC()
	mem := store.NewMemory(func() time.Time { return now })
	eng := &engine.Engine{
		Store: mem,
		Config: config.Config{
			PublicURL:        "https://billing.example.com",
			DefaultProcessor: protocol.ProcessorStripe,
			Catalog: config.Catalog{DefaultProcessor: protocol.ProcessorStripe, Plans: map[string]config.Plan{
				"plan_500gb": {Processor: protocol.ProcessorStripe, Currency: "USD", AmountMinor: 500, Interval: "month", Stripe: config.StripePlan{PriceID: "price_x"}},
			}},
		},
		Keys:     keys,
		Adapters: map[string]adapters.ProcessorAdapter{protocol.ProcessorStripe: fake.New(protocol.ProcessorStripe)},
		Clock:    func() time.Time { return now },
	}
	srv := &httpapi.Server{Engine: eng, Store: mem, Keys: keys, Clock: func() time.Time { return now }, Health: func() error { return nil }}
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("health %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/start?token="+fx.StartToken.Token, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("start %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc == "" {
		t.Fatal("missing redirect")
	}
}

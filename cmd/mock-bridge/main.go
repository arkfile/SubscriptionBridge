package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/fake"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/httpapi"
	"github.com/arkfile/SubscriptionBridge/internal/notify"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

// main runs an in-memory mock bridge with fake processors for local consumer tests.
func main() {
	root := os.Getenv("BRIDGE_CONSUMER_PAIRING_ROOT")
	if root == "" {
		root = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	}
	keys, err := hmac.DeriveKeys(root)
	if err != nil {
		os.Exit(1)
	}
	mem := store.NewMemory(nil)
	cfg := config.Config{
		PublicURL:          "http://127.0.0.1:8081",
		ConsumerWebhookURL: getenv("CONSUMER_WEBHOOK_URL", "http://127.0.0.1:9/webhook"),
		PairingRoot:        root,
		DefaultProcessor:   protocol.ProcessorStripe,
		Catalog: config.Catalog{
			DefaultProcessor: protocol.ProcessorStripe,
			Plans: map[string]config.Plan{
				"plan_500gb": {Processor: protocol.ProcessorStripe, Currency: "USD", AmountMinor: 500, Interval: "month", Stripe: config.StripePlan{PriceID: "price_fake"}},
			},
		},
	}
	eng := &engine.Engine{
		Store:  mem,
		Config: cfg,
		Keys:   keys,
		Adapters: map[string]adapters.ProcessorAdapter{
			protocol.ProcessorStripe: fake.New(protocol.ProcessorStripe),
			protocol.ProcessorAdyen:  fake.New(protocol.ProcessorAdyen),
		},
		Log: slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}
	api := &httpapi.Server{Engine: eng, Store: mem, Keys: keys, Health: func() error { return nil }, EnableMock: true}
	n := &notify.Notifier{Store: mem, Keys: keys, WebhookURL: cfg.ConsumerWebhookURL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Loop(ctx, time.Second)
	addr := getenv("BRIDGE_LISTEN", "127.0.0.1:8081")
	srv := &http.Server{Addr: addr, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		os.Exit(1)
	}
}

// getenv returns an environment value or fallback.
func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

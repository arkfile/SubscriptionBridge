package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	adyenadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
	stripeadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/stripe"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/envelope"
	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/httpapi"
	"github.com/arkfile/SubscriptionBridge/internal/notify"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/scheduler"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// main loads config, checks schema, and runs HTTP, notifier, and scheduler loops.
func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", "invalid")
		os.Exit(1)
	}
	if err := cfg.LoadCatalog(); err != nil {
		log.Error("plans", "err", "invalid")
		os.Exit(1)
	}
	keys, err := hmac.DeriveKeys(cfg.PairingRoot)
	if err != nil {
		log.Error("pairing_root", "err", "invalid")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", "unavailable")
		os.Exit(1)
	}
	defer pool.Close()
	if err := store.CheckSchema(ctx, pool); err != nil {
		log.Error("schema", "err", "mismatch")
		os.Exit(1)
	}
	db := store.NewPostgres(pool, nil)
	ads := map[string]adapters.ProcessorAdapter{}
	if cfg.StripeSecretKey != "" {
		ads[protocol.ProcessorStripe] = stripeadapter.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret)
	}
	if cfg.AdyenAPIKey != "" {
		ads[protocol.ProcessorAdyen] = adyenadapter.New(cfg.AdyenAPIKey, cfg.AdyenHMACKey, cfg.AdyenEnvironment, cfg.AdyenLivePrefix)
	}
	var box *envelope.Box
	if cfg.AdyenDataEncryptionKey != "" {
		box, err = envelope.New(cfg.AdyenDataEncryptionKey)
		if err != nil {
			log.Error("encryption_key", "err", "invalid")
			os.Exit(1)
		}
	}
	eng := &engine.Engine{Store: db, Config: cfg, Keys: keys, Adapters: ads, Box: box, Log: log}
	api := &httpapi.Server{
		Engine: eng,
		Store:  db,
		Keys:   keys,
		Log:    log,
		Health: func() error { return db.Ping(context.Background()) },
	}
	n := &notify.Notifier{Store: db, Keys: keys, WebhookURL: cfg.ConsumerWebhookURL, Log: log}
	sched := &scheduler.Scheduler{Store: db, Engine: eng, Adyen: ads[protocol.ProcessorAdyen], Box: box, Config: cfg, Log: log}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go n.Loop(ctx, 2*time.Second)
	if cfg.SchedulerEnabled {
		go sched.Loop(ctx, 5*time.Second)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			_ = eng.SweepCheckouts(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listen", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http", "err", "failed")
		os.Exit(1)
	}
}

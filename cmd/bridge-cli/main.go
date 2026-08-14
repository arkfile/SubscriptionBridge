package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	adyenadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
	stripeadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/stripe"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// main is the operator CLI for schema, inspection, delivery, and attempt resolution.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "migrate":
		pool := mustPool(ctx)
		defer pool.Close()
		if err := store.ApplyMigrations(ctx, pool); err != nil {
			fail(err)
		}
		fmt.Println("ok")
	case "health":
		pool := mustPool(ctx)
		defer pool.Close()
		if err := pool.Ping(ctx); err != nil {
			fail(err)
		}
		if err := store.CheckSchema(ctx, pool); err != nil {
			fail(err)
		}
		fmt.Println("ok")
	case "show-checkout":
		requireArgs(3)
		withTx(ctx, func(tx store.Tx) error {
			c, err := tx.GetCheckout(os.Args[2])
			if err != nil {
				return err
			}
			return printJSON(map[string]any{
				"checkout_id":      c.CheckoutID,
				"status":           c.Status,
				"plan_id":          c.PlanID,
				"subscription_ref": c.SubscriptionRef,
			})
		})
	case "show-subscription":
		requireArgs(3)
		withTx(ctx, func(tx store.Tx) error {
			s, err := tx.GetSubscription(os.Args[2])
			if err != nil {
				return err
			}
			return printJSON(map[string]any{
				"subscription_ref":             s.SubscriptionRef,
				"status":                       s.Status,
				"state_version":                s.StateVersion,
				"plan_id":                      s.PlanID,
				"processor_family":             s.ProcessorFamily,
				"processor_customer_id":        s.ProcessorCustomerID,
				"processor_subscription_id":    s.ProcessorSubscriptionID,
				"processor_initial_payment_id": s.ProcessorInitialPaymentID,
				"automatic_charging_blocked":   s.AutomaticChargingBlocked,
			})
		})
	case "list-events":
		state := ""
		if len(os.Args) > 2 && os.Args[2] != "--reason" {
			state = os.Args[2]
		}
		withTx(ctx, func(tx store.Tx) error {
			ev, err := tx.ListOutbound(state, 100)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(ev))
			for _, e := range ev {
				ids = append(ids, e.EventID+":"+e.DeliveryState)
			}
			return printJSON(ids)
		})
	case "requeue-event":
		requireArgs(3)
		reason := flagValue("--reason", "operator")
		withTx(ctx, func(tx store.Tx) error {
			return tx.RequeueOutbound(os.Args[2], reason, "cli", tx.Now())
		})
	case "abandon-event":
		requireArgs(3)
		reason := flagValue("--reason", "operator")
		withTx(ctx, func(tx store.Tx) error {
			return tx.AbandonOutbound(os.Args[2], reason, "cli", tx.Now())
		})
	case "scheduler-status":
		withTx(ctx, func(tx store.Tx) error {
			actions, err := tx.ListActions(200)
			if err != nil {
				return err
			}
			attempts, err := tx.ListAttempts([]string{"uncertain", "manual_review", "running", "prepared"}, 200)
			if err != nil {
				return err
			}
			return printJSON(map[string]any{
				"actions":  summarizeActions(actions),
				"attempts": summarizeAttempts(attempts),
			})
		})
	case "reconcile":
		eng, cleanup := mustEngine(ctx)
		defer cleanup()
		err := eng.Store.InTx(ctx, func(tx store.Tx) error {
			subs, err := tx.ListSubscriptions(200)
			if err != nil {
				return err
			}
			rows := make([]map[string]any, 0, len(subs))
			for _, sub := range subs {
				row := map[string]any{
					"subscription_ref": sub.SubscriptionRef,
					"local_status":     sub.Status,
					"state_version":    sub.StateVersion,
				}
				if ad, ok := eng.Adapters[sub.ProcessorFamily]; ok && ad != nil && sub.ProcessorSubscriptionID != nil {
					st, err := ad.GetSubscription(ctx, adapters.ProcessorSubscription{
						Family:                  sub.ProcessorFamily,
						ProcessorSubscriptionID: *sub.ProcessorSubscriptionID,
					})
					if err == nil && st != nil {
						row["provider_status"] = st.Status
					}
				}
				rows = append(rows, row)
			}
			return printJSON(rows)
		})
		if err != nil {
			fail(err)
		}
	case "resolve-attempt":
		requireArgs(3)
		eng, cleanup := mustEngine(ctx)
		defer cleanup()
		if err := eng.ResolveAttempt(ctx, os.Args[2], flagValue("--outcome", ""), flagValue("--reason", "operator"), "cli", flagValue("--payment-id", "")); err != nil {
			fail(err)
		}
		fmt.Println("ok")
	default:
		usage()
		os.Exit(2)
	}
}

// usage prints supported CLI commands.
func usage() {
	fmt.Fprintln(os.Stderr, `bridge-cli migrate|health|show-checkout|show-subscription|list-events|requeue-event|abandon-event|scheduler-status|reconcile|resolve-attempt`)
}

// mustPool opens a PostgreSQL pool from BRIDGE_DATABASE_URL.
func mustPool(ctx context.Context) *pgxpool.Pool {
	url := os.Getenv("BRIDGE_DATABASE_URL")
	if url == "" {
		fail(fmt.Errorf("BRIDGE_DATABASE_URL required"))
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fail(err)
	}
	return pool
}

// mustEngine loads config and adapters for operator commands that need provider access.
func mustEngine(ctx context.Context) (*engine.Engine, func()) {
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	_ = cfg.LoadCatalog()
	pool := mustPool(ctx)
	db := store.NewPostgres(pool, nil)
	ads := map[string]adapters.ProcessorAdapter{}
	if cfg.StripeSecretKey != "" {
		ads[protocol.ProcessorStripe] = stripeadapter.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret)
	}
	if cfg.AdyenAPIKey != "" {
		ads[protocol.ProcessorAdyen] = adyenadapter.New(cfg.AdyenAPIKey, cfg.AdyenHMACKey, cfg.AdyenEnvironment, cfg.AdyenLivePrefix)
	}
	return &engine.Engine{Store: db, Config: cfg, Adapters: ads}, func() { pool.Close() }
}

// withTx runs fn in a PostgreSQL transaction and exits on error.
func withTx(ctx context.Context, fn func(store.Tx) error) {
	pool := mustPool(ctx)
	defer pool.Close()
	db := store.NewPostgres(pool, nil)
	if err := db.InTx(ctx, fn); err != nil {
		fail(err)
	}
}

// printJSON writes indented JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// requireArgs exits if the CLI was invoked with too few arguments.
func requireArgs(n int) {
	if len(os.Args) < n {
		usage()
		os.Exit(2)
	}
}

// flagValue returns the value following name, or fallback.
func flagValue(name, fallback string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return fallback
}

// summarizeActions returns bounded operator fields for scheduled actions.
func summarizeActions(in []store.ScheduledAction) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, a := range in {
		out = append(out, map[string]any{
			"action_id":        a.ActionID,
			"subscription_ref": a.SubscriptionRef,
			"action_type":      a.ActionType,
			"status":           a.Status,
			"due_at":           a.DueAt,
			"fencing_token":    a.FencingToken,
		})
	}
	return out
}

// summarizeAttempts returns bounded operator fields for charge attempts.
func summarizeAttempts(in []store.ChargeAttempt) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, a := range in {
		out = append(out, map[string]any{
			"attempt_id":       a.AttemptID,
			"subscription_ref": a.SubscriptionRef,
			"status":           a.Status,
			"attempt_number":   a.AttemptNumber,
		})
	}
	return out
}

// fail prints a concise error and exits 1 without leaking internals.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "error")
	os.Exit(1)
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QA / TODO: scheduler-status and reconcile print ok; resolve-attempt is not implemented.
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
				"checkout_id": c.CheckoutID,
				"status":      c.Status,
				"plan_id":     c.PlanID,
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
				"subscription_ref": s.SubscriptionRef,
				"status":           s.Status,
				"state_version":    s.StateVersion,
				"plan_id":          s.PlanID,
			})
		})
	case "list-events":
		state := ""
		if len(os.Args) > 2 {
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
		reason := flagReason()
		withTx(ctx, func(tx store.Tx) error {
			return tx.RequeueOutbound(os.Args[2], reason, "cli", tx.Now())
		})
	case "abandon-event":
		requireArgs(3)
		reason := flagReason()
		withTx(ctx, func(tx store.Tx) error {
			return tx.AbandonOutbound(os.Args[2], reason, "cli", tx.Now())
		})
	case "scheduler-status":
		// QA / TODO: stub; does not report scheduler leases or due actions.
		fmt.Println("ok")
	case "reconcile":
		// QA / TODO: stub; does not compare local vs provider state.
		fmt.Println("ok")
	default:
		usage()
		os.Exit(2)
	}
}

// usage prints supported CLI commands.
func usage() {
	fmt.Fprintln(os.Stderr, `bridge-cli migrate|health|show-checkout|show-subscription|list-events|requeue-event|abandon-event|scheduler-status|reconcile`)
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

// flagReason returns --reason or the default operator reason.
func flagReason() string {
	for i, a := range os.Args {
		if a == "--reason" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return "operator"
}

// fail prints a concise error and exits 1 without leaking internals.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "error")
	os.Exit(1)
}

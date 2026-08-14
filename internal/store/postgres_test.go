package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// openTestPostgres migrates and truncates bridge tables so repeated CI runs cannot collide.
func openTestPostgres(t *testing.T) (*pgxpool.Pool, *store.Postgres) {
	t.Helper()
	url := os.Getenv("BRIDGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BRIDGE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `TRUNCATE
		sb_charge_attempts,
		sb_scheduled_actions,
		sb_provider_event_quarantine,
		sb_processor_events,
		sb_processing_leases,
		sb_outbound_events,
		sb_operator_audit,
		sb_subscriptions,
		sb_checkouts
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	return pool, store.NewPostgres(pool, func() time.Time { return now })
}

// mustID allocates a prefixed opaque identifier.
func mustID(t *testing.T, prefix string) string {
	t.Helper()
	id, err := protocol.NewOpaqueID(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustUUID allocates a UUID-shaped primary key.
func mustUUID(t *testing.T) string {
	t.Helper()
	b, err := protocol.RandomClaimToken()
	if err != nil {
		t.Fatal(err)
	}
	return protocol.UUIDString(b)
}

// TestPostgresAttemptReferenceLookup exercises unique attempt_reference on PostgreSQL.
func TestPostgresAttemptReferenceLookup(t *testing.T) {
	_, db := openTestPostgres(t)
	ctx := context.Background()
	checkoutID := mustID(t, protocol.CheckoutPrefix)
	subRef := mustID(t, protocol.SubscriptionPrefix)
	shopper := mustID(t, protocol.ShopperPrefix)
	attemptRef := mustID(t, protocol.AttemptPrefix)
	actionID := mustUUID(t)
	attemptID := mustUUID(t)
	err := db.InTx(ctx, func(tx store.Tx) error {
		now := tx.Now()
		co := store.Checkout{
			CheckoutID:                checkoutID,
			PlanID:                    "plan_500gb",
			NormalizedReturnURL:       "https://app.example.com/",
			ProcessorFamily:           protocol.ProcessorAdyen,
			RequestFingerprint:        make([]byte, 32),
			ProviderIdempotencyKey:    "sb_checkout_" + checkoutID,
			ProcessorShopperReference: store.StrPtr(shopper),
			Status:                    protocol.CheckoutPending,
			ExpiresAt:                 now.Add(time.Hour),
		}
		if err := tx.InsertCheckout(co); err != nil {
			return err
		}
		sub := store.Subscription{
			SubscriptionRef:           subRef,
			CheckoutID:                co.CheckoutID,
			PlanID:                    "plan_500gb",
			Status:                    protocol.StatusActive,
			StateVersion:              1,
			ProcessorFamily:           protocol.ProcessorAdyen,
			ProcessorShopperReference: co.ProcessorShopperReference,
			CurrentPeriodStart:        now,
			CurrentPeriodEnd:          protocol.AddCalendarMonths(now, 1),
			StateChangedAt:            now,
		}
		if err := tx.InsertSubscription(sub); err != nil {
			return err
		}
		act := store.ScheduledAction{
			ActionID:        actionID,
			ActionKey:       protocol.ActionKey(sub.SubscriptionRef, protocol.ActionRenew, sub.CurrentPeriodEnd),
			SubscriptionRef: sub.SubscriptionRef,
			ActionType:      protocol.ActionRenew,
			TargetAt:        sub.CurrentPeriodEnd,
			DueAt:           now,
			Status:          "pending",
		}
		if err := tx.InsertAction(act); err != nil {
			return err
		}
		att := store.ChargeAttempt{
			AttemptID:                attemptID,
			ActionID:                 act.ActionID,
			SubscriptionRef:          sub.SubscriptionRef,
			PeriodStart:              sub.CurrentPeriodEnd,
			PeriodEnd:                protocol.AddCalendarMonths(sub.CurrentPeriodEnd, 1),
			AttemptNumber:            1,
			ProviderEndpoint:         "https://example/payments",
			ProviderAPIVersion:       "v71",
			MerchantAccount:          "ExampleMerchant",
			AmountMinor:              500,
			Currency:                 "USD",
			AttemptReference:         attemptRef,
			ShopperReference:         shopper,
			ShopperInteraction:       "ContAuth",
			RecurringProcessingModel: "Subscription",
			IdempotencyKey:           "sb_charge_" + attemptRef,
			RequestFingerprint:       make([]byte, 32),
			RequestCiphertext:        []byte(`{}`),
			RequestNonce:             make([]byte, 12),
			RequestKeyVersion:        "v1",
			Status:                   "prepared",
		}
		if err := tx.InsertAttempt(att); err != nil {
			return err
		}
		got, err := tx.GetAttemptByReference(attemptRef)
		if err != nil {
			return err
		}
		if got.AttemptID != att.AttemptID {
			t.Fatalf("got %s", got.AttemptID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostgresSkipLockedClaimsOnce proves two workers cannot claim the same due action.
func TestPostgresSkipLockedClaimsOnce(t *testing.T) {
	_, db := openTestPostgres(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	checkoutID := mustID(t, protocol.CheckoutPrefix)
	subRef := mustID(t, protocol.SubscriptionPrefix)
	shopper := mustID(t, protocol.ShopperPrefix)
	actionID := mustUUID(t)
	err := db.InTx(ctx, func(tx store.Tx) error {
		co := store.Checkout{
			CheckoutID:                checkoutID,
			PlanID:                    "plan_500gb",
			NormalizedReturnURL:       "https://app.example.com/",
			ProcessorFamily:           protocol.ProcessorAdyen,
			RequestFingerprint:        make([]byte, 32),
			ProviderIdempotencyKey:    "sb_checkout_" + checkoutID,
			ProcessorShopperReference: store.StrPtr(shopper),
			Status:                    protocol.CheckoutCompleted,
			ExpiresAt:                 now.Add(time.Hour),
		}
		if err := tx.InsertCheckout(co); err != nil {
			return err
		}
		sub := store.Subscription{
			SubscriptionRef:           subRef,
			CheckoutID:                checkoutID,
			PlanID:                    "plan_500gb",
			Status:                    protocol.StatusActive,
			StateVersion:              1,
			ProcessorFamily:           protocol.ProcessorAdyen,
			ProcessorShopperReference: store.StrPtr(shopper),
			CurrentPeriodStart:        now,
			CurrentPeriodEnd:          protocol.AddCalendarMonths(now, 1),
			StateChangedAt:            now,
		}
		if err := tx.InsertSubscription(sub); err != nil {
			return err
		}
		return tx.InsertAction(store.ScheduledAction{
			ActionID:        actionID,
			ActionKey:       protocol.ActionKey(subRef, protocol.ActionRenew, sub.CurrentPeriodEnd),
			SubscriptionRef: subRef,
			ActionType:      protocol.ActionRenew,
			TargetAt:        sub.CurrentPeriodEnd,
			DueAt:           now,
			Status:          "pending",
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 2)
	var hold sync.WaitGroup
	hold.Add(1)
	for i := 0; i < 2; i++ {
		go func() {
			err := db.InTx(ctx, func(tx store.Tx) error {
				a, err := tx.ClaimDueAction(now, time.Minute, "pending", protocol.ActionRenew)
				if err != nil {
					return err
				}
				ch <- result{id: a.ActionID}
				hold.Wait()
				return nil
			})
			if err != nil {
				ch <- result{err: err}
			}
		}()
	}
	var won, lost int
	for i := 0; i < 2; i++ {
		select {
		case got := <-ch:
			switch {
			case got.err == nil && got.id == actionID:
				won++
			case errors.Is(got.err, store.ErrNotFound):
				lost++
			default:
				t.Fatalf("claim %+v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("SKIP LOCKED claim timed out")
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("want one winner and one miss, won=%d lost=%d", won, lost)
	}
	hold.Done()
}

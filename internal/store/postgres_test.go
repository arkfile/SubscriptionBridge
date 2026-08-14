package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresAttemptReferenceLookup exercises unique attempt_reference on PostgreSQL.
func TestPostgresAttemptReferenceLookup(t *testing.T) {
	url := os.Getenv("BRIDGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BRIDGE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db := store.NewPostgres(pool, func() time.Time { return time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) })
	err = db.InTx(ctx, func(tx store.Tx) error {
		now := tx.Now()
		co := store.Checkout{
			CheckoutID:                "subchk_pg_lookup_1",
			PlanID:                    "plan_500gb",
			NormalizedReturnURL:       "https://app.example.com/",
			ProcessorFamily:           protocol.ProcessorAdyen,
			RequestFingerprint:        make([]byte, 32),
			ProviderIdempotencyKey:    "sb_checkout_subchk_pg_lookup_1",
			ProcessorShopperReference: store.StrPtr("sbr_pg_1"),
			Status:                    protocol.CheckoutPending,
			ExpiresAt:                 now.Add(time.Hour),
		}
		if err := tx.InsertCheckout(co); err != nil {
			return err
		}
		sub := store.Subscription{
			SubscriptionRef:           "sub_pg_lookup_1",
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
			ActionID:        "550e8400-e29b-41d4-a716-446655440000",
			ActionKey:       protocol.ActionKey(sub.SubscriptionRef, protocol.ActionRenew, sub.CurrentPeriodEnd),
			SubscriptionRef: sub.SubscriptionRef,
			ActionType:      protocol.ActionRenew,
			TargetAt:        sub.CurrentPeriodEnd,
			DueAt:           sub.CurrentPeriodEnd,
			Status:          "pending",
		}
		if err := tx.InsertAction(act); err != nil {
			return err
		}
		att := store.ChargeAttempt{
			AttemptID:                "550e8400-e29b-41d4-a716-446655440001",
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
			AttemptReference:         "sba_pg_lookup_1",
			ShopperReference:         "sbr_pg_1",
			ShopperInteraction:       "ContAuth",
			RecurringProcessingModel: "Subscription",
			IdempotencyKey:           "sb_charge_sba_pg_lookup_1",
			RequestFingerprint:       make([]byte, 32),
			RequestCiphertext:        []byte(`{}`),
			RequestNonce:             make([]byte, 12),
			RequestKeyVersion:        "v1",
			Status:                   "prepared",
		}
		if err := tx.InsertAttempt(att); err != nil {
			return err
		}
		got, err := tx.GetAttemptByReference("sba_pg_lookup_1")
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

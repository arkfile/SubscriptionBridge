package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	adyenadapter "github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/engine"
	"github.com/arkfile/SubscriptionBridge/internal/envelope"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/store"
)

type Scheduler struct {
	Store  store.Store
	Engine *engine.Engine
	Adyen  adapters.ProcessorAdapter
	Box    *envelope.Box
	Config config.Config
	Log    *slog.Logger
	Clock  func() time.Time
	Lease  time.Duration
}

// now returns the scheduler clock truncated to UTC seconds.
func (s *Scheduler) now() time.Time {
	if s.Clock != nil {
		return protocol.TruncateUTC(s.Clock())
	}
	return s.Store.Now()
}

// QA / TODO: expire and renew only; no uncertain exact-replay, manual_review block, or exhausted-dunning expiry.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if err := s.runExpire(ctx); err != nil {
		return err
	}
	return s.runRenew(ctx)
}

// Loop runs due expire and renew actions until ctx is cancelled.
func (s *Scheduler) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = s.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runExpire claims a due expire action and transitions the subscription to expired.
func (s *Scheduler) runExpire(ctx context.Context) error {
	now := s.now()
	return s.Store.InTx(ctx, func(tx store.Tx) error {
		action, err := tx.ClaimDueAction(now, s.lease(), "expire")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if action.ActionType != protocol.ActionExpire {
			st := "pending"
			return tx.FinishAction(action.ActionID, *action.ClaimToken, action.FencingToken, st, nil, nil)
		}
		sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
		if err != nil {
			return err
		}
		dec, err := engine.Decide(sub, engine.Observation{Status: protocol.StatusExpired}, now)
		if err != nil {
			return err
		}
		if err := s.Engine.Commit(tx, sub, dec); err != nil {
			return err
		}
		return tx.FinishAction(action.ActionID, *action.ClaimToken, action.FencingToken, "completed", nil, nil)
	})
}

// runRenew claims a due Adyen renew action and charges it.
func (s *Scheduler) runRenew(ctx context.Context) error {
	var action store.ScheduledAction
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		action, err = tx.ClaimDueAction(tx.Now(), s.lease(), "renew")
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if action.ActionType != protocol.ActionRenew {
		return nil
	}
	return s.charge(ctx, action)
}

// charge persists the canonical request, calls Adyen, and records attempt outcome.
func (s *Scheduler) charge(ctx context.Context, action store.ScheduledAction) error {
	var attempt store.ChargeAttempt
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
		if err != nil {
			return err
		}
		if sub.AutomaticChargingBlocked {
			return protocol.ErrAutomaticChargeBlocked
		}
		attempts, err := tx.GetAttemptsForAction(action.ActionID)
		if err != nil {
			return err
		}
		nextNum := len(attempts) + 1
		for _, a := range attempts {
			if a.Status == "prepared" || a.Status == "uncertain" || a.Status == "running" {
				attempt = a
				return nil
			}
			if a.AttemptNumber >= nextNum {
				nextNum = a.AttemptNumber + 1
			}
		}
		plan, _, err := s.Config.ResolvePlan(sub.PlanID)
		if err != nil {
			return err
		}
		token := ""
		if s.Box != nil && len(sub.PaymentMethodCiphertext) > 0 && sub.PaymentMethodKeyVersion != nil {
			plain, err := s.Box.Open(sub.PaymentMethodCiphertext, sub.PaymentMethodNonce, envelope.AAD("payment-method", sub.SubscriptionRef), *sub.PaymentMethodKeyVersion)
			if err != nil {
				return err
			}
			token = string(plain)
		}
		ref, err := protocol.NewAttemptReference()
		if err != nil {
			return err
		}
		idemp := "sb_charge_" + ref
		shopper := ""
		if sub.ProcessorShopperReference != nil {
			shopper = *sub.ProcessorShopperReference
		}
		body := adyenadapter.CanonicalPaymentBody(plan.Adyen.MerchantAccount, plan.Currency, ref, shopper, token, plan.AmountMinor)
		fp := sha256.Sum256(body)
		var ct, nonce []byte
		ver := envelope.VersionV1
		if s.Box != nil {
			ct, nonce, ver, err = s.Box.Seal(body, envelope.AAD("charge-request", ref))
			if err != nil {
				return err
			}
		}
		aid, err := newID()
		if err != nil {
			return err
		}
		endpoint := ""
		if ad, ok := s.Adyen.(*adyenadapter.Adapter); ok {
			endpoint = ad.PaymentsURL()
		}
		attempt = store.ChargeAttempt{
			AttemptID:                aid,
			ActionID:                 action.ActionID,
			SubscriptionRef:          sub.SubscriptionRef,
			PeriodStart:              sub.CurrentPeriodEnd,
			PeriodEnd:                protocol.AddCalendarMonths(sub.CurrentPeriodEnd, 1),
			AttemptNumber:            nextNum,
			ProviderEndpoint:         endpoint,
			ProviderAPIVersion:       adyenadapter.APIVersion,
			MerchantAccount:          plan.Adyen.MerchantAccount,
			AmountMinor:              plan.AmountMinor,
			Currency:                 plan.Currency,
			AttemptReference:         ref,
			ShopperReference:         shopper,
			ShopperInteraction:       "ContAuth",
			RecurringProcessingModel: "Subscription",
			IdempotencyKey:           idemp,
			RequestFingerprint:       fp[:],
			RequestCiphertext:        ct,
			RequestNonce:             nonce,
			RequestKeyVersion:        ver,
			Status:                   "prepared",
		}
		return tx.InsertAttempt(attempt)
	})
	if err != nil {
		return err
	}
	now := s.now()
	if action.ClaimToken == nil {
		return nil
	}
	claim := *action.ClaimToken
	fence := action.FencingToken
	err = s.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		action, attempt, err = tx.ClaimAttemptWithAction(action.ActionID, attempt.AttemptID, claim, fence, now, s.lease())
		return err
	})
	if errors.Is(err, store.ErrNotOwned) {
		return nil
	}
	if err != nil {
		return err
	}
	body := attempt.RequestCiphertext
	if s.Box != nil {
		body, err = s.Box.Open(attempt.RequestCiphertext, attempt.RequestNonce, envelope.AAD("charge-request", attempt.AttemptReference), attempt.RequestKeyVersion)
		if err != nil {
			return err
		}
	}
	res, callErr := s.Adyen.ChargeRenewal(ctx, adapters.RenewalRequest{
		Endpoint:         attempt.ProviderEndpoint,
		APIVersion:       attempt.ProviderAPIVersion,
		IdempotencyKey:   attempt.IdempotencyKey,
		CanonicalBody:    body,
		AttemptReference: attempt.AttemptReference,
		MerchantAccount:  attempt.MerchantAccount,
		AmountMinor:      attempt.AmountMinor,
		Currency:         attempt.Currency,
	})
	return s.Store.InTx(ctx, func(tx store.Tx) error {
		cur, err := tx.GetAttempt(attempt.AttemptID)
		if err != nil {
			return err
		}
		if cur.ClaimToken == nil || *cur.ClaimToken != claim || cur.FencingToken != fence {
			return nil
		}
		if callErr != nil || res.Uncertain {
			cur.Status = "uncertain"
			cur.ClaimToken = nil
			cur.LeaseUntil = nil
			if err := tx.UpdateAttempt(cur); err != nil {
				return err
			}
			due := now.Add(time.Minute)
			cls := "uncertain"
			return tx.FinishAction(action.ActionID, claim, fence, "uncertain", &due, &cls)
		}
		if res.Status == "authorized" {
			cur.Status = "authorized"
			cur.ProcessorPaymentID = store.StrPtr(res.ProcessorPaymentID)
			done := now
			cur.CompletedAt = &done
			cur.ClaimToken = nil
			cur.LeaseUntil = nil
			if err := tx.UpdateAttempt(cur); err != nil {
				return err
			}
			sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
			if err != nil {
				return err
			}
			dec, err := engine.Decide(sub, engine.Observation{
				Status:      protocol.StatusActive,
				PeriodStart: cur.PeriodStart,
				PeriodEnd:   cur.PeriodEnd,
			}, now)
			if err != nil {
				return err
			}
			if err := s.Engine.Commit(tx, sub, dec); err != nil {
				return err
			}
			return tx.FinishAction(action.ActionID, claim, fence, "completed", nil, nil)
		}
		cur.Status = "refused"
		if res.ProcessorPaymentID != "" {
			cur.ProcessorPaymentID = store.StrPtr(res.ProcessorPaymentID)
		}
		if res.RefusalCode != "" {
			cur.RefusalReasonCode = store.StrPtr(res.RefusalCode)
		}
		done := now
		cur.CompletedAt = &done
		cur.ClaimToken = nil
		cur.LeaseUntil = nil
		if err := tx.UpdateAttempt(cur); err != nil {
			return err
		}
		sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
		if err != nil {
			return err
		}
		dec, err := engine.Decide(sub, engine.Observation{Status: protocol.StatusPastDue}, now)
		if err != nil {
			return err
		}
		if err := s.Engine.Commit(tx, sub, dec); err != nil {
			return err
		}
		due := now.Add(s.nextDelay(cur.AttemptNumber))
		return tx.FinishAction(action.ActionID, claim, fence, "pending", &due, store.StrPtr("refused"))
	})
}

// nextDelay returns the configured dunning delay for an attempt number.
func (s *Scheduler) nextDelay(attemptNumber int) time.Duration {
	idx := attemptNumber - 1
	if idx >= 0 && idx < len(s.Config.RenewalRetryDelays) {
		return s.Config.RenewalRetryDelays[idx]
	}
	if d := s.Config.DunningTermination; d > 0 {
		return d
	}
	return 24 * time.Hour
}

// lease returns the scheduler claim lease duration.
func (s *Scheduler) lease() time.Duration {
	if s.Lease > 0 {
		return s.Lease
	}
	return 2 * time.Minute
}

// newID allocates a UUID for action and attempt rows.
func newID() (string, error) {
	b, err := protocol.RandomClaimToken()
	if err != nil {
		return "", err
	}
	return protocol.UUIDString(b), nil
}

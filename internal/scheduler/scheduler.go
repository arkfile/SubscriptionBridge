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

// RunOnce claims due expire, renew, and uncertain-resolution work.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if err := s.runExpire(ctx); err != nil {
		return err
	}
	if err := s.runRenew(ctx); err != nil {
		return err
	}
	return s.runUncertain(ctx)
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
		action, err := tx.ClaimDueAction(now, s.lease(), "pending", protocol.ActionExpire)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
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

// runRenew prepares a pending Adyen renew action and charges it.
func (s *Scheduler) runRenew(ctx context.Context) error {
	var action store.ScheduledAction
	var attempt store.ChargeAttempt
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		action, err = tx.LockDueAction(tx.Now(), "pending", protocol.ActionRenew)
		if err != nil {
			return err
		}
		attempt, err = s.ensurePrepared(tx, action)
		return err
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, protocol.ErrAutomaticChargeBlocked) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.submit(ctx, action, attempt, false)
}

// runUncertain exact-replays an uncertain attempt or moves it to manual_review at deadline.
func (s *Scheduler) runUncertain(ctx context.Context) error {
	now := s.now()
	var action store.ScheduledAction
	var attempt store.ChargeAttempt
	terminal := false
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		action, err = tx.LockDueAction(now, "uncertain", protocol.ActionRenew)
		if err != nil {
			return err
		}
		attempts, err := tx.GetAttemptsForAction(action.ActionID)
		if err != nil {
			return err
		}
		found := false
		for _, a := range attempts {
			if a.Status == "uncertain" || a.Status == "running" {
				attempt = a
				found = true
				break
			}
		}
		if !found {
			return store.ErrNotFound
		}
		if attempt.ResolutionDeadline != nil && !attempt.ResolutionDeadline.After(now) {
			attempt.Status = "manual_review"
			attempt.ClaimToken = nil
			attempt.LeaseUntil = nil
			if err := tx.UpdateAttempt(attempt); err != nil {
				return err
			}
			if err := tx.ForceFinishAction(action.ActionID, "manual_review", nil, store.StrPtr("resolution_deadline")); err != nil {
				return err
			}
			sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
			if err != nil {
				return err
			}
			terminal = true
			return s.Engine.BlockAutomaticCharging(tx, sub, "resolution_deadline", now)
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || terminal {
		return err
	}
	return s.submit(ctx, action, attempt, true)
}

// ensurePrepared returns the open attempt for an action or inserts the next prepared row.
func (s *Scheduler) ensurePrepared(tx store.Tx, action store.ScheduledAction) (store.ChargeAttempt, error) {
	sub, err := tx.GetSubscriptionForUpdate(action.SubscriptionRef)
	if err != nil {
		return store.ChargeAttempt{}, err
	}
	if sub.AutomaticChargingBlocked {
		return store.ChargeAttempt{}, protocol.ErrAutomaticChargeBlocked
	}
	attempts, err := tx.GetAttemptsForAction(action.ActionID)
	if err != nil {
		return store.ChargeAttempt{}, err
	}
	nextNum := 1
	for _, a := range attempts {
		if a.Status == "prepared" || a.Status == "uncertain" || a.Status == "running" {
			return a, nil
		}
		if a.AttemptNumber >= nextNum {
			nextNum = a.AttemptNumber + 1
		}
	}
	plan, _, err := s.Config.ResolvePlan(sub.PlanID)
	if err != nil {
		return store.ChargeAttempt{}, err
	}
	token := ""
	if s.Box != nil && len(sub.PaymentMethodCiphertext) > 0 && sub.PaymentMethodKeyVersion != nil {
		plain, err := s.Box.Open(sub.PaymentMethodCiphertext, sub.PaymentMethodNonce, envelope.AAD("payment-method", sub.SubscriptionRef), *sub.PaymentMethodKeyVersion)
		if err != nil {
			return store.ChargeAttempt{}, err
		}
		token = string(plain)
	}
	ref, err := protocol.NewAttemptReference()
	if err != nil {
		return store.ChargeAttempt{}, err
	}
	idemp := "sb_charge_" + ref
	shopper := ""
	if sub.ProcessorShopperReference != nil {
		shopper = *sub.ProcessorShopperReference
	}
	body := adyenadapter.CanonicalPaymentBody(plan.Adyen.MerchantAccount, plan.Currency, ref, shopper, token, plan.AmountMinor)
	fp := sha256.Sum256(body)
	aid, err := newID()
	if err != nil {
		return store.ChargeAttempt{}, err
	}
	var ct, nonce []byte
	ver := envelope.VersionV1
	if s.Box != nil {
		ct, nonce, ver, err = s.Box.Seal(body, envelope.AAD("charge-request", aid))
		if err != nil {
			return store.ChargeAttempt{}, err
		}
	} else {
		ct = body
	}
	endpoint := ""
	if ad, ok := s.Adyen.(*adyenadapter.Adapter); ok {
		endpoint = ad.PaymentsURL()
	}
	attempt := store.ChargeAttempt{
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
	if err := tx.InsertAttempt(attempt); err != nil {
		if errors.Is(err, store.ErrConflict) {
			attempts, err := tx.GetAttemptsForAction(action.ActionID)
			if err != nil {
				return store.ChargeAttempt{}, err
			}
			for _, a := range attempts {
				if a.Status == "prepared" || a.Status == "uncertain" || a.Status == "running" {
					return a, nil
				}
			}
		}
		return store.ChargeAttempt{}, err
	}
	return attempt, nil
}

// submit claims a prepared or uncertain attempt, calls Adyen, and records the outcome.
func (s *Scheduler) submit(ctx context.Context, action store.ScheduledAction, attempt store.ChargeAttempt, replay bool) error {
	now := s.now()
	deadline := now.Add(s.Config.AdyenResolutionDeadline)
	if s.Config.AdyenResolutionDeadline <= 0 {
		deadline = now.Add(144 * time.Hour)
	}
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		claimed, err := tx.ClaimAction(action.ActionID, now, s.lease())
		if err != nil {
			return err
		}
		action = claimed
		if action.ClaimToken == nil {
			return store.ErrNotOwned
		}
		action, attempt, err = tx.ClaimAttemptWithAction(action.ActionID, attempt.AttemptID, *action.ClaimToken, action.FencingToken, now, s.lease(), deadline)
		return err
	})
	if errors.Is(err, store.ErrNotOwned) || errors.Is(err, store.ErrNotFound) || errors.Is(err, protocol.ErrAutomaticChargeBlocked) {
		return nil
	}
	if err != nil {
		return err
	}
	body := attempt.RequestCiphertext
	if s.Box != nil && len(attempt.RequestNonce) > 0 {
		body, err = s.Box.Open(attempt.RequestCiphertext, attempt.RequestNonce, envelope.AAD("charge-request", attempt.AttemptID), attempt.RequestKeyVersion)
		if err != nil {
			return err
		}
	}
	var res adapters.RenewalResult
	var callErr error
	if replay {
		resolved, err := s.Adyen.ResolveRenewalAttempt(ctx, adapters.RenewalAttempt{
			Endpoint:       attempt.ProviderEndpoint,
			APIVersion:     attempt.ProviderAPIVersion,
			IdempotencyKey: attempt.IdempotencyKey,
			CanonicalBody:  body,
			AttemptRef:     attempt.AttemptReference,
		})
		callErr = err
		res = adapters.RenewalResult{
			Status:             resolved.Status,
			ProcessorPaymentID: resolved.ProcessorPaymentID,
			RefusalCode:        resolved.RefusalCode,
			Uncertain:          resolved.Uncertain,
		}
	} else {
		res, callErr = s.Adyen.ChargeRenewal(ctx, adapters.RenewalRequest{
			Endpoint:         attempt.ProviderEndpoint,
			APIVersion:       attempt.ProviderAPIVersion,
			IdempotencyKey:   attempt.IdempotencyKey,
			CanonicalBody:    body,
			AttemptReference: attempt.AttemptReference,
			MerchantAccount:  attempt.MerchantAccount,
			AmountMinor:      attempt.AmountMinor,
			Currency:         attempt.Currency,
		})
	}
	claim := ""
	if action.ClaimToken != nil {
		claim = *action.ClaimToken
	}
	fence := action.FencingToken
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
			if cur.ResolutionDeadline != nil && due.After(*cur.ResolutionDeadline) {
				due = *cur.ResolutionDeadline
			}
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
		if !engine.IsRetryableRefusal(res.RefusalCode) || cur.AttemptNumber >= len(s.Config.RenewalRetryDelays) {
			if err := tx.FinishAction(action.ActionID, claim, fence, "completed", nil, store.StrPtr("refused")); err != nil {
				return err
			}
			delay := s.Config.DunningTermination
			target := protocol.TruncateUTC(now.Add(delay))
			key := protocol.ActionKey(sub.SubscriptionRef, protocol.ActionExpire, target)
			aid, err := newID()
			if err != nil {
				return err
			}
			err = tx.InsertAction(store.ScheduledAction{
				ActionID:        aid,
				ActionKey:       key,
				SubscriptionRef: sub.SubscriptionRef,
				ActionType:      protocol.ActionExpire,
				TargetAt:        target,
				DueAt:           target,
				Status:          "pending",
			})
			if errors.Is(err, store.ErrConflict) {
				return nil
			}
			return err
		}
		due := now.Add(s.Config.RenewalRetryDelays[cur.AttemptNumber-1])
		return tx.FinishAction(action.ActionID, claim, fence, "pending", &due, store.StrPtr("refused"))
	})
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

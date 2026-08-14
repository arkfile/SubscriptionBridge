package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrNotOwned        = errors.New("not owned")
	ErrSchemaMismatch  = errors.New("schema version mismatch")
	ErrCheckoutBinding = errors.New("checkout binding conflict")
)

const CurrentSchemaVersion = 1

type Store interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	InTx(ctx context.Context, fn func(Tx) error) error
	Now() time.Time
}

type Tx interface {
	Now() time.Time

	InsertCheckout(c Checkout) error
	GetCheckout(id string) (Checkout, error)
	GetCheckoutForUpdate(id string) (Checkout, error)
	UpdateCheckout(c Checkout) error
	ExpireDueCheckouts(now time.Time) (int, error)

	InsertSubscription(s Subscription) error
	GetSubscription(ref string) (Subscription, error)
	GetSubscriptionForUpdate(ref string) (Subscription, error)
	GetSubscriptionByCheckout(checkoutID string) (Subscription, error)
	GetSubscriptionByProcessor(family, processorSubID string) (Subscription, error)
	GetSubscriptionByInitialPayment(family, paymentID string) (Subscription, error)
	GetSubscriptionByChargePayment(paymentID string) (Subscription, error)
	UpdateSubscription(s Subscription) error

	InsertOutbound(e OutboundEvent) error
	GetOutbound(eventID string) (OutboundEvent, error)
	ListOutbound(state string, limit int) ([]OutboundEvent, error)
	ClaimDueOutbound(now time.Time, lease time.Duration) (OutboundEvent, error)
	CompleteOutbound(eventID, claimToken string, fence int64, now time.Time) error
	RetryOutbound(eventID, claimToken string, fence int64, next time.Time, errClass string) error
	DeadLetterOutbound(eventID, claimToken string, fence int64, now time.Time, errClass string) error
	AbandonOutbound(eventID, reason, actor string, now time.Time) error
	RequeueOutbound(eventID, reason, actor string, now time.Time) error

	InsertProcessorEvent(e ProcessorEvent) (inserted bool, err error)
	GetProcessorEvent(family, id string) (ProcessorEvent, error)
	ClaimProcessorEvent(family, id string, processingKey string, now time.Time, lease time.Duration) (ProcessorEvent, ProcessingLease, error)
	FinishProcessorEvent(family, id, claimToken string, fence int64, state string, subRef *string, now time.Time) error
	ReleaseProcessorLease(key, claimToken string, fence int64) error

	InsertAction(a ScheduledAction) error
	GetActionByKey(key string) (ScheduledAction, error)
	ClaimDueAction(now time.Time, lease time.Duration, kinds ...string) (ScheduledAction, error)
	FinishAction(actionID, claimToken string, fence int64, status string, dueAt *time.Time, errClass *string) error
	CancelActionsForSubscription(ref, exceptKey string) error

	InsertAttempt(a ChargeAttempt) error
	GetAttempt(id string) (ChargeAttempt, error)
	GetAttemptsForAction(actionID string) ([]ChargeAttempt, error)
	UpdateAttempt(a ChargeAttempt) error
	ClaimAttemptWithAction(actionID, attemptID, claimToken string, fence int64, now time.Time, lease time.Duration) (ScheduledAction, ChargeAttempt, error)

	InsertAudit(a Audit) error
}

// IsTerminalCheckout reports whether a checkout can no longer be resumed.
func IsTerminalCheckout(status string) bool {
	switch status {
	case "completed", "expired", "canceled":
		return true
	default:
		return false
	}
}

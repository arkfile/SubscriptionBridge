package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	Pool  *pgxpool.Pool
	clock func() time.Time
}

// NewPostgres wraps a pgx pool as the production store.
func NewPostgres(pool *pgxpool.Pool, now func() time.Time) *Postgres {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Postgres{Pool: pool, clock: now}
}

// Ping checks database connectivity.
func (p *Postgres) Ping(ctx context.Context) error { return p.Pool.Ping(ctx) }

// SchemaVersion reads the highest applied migration version.
func (p *Postgres) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := p.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM sb_schema_migrations`).Scan(&v)
	return v, err
}

// Now returns the store clock truncated to UTC seconds.
func (p *Postgres) Now() time.Time { return protocol.TruncateUTC(p.clock()) }

// InTx runs fn in a PostgreSQL transaction and commits on success.
func (p *Postgres) InTx(ctx context.Context, fn func(Tx) error) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(&pgTx{ctx: ctx, tx: tx, now: p.Now}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type pgTx struct {
	ctx context.Context
	tx  pgx.Tx
	now func() time.Time
}

// Now returns the transaction clock truncated to UTC seconds.
func (t *pgTx) Now() time.Time { return t.now() }

// InsertCheckout persists a new checkout; conflicting IDs fail.
func (t *pgTx) InsertCheckout(c Checkout) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_checkouts (
		checkout_id, plan_id, normalized_return_url, processor_family, request_fingerprint,
		provider_idempotency_key, processor_shopper_reference, status, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		c.CheckoutID, c.PlanID, c.NormalizedReturnURL, c.ProcessorFamily, c.RequestFingerprint,
		c.ProviderIdempotencyKey, c.ProcessorShopperReference, c.Status, c.ExpiresAt)
	return mapErr(err)
}

// GetCheckout loads a checkout by opaque ID.
func (t *pgTx) GetCheckout(id string) (Checkout, error) {
	return t.scanCheckout(`SELECT checkout_id, plan_id, normalized_return_url, processor_family, request_fingerprint,
		provider_idempotency_key, processor_shopper_reference, status, subscription_ref, processor_checkout_id, expires_at, created_at, updated_at
		FROM sb_checkouts WHERE checkout_id=$1`, id)
}

// GetCheckoutForUpdate loads a checkout with a row lock.
func (t *pgTx) GetCheckoutForUpdate(id string) (Checkout, error) {
	return t.scanCheckout(`SELECT checkout_id, plan_id, normalized_return_url, processor_family, request_fingerprint,
		provider_idempotency_key, processor_shopper_reference, status, subscription_ref, processor_checkout_id, expires_at, created_at, updated_at
		FROM sb_checkouts WHERE checkout_id=$1 FOR UPDATE`, id)
}

// scanCheckout scans a checkout row.
func (t *pgTx) scanCheckout(q string, args ...any) (Checkout, error) {
	var c Checkout
	err := t.tx.QueryRow(t.ctx, q, args...).Scan(
		&c.CheckoutID, &c.PlanID, &c.NormalizedReturnURL, &c.ProcessorFamily, &c.RequestFingerprint,
		&c.ProviderIdempotencyKey, &c.ProcessorShopperReference, &c.Status, &c.SubscriptionRef, &c.ProcessorCheckoutID,
		&c.ExpiresAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkout{}, ErrNotFound
	}
	return c, err
}

// UpdateCheckout writes checkout status and bound identifiers.
func (t *pgTx) UpdateCheckout(c Checkout) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE sb_checkouts SET status=$2, subscription_ref=$3, processor_checkout_id=$4, expires_at=$5, updated_at=NOW() WHERE checkout_id=$1`,
		c.CheckoutID, c.Status, c.SubscriptionRef, c.ProcessorCheckoutID, c.ExpiresAt)
	return err
}

// ExpireDueCheckouts marks pending checkouts whose TTL has elapsed.
func (t *pgTx) ExpireDueCheckouts(now time.Time) (int, error) {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_checkouts SET status='expired', updated_at=NOW()
		WHERE status IN ('creating','pending') AND expires_at <= $1`, now)
	return int(tag.RowsAffected()), err
}

// InsertSubscription persists a new subscription at state version 1.
func (t *pgTx) InsertSubscription(s Subscription) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_subscriptions (
		subscription_ref, checkout_id, plan_id, status, state_version, processor_family,
		processor_customer_id, processor_subscription_id, processor_initial_payment_id, processor_shopper_reference,
		payment_method_ciphertext, payment_method_nonce, payment_method_key_version,
		current_period_start, current_period_end, cancel_at_period_end, state_changed_at,
		past_due_since, canceled_at, automatic_charging_blocked, charging_block_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		s.SubscriptionRef, s.CheckoutID, s.PlanID, s.Status, s.StateVersion, s.ProcessorFamily,
		s.ProcessorCustomerID, s.ProcessorSubscriptionID, s.ProcessorInitialPaymentID, s.ProcessorShopperReference,
		s.PaymentMethodCiphertext, s.PaymentMethodNonce, s.PaymentMethodKeyVersion,
		s.CurrentPeriodStart, s.CurrentPeriodEnd, s.CancelAtPeriodEnd, s.StateChangedAt,
		s.PastDueSince, s.CanceledAt, s.AutomaticChargingBlocked, s.ChargingBlockReason)
	return mapErr(err)
}

// scanSub scans a subscription row.
func (t *pgTx) scanSub(q string, args ...any) (Subscription, error) {
	var s Subscription
	err := t.tx.QueryRow(t.ctx, q, args...).Scan(
		&s.SubscriptionRef, &s.CheckoutID, &s.PlanID, &s.Status, &s.StateVersion, &s.ProcessorFamily,
		&s.ProcessorCustomerID, &s.ProcessorSubscriptionID, &s.ProcessorInitialPaymentID, &s.ProcessorShopperReference,
		&s.PaymentMethodCiphertext, &s.PaymentMethodNonce, &s.PaymentMethodKeyVersion,
		&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.CancelAtPeriodEnd, &s.StateChangedAt,
		&s.PastDueSince, &s.CanceledAt, &s.AutomaticChargingBlocked, &s.ChargingBlockReason,
		&s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	return s, err
}

const subCols = `subscription_ref, checkout_id, plan_id, status, state_version, processor_family,
		processor_customer_id, processor_subscription_id, processor_initial_payment_id, processor_shopper_reference,
		payment_method_ciphertext, payment_method_nonce, payment_method_key_version,
		current_period_start, current_period_end, cancel_at_period_end, state_changed_at,
		past_due_since, canceled_at, automatic_charging_blocked, charging_block_reason, created_at, updated_at`

// GetSubscription loads a subscription by opaque ref.
func (t *pgTx) GetSubscription(ref string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions WHERE subscription_ref=$1`, ref)
}
// GetSubscriptionForUpdate loads a subscription with a row lock.
func (t *pgTx) GetSubscriptionForUpdate(ref string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions WHERE subscription_ref=$1 FOR UPDATE`, ref)
}
// GetSubscriptionByCheckout finds the subscription bound to a checkout.
func (t *pgTx) GetSubscriptionByCheckout(id string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions WHERE checkout_id=$1`, id)
}
// GetSubscriptionByProcessor looks up a subscription by processor IDs.
func (t *pgTx) GetSubscriptionByProcessor(family, id string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions WHERE processor_family=$1 AND processor_subscription_id=$2`, family, id)
}
// GetSubscriptionByInitialPayment looks up a subscription by the initial payment ID.
func (t *pgTx) GetSubscriptionByInitialPayment(family, id string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions WHERE processor_family=$1 AND processor_initial_payment_id=$2`, family, id)
}
// GetSubscriptionByChargePayment looks up a subscription by a later charge payment ID.
func (t *pgTx) GetSubscriptionByChargePayment(id string) (Subscription, error) {
	return t.scanSub(`SELECT `+subCols+` FROM sb_subscriptions s JOIN sb_charge_attempts a ON a.subscription_ref=s.subscription_ref WHERE a.processor_payment_id=$1`, id)
}

// UpdateSubscription writes canonical subscription state.
func (t *pgTx) UpdateSubscription(s Subscription) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE sb_subscriptions SET
		plan_id=$2, status=$3, state_version=$4, processor_customer_id=$5, processor_subscription_id=$6,
		processor_initial_payment_id=$7, payment_method_ciphertext=$8, payment_method_nonce=$9, payment_method_key_version=$10,
		current_period_start=$11, current_period_end=$12, cancel_at_period_end=$13, state_changed_at=$14,
		past_due_since=$15, canceled_at=$16, automatic_charging_blocked=$17, charging_block_reason=$18, updated_at=NOW()
		WHERE subscription_ref=$1`,
		s.SubscriptionRef, s.PlanID, s.Status, s.StateVersion, s.ProcessorCustomerID, s.ProcessorSubscriptionID,
		s.ProcessorInitialPaymentID, s.PaymentMethodCiphertext, s.PaymentMethodNonce, s.PaymentMethodKeyVersion,
		s.CurrentPeriodStart, s.CurrentPeriodEnd, s.CancelAtPeriodEnd, s.StateChangedAt,
		s.PastDueSince, s.CanceledAt, s.AutomaticChargingBlocked, s.ChargingBlockReason)
	return err
}

// InsertOutbound appends an immutable outbound callback row.
func (t *pgTx) InsertOutbound(e OutboundEvent) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_outbound_events (
		event_id, event_type, subscription_ref, checkout_id, state_version, payload_body, payload_json, delivery_state, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,'pending',$8)`,
		e.EventID, e.EventType, e.SubscriptionRef, e.CheckoutID, e.StateVersion, e.PayloadBody, e.PayloadBody, e.NextAttemptAt)
	return mapErr(err)
}

// GetOutbound loads an outbound event by event_id.
func (t *pgTx) GetOutbound(eventID string) (OutboundEvent, error) {
	return t.scanOutbound(`SELECT event_id, event_type, subscription_ref, checkout_id, state_version, payload_body, delivery_state, attempt_count, next_attempt_at, delivered_at, dead_lettered_at, abandoned_at, last_error_class, claim_token::text, fencing_token, lease_until, created_at FROM sb_outbound_events WHERE event_id=$1`, eventID)
}

// ListOutbound returns outbound events, optionally filtered by delivery state.
func (t *pgTx) ListOutbound(state string, limit int) ([]OutboundEvent, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT event_id, event_type, subscription_ref, checkout_id, state_version, payload_body, delivery_state, attempt_count, next_attempt_at, delivered_at, dead_lettered_at, abandoned_at, last_error_class, claim_token::text, fencing_token, lease_until, created_at FROM sb_outbound_events WHERE ($1='' OR delivery_state=$1) ORDER BY created_at LIMIT $2`, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboundEvent
	for rows.Next() {
		e, err := scanOutboundRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanOutboundRow scans an outbound event from any row source.
func scanOutboundRow(row rowScanner) (OutboundEvent, error) {
	var e OutboundEvent
	err := row.Scan(&e.EventID, &e.EventType, &e.SubscriptionRef, &e.CheckoutID, &e.StateVersion, &e.PayloadBody, &e.DeliveryState, &e.AttemptCount, &e.NextAttemptAt, &e.DeliveredAt, &e.DeadLetteredAt, &e.AbandonedAt, &e.LastErrorClass, &e.ClaimToken, &e.FencingToken, &e.LeaseUntil, &e.CreatedAt)
	return e, err
}

// scanOutbound scans one outbound event.
func (t *pgTx) scanOutbound(q string, args ...any) (OutboundEvent, error) {
	e, err := scanOutboundRow(t.tx.QueryRow(t.ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundEvent{}, ErrNotFound
	}
	return e, err
}

// ClaimDueOutbound leases one due outbound row with a new fence.
func (t *pgTx) ClaimDueOutbound(now time.Time, lease time.Duration) (OutboundEvent, error) {
	row := t.tx.QueryRow(t.ctx, `
		UPDATE sb_outbound_events SET claim_token=gen_random_uuid(), fencing_token=fencing_token+1, lease_until=$2
		WHERE event_id=(
			SELECT event_id FROM sb_outbound_events
			WHERE delivery_state='pending' AND next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until < $1)
			ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING event_id, event_type, subscription_ref, checkout_id, state_version, payload_body, delivery_state, attempt_count, next_attempt_at, delivered_at, dead_lettered_at, abandoned_at, last_error_class, claim_token::text, fencing_token, lease_until, created_at`, now, now.Add(lease))
	e, err := scanOutboundRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundEvent{}, ErrNotFound
	}
	return e, err
}

// CompleteOutbound marks a claimed outbound event delivered.
func (t *pgTx) CompleteOutbound(eventID, claimToken string, fence int64, now time.Time) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_outbound_events SET delivery_state='delivered', delivered_at=$4, next_attempt_at=NULL, claim_token=NULL, lease_until=NULL, attempt_count=attempt_count+1
		WHERE event_id=$1 AND delivery_state='pending' AND claim_token=$2::uuid AND fencing_token=$3`, eventID, claimToken, fence, now)
	return owned(tag.RowsAffected(), err)
}

// RetryOutbound schedules a claimed outbound event for another attempt.
func (t *pgTx) RetryOutbound(eventID, claimToken string, fence int64, next time.Time, errClass string) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_outbound_events SET next_attempt_at=$4, last_error_class=$5, claim_token=NULL, lease_until=NULL, attempt_count=attempt_count+1
		WHERE event_id=$1 AND delivery_state='pending' AND claim_token=$2::uuid AND fencing_token=$3`, eventID, claimToken, fence, next, errClass)
	return owned(tag.RowsAffected(), err)
}

// DeadLetterOutbound terminals a claimed outbound event after a deterministic 4xx.
func (t *pgTx) DeadLetterOutbound(eventID, claimToken string, fence int64, now time.Time, errClass string) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_outbound_events SET delivery_state='dead_lettered', dead_lettered_at=$4, next_attempt_at=NULL, claim_token=NULL, lease_until=NULL, last_error_class=$5, attempt_count=attempt_count+1
		WHERE event_id=$1 AND delivery_state='pending' AND claim_token=$2::uuid AND fencing_token=$3`, eventID, claimToken, fence, now, errClass)
	return owned(tag.RowsAffected(), err)
}

// AbandonOutbound operator-terminals an outbound event and records audit.
func (t *pgTx) AbandonOutbound(eventID, reason, actor string, now time.Time) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_outbound_events SET delivery_state='abandoned', abandoned_at=$2, next_attempt_at=NULL, claim_token=NULL, lease_until=NULL WHERE event_id=$1 AND delivery_state='pending'`, eventID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return t.InsertAudit(Audit{AuditID: mustPGToken(), Action: "abandon-event", TargetType: "event", TargetID: eventID, Actor: actor, Reason: reason, Metadata: map[string]any{}})
}

// RequeueOutbound returns a terminal outbound event to pending for replay.
func (t *pgTx) RequeueOutbound(eventID, reason, actor string, now time.Time) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_outbound_events SET delivery_state='pending', next_attempt_at=$2, dead_lettered_at=NULL, abandoned_at=NULL, delivered_at=NULL, claim_token=NULL, lease_until=NULL WHERE event_id=$1 AND delivery_state IN ('dead_lettered','abandoned')`, eventID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return t.InsertAudit(Audit{AuditID: mustPGToken(), Action: "requeue-event", TargetType: "event", TargetID: eventID, Actor: actor, Reason: reason, Metadata: map[string]any{}})
}

// InsertProcessorEvent records a provider event idempotently and reports whether it was new.
func (t *pgTx) InsertProcessorEvent(e ProcessorEvent) (bool, error) {
	fields, _ := json.Marshal(e.NormalizedFields)
	tag, err := t.tx.Exec(t.ctx, `INSERT INTO sb_processor_events (
		processor_family, processor_event_id, processing_action_id, provider_event_type, payload_hash, normalized_fields,
		sensitive_ciphertext, sensitive_nonce, sensitive_key_version, processing_state)
		VALUES ($1,$2,$3::uuid,$4,$5,$6,$7,$8,$9,'pending') ON CONFLICT DO NOTHING`,
		e.ProcessorFamily, e.ProcessorEventID, e.ProcessingActionID, e.ProviderEventType, e.PayloadHash, fields,
		e.SensitiveCiphertext, e.SensitiveNonce, e.SensitiveKeyVersion)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// GetProcessorEvent loads a stored provider event.
func (t *pgTx) GetProcessorEvent(family, id string) (ProcessorEvent, error) {
	var e ProcessorEvent
	var fields []byte
	err := t.tx.QueryRow(t.ctx, `SELECT processor_family, processor_event_id, processing_action_id::text, provider_event_type, payload_hash, normalized_fields, sensitive_ciphertext, sensitive_nonce, sensitive_key_version, processing_state, subscription_ref, received_at, processed_at, claim_token::text, fencing_token, lease_until, last_error_class FROM sb_processor_events WHERE processor_family=$1 AND processor_event_id=$2`, family, id).Scan(
		&e.ProcessorFamily, &e.ProcessorEventID, &e.ProcessingActionID, &e.ProviderEventType, &e.PayloadHash, &fields,
		&e.SensitiveCiphertext, &e.SensitiveNonce, &e.SensitiveKeyVersion, &e.ProcessingState, &e.SubscriptionRef,
		&e.ReceivedAt, &e.ProcessedAt, &e.ClaimToken, &e.FencingToken, &e.LeaseUntil, &e.LastErrorClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProcessorEvent{}, ErrNotFound
	}
	_ = json.Unmarshal(fields, &e.NormalizedFields)
	return e, err
}

// ClaimProcessorEvent takes or reclaims a processing lease and increments the fence.
func (t *pgTx) ClaimProcessorEvent(family, id, processingKey string, now time.Time, lease time.Duration) (ProcessorEvent, ProcessingLease, error) {
	e, err := t.GetProcessorEvent(family, id)
	if err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	until := now.Add(lease)
	_, err = t.tx.Exec(t.ctx, `INSERT INTO sb_processing_leases (processing_key, status, active_action_id, claim_token, fencing_token, lease_until, updated_at)
		VALUES ($1,'running',$2::uuid,gen_random_uuid(),1,$3,NOW())
		ON CONFLICT (processing_key) DO UPDATE SET status='running', active_action_id=$2::uuid, claim_token=gen_random_uuid(), fencing_token=sb_processing_leases.fencing_token+1, lease_until=$3, updated_at=NOW()
		WHERE sb_processing_leases.status='idle' OR sb_processing_leases.lease_until < $4`,
		processingKey, e.ProcessingActionID, until, now)
	if err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	var leaseRow ProcessingLease
	if err := t.tx.QueryRow(t.ctx, `SELECT processing_key, status, active_action_id::text, claim_token::text, fencing_token, lease_until FROM sb_processing_leases WHERE processing_key=$1`, processingKey).Scan(
		&leaseRow.ProcessingKey, &leaseRow.Status, &leaseRow.ActiveActionID, &leaseRow.ClaimToken, &leaseRow.FencingToken, &leaseRow.LeaseUntil); err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	_, err = t.tx.Exec(t.ctx, `UPDATE sb_processor_events SET processing_state='running', claim_token=$3::uuid, fencing_token=$4, lease_until=$5 WHERE processor_family=$1 AND processor_event_id=$2`, family, id, *leaseRow.ClaimToken, leaseRow.FencingToken, until)
	if err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	e, err = t.GetProcessorEvent(family, id)
	return e, leaseRow, err
}

// FinishProcessorEvent commits processing outcome if this worker still owns the lease.
func (t *pgTx) FinishProcessorEvent(family, id, claimToken string, fence int64, state string, subRef *string, now time.Time) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_processor_events SET processing_state=$4, claim_token=NULL, lease_until=NULL, subscription_ref=$5, processed_at=CASE WHEN $4='processed' THEN $6 ELSE processed_at END
		WHERE processor_family=$1 AND processor_event_id=$2 AND processing_state='running' AND claim_token=$3::uuid AND fencing_token=$7`,
		family, id, claimToken, state, subRef, now, fence)
	return owned(tag.RowsAffected(), err)
}

// ReleaseProcessorLease drops a lease when this worker still owns it.
func (t *pgTx) ReleaseProcessorLease(key, claimToken string, fence int64) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_processing_leases SET status='idle', active_action_id=NULL, claim_token=NULL, lease_until=NULL, updated_at=NOW() WHERE processing_key=$1 AND claim_token=$2::uuid AND fencing_token=$3`, key, claimToken, fence)
	return owned(tag.RowsAffected(), err)
}

// InsertAction records a durable scheduled action by stable key.
func (t *pgTx) InsertAction(a ScheduledAction) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_scheduled_actions (action_id, action_key, subscription_ref, action_type, target_at, due_at, status) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)`,
		a.ActionID, a.ActionKey, a.SubscriptionRef, a.ActionType, a.TargetAt, a.DueAt, a.Status)
	return mapErr(err)
}

// GetActionByKey loads a scheduled action by its stable key.
func (t *pgTx) GetActionByKey(key string) (ScheduledAction, error) {
	var a ScheduledAction
	err := t.tx.QueryRow(t.ctx, `SELECT action_id::text, action_key, subscription_ref, action_type, target_at, due_at, status, claim_token::text, fencing_token, lease_until, last_error_class FROM sb_scheduled_actions WHERE action_key=$1`, key).Scan(
		&a.ActionID, &a.ActionKey, &a.SubscriptionRef, &a.ActionType, &a.TargetAt, &a.DueAt, &a.Status, &a.ClaimToken, &a.FencingToken, &a.LeaseUntil, &a.LastErrorClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduledAction{}, ErrNotFound
	}
	return a, err
}

// ClaimDueAction leases one due scheduled action of the requested kinds.
func (t *pgTx) ClaimDueAction(now time.Time, lease time.Duration, kinds ...string) (ScheduledAction, error) {
	kind := ""
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	var a ScheduledAction
	err := t.tx.QueryRow(t.ctx, `UPDATE sb_scheduled_actions SET status='running', claim_token=gen_random_uuid(), fencing_token=fencing_token+1, lease_until=$2, updated_at=NOW()
		WHERE action_id=(
			SELECT action_id FROM sb_scheduled_actions
			WHERE status IN ('pending','uncertain') AND due_at <= $1 AND (lease_until IS NULL OR lease_until < $1)
			AND ($3='' OR action_type=$3)
			ORDER BY due_at FOR UPDATE SKIP LOCKED LIMIT 1
		) RETURNING action_id::text, action_key, subscription_ref, action_type, target_at, due_at, status, claim_token::text, fencing_token, lease_until, last_error_class`, now, now.Add(lease), kind).Scan(
		&a.ActionID, &a.ActionKey, &a.SubscriptionRef, &a.ActionType, &a.TargetAt, &a.DueAt, &a.Status, &a.ClaimToken, &a.FencingToken, &a.LeaseUntil, &a.LastErrorClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduledAction{}, ErrNotFound
	}
	return a, err
}

// FinishAction completes or reschedules a claimed action if the fence still matches.
func (t *pgTx) FinishAction(actionID, claimToken string, fence int64, status string, dueAt *time.Time, errClass *string) error {
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_scheduled_actions SET status=$4, due_at=COALESCE($5, due_at), last_error_class=$6, claim_token=NULL, lease_until=NULL, updated_at=NOW()
		WHERE action_id=$1::uuid AND status='running' AND claim_token=$2::uuid AND fencing_token=$3`, actionID, claimToken, fence, status, dueAt, errClass)
	return owned(tag.RowsAffected(), err)
}

// CancelActionsForSubscription cancels pending actions except an optional keep-key.
func (t *pgTx) CancelActionsForSubscription(ref, exceptKey string) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE sb_scheduled_actions SET status='canceled', claim_token=NULL, lease_until=NULL, updated_at=NOW() WHERE subscription_ref=$1 AND action_key <> $2 AND status IN ('pending','running')`, ref, exceptKey)
	return err
}

// InsertAttempt records a charge attempt with its encrypted canonical request.
func (t *pgTx) InsertAttempt(a ChargeAttempt) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_charge_attempts (
		attempt_id, action_id, subscription_ref, period_start, period_end, attempt_number, provider_endpoint, provider_api_version,
		merchant_account, amount_minor, currency, attempt_reference, shopper_reference, shopper_interaction, recurring_processing_model,
		idempotency_key, request_fingerprint, request_ciphertext, request_nonce, request_key_version, status)
		VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'ContAuth','Subscription',$14,$15,$16,$17,$18,$19)`,
		a.AttemptID, a.ActionID, a.SubscriptionRef, a.PeriodStart, a.PeriodEnd, a.AttemptNumber, a.ProviderEndpoint, a.ProviderAPIVersion,
		a.MerchantAccount, a.AmountMinor, a.Currency, a.AttemptReference, a.ShopperReference, a.IdempotencyKey, a.RequestFingerprint,
		a.RequestCiphertext, a.RequestNonce, a.RequestKeyVersion, a.Status)
	return mapErr(err)
}

// GetAttempt loads a charge attempt by ID.
func (t *pgTx) GetAttempt(id string) (ChargeAttempt, error) {
	var a ChargeAttempt
	err := t.tx.QueryRow(t.ctx, `SELECT attempt_id::text, action_id::text, subscription_ref, period_start, period_end, attempt_number, provider_endpoint, provider_api_version, merchant_account, amount_minor, currency, attempt_reference, shopper_reference, shopper_interaction, recurring_processing_model, idempotency_key, request_fingerprint, request_ciphertext, request_nonce, request_key_version, processor_payment_id, status, claim_token::text, fencing_token, lease_until, first_submitted_at, resolution_deadline, refusal_reason_code, completed_at FROM sb_charge_attempts WHERE attempt_id=$1::uuid`, id).Scan(
		&a.AttemptID, &a.ActionID, &a.SubscriptionRef, &a.PeriodStart, &a.PeriodEnd, &a.AttemptNumber, &a.ProviderEndpoint, &a.ProviderAPIVersion, &a.MerchantAccount, &a.AmountMinor, &a.Currency, &a.AttemptReference, &a.ShopperReference, &a.ShopperInteraction, &a.RecurringProcessingModel, &a.IdempotencyKey, &a.RequestFingerprint, &a.RequestCiphertext, &a.RequestNonce, &a.RequestKeyVersion, &a.ProcessorPaymentID, &a.Status, &a.ClaimToken, &a.FencingToken, &a.LeaseUntil, &a.FirstSubmittedAt, &a.ResolutionDeadline, &a.RefusalReasonCode, &a.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargeAttempt{}, ErrNotFound
	}
	return a, err
}

// GetAttemptsForAction lists charge attempts for a scheduled action.
func (t *pgTx) GetAttemptsForAction(actionID string) ([]ChargeAttempt, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT attempt_id::text FROM sb_charge_attempts WHERE action_id=$1::uuid ORDER BY attempt_number`, actionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChargeAttempt
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		a, err := t.GetAttempt(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAttempt writes charge-attempt status and provider correlation.
func (t *pgTx) UpdateAttempt(a ChargeAttempt) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE sb_charge_attempts SET status=$2, processor_payment_id=$3, claim_token=$4::uuid, fencing_token=$5, lease_until=$6, first_submitted_at=$7, resolution_deadline=$8, refusal_reason_code=$9, completed_at=$10 WHERE attempt_id=$1::uuid`,
		a.AttemptID, a.Status, a.ProcessorPaymentID, a.ClaimToken, a.FencingToken, a.LeaseUntil, a.FirstSubmittedAt, a.ResolutionDeadline, a.RefusalReasonCode, a.CompletedAt)
	return err
}

// ClaimAttemptWithAction re-validates action ownership before mutating an attempt.
func (t *pgTx) ClaimAttemptWithAction(actionID, attemptID, claimToken string, fence int64, now time.Time, lease time.Duration) (ScheduledAction, ChargeAttempt, error) {
	until := now.Add(lease)
	tag, err := t.tx.Exec(t.ctx, `UPDATE sb_scheduled_actions SET status='running', claim_token=$2::uuid, fencing_token=$3, lease_until=$4, updated_at=NOW() WHERE action_id=$1::uuid`, actionID, claimToken, fence, until)
	if err != nil {
		return ScheduledAction{}, ChargeAttempt{}, err
	}
	if tag.RowsAffected() == 0 {
		return ScheduledAction{}, ChargeAttempt{}, ErrNotOwned
	}
	_, err = t.tx.Exec(t.ctx, `UPDATE sb_charge_attempts SET status='running', claim_token=$2::uuid, fencing_token=$3, lease_until=$4,
		first_submitted_at=COALESCE(first_submitted_at, date_trunc('second',$5)), resolution_deadline=COALESCE(resolution_deadline, date_trunc('second',$5)+interval '6 days')
		WHERE attempt_id=$1::uuid`, attemptID, claimToken, fence, until, now)
	if err != nil {
		return ScheduledAction{}, ChargeAttempt{}, err
	}
	a, err := t.GetActionByKey("") // replaced below
	_ = a
	var act ScheduledAction
	err = t.tx.QueryRow(t.ctx, `SELECT action_id::text, action_key, subscription_ref, action_type, target_at, due_at, status, claim_token::text, fencing_token, lease_until, last_error_class FROM sb_scheduled_actions WHERE action_id=$1::uuid`, actionID).Scan(
		&act.ActionID, &act.ActionKey, &act.SubscriptionRef, &act.ActionType, &act.TargetAt, &act.DueAt, &act.Status, &act.ClaimToken, &act.FencingToken, &act.LeaseUntil, &act.LastErrorClass)
	if err != nil {
		return ScheduledAction{}, ChargeAttempt{}, err
	}
	att, err := t.GetAttempt(attemptID)
	return act, att, err
}

// InsertAudit appends an operator audit row.
func (t *pgTx) InsertAudit(a Audit) error {
	meta, _ := json.Marshal(a.Metadata)
	_, err := t.tx.Exec(t.ctx, `INSERT INTO sb_operator_audit (audit_id, action, target_type, target_id, actor, reason, metadata) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)`,
		a.AuditID, a.Action, a.TargetType, a.TargetID, a.Actor, a.Reason, meta)
	return err
}

// mapErr maps pgx.ErrNoRows to ErrNotFound.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// owned returns ErrNotOwned when a conditional update matched no rows.
func owned(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotOwned
	}
	return nil
}

// QA / TODO: panics on RNG failure; claim-token helper for leases.
func mustPGToken() string {
	b, err := protocol.RandomClaimToken()
	if err != nil {
		panic(err)
	}
	return protocol.UUIDString(b)
}

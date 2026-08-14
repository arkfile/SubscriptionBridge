package store

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

type Memory struct {
	mu         sync.Mutex
	now        func() time.Time
	checkouts  map[string]Checkout
	subs       map[string]Subscription
	outbound   map[string]OutboundEvent
	procEvents map[string]ProcessorEvent
	leases     map[string]ProcessingLease
	actions    map[string]ScheduledAction
	attempts   map[string]ChargeAttempt
	audits     []Audit
	schema     int
}

// NewMemory constructs an in-memory store for tests.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Memory{
		now:        now,
		checkouts:  map[string]Checkout{},
		subs:       map[string]Subscription{},
		outbound:   map[string]OutboundEvent{},
		procEvents: map[string]ProcessorEvent{},
		leases:     map[string]ProcessingLease{},
		actions:    map[string]ScheduledAction{},
		attempts:   map[string]ChargeAttempt{},
		schema:     CurrentSchemaVersion,
	}
}

// Ping is a no-op health check for the in-memory store.
func (m *Memory) Ping(context.Context) error { return nil }

// SchemaVersion returns the in-memory schema version.
func (m *Memory) SchemaVersion(context.Context) (int, error) { return m.schema, nil }

// Now returns the store clock truncated to UTC seconds.
func (m *Memory) Now() time.Time { return protocol.TruncateUTC(m.now()) }

// InTx runs fn under a mutex; nested InTx deadlocks.
func (m *Memory) InTx(_ context.Context, fn func(Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := m.snapshot()
	if err := fn(&memTx{m: m}); err != nil {
		m.restore(snap)
		return err
	}
	return nil
}

type memSnap struct {
	checkouts  map[string]Checkout
	subs       map[string]Subscription
	outbound   map[string]OutboundEvent
	procEvents map[string]ProcessorEvent
	leases     map[string]ProcessingLease
	actions    map[string]ScheduledAction
	attempts   map[string]ChargeAttempt
	audits     []Audit
}

// snapshot clones maps so a failed transaction can roll back.
func (m *Memory) snapshot() memSnap {
	return memSnap{
		checkouts:  cloneMap(m.checkouts),
		subs:       cloneMap(m.subs),
		outbound:   cloneMap(m.outbound),
		procEvents: cloneMap(m.procEvents),
		leases:     cloneMap(m.leases),
		actions:    cloneMap(m.actions),
		attempts:   cloneMap(m.attempts),
		audits:     append([]Audit{}, m.audits...),
	}
}

// restore replaces store maps from a snapshot.
func (m *Memory) restore(s memSnap) {
	m.checkouts = s.checkouts
	m.subs = s.subs
	m.outbound = s.outbound
	m.procEvents = s.procEvents
	m.leases = s.leases
	m.actions = s.actions
	m.attempts = s.attempts
	m.audits = s.audits
}

// cloneMap shallow-copies a map.
func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type memTx struct{ m *Memory }

// Now returns the transaction clock truncated to UTC seconds.
func (t *memTx) Now() time.Time { return t.m.Now() }

// InsertCheckout persists a new checkout; conflicting IDs fail.
func (t *memTx) InsertCheckout(c Checkout) error {
	if _, ok := t.m.checkouts[c.CheckoutID]; ok {
		return ErrConflict
	}
	for _, existing := range t.m.checkouts {
		if existing.ProviderIdempotencyKey == c.ProviderIdempotencyKey {
			return ErrConflict
		}
	}
	c.CreatedAt = t.Now()
	c.UpdatedAt = c.CreatedAt
	t.m.checkouts[c.CheckoutID] = c
	return nil
}

// GetCheckout loads a checkout by opaque ID.
func (t *memTx) GetCheckout(id string) (Checkout, error) {
	c, ok := t.m.checkouts[id]
	if !ok {
		return Checkout{}, ErrNotFound
	}
	return c, nil
}

// GetCheckoutForUpdate loads a checkout with a row lock.
func (t *memTx) GetCheckoutForUpdate(id string) (Checkout, error) {
	return t.GetCheckout(id)
}

// UpdateCheckout writes checkout status and bound identifiers.
func (t *memTx) UpdateCheckout(c Checkout) error {
	if _, ok := t.m.checkouts[c.CheckoutID]; !ok {
		return ErrNotFound
	}
	c.UpdatedAt = t.Now()
	t.m.checkouts[c.CheckoutID] = c
	return nil
}

// ExpireDueCheckouts marks pending checkouts whose TTL has elapsed.
func (t *memTx) ExpireDueCheckouts(now time.Time) (int, error) {
	n := 0
	for id, c := range t.m.checkouts {
		if (c.Status == "creating" || c.Status == "pending") && !c.ExpiresAt.After(now) {
			c.Status = "expired"
			c.UpdatedAt = now
			t.m.checkouts[id] = c
			n++
		}
	}
	return n, nil
}

// InsertSubscription persists a new subscription at state version 1.
func (t *memTx) InsertSubscription(s Subscription) error {
	if _, ok := t.m.subs[s.SubscriptionRef]; ok {
		return ErrConflict
	}
	for _, existing := range t.m.subs {
		if existing.CheckoutID == s.CheckoutID {
			return ErrConflict
		}
	}
	s.CreatedAt = t.Now()
	s.UpdatedAt = s.CreatedAt
	t.m.subs[s.SubscriptionRef] = s
	return nil
}

// GetSubscription loads a subscription by opaque ref.
func (t *memTx) GetSubscription(ref string) (Subscription, error) {
	s, ok := t.m.subs[ref]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	return s, nil
}

// GetSubscriptionForUpdate loads a subscription with a row lock.
func (t *memTx) GetSubscriptionForUpdate(ref string) (Subscription, error) {
	return t.GetSubscription(ref)
}

// GetSubscriptionByCheckout finds the subscription bound to a checkout.
func (t *memTx) GetSubscriptionByCheckout(checkoutID string) (Subscription, error) {
	for _, s := range t.m.subs {
		if s.CheckoutID == checkoutID {
			return s, nil
		}
	}
	return Subscription{}, ErrNotFound
}

// GetSubscriptionByProcessor looks up a subscription by processor IDs.
func (t *memTx) GetSubscriptionByProcessor(family, processorSubID string) (Subscription, error) {
	for _, s := range t.m.subs {
		if s.ProcessorFamily == family && s.ProcessorSubscriptionID != nil && *s.ProcessorSubscriptionID == processorSubID {
			return s, nil
		}
	}
	return Subscription{}, ErrNotFound
}

// GetSubscriptionByInitialPayment looks up a subscription by the initial payment ID.
func (t *memTx) GetSubscriptionByInitialPayment(family, paymentID string) (Subscription, error) {
	for _, s := range t.m.subs {
		if s.ProcessorFamily == family && s.ProcessorInitialPaymentID != nil && *s.ProcessorInitialPaymentID == paymentID {
			return s, nil
		}
	}
	return Subscription{}, ErrNotFound
}

// GetSubscriptionByChargePayment looks up a subscription by a later charge payment ID.
func (t *memTx) GetSubscriptionByChargePayment(paymentID string) (Subscription, error) {
	for _, a := range t.m.attempts {
		if a.ProcessorPaymentID != nil && *a.ProcessorPaymentID == paymentID {
			return t.GetSubscription(a.SubscriptionRef)
		}
	}
	return Subscription{}, ErrNotFound
}

// UpdateSubscription writes canonical subscription state.
func (t *memTx) UpdateSubscription(s Subscription) error {
	if _, ok := t.m.subs[s.SubscriptionRef]; !ok {
		return ErrNotFound
	}
	s.UpdatedAt = t.Now()
	t.m.subs[s.SubscriptionRef] = s
	return nil
}

// InsertOutbound appends an immutable outbound callback row.
func (t *memTx) InsertOutbound(e OutboundEvent) error {
	if _, ok := t.m.outbound[e.EventID]; ok {
		return ErrConflict
	}
	for _, existing := range t.m.outbound {
		if existing.SubscriptionRef == e.SubscriptionRef && existing.StateVersion == e.StateVersion {
			return ErrConflict
		}
	}
	e.CreatedAt = t.Now()
	t.m.outbound[e.EventID] = e
	return nil
}

// GetOutbound loads an outbound event by event_id.
func (t *memTx) GetOutbound(eventID string) (OutboundEvent, error) {
	e, ok := t.m.outbound[eventID]
	if !ok {
		return OutboundEvent{}, ErrNotFound
	}
	return e, nil
}

// ListOutbound returns outbound events, optionally filtered by delivery state.
func (t *memTx) ListOutbound(state string, limit int) ([]OutboundEvent, error) {
	var out []OutboundEvent
	for _, e := range t.m.outbound {
		if state == "" || e.DeliveryState == state {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimDueOutbound leases one due outbound row with a new fence.
func (t *memTx) ClaimDueOutbound(now time.Time, lease time.Duration) (OutboundEvent, error) {
	var chosen *OutboundEvent
	for _, e := range t.m.outbound {
		if e.DeliveryState != protocol.DeliveryPending || e.NextAttemptAt == nil || e.NextAttemptAt.After(now) {
			continue
		}
		if e.LeaseUntil != nil && e.LeaseUntil.After(now) {
			continue
		}
		copy := e
		if chosen == nil || copy.NextAttemptAt.Before(*chosen.NextAttemptAt) {
			chosen = &copy
		}
	}
	if chosen == nil {
		return OutboundEvent{}, ErrNotFound
	}
	token, err := newToken()
	if err != nil {
		return OutboundEvent{}, err
	}
	e := t.m.outbound[chosen.EventID]
	until := now.Add(lease)
	e.ClaimToken = &token
	e.FencingToken++
	e.LeaseUntil = &until
	t.m.outbound[e.EventID] = e
	return e, nil
}

// ownedOutbound returns the outbound row if claim token and fence still match.
func (t *memTx) ownedOutbound(eventID, claimToken string, fence int64) (OutboundEvent, error) {
	e, ok := t.m.outbound[eventID]
	if !ok {
		return OutboundEvent{}, ErrNotFound
	}
	if e.DeliveryState != protocol.DeliveryPending || e.ClaimToken == nil || *e.ClaimToken != claimToken || e.FencingToken != fence {
		return OutboundEvent{}, ErrNotOwned
	}
	return e, nil
}

// CompleteOutbound marks a claimed outbound event delivered.
func (t *memTx) CompleteOutbound(eventID, claimToken string, fence int64, now time.Time) error {
	e, err := t.ownedOutbound(eventID, claimToken, fence)
	if err != nil {
		return err
	}
	e.DeliveryState = protocol.DeliveryDelivered
	e.DeliveredAt = &now
	e.NextAttemptAt = nil
	e.ClaimToken = nil
	e.LeaseUntil = nil
	e.AttemptCount++
	t.m.outbound[eventID] = e
	return nil
}

// RetryOutbound schedules a claimed outbound event for another attempt.
func (t *memTx) RetryOutbound(eventID, claimToken string, fence int64, next time.Time, errClass string) error {
	e, err := t.ownedOutbound(eventID, claimToken, fence)
	if err != nil {
		return err
	}
	e.NextAttemptAt = &next
	e.LastErrorClass = &errClass
	e.ClaimToken = nil
	e.LeaseUntil = nil
	e.AttemptCount++
	t.m.outbound[eventID] = e
	return nil
}

// DeadLetterOutbound terminals a claimed outbound event after a deterministic 4xx.
func (t *memTx) DeadLetterOutbound(eventID, claimToken string, fence int64, now time.Time, errClass string) error {
	e, err := t.ownedOutbound(eventID, claimToken, fence)
	if err != nil {
		return err
	}
	e.DeliveryState = protocol.DeliveryDeadLettered
	e.DeadLetteredAt = &now
	e.NextAttemptAt = nil
	e.ClaimToken = nil
	e.LeaseUntil = nil
	e.LastErrorClass = &errClass
	e.AttemptCount++
	t.m.outbound[eventID] = e
	return nil
}

// AbandonOutbound operator-terminals an outbound event and records audit.
func (t *memTx) AbandonOutbound(eventID, reason, actor string, now time.Time) error {
	e, ok := t.m.outbound[eventID]
	if !ok {
		return ErrNotFound
	}
	if e.DeliveryState != protocol.DeliveryPending {
		return ErrConflict
	}
	e.DeliveryState = protocol.DeliveryAbandoned
	e.AbandonedAt = &now
	e.NextAttemptAt = nil
	e.ClaimToken = nil
	e.LeaseUntil = nil
	t.m.outbound[eventID] = e
	return t.InsertAudit(Audit{
		AuditID:    mustToken(),
		Action:     "abandon-event",
		TargetType: "event",
		TargetID:   eventID,
		Actor:      actor,
		Reason:     reason,
		Metadata:   map[string]any{},
	})
}

// RequeueOutbound returns a terminal outbound event to pending for replay.
func (t *memTx) RequeueOutbound(eventID, reason, actor string, now time.Time) error {
	e, ok := t.m.outbound[eventID]
	if !ok {
		return ErrNotFound
	}
	if e.DeliveryState != protocol.DeliveryDeadLettered && e.DeliveryState != protocol.DeliveryAbandoned {
		return ErrConflict
	}
	e.DeliveryState = protocol.DeliveryPending
	e.NextAttemptAt = &now
	e.DeadLetteredAt = nil
	e.AbandonedAt = nil
	e.DeliveredAt = nil
	e.ClaimToken = nil
	e.LeaseUntil = nil
	t.m.outbound[eventID] = e
	return t.InsertAudit(Audit{
		AuditID:    mustToken(),
		Action:     "requeue-event",
		TargetType: "event",
		TargetID:   eventID,
		Actor:      actor,
		Reason:     reason,
		Metadata:   map[string]any{},
	})
}

// procKey builds the in-memory provider-event map key.
func procKey(family, id string) string { return family + "\x00" + id }

// InsertProcessorEvent records a provider event idempotently and reports whether it was new.
func (t *memTx) InsertProcessorEvent(e ProcessorEvent) (bool, error) {
	k := procKey(e.ProcessorFamily, e.ProcessorEventID)
	if _, ok := t.m.procEvents[k]; ok {
		return false, nil
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = t.Now()
	}
	t.m.procEvents[k] = e
	return true, nil
}

// GetProcessorEvent loads a stored provider event.
func (t *memTx) GetProcessorEvent(family, id string) (ProcessorEvent, error) {
	e, ok := t.m.procEvents[procKey(family, id)]
	if !ok {
		return ProcessorEvent{}, ErrNotFound
	}
	return e, nil
}

// ClaimProcessorEvent takes or reclaims a processing lease and increments the fence.
func (t *memTx) ClaimProcessorEvent(family, id, processingKey string, now time.Time, lease time.Duration) (ProcessorEvent, ProcessingLease, error) {
	e, err := t.GetProcessorEvent(family, id)
	if err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	if e.ProcessingState == "processed" || e.ProcessingState == "quarantined" || e.ProcessingState == "manual_review" {
		return ProcessorEvent{}, ProcessingLease{}, ErrConflict
	}
	leaseRow, ok := t.m.leases[processingKey]
	if ok && leaseRow.Status == "running" && leaseRow.LeaseUntil != nil && leaseRow.LeaseUntil.After(now) {
		if leaseRow.ActiveActionID == nil || *leaseRow.ActiveActionID != e.ProcessingActionID {
			return ProcessorEvent{}, ProcessingLease{}, ErrConflict
		}
	}
	token, err := newToken()
	if err != nil {
		return ProcessorEvent{}, ProcessingLease{}, err
	}
	until := now.Add(lease)
	leaseRow.ProcessingKey = processingKey
	leaseRow.Status = "running"
	leaseRow.ActiveActionID = &e.ProcessingActionID
	leaseRow.ClaimToken = &token
	leaseRow.FencingToken++
	leaseRow.LeaseUntil = &until
	e.ProcessingState = "running"
	e.ClaimToken = &token
	e.FencingToken = leaseRow.FencingToken
	e.LeaseUntil = &until
	t.m.leases[processingKey] = leaseRow
	t.m.procEvents[procKey(family, id)] = e
	return e, leaseRow, nil
}

// FinishProcessorEvent commits processing outcome if this worker still owns the lease.
func (t *memTx) FinishProcessorEvent(family, id, claimToken string, fence int64, state string, subRef *string, now time.Time) error {
	e, err := t.GetProcessorEvent(family, id)
	if err != nil {
		return err
	}
	if e.ProcessingState != "running" || e.ClaimToken == nil || *e.ClaimToken != claimToken || e.FencingToken != fence {
		return ErrNotOwned
	}
	e.ProcessingState = state
	e.ClaimToken = nil
	e.LeaseUntil = nil
	e.SubscriptionRef = subRef
	if state == "processed" {
		e.ProcessedAt = &now
	}
	t.m.procEvents[procKey(family, id)] = e
	return nil
}

// ReleaseProcessorLease drops a lease when this worker still owns it.
func (t *memTx) ReleaseProcessorLease(key, claimToken string, fence int64) error {
	lease, ok := t.m.leases[key]
	if !ok {
		return ErrNotFound
	}
	if lease.ClaimToken == nil || *lease.ClaimToken != claimToken || lease.FencingToken != fence {
		return ErrNotOwned
	}
	lease.Status = "idle"
	lease.ActiveActionID = nil
	lease.ClaimToken = nil
	lease.LeaseUntil = nil
	t.m.leases[key] = lease
	return nil
}

// InsertAction records a durable scheduled action by stable key.
func (t *memTx) InsertAction(a ScheduledAction) error {
	if _, ok := t.m.actions[a.ActionID]; ok {
		return ErrConflict
	}
	for _, existing := range t.m.actions {
		if existing.ActionKey == a.ActionKey {
			return ErrConflict
		}
	}
	t.m.actions[a.ActionID] = a
	return nil
}

// GetAction loads a scheduled action by ID.
func (t *memTx) GetAction(id string) (ScheduledAction, error) {
	a, ok := t.m.actions[id]
	if !ok {
		return ScheduledAction{}, ErrNotFound
	}
	return a, nil
}

// GetActionByKey loads a scheduled action by its stable key.
func (t *memTx) GetActionByKey(key string) (ScheduledAction, error) {
	for _, a := range t.m.actions {
		if a.ActionKey == key {
			return a, nil
		}
	}
	return ScheduledAction{}, ErrNotFound
}

// kindAllowed reports whether actionType is in kinds, or true when kinds is empty.
func kindAllowed(kinds []string, actionType string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == actionType {
			return true
		}
	}
	return false
}

// actionDue reports whether a scheduled action is claimable for the requested status.
func (t *memTx) actionDue(a ScheduledAction, now time.Time, status string) bool {
	switch a.Status {
	case status:
	case "running":
		if status != "pending" && status != "uncertain" {
			return false
		}
		if a.LeaseUntil != nil && a.LeaseUntil.After(now) {
			return false
		}
	default:
		return false
	}
	if a.DueAt.After(now) && a.Status != "running" {
		return false
	}
	if a.Status != "running" && a.LeaseUntil != nil && a.LeaseUntil.After(now) {
		return false
	}
	if a.ActionType == protocol.ActionRenew {
		sub, err := t.GetSubscription(a.SubscriptionRef)
		if err != nil || sub.AutomaticChargingBlocked {
			return false
		}
	}
	return true
}

// LockDueAction returns one due action of the requested status and kinds without claiming it.
func (t *memTx) LockDueAction(now time.Time, status string, kinds ...string) (ScheduledAction, error) {
	var chosen *ScheduledAction
	for _, a := range t.m.actions {
		if !t.actionDue(a, now, status) || !kindAllowed(kinds, a.ActionType) {
			continue
		}
		copy := a
		if chosen == nil || copy.DueAt.Before(chosen.DueAt) {
			chosen = &copy
		}
	}
	if chosen == nil {
		return ScheduledAction{}, ErrNotFound
	}
	return *chosen, nil
}

// ClaimAction takes or reclaims ownership of a specific action and increments its fence.
func (t *memTx) ClaimAction(actionID string, now time.Time, lease time.Duration) (ScheduledAction, error) {
	a, ok := t.m.actions[actionID]
	if !ok {
		return ScheduledAction{}, ErrNotFound
	}
	if a.Status != "pending" && a.Status != "uncertain" {
		if a.Status != "running" || (a.LeaseUntil != nil && a.LeaseUntil.After(now)) {
			return ScheduledAction{}, ErrNotOwned
		}
	}
	if a.ActionType == protocol.ActionRenew {
		sub, err := t.GetSubscription(a.SubscriptionRef)
		if err != nil || sub.AutomaticChargingBlocked {
			return ScheduledAction{}, protocol.ErrAutomaticChargeBlocked
		}
	}
	token, err := newToken()
	if err != nil {
		return ScheduledAction{}, err
	}
	until := now.Add(lease)
	a.Status = "running"
	a.ClaimToken = &token
	a.FencingToken++
	a.LeaseUntil = &until
	t.m.actions[a.ActionID] = a
	return a, nil
}

// ClaimDueAction locks and claims one due scheduled action of the requested status and kinds.
func (t *memTx) ClaimDueAction(now time.Time, lease time.Duration, status string, kinds ...string) (ScheduledAction, error) {
	a, err := t.LockDueAction(now, status, kinds...)
	if err != nil {
		return ScheduledAction{}, err
	}
	return t.ClaimAction(a.ActionID, now, lease)
}

// FinishAction completes or reschedules a claimed action if the fence still matches.
func (t *memTx) FinishAction(actionID, claimToken string, fence int64, status string, dueAt *time.Time, errClass *string) error {
	a, ok := t.m.actions[actionID]
	if !ok {
		return ErrNotFound
	}
	if a.Status != "running" || a.ClaimToken == nil || *a.ClaimToken != claimToken || a.FencingToken != fence {
		return ErrNotOwned
	}
	a.Status = status
	a.LastErrorClass = errClass
	if dueAt != nil {
		a.DueAt = *dueAt
	}
	if status != "running" {
		a.ClaimToken = nil
		a.LeaseUntil = nil
	}
	t.m.actions[actionID] = a
	return nil
}

// ForceFinishAction completes a non-terminal action without a fencing match, for webhook correlation.
func (t *memTx) ForceFinishAction(actionID, status string, dueAt *time.Time, errClass *string) error {
	a, ok := t.m.actions[actionID]
	if !ok {
		return ErrNotFound
	}
	switch a.Status {
	case "pending", "running", "uncertain":
	default:
		return nil
	}
	a.Status = status
	a.LastErrorClass = errClass
	if dueAt != nil {
		a.DueAt = *dueAt
	}
	a.ClaimToken = nil
	a.LeaseUntil = nil
	t.m.actions[actionID] = a
	return nil
}

// CancelActionsForSubscription cancels pending actions except an optional keep-key.
func (t *memTx) CancelActionsForSubscription(ref, exceptKey string) error {
	for id, a := range t.m.actions {
		if a.SubscriptionRef == ref && a.ActionKey != exceptKey && (a.Status == "pending" || a.Status == "uncertain") {
			a.Status = "canceled"
			a.ClaimToken = nil
			a.LeaseUntil = nil
			t.m.actions[id] = a
		}
	}
	return nil
}

// ListActions returns scheduled actions for operator status.
func (t *memTx) ListActions(limit int) ([]ScheduledAction, error) {
	out := make([]ScheduledAction, 0, len(t.m.actions))
	for _, a := range t.m.actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InsertAttempt records a charge attempt with its encrypted canonical request.
func (t *memTx) InsertAttempt(a ChargeAttempt) error {
	if _, ok := t.m.attempts[a.AttemptID]; ok {
		return ErrConflict
	}
	for _, existing := range t.m.attempts {
		if existing.AttemptReference == a.AttemptReference || existing.IdempotencyKey == a.IdempotencyKey {
			return ErrConflict
		}
		if existing.SubscriptionRef == a.SubscriptionRef && existing.PeriodStart.Equal(a.PeriodStart) && existing.AttemptNumber == a.AttemptNumber {
			return ErrConflict
		}
	}
	t.m.attempts[a.AttemptID] = a
	return nil
}

// GetAttempt loads a charge attempt by ID.
func (t *memTx) GetAttempt(id string) (ChargeAttempt, error) {
	a, ok := t.m.attempts[id]
	if !ok {
		return ChargeAttempt{}, ErrNotFound
	}
	return a, nil
}

// GetAttemptByReference loads a charge attempt by its stable Adyen reference.
func (t *memTx) GetAttemptByReference(ref string) (ChargeAttempt, error) {
	for _, a := range t.m.attempts {
		if a.AttemptReference == ref {
			return a, nil
		}
	}
	return ChargeAttempt{}, ErrNotFound
}

// GetAttemptsForAction lists charge attempts for a scheduled action.
func (t *memTx) GetAttemptsForAction(actionID string) ([]ChargeAttempt, error) {
	var out []ChargeAttempt
	for _, a := range t.m.attempts {
		if a.ActionID == actionID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return aNum(out[i]) < aNum(out[j]) })
	return out, nil
}

// ListAttempts returns charge attempts matching the given statuses.
func (t *memTx) ListAttempts(statuses []string, limit int) ([]ChargeAttempt, error) {
	allow := map[string]bool{}
	for _, s := range statuses {
		allow[s] = true
	}
	var out []ChargeAttempt
	for _, a := range t.m.attempts {
		if len(allow) > 0 && !allow[a.Status] {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return aNum(out[i]) < aNum(out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// aNum returns a charge attempt's sequence number for sorting.
func aNum(a ChargeAttempt) int { return a.AttemptNumber }

// UpdateAttempt writes charge-attempt status and provider correlation.
func (t *memTx) UpdateAttempt(a ChargeAttempt) error {
	if _, ok := t.m.attempts[a.AttemptID]; !ok {
		return ErrNotFound
	}
	t.m.attempts[a.AttemptID] = a
	return nil
}

// ClaimAttemptWithAction re-validates action ownership before mutating an attempt.
func (t *memTx) ClaimAttemptWithAction(actionID, attemptID, claimToken string, fence int64, now time.Time, lease time.Duration, resolutionDeadline time.Time) (ScheduledAction, ChargeAttempt, error) {
	a, ok := t.m.actions[actionID]
	if !ok {
		return ScheduledAction{}, ChargeAttempt{}, ErrNotFound
	}
	if a.Status != "running" || a.ClaimToken == nil || *a.ClaimToken != claimToken || a.FencingToken != fence {
		return ScheduledAction{}, ChargeAttempt{}, ErrNotOwned
	}
	att, ok := t.m.attempts[attemptID]
	if !ok {
		return ScheduledAction{}, ChargeAttempt{}, ErrNotFound
	}
	until := now.Add(lease)
	att.Status = "running"
	att.ClaimToken = &claimToken
	att.FencingToken = fence
	att.LeaseUntil = &until
	if att.FirstSubmittedAt == nil {
		n := protocol.TruncateUTC(now)
		att.FirstSubmittedAt = &n
		dl := protocol.TruncateUTC(resolutionDeadline)
		if dl.IsZero() {
			dl = n.Add(6 * 24 * time.Hour)
		}
		att.ResolutionDeadline = &dl
	}
	t.m.attempts[attemptID] = att
	return a, att, nil
}

// ListSubscriptions returns subscriptions for operator reconcile.
func (t *memTx) ListSubscriptions(limit int) ([]Subscription, error) {
	out := make([]Subscription, 0, len(t.m.subs))
	for _, s := range t.m.subs {
		out = append(out, s)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LatestOutbound returns the highest state_version outbound event for a subscription.
func (t *memTx) LatestOutbound(subscriptionRef string) (OutboundEvent, error) {
	var best OutboundEvent
	found := false
	for _, e := range t.m.outbound {
		if e.SubscriptionRef != subscriptionRef {
			continue
		}
		if !found || e.StateVersion > best.StateVersion {
			best = e
			found = true
		}
	}
	if !found {
		return OutboundEvent{}, ErrNotFound
	}
	return best, nil
}

// InsertAudit appends an operator audit row.
func (t *memTx) InsertAudit(a Audit) error {
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	_, _ = json.Marshal(a.Metadata)
	t.m.audits = append(t.m.audits, a)
	return nil
}

// newToken allocates a UUID-shaped claim token.
func newToken() (string, error) {
	b, err := protocol.RandomClaimToken()
	if err != nil {
		return "", err
	}
	return protocol.UUIDString(b), nil
}

// QA / TODO: panics on RNG failure; in-memory claim-token helper.
func mustToken() string {
	s, err := newToken()
	if err != nil {
		panic(err)
	}
	return s
}

package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type Callback struct {
	Protocol           string `json:"protocol"`
	Version            int    `json:"version"`
	EventID            string `json:"event_id"`
	EventType          string `json:"event_type"`
	CheckoutID         string `json:"checkout_id"`
	SubscriptionRef    string `json:"subscription_ref"`
	PlanID             string `json:"plan_id"`
	StateVersion       int64  `json:"state_version"`
	Status             string `json:"status"`
	CurrentPeriodStart string `json:"current_period_start"`
	CurrentPeriodEnd   string `json:"current_period_end"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	StateChangedAt     string `json:"state_changed_at"`
}

type Snapshot struct {
	Protocol           string `json:"protocol"`
	Version            int    `json:"version"`
	CheckoutID         string `json:"checkout_id"`
	SubscriptionRef    string `json:"subscription_ref"`
	PlanID             string `json:"plan_id"`
	StateVersion       int64  `json:"state_version"`
	Status             string `json:"status"`
	CurrentPeriodStart string `json:"current_period_start"`
	CurrentPeriodEnd   string `json:"current_period_end"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	StateChangedAt     string `json:"state_changed_at"`
}

type StartClaims struct {
	CheckoutID string
	PlanID     string
	ReturnURL  string
	Iat        int64
	Exp        int64
}

type PortalClaims struct {
	SubscriptionRef string
	ReturnURL       string
	Iat             int64
	Exp             int64
}

var callbackFields = []string{
	"protocol", "version", "event_id", "event_type", "checkout_id",
	"subscription_ref", "plan_id", "state_version", "status",
	"current_period_start", "current_period_end", "cancel_at_period_end",
	"state_changed_at",
}

var snapshotFields = []string{
	"protocol", "version", "checkout_id", "subscription_ref", "plan_id",
	"state_version", "status", "current_period_start", "current_period_end",
	"cancel_at_period_end", "state_changed_at",
}

var startFields = []string{"checkout_id", "plan_id", "return_url", "iat", "exp"}
var portalFields = []string{"subscription_ref", "return_url", "iat", "exp"}

// ParseStartClaims strictly decodes and validates a start-token payload.
func ParseStartClaims(raw []byte) (StartClaims, error) {
	var wire struct {
		CheckoutID *string      `json:"checkout_id"`
		PlanID     *string      `json:"plan_id"`
		ReturnURL  *string      `json:"return_url"`
		Iat        *json.Number `json:"iat"`
		Exp        *json.Number `json:"exp"`
	}
	if err := UnmarshalStrict(raw, &wire, startFields); err != nil {
		return StartClaims{}, err
	}
	iat, err := parseRequiredUnix(wire.Iat)
	if err != nil {
		return StartClaims{}, err
	}
	exp, err := parseRequiredUnix(wire.Exp)
	if err != nil {
		return StartClaims{}, err
	}
	if wire.CheckoutID == nil || wire.PlanID == nil || wire.ReturnURL == nil {
		return StartClaims{}, ErrMissingField
	}
	if err := ValidateCheckoutID(*wire.CheckoutID); err != nil {
		return StartClaims{}, err
	}
	if err := ValidatePlanID(*wire.PlanID); err != nil {
		return StartClaims{}, err
	}
	returnURL, err := NormalizeReturnURL(*wire.ReturnURL)
	if err != nil {
		return StartClaims{}, err
	}
	return StartClaims{
		CheckoutID: *wire.CheckoutID,
		PlanID:     *wire.PlanID,
		ReturnURL:  returnURL,
		Iat:        iat,
		Exp:        exp,
	}, nil
}

// ParsePortalClaims strictly decodes and validates a portal-token payload.
func ParsePortalClaims(raw []byte) (PortalClaims, error) {
	var wire struct {
		SubscriptionRef *string      `json:"subscription_ref"`
		ReturnURL       *string      `json:"return_url"`
		Iat             *json.Number `json:"iat"`
		Exp             *json.Number `json:"exp"`
	}
	if err := UnmarshalStrict(raw, &wire, portalFields); err != nil {
		return PortalClaims{}, err
	}
	iat, err := parseRequiredUnix(wire.Iat)
	if err != nil {
		return PortalClaims{}, err
	}
	exp, err := parseRequiredUnix(wire.Exp)
	if err != nil {
		return PortalClaims{}, err
	}
	if wire.SubscriptionRef == nil || wire.ReturnURL == nil {
		return PortalClaims{}, ErrMissingField
	}
	if err := ValidateSubscriptionRef(*wire.SubscriptionRef); err != nil {
		return PortalClaims{}, err
	}
	returnURL, err := NormalizeReturnURL(*wire.ReturnURL)
	if err != nil {
		return PortalClaims{}, err
	}
	return PortalClaims{
		SubscriptionRef: *wire.SubscriptionRef,
		ReturnURL:       returnURL,
		Iat:             iat,
		Exp:             exp,
	}, nil
}

// parseRequiredUnix requires a JSON number that is a canonical Unix timestamp.
func parseRequiredUnix(n *json.Number) (int64, error) {
	if n == nil {
		return 0, ErrMissingField
	}
	return ParseUnixSeconds(n.String())
}

// ParseCallback strictly decodes and validates an outbound callback body.
func ParseCallback(raw []byte) (Callback, error) {
	var cb Callback
	if err := UnmarshalStrict(raw, &cb, callbackFields); err != nil {
		return Callback{}, err
	}
	if err := cb.Validate(); err != nil {
		return Callback{}, err
	}
	return cb, nil
}

// ParseSnapshot strictly decodes and validates a reconciliation snapshot body.
func ParseSnapshot(raw []byte) (Snapshot, error) {
	var snap Snapshot
	if err := UnmarshalStrict(raw, &snap, snapshotFields); err != nil {
		return Snapshot{}, err
	}
	if err := snap.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// Validate checks identifier, timestamp, and event/status pairing rules.
func (c Callback) Validate() error {
	if c.Protocol != Name {
		return ErrInvalidProtocol
	}
	if c.Version != Version {
		return ErrUnsupportedVersion
	}
	if err := ValidateEventID(c.EventID); err != nil {
		return err
	}
	if err := ValidateCheckoutID(c.CheckoutID); err != nil {
		return err
	}
	if err := ValidateSubscriptionRef(c.SubscriptionRef); err != nil {
		return err
	}
	if err := ValidatePlanID(c.PlanID); err != nil {
		return err
	}
	if c.StateVersion < 1 {
		return fmt.Errorf("%w: state_version", ErrInvalidJSON)
	}
	start, err := ParseUTC(c.CurrentPeriodStart)
	if err != nil {
		return err
	}
	end, err := ParseUTC(c.CurrentPeriodEnd)
	if err != nil {
		return err
	}
	if !end.After(start) {
		return ErrUnorderedPeriod
	}
	if _, err := ParseUTC(c.StateChangedAt); err != nil {
		return err
	}
	return ValidateEventStatus(c.EventType, c.Status, c.CancelAtPeriodEnd)
}

// Validate checks identifier, timestamp, and status flag rules.
func (s Snapshot) Validate() error {
	if s.Protocol != Name {
		return ErrInvalidProtocol
	}
	if s.Version != Version {
		return ErrUnsupportedVersion
	}
	if err := ValidateCheckoutID(s.CheckoutID); err != nil {
		return err
	}
	if err := ValidateSubscriptionRef(s.SubscriptionRef); err != nil {
		return err
	}
	if err := ValidatePlanID(s.PlanID); err != nil {
		return err
	}
	if s.StateVersion < 1 {
		return fmt.Errorf("%w: state_version", ErrInvalidJSON)
	}
	start, err := ParseUTC(s.CurrentPeriodStart)
	if err != nil {
		return err
	}
	end, err := ParseUTC(s.CurrentPeriodEnd)
	if err != nil {
		return err
	}
	if !end.After(start) {
		return ErrUnorderedPeriod
	}
	if _, err := ParseUTC(s.StateChangedAt); err != nil {
		return err
	}
	return ValidateStatusFlags(s.Status, s.CancelAtPeriodEnd)
}

// ValidateEventStatus enforces the allowed event_type and status combinations.
func ValidateEventStatus(eventType, status string, cancelAtPeriodEnd bool) error {
	if err := ValidateStatusFlags(status, cancelAtPeriodEnd); err != nil {
		return err
	}
	ok := false
	switch eventType {
	case EventActivated:
		ok = status == StatusActive || status == StatusTrialing
	case EventRenewed:
		ok = status == StatusActive
	case EventPastDue:
		ok = status == StatusPastDue
	case EventCanceled:
		ok = status == StatusCanceled
	case EventExpired:
		ok = status == StatusExpired
	case EventPlanChanged:
		ok = status == StatusActive
	case EventSync:
		return fmt.Errorf("%w: subscription.sync is not a callback pair", ErrInvalidEventStatus)
	default:
		return ErrInvalidEventType
	}
	if !ok {
		return ErrInvalidEventStatus
	}
	return nil
}

// ValidateStatusFlags requires cancel_at_period_end only for canceled status.
func ValidateStatusFlags(status string, cancelAtPeriodEnd bool) error {
	switch status {
	case StatusActive, StatusTrialing, StatusPastDue:
		if cancelAtPeriodEnd {
			return ErrCancelFlagMismatch
		}
	case StatusCanceled:
		if !cancelAtPeriodEnd {
			return ErrCancelFlagMismatch
		}
	case StatusExpired:
		if cancelAtPeriodEnd {
			return ErrCancelFlagMismatch
		}
	default:
		return ErrInvalidStatus
	}
	return nil
}

// SnapshotFromCallback projects a callback into the consumer snapshot shape.
func SnapshotFromCallback(cb Callback) Snapshot {
	return Snapshot{
		Protocol:           cb.Protocol,
		Version:            cb.Version,
		CheckoutID:         cb.CheckoutID,
		SubscriptionRef:    cb.SubscriptionRef,
		PlanID:             cb.PlanID,
		StateVersion:       cb.StateVersion,
		Status:             cb.Status,
		CurrentPeriodStart: cb.CurrentPeriodStart,
		CurrentPeriodEnd:   cb.CurrentPeriodEnd,
		CancelAtPeriodEnd:  cb.CancelAtPeriodEnd,
		StateChangedAt:     cb.StateChangedAt,
	}
}

// NewCallback builds and validates a consumer-visible callback.
func NewCallback(eventID, eventType, checkoutID, subscriptionRef, planID string, version int64, status string, periodStart, periodEnd, changedAt time.Time, cancelAtPeriodEnd bool) (Callback, error) {
	cb := Callback{
		Protocol:           Name,
		Version:            Version,
		EventID:            eventID,
		EventType:          eventType,
		CheckoutID:         checkoutID,
		SubscriptionRef:    subscriptionRef,
		PlanID:             planID,
		StateVersion:       version,
		Status:             status,
		CurrentPeriodStart: FormatUTC(periodStart),
		CurrentPeriodEnd:   FormatUTC(periodEnd),
		CancelAtPeriodEnd:  cancelAtPeriodEnd,
		StateChangedAt:     FormatUTC(changedAt),
	}
	if err := cb.Validate(); err != nil {
		return Callback{}, err
	}
	return cb, nil
}

// MustInt64 formats a JSON number as a decimal string.
func MustInt64(n json.Number) string {
	return strconv.FormatInt(mustParse(n), 10)
}

// mustParse converts a JSON number to int64, returning 0 on failure.
func mustParse(n json.Number) int64 {
	v, err := n.Int64()
	if err != nil {
		return 0
	}
	return v
}

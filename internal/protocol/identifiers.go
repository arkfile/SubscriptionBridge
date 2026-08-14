package protocol

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var identifierSuffix = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateOpaqueID checks prefix, ASCII suffix charset, and the 160-character limit.
func ValidateOpaqueID(value, prefix string) error {
	if value == "" || !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%w: expected %s prefix", ErrInvalidIdentifier, prefix)
	}
	if len(value) > MaxIdentifierLength {
		return fmt.Errorf("%w: exceeds %d characters", ErrInvalidIdentifier, MaxIdentifierLength)
	}
	for i := 0; i < len(value); i++ {
		if value[i] > 127 {
			return fmt.Errorf("%w: non-ascii", ErrInvalidIdentifier)
		}
	}
	suffix := value[len(prefix):]
	if suffix == "" || !identifierSuffix.MatchString(suffix) {
		return fmt.Errorf("%w: invalid suffix", ErrInvalidIdentifier)
	}
	return nil
}

// ValidateCheckoutID requires a subchk_ opaque identifier.
func ValidateCheckoutID(id string) error {
	return ValidateOpaqueID(id, CheckoutPrefix)
}

// ValidateSubscriptionRef requires a sub_ opaque identifier.
func ValidateSubscriptionRef(id string) error {
	return ValidateOpaqueID(id, SubscriptionPrefix)
}

// ValidateEventID requires an evt_ opaque identifier.
func ValidateEventID(id string) error {
	return ValidateOpaqueID(id, EventPrefix)
}

// ValidatePlanID enforces nonempty UTF-8 plan IDs at most 128 bytes.
func ValidatePlanID(planID string) error {
	if planID == "" || !utf8.ValidString(planID) {
		return ErrInvalidPlanID
	}
	if strings.TrimFunc(planID, unicode.IsSpace) == "" {
		return ErrInvalidPlanID
	}
	if len(planID) > MaxPlanIDBytes {
		return ErrInvalidPlanID
	}
	return nil
}

// RejectIdentityFields fails closed if a protocol object contains consumer-identity keys.
func RejectIdentityFields(fields map[string]struct{}) error {
	forbidden := []string{
		"username", "user_name", "user", "account_id", "account", "email",
		"customer_email", "user_id", "consumer_id", "tenant", "tenant_id",
	}
	for _, name := range forbidden {
		if _, ok := fields[name]; ok {
			return fmt.Errorf("%w: %s", ErrIdentityField, name)
		}
	}
	return nil
}

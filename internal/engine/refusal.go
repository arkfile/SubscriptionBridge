package engine

import "strings"

// IsRetryableRefusal reports whether an Adyen refusal may be retried automatically.
func IsRetryableRefusal(code string) bool {
	c := strings.ToUpper(strings.TrimSpace(code))
	c = strings.ReplaceAll(c, " ", "_")
	switch c {
	case "STOLEN", "INVALID", "CLOSED", "REVOKED",
		"STOLEN_CARD", "INVALID_CARD", "INVALID_CARD_NUMBER",
		"EXPIRED_CARD", "RESTRICTED_CARD", "BLOCKED_CARD",
		"6", "8", "5", "25":
		return false
	default:
		return true
	}
}

package engine_test

import (
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/engine"
)

// TestRefusalPolicy classifies stolen/invalid/closed/revoked codes as non-retryable.
func TestRefusalPolicy(t *testing.T) {
	if engine.IsRetryableRefusal("STOLEN") || engine.IsRetryableRefusal("8") || engine.IsRetryableRefusal("expired_card") {
		t.Fatal("expected non-retryable")
	}
	if !engine.IsRetryableRefusal("12") || !engine.IsRetryableRefusal("") {
		t.Fatal("expected retryable")
	}
}

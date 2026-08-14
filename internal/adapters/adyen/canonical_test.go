package adyen_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
)

// TestCanonicalPaymentBody locks the exact ContAuth JSON byte layout.
func TestCanonicalPaymentBody(t *testing.T) {
	got := adyen.CanonicalPaymentBody("ExampleMerchant", "usd", "sba_550e8400-e29b-41d4-a716-446655440000", "sbr_7f3a9c2e", "tok_secret", 500)
	want := `{"merchantAccount":"ExampleMerchant","amount":{"value":500,"currency":"USD"},"reference":"sba_550e8400-e29b-41d4-a716-446655440000","shopperReference":"sbr_7f3a9c2e","shopperInteraction":"ContAuth","recurringProcessingModel":"Subscription","storedPaymentMethodId":"tok_secret"}`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) == "" {
		t.Fatal("fingerprint")
	}
}

package stripe_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/adapters/stripe"
)

// TestStripeWebhookMapping rejects invalid Stripe signatures before mapping events.
func TestStripeWebhookMapping(t *testing.T) {
	ad := stripe.New("sk_test", "whsec_test")
	body := []byte(`{"id":"evt_1","type":"customer.subscription.updated","data":{"object":{"id":"sub_1","customer":"cus_1","status":"active"}}}`)
	if _, err := ad.ParseWebhook(context.Background(), http.Header{"Stripe-Signature": []string{"t=1,v1=00"}}, body); err == nil {
		t.Fatal("expected signature failure")
	}
}

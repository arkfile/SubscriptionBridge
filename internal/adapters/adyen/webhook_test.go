package adyen_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/adapters"
	"github.com/arkfile/SubscriptionBridge/internal/adapters/adyen"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

const testHMACKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// signAdyenItem computes the notification HMAC over the canonical field list.
func signAdyenItem(t *testing.T, item map[string]any) string {
	t.Helper()
	key, err := hex.DecodeString(testHMACKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := item["amount"].(map[string]any)
	payload := stringifyTest(item["pspReference"]) + ":" + stringifyTest(item["originalReference"]) + ":" +
		stringifyTest(item["merchantAccountCode"]) + ":" + stringifyTest(item["merchantReference"]) + ":" +
		stringifyTest(amount["value"]) + ":" + stringifyTest(amount["currency"]) + ":" +
		stringifyTest(item["eventCode"]) + ":" + stringifyTest(item["success"])
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// stringifyTest renders Adyen JSON scalars for the test HMAC payload.
func stringifyTest(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

// TestParseWebhookPaymentUpdateUsesCheckoutMetadata maps sbpm_ merchant refs via metadata.checkout_id.
func TestParseWebhookPaymentUpdateUsesCheckoutMetadata(t *testing.T) {
	item := map[string]any{
		"pspReference":        "psp_pm_1",
		"originalReference":   "",
		"merchantAccountCode": "ExampleMerchant",
		"merchantReference":   "sbpm_550e8400-e29b-41d4-a716-446655440000",
		"amount":              map[string]any{"value": 0, "currency": "USD"},
		"eventCode":           "AUTHORISATION",
		"success":             "true",
	}
	item["additionalData"] = map[string]any{
		"hmacSignature":                      signAdyenItem(t, item),
		"metadata.checkout_id":               "subchk_7f3a9c2e",
		"recurring.recurringDetailReference": "tok_replaced",
	}
	body, err := json.Marshal(map[string]any{
		"notificationItems": []map[string]any{
			{"NotificationRequestItem": item},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ad := adyen.New("api", testHMACKeyHex, "test", "")
	events, err := ad.ParseWebhook(context.Background(), nil, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events %d", len(events))
	}
	ev := events[0]
	if ev.NormalizedKind != adapters.KindAdyenInitialAuthorisation {
		t.Fatalf("kind %s", ev.NormalizedKind)
	}
	if ev.Fields["checkout_id"] != "subchk_7f3a9c2e" {
		t.Fatalf("checkout_id %+v", ev.Fields["checkout_id"])
	}
	if _, ok := ev.Fields["attempt_reference"]; ok {
		t.Fatal("payment update must not be a renewal")
	}
	if string(ev.SensitivePlain) != "tok_replaced" {
		t.Fatalf("token %q", ev.SensitivePlain)
	}
	if protocol.ValidateCheckoutID(ev.Fields["checkout_id"].(string)) != nil {
		t.Fatal("checkout_id must remain a protocol identifier")
	}
}

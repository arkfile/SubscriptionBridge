package protocol_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestIdentifiers checks opaque ID prefixes, charset, and length.
func TestIdentifiers(t *testing.T) {
	if err := protocol.ValidateCheckoutID("subchk_7f3a9c2e"); err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateSubscriptionRef("sub_a8f3c1d2"); err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateEventID("evt_550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatal(err)
	}
	long := "subchk_" + strings.Repeat("a", 153)
	if err := protocol.ValidateCheckoutID(long); err != nil {
		t.Fatalf("160-char id should pass: %v", err)
	}
	if err := protocol.ValidateCheckoutID(long + "a"); err == nil {
		t.Fatal("161-char id should fail")
	}
	for _, bad := range []string{"", "subchk_", "subchk_.", "chk_abc", "subchk_abc!", "subchk_abc/def"} {
		if err := protocol.ValidateCheckoutID(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// TestPlanID rejects empty, whitespace-only, and oversized plan IDs.
func TestPlanID(t *testing.T) {
	if err := protocol.ValidatePlanID("plan_500gb"); err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidatePlanID(" "); err == nil {
		t.Fatal("whitespace plan")
	}
	if err := protocol.ValidatePlanID(strings.Repeat("p", 129)); err == nil {
		t.Fatal("overlong plan")
	}
	if !utf8.ValidString("plan_500gb") {
		t.Fatal("fixture plan should be utf8")
	}
}

// TestJSONRejectsUnknownDuplicateTrailing checks strict JSON decoding.
func TestJSONRejectsUnknownDuplicateTrailing(t *testing.T) {
	fx := testdata.Load(t)
	if _, err := protocol.ParseCallback([]byte(fx.Callback.BodyJSONUTF8)); err != nil {
		t.Fatal(err)
	}
	unknown := []byte(`{"protocol":"subscription-bridge","version":1,"event_id":"evt_550e8400-e29b-41d4-a716-446655440000","event_type":"subscription.activated","checkout_id":"subchk_7f3a9c2e","subscription_ref":"sub_a8f3c1d2","plan_id":"plan_500gb","state_version":1,"status":"active","current_period_start":"2026-01-01T00:00:00Z","current_period_end":"2026-02-01T00:00:00Z","cancel_at_period_end":false,"state_changed_at":"2026-01-01T00:00:00Z","extra":1}`)
	if _, err := protocol.ParseCallback(unknown); err == nil {
		t.Fatal("unknown field")
	}
	dup := []byte(`{"protocol":"subscription-bridge","protocol":"subscription-bridge","version":1,"event_id":"evt_550e8400-e29b-41d4-a716-446655440000","event_type":"subscription.activated","checkout_id":"subchk_7f3a9c2e","subscription_ref":"sub_a8f3c1d2","plan_id":"plan_500gb","state_version":1,"status":"active","current_period_start":"2026-01-01T00:00:00Z","current_period_end":"2026-02-01T00:00:00Z","cancel_at_period_end":false,"state_changed_at":"2026-01-01T00:00:00Z"}`)
	if _, err := protocol.ParseCallback(dup); err == nil {
		t.Fatal("duplicate field")
	}
	trail := append([]byte(fx.Callback.BodyJSONUTF8), []byte(`{}`)...)
	if _, err := protocol.ParseCallback(trail); err == nil {
		t.Fatal("trailing json")
	}
}

// TestEventStatusMatrix checks allowed event_type and status pairs.
func TestEventStatusMatrix(t *testing.T) {
	ok := [][2]string{
		{protocol.EventActivated, protocol.StatusActive},
		{protocol.EventActivated, protocol.StatusTrialing},
		{protocol.EventRenewed, protocol.StatusActive},
		{protocol.EventPastDue, protocol.StatusPastDue},
		{protocol.EventCanceled, protocol.StatusCanceled},
		{protocol.EventExpired, protocol.StatusExpired},
		{protocol.EventPlanChanged, protocol.StatusActive},
	}
	for _, pair := range ok {
		cancel := pair[1] == protocol.StatusCanceled
		if err := protocol.ValidateEventStatus(pair[0], pair[1], cancel); err != nil {
			t.Fatalf("%s/%s: %v", pair[0], pair[1], err)
		}
	}
	if err := protocol.ValidateEventStatus(protocol.EventActivated, protocol.StatusExpired, false); err == nil {
		t.Fatal("activated/expired")
	}
	if err := protocol.ValidateEventStatus(protocol.EventSync, protocol.StatusActive, false); err == nil {
		t.Fatal("sync is not a callback")
	}
	if err := protocol.ValidateStatusFlags(protocol.StatusCanceled, false); err == nil {
		t.Fatal("canceled requires cancel_at_period_end")
	}
	if err := protocol.ValidateStatusFlags(protocol.StatusExpired, true); err == nil {
		t.Fatal("expired forbids cancel_at_period_end")
	}
}

// TestCalendarMonths checks month-end clamping for calendar periods.
func TestCalendarMonths(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	feb := protocol.AddCalendarMonths(jan31, 1)
	if feb.Day() != 28 || feb.Month() != time.February {
		t.Fatalf("jan31+1m = %s", feb)
	}
	mar := protocol.AddCalendarMonths(jan31, 2)
	if mar.Day() != 31 || mar.Month() != time.March {
		t.Fatalf("jan31+2m = %s", mar)
	}
	leap := protocol.AddCalendarMonths(time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC), 1)
	if leap.Day() != 29 {
		t.Fatalf("leap feb = %s", leap)
	}
}

// TestActionKeyStable checks scheduled-action keys are stable for the same inputs.
func TestActionKeyStable(t *testing.T) {
	target := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	a := protocol.ActionKey("sub_a8f3c1d2", protocol.ActionRenew, target)
	b := protocol.ActionKey("sub_a8f3c1d2", protocol.ActionRenew, target.Add(500*time.Millisecond))
	if a != b {
		t.Fatal("action key must truncate to seconds")
	}
	if !strings.HasPrefix(a, "act_") {
		t.Fatalf("prefix %s", a)
	}
	c := protocol.ActionKey("sub_a8f3c1d2", protocol.ActionExpire, target)
	if a == c {
		t.Fatal("renew and expire keys must differ")
	}
}

// TestReturnURLNormalization accepts HTTPS and loopback HTTP return URLs.
func TestReturnURLNormalization(t *testing.T) {
	got, err := protocol.NormalizeReturnURL("https://APP.Example.COM:443/billing/return?b=2&a=1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://app.example.com/billing/return?b=2&a=1" {
		t.Fatalf("got %s", got)
	}
	if _, err := protocol.NormalizeReturnURL("http://example.com/x"); err == nil {
		t.Fatal("non-loopback http")
	}
	if _, err := protocol.NormalizeReturnURL("http://127.0.0.1:8080/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.NormalizeReturnURL("https://user:pass@example.com/x"); err == nil {
		t.Fatal("userinfo")
	}
	if _, err := protocol.NormalizeReturnURL("https://example.com/x#frag"); err == nil {
		t.Fatal("fragment")
	}
}

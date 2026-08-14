package hmac_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestFixtureDerivationAndSignatures matches HKDF and HMAC golden vectors from the fixture.
func TestFixtureDerivationAndSignatures(t *testing.T) {
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	if hmac.KeyHex(keys.Token) != fx.PairingRoot.Keys.Token.DerivedKeyHex {
		t.Fatalf("token key\n got %s\nwant %s", hmac.KeyHex(keys.Token), fx.PairingRoot.Keys.Token.DerivedKeyHex)
	}
	if hmac.KeyHex(keys.Callback) != fx.PairingRoot.Keys.Callback.DerivedKeyHex {
		t.Fatalf("callback key mismatch")
	}
	if hmac.KeyHex(keys.Reconcile) != fx.PairingRoot.Keys.Reconcile.DerivedKeyHex {
		t.Fatalf("reconcile key mismatch")
	}
	if fx.PairingRoot.SaltASCII != protocol.HKDFSalt {
		t.Fatalf("salt %q", fx.PairingRoot.SaltASCII)
	}
	if fx.PairingRoot.Keys.Token.InfoASCII != protocol.TokenInfo {
		t.Fatal("token info")
	}
	if fx.PairingRoot.Keys.Callback.InfoASCII != protocol.CallbackInfo {
		t.Fatal("callback info")
	}
	if fx.PairingRoot.Keys.Reconcile.InfoASCII != protocol.ReconcileInfo {
		t.Fatal("reconcile info")
	}

	gotStart, err := hmac.EncodeToken(keys.Token, []byte(fx.StartToken.PayloadJSONUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if gotStart != fx.StartToken.Token {
		t.Fatalf("start token\n got %s\nwant %s", gotStart, fx.StartToken.Token)
	}
	if base64.RawURLEncoding.EncodeToString([]byte(fx.StartToken.PayloadJSONUTF8)) != fx.StartToken.PayloadBase64URL {
		t.Fatal("start payload b64")
	}

	gotPortal, err := hmac.EncodeToken(keys.Token, []byte(fx.PortalToken.PayloadJSONUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if gotPortal != fx.PortalToken.Token {
		t.Fatalf("portal token mismatch")
	}

	now := time.Unix(fx.Callback.SignatureTimestamp, 0).UTC()
	start, _, err := hmac.ParseStartToken(keys.Token, fx.StartToken.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	if start.CheckoutID != "subchk_7f3a9c2e" || start.PlanID != "plan_500gb" {
		t.Fatalf("start claims %+v", start)
	}
	portal, _, err := hmac.ParsePortalToken(keys.Token, fx.PortalToken.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	if portal.SubscriptionRef != "sub_a8f3c1d2" {
		t.Fatalf("portal claims %+v", portal)
	}

	header, err := hmac.CallbackHeader(keys.Callback, fx.Callback.SignatureTimestamp, []byte(fx.Callback.BodyJSONUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if header != fx.Callback.SignatureHeader {
		t.Fatalf("callback header\n got %s\nwant %s", header, fx.Callback.SignatureHeader)
	}
	if err := hmac.VerifyCallbackHeader(keys.Callback, fx.Callback.SignatureHeader, []byte(fx.Callback.BodyJSONUTF8), now); err != nil {
		t.Fatal(err)
	}
	cb, err := protocol.ParseCallback([]byte(fx.Callback.BodyJSONUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if cb.EventType != protocol.EventActivated {
		t.Fatalf("event %s", cb.EventType)
	}

	auth, err := hmac.ReconcileHeader(keys.Reconcile, fx.ReconciliationRequest.SignatureTimestamp, fx.ReconciliationRequest.Path)
	if err != nil {
		t.Fatal(err)
	}
	if auth != fx.ReconciliationRequest.AuthorizationHeader {
		t.Fatalf("reconcile header\n got %s\nwant %s", auth, fx.ReconciliationRequest.AuthorizationHeader)
	}
	if err := hmac.VerifyReconcileHeader(keys.Reconcile, fx.ReconciliationRequest.AuthorizationHeader, fx.ReconciliationRequest.Path, now); err != nil {
		t.Fatal(err)
	}

	snap, err := protocol.ParseSnapshot([]byte(fx.Snapshot.BodyJSONUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != protocol.StatusActive || fx.Snapshot.ResponseIsIndependentlySigned {
		t.Fatalf("snapshot %+v signed=%v", snap, fx.Snapshot.ResponseIsIndependentlySigned)
	}
	if !bytes.Equal([]byte(fx.Callback.SignatureBaseUTF8), []byte("1767225605."+fx.Callback.BodyJSONUTF8)) {
		t.Fatal("callback signature base")
	}
}

// TestPairingRootRejection rejects uppercase, wrong-length, and non-hex pairing roots.
func TestPairingRootRejection(t *testing.T) {
	fx := testdata.Load(t)
	cases := []string{
		"",
		"00",
		strings.ToUpper(fx.PairingRoot.ConfiguredHex),
		fx.PairingRoot.ConfiguredHex + "00",
		"  " + fx.PairingRoot.ConfiguredHex,
		"0x" + fx.PairingRoot.ConfiguredHex,
		strings.Replace(fx.PairingRoot.ConfiguredHex, "a", "g", 1),
		strings.Replace(fx.PairingRoot.ConfiguredHex, "a", "A", 1),
	}
	for _, c := range cases {
		if _, err := hmac.DeriveKeys(c); err == nil {
			t.Fatalf("accepted %q", c)
		}
	}
}

// TestTokenRejectsNonCanonical rejects padded or otherwise non-canonical tokens.
func TestTokenRejectsNonCanonical(t *testing.T) {
	fx := testdata.Load(t)
	keys, err := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1767225605, 0).UTC()
	padded := fx.StartToken.PayloadBase64URL + "=" + "." + fx.StartToken.SignatureHex
	if _, _, err := hmac.ParseStartToken(keys.Token, padded, now); err == nil {
		t.Fatal("accepted padded token")
	}
	upper := fx.StartToken.PayloadBase64URL + "." + strings.ToUpper(fx.StartToken.SignatureHex)
	if _, _, err := hmac.ParseStartToken(keys.Token, upper, now); err == nil {
		t.Fatal("accepted uppercase signature")
	}
	if _, _, err := hmac.ParseStartToken(keys.Token, " "+fx.StartToken.Token, now); err == nil {
		t.Fatal("accepted whitespace")
	}
}

// TestHeaderRejectsNonCanonical rejects malformed callback and reconcile headers.
func TestHeaderRejectsNonCanonical(t *testing.T) {
	fx := testdata.Load(t)
	keys, _ := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	now := time.Unix(fx.Callback.SignatureTimestamp, 0).UTC()
	body := []byte(fx.Callback.BodyJSONUTF8)
	bad := []string{
		"v1=" + fx.Callback.SignatureHex + ",t=1767225605",
		"t=1767225605, v1=" + fx.Callback.SignatureHex,
		"t=01767225605,v1=" + fx.Callback.SignatureHex,
		"t=1767225605,v1=" + strings.ToUpper(fx.Callback.SignatureHex),
		"t=1767225605,v1=" + fx.Callback.SignatureHex + ",v1=" + fx.Callback.SignatureHex,
		"Bearer t=1767225605,v1=" + fx.Callback.SignatureHex,
	}
	for _, h := range bad {
		if err := hmac.VerifyCallbackHeader(keys.Callback, h, body, now); err == nil {
			t.Fatalf("accepted %q", h)
		}
	}
}

// TestTokenLifetimeBoundaries checks iat/exp TTL and clock-skew limits.
func TestTokenLifetimeBoundaries(t *testing.T) {
	fx := testdata.Load(t)
	keys, _ := hmac.DeriveKeys(fx.PairingRoot.ConfiguredHex)
	iat := int64(1767225600)
	exp := int64(1767226500)
	payload := []byte(`{"checkout_id":"subchk_7f3a9c2e","plan_id":"plan_500gb","return_url":"https://app.example.com/billing/return","iat":1767225600,"exp":1767226500}`)
	token, err := hmac.EncodeToken(keys.Token, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hmac.ParseStartToken(keys.Token, token, time.Unix(exp+300, 0).UTC()); err != nil {
		t.Fatalf("exp+skew should pass: %v", err)
	}
	if _, _, err := hmac.ParseStartToken(keys.Token, token, time.Unix(exp+301, 0).UTC()); err == nil {
		t.Fatal("exp+skew+1 should fail")
	}
	if _, _, err := hmac.ParseStartToken(keys.Token, token, time.Unix(iat-300, 0).UTC()); err != nil {
		t.Fatalf("iat-skew should pass: %v", err)
	}
	if protocol.ValidateTokenLifetime(iat, iat+901, time.Unix(iat, 0)) == nil {
		t.Fatal("ttl > 900s should fail")
	}
	if protocol.ValidateTokenLifetime(iat+301, iat+901, time.Unix(iat, 0)) == nil {
		t.Fatal("future iat beyond skew should fail")
	}
}

package envelope_test

import (
	"bytes"
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/envelope"
)

// TestSealOpenRoundTrip encrypts and decrypts with matching AAD.
func TestSealOpenRoundTrip(t *testing.T) {
	box, err := envelope.New("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	aad := envelope.AAD("payment-method", "sub_a8f3c1d2")
	ct, nonce, ver, err := box.Seal([]byte("tok_secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if ver != envelope.VersionV1 || len(nonce) != 12 {
		t.Fatalf("ver=%s nonce=%d", ver, len(nonce))
	}
	plain, err := box.Open(ct, nonce, aad, ver)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, []byte("tok_secret")) {
		t.Fatalf("plain %q", plain)
	}
	if _, err := box.Open(ct, nonce, envelope.AAD("payment-method", "other"), ver); err == nil {
		t.Fatal("aad mismatch should fail")
	}
}

// TestRejectsInvalidKey rejects a non-hex data-encryption key.
func TestRejectsInvalidKey(t *testing.T) {
	if _, err := envelope.New("not-hex"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := envelope.New("000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F"); err == nil {
		t.Fatal("uppercase hex")
	}
}

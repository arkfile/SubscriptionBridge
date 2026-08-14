package hmac

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

type Keys struct {
	Token     []byte
	Callback  []byte
	Reconcile []byte
}

// ParsePairingRoot accepts exactly 64 lowercase hex characters and returns 32 bytes.
func ParsePairingRoot(configured string) ([]byte, error) {
	if len(configured) != 64 {
		return nil, protocol.ErrInvalidPairingRoot
	}
	for i := 0; i < len(configured); i++ {
		c := configured[i]
		if c >= 'A' && c <= 'F' {
			return nil, protocol.ErrInvalidPairingRoot
		}
		if !isLowerHex(c) {
			return nil, protocol.ErrInvalidPairingRoot
		}
	}
	decoded, err := hex.DecodeString(configured)
	if err != nil || len(decoded) != protocol.KeySize {
		return nil, protocol.ErrInvalidPairingRoot
	}
	return decoded, nil
}

// isLowerHex reports whether c is 0-9 or a-f.
func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// DeriveKeys HKDF-SHA256-derives the token, callback, and reconcile keys from the pairing root.
func DeriveKeys(configuredRoot string) (Keys, error) {
	root, err := ParsePairingRoot(configuredRoot)
	if err != nil {
		return Keys{}, err
	}
	token, err := hkdf.Key(sha256.New, root, []byte(protocol.HKDFSalt), protocol.TokenInfo, protocol.KeySize)
	if err != nil {
		return Keys{}, err
	}
	callback, err := hkdf.Key(sha256.New, root, []byte(protocol.HKDFSalt), protocol.CallbackInfo, protocol.KeySize)
	if err != nil {
		return Keys{}, err
	}
	reconcile, err := hkdf.Key(sha256.New, root, []byte(protocol.HKDFSalt), protocol.ReconcileInfo, protocol.KeySize)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Token: token, Callback: callback, Reconcile: reconcile}, nil
}

// Sign returns a lowercase hex HMAC-SHA256 over message.
func Sign(key, message []byte) string {
	mac := sha256HMAC(key, message)
	return hex.EncodeToString(mac)
}

// Verify constant-time-compares a lowercase hex HMAC against message.
func Verify(key, message []byte, signatureHex string) error {
	if len(signatureHex) != protocol.SignatureHexLen || strings.ToLower(signatureHex) != signatureHex {
		return protocol.ErrInvalidSignature
	}
	for i := 0; i < len(signatureHex); i++ {
		if !isLowerHex(signatureHex[i]) {
			return protocol.ErrInvalidSignature
		}
	}
	expected, err := hex.DecodeString(signatureHex)
	if err != nil {
		return protocol.ErrInvalidSignature
	}
	got := sha256HMAC(key, message)
	if !constantEq(expected, got) {
		return protocol.ErrInvalidSignature
	}
	return nil
}

// sha256HMAC computes HMAC-SHA256.
func sha256HMAC(key, message []byte) []byte {
	return hmacSHA256(key, message)
}

// KeyHex encodes a derived key as lowercase hex for golden-vector tests.
func KeyHex(key []byte) string {
	return hex.EncodeToString(key)
}

// ValidateSingleRoot checks that the configured pairing root is a valid v1 root.
func ValidateSingleRoot(configured string) error {
	_, err := ParsePairingRoot(configured)
	return err
}

// QA / TODO: panics on invalid root; startup/test helper, not request path.
func MustDerive(configuredRoot string) Keys {
	keys, err := DeriveKeys(configuredRoot)
	if err != nil {
		panic(fmt.Sprintf("pairing root: %v", err))
	}
	return keys
}

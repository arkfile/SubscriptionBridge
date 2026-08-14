package hmac

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

// EncodeToken concatenates unpadded base64url payload and lowercase hex HMAC.
func EncodeToken(key, payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", protocol.ErrInvalidToken
	}
	sig := Sign(key, payload)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + sig
	if len(token) > protocol.MaxTokenBytes {
		return "", protocol.ErrInvalidToken
	}
	return token, nil
}

// SplitToken splits a token on the single required dot separator.
func SplitToken(token string) (payload, sig string, err error) {
	if token == "" || len(token) > protocol.MaxTokenBytes {
		return "", "", protocol.ErrInvalidToken
	}
	if strings.Count(token, ".") != 1 {
		return "", "", protocol.ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if parts[0] == "" || parts[1] == "" {
		return "", "", protocol.ErrInvalidToken
	}
	return parts[0], parts[1], nil
}

// DecodeAndVerify checks token grammar, canonical encoding, and HMAC.
func DecodeAndVerify(key []byte, token string) ([]byte, error) {
	payloadB64, sig, err := SplitToken(token)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(payloadB64, "= \t\r\n") {
		return nil, protocol.ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, protocol.ErrInvalidToken
	}
	if base64.RawURLEncoding.EncodeToString(payload) != payloadB64 {
		return nil, protocol.ErrInvalidToken
	}
	if err := Verify(key, payload, sig); err != nil {
		return nil, err
	}
	return payload, nil
}

// ParseStartToken verifies a start token and its lifetime claims.
func ParseStartToken(key []byte, token string, now time.Time) (protocol.StartClaims, []byte, error) {
	payload, err := DecodeAndVerify(key, token)
	if err != nil {
		return protocol.StartClaims{}, nil, err
	}
	claims, err := protocol.ParseStartClaims(payload)
	if err != nil {
		return protocol.StartClaims{}, nil, err
	}
	if err := protocol.ValidateTokenLifetime(claims.Iat, claims.Exp, now); err != nil {
		return protocol.StartClaims{}, nil, err
	}
	return claims, payload, nil
}

// ParsePortalToken verifies a portal token and its lifetime claims.
func ParsePortalToken(key []byte, token string, now time.Time) (protocol.PortalClaims, []byte, error) {
	payload, err := DecodeAndVerify(key, token)
	if err != nil {
		return protocol.PortalClaims{}, nil, err
	}
	claims, err := protocol.ParsePortalClaims(payload)
	if err != nil {
		return protocol.PortalClaims{}, nil, err
	}
	if err := protocol.ValidateTokenLifetime(claims.Iat, claims.Exp, now); err != nil {
		return protocol.PortalClaims{}, nil, err
	}
	return claims, payload, nil
}

// Fingerprint is the SHA-256 of a start-token payload used to bind checkout reuse.
func Fingerprint(payload []byte) [32]byte {
	sum := sha256Sum(payload)
	return sum
}

// sha256Sum hashes b with SHA-256.
func sha256Sum(b []byte) [32]byte {
	return sha256Fixed(b)
}

// SignStartPayload encodes a start-token payload.
func SignStartPayload(key []byte, payload []byte) (string, error) {
	return EncodeToken(key, payload)
}

// TokenError wraps a token verification error.
func TokenError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w", err)
}

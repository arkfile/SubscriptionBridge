package hmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
)

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return mac.Sum(nil)
}

// constantEq compares equal-length slices in constant time.
func constantEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

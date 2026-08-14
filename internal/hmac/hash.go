package hmac

import "crypto/sha256"

// sha256Fixed returns SHA-256 of b as a fixed array.
func sha256Fixed(b []byte) [32]byte {
	return sha256.Sum256(b)
}

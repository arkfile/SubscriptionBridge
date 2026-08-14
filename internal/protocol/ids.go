package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// NewOpaqueID generates a prefixed UUID-shaped opaque identifier.
func NewOpaqueID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := prefix + formatUUID(b)
	if err := ValidateOpaqueID(id, prefix); err != nil {
		return "", err
	}
	return id, nil
}

// formatUUID renders 16 bytes as hyphenated lowercase hex.
func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// NewEventID allocates an evt_ identifier.
func NewEventID() (string, error) {
	return NewOpaqueID(EventPrefix)
}

// NewSubscriptionRef allocates a sub_ identifier.
func NewSubscriptionRef() (string, error) {
	return NewOpaqueID(SubscriptionPrefix)
}

// NewShopperReference allocates a bridge-generated Adyen shopper reference.
func NewShopperReference() (string, error) {
	return NewOpaqueID(ShopperPrefix)
}

// NewAttemptReference allocates a stable Adyen charge-attempt reference.
func NewAttemptReference() (string, error) {
	return NewOpaqueID(AttemptPrefix)
}

// NewPaymentUpdateReference allocates an Adyen merchant reference for a portal payment-method update.
func NewPaymentUpdateReference() (string, error) {
	return NewOpaqueID(PaymentUpdatePrefix)
}

// RandomClaimToken returns a UUID-shaped random claim token.
func RandomClaimToken() ([16]byte, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return b, err
	}
	// RFC 4122 UUID v4 variant bits keep the token UUID-shaped for the UUID column.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return b, nil
}

// UUIDString formats a 16-byte token as a UUID string.
func UUIDString(b [16]byte) string {
	return formatUUID(b)
}

// QA / TODO: panics on RNG failure; test/startup helper, not production request path.
func MustRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// PutUint64 encodes n as 8-byte big-endian.
func PutUint64(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

// ActionKey derives the stable scheduled-action key from subscription, type, and target time.
func ActionKey(subscriptionRef, actionType string, target time.Time) string {
	target = TruncateUTC(target)
	material := ActionKeyInfo + "\n" + subscriptionRef + "\n" + actionType + "\n" + target.Format(time.RFC3339)
	sum := sha256.Sum256([]byte(material))
	return ActionKeyPrefix + hex.EncodeToString(sum[:])
}

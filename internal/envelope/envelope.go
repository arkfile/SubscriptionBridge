package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

const VersionV1 = "v1"

var (
	ErrInvalidKey     = errors.New("invalid data-encryption key")
	ErrUnknownVersion = errors.New("unknown encryption key version")
	ErrOpen           = errors.New("envelope open failed")
)

type Box struct {
	key     []byte
	version string
}

// New constructs an AES-256-GCM box from a 64-character lowercase hex key.
func New(configuredHex string) (*Box, error) {
	if len(configuredHex) != 64 {
		return nil, ErrInvalidKey
	}
	for i := 0; i < len(configuredHex); i++ {
		c := configuredHex[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return nil, ErrInvalidKey
		}
	}
	key, err := hex.DecodeString(configuredHex)
	if err != nil || len(key) != protocol.KeySize {
		return nil, ErrInvalidKey
	}
	return &Box{key: key, version: VersionV1}, nil
}

// AAD builds authenticated associated data for a purpose and record ID.
func AAD(purpose, recordID string) []byte {
	return []byte("subscription-bridge/v1/" + purpose + "/" + recordID)
}

// Seal encrypts plaintext with a random nonce and returns ciphertext plus key version.
func (b *Box) Seal(plaintext, aad []byte) (ciphertext, nonce []byte, keyVersion string, err error) {
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, "", err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, "", err
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, aad)
	return ciphertext, nonce, b.version, nil
}

// Open decrypts ciphertext after checking the stored key version.
func (b *Box) Open(ciphertext, nonce, aad []byte, keyVersion string) ([]byte, error) {
	if keyVersion != b.version {
		return nil, fmt.Errorf("%w: %s", ErrUnknownVersion, keyVersion)
	}
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

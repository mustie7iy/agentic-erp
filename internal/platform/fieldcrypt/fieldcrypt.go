// Package fieldcrypt encrypts sensitive fields, bank account numbers and tax
// identifiers, before they reach the database (01-requirements, security;
// 03-data-model §3.6). Values are sealed with AES-256-GCM, bound to their
// tenant as associated data, and fingerprinted with a keyed HMAC-SHA256 so
// equal values can be found across partners without decrypting them.
//
// Both keys derive from one secret that is the same on every replica. Key
// rotation and per-tenant keys in a KMS come later; the sealed format starts
// with a version byte so they can be introduced without rewriting data.
package fieldcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
)

const version byte = 1

// Keys seals values and computes fingerprints.
type Keys struct {
	aead cipher.AEAD
	mac  []byte
}

// New derives the keys from a secret of at least 32 bytes.
func New(secret []byte) (*Keys, error) {
	if len(secret) < 32 {
		return nil, errors.New("fieldcrypt: the secret must be at least 32 bytes")
	}
	block, _ := aes.NewCipher(derive(secret, "erp field encryption v1")) // a 32-byte key is always valid
	aead, _ := cipher.NewGCM(block)                                      // AES always supports GCM
	return &Keys{aead: aead, mac: derive(secret, "erp field fingerprint v1")}, nil
}

func derive(secret []byte, label string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Normalize is the form values are sealed and fingerprinted in: upper case
// without spaces, so "de89 3704" and "DE893704" are the same account.
func Normalize(value string) string {
	return strings.ToUpper(strings.Join(strings.Fields(value), ""))
}

// Seal encrypts the normalized value for the tenant.
func (k *Keys) Seal(tenant uuid.UUID, value string) []byte {
	nonce := make([]byte, k.aead.NonceSize())
	rand.Read(nonce) // never fails (crypto/rand)
	out := append([]byte{version}, nonce...)
	return k.aead.Seal(out, nonce, []byte(Normalize(value)), tenant[:])
}

// ErrCorrupt means a sealed value cannot be decrypted with these keys for
// this tenant.
var ErrCorrupt = errors.New("fieldcrypt: sealed value is corrupt or belongs to another tenant or key")

// Open decrypts a sealed value of the tenant.
func (k *Keys) Open(tenant uuid.UUID, sealed []byte) (string, error) {
	n := k.aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != version {
		return "", ErrCorrupt
	}
	plain, err := k.aead.Open(nil, sealed[1:1+n], sealed[1+n:], tenant[:])
	if err != nil {
		return "", ErrCorrupt
	}
	return string(plain), nil
}

// Fingerprint is the hex HMAC-SHA256 of the normalized value within the
// tenant: equal values of one tenant have equal fingerprints, and nothing
// about the value can be learned from it without the key.
func (k *Keys) Fingerprint(tenant uuid.UUID, value string) string {
	m := hmac.New(sha256.New, k.mac)
	m.Write(tenant[:])
	m.Write([]byte(Normalize(value)))
	return hex.EncodeToString(m.Sum(nil))
}

// Last4 is the masked form shown to clients: the last up to four characters
// of the normalized value.
func Last4(value string) string {
	v := Normalize(value)
	if len(v) > 4 {
		v = v[len(v)-4:]
	}
	return v
}

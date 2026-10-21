package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Sealer seals what erp-web keeps of a person: a session's tokens and
// CSRF secret in the store, a sign-in's state in its cookie. It is
// AES-256-GCM under a key derived from erp-web's session secret, the
// field-encryption pattern of ADR-0018, with what the value belongs to as
// associated data, so a sealed value moved to another session (or from a
// cookie into the store) does not open. Keys rotate (ADR-0028): Seal uses
// the first secret, Open any of them, chosen by the key id the sealed
// value starts with; sessions sealed under a retired secret no longer
// open and their people sign in again.
type Sealer struct {
	keys []sealKey
}

type sealKey struct {
	id   [8]byte
	aead cipher.AEAD
}

// The sealed format's version, then the key id, the nonce and the
// ciphertext with its tag.
const sealVersion byte = 1

// MinSecret is the shortest session secret accepted: 256 bits.
const MinSecret = 32

// ErrUnsealed means a sealed value is malformed, under no key the Sealer
// holds, or was altered or moved.
var ErrUnsealed = errors.New("session: the sealed value does not open")

// NewSealer derives the keys from the secrets, the current one first.
func NewSealer(secrets ...[]byte) (*Sealer, error) {
	if len(secrets) == 0 {
		return nil, errors.New("session: a session secret is required")
	}
	s := &Sealer{}
	for _, secret := range secrets {
		if len(secret) < MinSecret {
			return nil, errors.New("session: a session secret must be at least 32 bytes")
		}
		block, _ := aes.NewCipher(derive(secret, "erp-web session sealing v1")) // a 32-byte key is always valid
		aead, _ := cipher.NewGCM(block)                                         // AES always supports GCM
		k := sealKey{aead: aead}
		copy(k.id[:], derive(secret, "erp-web session key id v1"))
		s.keys = append(s.keys, k)
	}
	return s, nil
}

func derive(secret []byte, label string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Seal seals plaintext for what aad names, under the current key.
func (s *Sealer) Seal(aad, plaintext []byte) []byte {
	k := s.keys[0]
	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error: it crashes the program instead (Go 1.24+)
	out := append([]byte{sealVersion}, k.id[:]...)
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, plaintext, aad)
}

// Open opens a value Seal sealed for aad.
func (s *Sealer) Open(aad, sealed []byte) ([]byte, error) {
	const head = 1 + 8
	if len(sealed) < head || sealed[0] != sealVersion {
		return nil, ErrUnsealed
	}
	for _, k := range s.keys {
		if !hmac.Equal(k.id[:], sealed[1:head]) {
			continue
		}
		rest := sealed[head:]
		if len(rest) < k.aead.NonceSize() {
			return nil, ErrUnsealed
		}
		plain, err := k.aead.Open(nil, rest[:k.aead.NonceSize()], rest[k.aead.NonceSize():], aad)
		if err != nil {
			return nil, ErrUnsealed
		}
		return plain, nil
	}
	return nil, ErrUnsealed
}

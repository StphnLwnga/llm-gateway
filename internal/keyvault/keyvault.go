// Package keyvault encrypts secrets at rest with envelope encryption: each
// secret has its own data key (DEK), wrapped by a key-encryption key (KEK)
// that never leaves its implementation. It protects a database dump or
// backup; it does not protect against code running on the gateway host.
package keyvault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
)

// DEKSize is the length of a data key: AES-256.
const DEKSize = 32

// PurposeProviderCredential labels the AAD of a tenant's provider credential.
const PurposeProviderCredential = "llm-gateway/provider-credential/v1" //nolint:gosec // G101: a label, not a credential

var (
	// ErrDecrypt is every failure to unwrap a DEK or open a ciphertext. It
	// does not say which layer failed.
	ErrDecrypt = errors.New("keyvault: decryption failed")
	// ErrUnknownKEK means the envelope names a KEK that the keyring does not hold.
	ErrUnknownKEK = errors.New("keyvault: unknown KEK ID")
)

// KEK wraps and unwraps data keys. Its key material never leaves it, as with a KMS.
type KEK interface {
	ActiveID() string
	// Wrap wraps dek with the active key and returns that key's ID.
	Wrap(ctx context.Context, dek, aad []byte) (keyID string, wrapped []byte, err error)
	Unwrap(ctx context.Context, keyID string, wrapped, aad []byte) (dek []byte, err error)
}

// Envelope is a sealed secret: the KEK ID, the wrapped DEK and the ciphertext.
type Envelope struct {
	KEKID      string
	WrappedDEK []byte
	Ciphertext []byte
}

// AAD encodes purpose and fields, each as a 4-byte big-endian length and its
// bytes, so no two different inputs give the same AAD.
func AAD(purpose string, fields ...string) []byte {
	n := 4 + len(purpose)
	for _, f := range fields {
		n += 4 + len(f)
	}
	b := make([]byte, 0, n)
	for _, s := range append([]string{purpose}, fields...) {
		if uint64(len(s)) > math.MaxUint32 {
			panic("keyvault: AAD field longer than 4 GiB")
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // G115: checked above
		b = append(b, s...)
	}
	return b
}

func seal(aead cipher.AEAD, plaintext, aad []byte) []byte {
	return aead.Seal(nil, nil, plaintext, aad) //nolint:gosec // G407: the nonce argument is empty, not hardcoded
}

// newAEAD returns AES-256-GCM that puts a random nonce in front of each output.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Vault seals and opens secrets with a new DEK for each secret.
type Vault struct {
	kek KEK
}

// New returns a vault over kek.
func New(kek KEK) *Vault {
	return &Vault{kek: kek}
}

// Seal encrypts plaintext with a new DEK and wraps the DEK. Both layers use aad.
func (v *Vault) Seal(ctx context.Context, plaintext, aad []byte) (Envelope, error) {
	dek := make([]byte, DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext := seal(aead, plaintext, aad)
	id, wrapped, err := v.kek.Wrap(ctx, dek, aad)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{KEKID: id, WrappedDEK: wrapped, Ciphertext: ciphertext}, nil
}

// Open returns the plaintext of env, or ErrDecrypt if any part of it or aad was changed.
func (v *Vault) Open(ctx context.Context, env Envelope, aad []byte) ([]byte, error) {
	dek, err := v.kek.Unwrap(ctx, env.KEKID, env.WrappedDEK, aad)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, nil, env.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// Rewrap wraps the DEK of env with the active KEK. The ciphertext is not
// touched, and an envelope that already uses the active KEK is returned as it is.
func (v *Vault) Rewrap(ctx context.Context, env Envelope, aad []byte) (Envelope, error) {
	if env.KEKID == v.kek.ActiveID() {
		return env, nil
	}
	dek, err := v.kek.Unwrap(ctx, env.KEKID, env.WrappedDEK, aad)
	if err != nil {
		return Envelope{}, err
	}
	id, wrapped, err := v.kek.Wrap(ctx, dek, aad)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{KEKID: id, WrappedDEK: wrapped, Ciphertext: env.Ciphertext}, nil
}

package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// KeyLen is the byte length of an AES-256 key.
const KeyLen = 32

var (
	// ErrCiphertextTooShort means the blob cannot possibly contain a nonce plus
	// an authentication tag.
	ErrCiphertextTooShort = errors.New("secret: ciphertext is too short")

	// ErrDecrypt covers both a wrong key and a tampered blob. The two are
	// deliberately indistinguishable to a caller: telling an attacker which one
	// happened is free information for them and none for us.
	ErrDecrypt = errors.New("secret: decryption failed")
)

// Sealer encrypts and authenticates small secrets at rest.
//
// Ciphertext layout is nonce || AES-256-GCM ciphertext-and-tag. The caller
// supplies additional authenticated data (AAD) that binds a ciphertext to its
// purpose: an encrypted GitHub token cannot be relocated into the field holding
// the registry password and still decrypt, even though both use the same key.
//
// A Sealer is safe for concurrent use.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a Sealer from a 32-byte key, as produced by DeriveKey.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("secret: key must be %d bytes, got %d", KeyLen, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: new cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: new gcm: %w", err)
	}

	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext, authenticating aad alongside it. The nonce is
// generated per call and prepended to the result.
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}

	// Passing nonce as dst appends the ciphertext directly after it, giving us
	// nonce || ciphertext in one allocation.
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open reverses Seal. It returns ErrDecrypt if the key is wrong, the blob was
// tampered with, or the aad does not match the one used to seal it.
func (s *Sealer) Open(blob, aad []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(blob) < nonceSize+s.aead.Overhead() {
		return nil, ErrCiphertextTooShort
	}

	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]

	plaintext, err := s.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

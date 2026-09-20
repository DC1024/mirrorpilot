package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Parameters for master-key derivation.
//
// Deliberately more expensive than the password-hash parameters above: this runs
// once per login (to unlock stored credentials), not once per request, so we can
// afford to make an offline guessing attack on a stolen volume much costlier.
const (
	deriveTime    = 4
	deriveMemory  = 96 * 1024 // KiB, i.e. 96 MiB
	deriveThreads = 2
	deriveSaltLen = 16
)

// NewSalt returns a random salt of the length DeriveKey expects. Store it
// alongside the encrypted data; it is not secret, but it must be stable, or the
// same master password will produce a different key.
func NewSalt() ([]byte, error) {
	salt := make([]byte, deriveSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("secret: read salt: %w", err)
	}
	return salt, nil
}

// DeriveKey turns the user's master password and a stored salt into an AES-256
// key suitable for NewSealer.
//
// The password is not persisted anywhere and should not be retained by the
// caller longer than needed.
func DeriveKey(masterPassword string, salt []byte) []byte {
	return argon2.IDKey(
		[]byte(masterPassword), salt,
		deriveTime, deriveMemory, deriveThreads,
		KeyLen,
	)
}

// Fingerprint returns a short, non-reversible identifier for a key.
//
// Storing this lets us tell "the user typed the wrong master password" apart
// from "the stored data is corrupted" — a genuinely useful distinction to show
// in the UI. It reveals nothing that helps recover the key, as long as the
// master password itself has adequate entropy.
func Fingerprint(key []byte) string {
	prefixed := append([]byte("mirrorpilot-key-fingerprint:"), key...)
	sum := sha256.Sum256(prefixed)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

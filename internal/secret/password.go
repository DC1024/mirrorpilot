// Package secret handles the two fundamentally different kinds of secret
// material this project stores.
//
//  1. Passwords (the panel login). These must never be recoverable, so they are
//     hashed with Argon2id and verified by re-deriving and comparing.
//
//  2. Third-party credentials (GitHub tokens, cloud registry keys). These must
//     be recoverable, because we have to present them to those APIs verbatim.
//     They are encrypted with AES-256-GCM under a key derived from the user's
//     master password.
//
// The master password itself is never written to disk. It exists only in memory
// for the lifetime of a session, which is what makes a stolen /data volume
// useless on its own.
package secret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for password hashing.
//
// These aim for roughly 100ms on a modern core while staying within the memory
// budget of the modest hardware this project targets. They are embedded in every
// hash string, so raising them later does not invalidate existing hashes — old
// hashes keep verifying with their own recorded parameters.
const (
	hashTime    = 3
	hashMemory  = 64 * 1024 // KiB, i.e. 64 MiB
	hashThreads = 2
	hashKeyLen  = 32
	hashSaltLen = 16
)

var (
	// ErrInvalidHash means the stored string is not a well-formed Argon2id
	// PHC string we can parse.
	ErrInvalidHash = errors.New("secret: invalid password hash format")

	// ErrMismatch means the password did not match the stored hash.
	ErrMismatch = errors.New("secret: password does not match")
)

// phcParams holds the parameters parsed out of a PHC string.
type phcParams struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	hash    []byte
}

// HashPassword derives an Argon2id hash of plain and returns it in PHC string
// format:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<base64 salt>$<base64 hash>
//
// A fresh random salt is generated on every call, so hashing the same password
// twice yields different strings.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secret: read salt: %w", err)
	}

	sum := argon2.IDKey([]byte(plain), salt, hashTime, hashMemory, hashThreads, hashKeyLen)

	return encodePHC(salt, sum), nil
}

// VerifyPassword reports whether plain matches encoded. It returns ErrMismatch
// on a wrong password and ErrInvalidHash on a malformed stored value, so callers
// can distinguish "user typo" from "database corruption".
//
// Comparison is constant-time.
func VerifyPassword(encoded, plain string) error {
	p, err := decodePHC(encoded)
	if err != nil {
		return err
	}

	// Re-derive using the parameters recorded in the hash, not the current
	// constants, so hashes created under older settings keep working.
	sum := argon2.IDKey([]byte(plain), p.salt, p.time, p.memory, p.threads, uint32(len(p.hash)))

	if subtle.ConstantTimeCompare(sum, p.hash) != 1 {
		return ErrMismatch
	}
	return nil
}

func encodePHC(salt, sum []byte) string {
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		hashMemory, hashTime, hashThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	)
}

func decodePHC(s string) (phcParams, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return phcParams{}, ErrInvalidHash
	}

	versionStr, ok := strings.CutPrefix(parts[2], "v=")
	if !ok {
		return phcParams{}, ErrInvalidHash
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil || version != argon2.Version {
		return phcParams{}, fmt.Errorf("%w: unsupported argon2 version", ErrInvalidHash)
	}

	var (
		mem, time, par int
		p              phcParams
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &par); err != nil {
		return phcParams{}, ErrInvalidHash
	}
	// Reject nonsensical parameters outright. Without this, a corrupted record
	// could ask us to allocate an absurd amount of memory on the login path.
	if mem <= 0 || mem > 4*1024*1024 || time <= 0 || time > 64 || par <= 0 || par > 255 {
		return phcParams{}, fmt.Errorf("%w: parameters out of range", ErrInvalidHash)
	}
	p.memory, p.time, p.threads = uint32(mem), uint32(time), uint8(par)

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return phcParams{}, ErrInvalidHash
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) == 0 {
		return phcParams{}, ErrInvalidHash
	}
	p.salt, p.hash = salt, hash

	return p, nil
}

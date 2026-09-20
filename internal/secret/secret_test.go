package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const plain = "correct horse battery staple"

	encoded, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := VerifyPassword(encoded, plain); err != nil {
		t.Errorf("VerifyPassword with the right password: %v", err)
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	encoded, err := HashPassword("right-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	err = VerifyPassword(encoded, "wrong-password")
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("VerifyPassword with a wrong password = %v, want ErrMismatch", err)
	}
}

// Two hashes of the same password must differ, or the salt is not random.
func TestHashPasswordUsesFreshSalt(t *testing.T) {
	first, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if first == second {
		t.Error("hashing the same password twice produced identical strings; salt is not random")
	}
}

func TestHashEncodingIsPHC(t *testing.T) {
	encoded, err := HashPassword("whatever")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=") {
		t.Errorf("hash %q does not start with an argon2id PHC prefix", encoded)
	}
	if strings.Count(encoded, "$") != 5 {
		t.Errorf("hash %q does not have 5 dollar separators", encoded)
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"not phc":            "just-a-plain-string",
		"wrong algorithm":    "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"missing fields":     "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"bad version":        "$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"non numeric params": "$argon2id$v=19$m=x,t=y,p=z$c2FsdA$aGFzaA",
		"bad salt encoding":  "$argon2id$v=19$m=65536,t=3,p=2$!!!!$aGFzaA",
		"bad hash encoding":  "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$!!!!",
		"empty salt":         "$argon2id$v=19$m=65536,t=3,p=2$$aGFzaA",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			err := VerifyPassword(encoded, "anything")
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("VerifyPassword(%q) = %v, want ErrInvalidHash", encoded, err)
			}
		})
	}
}

// A corrupted record must not be able to make us allocate an unbounded amount of
// memory on the login path.
func TestDecodePHCRejectsAbsurdParameters(t *testing.T) {
	huge := "$argon2id$v=19$m=999999999,t=3,p=2$c2FsdA$aGFzaA"

	if _, err := decodePHC(huge); !errors.Is(err, ErrInvalidHash) {
		t.Errorf("decodePHC with absurd memory = %v, want ErrInvalidHash", err)
	}
}

func TestSealerRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)

	s, err := NewSealer(key)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	plaintext := []byte("ghp_aGitHubPersonalAccessToken")
	aad := []byte("credential:github_token")

	blob, err := s.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, plaintext) {
		t.Fatal("ciphertext contains the plaintext")
	}

	got, err := s.Open(blob, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Open returned %q, want %q", got, plaintext)
	}
}

func TestSealerRejectsWrongKey(t *testing.T) {
	aad := []byte("credential:registry_password")

	sealerA, err := NewSealer(bytes.Repeat([]byte{0x01}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	sealerB, err := NewSealer(bytes.Repeat([]byte{0x02}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	blob, err := sealerA.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := sealerB.Open(blob, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with the wrong key = %v, want ErrDecrypt", err)
	}
}

// The AAD exists so a ciphertext cannot be moved between fields.
func TestSealerRejectsMismatchedAAD(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{0x07}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	blob, err := s.Seal([]byte("token"), []byte("credential:github_token"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := s.Open(blob, []byte("credential:registry_password")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with mismatched AAD = %v, want ErrDecrypt", err)
	}
}

func TestSealerDetectsTampering(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{0x09}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	blob, err := s.Seal([]byte("untouched"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Flip a bit in the last byte — inside the authentication tag.
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0x01

	if _, err := s.Open(tampered, []byte("aad")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open on a tampered blob = %v, want ErrDecrypt", err)
	}
}

func TestSealerNonceIsFresh(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{0x0b}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	first, err := s.Seal([]byte("same"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := s.Seal([]byte("same"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if bytes.Equal(first, second) {
		t.Error("sealing the same plaintext twice produced identical output; nonce is not random")
	}
}

func TestSealerRejectsShortBlob(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{0x0d}, KeyLen))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	if _, err := s.Open([]byte{0x01, 0x02}, []byte("aad")); !errors.Is(err, ErrCiphertextTooShort) {
		t.Errorf("Open on a truncated blob = %v, want ErrCiphertextTooShort", err)
	}
}

func TestNewSealerRejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewSealer(make([]byte, n)); err == nil {
			t.Errorf("NewSealer with a %d-byte key returned nil error", n)
		}
	}
}

func TestDeriveKeyIsDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")

	first := DeriveKey("master-password", salt)
	second := DeriveKey("master-password", salt)

	if !bytes.Equal(first, second) {
		t.Error("DeriveKey is not deterministic for the same password and salt")
	}
	if len(first) != KeyLen {
		t.Errorf("DeriveKey returned %d bytes, want %d", len(first), KeyLen)
	}
}

func TestDeriveKeyVariesWithSaltAndPassword(t *testing.T) {
	base := DeriveKey("master-password", []byte("0123456789abcdef"))

	otherSalt := DeriveKey("master-password", []byte("fedcba9876543210"))
	if bytes.Equal(base, otherSalt) {
		t.Error("changing the salt did not change the derived key")
	}

	otherPassword := DeriveKey("different-password", []byte("0123456789abcdef"))
	if bytes.Equal(base, otherPassword) {
		t.Error("changing the password did not change the derived key")
	}
}

func TestNewSaltIsRandom(t *testing.T) {
	first, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	second, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}

	if len(first) != deriveSaltLen {
		t.Errorf("NewSalt returned %d bytes, want %d", len(first), deriveSaltLen)
	}
	if bytes.Equal(first, second) {
		t.Error("NewSalt returned the same value twice")
	}
}

func TestFingerprint(t *testing.T) {
	keyA := bytes.Repeat([]byte{0xaa}, KeyLen)
	keyB := bytes.Repeat([]byte{0xbb}, KeyLen)

	if Fingerprint(keyA) != Fingerprint(keyA) {
		t.Error("Fingerprint is not stable for the same key")
	}
	if Fingerprint(keyA) == Fingerprint(keyB) {
		t.Error("Fingerprint collided for two different keys")
	}
	if len(Fingerprint(keyA)) > 16 {
		t.Errorf("Fingerprint is %d chars, want something short", len(Fingerprint(keyA)))
	}
}

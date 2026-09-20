package auth

import (
	"context"
	"fmt"

	"github.com/DC1024/mirrorpilot/internal/secret"
)

// ValidatePassword enforces the length policy.
//
// Exported so the web layer can reject a too-short password on the form before
// running an intentionally expensive hash, and show the reason next to the
// field instead of on a separate error page.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: use at most %d bytes", ErrWeakPassword, MaxPasswordLength)
	}
	return nil
}

// ChangePassword rotates the master key and re-encrypts everything sealed under
// the old one.
//
// The order of operations is the whole design. Re-encryption happens entirely
// in memory first; only when every credential has been successfully opened and
// resealed do we touch the database, and then in a single transaction that
// swaps the credentials, the salt, the fingerprint, and the password hash
// together. If any step fails before that commit, the old state is still
// coherent and the user can simply try again.
//
// Doing it the other way round — write the new salt, then re-encrypt — would
// mean a failure between the two steps leaves credentials sealed under a key
// nobody can derive any more. That is unrecoverable data loss, not a bad error
// message.
//
// It returns the number of sessions that were invalidated.
func (m *Manager) ChangePassword(ctx context.Context, currentPassword, newPassword string) (int64, error) {
	if err := m.checkPassword(ctx, currentPassword); err != nil {
		return 0, err
	}
	if err := ValidatePassword(newPassword); err != nil {
		return 0, err
	}

	// Refuse before doing any work if we cannot read what we are about to
	// rewrite. Without this we would rotate the salt and leave the old
	// ciphertext in place, which is exactly the failure mode described above.
	oldSealer, err := m.Sealer()
	if err != nil {
		return 0, err
	}

	stored, err := m.store.Credentials(ctx)
	if err != nil {
		return 0, err
	}

	salt, err := secret.NewSalt()
	if err != nil {
		return 0, err
	}
	key := secret.DeriveKey(newPassword, salt)

	newSealer, err := secret.NewSealer(key)
	if err != nil {
		return 0, err
	}

	rekeyed := make(map[string][]byte, len(stored))
	for name, blob := range stored {
		plaintext, err := oldSealer.Open(blob, aad(name))
		if err != nil {
			// Returning here is safe: nothing has been written yet, and a
			// half-re-encrypted set must never reach the database.
			return 0, fmt.Errorf("auth: re-encrypt credential %q: %w", name, err)
		}
		sealed, err := newSealer.Seal(plaintext, aad(name))
		if err != nil {
			return 0, fmt.Errorf("auth: re-encrypt credential %q: %w", name, err)
		}
		rekeyed[name] = sealed
	}

	hash, err := secret.HashPassword(newPassword)
	if err != nil {
		return 0, err
	}

	if err := m.store.Rekey(ctx, hash, salt, secret.Fingerprint(key), rekeyed); err != nil {
		return 0, err
	}

	// The key goes live only after the vault agrees with it, so this can never
	// install a key the stored salt no longer produces.
	if err := m.installKey(key); err != nil {
		return 0, err
	}

	// Changing the password logs everyone out. The point of the operation is
	// that old sessions stop working, so leaving them alive would undercut it
	// — and the count is worth showing the user.
	kicked, err := m.store.DeleteAllSessions(ctx)
	if err != nil {
		return kicked, err
	}

	return kicked, nil
}

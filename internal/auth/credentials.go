package auth

import (
	"context"
	"fmt"
)

// aad binds a ciphertext to the credential slot it belongs to.
//
// AES-GCM authenticates this alongside the payload, so a blob copied from one
// slot to another fails to decrypt rather than silently handing the wrong token
// to the wrong API. Cheap insurance against a bug (or a malicious edit) in the
// settings handler.
func aad(name string) []byte {
	return []byte("mirrorpilot:credential:" + name)
}

// SaveCredential encrypts plaintext under the master key and stores it.
func (m *Manager) SaveCredential(ctx context.Context, name string, plaintext []byte) error {
	sealer, err := m.Sealer()
	if err != nil {
		return err
	}

	blob, err := sealer.Seal(plaintext, aad(name))
	if err != nil {
		return fmt.Errorf("auth: encrypt credential %q: %w", name, err)
	}

	return m.store.SaveCredential(ctx, name, blob)
}

// Credential decrypts one stored credential. A missing one returns
// store.ErrNotFound, which callers treat as "not configured yet".
func (m *Manager) Credential(ctx context.Context, name string) ([]byte, error) {
	sealer, err := m.Sealer()
	if err != nil {
		return nil, err
	}

	blob, err := m.store.Credential(ctx, name)
	if err != nil {
		return nil, err
	}

	plaintext, err := sealer.Open(blob, aad(name))
	if err != nil {
		return nil, fmt.Errorf("auth: decrypt credential %q: %w", name, err)
	}
	return plaintext, nil
}

// DeleteCredential removes one credential. Deleting an absent one is a no-op.
func (m *Manager) DeleteCredential(ctx context.Context, name string) error {
	return m.store.DeleteCredential(ctx, name)
}

// ConfiguredCredentials reports which credential slots are populated.
//
// It deliberately does not need the master key: the dashboard uses it to render
// filled/empty indicators while the panel is still locked, and reading a name
// out of a row reveals nothing about the value.
func (m *Manager) ConfiguredCredentials(ctx context.Context) (map[string]bool, error) {
	stored, err := m.store.Credentials(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(stored))
	for name := range stored {
		out[name] = true
	}
	return out, nil
}

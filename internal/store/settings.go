package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Setting keys. Kept as constants so a typo becomes a compile error rather than
// a silently ignored preference.
const (
	// SettingLocale is the UI language, e.g. "en" or "zh-CN".
	SettingLocale = "locale"

	// SettingTheme is "light", "dark", or "auto".
	SettingTheme = "theme"

	// SettingACRRegistry is the non-secret registry host, e.g.
	// "registry.cn-hangzhou.aliyuncs.com". Not encrypted: it is not sensitive
	// and showing it in the UI is useful.
	SettingACRRegistry = "acr_registry"

	// SettingACRNamespace is the non-secret ACR namespace.
	SettingACRNamespace = "acr_namespace"

	// SettingProbeRepository is the Docker Hub repository that probes measure,
	// e.g. "library/alpine". Not sensitive, and worth showing: a speed number
	// means nothing without saying which image produced it.
	SettingProbeRepository = "probe_repository"

	// SettingProbeReference is the tag or digest of the image probes measure.
	//
	// A digest is the honest choice. A tag can be answered from a stale mirror
	// cache, so two mirrors measured in the same batch may have served
	// materially different bytes — which makes the comparison the tool exists
	// to produce quietly unfair.
	SettingProbeReference = "probe_reference"
)

// Credential names, used as the primary key of the credentials table.
//
// These are deliberately not settings: a credential's value lives encrypted in
// credentials, never as plaintext in settings. Only the *name* is shared
// vocabulary, which is why they sit in their own block.
const (
	// CredentialGitHubToken is the GitHub personal access token used to
	// dispatch the relocation workflow.
	CredentialGitHubToken = "github_token"

	// CredentialACRPassword is the Alibaba Cloud registry password.
	CredentialACRPassword = "acr_password"
)

// Setting reads one setting. ErrNotFound means it was never set; callers should
// fall back to a default rather than treating that as a failure.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string

	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return "", wrapNotFound(fmt.Errorf("store: load setting %q: %w", key, err))
	}
	return value, nil
}

// SetSetting upserts one setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: save setting %q: %w", key, err)
	}
	return nil
}

// Settings returns every setting at once. Handy for rendering a preferences
// form without a query per field.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan setting: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate settings: %w", err)
	}
	return out, nil
}

// DeleteSetting removes a setting, reverting it to its default. Missing is fine.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	return nil
}

// SettingOrDefault reads a setting, returning fallback when it is absent.
func (s *Store) SettingOrDefault(ctx context.Context, key, fallback string) (string, error) {
	value, err := s.Setting(ctx, key)
	switch {
	case err == nil:
		return value, nil
	case errors.Is(err, ErrNotFound):
		return fallback, nil
	default:
		return "", err
	}
}

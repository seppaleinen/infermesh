package main

import (
	"errors"
	"fmt"
)

const keyringServiceName = "infermesh"

var knownSecretRefs = map[string]bool{
	"router/apikey":    true,
	"worker/customauth": true,
	"mtls/cert":        true,
	"mtls/key":         true,
	"relay/creds":      true,
}

type ConfigService struct {
	kr   Keyring
	path string // absolute path of the settings YAML file
}

// NewConfigService creates a ConfigService with the provided keyring and
// settings path. The path is where non-secret settings are persisted; secrets
// always live in the keyring, never in this file.
func NewConfigService(kr Keyring, path string) *ConfigService {
	return &ConfigService{kr: kr, path: path}
}

// ServiceName implements the Wails service interface for ConfigService.
func (*ConfigService) ServiceName() string {
	return "ConfigService"
}

// LoadSettings loads the configuration from disk and merges defaults.
func (cs *ConfigService) LoadSettings() (Settings, error) {
	s, err := LoadFrom(cs.path)
	if err != nil {
		return Settings{}, err
	}
	return mergeDefaults(s), nil
}

// SaveSettings writes the Settings to disk. If the Settings contain any
// secrets and the keyring is unavailable, it fails early with the hard-fail
// error before touching any filesystem. On success, the secret values are
// stripped from the YAML, and the secret references are stored under
// SecretRefs only. The returned bool reports whether the keyring is
// available, so the UI can warn the user even when no secrets are present.
func (cs *ConfigService) SaveSettings(s Settings) (bool, error) {
	// Hard-fail check: if keyring unavailable and secrets present, abort
	// early — never write plaintext secrets to disk.
	if !cs.kr.Available() {
		for _, v := range s.Secrets {
			if v != "" {
				return false, errKeyringUnavailable
			}
		}
		// No secrets: safe to persist non-secret settings. Report
		// keyring-down so the UI can warn.
	}

	// Strip secrets before saving. SecretRefs are merged with whatever is
	// already on disk: a caller may have called SetSecret (which persists a
	// ref directly) and then saved non-secret settings; replacing SecretRefs
	// wholesale would silently drop the previously-persisted ref.
	saved, err := LoadFrom(cs.path)
	if err != nil {
		return false, fmt.Errorf("load existing settings: %w", err)
	}
	if saved.SecretRefs == nil {
		saved.SecretRefs = make(map[string]string)
	}
	for k, v := range s.Secrets {
		if v == "" {
			continue
		}
		if err := cs.kr.Set(k, v); err != nil {
			return false, fmt.Errorf("keyring set error: %w", err)
		}
		saved.SecretRefs[k] = k
	}
	s.SecretRefs = saved.SecretRefs
	s.Secrets = nil

	if err := SaveTo(s, cs.path); err != nil {
		return false, err
	}
	return cs.kr.Available(), nil
}

// SetSecret stores a secret value under the given reference. It performs a
// hard-fail check against keyring availability and writes to the keyring.
func (cs *ConfigService) SetSecret(ref, value string) error {
	if !cs.kr.Available() {
		return errKeyringUnavailable
	}
	if err := cs.kr.Set(ref, value); err != nil {
		return err
	}

	// Record the reference in the YAML file (never the value).
	s, err := LoadFrom(cs.path)
	if err != nil {
		return err
	}
	s.SecretRefs[ref] = ref
	s.Secrets = nil

	return SaveTo(s, cs.path)
}

// GetSecret retrieves a secret value from the keyring. It returns the
// sentinel errNotFound if the key does not exist.
func (cs *ConfigService) GetSecret(ref string) (string, error) {
	return cs.kr.Get(ref)
}

// DeleteSecret removes a secret from the keyring and drops its reference
// from the YAML file. If the key is not present in the keyring, it is
// ignored (silent non-error).
func (cs *ConfigService) DeleteSecret(ref string) error {
	if err := cs.kr.Delete(ref); err != nil {
		if errors.Is(err, errNotFound) {
			// Key was not present; ignore silently
		} else {
			return err
		}
	}

	// Update YAML to remove the SecretRefs entry
	s, err := LoadFrom(cs.path)
	if err != nil {
		return fmt.Errorf("load existing settings: %w", err)
	}
	delete(s.SecretRefs, ref)
	s.Secrets = nil

	return SaveTo(s, cs.path)
}

// IsKeyringAvailable reports whether the underlying keyring implementation
// is available on the host.
func (cs *ConfigService) IsKeyringAvailable() bool {
	return cs.kr.Available()
}

// GetKeyringServiceName returns the service name used when storing secrets
// in the OS keyring.
func (*ConfigService) GetKeyringServiceName() string {
	return keyringServiceName
}

// ValidateSecretRefs ensures that all keys present in SecretRefs are within
// the known set of acceptable references.
func (*ConfigService) ValidateSecretRefs(s Settings) error {
	for ref := range s.SecretRefs {
		if !knownSecretRefs[ref] {
			return fmt.Errorf("unknown secret ref: %s", ref)
		}
	}
	return nil
}
package main

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/zalando/go-keyring"
)

// keyringService is the macOS keychain service name used to scope all
// infermesh secrets. The account name mirrors the secret ref (e.g.
// "router/apikey") so the YAML file only ever holds a reference.
const keyringService = "infermesh"

// errKeyringUnavailable is returned when the host has no usable keyring
// backend. The desktop app treats this as a hard-fail: callers must not
// write plaintext secrets to disk when the keyring is down.
var errKeyringUnavailable = errors.New("keyring is unavailable on this host")

// errNotFound is the canonical "secret not found" error surfaced by the
// in-memory test double. It aliases the upstream sentinel so callers can
// use errors.Is(err, errNotFound) without importing the third-party pkg.
var errNotFound = errors.New("secret not found in keyring")

// Keyring is the narrow interface the ConfigService depends on. Both the
// production wrapper (prodKeyring) and the test doubles (memKeyring,
// failKeyring) implement it, so the service is unit-testable without a
// live macOS keychain.
type Keyring interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
	Available() bool
}

// prodKeyring wraps the zalando/go-keyring library under the "infermesh"
// service. On macOS this routes through the local keychain; on other
// hosts Available() reports false so the desktop app refuses to persist
// secrets rather than silently falling back to plaintext.
type prodKeyring struct{}

// newProdKeyring returns a Keyring backed by the OS keychain.
func newProdKeyring() Keyring {
	return prodKeyring{}
}

// Get reads a secret from the keychain.
func (prodKeyring) Get(key string) (string, error) {
	v, err := keyring.Get(keyringService, key)
	if err != nil {
		return "", errNotFound
	}
	return v, nil
}

// Set stores a secret in the keychain.
func (prodKeyring) Set(key, value string) error {
	return keyring.Set(keyringService, key, value)
}

// Delete removes a secret from the keychain.
func (prodKeyring) Delete(key string) error {
	return keyring.Delete(keyringService, key)
}

// Available probes the host for a usable keychain. On macOS this shells
// out to `security list-keychains` — exit 0 plus non-empty output means
// the keychain daemon is up. The probe is intentionally cheap: a single
// subprocess call, no keychain transaction.
func (prodKeyring) Available() bool {
	cmd := exec.Command("security", "list-keychains")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// memKeyring is an in-memory test double. Available() always reports true
// so the ConfigService exercises its full code path without a live host.
type memKeyring struct {
	m map[string]string
}

func newMemKeyring() *memKeyring {
	return &memKeyring{m: make(map[string]string)}
}

func (m *memKeyring) Get(key string) (string, error) {
	v, ok := m.m[key]
	if !ok {
		return "", errNotFound
	}
	return v, nil
}

func (m *memKeyring) Set(key, value string) error {
	m.m[key] = value
	return nil
}

func (m *memKeyring) Delete(key string) error {
	delete(m.m, key)
	return nil
}

func (m *memKeyring) Available() bool {
	return true
}

// failKeyring is a test double that simulates a host with no keyring. It
// is the hammer for the hard-fail contract: any caller that tries to
// persist a secret must see errKeyringUnavailable before touching disk.
type failKeyring struct{}

func (failKeyring) Get(key string) (string, error) {
	return "", errKeyringUnavailable
}

func (failKeyring) Set(key, value string) error {
	return errKeyringUnavailable
}

func (failKeyring) Delete(key string) error {
	return errKeyringUnavailable
}

func (failKeyring) Available() bool {
	return false
}
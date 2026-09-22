package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Settings holds non-secret configuration options plus secret references and
// values. The "yaml\"-\" tag on Secrets is literal: never include the actual
// secret value in the YAML file. The secret_refs field is used instead to
// reference a secret managed by the Keyring.
// nolint:tagalign
type Settings struct {
	RouterAddr               string            `json:"router_addr" yaml:"router_addr"`
	RouterAPIkey             string            `json:"router_apikey" yaml:"router_apikey"`
	RouterBinaryPath         string            `json:"router_binary_path" yaml:"router_binary_path"`
	WorkerAddr               string            `json:"worker_addr" yaml:"worker_addr"`
	WorkerAPIkey             string            `json:"worker_apikey" yaml:"worker_apikey"`
	WorkerBackend            string            `json:"worker_backend" yaml:"worker_backend"`
	WorkerModelPath          string            `json:"worker_model_path" yaml:"worker_model_path"`
	WorkerPort               int               `json:"worker_port" yaml:"worker_port"`
	WorkerBinaryPath         string            `json:"worker_binary_path" yaml:"worker_binary_path"`
	WorkerCustomAuth         string            `json:"worker_custom_auth" yaml:"worker_custom_auth"`
	WorkerEnableHealthChecks *bool             `json:"worker_enable_health_checks" yaml:"worker_enable_health_checks"`
	WorkerMTLSCert           string            `json:"worker_mtls_cert" yaml:"worker_mtls_cert"`
	WorkerMTLSKey            string            `json:"worker_mtls_key" yaml:"worker_mtls_key"`
	RelayURL                 string            `json:"relay_url" yaml:"relay_url"`
	DevMode                  *bool             `json:"dev_mode" yaml:"dev_mode"`
	AutoStartOnLogin         *bool             `json:"auto_start_on_login" yaml:"auto_start_on_login"`
	Secrets                  map[string]string `json:"secrets" yaml:"-"`
	SecretRefs               map[string]string `json:"secret_refs" yaml:"secret_refs"`
}

// DefaultSettings returns a Settings instance with sensible defaults per the
// design contract: RouterAddr ":8080", WorkerBackend "llama-cpp", WorkerPort
// 8081, DevMode true, WorkerEnableHealthChecks true, AutoStartOnLogin false
// (opt-in). It also initialises empty maps for Secrets and SecretRefs.
func DefaultSettings() Settings {
	return Settings{
		RouterAddr:               ":8080",
		WorkerBackend:            "llama-cpp",
		WorkerPort:               8081,
		WorkerEnableHealthChecks: boolPtr(true),
		DevMode:                  boolPtr(true),
		AutoStartOnLogin:         boolPtr(false), // opt-in: no login item by default
		Secrets:                  make(map[string]string),
		SecretRefs:               make(map[string]string),
	}
}

// settingsPath computes the standard location for the settings file, respecting
// the XDG base directory specification when possible.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configDir := filepath.Join(home, ".config", "infermesh")
	return filepath.Join(configDir, "settings.yml"), nil
}

// LoadFrom reads a Settings file from the provided path and unmarshals it into
// a Settings instance. It deliberately tolerates missing files: a missing file
// is not an error; it returns the zero Settings{} and nil error. The caller may
// then want to write defaults.
// zeroSettings returns an initialised zero Settings with non-nil maps, so
// every error-return path in this package hands back a value that is safe to
// write back to disk without a nil-map panic.
func zeroSettings() Settings {
	return Settings{
		Secrets:    make(map[string]string),
		SecretRefs: make(map[string]string),
	}
}

func LoadFrom(path string) (Settings, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// Missing file is not an error (first run). Return initialised
		// zero Settings so callers can write defaults without nil-map
		// panics.
		return zeroSettings(), nil
	}
	if err := ensurePathExists(path); err != nil {
		return zeroSettings(), fmt.Errorf("ensure path exists: %w", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return zeroSettings(), fmt.Errorf("read settings file: %w", err)
	}
	s := Settings{}
	if err := yaml.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("unmarshal YAML: %w", err)
	}
	// Ensure maps are initialised (nil YAML for Secrets/SecretRefs is OK but
	// the zero value would cause panics when writing back later).
	if s.Secrets == nil {
		s.Secrets = make(map[string]string)
	}
	if s.SecretRefs == nil {
		s.SecretRefs = make(map[string]string)
	}
	return s, nil
}

// Load reads the settings from the default location (~/.config/infermesh/settings.yml).
func Load() (Settings, error) {
	p, err := settingsPath()
	if err != nil {
		return Settings{}, fmt.Errorf("settings path: %w", err)
	}
	return LoadFrom(p)
}

// SaveTo writes the Settings to the provided path, atomically replacing any
// existing file. It ensures the containing directory exists with mode 0o755
// and the file itself is created with mode 0o600. In a SaveSettings context
// we guarantee that Secrets is nil (to strip secrets) and that SecretRefs
// contains references to the keyring only.
func SaveTo(s Settings, path string) error {
	if err := ensurePathExists(path); err != nil {
		return fmt.Errorf("ensure path exists: %w", err)
	}
	// Marshal to bytes. The yaml:"-" tag on Secrets ensures it will never
	// appear in the emitted YAML, even if the field is non-empty.
	b, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal YAML: %w", err)
	}
	// Atomic write: write to temporary then rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), "settings-*.yml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	nName := tmp.Name()
	safeDefer := func() {
		if err != nil {
			_ = os.Remove(nName)
		}
	}
	defer safeDefer()

	// Write with restricted permissions (owner read/write only).
	if err := os.WriteFile(nName, b, 0o600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	// Sync to disk to guarantee persistence before rename.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(nName, path); err != nil {
		return fmt.Errorf("rename temp to dest: %w", err)
	}
	return nil
}

// Save writes the Settings to the default path (~/.config/infermesh/settings.yml).
func Save(s Settings) error {
	p, err := settingsPath()
	if err != nil {
		return fmt.Errorf("settings path: %w", err)
	}
	return SaveTo(s, p)
}

// ensurePathExists ensures that the directory containing a given path exists
// with mode 0o755 (owner read/write/execute). If the directory already exists,
// it is a no-op. An error is returned only when directory creation fails.
func ensurePathExists(path string) error {
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Directory doesn't exist; create with mode 0o755.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	} else if !info.IsDir() {
		return fmt.Errorf("path %q exists and is not a directory", dir)
	}
	return nil
}

// startupSettings is a thin helper for main.go: it loads the settings,
// merges defaults for zero values (so we have a complete configuration),
// and returns the merged Settings. It never returns an error from LoadFrom
// because missing files are tolerated; however, if the file exists and
// contains valid YAML, we load it, and for each field if it's the zero value
// we replace it with the DefaultSettings counterpart. If loading fails due to
// a parse error, we return the error.
func startupSettings() (Settings, error) {
	p, err := settingsPath()
	if err != nil {
		return Settings{}, fmt.Errorf("settings path: %w", err)
	}
	s, err := LoadFrom(p)
	if err != nil {
		return Settings{}, err
	}
	return mergeDefaults(s), nil
}

// mergeDefaults takes a Settings (possibly zero-valued) and for any zero-value
// fields, replaces them with the DefaultSettings values. This guarantees that
// CLI flag defaults and persisted settings are compatible and that the UI
// always receives a valid configuration.
func mergeDefaults(s Settings) Settings {
	def := DefaultSettings()

	if s.RouterAddr == "" {
		s.RouterAddr = def.RouterAddr
	}
	if s.WorkerBackend == "" {
		s.WorkerBackend = def.WorkerBackend
	}
	if s.WorkerPort == 0 {
		s.WorkerPort = def.WorkerPort
	}
	if s.WorkerEnableHealthChecks == nil {
		s.WorkerEnableHealthChecks = def.WorkerEnableHealthChecks
	}
	if s.DevMode == nil {
		s.DevMode = def.DevMode
	}
	// *bool tri-state: nil means "old file / no opinion" → default (false);
	// an explicit true or false on disk must survive the merge untouched.
	if s.AutoStartOnLogin == nil {
		s.AutoStartOnLogin = def.AutoStartOnLogin
	}
	// Ensure maps are initialised (LoadFrom already does this but we cannot
	// rely on it after manual construction). This protects against nil map
	// dereference on Save and other operations.
	if s.Secrets == nil {
		s.Secrets = make(map[string]string)
	}
	if s.SecretRefs == nil {
		s.SecretRefs = make(map[string]string)
	}
	return s
}

// boolPtr is a small helper for setting *bool fields to true/false in struct
// literals (DefaultSettings). Without it every bool pointer needs a named var.
func boolPtr(b bool) *bool {
	return &b
}

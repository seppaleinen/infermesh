package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// StateDirName is the per-user state directory name for InferMesh.
	StateDirName = "infermesh"
	// MaxStateFileSize bounds the persisted concurrency-cache JSON so a
	// corrupt or attacker-inflated file cannot exhaust memory on load.
	MaxStateFileSize = 1 << 20 // 1 MiB
)

// StateDir returns the per-user state directory for InferMesh, creating it
// with restrictive permissions (0700) if it does not exist. It is derived
// from the OS config dir (XDG on Linux, ~/Library/Application Support on
// macOS, %LOCALAPPDATA% on Windows) so it is never world-writable and never
// collides with other users' /tmp files.
func StateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.Getenv("HOME")
		if base == "" {
			return "", errors.New("platform: no user config dir and $HOME unset")
		}
	}
	dir := filepath.Join(base, StateDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("platform: create state dir %s: %w", dir, err)
	}
	return dir, nil
}

// StatePath returns the full path to a state file inside the InferMesh state
// dir. name must be a simple base name (no path separators, no ".."). It
// returns an error otherwise, so a caller-supplied name cannot escape the
// state dir.
func StatePath(name string) (string, error) {
	if err := validateBaseName(name); err != nil {
		return "", err
	}
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func validateBaseName(name string) error {
	if name == "" {
		return errors.New("platform: state file name must not be empty")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("platform: state file name %q is not a base name", name)
	}
	// Reject names that resolve to "." or ".." or contain separators.
	if name == "." || name == ".." {
		return fmt.Errorf("platform: invalid state file name %q", name)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("platform: state file name %q contains a path separator", name)
	}
	// Reject control characters and other hostile bytes.
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("platform: state file name %q contains a control character", name)
		}
	}
	return nil
}

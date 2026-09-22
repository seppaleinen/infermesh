package main

import (
	"fmt"
	"os/exec"
)

// cmdRunner is the narrow seam behind exec.Command for autostart registration;
// tests substitute a fake so registration functions never touch a real shell.
// It mirrors the cmdBuilder seam in supervisor_process.go.
type cmdRunner interface {
	// Run executes name with args and returns combined output + error.
	Run(name string, args ...string) (string, error)
}

// execRunner is the production cmdRunner backed by os/exec. Commands are
// invoked directly (argv arrays), never through a shell.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// autostarter is the OS registration backend AutoStartService drives.
type autostarter interface {
	Enable() error
	Disable() error
	IsEnabled() (bool, error)
}

// AutoStartService is the Wails service that exposes login-item auto-start
// registration to the frontend (issue #45).
//
// It deliberately owns only the OS registration: persistence of the
// AutoStartOnLogin preference belongs to ConfigService, so an OS-level
// registration failure must not fail the settings save. Such failures are
// therefore returned as a human-readable warning string with a nil error;
// the error return is reserved for internal cases (e.g. an unwired backend).
type AutoStartService struct {
	backend autostarter
}

// NewAutoStartService creates the service around the provided platform
// backend (see newPlatformAutostart). A nil backend degrades to a warning
// mode instead of panicking, so hosts without auto-start support still boot.
func NewAutoStartService(a autostarter) *AutoStartService {
	return &AutoStartService{backend: a}
}

// ServiceName implements the Wails service interface for binding discovery.
func (*AutoStartService) ServiceName() string { return "AutoStartService" }

// IsAutoStartEnabled reports the ACTUAL OS registration state (not the
// persisted preference). With no backend wired it returns (false, error).
func (s *AutoStartService) IsAutoStartEnabled() (bool, error) {
	if s == nil || s.backend == nil {
		return false, fmt.Errorf("auto-start backend is not wired on this host")
	}
	return s.backend.IsEnabled()
}

// SetAutoStart registers (true) or removes (false) the login item with the
// OS. On success it returns ("", nil). OS failures come back as a non-empty
// human warning string with a nil error so the caller-owned settings save is
// never failed by the registration step; the error return is reserved for
// the internal unwired-backend case, which is also reported as a warning
// rather than an error to keep the frontend flow simple.
func (s *AutoStartService) SetAutoStart(enabled bool) (string, error) {
	if s == nil || s.backend == nil {
		return "auto-start is not configured on this host", nil
	}
	var err error
	if enabled {
		err = s.backend.Enable()
	} else {
		err = s.backend.Disable()
	}
	if err == nil {
		return "", nil
	}
	if enabled {
		return fmt.Sprintf("Auto-start could not be enabled: %v", err), nil
	}
	return fmt.Sprintf("Auto-start could not be disabled: %v", err), nil
}

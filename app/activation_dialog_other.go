//go:build !darwin && !linux && !windows

package main

import "fmt"

// platformNotifyAlreadyRunning has no implementation on this platform, but
// must still exist so the package links and main()'s second-launch path
// reports a handled failure instead of a build error. See
// activation_dialog.go for the full contract.
func platformNotifyAlreadyRunning() error {
	return fmt.Errorf("no already-running indication implemented for this platform: %w", ErrFallbackUnavailable)
}

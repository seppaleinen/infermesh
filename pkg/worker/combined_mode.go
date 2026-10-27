package worker

import (
	"errors"
	"fmt"
)

// ValidateCombinedFlags performs fail-fast validation for the combined
// router+worker mode, returning an error (for exit 2 with a clear message)
// when the flags cannot run in one process. Kept for backward compatibility
// with callers that still reference the old unified binary; the separate
// router and worker binaries never invoke it.
func ValidateCombinedFlags(prodMode, workerMode bool, routerPort, workerPort int, backend string) error {
	if prodMode && workerMode {
		return errors.New("--worker is only supported in dev mode; use 'router --dev-mode --worker'")
	}
	if !workerMode {
		return nil
	}
	if routerPort == workerPort {
		return fmt.Errorf("worker port %d collides with router port %d; choose a different --port", workerPort, routerPort)
	}
	if bp := backendPort(backend); bp == workerPort {
		return fmt.Errorf("worker port %d collides with the %s backend's hardcoded port %d; choose a different --port", workerPort, backend, bp)
	}
	return nil
}

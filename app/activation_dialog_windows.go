//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	mbOK          = 0x00000000
	mbIconWarning = 0x00000030
	mbTaskModal   = 0x00002000

	// alreadyRunningTimeoutMs auto-dismisses the box with no user
	// interaction, mirroring the ~10s bounded darwin alert.
	alreadyRunningTimeoutMs = 10000
)

var (
	user32                = syscall.NewLazyDLL("user32.dll")
	procMessageBoxTimeout = user32.NewProc("MessageBoxTimeoutW")
)

// platformNotifyAlreadyRunning shows a time-bounded modal warning via
// MessageBoxTimeoutW (user32, present since XP — no golang.org/x/sys
// needed). See activation_dialog.go for the full contract. The stdlib-only
// syscall has no Unix-only surface, so this file stays portable-safe.
func platformNotifyAlreadyRunning() error {
	if err := procMessageBoxTimeout.Find(); err != nil {
		return fmt.Errorf("MessageBoxTimeoutW unavailable: %w", ErrFallbackUnavailable)
	}

	title, err := syscall.UTF16PtrFromString("InferMesh")
	if err != nil {
		return fmt.Errorf("encode alert title: %w", err)
	}
	message, err := syscall.UTF16PtrFromString("InferMesh is already running. This message closes automatically.")
	if err != nil {
		return fmt.Errorf("encode alert text: %w", err)
	}

	ret, _, callErr := procMessageBoxTimeout.Call(
		0, // no owner window; MB_TASKMODAL makes it application-modal
		uintptr(unsafe.Pointer(message)),
		uintptr(unsafe.Pointer(title)),
		mbOK|mbIconWarning|mbTaskModal,
		0, // language ID: system default
		alreadyRunningTimeoutMs,
	)
	if ret == 0 {
		// 0 = the call itself failed (IDOK=1, timeout=32000 are both fine).
		return fmt.Errorf("MessageBoxTimeoutW failed: %w", callErr)
	}
	return nil
}

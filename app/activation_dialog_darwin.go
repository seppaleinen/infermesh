//go:build darwin

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// platformNotifyAlreadyRunning shows the "already running" indication by
// running /usr/bin/osascript OUT of process (see activation_dialog.go for
// the full contract).
//
// Deliberately not an in-process cgo NSAlert: in a non-GUI/Background
// launchd session AppKit may never spin the modal run loop (hang) or abort
// the process (non-zero exit) — either would break the second-instance
// contract of "never hang, always exit 0". A child osascript cannot do
// either to its parent: CommandContext kills it at fallbackNotifyTimeout,
// and any failure is returned as an error while this process still exits 0.
//
// `giving up after 10` makes the dialog self-dismiss with no user
// interaction (the display dialog command itself returns after 10s), and
// the parent's fallbackNotifyTimeout is the hard upper bound on top of
// that.
const osascriptAlert = `display dialog "InferMesh is already running. This message closes automatically." with title "InferMesh" buttons {"OK"} default button "OK" giving up after 10`

func platformNotifyAlreadyRunning() error {
	ctx, cancel := context.WithTimeout(context.Background(), fallbackNotifyTimeout)
	defer cancel()

	// Absolute path: osascript ships with macOS; no PATH lookup needed.
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", osascriptAlert)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Includes ctx kill at fallbackNotifyTimeout ("signal: killed") and
		// non-GUI-session failures — reported, never fatal to this process.
		return fmt.Errorf("osascript already-running alert: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

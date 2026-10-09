//go:build linux

package main

import (
	"context"
	"fmt"
	"os/exec"
)

// platformNotifyAlreadyRunning shows the "already running" indication via
// the first desktop helper that actually works (zenity, then kdialog),
// bounded by fallbackNotifyTimeout (see activation_dialog.go for the full
// contract). A helper that exists but fails to render is not fatal — the
// next candidate is tried, and the last failure is reported.
func platformNotifyAlreadyRunning() error {
	ctx, cancel := context.WithTimeout(context.Background(), fallbackNotifyTimeout)
	defer cancel()

	const text = "InferMesh is already running.\nThis message closes automatically."
	candidates := []struct {
		name string
		args []string
	}{
		{"zenity", []string{"--warning", "--title=InferMesh", "--text=" + text}},
		{"kdialog", []string{"--title", "InferMesh", "--warning", text}},
	}

	var lastErr error
	for _, c := range candidates {
		path, err := exec.LookPath(c.name)
		if err != nil {
			continue
		}
		if err := exec.CommandContext(ctx, path, c.args...).Run(); err != nil {
			// Helper exists but could not render (e.g. no display): try
			// the next candidate, remembering the concrete failure.
			lastErr = fmt.Errorf("already-running indication via %s: %w", c.name, err)
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("neither zenity nor kdialog found: %w", ErrFallbackUnavailable)
}

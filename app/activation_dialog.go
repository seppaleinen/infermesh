package main

import (
	"errors"
	"time"
)

// ErrFallbackUnavailable reports that this platform/session has no usable
// way to display the visible "InferMesh is already running" indication —
// e.g. no zenity/kdialog on Linux, a non-GUI session, or an unsupported
// platform. platformNotifyAlreadyRunning should return an error wrapping it
// when nothing could be shown at all, so callers can distinguish "shown"
// (nil) from "attempted but failed" and from "nothing available".
var ErrFallbackUnavailable = errors.New("no already-running indication available")

// fallbackNotifyTimeout is the hard out-of-process bound on the fallback
// alert on every platform that shells out (darwin osascript, linux
// zenity/kdialog): exec.CommandContext kills the helper at this deadline,
// so the second instance can never hang on it. The helpers show a
// ~10s self-dismissing dialog inside this outer bound.
const fallbackNotifyTimeout = 12 * time.Second

// platformNotifyAlreadyRunning — per-platform contract (implemented in
// activation_dialog_{darwin,linux,windows,other}.go so the symbol always
// exists):
//
//   - Shows a visible "InferMesh is already running" indication in the
//     second instance — best-effort: it may not render at all in a
//     non-GUI/Background session, but the call must stay bounded and
//     non-fatal either way.
//   - MUST return within its own time bound even with no user interaction
//     (12s hard out-of-process bound on darwin/linux via
//     exec.CommandContext killing the helper; 10s MessageBoxTimeoutW on
//     windows): never hang, never abort the process — every second-instance
//     path still exits 0.
//   - Returns nil when the helper reported success (dialog dispatched and
//     self-dismissing via its own ~10s give-up), an error when the attempt
//     failed (helper exited non-zero or timed out), or an error wrapping
//     ErrFallbackUnavailable when nothing is available at all (no helper
//     binary / unsupported platform).
//   - GUI-free tests never invoke it; they pass recording stubs to
//     handleSecondLaunch instead.
//
// Implemented per-platform (build tags) as platformNotifyAlreadyRunning()
// in activation_dialog_{darwin,linux,windows,other}.go.

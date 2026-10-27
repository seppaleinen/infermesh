//go:build !server

package main

import (
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// closeToTrayWindow is the slice of *application.WebviewWindow that
// registerCloseToTray needs. It exists so the hook wiring can be unit-tested
// with a stub, without a live window (issue #49). It is NOT a Wails service
// and must never be added to application.Options.Services.
type closeToTrayWindow interface {
	Hide() application.Window
	RegisterHook(events.WindowEventType, func(*application.WindowEvent)) func()
}

// registerCloseToTray makes the red-X / OS close hide the window to the tray
// instead of destroying it (issue #49).
//
// RegisterHook runs synchronously BEFORE the default WindowClosing listeners
// (wails v3.0.0 webview_window.go:366 marks the window destroyed +
// closes it; after that Show() is a no-op and the tray can never re-show the
// window). Calling e.Cancel() prevents those listeners from running, so the
// window survives hidden and tray Open / click-to-toggle keep working.
//
// NOTE: any future "close really quits" path must call app.Quit(), never
// win.Close() — a programmatic Close() also fires this hook. Quit and Cmd+Q
// go through App.Quit(), not the WindowClosing event path, so OnShutdown
// supervisor cleanup is unaffected.
func registerCloseToTray(win closeToTrayWindow) {
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})
}

func main() {
	// Single-instance guard: flock on a per-user lock file. Held for the
	// process lifetime; the kernel drops it on any exit path (including
	// kill -9), so no cleanup is needed beyond this defer.
	lock, err := acquireInstanceLock(lockFilePath())
	gotLock := err == nil // may run fail-open below without the lock — must not own the activation socket then
	if errors.Is(err, ErrAlreadyRunning) {
		// Second instance (issue #93): first try to bring the running
		// instance's window to the front over the activation socket next
		// to the lock file; if it cannot be signalled (no listener /
		// wedged / old build), show the visible time-bounded
		// "already running" indication instead of exiting silently.
		outcome, actErr := handleSecondLaunch(activationSocketPath(lockFilePath()), platformNotifyAlreadyRunning)
		log.Printf("another InferMesh instance is already running (outcome=%s): %v; exiting", outcome, actErr)
		return // exit(0): every second-instance path exits 0 for LaunchAgent benignness
	}
	if err != nil {
		// Fail open: the guard is UX, not a security control.
		log.Printf("single-instance lock unavailable, continuing without guard: %v", err)
	}
	defer lock.release() //nolint:errcheck // keeps *os.File referenced + unlocks on main() return

	_, settingsPath, supervisor, rc, autoStartSvc, _, _ := initApp()

	services := makeServices(settingsPath, supervisor, rc, autoStartSvc)
	opts := makeBaseOptions(services)

	app := application.New(opts)

	// Window setup
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "InferMesh Desktop",
		Width:            1000,
		Height:           618,
		BackgroundColour: application.NewRGB(6, 7, 15),
		URL:              "/",
	})

	// Red-X / OS close hides to tray instead of destroying the window
	// (issue #49); see registerCloseToTray for the wails hook ordering
	// details. Must be registered before app.Run so it precedes the default
	// WindowClosing listeners.
	registerCloseToTray(win)

	// Readiness gate for the activation listener (issue #93): the listener
	// below starts before app.Run(), but globalApplication.impl is only set
	// inside Run() (wails application.go:664) and the pending windows are
	// run during startup — until then win.Show() silently returns on its
	// `globalApplication.impl == nil` guard and win.Focus() on its
	// `w.impl == nil` guard (webview_window.go), while
	// application.InvokeSync would nil-panic (dispatchOnMainThread reads
	// a.impl). ApplicationStarted is emitted from the platform launch event
	// (darwin: Mac.ApplicationDidFinishLaunching mapped in
	// events_common_darwin.go) only after Run() has started the event loop
	// and run the pending windows, so once appReady is true both calls are
	// effective, and the ack can be gated on it.
	var appReady atomic.Bool
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		appReady.Store(true)
	})

	// Single-instance activation listener (issue #93): runs only after the
	// window exists and the readiness flag is registered (both before
	// app.Run), so an incoming activation request can never race ahead of
	// a window to show. A request arriving before ApplicationStarted gets
	// NO ack (the callback returns false) and the second instance falls
	// back to the visible alert instead of exiting silently.
	if !gotLock {
		// Fail-open instance: we do not hold the flock, so the socket path
		// may belong to the true lock holder. startActivationListener
		// unlinks its path before binding and again on Close — never touch
		// it here, or we would steal the live instance's socket.
		log.Print("single-instance lock not held; skipping activation listener")
	} else {
		actServer, err := startActivationListener(activationSocketPath(lockFilePath()), func() bool {
			if !appReady.Load() {
				// App event loop not up yet: Show/Focus would silently
				// no-op, so report failure — no ack means the second
				// instance shows the visible alert (non-silent).
				return false
			}
			// Run both calls on the application thread: Show() writes
			// w.options.Hidden and reads w.impl on the calling goroutine
			// (webview_window.go Show/Focus), so concurrent accept
			// goroutines would race main-thread writes to w.impl. Nested
			// InvokeSync is a no-op when already on the main thread
			// (application.go dispatchOnMainThread). Never called while
			// !appReady — see the gate above.
			application.InvokeSync(func() {
				win.Show()
				win.Focus()
			})
			return true
		})
		if err != nil {
			log.Printf("single-instance activation listener unavailable; second launches will show a dialog: %v", err)
		} else {
			defer actServer.Close() //nolint:errcheck
		}
	}

	// On shutdown, tear down any supervised children so the user's machine is
	// left in the same state the app found it in. Uses the SAME supervisor
	// instance that was registered as a service (see above).
	app.OnShutdown(func() {
		supervisor.StopAll()
	})

	// System Tray setup
	tray := app.SystemTray.New()
	// The macOS menu-bar icon ONLY renders if an image is attached to the
	// NSStatusItem. SetTemplateIcon marks the image as a template (black +
	// alpha only) so macOS can adapt it for light/dark menu bars. Linux
	// setTemplateIcon falls back to setIcon; Windows setTemplateIcon is a
	// NO-OP — so SetIcon must be called first on every platform.
	tray.SetIcon(trayIcon)
	tray.SetTemplateIcon(trayIcon)
	// AttachWindow gives the tray its Wails smart defaults: left-click toggles
	// the window (including the Windows close-state dance) and right-click
	// opens the menu — no explicit OnClick handler needed. Verified against
	// wails v3.0.0 pkg/application/systemtray.go (applySmartDefaults).
	tray.AttachWindow(win)

	menu := application.NewMenu()

	// Disabled status line, refreshed by the status ticker below.
	statusItem := menu.Add("Status: Stopped")
	statusItem.SetEnabled(false)
	menu.AddSeparator()
	menu.Add("Open").OnClick(func(*application.Context) {
		win.Show()
		win.Focus()
	})
	menu.Add("Hide").OnClick(func(*application.Context) {
		win.Hide()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) {
		app.Quit()
	})

	tray.SetMenu(menu)
	tray.SetTooltip("InferMesh — Stopped")

	// Heartbeat + tray status: a single 5s ticker drives both the frontend
	// "time" demo event and the tray status/tooltip summary. Status() probes
	// the router/worker with a bounded 2s timeout, so a tick can block this
	// goroutine briefly.
	//
	// The tray updates MUST run on the application main thread: unlike
	// SystemTray.SetTooltip, which marshals itself via InvokeSync
	// (systemtray.go:287-295), MenuItem.SetLabel drives the native NSMenu
	// through cgo/AppKit directly (menuitem.go:312-318 →
	// menuitem_darwin.go setLabel), so calling it off-main is
	// undefined-behaviour. One InvokeSync hops both label + tooltip onto the
	// main thread; the tooltip's own InvokeSync then runs inline (dispatch
	// is a no-op when already on the main thread).
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			app.Event.Emit("time", time.Now().Format(time.RFC1123))
			summary := "Stopped"
			if st, err := supervisor.Status(); err == nil {
				summary = summarizePoolStatus(st)
			}
			application.InvokeSync(func() {
				statusItem.SetLabel("Status: " + summary)
				tray.SetTooltip("InferMesh — " + summary)
			})
		}
	}()

	err = app.Run()
	if err != nil {
		log.Fatal(err)
	}
}

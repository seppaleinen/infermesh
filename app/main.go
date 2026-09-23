package main

import (
	"embed"
	"errors"
	"log"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/trayTemplate.png
var trayIcon []byte

// summarizePoolStatus reduces a SupervisorStatus snapshot to the short human
// sentence shown on the tray status line and in the tray tooltip.
func summarizePoolStatus(st SupervisorStatus) string {
	routerUp := st.Router.State == StateRunning
	workerUp := st.Worker.State == StateRunning
	switch {
	case routerUp && workerUp:
		return "Router + worker running"
	case routerUp:
		return "Router running"
	case workerUp:
		return "Worker running"
	default:
		return "Stopped"
	}
}

func main() {
	// Single-instance guard: flock on a per-user lock file. Held for the
	// process lifetime; the kernel drops it on any exit path (including
	// kill -9), so no cleanup is needed beyond this defer.
	lock, err := acquireInstanceLock(lockFilePath())
	if errors.Is(err, ErrAlreadyRunning) {
		log.Print("another InferMesh instance is already running; exiting")
		return // exit(0): benign for LaunchAgent-triggered relaunch
	}
	if err != nil {
		// Fail open: the guard is UX, not a security control.
		log.Printf("single-instance lock unavailable, continuing without guard: %v", err)
	}
	defer lock.release() //nolint:errcheck // keeps *os.File referenced + unlocks on main() return

	settings, err := startupSettings()
	if err != nil {
		log.Fatalf("failed to load startup settings: %v", err)
	}

	settingsPath, err := settingsPath()
	if err != nil {
		log.Fatalf("failed to resolve settings path: %v", err)
	}

	// Build the Supervisor ONCE, before application.New: the same instance is
	// registered as a Wails service AND used by OnShutdown, so StopAll() on
	// shutdown actually sees the children that were started. (A second
	// instance would own none of them and StopAll would be a no-op.)
	supervisor := NewSupervisor(newProdKeyring(), settingsPath, "", "")

	// Auto-start backend registers this very binary as a login item. If the
	// executable path cannot be resolved, fall back to an empty path — the
	// backend then fails at registration time as a user-visible warning
	// instead of crashing the app at startup.
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("cannot resolve app executable path, auto-start registration will warn: %v", err)
		exePath = ""
	}
	autoStartSvc := NewAutoStartService(newPlatformAutostart(execRunner{}, exePath))

	app := application.New(application.Options{
		Name:        "InferMesh",
		Description: "Visual manager for InferMesh pool",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// RouterClient exposes the /v1/workers HTTP projection to the Vue UI.
		// Bound via application.NewService; methods are auto-discovered by
		// reflect in pkg/application/bindings.go:getMethods.
		// ConfigService provides access to persistent secrets via the OS keyring.
		// Supervisor launches and manages the headless router/worker binaries.
		// AutoStartService registers/removes the OS login item (issue #45); it
		// must stay in this list so `wails3 generate bindings` discovers it.
		Services: []application.Service{
			application.NewService(NewRouterClient(settings.RouterAddr)),
			application.NewService(NewConfigService(newProdKeyring(), settingsPath)),
			application.NewService(supervisor),
			application.NewService(autoStartSvc),
		},
	})

	// Window setup
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "InferMesh Desktop",
		Width:            1000,
		Height:           618,
		BackgroundColour: application.NewRGB(6, 7, 15),
		URL:              "/",
	})

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
	// wails v3.0.0-beta.24 pkg/application/systemtray.go (applySmartDefaults).
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

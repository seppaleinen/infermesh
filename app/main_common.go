package main

import (
	"embed"
	"log"
	"os"
	"path/filepath"

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

// initApp builds common application state: settings, supervisor, routerClient,
// autoStartService. Returns (settings, settingsPath, supervisor, rc, autoStartSvc, binaryDir, exePath).
func initApp() (Settings, string, *Supervisor, *RouterClient, *AutoStartService, string, string) {
	settings, err := startupSettings()
	if err != nil {
		log.Fatalf("failed to load startup settings: %v", err)
	}

	settingsPath, err := settingsPath()
	if err != nil {
		log.Fatalf("failed to resolve settings path: %v", err)
	}

	var exePath string
	exePath, err = os.Executable()
	if err != nil {
		log.Printf("cannot resolve app executable path, auto-start registration will warn: %v", err)
		exePath = ""
	}

	binaryDir := ""
	if exePath != "" {
		if root, err := filepath.Abs(filepath.Join(filepath.Dir(exePath), "..", "..", "..", "..", "..")); err == nil {
			binaryDir = root
		} else if root, err := filepath.Abs(filepath.Join(filepath.Dir(exePath), "..", "..", "..", "..")); err == nil {
			binaryDir = root
		} else if root, err := filepath.Abs(filepath.Join(filepath.Dir(exePath), "..", "..", "..")); err == nil {
			binaryDir = root
		}
	}

	supervisor := NewSupervisorService(newProdKeyring(), settingsPath, binaryDir, "")
	rc := NewRouterClient(settings.RouterAddr)
	supervisor.SetRouterClient(rc)
	autoStartSvc := NewAutoStartService(newPlatformAutostart(execRunner{}, exePath))

	return settings, settingsPath, supervisor, rc, autoStartSvc, binaryDir, exePath
}

// makeServices returns the shared service list.
func makeServices(settingsPath string, supervisor *Supervisor, rc *RouterClient, autoStartSvc *AutoStartService) []application.Service {
	return []application.Service{
		application.NewService(rc),
		application.NewService(func() *ConfigService {
			c := NewConfigService(newProdKeyring(), settingsPath)
			c.SetSupervisor(supervisor)
			return c
		}()),
		application.NewService(supervisor),
		application.NewService(autoStartSvc),
	}
}

// makeBaseOptions returns the base application.Options shared by both builds.
func makeBaseOptions(services []application.Service) application.Options {
	return application.Options{
		Name:        "InferMesh",
		Description: "Visual manager for InferMesh pool",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Services: services,
	}
}

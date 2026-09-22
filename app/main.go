package main

import (
	"embed"
	"log"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/trayTemplate.png
var trayIcon []byte

func main() {
	app := application.New(application.Options{
		Name:        "InferMesh",
		Description: "Visual manager for InferMesh pool",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// RouterClient exposes the /v1/workers HTTP projection to the Vue UI.
		// Bound via application.NewService; methods are auto-discovered by
		// reflect in pkg/application/bindings.go:getMethods.
		Services: []application.Service{
			application.NewService(NewRouterClient(defaultRouterURL)),
		},
	})

	// Window setup
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "InferMesh Desktop",
		Width:  1000,
		Height: 618,
		BackgroundColour: application.NewRGB(6, 7, 15),
		URL:             "/",
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
	menu := application.NewMenu()

	menu.Add("Open").OnClick(func(*application.Context) {
		win.Show()
		win.Focus()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) {
		app.Quit()
	})

	tray.SetMenu(menu)

	// Background task simulation
	go func() {
		for {
			time.Sleep(time.Second * 5)
			app.Event.Emit("time", time.Now().Format(time.RFC1123))
		}
	}()

	err := app.Run()
	if err != nil {
		log.Fatal(err)
	}
}

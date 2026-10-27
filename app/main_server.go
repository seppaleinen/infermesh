//go:build server

package main

import (
	"log"
	"os"
	"strconv"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	_, settingsPath, supervisor, rc, autoStartSvc, _, _ := initApp()

	services := makeServices(settingsPath, supervisor, rc, autoStartSvc)
	opts := makeBaseOptions(services)

	// Server options
	host := "127.0.0.1"
	port := 8080
	if p := os.Getenv("WAILS_SERVER_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	opts.Server = application.ServerOptions{
		Host: host,
		Port: port,
	}

	app := application.New(opts)
	app.OnShutdown(func() {
		supervisor.StopAll()
	})

	err := app.Run()
	if err != nil {
		log.Fatal(err)
	}
}

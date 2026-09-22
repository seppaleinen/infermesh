module github.com/seppaleinen/infermesh/app

go 1.27

require (
	github.com/seppaleinen/infermesh v0.0.0-00010101000000-000000000000
	github.com/wailsapp/wails/v3 v3.0.0-beta.24
	github.com/zalando/go-keyring v0.2.6
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/seppaleinen/infermesh => ../

require (
	al.essio.dev/pkg/shellescape v1.6.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

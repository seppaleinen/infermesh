# InferMesh Desktop

Native manager window + system tray for the InferMesh inference pool: a
Dashboard of pool status, Settings for the router/worker (secrets go to the OS
keyring), a process supervisor that starts/stops the headless `router` and
`worker` binaries, autostart (login item), and a tray with Open/Hide/Quit and
live status. Built with [Wails v3](https://v3.wails.io/) (`v3.0.0`).

This is a **separate Go module** (`replace github.com/seppaleinen/infermesh => ../`
in `app/go.mod`) — root `go test` / `make lint` do not descend into it.

## Structure

- `main.go` — Wails application: services (RouterClient, ConfigService,
  Supervisor, AutoStartService), window, system tray.
- `*_service.go` — backend services (config storage, keyring, router client,
  process supervisor, autostart).
- `frontend/` — Vue 3 + Vite + TS UI, bundled into the binary at build time.
- `build/` — platform tasks and assets: `config.yml` (product identity),
  `darwin/` (Info.plist, icons), `linux/` (nfpm/appimage), `windows/`
  (info.json, manifest, msix, nsis).
- `tools/gen-trayicon/` — stdlib-only generator for the tray template icon.

## Commands

Run from this directory with the `wails3` CLI (pinned `v3.0.0`):

```bash
wails3 task build            # production host-OS binary -> bin/app
wails3 task dev              # dev mode with hot reload
wails3 task run              # run the built binary
APP_NAME=InferMesh wails3 task package   # macOS .app bundle (ad-hoc signed)
APP_NAME=InferMesh wails3 task darwin:package:dmg   # macOS .dmg
```

`APP_NAME=InferMesh` is the default product identity for packaging. The root
Makefile provides shortcuts that also prepend `$(go env GOPATH)/bin` to PATH:

- `make build-app` → `bin/app/infermesh-app`
- `make package-app` / `make package-app-dmg` / `make package-app-linux` /
  `make package-app-windows`
- `make test-app-smoke`

## Tests

```bash
go test ./...                     # GUI-free unit tests (smoke file excluded)
go test -tags smoke -count=1 -v -run '^TestAppSmoke$$' .   # smoke test
```

The smoke test (`smoke_test.go`, build tag `smoke`) launches the built app,
waits through a 15s boot window, runs advisory GUI probes, and tears the
process down. Liveness is the pass criterion. Env overrides: `SKIP=1` /
`INFERMESH_SMOKE_SKIP=1` skip; `INFERMESH_APP_BIN` points at the binary.

### E2E Testing with Playwright

Prerequisites:
- Node 22
- npm
- Go 1.27+
- wails3 v3.0.0

How to run:

```bash
# Install Playwright browsers (first time)
cd app/frontend
npx playwright install --with-deps chromium

# Run E2E tests (starts mock router + Wails server)
npx playwright test

# Run with UI
npx playwright test --ui
```

Running locally with Wails server:
The Playwright `webServer` command builds the app in server mode (`-tags server,production`) and injects router settings via a temporary `settings.yml` at `$PLAYWRIGHT_TEMP_HOME/.config/infermesh/settings.yml` with `router_addr: http://127.0.0.1:8081`. The mock router (`app/mock/mockRouter.js`) is started concurrently on port 8081 before the Wails server binds to 8080.

What the tests cover:
- `app-startup.spec.ts` — page loads, header renders, config loads in server mode
- `router-connection.spec.ts` — unreachable router shows error card, reachable mock router shows "Router running" status
- `worker-listing.spec.ts` — mock workers array renders cards with ID, hostname, loaded models, status, in-flight counts
- `model-browsing.spec.ts` — `/meta/models/popular` list renders and filtering works
- `request-monitoring.spec.ts` — in-flight requests table renders with status and duration
- `settings.spec.ts` — settings form loads, validation shows error on invalid URL

Notes:
Playwright tests run against a Wails server build (`-tags server,production`) with a mock Node router on port 8081. The mock router serves canned responses matching `pkg/router` types for workers, models, requests, stats, aliases, and auto-routing. Tests verify UI rendering and basic workflows without real ML models.

CI runs E2E tests on `ubuntu-latest` in parallel with app-frontend.

## Packaging notes

- **macOS** — native (CGO/cocoa). `wails3 task package` produces `bin/InferMesh.app`
  (ad-hoc signed); `darwin:package:dmg` produces `bin/InferMesh.dmg`.
- **Linux** — requires a Linux host with webkit2gtk (GTK4 + WebKitGTK 6.0) or
  the wails-cross Docker image; `wails3 task package` produces
  AppImage + deb/rpm/aur. `make package-app-linux` fails with guidance on
  non-Linux hosts.
- **Windows** — `wails3 task build GOOS=windows ARCH=amd64` (CGO_ENABLED=0) is a
  compile-check and **currently fails**: the process supervisor uses Unix-only
  syscalls (`syscall.Setpgid` / `syscall.Kill` in `supervisor_process.go`) that
  don't exist on Windows, and Windows desktop support is deferred (AGENTS.md).
  The packaging config already carries the InferMesh identity (info.json,
  manifest, msix, nsis), so `bin/app/InferMesh.exe` and the NSIS installer
  (`makensis` required) are produced automatically once Windows support lands.

## Prerequisites

- Go 1.27+
- Node + npm (first build installs frontend deps)
- CGO toolchain (Xcode CLT on macOS)
- `wails3` at `v3.0.0`:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
```
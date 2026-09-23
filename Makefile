# Build the router and worker binaries (separate from the relay broker)
build-router:
	go build -o bin/infermesh-router ./cmd/router

build-worker:
	go build -o bin/infermesh-worker ./cmd/worker

build: build-router build-worker

# Build the relay broker binary (outbound-only WebSocket relay)
relay:
	go build -o bin/infermesh-relay ./cmd/relay

# Build static binaries (CGO_ENABLED=0 for cross-platform compatibility)
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-router-linux-amd64 ./cmd/router
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/infermesh-router-linux-arm64 ./cmd/router
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o bin/infermesh-router-darwin-amd64 ./cmd/router
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-router-darwin-arm64 ./cmd/router
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/infermesh-router-windows-amd64.exe ./cmd/router
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-worker-linux-amd64 ./cmd/worker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/infermesh-worker-linux-arm64 ./cmd/worker
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o bin/infermesh-worker-darwin-amd64 ./cmd/worker
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-worker-darwin-arm64 ./cmd/worker
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/infermesh-worker-windows-amd64.exe ./cmd/worker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-relay-linux-amd64 ./cmd/relay
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/infermesh-relay-linux-arm64 ./cmd/relay
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o bin/infermesh-relay-darwin-amd64 ./cmd/relay
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-relay-darwin-arm64 ./cmd/relay
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/infermesh-relay-windows-amd64.exe ./cmd/relay

# --- Desktop app (Wails v3) -------------------------------------------------
# wails3 is installed to $(go env GOPATH)/bin by `go install`; prepend it to
# PATH for this Makefile AND for go-task's child shells (the Taskfiles shell
# out to `wails3 generate ...` / `wails3 tool has ...` internally).
export PATH := $(shell go env GOPATH)/bin:$(PATH)

APP_NAME    ?= InferMesh
APP_BIN_DIR ?= bin/app

# Pinned by app/go.mod and AGENTS.md — keep in sync.
WAILS3_VERSION ?= v3.0.0-beta.24

# Ensure wails3 is on PATH; auto-install the pinned version when missing.
# Fails ONLY when the install itself fails (no network, go install error).
define require-wails3
	@if ! command -v wails3 >/dev/null 2>&1; then \
		echo "wails3 CLI not found (checked PATH, incl. $$(go env GOPATH)/bin)."; \
		echo "Installing pinned $(WAILS3_VERSION)…"; \
		go install github.com/wailsapp/wails/v3/cmd/wails3@$(WAILS3_VERSION) || { echo "wails3 auto-install FAILED"; exit 1; }; \
	fi
	@command -v wails3 >/dev/null 2>&1 || { echo "wails3 still not on PATH after install — is $$(go env GOPATH)/bin in PATH?"; exit 1; }
endef

build-app:
	$(require-wails3)
	cd app && wails3 task build OUTPUT=../bin/app/infermesh-app

# macOS: build host-arch production binary, create the .app bundle (ad-hoc
# signed), relocate to bin/app/. Leaves the raw binary at app/bin/InferMesh
# (gitignored) so re-runs stay idempotent.
package-app:
	$(require-wails3)
	cd app && wails3 task package APP_NAME=$(APP_NAME)
	mkdir -p $(APP_BIN_DIR)
	rm -rf $(APP_BIN_DIR)/$(APP_NAME).app
	mv app/bin/$(APP_NAME).app $(APP_BIN_DIR)/
	@echo "Packaged: $(APP_BIN_DIR)/$(APP_NAME).app (ad-hoc signed)"

# macOS: .dmg (darwin-only; builds package first).
package-app-dmg:
	$(require-wails3)
	cd app && wails3 task darwin:package:dmg APP_NAME=$(APP_NAME)
	mkdir -p $(APP_BIN_DIR)
	mv -f app/bin/*.dmg $(APP_BIN_DIR)/ 2>/dev/null || { echo "expected app/bin/*.dmg but nothing found"; exit 1; }
	@echo "Packaged: $(APP_BIN_DIR)/$(APP_NAME).dmg"

# Linux: AppImage + deb. Requires a Linux host (webkit2gtk) or Docker with the
# wails-cross image. On non-Linux hosts this prints guidance and FAILS (exit 1):
# a silent success would hide a missing deliverable. Never part of `make test`.
package-app-linux:
	$(require-wails3)
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "Linux packaging (AppImage + deb) requires a Linux host with webkit2gtk,"; \
		echo "or the wails-cross Docker image (see app/build/linux/Taskfile.yml)."; \
		echo "Run it on a Linux CI runner: cd app && wails3 task package APP_NAME=$(APP_NAME)"; \
		exit 1; \
	fi
	cd app && wails3 task package APP_NAME=$(APP_NAME)
	mkdir -p $(APP_BIN_DIR)
	mv -f app/bin/$(APP_NAME)-*.AppImage $(APP_BIN_DIR)/ 2>/dev/null || true
	mv -f app/bin/*.deb $(APP_BIN_DIR)/ 2>/dev/null || true
	@echo "Packaged: $(APP_BIN_DIR)/ (AppImage + deb)"

# Windows: compile-check = full cross-compile (frontend + syso + production
# flags) to bin/app/InferMesh.exe. NSIS installer only when makensis exists;
# otherwise print guidance and still succeed (the compile-check is the
# deliverable on non-Windows hosts).
package-app-windows:
	$(require-wails3)
	cd app && wails3 task build GOOS=windows ARCH=amd64 APP_NAME=$(APP_NAME)
	mkdir -p $(APP_BIN_DIR)
	mv -f app/bin/$(APP_NAME).exe $(APP_BIN_DIR)/$(APP_NAME).exe
	@if command -v makensis >/dev/null 2>&1; then \
		echo "makensis found: building NSIS installer"; \
		cd app && wails3 task package GOOS=windows ARCH=amd64 APP_NAME=$(APP_NAME) FORMAT=nsis INSTALL_SCOPE=machine; \
		mkdir -p $(APP_BIN_DIR); \
		mv -f app/bin/*-installer.exe $(APP_BIN_DIR)/ 2>/dev/null || true; \
		echo "Packaged: $(APP_BIN_DIR)/ (exe + NSIS installer)"; \
	else \
		echo "makensis not found - skipping NSIS installer (compile-check only)."; \
		echo "Install NSIS (brew install nsis / apt install nsis) to build the Windows installer."; \
	fi
	@echo "Built: $(APP_BIN_DIR)/$(APP_NAME).exe (compile-check)"

# Build the app, launch it, probe window/tray presence LIGHTLY. Skips (exit 0)
# on SKIP=1 / INFERMESH_SMOKE_SKIP=1 or headless Linux. Not part of `make test`.
test-app-smoke: build-app
	cd app && go test -tags smoke -count=1 -v -run '^TestAppSmoke$$' .

# Unit tests (method-level, table-driven)
test-unit:
	go test ./pkg/...

# Security unit tests
test-security:
	go test ./pkg/security/...

# Integration tests (multi-component interaction; skip if dir absent)
test-integration:
	@if [ -d ./tests/integration ]; then go test ./tests/integration/...; else echo "No ./tests/integration directory; skipping"; fi

# End-to-end tests (full flow, real network)
test-e2e:
	@if [ -d ./tests/e2e ]; then go test ./tests/e2e/...; else echo "No ./tests/e2e directory; skipping"; fi

# Run all tests: unit + integration + e2e
test: test-unit test-integration test-e2e

# Platform-aware tests: run on current platform
test-platform: test test-integration test-e2e

# Test all platforms (requires cross-compilation)
test-all-platforms: test-platform
	@echo "Testing cross-platform binaries..."
	@echo "Run ./hack/test_platforms.sh for platform-specific validation"

# Linting (golangci-lint)
lint:
	golangci-lint run

# Install git hooks (pre-push runs `make lint` before every push)
install-hooks:
	mkdir -p $$(git rev-parse --git-path hooks)
	cp hack/pre-push $$(git rev-parse --git-path hooks)/pre-push
	chmod +x $$(git rev-parse --git-path hooks)/pre-push
	@echo "pre-push hook installed (runs 'make lint' before push; bypass with --no-verify)"

# Go modules
tidy:
	go mod tidy

# Clean build artifacts
clean:
	rm -rf bin/
	rm -rf dist/
	rm -rf cover/
	find . -type f -name "*.cover" -delete
	find . -type d -name "cover" -exec rm -rf {} +

# macOS app bundles (firewall / local network workaround)
app:
	bash packaging/macos/build_app.sh

# Coverage
coverage:
	go test -coverprofile=cover/coverage.out ./pkg/...
	go tool funccover -mode=count -func=cover/coverage.out
	@echo "Coverage report: cover/coverage.out"

.PHONY: build build-router build-worker build-static build-app package-app package-app-dmg package-app-linux package-app-windows relay test test-security test-integration test-e2e test-platform test-all-platforms lint install-hooks tidy clean app coverage test-app-smoke

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
# signed with hardened runtime + entitlements), relocate to bin/app/.
# Leaves the raw binary at app/bin/InferMesh (gitignored) so re-runs stay
# idempotent. The Taskfile's `codesign:verify` task runs after bundling and
# exits non-zero if the bundle is not ad-hoc signed — so a failed signature
# fails the whole build.
package-app:
	$(require-wails3)
	cd app && wails3 task package APP_NAME=$(APP_NAME)
	mkdir -p $(APP_BIN_DIR)
	rm -rf $(APP_BIN_DIR)/$(APP_NAME).app
	mv app/bin/$(APP_NAME).app $(APP_BIN_DIR)/
	@echo "Packaged: $(APP_BIN_DIR)/$(APP_NAME).app (ad-hoc signed, hardened runtime)"

# macOS: .digm (darwin-only; builds package first, which signs + verifies).
# A README is dropped into the DMG so users find the Gatekeeper workaround
# without digging through System Settings.
package-app-dmg:
	$(require-wails3)
	cd app && wails3 task darwin:package:dmg APP_NAME=$(APP_NAME) DMG_FILES="README.md=build/darwin/README.md"
	mkdir -p $(APP_BIN_DIR)
	mv -f app/bin/*.dmg $(APP_BIN_DIR)/ 2>/dev/null || { echo "expected app/bin/*.dmg but nothing found"; exit 1; }
	@echo "Packaged: $(APP_BIN_DIR)/$(APP_NAME).dmg (ad-hoc signed, hardened runtime)"

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

# Docker images for router and worker (multi-arch via buildx)
# Usage: make pkg-docker IMAGE=ghcr.io/seppaleinen/infermesh VERSION=v0.1.16
# Requires: docker buildx (included in Docker Desktop / docker-ce)
# Builds both router and worker images, tagged with VERSION and latest.
# Set PLATFORMS to override (default: linux/amd64,linux/arm64).
IMAGE      ?= ghcr.io/seppaleinen/infermesh
VERSION    ?= latest
PLATFORMS  ?= linux/amd64,linux/arm64
DOCKERFILE ?= docker/Dockerfile

pkg-docker:
	@echo "Building Docker images for $(IMAGE):router/$(VERSION) and $(IMAGE):worker/$(VERSION)"
	@echo "Platforms: $(PLATFORMS)"
	docker buildx build \
		--platform $(PLATFORMS) \
		--target router \
		-t $(IMAGE)/router:$(VERSION) \
		-t $(IMAGE)/router:latest \
		-f $(DOCKERFILE) \
		--load .
	docker buildx build \
		--platform $(PLATFORMS) \
		--target worker \
		-t $(IMAGE)/worker:$(VERSION) \
		-t $(IMAGE)/worker:latest \
		-f $(DOCKERFILE) \
		--load .

# Build and push Docker images to registry (CI use; requires auth)
# Usage: make pkg-docker-push IMAGE=ghcr.io/seppaleinen/infermesh VERSION=v0.1.16
pkg-docker-push:
	@echo "Building and pushing Docker images for $(IMAGE):router/$(VERSION) and $(IMAGE):worker/$(VERSION)"
	@echo "Platforms: $(PLATFORMS)"
	docker buildx build \
		--platform $(PLATFORMS) \
		--target router \
		-t $(IMAGE)/router:$(VERSION) \
		-t $(IMAGE)/router:latest \
		-f $(DOCKERFILE) \
		--push .
	docker buildx build \
		--platform $(PLATFORMS) \
		--target worker \
		-t $(IMAGE)/worker:$(VERSION) \
		-t $(IMAGE)/worker:latest \
		-f $(DOCKERFILE) \
		--push .

# Package the Helm chart: lint + package into infermesh-<version>.tgz
# Byproduct .tgz is gitignored (see .gitignore: *.tgz).
pkg-helm:
	@echo "Linting chart charts/infermesh ..."
	helm lint charts/infermesh
	@echo "Packaging chart ..."
	helm package charts/infermesh --destination .
	@echo "Packaged: infermesh-$$(grep '^version:' charts/infermesh/Chart.yaml | awk '{print $$2}').tgz"

# Validate the docker-compose files (validate only — does NOT start containers).
# Requires the docker compose plugin (or docker-compose). If neither is
# available, the YAML is structurally validated by a Python parser instead.
# Validates both docker-compose.yml (pre-built images) and
# docker-compose.build.yml (build-from-source).
pkg-compose:
	@echo "Validating docker-compose.yml and docker-compose.build.yml ..."
	@if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then \
		docker compose -f docker-compose.yml config -q && echo "docker-compose.yml OK"; \
		docker compose -f docker-compose.build.yml config -q && echo "docker-compose.build.yml OK"; \
	elif command -v docker-compose >/dev/null 2>&1; then \
		docker-compose -f docker-compose.yml config -q && echo "docker-compose.yml OK"; \
		docker-compose -f docker-compose.build.yml config -q && echo "docker-compose.build.yml OK"; \
	else \
		echo "NOTE: neither 'docker compose' nor 'docker-compose' is installed; structurally validating YAML instead."; \
		python3 hack/compose_validate.py; \
	fi

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

# Build packaged binaries and run e2e tests against them
test-packaged: build
	@echo "Running E2E tests against packaged binaries..."
	go test ./tests/e2e/... -v -count=1

# Build static binaries and run e2e tests against current-platform ones
test-static: build-static
	@HOST_BIN_OS=$$(go env GOOS); \
	HOST_BIN_ARCH=$$(go env GOARCH); \
	EXE=""; \
	[ "$$HOST_BIN_OS" = "windows" ] && EXE=".exe"; \
	ROUTER=bin/infermesh-router-$$HOST_BIN_OS-$$HOST_BIN_ARCH$$EXE; \
	WORKER=bin/infermesh-worker-$$HOST_BIN_OS-$$HOST_BIN_ARCH$$EXE; \
	if [ ! -f "$$ROUTER" ] || [ ! -f "$$WORKER" ]; then \
		echo "skipping e2e: static binaries for $$HOST_BIN_OS/$$HOST_BIN_ARCH not found"; exit 0; \
	fi; \
	chmod +x $$ROUTER $$WORKER 2>/dev/null || true; \
	cp -f $$ROUTER bin/infermesh-router; \
	cp -f $$WORKER bin/infermesh-worker; \
	echo "Running E2E tests against static binaries ($$HOST_BIN_OS/$$HOST_BIN_ARCH)..."; \
	go test ./tests/e2e/... -v -count=1

# Run all tests: unit + integration + e2e
test: vet-app test-unit test-integration test-e2e

# Run all app verification (TypeScript, Go tests, build)
test-app: vet-app
	cd app && go test ./...
	cd app && wails3 task build OUTPUT=/dev/null 2>&1 || (echo "App build failed" && exit 1)

# Run all tests including app module and app build verification
test-all: test test-app
	golangci-lint run ./...

# Platform-aware tests: run on current platform
test-platform: test test-integration test-e2e

# Test all platforms (requires cross-compilation)
test-all-platforms: test-platform
	@echo "Testing cross-platform binaries..."
	@echo "Run ./hack/test_platforms.sh for platform-specific validation"

# Linting (golangci-lint)
lint:
	golangci-lint run

# Validate contract docs against source. Catches stale claims (CLI flags,
# file:line references, "no auth" claims) at PR time. See
# hack/validate-contracts.sh for the rules. Added after the #123/#128/#129
# post-mortems found contract docs drifting from code across four merged PRs.
validate-contracts:
	hack/validate-contracts.sh

vet-app:
	@echo "⟶ Running TypeScript type check on the Wails app…"
	cd app/frontend && ./node_modules/.bin/vue-tsc --noEmit

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
	mkdir -p cover
	go test -coverprofile=cover/coverage.out ./pkg/...
	go tool cover -func=cover/coverage.out
	@echo "Per-package coverage:"
	@go tool cover -func=cover/coverage.out | \
		grep -E "github.com/seppaleinen/infermesh/pkg/" | \
		sed 's|github.com/seppaleinen/infermesh/pkg/||' | \
		awk -F'\t' '{split($$1,a,"/"); pkg=a[1]; c=$$NF; gsub(/%.*/,"",c); sum[pkg]+=c; cnt[pkg]++} END {for(p in sum) printf "  %s: %.1f%%\n", p, sum[p]/cnt[p]}' | \
		sort
	@echo "Coverage report: cover/coverage.out"

.PHONY: build build-router build-worker build-static build-app package-app package-app-dmg package-app-linux package-app-windows relay test test-app test-all test-security test-integration test-e2e test-packaged test-static test-platform test-all-platforms lint validate-contracts install-hooks tidy clean app coverage test-app-smoke pkg-docker pkg-docker-push pkg-helm pkg-compose

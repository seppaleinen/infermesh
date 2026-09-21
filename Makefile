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

.PHONY: build build-router build-worker build-static relay test test-security test-integration test-e2e test-platform test-all-platforms lint install-hooks tidy clean app coverage

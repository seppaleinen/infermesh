# Build the unified infermesh binary (router + worker subcommands)
build:
	go build -o bin/infermesh ./cmd/infermesh

# Build static binaries (CGO_ENABLED=0 for cross-platform compatibility)
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/infermesh-linux-amd64 ./cmd/infermesh
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/infermesh-darwin-arm64 ./cmd/infermesh

# Unit tests (method-level, table-driven)
test:
	go test ./pkg/...

# Security unit tests
test-security:
	go test ./pkg/security/...

# Integration tests (multi-component interaction)
test-integration:
	go test ./tests/integration/...

# End-to-end tests (full flow, real network)
test-e2e:
	go test ./tests/e2e/...

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

.PHONY: build build-static test test-security test-integration test-e2e test-platform test-all-platforms lint install-hooks tidy clean app coverage

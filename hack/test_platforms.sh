#!/usr/bin/env bash
# test_platforms.sh — Helper script to run platform-specific tests for InferMesh.
#
# Usage:
#   ./hack/test_platforms.sh              # Run all platform tests on current platform
#   ./hack/test_platforms.sh --build      # Build binaries first
#   ./hack/test_platforms.sh --cross      # Test cross-platform binaries
#   ./hack/test_platforms.sh --all        # Run everything
#
# This script is intended for local development and CI use.
# It detects the current platform and runs appropriate tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
  echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
  echo -e "${RED}[ERROR]${NC} $1"
}

# Detect platform
detect_platform() {
  case "$(uname -s)" in
    Darwin)
      echo "darwin"
      ;;
    Linux)
      echo "linux"
      ;;
    *)
      echo "unknown"
      ;;
  esac
}

# Detect architecture
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      echo "unknown"
      ;;
  esac
}

PLATFORM=$(detect_platform)
ARCH=$(detect_arch)

log_info "Detected platform: $PLATFORM, architecture: $ARCH"

# Build binaries if requested
if [[ "${1:-}" == "--build" ]] || [[ "${1:-}" == "--all" ]]; then
  log_info "Building binaries..."
  make build
fi

# Run unit tests
run_unit_tests() {
  log_info "Running unit tests..."
  go test ./pkg/... -v -count=1
}

# Run integration tests
run_integration_tests() {
  log_info "Running integration tests..."
  go test ./tests/integration/... -v -count=1
}

# Run E2E tests
run_e2e_tests() {
  log_info "Running E2E tests..."
  go test ./tests/e2e/... -v -count=1
}

# Run all tests
run_all_tests() {
  log_info "Running all tests..."
  go test ./... -v -count=1
}

# Test cross-platform binaries
test_cross_platform() {
  log_info "Building cross-platform binaries..."
  make build-static

  log_info "Verifying cross-platform binaries..."
  local binaries=(
    "bin/infermesh-linux-amd64"
    "bin/infermesh-darwin-arm64"
  )

  for bin in "${binaries[@]}"; do
    if [[ -f "$bin" ]]; then
      log_info "  ✓ $bin exists"
    else
      log_error "  ✗ $bin missing"
      return 1
    fi
  done

  log_info "Cross-platform build verification complete."
}

# Test current platform binary
test_current_platform() {
  log_info "Testing current platform binary..."
  if [[ -f "bin/infermesh" ]]; then
    log_info "  ✓ bin/infermesh exists"
  else
    log_warn "  Binary not found, building first..."
    make build
  fi
}

# Main
case "${1:-}" in
  --build)
    run_unit_tests
    run_integration_tests
    run_e2e_tests
    ;;
  --cross)
    test_cross_platform
    ;;
  --all)
    run_all_tests
    test_cross_platform
    ;;
  "")
    test_current_platform
    run_unit_tests
    run_integration_tests
    run_e2e_tests
    ;;
  *)
    log_error "Unknown option: $1"
    echo "Usage: $0 [--build|--cross|--all]"
    exit 1
    ;;
esac

log_info "All tests completed successfully."
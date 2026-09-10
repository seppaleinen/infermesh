package platform

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// DetectPlatform returns the current platform name.
// Uses runtime.GOOS as the primary source (always reliable),
// falling back to env-var heuristics for cross-compiled/test contexts.
func DetectPlatform() string {
	switch runtime.GOOS {
	case "linux":
		return "linux"
	case "darwin":
		return "darwin"
	}
	switch {
	case isDarwin():
		return "darwin"
	case isLinux():
		return "linux"
	default:
		return "unknown"
	}
}

// isDarwin checks if the current OS is macOS (including Apple Silicon).
func isDarwin() bool {
	return strings.Contains(os.Getenv("OSTYPE"), "darwin") || strings.ToLower(os.Getenv("APPLE_PLATFORM")) == "darwin"
}

// isLinux checks if the current OS is Linux.
func isLinux() bool {
	return strings.ToLower(os.Getenv("OSTYPE")) == "linux" || strings.HasPrefix(strings.ToLower(os.Getenv("KERNEL_NAME")), "linux")
}

// isArm64 checks if the architecture is ARM 64-bit (Apple Silicon).
func isArm64() bool {
	arch := os.Getenv("ARCH")
	if arch == "arm64" || arch == "aarch64" {
		return true
	}
	out, err := execCommand("uname", "-m")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(out), "arm64") || strings.Contains(strings.ToLower(out), "aarch64")
}

// isAppleSilicon checks if running on Apple Silicon (M1/M2/M3/M4).
func isAppleSilicon() bool {
	return isArm64() && isDarwin()
}

// execCommand runs a command and returns its stdout output.
func execCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GPUInfo represents GPU information discovered for the current platform.
type GPUInfo struct {
	Vendor      string
	Model       string
	ComputeCore int
	TotalVRAM   int64
	FreeVRAM    int64
}

// DetectGPUPlatform detects the GPU platform and returns GPU info.
func DetectGPUPlatform() (*GPUInfo, error) {
	return nil, nil // Placeholder: actual detection not implemented yet
}
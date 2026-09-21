package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SystemMetrics holds system-level metrics for the current platform.
type SystemMetrics struct {
	CPULoadPercent float64 `json:"cpu_load_percent"`
	MemoryUsedMB   int64   `json:"memory_used_mb"`
	MemoryFreeMB   int64   `json:"memory_free_mb"`
	DiskFreeGB     float64 `json:"disk_free_gb"`
}

// CollectSystemMetrics collects system metrics using platform-specific methods.
func CollectSystemMetrics() (*SystemMetrics, error) {
	switch DetectPlatform() {
	case "linux":
		return collectLinuxSystemMetrics()
	case "darwin":
		return collectDarwinSystemMetrics()
	default:
		return nil, fmt.Errorf("unsupported platform for system metrics")
	}
}

// collectLinuxSystemMetrics collects metrics from /proc on Linux.
func collectLinuxSystemMetrics() (*SystemMetrics, error) {
	cpuLoad, err := readCPULoad()
	if err != nil {
		cpuLoad = 0
	}

	memUsed, memFree, err := readMemoryInfo()
	if err != nil {
		memUsed, memFree = 0, 0
	}

	diskFree, err := readDiskFree("/")
	if err != nil {
		diskFree = 0
	}

	return &SystemMetrics{
		CPULoadPercent: cpuLoad,
		MemoryUsedMB:   memUsed,
		MemoryFreeMB:   memFree,
		DiskFreeGB:     diskFree,
	}, nil
}

// collectDarwinSystemMetrics collects metrics on macOS using sysctl and other Darwin tools.
func collectDarwinSystemMetrics() (*SystemMetrics, error) {
	cpuLoad, err := readDarwinCPULoad()
	if err != nil {
		cpuLoad = 0
	}

	memUsed, memFree, err := readDarwinMemoryInfo()
	if err != nil {
		memUsed, memFree = 0, 0
	}

	diskFree, err := readDiskFree("/")
	if err != nil {
		diskFree = 0
	}

	return &SystemMetrics{
		CPULoadPercent: cpuLoad,
		MemoryUsedMB:   memUsed,
		MemoryFreeMB:   memFree,
		DiskFreeGB:     diskFree,
	}, nil
}

// readCPULoad reads CPU load from /proc/loadavg (Linux).
func readCPULoad() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("invalid loadavg format")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return load, nil
}

// readMemoryInfo reads memory from /proc/meminfo (Linux).
func readMemoryInfo() (int64, int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}

	var total, free, available int64
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseInt(fields[1], 10, 64)
				total = val / 1024 // Convert KB to MB
			}
		} else if strings.HasPrefix(line, "MemFree:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseInt(fields[1], 10, 64)
				free = val / 1024
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseInt(fields[1], 10, 64)
				available = val / 1024
			}
		}
	}

	if available > 0 {
		return total - available, available, nil
	}
	return total - free, free, nil
}

// readDarwinCPULoad reads CPU load on macOS using sysctl.
func readDarwinCPULoad() (float64, error) {
	out, err := execCommand("sysctl", "-n", "vm.loadavg")
	if err != nil {
		return 0, err
	}
	// macOS sysctl returns: { 1.23 1.45 1.67 }
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 1 {
		return 0, fmt.Errorf("invalid loadavg format from sysctl")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return load, nil
}

// readDarwinMemoryInfo reads memory info on macOS using sysctl.
func readDarwinMemoryInfo() (int64, int64, error) {
	// Get total memory
	totalOut, err := execCommand("sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	totalBytes, err := strconv.ParseInt(strings.TrimSpace(totalOut), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	totalMB := totalBytes / (1024 * 1024)

	// Get free memory using vm_stat
	vmStatOut, err := execCommand("vm_stat")
	if err != nil {
		return 0, 0, err
	}

	var pageFree int64
	lines := strings.Split(string(vmStatOut), "\n")
	for _, line := range lines {
		if strings.Contains(line, "Pages free") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				val, err := strconv.ParseInt(parts[2], 10, 64)
				if err == nil {
					// macOS page size is typically 4096 bytes
					pageFree = val * 4096 / (1024 * 1024) // Convert to MB
				}
			}
		}
	}

	return totalMB - pageFree, pageFree, nil
}


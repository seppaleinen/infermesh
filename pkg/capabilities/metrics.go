package capabilities

import (
	"fmt"

	"github.com/seppaleinen/infermesh/pkg/platform"
)

// SystemMetrics holds system-level metrics.
type SystemMetrics struct {
	CPULoadPercent float64 `json:"cpu_load_percent"`
	MemoryUsedMB   int64   `json:"memory_used_mb"`
	MemoryFreeMB   int64   `json:"memory_free_mb"`
	DiskFreeGB     float64 `json:"disk_free_gb"`
}

// RuntimeMetrics holds runtime metrics for a worker.
type RuntimeMetrics struct {
	GPUUtilization   float64 `json:"gpu_utilization"`
	VRAMUsedMB       int64   `json:"vram_used_mb"`
	VRAMFreeMB       int64   `json:"vram_free_mb"`
	QueueDepth       int     `json:"queue_depth"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	TotalRequests    int64   `json:"total_requests"`
	LastRequestTime  string  `json:"last_request_time"`
	UptimeSeconds    int64   `json:"uptime_seconds"`
}

// CollectRuntimeMetrics collects current runtime metrics for the worker.
func CollectRuntimeMetrics() (*RuntimeMetrics, error) {
	gpu, err := platform.DetectGPUPlatform()
	if err != nil {
		return nil, fmt.Errorf("detecting GPU for runtime metrics: %w", err)
	}

	var gpuUtil float64
	var vramUsed, vramFree int64

	if gpu != nil && gpu.Model != "" && gpu.Model != "no GPU detected" {
		// Use first GPU (single GPU per worker in MVP)
		vramTotal := gpu.TotalVRAM
		vramFree = gpu.FreeVRAM
		vramUsed = vramTotal - vramFree
	}

	return &RuntimeMetrics{
		GPUUtilization:  gpuUtil,
		VRAMUsedMB:      vramUsed,
		VRAMFreeMB:      vramFree,
		QueueDepth:      0,
		AvgLatencyMs:     0,
		TotalRequests:   0,
		LastRequestTime: "",
		UptimeSeconds:   0,
	}, nil
}

// CollectSystemMetrics collects system-level metrics using platform-specific methods.
func CollectSystemMetrics() (*SystemMetrics, error) {
	sysMetrics, err := platform.CollectSystemMetrics()
	if err != nil {
		return nil, fmt.Errorf("collecting system metrics: %w", err)
	}

	return &SystemMetrics{
		CPULoadPercent: sysMetrics.CPULoadPercent,
		MemoryUsedMB:   sysMetrics.MemoryUsedMB,
		MemoryFreeMB:   sysMetrics.MemoryFreeMB,
		DiskFreeGB:     sysMetrics.DiskFreeGB,
	}, nil
}
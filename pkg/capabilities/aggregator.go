package capabilities

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// Aggregator assembles detections from various sources and provides periodic refresh.
type Aggregator struct {
	config     Config
	log        *slog.Logger
	mu         sync.RWMutex
	capabilities protocol.Capabilities
	lastUpdate time.Time
}

// NewAggregator creates a new capabilities aggregator.
func NewAggregator(config Config, log *slog.Logger) *Aggregator {
	if log == nil {
		log = slog.Default()
	}
	config.Validate()
	return &Aggregator{
		config:     config,
		log:        log,
		capabilities: protocol.Capabilities{},
	}
}

// Detect performs a full detection of capabilities.
func (a *Aggregator) Detect() (protocol.Capabilities, error) {
	a.log.Debug("detecting capabilities")

	var caps protocol.Capabilities

	// Detect GPU
	gpu, err := DetectGPU()
	if err != nil {
		a.log.Warn("GPU detection failed", "error", err)
	} else if gpu != nil && gpu.Model != "" && gpu.Model != "no GPU detected" {
		caps.GPU = protocol.GPUInfo{
			Vendor:      gpu.Vendor,
			Model:       gpu.Model,
			ComputeCore: gpu.ComputeCore,
			TotalVRAM:   gpu.TotalVRAM,
			FreeVRAM:    gpu.FreeVRAM,
		}
		caps.VRAM = protocol.MemoryInfo{
			TotalMB: gpu.TotalVRAM,
			FreeMB:  gpu.FreeVRAM,
		}
	} else {
		caps.GPU = protocol.GPUInfo{
			Vendor:      "none",
			Model:       "no GPU detected",
			ComputeCore: 0,
			TotalVRAM:   0,
			FreeVRAM:    0,
		}
	}

	// Load models from config
	models, err := LoadModelsConfig(a.config.ModelConfigPath)
	if err != nil {
		a.log.Warn("model config loading failed", "error", err)
	} else if models != nil {
		for _, m := range models.Models {
			caps.Models = append(caps.Models, m.ToModelInfo())
		}
	}

	// Detect engines
	engines, err := DetectEngines(EngineDetectionConfig{})
	if err != nil {
		a.log.Warn("engine detection failed", "error", err)
	}
	caps.Engines = engines

	// Collect system memory (best-effort; missing metrics must not break heartbeat)
	sysMetrics, err := CollectSystemMetrics()
	if err != nil {
		a.log.Debug("system metrics unavailable, using zero values", "error", err)
	} else {
		caps.System = protocol.MemoryInfo{
			TotalMB: sysMetrics.MemoryUsedMB + sysMetrics.MemoryFreeMB,
			FreeMB:  sysMetrics.MemoryFreeMB,
		}
	}

	a.mu.Lock()
	a.capabilities = caps
	a.lastUpdate = time.Now()
	a.mu.Unlock()

	return caps, nil
}

// Get returns the current capabilities snapshot.
func (a *Aggregator) Get() protocol.Capabilities {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.capabilities
}

// StartRefresh starts periodic detection in a goroutine.
// It calls Detect immediately, then refreshes at the configured interval.
func (a *Aggregator) StartRefresh(ctx context.Context) {
	// Initial detection
	if _, err := a.Detect(); err != nil {
		a.log.Error("initial capability detection failed", "error", err)
	}

	ticker := time.NewTicker(a.config.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.Detect(); err != nil {
				a.log.Error("capability refresh failed", "error", err)
			}
		}
	}
}

// GetLastUpdate returns the timestamp of the last detection.
func (a *Aggregator) GetLastUpdate() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastUpdate
}
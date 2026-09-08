package capabilities

import (
	"fmt"

	"github.com/seppaleinen/infermesh/pkg/platform"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// DetectGPU detects GPU using platform-specific methods.
// Returns nil, nil if no GPU is detected (not an error).
func DetectGPU() (*platform.GPUInfo, error) {
	return platform.DetectGPUPlatform()
}

// DefaultNVIDIADeviceID is the PCI vendor ID for NVIDIA.
const DefaultNVIDIADeviceID = "0x10de"

// Known NVIDIA device IDs mapped to model names.
var nvidiaDeviceMap = map[string]string{
	"0x2782": "RTX 4090",
	"0x2684": "RTX 3090",
	"0x2231": "RTX 3080",
	"0x2206": "RTX 3070",
	"0x2482": "RTX 2080 Ti",
	"0x1e04": "RTX 2080",
	"0x1f82": "Titan RTX",
	"0x0622": "GTX 1080 Ti",
	"0x1c03": "GTX 1080",
}

// GPU represents a single GPU device.
type GPU struct {
	Index     int
	Vendor    string
	Model     string
	DeviceID  string
	TotalVRAM int64 // in MB
	FreeVRAM  int64 // in MB
	Util      int   // percentage
}

// ToGPUInfo converts a GPU to the protocol GPUInfo struct.
func (g *GPU) ToGPUInfo() protocol.GPUInfo {
	return protocol.GPUInfo{
		Vendor:      g.Vendor,
		Model:       g.Model,
		ComputeCore: 0, // Not available from sysfs
		TotalVRAM:   g.TotalVRAM,
		FreeVRAM:    g.FreeVRAM,
	}
}

// deviceIDToModel maps a PCI device ID to a human-readable model name.
func deviceIDToModel(deviceID string) string {
	model, ok := nvidiaDeviceMap[deviceID]
	if !ok {
		return fmt.Sprintf("NVIDIA Device %s", deviceID)
	}
	return model
}
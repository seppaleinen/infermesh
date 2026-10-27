package worker

import (
	"fmt"
	"io"
	"os"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// PrintCapabilities prints the detected capabilities in a human-readable
// format to w. It returns 0 on success; on error the message is written to
// os.Stderr and 1 is returned.
func PrintCapabilities(w io.Writer) int {
	cfg := capabilities.Defaults()
	agg := capabilities.NewAggregator(cfg, nil)

	caps, err := agg.Detect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error detecting capabilities: %v\n", err)
		return 1
	}

	if err := writeCapabilities(w, caps); err != nil {
		return 1
	}
	return 0
}

// writeCapabilities renders the capability report to w, returning the first
// write error encountered.
func writeCapabilities(w io.Writer, caps protocol.Capabilities) error {
	wr := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	// Helper that writes and returns error if non-nil, avoiding deep nesting
	ok := func(err error) bool { return err != nil }
	
	if ok(wr("=== InferMesh Worker Capabilities ===\n")) { return nil }
	if ok(wr("\n")) { return nil }

	if err := writeGPU(wr, caps); err != nil { return err }
	if err := writeVRAM(wr, caps); err != nil { return err }
	if err := writeSystem(wr, caps); err != nil { return err }
	if err := writeModels(wr, caps); err != nil { return err }
	if err := writeEngines(wr, caps); err != nil { return err }
	return nil
}

// writeGPU writes the GPU section.
func writeGPU(write func(string, ...any) error, caps protocol.Capabilities) error {
	if err := write("GPU:\n"); err != nil {
		return err
	}
	hasGPU := caps.GPU.Vendor != "" && caps.GPU.Model != "no GPU detected"
	if !hasGPU {
		if err := write("  No GPU detected\n"); err != nil {
			return err
		}
		return write("\n")
	}
	if err := writeGPUDetails(write, caps.GPU); err != nil {
		return err
	}
	return write("\n")
}

// writeGPUDetails writes GPU details for a detected GPU.
func writeGPUDetails(write func(string, ...any) error, gpu protocol.GPUInfo) error {
	if err := write("  Vendor:     %s\n", gpu.Vendor); err != nil {
		return err
	}
	if err := write("  Model:      %s\n", gpu.Model); err != nil {
		return err
	}
	if err := write("  Compute Cores: %d\n", gpu.ComputeCore); err != nil {
		return err
	}
	if err := write("  Total VRAM: %d MB\n", gpu.TotalVRAM); err != nil {
		return err
	}
	if err := write("  Free VRAM:  %d MB\n", gpu.FreeVRAM); err != nil {
		return err
	}
	return nil
}

// writeVRAM writes the VRAM section.
func writeVRAM(write func(string, ...any) error, caps protocol.Capabilities) error {
	if err := write("VRAM:\n"); err != nil {
		return err
	}
	if err := write("  Total: %d MB\n", caps.VRAM.TotalMB); err != nil {
		return err
	}
	if err := write("  Free: %d MB\n", caps.VRAM.FreeMB); err != nil {
		return err
	}
	return write("\n")
}

// writeSystem writes the system section.
func writeSystem(write func(string, ...any) error, caps protocol.Capabilities) error {
	if err := write("System:\n"); err != nil {
		return err
	}
	if err := write("  Total Memory: %d MB\n", caps.System.TotalMB); err != nil {
		return err
	}
	if err := write("  Free Memory: %d MB\n", caps.System.FreeMB); err != nil {
		return err
	}
	return write("\n")
}

// writeModels writes the models section.
func writeModels(write func(string, ...any) error, caps protocol.Capabilities) error {
	if err := write("Models:\n"); err != nil {
		return err
	}
	if len(caps.Models) > 0 {
		for _, m := range caps.Models {
			if err := write("  %s (%d bytes, %s, backend: %s, loaded: %t)\n",
				m.Name, m.Size, m.Quantization, m.Backend, m.Loaded); err != nil {
				return err
			}
		}
	} else {
		if err := write("  No models declared\n"); err != nil {
			return err
		}
	}
	return write("\n")
}

// writeEngines writes the engines section.
func writeEngines(write func(string, ...any) error, caps protocol.Capabilities) error {
	if err := write("Engines:\n"); err != nil {
		return err
	}
	if len(caps.Engines) > 0 {
		for _, e := range caps.Engines {
			if err := write("  %s\n", e); err != nil {
				return err
			}
		}
	} else {
		if err := write("  No engines detected\n"); err != nil {
			return err
		}
	}
	return write("\n")
}

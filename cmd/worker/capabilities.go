package main

import (
	"fmt"
	"os"

	"github.com/seppaleinen/infermesh/pkg/capabilities"
)

// capabilitiesCmd prints the detected capabilities in a human-readable format
// and returns 0 on success, 1 on error.
func capabilitiesCmd() int {
	cfg := capabilities.Defaults()
	agg := capabilities.NewAggregator(cfg, nil)

	caps, err := agg.Detect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error detecting capabilities: %v\n", err)
		return 1
	}

	fmt.Println("=== InferMesh Worker Capabilities ===")
	fmt.Println()

	// GPU
	fmt.Println("GPU:")
	if caps.GPU.Vendor != "" && caps.GPU.Model != "no GPU detected" {
		fmt.Printf("  Vendor:     %s\n", caps.GPU.Vendor)
		fmt.Printf("  Model:      %s\n", caps.GPU.Model)
		fmt.Printf("  Compute Cores: %d\n", caps.GPU.ComputeCore)
		fmt.Printf("  Total VRAM: %d MB\n", caps.GPU.TotalVRAM)
		fmt.Printf("  Free VRAM:  %d MB\n", caps.GPU.FreeVRAM)
	} else {
		fmt.Println("  No GPU detected")
	}
	fmt.Println()

	// VRAM
	fmt.Println("VRAM:")
	fmt.Printf("  Total: %d MB\n", caps.VRAM.TotalMB)
	fmt.Printf("  Free:  %d MB\n", caps.VRAM.FreeMB)
	fmt.Println()

	// System
	fmt.Println("System:")
	fmt.Printf("  Total Memory: %d MB\n", caps.System.TotalMB)
	fmt.Printf("  Free Memory:  %d MB\n", caps.System.FreeMB)
	fmt.Println()

	// Models
	fmt.Println("Models:")
	if len(caps.Models) > 0 {
		for _, m := range caps.Models {
			fmt.Printf("  %s (%d bytes, %s, backend: %s, loaded: %t)\n",
				m.Name, m.Size, m.Quantization, m.Backend, m.Loaded)
		}
	} else {
		fmt.Println("  No models declared")
	}
	fmt.Println()

	// Engines
	fmt.Println("Engines:")
	if len(caps.Engines) > 0 {
		for _, e := range caps.Engines {
			fmt.Printf("  %s\n", e)
		}
	} else {
		fmt.Println("  No engines detected")
	}
	fmt.Println()

	return 0
}
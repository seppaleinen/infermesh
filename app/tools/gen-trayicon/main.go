// Command gen-trayicon regenerates app/assets/*.png icons — the monochrome
// (black + alpha) "mesh" glyph used as the macOS menu-bar template icon and the
// tray icon fallback on Linux/Windows. Generates multiple sizes (16x16, 22x22,
// 32x32, 64x64) as requested in issue #47 "Need an icon".
//
// macOS template images must be black + alpha only (white is treated as
// transparent), so every pixel is RGB(0,0,0) and only the alpha channel varies.
//
// stdlib only: image, image/color, image/png, math, os, runtime.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
)

// Define the icon sizes we want (16px tray, 22px menu, 32px big, 64px large)
var sizes = []int{16, 22, 32, 64}

const (
	nodeRadius = 2.3 // node dot radius (px) — kept as-is for proportional scaling
	lineWidth  = 0.9 // connecting wire half-width (px)
)

// nodes is a 3x3 grid of dots, 5px apart, scaled for each size.
// This generates the mesh glyph layout centered in the canvas.
func generateNodes(size int) [][2]float64 {
	var pts [][2]float64
	spacing := 5.0
	// Center grid: starting x = (size - (2 * spacing)) / 2? Actually 3 nodes wide: cols = 3, total width = 2*spacing
	// Starting offset from left: (size - (2 * spacing)) / 2
	start := (float64(size) - 2*spacing) / 2
	for row := 0; row < 3; row++ {
		for col := 0; col < 3; col++ {
			x := start + float64(col)*spacing
			y := start + float64(row)*spacing
			pts = append(pts, [2]float64{x, y})
		}
	}
	return pts
}

// edges connect each node to its right and lower neighbours, forming the
// wire-mesh grid of the InferMesh glyph. Scales with size.
func generateEdges(nodes [][2]float64) [][4]float64 {
	var segs [][4]float64
	for i, n := range nodes {
		if i%3 < 2 { // right neighbour
			j := nodes[i+1]
			segs = append(segs, [4]float64{n[0], n[1], j[0], j[1]})
		}
		if i < 6 { // lower neighbour
			j := nodes[i+3]
			segs = append(segs, [4]float64{n[0], n[1], j[0], j[1]})
		}
	}
	return segs
}

// distToSegment returns the shortest distance from point (px,py) to the line
// segment from (ax,ay) to (bx,by).
func distToSegment(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	if dx == 0 && dy == 0 {
		return math.Hypot(px-ax, py-ay)
	}
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// clamp01 clamps v into [0,1].
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// generateIcon creates a PNG icon of the given size.
func generateIcon(size int) (*image.RGBA, [][2]float64, [][4]float64) {
	nodes := generateNodes(size)
	edges := generateEdges(nodes)

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			cx, cy := float64(px)+0.5, float64(py)+0.5

			nodeDist := math.Inf(1)
			for _, n := range nodes {
				if d := math.Hypot(cx-n[0], cy-n[1]); d < nodeDist {
					nodeDist = d
				}
			}
			lineDist := math.Inf(1)
			for _, e := range edges {
				if d := distToSegment(cx, cy, e[0], e[1], e[2], e[3]); d < lineDist {
					lineDist = d
				}
			}

			// 1px soft edge on every shape: d <= r-0.5 is fully opaque,
			// d >= r+0.5 is fully transparent.
			nodeAlpha := clamp01(nodeRadius + 0.5 - nodeDist)
			lineAlpha := clamp01(lineWidth + 0.5 - lineDist)
			alpha := uint8(math.Round(math.Max(nodeAlpha, lineAlpha) * 255))
			img.SetRGBA(px, py, color.RGBA{R: 0, G: 0, B: 0, A: alpha})
		}
	}
	return img, nodes, edges
}

func main() {
	_, srcFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "gen-trayicon: cannot locate source file")
		os.Exit(1)
	}
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(srcFile))) // gen-trayicon -> tools -> app/
	assetsDir := filepath.Join(appDir, "assets")

	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "gen-trayicon: %v\n", err)
		os.Exit(1)
	}

	for _, size := range sizes {
		img, _, _ := generateIcon(size)

		out := filepath.Join(assetsDir, fmt.Sprintf("trayIcon%d.png", size))

		f, err := os.Create(out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-trayicon: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if err := f.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "gen-trayicon: %v\n", err)
			}
		}()

		if err := png.Encode(f, img); err != nil {
			fmt.Fprintf(os.Stderr, "gen-trayicon: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("gen-trayicon: wrote %s (%dx%d)\n", out, size, size)
	}
}
// Command gen-trayicon regenerates app/assets/trayTemplate.png — the monochrome
// (black + alpha) "mesh" glyph used as the macOS menu-bar template icon and the
// tray icon fallback on Linux/Windows.
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

// size is the icon canvas, in pixels. 22x22 matches the conventional macOS
// menu-bar template size and stays readable (>=16) on Linux/Windows trays.
const size = 22

const (
	nodeRadius = 2.3 // node dot radius (px)
	lineWidth  = 0.9 // connecting wire half-width (px)
)

// nodes is a 3x3 grid of dots, 5px apart, centered on the 22px canvas.
var nodes = func() [][2]float64 {
	var pts [][2]float64
	for row := 0; row < 3; row++ {
		for col := 0; col < 3; col++ {
			pts = append(pts, [2]float64{6 + float64(col)*5, 6 + float64(row)*5})
		}
	}
	return pts
}()

// edges connect each node to its right and lower neighbours, forming the
// wire-mesh grid of the InferMesh glyph.
var edges = func() [][4]float64 {
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
}()

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

func main() {
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

	// Resolve the output relative to this source file so the generator works
	// regardless of the working directory it is invoked from. main.go lives at
	// app/tools/gen-trayicon/main.go, so three Dir() calls climb to app/.
	_, srcFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "gen-trayicon: cannot locate source file")
		os.Exit(1)
	}
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(srcFile))) // gen-trayicon -> tools -> app/
	out := filepath.Join(appDir, "assets", "trayTemplate.png")

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "gen-trayicon: %v\n", err)
		os.Exit(1)
	}
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
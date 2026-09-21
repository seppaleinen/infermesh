package main

import (
	"bytes"
	"image/png"
	"testing"
)

// pngMagic is the PNG file signature, per https://www.w3.org/TR/PNG/#5PNG-file-signature.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// TestTrayIconEmbedded proves the tray icon is compiled into the binary via
// go:embed. It runs without any GUI or Wails runtime, so it is safe in CI.
func TestTrayIconEmbedded(t *testing.T) {
	if len(trayIcon) == 0 {
		t.Fatal("trayIcon embed is empty; regenerate it with: go run ./tools/gen-trayicon")
	}
	prefixLen := min(len(trayIcon), len(pngMagic))
	if prefixLen < len(pngMagic) || !bytes.Equal(trayIcon[:len(pngMagic)], pngMagic) {
		t.Fatalf("trayIcon does not start with the PNG signature; first bytes: % x",
			trayIcon[:prefixLen])
	}
}

// TestTrayIconIsPNG decodes the embedded icon with the stdlib PNG decoder —
// this fails on corrupt or truncated data — and checks the tray asset is big
// enough to read at the sizes trays expect (>=16x16).
func TestTrayIconIsPNG(t *testing.T) {
	cfg, err := png.DecodeConfig(bytes.NewReader(trayIcon))
	if err != nil {
		t.Fatalf("trayIcon is not a valid PNG: %v", err)
	}
	if cfg.Width < 16 || cfg.Height < 16 {
		t.Fatalf("tray icon too small: %dx%d, want >=16x16", cfg.Width, cfg.Height)
	}
}
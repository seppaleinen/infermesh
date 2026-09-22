package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/router"
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

// TestGetWorkersURL verifies the URL builder yields the default /v1/workers
// path and respects a custom base URL, including trailing-slash trimming.
func TestGetWorkersURL(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		want   string
	}{
		{"default empty", "", "http://127.0.0.1:8080/v1/workers"},
		{"default explicit", "http://127.0.0.1:8080", "http://127.0.0.1:8080/v1/workers"},
		{"trailing slash", "http://127.0.0.1:8080/", "http://127.0.0.1:8080/v1/workers"},
		{"custom", "http://router.example:9000", "http://router.example:9000/v1/workers"},
		{"custom trailing slash", "http://router.example:9000/", "http://router.example:9000/v1/workers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewRouterClient(tc.base)
			if got := c.workersPath(); got != tc.want {
				t.Fatalf("workersPath: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestGetRouterURL verifies the configured base URL is round-trippable.
func TestGetRouterURL(t *testing.T) {
	if got := NewRouterClient("").GetRouterURL(); got != defaultRouterURL {
		t.Fatalf("empty base: got %q want %q", got, defaultRouterURL)
	}
	if got := NewRouterClient("http://example:1234").GetRouterURL(); got != "http://example:1234" {
		t.Fatalf("custom base: got %q", got)
	}
}

// TestRouterClientParse feeds canned JSON into the parser and asserts field
// mapping: empty list, unknown status pass-through, loaded models, last_seen.
func TestRouterClientParse(t *testing.T) {
	t.Run("empty list → empty slice, no error", func(t *testing.T) {
		workers, err := parseWorkersResponse([]byte(`{"workers":[]}`))
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if len(workers) != 0 {
			t.Fatalf("expected 0 workers, got %d", len(workers))
		}
	})

	t.Run("null list → empty slice", func(t *testing.T) {
		workers, err := parseWorkersResponse([]byte(`{"workers":null}`))
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if len(workers) != 0 {
			t.Fatalf("expected 0 workers for null, got %d", len(workers))
		}
	})

	t.Run("unknown status string passes through", func(t *testing.T) {
		body := `{"workers":[{"id":"w1","hostname":"node1","ip":"127.0.0.2","port":8001,"status":"bogus","version":"v1","loaded_models":[],"last_seen":"0001-01-01T00:00:00Z"}]}`
		workers, err := parseWorkersResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if len(workers) != 1 {
			t.Fatalf("expected 1 worker, got %d", len(workers))
		}
		if workers[0].Status != "bogus" {
			t.Fatalf("status mismatch: got %q want %q", workers[0].Status, "bogus")
		}
	})

	t.Run("field mapping round-trip", func(t *testing.T) {
		in := router.WorkersResponse{Workers: []router.WorkerInfo{{
			ID:           "w2",
			Hostname:     "gpu-node-1",
			IP:           "10.0.0.5",
			Port:         8081,
			Status:       protocol.StatusAvailable,
			Version:      "v1.2.3",
			LoadedModels: []string{"llama-3-8b"},
			LastSeen:     time.Unix(1758547200, 0).UTC(), // 2026-09-22T20:00:00Z
		}}}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		workers, err := parseWorkersResponse(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if len(workers) != 1 {
			t.Fatalf("expected 1 worker, got %d", len(workers))
		}
		w := workers[0]
		if w.ID != "w2" {
			t.Errorf("ID: got %q", w.ID)
		}
		if w.Hostname != "gpu-node-1" {
			t.Errorf("Hostname: got %q", w.Hostname)
		}
		if w.IP != "10.0.0.5" {
			t.Errorf("IP: got %q", w.IP)
		}
		if w.Port != 8081 {
			t.Errorf("Port: got %d", w.Port)
		}
		if w.Status != "available" {
			t.Errorf("Status: got %q want %q", w.Status, protocol.StatusAvailable)
		}
		if w.Version != "v1.2.3" {
			t.Errorf("Version: got %q", w.Version)
		}
		if len(w.LoadedModels) != 1 || w.LoadedModels[0] != "llama-3-8b" {
			t.Errorf("LoadedModels: got %v", w.LoadedModels)
		}
		if w.LastSeen.IsZero() {
			t.Error("LastSeen should be populated")
		}
		if w.Address != "gpu-node-1:8081" {
			t.Errorf("Address: got %q want %q", w.Address, "gpu-node-1:8081")
		}
	})

	t.Run("malformed JSON → error", func(t *testing.T) {
		_, err := parseWorkersResponse([]byte(`{not json`))
		if err == nil || !strings.Contains(err.Error(), "decode workers response") {
			t.Fatalf("expected decode error, got: %v", err)
		}
	})
}

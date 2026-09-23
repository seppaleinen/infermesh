package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
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
		name string
		base string
		want string
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

// TestSettingsRoundTrip verifies that saving and loading settings preserves
// all fields, including unicode model paths. It also asserts that the YAML
// output contains no "secrets" key because secrets must only exist in the
// keyring.
func TestSettingsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.yml")

	s := DefaultSettings()
	s.RouterAddr = ":9000"
	s.WorkerBackend = "ollama"
	s.WorkerModelPath = "/Users/👤/models/🤖-model.gguf"
	s.WorkerPort = 9001
	s.WorkerEnableHealthChecks = boolPtr(false)
	s.RelayURL = "ws://relay.example:8080"
	s.Secrets = map[string]string{
		"router/apikey":     "test-api-key",
		"worker/customauth": "test-custom-auth",
	}
	s.SecretRefs = map[string]string{
		"router/apikey":     "router/apikey",
		"worker/customauth": "worker/customauth",
	}

	if err := SaveTo(s, path); err != nil {
		t.Fatalf("SaveTo failed: %v", err)
	}

	// Verify YAML contains no "secrets" key
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var rawMap map[string]any
	if err := yaml.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := rawMap["secrets"]; ok {
		t.Fatal("YAML must not contain 'secrets' key")
	}
	if _, ok := rawMap["secret_refs"]; !ok {
		t.Fatal("YAML must contain 'secret_refs' key")
	}

	// Load and verify fields
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}
	loaded = mergeDefaults(loaded)

	if loaded.RouterAddr != s.RouterAddr {
		t.Errorf("RouterAddr: got %q want %q", loaded.RouterAddr, s.RouterAddr)
	}
	if loaded.WorkerBackend != s.WorkerBackend {
		t.Errorf("WorkerBackend: got %q want %q", loaded.WorkerBackend, s.WorkerBackend)
	}
	if loaded.WorkerModelPath != s.WorkerModelPath {
		t.Errorf("WorkerModelPath: got %q want %q", loaded.WorkerModelPath, s.WorkerModelPath)
	}
	if loaded.WorkerPort != s.WorkerPort {
		t.Errorf("WorkerPort: got %d want %d", loaded.WorkerPort, s.WorkerPort)
	}
	if loaded.WorkerEnableHealthChecks == nil || s.WorkerEnableHealthChecks == nil || *loaded.WorkerEnableHealthChecks != *s.WorkerEnableHealthChecks {
		t.Errorf("WorkerEnableHealthChecks: got %v want %v", loaded.WorkerEnableHealthChecks, s.WorkerEnableHealthChecks)
	}
	if loaded.RelayURL != s.RelayURL {
		t.Errorf("RelayURL: got %q want %q", loaded.RelayURL, s.RelayURL)
	}
	if len(loaded.Secrets) != 0 {
		t.Errorf("Secrets should be empty after load, got %v", loaded.Secrets)
	}
	if loaded.SecretRefs["router/apikey"] != "router/apikey" {
		t.Errorf("SecretRefs router/apikey missing")
	}
	if loaded.SecretRefs["worker/customauth"] != "worker/customauth" {
		t.Errorf("SecretRefs worker/customauth missing")
	}
}

// TestSettingsMissingFile verifies that loading a non-existent file returns
// a zero Settings and nil error.
func TestSettingsMissingFile(t *testing.T) {
	s, err := LoadFrom("/nonexistent/path/settings.yml")
	if err != nil {
		t.Fatalf("LoadFrom should not error on missing file: %v", err)
	}
	if s.RouterAddr != "" || s.WorkerBackend != "" || s.WorkerPort != 0 {
		t.Errorf("expected zero Settings, got: %+v", s)
	}
}

// TestKeyringUnavailableHardFails verifies the hard-fail contract: when the
// keyring is unavailable and secrets are present, SaveSettings must return
// errKeyringUnavailable before touching the filesystem.
func TestKeyringUnavailableHardFails(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.yml")

	cs := NewConfigService(failKeyring{}, path)
	s := DefaultSettings()
	s.Secrets = map[string]string{
		"router/apikey": "should-never-be-written",
	}

	ok, err := cs.SaveSettings(s)
	if ok {
		t.Fatal("SaveSettings should return false when keyring unavailable")
	}
	if !errors.Is(err, errKeyringUnavailable) {
		t.Fatalf("expected errKeyringUnavailable, got: %v", err)
	}

	// Verify no plaintext secret was written to disk
	raw, err := os.ReadFile(path)
	if err == nil {
		var rawMap map[string]any
		if yaml.Unmarshal(raw, &rawMap) == nil {
			if _, ok := rawMap["secrets"]; ok {
				t.Fatal("YAML must not contain 'secrets' key when keyring down")
			}
		}
	}
}

// TestKeyringRoundTrip exercises the full secret lifecycle with the in-memory
// keyring: SetSecret → GetSecret → DeleteSecret → GetSecret returns not-found.
func TestKeyringRoundTrip(t *testing.T) {
	cs := NewConfigService(newMemKeyring(), filepath.Join(t.TempDir(), "settings.yml"))

	if err := cs.SetSecret("router/apikey", "my-secret-key"); err != nil {
		t.Fatalf("SetSecret failed: %v", err)
	}

	got, err := cs.GetSecret("router/apikey")
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	if got != "my-secret-key" {
		t.Errorf("GetSecret: got %q want %q", got, "my-secret-key")
	}

	if err := cs.DeleteSecret("router/apikey"); err != nil {
		t.Fatalf("DeleteSecret failed: %v", err)
	}

	_, err = cs.GetSecret("router/apikey")
	if err == nil {
		t.Fatal("GetSecret after DeleteSecret should return not-found")
	}
	if !errors.Is(err, errNotFound) {
		t.Errorf("expected errNotFound, got: %v", err)
	}
}

// TestDefaultsMatchCLI verifies that DefaultSettings() matches the documented
// CLI defaults and that the keyring service name is correct.
func TestDefaultsMatchCLI(t *testing.T) {
	s := DefaultSettings()
	if s.RouterAddr != ":8080" {
		t.Errorf("RouterAddr: got %q want %q", s.RouterAddr, ":8080")
	}
	if s.WorkerBackend != "llama-cpp" {
		t.Errorf("WorkerBackend: got %q want %q", s.WorkerBackend, "llama-cpp")
	}
	if s.WorkerPort != 8081 {
		t.Errorf("WorkerPort: got %d want %d", s.WorkerPort, 8081)
	}
	if s.WorkerEnableHealthChecks == nil || !*s.WorkerEnableHealthChecks {
		t.Error("WorkerEnableHealthChecks should be true")
	}
	if s.DevMode == nil || !*s.DevMode {
		t.Error("DevMode should be true")
	}
	if s.AutoStartOnLogin == nil || *s.AutoStartOnLogin {
		t.Error("AutoStartOnLogin should be false by default (opt-in)")
	}

	cs := NewConfigService(newMemKeyring(), filepath.Join(t.TempDir(), "settings.yml"))
	if cs.GetKeyringServiceName() != "infermesh" {
		t.Errorf("GetKeyringServiceName: got %q want %q", cs.GetKeyringServiceName(), "infermesh")
	}
}

// TestKeyringAvailableOnHost verifies that the production keyring reports
// available on macOS. On other platforms the test is skipped.
func TestKeyringAvailableOnHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain availability probe only runs on macOS")
	}
	if !newProdKeyring().Available() {
		t.Fatal("prod keyring should be available on macOS")
	}
}

// TestSaveSettingsStripsSecretsFromYAML verifies that when the keyring is
// available, SaveSettings writes YAML without secret values but with
// SecretRefs populated, and returns (true, nil).
func TestSaveSettingsStripsSecretsFromYAML(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.yml")

	cs := NewConfigService(newMemKeyring(), path)
	s := DefaultSettings()
	s.Secrets = map[string]string{
		"router/apikey": "secret-value",
		"mtls/cert":     "cert-value",
	}

	ok, err := cs.SaveSettings(s)
	if !ok {
		t.Fatal("SaveSettings should return true when keyring available")
	}
	if err != nil {
		t.Fatalf("SaveSettings returned error: %v", err)
	}

	// Verify YAML has no secret values
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var rawMap map[string]any
	if err := yaml.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := rawMap["secrets"]; ok {
		t.Fatal("YAML must not contain 'secrets' key")
	}
	secretRefs, ok := rawMap["secret_refs"].(map[string]any)
	if !ok {
		t.Fatal("YAML must contain 'secret_refs' map")
	}
	if secretRefs["router/apikey"] != "router/apikey" {
		t.Errorf("secret_refs[router/apikey] missing")
	}
	if secretRefs["mtls/cert"] != "mtls/cert" {
		t.Errorf("secret_refs[mtls/cert] missing")
	}
}

// TestSaveSettingsNoSecretsKeyringDown verifies that when the keyring is
// unavailable but the Settings contain no secrets, SaveSettings succeeds
// and writes YAML normally.
func TestSaveSettingsNoSecretsKeyringDown(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.yml")

	cs := NewConfigService(failKeyring{}, path)
	s := DefaultSettings()
	s.RouterAddr = ":9999"
	// No secrets

	ok, err := cs.SaveSettings(s)
	if ok {
		t.Fatal("SaveSettings should return false when keyring down (even without secrets, per contract)")
	}
	if err != nil {
		t.Fatalf("expected nil error when no secrets, got: %v", err)
	}

	// Verify YAML was written normally
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var rawMap map[string]any
	if err := yaml.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if rawMap["router_addr"] != ":9999" {
		t.Errorf("router_addr not written: got %v", rawMap["router_addr"])
	}
}

// TestNewRouterClientNormalizesBareHostPort verifies that NewRouterClient
// normalises a bare ":8080" to "http://:8080" (the workersPath will include it).
func TestNewRouterClientNormalizesBareHostPort(t *testing.T) {
	c := NewRouterClient(":8080")
	if c.workersPath() != "http://:8080/v1/workers" {
		t.Errorf("workersPath: got %q want %q", c.workersPath(), "http://:8080/v1/workers")
	}
}

// TestSummarizePoolStatus verifies the tray status/tooltip summary for each
// combination of router/worker process states (issue #45).
func TestSummarizePoolStatus(t *testing.T) {
	running := ProcessStatus{State: StateRunning, Running: true}
	stopped := ProcessStatus{State: StateStopped}
	cases := []struct {
		name string
		st   SupervisorStatus
		want string
	}{
		{"both running", SupervisorStatus{Router: running, Worker: running}, "Router + worker running"},
		{"router only", SupervisorStatus{Router: running, Worker: stopped}, "Router running"},
		{"worker only", SupervisorStatus{Router: stopped, Worker: running}, "Worker running"},
		{"both stopped", SupervisorStatus{Router: stopped, Worker: stopped}, "Stopped"},
		{"router error state counts as not running", SupervisorStatus{
			Router: ProcessStatus{State: StateError, Error: "boom"},
			Worker: running,
		}, "Worker running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizePoolStatus(tc.st); got != tc.want {
				t.Errorf("summarizePoolStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// stubCloseToTrayWindow is a GUI-free stand-in for *application.WebviewWindow
// implementing the closeToTrayWindow interface (issue #49). It records how
// many times Hide was called and captures the registered hook + its event type
// so tests can drive it without a live window.
type stubCloseToTrayWindow struct {
	hideCalls int
	hookType  events.WindowEventType
	hook      func(*application.WindowEvent)
}

func (s *stubCloseToTrayWindow) Hide() application.Window {
	s.hideCalls++
	return nil
}

func (s *stubCloseToTrayWindow) RegisterHook(eventType events.WindowEventType, callback func(*application.WindowEvent)) func() {
	s.hookType = eventType
	s.hook = callback
	return func() {}
}

// TestRegisterCloseToTrayHidesAndCancels verifies the WindowClosing hook hides
// the window and cancels the event on every invocation, so wails' default
// close listeners (which destroy the window and make Show() a no-op) never
// run. The multi-invocation cases assert idempotence: repeated red-X just
// re-hides.
func TestRegisterCloseToTrayHidesAndCancels(t *testing.T) {
	cases := []struct {
		name     string
		invokes  int
		wantHits int
	}{
		{"single red-X", 1, 1},
		{"idempotent: repeated red-X just re-hides", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCloseToTrayWindow{}
			registerCloseToTray(stub)

			for i := 0; i < tc.invokes; i++ {
				evt := application.NewWindowEvent()
				stub.hook(evt)
				if !evt.IsCancelled() {
					t.Fatalf("invocation %d: event not cancelled", i+1)
				}
			}
			if stub.hideCalls != tc.wantHits {
				t.Errorf("Hide calls: got %d want %d", stub.hideCalls, tc.wantHits)
			}
		})
	}
}

// TestRegisterCloseToTrayHookEventType verifies the hook is registered for the
// Common.WindowClosing event, which is the event macOS red-X, Windows WM_CLOSE
// and Linux delete-event all funnel through (issue #49).
func TestRegisterCloseToTrayHookEventType(t *testing.T) {
	stub := &stubCloseToTrayWindow{}
	registerCloseToTray(stub)

	if stub.hookType != events.Common.WindowClosing {
		t.Fatalf("hook event type: got %d want %d", stub.hookType, events.Common.WindowClosing)
	}
}

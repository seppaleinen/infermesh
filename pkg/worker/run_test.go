package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestRouterBaseFromListenAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{"default port", ":8080", "http://127.0.0.1:8080", false},
		{"any host", "0.0.0.0:8080", "http://127.0.0.1:8080", false},
		{"localhost", "localhost:8080", "http://127.0.0.1:8080", false},
		{"explicit loopback", "127.0.0.1:9000", "http://127.0.0.1:9000", false},
		{"ipv6 loopback", "[::1]:8080", "http://127.0.0.1:8080", false},
		{"non-loopback host preserved", "myhost:8080", "http://myhost:8080", false},
		{"missing port", "noport", "", true},
		{"port only", "8080", "", true},
		{"non-numeric port", ":abc", "", true},
		{"port out of range", ":70000", "", true},
		{"port zero", ":0", "", true},
		{"malformed brackets", "[::1", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RouterBaseFromListenAddr(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.addr, err)
			}
			if got != tt.want {
				t.Fatalf("RouterBaseFromListenAddr(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestNewBackendFromName(t *testing.T) {
	tests := []struct {
		name       string
		backendURL string
		want       string
	}{
		{"llama-cpp", "", "llama-cpp"},
		{"ollama", "", "ollama"},
		{"lmstudio", "", "lmstudio"},
		{"vllm", "", "vllm"},
		{"custom", "", "custom"},
		{"unknown name falls back to llama-cpp", "", "llama-cpp"},
		{"llama-cpp with mock URL", "http://127.0.0.1:18080", "llama-cpp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewBackendFromName(tt.name, "", tt.backendURL, 0, 0, 0)
			if err != nil {
				t.Fatalf("NewBackendFromName(%q) unexpected error: %v", tt.name, err)
			}
			if got := b.Name(); got != tt.want {
				t.Fatalf("NewBackendFromName(%q).Name() = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestNewBackendFromName_PropagatesThreads verifies that the --threads flag
// reaches the llama-cpp backend (issue #139). Non-llama-cpp backends ignore
// the flag (their GetMetrics reports Threads=0).
func TestNewBackendFromName_PropagatesThreads(t *testing.T) {
	tests := []struct {
		name        string
		backend     string
		threads     int
		wantThreads int
	}{
		{"llama-cpp with 8 threads", "llama-cpp", 8, 8},
		{"llama-cpp with 1 thread", "llama-cpp", 1, 1},
		{"ollama ignores threads", "ollama", 8, 0},
		{"lmstudio ignores threads", "lmstudio", 8, 0},
		{"vllm ignores threads", "vllm", 8, 0},
		{"custom ignores threads", "custom", 8, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewBackendFromName(tt.backend, "", "", 0, 0, tt.threads)
			if err != nil {
				t.Fatalf("NewBackendFromName(%q) unexpected error: %v", tt.backend, err)
			}
			m, err := b.GetMetrics()
			if err != nil {
				t.Fatalf("GetMetrics() returned error: %v", err)
			}
			if m.Threads != tt.wantThreads {
				t.Errorf("GetMetrics().Threads = %d, want %d", m.Threads, tt.wantThreads)
			}
		})
	}
}

// TestNewBackendFromName_ThreadsDefault verifies that a non-positive threads
// value falls back to 4 (the historical hardcoded value), keeping existing
// setups byte-for-byte unchanged (issue #139).
func TestNewBackendFromName_ThreadsDefault(t *testing.T) {
	b, err := NewBackendFromName("llama-cpp", "", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("NewBackendFromName unexpected error: %v", err)
	}
	m, err := b.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics() returned error: %v", err)
	}
	if m.Threads != 4 {
		t.Errorf("expected default threads 4, got %d", m.Threads)
	}
}

func TestValidateCombinedFlags(t *testing.T) {
	tests := []struct {
		name       string
		prodMode   bool
		workerMode bool
		routerPort int
		workerPort int
		backend    string
		wantErr    string // substring expected in the error; "" means no error
	}{
		{"dev combined ok", false, true, 8080, 8081, "lmstudio", ""},
		{"router only ok", false, false, 8080, 8081, "lmstudio", ""},
		{"prod combined rejected", true, true, 8080, 8081, "lmstudio", "only supported in dev mode"},
		{"worker port equals router port", false, true, 8080, 8080, "lmstudio", "collides with router port"},
		{"worker port equals ollama backend port", false, true, 8080, 11434, "ollama", "collides with the ollama backend"},
		{"worker port equals lmstudio backend port", false, true, 8080, 1234, "lmstudio", "collides with the lmstudio backend"},
		{"worker port equals vllm backend port", false, true, 8080, 8000, "vllm", "collides with the vllm backend"},
		{"worker port equals custom backend port", false, true, 8080, 8000, "custom", "collides with the custom backend"},
		{"worker port equals llama-cpp backend port", false, true, 9000, 8080, "llama-cpp", "collides with the llama-cpp backend"},
		{"distinct ports ok", false, true, 9000, 8081, "llama-cpp", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCombinedFlags(tt.prodMode, tt.workerMode, tt.routerPort, tt.workerPort, tt.backend)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestPrintCapabilities(t *testing.T) {
	var buf strings.Builder
	if rc := PrintCapabilities(&buf); rc != 0 {
		t.Fatalf("PrintCapabilities returned %d, want 0", rc)
	}
}

// TestEffectiveReloadConfig tests that effectiveReloadConfig reads env vars
// and preserves current values when env vars are empty.
func TestEffectiveReloadConfig(t *testing.T) {
	h := &Handle{
		log: slog.New(slog.NewTextHandler(os.Stdout, nil)),
		regConfig: ReloadableConfig{
			RouterBase:     "http://127.0.0.1:8080",
			RelayURL:       "ws://127.0.0.1:9000",
			RelayAPIKey:    "old-key",
			APIKey:         "old-api",
			AllowedModels:  "model-a",
			ExcludedModels: "model-b",
		},
	}

	// First: set new router URL via env var, leave others empty
	_ = os.Setenv("INFERMESH_ROUTER_URL", "http://127.0.0.1:9000")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_ROUTER_URL") })
	newCfg := h.effectiveReloadConfig(h.regConfig)
	if newCfg.RouterBase != "http://127.0.0.1:9000" {
		t.Fatalf("expected RouterBase http://127.0.0.1:9000, got %q", newCfg.RouterBase)
	}
	if newCfg.RelayURL != "ws://127.0.0.1:9000" {
		t.Fatalf("expected RelayURL preserved, got %q", newCfg.RelayURL)
	}
	if newCfg.RelayAPIKey != "old-key" {
		t.Fatalf("expected RelayAPIKey preserved, got %q", newCfg.RelayAPIKey)
	}
	if newCfg.APIKey != "old-api" {
		t.Fatalf("expected APIKey preserved, got %q", newCfg.APIKey)
	}
	if newCfg.AllowedModels != "model-a" {
		t.Fatalf("expected AllowedModels preserved, got %q", newCfg.AllowedModels)
	}
	if newCfg.ExcludedModels != "model-b" {
		t.Fatalf("expected ExcludedModels preserved, got %q", newCfg.ExcludedModels)
	}

	// Second: set multiple env vars
	_ = os.Setenv("INFERMESH_RELAY_URL", "ws://127.0.0.1:8000")
	_ = os.Setenv("INFERMESH_API_KEY", "new-api")
	defer func() {
		_ = os.Unsetenv("INFERMESH_RELAY_URL")
		_ = os.Unsetenv("INFERMESH_API_KEY")
	}()
	newCfg = h.effectiveReloadConfig(h.regConfig)
	if newCfg.RelayURL != "ws://127.0.0.1:8000" {
		t.Fatalf("expected RelayURL ws://127.0.0.1:8000, got %q", newCfg.RelayURL)
	}
	if newCfg.APIKey != "new-api" {
		t.Fatalf("expected APIKey new-api, got %q", newCfg.APIKey)
	}
	if newCfg.RouterBase != "http://127.0.0.1:9000" {
		t.Fatalf("expected RouterBase preserved, got %q", newCfg.RouterBase)
	}
}

// TestReloadConfig_ReloadsRouterBase tests that ReloadConfig swaps the
// registration loop when RouterBase changes via env var.
func TestReloadConfig_ReloadsRouterBase(t *testing.T) {
	// Use a real worker with stub router for integration-style test
	var registered int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/dev/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registered, 1)
		w.WriteHeader(http.StatusOK)
	})
	stub := httptest.NewServer(mux)
	defer stub.Close()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            true,
		RouterBase:         stub.URL,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// Wait for initial registration
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&registered) > 0
	})

	// Reload with new router URL
	_ = os.Setenv("INFERMESH_ROUTER_URL", "http://127.0.0.1:19000")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_ROUTER_URL") })

	// Create a second stub to verify the switch
	var registered2 int32
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/v1/dev/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registered2, 1)
		w.WriteHeader(http.StatusOK)
	})
	stub2 := httptest.NewServer(mux2)
	defer stub2.Close()

	// Update env to point to second stub (but keep old RouterBase for comparison)
	_ = os.Setenv("INFERMESH_ROUTER_URL", stub2.URL)
	err = h.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Wait for new registration
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&registered2) > 0
	})

	// Verify the new registration went to the second stub
	if atomic.LoadInt32(&registered2) == 0 {
		t.Fatal("worker did not register with the new router after reload")
	}

	// Verify the config was updated
	if h.regConfig.RouterBase != stub2.URL {
		t.Fatalf("expected RouterBase %q, got %q", stub2.URL, h.regConfig.RouterBase)
	}
}

// TestReloadConfig_SwapsRelayToHTTP tests that ReloadConfig can swap the
// registration path from relay (WebSocket) to HTTP router base and back.
func TestReloadConfig_SwapsRelayToHTTP(t *testing.T) {
	// This test verifies the config swapping logic via effectiveReloadConfig
	// Full integration test would require a relay server which is more complex.
	h := &Handle{
		log: slog.New(slog.NewTextHandler(os.Stdout, nil)),
		regConfig: ReloadableConfig{
			RouterBase:  "",
			RelayURL:    "ws://127.0.0.1:9000",
			RelayAPIKey: "test-key",
		},
	}

	// First reload: set router URL, clear relay URL -> should switch to HTTP
	_ = os.Setenv("INFERMESH_ROUTER_URL", "http://127.0.0.1:8080")
	_ = os.Unsetenv("INFERMESH_RELAY_URL")
	newCfg := h.effectiveReloadConfig(h.regConfig)
	if newCfg.RouterBase != "http://127.0.0.1:8080" {
		t.Fatalf("expected RouterBase to be http://127.0.0.1:8080, got %q", newCfg.RouterBase)
	}
	// When env var is unset, current RelayURL value is preserved
	if newCfg.RelayURL != "ws://127.0.0.1:9000" {
		t.Fatalf("expected RelayURL to preserve ws://127.0.0.1:9000, got %q", newCfg.RelayURL)
	}
	_ = os.Unsetenv("INFERMESH_ROUTER_URL")

	// Second reload: set relay URL again -> maintains current config
	// Update h.regConfig to simulate ReloadConfig having stored the first change
	h.regConfig = newCfg
	_ = os.Setenv("INFERMESH_RELAY_URL", "ws://127.0.0.1:9000")
	newCfg = h.effectiveReloadConfig(h.regConfig)
	if newCfg.RelayURL != "ws://127.0.0.1:9000" {
		t.Fatalf("expected RelayURL to be ws://127.0.0.1:9000, got %q", newCfg.RelayURL)
	}
	// When INFERMESH_ROUTER_URL is unset, current RouterBase value is preserved from regConfig
	if newCfg.RouterBase != "http://127.0.0.1:8080" {
		t.Fatalf("expected RouterBase to preserve http://127.0.0.1:8080, got %q", newCfg.RouterBase)
	}
	_ = os.Unsetenv("INFERMESH_RELAY_URL")
	_ = os.Unsetenv("INFERMESH_ROUTER_URL")
}

// TestValidateRunConfig_CatchesMisconfigs tests that ValidateRunConfig catches
// invalid configuration combinations.
func TestValidateRunConfig_CatchesMisconfigs(t *testing.T) {
	tests := []struct {
		name       string
		devMode    bool
		prodRouter string
		routerBase string
		relayURL   string
		wantErr    string
	}{
		{"dev mode ok", true, "", "", "", ""},
		{"dev mode with relay ok", true, "", "", "ws://127.0.0.1:9000", ""},
		{"prod mode with relay rejected", false, "", "", "ws://127.0.0.1:9000", "relay URL is only supported in dev mode"},
		{"prod mode with non-loopback router rejected", false, "", "http://192.168.1.100:8080", "", "prod mode router base must be loopback"},
		{"prod mode with loopback router ok", false, "", "http://127.0.0.1:8080", "", ""},
		{"prod mode with prod-router ok", false, "https://127.0.0.1:8080", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := RunConfig{
				DevMode:            tt.devMode,
				ProdRouter:         tt.prodRouter,
				RouterBase:         tt.routerBase,
				RelayURL:           tt.relayURL,
				EnableHealthChecks: false,
			}
			err := ValidateRunConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error for %s: %v", tt.name, err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", tt.name)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestLocalIP(t *testing.T) {
	ip := LocalIP()
	if ip == "" {
		t.Fatal("LocalIP returned empty string")
	}
	if parsed := net.ParseIP(ip); parsed == nil {
		t.Fatalf("LocalIP returned non-IP value %q", ip)
	}
}

func TestHostname(t *testing.T) {
	if h := Hostname(); h == "" {
		t.Fatal("Hostname returned empty string")
	}
}

// TestRunWorkerDevHTTP exercises the dev-mode runtime end-to-end: the worker
// HTTP server comes up, registers against a stub router over HTTP, and
// Stop() shuts everything down.
func TestRunWorkerDevHTTP(t *testing.T) {
	var registered int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/dev/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registered, 1)
		w.WriteHeader(http.StatusOK)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            true,
		RouterBase:         stub.URL,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if atomic.LoadInt32(&registered) == 0 {
		waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&registered) > 0 })
	}
	if got := atomic.LoadInt32(&registered); got == 0 {
		t.Fatal("worker did not register with the stub router")
	}

	h.Stop()
}

// TestRunWorker_AllowedExcludedModels starts a worker with AllowedModels and
// ExcludedModels against a stub router that captures the posted WorkerInfo
// bodies. It asserts the opt-out flags are present in the initial registration
// AND in a subsequent heartbeat, proving the refresh closures preserve the
// flags after reassigning Capabilities.Models.
func TestRunWorker_AllowedExcludedModels(t *testing.T) {
	var mu sync.Mutex
	var bodies []protocol.WorkerInfo

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body protocol.WorkerInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            true,
		RouterBase:         router.URL,
		AllowedModels:      "llama3-8b, mistral-7b",
		ExcludedModels:     " , gpt-4 ,",
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// DefaultRegisterInterval is 10s, so the first heartbeat fires
	// immediately and the second one after one full interval. Wait for at
	// least 2 heartbeats to prove the refresh closure preserves the opt-out
	// flags after reassigning Capabilities.Models.
	deadline := time.Now().Add(12 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least 2 heartbeats, got %d", n)
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	for i, body := range bodies {
		if len(body.Capabilities.AllowedModels) != 2 {
			t.Errorf("heartbeat %d: expected 2 allowed models, got %d: %v",
				i, len(body.Capabilities.AllowedModels), body.Capabilities.AllowedModels)
		}
		if body.Capabilities.AllowedModels[0] != "llama3-8b" || body.Capabilities.AllowedModels[1] != "mistral-7b" {
			t.Errorf("heartbeat %d: allowed models = %v", i, body.Capabilities.AllowedModels)
		}
		if len(body.Capabilities.ExcludedModels) != 1 {
			t.Errorf("heartbeat %d: expected 1 excluded model, got %d: %v",
				i, len(body.Capabilities.ExcludedModels), body.Capabilities.ExcludedModels)
		}
		if body.Capabilities.ExcludedModels[0] != "gpt-4" {
			t.Errorf("heartbeat %d: excluded models = %v", i, body.Capabilities.ExcludedModels)
		}
	}
}

// TestReloadConfig_AppliesModelFiltersToHeartbeats verifies that after a
// ReloadConfig call with INFERMESH_ALLOWED_MODELS / INFERMESH_EXCLUDED_MODELS,
// the next heartbeat carries the updated model filters.
func TestReloadConfig_AppliesModelFiltersToHeartbeats(t *testing.T) {
	var mu sync.Mutex
	var bodies []protocol.WorkerInfo

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body protocol.WorkerInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            true,
		RouterBase:         router.URL,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// Wait for initial registration (should have no filters initially)
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		return n >= 1
	})

	mu.Lock()
	initialBodies := len(bodies)
	mu.Unlock()

	// Reload with new model filters via env vars
	_ = os.Setenv("INFERMESH_ALLOWED_MODELS", "new-llama, new-mistral")
	_ = os.Setenv("INFERMESH_EXCLUDED_MODELS", "new-gpt4")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_ALLOWED_MODELS") })
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_EXCLUDED_MODELS") })

	err = h.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Wait for at least one more heartbeat with new filters
	deadline := time.Now().Add(12 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > initialBodies {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for post-reload heartbeat (had %d, still %d)", initialBodies, n)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify at least one post-reload heartbeat has the new filters
	mu.Lock()
	defer mu.Unlock()
	found := false
	for i, body := range bodies {
		if len(body.Capabilities.AllowedModels) == 2 &&
			body.Capabilities.AllowedModels[0] == "new-llama" &&
			body.Capabilities.AllowedModels[1] == "new-mistral" &&
			len(body.Capabilities.ExcludedModels) == 1 &&
			body.Capabilities.ExcludedModels[0] == "new-gpt4" {
			found = true
			break
		}
		_ = i // avoid unused var in non-assert path
	}
	if !found {
		t.Fatalf("no heartbeat found with updated model filters; got %d heartbeats", len(bodies))
	}
}

// TestReloadConfig_PropagatesRelayAPIKeyToRegistration verifies that the
// relay API key set via INFERMESH_RELAY_API_KEY is included in the WorkerInfo
// APIKey field of the registration payload after ReloadConfig.
func TestReloadConfig_PropagatesRelayAPIKeyToRegistration(t *testing.T) {
	var mu sync.Mutex
	var bodies []protocol.WorkerInfo

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body protocol.WorkerInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer router.Close()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            true,
		RouterBase:         router.URL,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// Wait for initial registration (no relay API key initially)
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		return n >= 1
	})

	// Reload with new relay API key via env var
	_ = os.Setenv("INFERMESH_RELAY_API_KEY", "test-relay-key-123")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_RELAY_API_KEY") })

	err = h.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Wait for at least one more heartbeat with the new relay API key
	deadline := time.Now().Add(12 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for post-reload heartbeat")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify at least one post-reload heartbeat has the relay API key
	mu.Lock()
	defer mu.Unlock()
	found := false
	for i, body := range bodies {
		if body.APIKey == "test-relay-key-123" {
			found = true
			break
		}
		_ = i
	}
	if !found {
		t.Fatalf("no heartbeat found with updated relay API key; got %d heartbeats", len(bodies))
	}
}

// TestReloadConfig_ConcurrentWithInflightInference verifies that ReloadConfig
// can be called concurrently with in-flight inference requests without
// disrupting them. It uses a mock backend that returns canned responses.
func TestReloadConfig_ConcurrentWithInflightInference(t *testing.T) {
	// Create a server with a mock backend directly (bypassing RunWorker's
	// backend creation which would try to connect to real backends)
	log := testLogger()
	port := freeTestPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := NewServer(log, addr, security.Config{
		DevMode:            true,
		EnableHealthChecks: false,
	})
	mock := newMockBackend()
	mock.SetMockModels([]protocol.ModelInfo{
		{Name: "mock-model", Backend: "mock", Loaded: true},
	})
	srv.SetBackend(mock, "")

	// Start the server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
		}
	}()

	// Wait for server to be ready
	waitFor(t, 5*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// Create a Handle-like wrapper to call ReloadConfig. Since ReloadConfig
	// is a method on *Handle, we can't easily test it without a full Handle.
	// Instead, we test the underlying Server method that ReloadConfig calls:
	// srv.SetModelFilters. The concurrent inference test is done by firing
	// requests while calling SetModelFilters concurrently.
	done := make(chan struct{})
	var inferenceErr error
	var mu sync.Mutex

	// Fire continuous inference requests
	reqBody := `{"model":"mock-model","messages":[{"role":"user","content":"test"}],"max_tokens":10}`
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			select {
			case <-done:
				return
			default:
				req, err := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(reqBody))
				if err != nil {
					mu.Lock()
					inferenceErr = err
					mu.Unlock()
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					mu.Lock()
					inferenceErr = err
					mu.Unlock()
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					mu.Lock()
					inferenceErr = fmt.Errorf("inference returned %d", resp.StatusCode)
					mu.Unlock()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	// While inference is in flight, repeatedly change model filters
	for i := 0; i < 20; i++ {
		srv.SetModelFilters([]string{fmt.Sprintf("model-%d", i)}, []string{"excluded"})
		time.Sleep(5 * time.Millisecond)
	}

	// Stop the inference goroutine and check for errors
	close(done)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if inferenceErr != nil {
		t.Fatalf("in-flight inference failed during concurrent reload: %v", inferenceErr)
	}
	mu.Unlock()

	// Verify a final request works after the concurrent changes
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post-reload inference failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-reload inference returned %d", resp.StatusCode)
	}
}

// TestReloadConfig_RejectsInvalidConfig verifies that ReloadConfig rejects
// invalid configuration combinations (e.g., relay URL in prod mode) and
// leaves the existing registration config intact.
func TestReloadConfig_RejectsInvalidConfig(t *testing.T) {
	// Use a real worker with stub router for integration-style test
	var registered int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/dev/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registered, 1)
		w.WriteHeader(http.StatusOK)
	})
	stub := httptest.NewServer(mux)
	defer stub.Close()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            false, // prod mode
		RouterBase:         stub.URL,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// Wait for initial registration
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&registered) > 0
	})

	// Capture the current regConfig (protected by regMu)
	h.regMu.Lock()
	oldRegConfig := h.regConfig
	h.regMu.Unlock()

	// Attempt to reload with relay URL in prod mode (should be rejected)
	_ = os.Setenv("INFERMESH_RELAY_URL", "ws://127.0.0.1:9000")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_RELAY_URL") })

	err = h.ReloadConfig()
	if err == nil {
		t.Fatal("expected ReloadConfig to fail with relay URL in prod mode, got nil")
	}
	if !strings.Contains(err.Error(), "relay URL is only supported in dev mode") {
		t.Fatalf("expected error about relay URL in prod mode, got: %v", err)
	}

	// Verify the regConfig was NOT changed (reload failed, old config preserved)
	h.regMu.Lock()
	defer h.regMu.Unlock()
	if h.regConfig.RelayURL != oldRegConfig.RelayURL {
		t.Fatalf("regConfig.RelayURL was changed on failed reload: was %q, now %q", oldRegConfig.RelayURL, h.regConfig.RelayURL)
	}
	if h.regConfig.RouterBase != oldRegConfig.RouterBase {
		t.Fatalf("regConfig.RouterBase was changed on failed reload: was %q, now %q", oldRegConfig.RouterBase, h.regConfig.RouterBase)
	}
}

// TestReloadConfig_ChangesAPIKey tests that ReloadConfig changes the
// inference API key via SetAPIKey, and subsequent inference requests
// enforce the new key while rejecting the old one. Uses a direct Server
// with mock backend to avoid real backend dependencies.
func TestReloadConfig_ChangesAPIKey(t *testing.T) {
	log := testLogger()
	port := freeTestPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := NewServer(log, addr, security.Config{
		DevMode:            true,
		EnableHealthChecks: false,
		APIKey:             "initial-key",
	})
	mock := newMockBackend()
	mock.SetMockModels([]protocol.ModelInfo{
		{Name: "mock-model", Backend: "mock", Loaded: true},
	})
	srv.SetBackend(mock, "")

	// Start the server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
		}
	}()

	// Wait for server to be ready
	waitFor(t, 5*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// Helper to fire an inference request with a given API key
	doInference := func(key string) (int, error) {
		reqBody := `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`
		req, err := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, nil
	}

	// First: inference works with the initial key
	code, err := doInference("initial-key")
	if err != nil {
		t.Fatalf("initial inference failed: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("initial inference expected 200, got %d", code)
	}

	// Requests without key should fail (dev mode with key set requires auth)
	code, err = doInference("")
	if err != nil {
		t.Fatalf("no-key inference error: %v", err)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("no-key inference expected 401, got %d", code)
	}

	// Change the API key via SetAPIKey (what ReloadConfig calls)
	srv.SetAPIKey("new-key")

	// Old key should now be rejected
	code, err = doInference("initial-key")
	if err != nil {
		t.Fatalf("old-key inference error: %v", err)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("old-key inference expected 401, got %d", code)
	}

	// New key should be accepted
	code, err = doInference("new-key")
	if err != nil {
		t.Fatalf("new-key inference failed: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("new-key inference expected 200, got %d", code)
	}

	// No key should still fail
	code, err = doInference("")
	if err != nil {
		t.Fatalf("no-key inference error: %v", err)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("no-key inference expected 401, got %d", code)
	}
}

// TestReloadConfig_RestartsMTLSLoop verifies that a prod-mode worker
// registered over mTLS keeps registering after SIGHUP. Before the fix for
// issue #188, ReloadConfig cancelled the mTLS loop via launchRegistration
// (which only handles HTTP/WS paths) and started nothing, so the worker
// became unregistered.
func TestReloadConfig_RestartsMTLSLoop(t *testing.T) {
	caDir := t.TempDir()
	routerDir := t.TempDir()
	workerDir := t.TempDir()

	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCA failed: %v", err)
	}
	routerCertPath, routerKeyPath, err := security.GenerateNodeCert(routerDir, caCertPath, caKeyPath, "router-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert for router failed: %v", err)
	}
	workerCertPath, workerKeyPath, err := security.GenerateNodeCert(workerDir, caCertPath, caKeyPath, "worker-1")
	if err != nil {
		t.Fatalf("GenerateNodeCert for worker failed: %v", err)
	}

	var registered int32
	// Stub router: HTTPS, requires a client cert trusted by the CA.
	caPool := x509.NewCertPool()
	caBytes, _ := os.ReadFile(caCertPath)
	_ = caPool.AppendCertsFromPEM(caBytes)
	routerCert, err := tls.LoadX509KeyPair(routerCertPath, routerKeyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair for router failed: %v", err)
	}
	tlsCfg := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{routerCert},
		MinVersion:   tls.VersionTLS12,
	}
	stub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	stub.TLS = tlsCfg
	stub.StartTLS()
	defer stub.Close()

	// RunWorker needs the router URL to be https://127.0.0.1:<port> so
	// RegisterLoopWithRefreshMTLS dials it with the worker client cert.
	routerURL, err := url.Parse(stub.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	prodRouter := "https://127.0.0.1:" + routerURL.Port()

	port := freeTestPort(t)
	cfg := RunConfig{
		Backend:            "custom",
		ModelPath:          "/tmp/fake-model.bin",
		Port:               port,
		DevMode:            false,
		ProdRouter:         prodRouter,
		MTLSCert:           workerCertPath,
		MTLSKey:            workerKeyPath,
		CertDir:            caDir,
		EnableHealthChecks: false,
	}

	h, err := RunWorker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	defer h.Stop()

	// Wait for initial mTLS registration
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&registered) > 0
	})
	if atomic.LoadInt32(&registered) == 0 {
		t.Fatal("worker did not register over mTLS initially")
	}
	if !h.regUseMTLS {
		t.Fatal("worker was not configured for mTLS registration")
	}

	// SIGHUP reload: must re-launch the mTLS loop, not kill it.
	_ = os.Setenv("INFERMESH_API_KEY", "reloaded-key")
	t.Cleanup(func() { _ = os.Unsetenv("INFERMESH_API_KEY") })
	if err := h.ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Wait for a post-reload heartbeat
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&registered) >= 2
	})
	if atomic.LoadInt32(&registered) < 2 {
		t.Fatal("worker stopped registering over mTLS after SIGHUP")
	}
}

// freeTestPort reserves an ephemeral port on loopback and returns it.
func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitFor polls fn until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

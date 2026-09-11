package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
		name string
		want string
	}{
		{"llama-cpp", "llama-cpp"},
		{"ollama", "ollama"},
		{"lmstudio", "lmstudio"},
		{"vllm", "vllm"},
		{"custom", "custom"},
		{"unknown name falls back to llama-cpp", "llama-cpp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewBackendFromName(tt.name, "")
			if err != nil {
				t.Fatalf("NewBackendFromName(%q) unexpected error: %v", tt.name, err)
			}
			if got := b.Name(); got != tt.want {
				t.Fatalf("NewBackendFromName(%q).Name() = %q, want %q", tt.name, got, tt.want)
			}
		})
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
			err := validateCombinedFlags(tt.prodMode, tt.workerMode, tt.routerPort, tt.workerPort, tt.backend)
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
	out := buf.String()
	for _, want := range []string{
		"=== InferMesh Worker Capabilities ===",
		"GPU:",
		"VRAM:",
		"System:",
		"Models:",
		"Engines:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
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

func TestRunWorkerRequiresModelPathInProd(t *testing.T) {
	_, err := RunWorker(context.Background(), RunConfig{
		Backend: "lmstudio",
		Port:    8081,
		DevMode: false,
	})
	if !errors.Is(err, ErrMissingModelPath) {
		t.Fatalf("expected ErrMissingModelPath, got %v", err)
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

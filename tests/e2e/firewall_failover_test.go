package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFirewallFailover verifies that a non-streaming chat completion
// fails over from a worker whose backend refuses connections (simulating
// a firewall) to a worker with a healthy backend, while a streaming
// request fails fast with HTTP 503 (no failover for streaming).
func TestFirewallFailover(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", routerBin, err)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", workerBin, err)
	}

	mockScript := "mock_backend.py"
	if _, err := os.Stat(mockScript); err != nil {
		t.Skipf("skipping e2e: mock backend script %s not found: %v", mockScript, err)
	}

	// Ports: distinct from other e2e tests. Worker server ports and
	// mock backend ports are intentionally separate so nothing collides.
	const (
		routerPort  = 8101
		workerAPort = 8102 // firewall worker's own HTTP server
		workerBPort = 8103 // healthy worker's own HTTP server
		fwBackend   = 8104 // firewall mock backend (worker A's --backend-url)
		healthyPort = 8105 // healthy mock backend (worker B's --backend-url)
	)
	routerBase := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	// Model file for worker A. Its basename ("model.bin") is the model
	// name the worker advertises. Worker B has no --model-path, so it
	// only advertises the model discovered from its healthy backend.
	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}
	modelName := filepath.Base(modelPath)

	// Start mock backends.
	_ = startExternalMockBackend(t, mockScript, fwBackend, "firewall")
	_ = startExternalMockBackend(t, mockScript, healthyPort, "healthy")

	// Start router.
	var routerStderr bytes.Buffer
	router := exec.Command(routerBin, "--dev-mode", "--addr", fmt.Sprintf("127.0.0.1:%d", routerPort))
	router.Stderr = &routerStderr
	if err := router.Start(); err != nil {
		t.Fatalf("starting router: %v", err)
	}

	// Start worker A (firewall backend).
	var workerAStderr bytes.Buffer
	workerA := exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", fmt.Sprintf("%d", workerAPort),
		"--backend", "custom",
		"--backend-url", fmt.Sprintf("http://127.0.0.1:%d", fwBackend),
		"--model-path", modelPath,
	)
	workerA.Stderr = &workerAStderr
	if err := workerA.Start(); err != nil {
		_ = router.Process.Kill()
		t.Fatalf("starting worker A: %v", err)
	}

	// Start worker B (healthy backend).
	var workerBStderr bytes.Buffer
	workerB := exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", fmt.Sprintf("%d", workerBPort),
		"--backend", "custom",
		"--backend-url", fmt.Sprintf("http://127.0.0.1:%d", healthyPort),
	)
	workerB.Stderr = &workerBStderr
	if err := workerB.Start(); err != nil {
		_ = router.Process.Kill()
		_ = workerA.Process.Kill()
		t.Fatalf("starting worker B: %v", err)
	}

	defer func() {
		_ = workerB.Process.Kill()
		_ = workerA.Process.Kill()
		_ = router.Process.Kill()
		_, _ = workerB.Process.Wait()
		_, _ = workerA.Process.Wait()
		_, _ = router.Process.Wait()
	}()

	// Wait for both workers to register.
	waitForWorker(t, routerBase, workerAPort, 15*time.Second)
	waitForWorker(t, routerBase, workerBPort, 15*time.Second)

	// Wait for the capability cache to hydrate so the router can see
	// worker A's model and route to it.
	waitForModel(t, routerBase, modelName, 15*time.Second)

	// --- Non-streaming: must fail over from worker A to worker B. ---
	chatBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":false}`, modelName)
	chatResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer func() { _ = chatResp.Body.Close() }()
	if chatResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(chatResp.Body)
		t.Fatalf("non-streaming chat: expected 200, got %d (router stderr: %s, workerA stderr: %s, workerB stderr: %s, body: %s)",
			chatResp.StatusCode, routerStderr.String(), workerAStderr.String(), workerBStderr.String(), string(body))
	}
	if ct := chatResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("non-streaming content-type = %q, want application/json", ct)
	}
	raw, _ := io.ReadAll(chatResp.Body)
	if !strings.Contains(string(raw), "failover-verified") {
		t.Errorf("non-streaming response missing failover marker: %s", string(raw))
	}
	if !strings.Contains(string(raw), "choices") {
		t.Errorf("non-streaming response missing choices: %s", string(raw))
	}

	// --- Streaming: must fail fast with 503. ---
	streamBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, modelName)
	streamResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(streamResp.Body)
		t.Errorf("streaming chat: expected 503, got %d (body: %s)", streamResp.StatusCode, string(body))
	}

	// --- SIGTERM all processes → clean exit 0. ---
	for _, cmd := range []*exec.Cmd{workerB, workerA, router} {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("sending SIGTERM: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("process did not exit within 10s of SIGTERM")
		}
	}
}

func startExternalMockBackend(t *testing.T, script string, port int, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("python3", script, "--port", fmt.Sprintf("%d", port), "--mode", mode)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting mock backend (mode=%s port=%d): %v", mode, port, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// waitForModel polls /v1/models until the given model name appears in the
// aggregated list, or the timeout expires. This ensures the capability
// cache has hydrated worker models before routing requests.
func waitForModel(t *testing.T, base, model string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("model %q did not appear in /v1/models within %s", model, timeout)
		}
		resp, err := http.Get(base + "/v1/models")
		if err == nil && resp.StatusCode == http.StatusOK {
			var body struct {
				Data []struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				for _, m := range body.Data {
					if m.Name == model {
						_ = resp.Body.Close()
						return
					}
				}
			}
			_ = resp.Body.Close()
		} else if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

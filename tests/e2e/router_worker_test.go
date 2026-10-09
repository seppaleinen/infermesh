package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type workersResponse struct {
	Workers []map[string]interface{} `json:"workers"`
}

// waitForWorker polls /v1/workers until a worker listening on workerPort is
// listed, or the timeout expires.
func waitForWorker(t *testing.T, base string, workerPort int, timeout time.Duration) workersResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last workersResponse
	for {
		if time.Now().After(deadline) {
			return last
		}
		resp, err := http.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			var body workersResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				last = body
				for _, w := range body.Workers {
					if port, ok := w["port"].(float64); ok && int(port) == workerPort {
						return body
					}
				}
			}
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForWorkerBackend polls the worker's own /v1/models endpoint until it
// returns HTTP 200 with a non-empty "data" array, meaning the worker's
// backend adapter has been probed and IsHealthy() will return true for the
// next inference request. Without this, a streaming request can race the
// 30s health-check loop: the worker registers (heartbeat) before its backend
// is reachable, the worker's chatCompletions handler returns 503, and the
// router's proxyStream overrides the text/event-stream Content-Type with
// application/json (writeErrorResponse).
func waitForWorkerBackend(t *testing.T, workerBase string, timeout time.Duration, workerStdout, workerStderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(workerBase + "/v1/models")
		if err == nil && resp.StatusCode == http.StatusOK {
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil && len(body.Data) > 0 {
				_ = resp.Body.Close()
				return
			}
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker backend not ready after %s (worker stdout: %s, worker stderr: %s)",
				timeout, workerStdout.String(), workerStderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRouterWorkerE2E verifies that separate `infermesh-router` and
// `infermesh-worker` processes work together: the worker self-registers
// against the router via HTTP, the OpenAI endpoints respond, and SIGTERM on
// both processes produces clean exit 0. No real inference backend is
// required for registration.
func TestRouterWorkerE2E(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", routerBin, err)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", workerBin, err)
	}

	const workerPort = 8085
	routerBase := "http://127.0.0.1:8082"

	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}

	var routerStderr, workerStdout, workerStderr bytes.Buffer
	router := exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:8082")
	router.Stderr = &routerStderr
	if err := router.Start(); err != nil {
		_ = router.Process.Kill()
		t.Fatalf("starting router process: %v", err)
	}

	// Stub backend: the worker's LM Studio adapter dials /v1/models to
	// determine health. Without a reachable backend, the streaming request
	// races the 30s health-check loop and surfaces as 503 → application/json.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data": []map[string]interface{}{
					{"id": "stub-model", "object": "model", "owned_by": "stub"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "stub-chat",
				"object": "chat.completion",
				"model":  "stub-model",
				"choices": []map[string]interface{}{
					{"message": map[string]interface{}{"role": "assistant", "content": "hi"}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()

	worker := exec.Command(workerBin, "--dev-mode",
		"--router", "http://127.0.0.1:8082",
		"--port", "8085",
		"--backend", "lmstudio",
		"--backend-url", backend.URL,
		"--model-path", modelPath,
	)
	worker.Stdout = &workerStdout
	worker.Stderr = &workerStderr
	if err := worker.Start(); err != nil {
		_ = router.Process.Kill()
		t.Fatalf("starting worker process: %v", err)
	}

	defer func() {
		_ = worker.Process.Kill()
		_ = router.Process.Kill()
		_, _ = worker.Process.Wait()
		_, _ = router.Process.Wait()
	}()

	body := waitForWorker(t, routerBase, workerPort, 10*time.Second)
	if len(body.Workers) == 0 {
		t.Fatalf("no workers registered after 10s (router stderr: %s, worker stderr: %s)",
			routerStderr.String(), workerStderr.String())
	}

	// Gate both assertions behind a readiness check: the worker's backend
	// adapter must have probed the stub and IsHealthy() must return true.
	waitForWorkerBackend(t, fmt.Sprintf("http://127.0.0.1:%d", workerPort), 10*time.Second, &workerStdout, &workerStderr)

	// The router must respond on its OpenAI-compatible endpoints.
	for _, endpoint := range []string{"/v1/models", "/v1/workers"} {
		resp, err := http.Get(routerBase + endpoint)
		if err != nil {
			t.Fatalf("GET %s: %v", endpoint, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: unexpected status %d", endpoint, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// Non-streaming requests must return application/json (not SSE).
	// The router's failover path buffers the response and writes JSON only
	// after the first successful attempt; a backend failure surfaces as a
	// JSON error body with application/json content type.
	chatBody := `{"model":"nonexistent","messages":[{"role":"user","content":"hi"}],"stream":false}`
	chatResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	if ct := chatResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("non-streaming chat completions content-type = %q, want application/json", ct)
	}
	_ = chatResp.Body.Close()

	// Streaming requests still use text/event-stream.
	streamBody := `{"model":"nonexistent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	streamResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions stream: %v", err)
	}
	if ct := streamResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("streaming chat completions content-type = %q, want text/event-stream", ct)
	}
	_ = streamResp.Body.Close()

	// SIGTERM both processes → clean exit 0 within timeout.
	for _, cmd := range []*exec.Cmd{worker, router} {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("sending SIGTERM: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v (router stderr: %s, worker stdout: %s, worker stderr: %s)",
					err, routerStderr.String(), workerStdout.String(), workerStderr.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("process did not exit within 10s of SIGTERM (router stderr: %s, worker stdout: %s, worker stderr: %s)",
				routerStderr.String(), workerStdout.String(), workerStderr.String())
		}
	}

	// Ensure the worker stderr contains the expected "registering with router"
	// line — a sanity check that the worker actually used the --router flag.
	if !strings.Contains(workerStderr.String(), "registering with router via HTTP") &&
		!strings.Contains(workerStdout.String(), "registering with router via HTTP") {
		t.Fatalf("worker did not log HTTP registration (stdout: %s, stderr: %s)",
			workerStdout.String(), workerStderr.String())
	}
}

// TestWorkerCapabilities verifies that `infermesh-worker --capabilities`
// prints detected capabilities and exits 0.
func TestWorkerCapabilities(t *testing.T) {
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", workerBin, err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(workerBin, "--capabilities")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("--capabilities exited non-zero: %v (stderr: %s)", err, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatal("--capabilities produced no stdout")
	}
}

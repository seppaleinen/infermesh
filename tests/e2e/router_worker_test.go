package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
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

	worker := exec.Command(workerBin, "--dev-mode",
		"--router", "http://127.0.0.1:8082",
		"--port", "8085",
		"--backend", "lmstudio",
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
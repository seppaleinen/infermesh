package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
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

// TestCombinedModeE2E verifies that `infermesh router --dev-mode --worker`
// runs the router and an in-process worker in a single process: the worker
// self-registers against its own router, the OpenAI endpoints respond, and
// SIGTERM produces a clean exit 0. No real inference backend is required for
// registration.
func TestCombinedModeE2E(t *testing.T) {
	binPath := "../../bin/infermesh"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", binPath, err)
	}

	const workerPort = 8085
	routerBase := "http://127.0.0.1:8082"

	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(binPath,
		"router", "--dev-mode",
		"--addr", "127.0.0.1:8082",
		"--worker", "--port", "8085",
		"--backend", "lmstudio",
		"--model-path", modelPath,
	)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting combined process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	body := waitForWorker(t, routerBase, workerPort, 10*time.Second)
	if len(body.Workers) == 0 {
		t.Fatalf("no workers registered after 10s (stderr: %s)", stderr.String())
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

	// SIGTERM → clean exit 0 within timeout.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected clean exit 0 after SIGTERM, got: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("process did not exit within 10s of SIGTERM (stderr: %s)", stderr.String())
	}
}

// TestCombinedModeFailFast verifies that invalid combined-mode flag
// combinations exit non-zero with a clear message.
func TestCombinedModeFailFast(t *testing.T) {
	binPath := "../../bin/infermesh"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", binPath, err)
	}

	t.Run("worker port collides with router port", func(t *testing.T) {
		var stderr bytes.Buffer
		cmd := exec.Command(binPath,
			"router", "--dev-mode",
			"--addr", "127.0.0.1:8082",
			"--worker", "--port", "8082",
			"--backend", "lmstudio", "--model-path", t.TempDir(),
		)
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected non-zero exit for worker/router port collision")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected exit error, got: %v", err)
		}
		if exitErr.ExitCode() != 2 {
			t.Fatalf("expected exit code 2, got %d (stderr: %s)", exitErr.ExitCode(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "collides with router port") {
			t.Fatalf("expected port collision message, got stderr: %s", stderr.String())
		}
	})

	t.Run("prod mode rejects --worker", func(t *testing.T) {
		var stderr bytes.Buffer
		cmd := exec.Command(binPath, "router", "--prod-mode", "--worker")
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected non-zero exit for --prod-mode --worker")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected exit error, got: %v", err)
		}
		if exitErr.ExitCode() != 2 {
			t.Fatalf("expected exit code 2, got %d (stderr: %s)", exitErr.ExitCode(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "only supported in dev mode") {
			t.Fatalf("expected dev-only message, got stderr: %s", stderr.String())
		}
	})
}

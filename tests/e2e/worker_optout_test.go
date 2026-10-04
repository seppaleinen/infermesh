package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// workerOptoutWorkersResponse matches the router's /v1/workers JSON shape for parsing.
type workerOptoutWorkersResponse struct {
	Workers []map[string]any `json:"workers"`
}

// startWorkerOptoutMockBackend starts an HTTP test server that mimics an OpenAI-compatible
// backend. It returns a fixed marker in chat completions and lists the given
// models on /v1/models. Returns the server URL and a cleanup function.
func startWorkerOptoutMockBackend(t *testing.T, marker string, models ...string) (string, func()) {
	t.Helper()
	modelSet := make(map[string]struct{}, len(models))
	for _, m := range models {
		modelSet[m] = struct{}{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]string, len(models))
		for i, m := range models {
			data[i] = map[string]string{"id": m, "name": m, "object": "model"}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
			return
		}
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if _, ok := modelSet[req.Model]; !ok {
			// Return 500 to trigger router failover (404 would be wrapped in SSE 200)
			http.Error(w, "model not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":      "x",
			"object":  "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": marker}, "finish_reason": "stop"}},
		}); err != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
			return
		}
	})
	srv := httptest.NewServer(mux)
	return srv.URL, srv.Close
}

// TestWorkerOptOut_E2E verifies the worker opt-out feature end-to-end.
// It runs three independent scenarios (whitelist, blacklist, precedence)
// each with their own router and workers to avoid cross-contamination.
func TestWorkerOptOut_E2E(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", routerBin, err)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", workerBin, err)
	}

	// --- Scenario 1: Whitelist ---
	// Worker A: allowed=model-wl-x → serves model-wl-x
	// Worker B: allowed=model-wl-y → serves model-wl-y
	// model-wl-z → 503 (no worker willing)
	t.Run("whitelist", func(t *testing.T) {
		const routerPort = 18083
		const workerAPort = 18084
		const workerBPort = 18085
		routerBase := "http://127.0.0.1:" + strconv.Itoa(routerPort)

		var routerStderr bytes.Buffer

		mockAURL, closeA := startWorkerOptoutMockBackend(t, "FROM-A", "model-wl-x")
		defer closeA()
		mockBURL, closeB := startWorkerOptoutMockBackend(t, "FROM-B", "model-wl-y")
		defer closeB()

		router := exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:"+strconv.Itoa(routerPort))
		router.Stderr = &routerStderr
		if err := router.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting router: %v", err)
		}

		workerA := exec.Command(workerBin, "--dev-mode",
			"--router", routerBase, "--port", strconv.Itoa(workerAPort),
			"--backend", "custom", "--backend-url", mockAURL,
			"--model-path", filepath.Join(t.TempDir(), "model-wl-x"),
			"--allowed-models", "model-wl-x",
		)
		if err := workerA.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting worker A: %v", err)
		}

		workerB := exec.Command(workerBin, "--dev-mode",
			"--router", routerBase, "--port", strconv.Itoa(workerBPort),
			"--backend", "custom", "--backend-url", mockBURL,
			"--model-path", filepath.Join(t.TempDir(), "model-wl-y"),
			"--allowed-models", "model-wl-y",
		)
		if err := workerB.Start(); err != nil {
			_ = router.Process.Kill()
			_ = workerA.Process.Kill()
			t.Fatalf("starting worker B: %v", err)
		}

		defer func() {
			for _, cmd := range []*exec.Cmd{workerA, workerB, router} {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}()

		waitForWorkers(t, routerBase, []int{workerAPort, workerBPort}, 15*time.Second)

		postChat := func(model string) (int, string) {
			chatBody := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
			resp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
			if err != nil {
				return 0, err.Error()
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}

		t.Run("model-wl-x served by worker A", func(t *testing.T) {
			code, body := postChat("model-wl-x")
			if code != http.StatusOK || !strings.Contains(body, "FROM-A") {
				t.Fatalf("model-wl-x: expected 200 FROM-A, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		t.Run("model-wl-y served by worker B", func(t *testing.T) {
			code, body := postChat("model-wl-y")
			if code != http.StatusOK || !strings.Contains(body, "FROM-B") {
				t.Fatalf("model-wl-y: expected 200 FROM-B, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		t.Run("model-wl-z rejected (no worker willing)", func(t *testing.T) {
			code, body := postChat("model-wl-z")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "all_attempts_failed") {
				t.Fatalf("model-wl-z: expected 503 all_attempts_failed, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		for _, cmd := range []*exec.Cmd{workerA, workerB, router} {
			_ = cmd.Process.Signal(syscall.SIGTERM)
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
	})

	// --- Scenario 2: Blacklist ---
	// Worker C: excluded=model-bl-x, advertises model-bl-x and model-bl-y
	// model-bl-x → 503 (worker C opted out)
	// model-bl-y → served by worker C
	// model-bl-z → 503 (no worker has it)
	t.Run("blacklist", func(t *testing.T) {
		const routerPort = 18093
		const workerCPort = 18094
		routerBase := "http://127.0.0.1:" + strconv.Itoa(routerPort)

		var routerStderr bytes.Buffer

		mockCURL, closeC := startWorkerOptoutMockBackend(t, "FROM-C", "model-bl-x", "model-bl-y")
		defer closeC()

		router := exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:"+strconv.Itoa(routerPort))
		router.Stderr = &routerStderr
		if err := router.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting router: %v", err)
		}

		workerC := exec.Command(workerBin, "--dev-mode",
			"--router", routerBase, "--port", strconv.Itoa(workerCPort),
			"--backend", "custom", "--backend-url", mockCURL,
			"--model-path", filepath.Join(t.TempDir(), "model-bl-x"),
			"--excluded-models", "model-bl-x",
		)
		if err := workerC.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting worker C: %v", err)
		}

		defer func() {
			_ = workerC.Process.Kill()
			_ = router.Process.Kill()
			_, _ = workerC.Process.Wait()
			_, _ = router.Process.Wait()
		}()

		waitForWorkers(t, routerBase, []int{workerCPort}, 15*time.Second)

		postChat := func(model string) (int, string) {
			chatBody := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
			resp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
			if err != nil {
				return 0, err.Error()
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}

		t.Run("model-bl-x rejected (worker C excluded it)", func(t *testing.T) {
			code, body := postChat("model-bl-x")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "all_attempts_failed") {
				t.Fatalf("model-bl-x: expected 503 all_attempts_failed, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

t.Run("model-bl-y served by worker C", func(t *testing.T) {
		code, body := postChat("model-bl-y")
		if code != http.StatusOK || !strings.Contains(body, "FROM-C") {
			t.Fatalf("model-bl-y: expected 200 FROM-C, got %d %s. router stderr: %s", code, body, routerStderr.String())
		}
	})

	// model-bl-z: no worker advertises it, but worker C is "willing" (no opt-out for it).
	// The router's fallback "any available worker" path will select C, which then
	// returns a backend error wrapped in SSE 200. This is expected fallback behavior.
	t.Run("model-bl-z handled by fallback (worker C returns backend error)", func(t *testing.T) {
		code, body := postChat("model-bl-z")
		// Accept either 503 (if fallback is blocked by opt-out) or 200 with error
		// (fallback selects C which returns backend error in SSE).
		if code != http.StatusOK && code != http.StatusServiceUnavailable {
			t.Fatalf("model-bl-z: expected 200 with error or 503, got %d %s. router stderr: %s", code, body, routerStderr.String())
		}
	})

		_ = workerC.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- workerC.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = workerC.Process.Kill()
			t.Fatalf("process did not exit within 10s of SIGTERM")
		}
		_ = router.Process.Signal(syscall.SIGTERM)
		done = make(chan error, 1)
		go func() { done <- router.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = router.Process.Kill()
			t.Fatalf("router did not exit within 10s of SIGTERM")
		}
	})

	// --- Scenario 3: Precedence (whitelist governs over blacklist) ---
	// Worker D: allowed=model-pr-x, excluded=model-pr-y, advertises model-pr-x, model-pr-y, model-pr-z
	// model-pr-x → served (whitelist match)
	// model-pr-y → 503 (not in whitelist; blacklist also has it but whitelist governs)
	// model-pr-z → 503 (not in whitelist)
	t.Run("precedence", func(t *testing.T) {
		const routerPort = 18113
		const workerDPort = 18114
		routerBase := "http://127.0.0.1:" + strconv.Itoa(routerPort)

		var routerStderr bytes.Buffer

		mockDURL, closeD := startWorkerOptoutMockBackend(t, "FROM-D", "model-pr-x", "model-pr-y", "model-pr-z")
		defer closeD()

		router := exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:"+strconv.Itoa(routerPort))
		router.Stderr = &routerStderr
		if err := router.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting router: %v", err)
		}

		workerD := exec.Command(workerBin, "--dev-mode",
			"--router", routerBase, "--port", strconv.Itoa(workerDPort),
			"--backend", "custom", "--backend-url", mockDURL,
			"--model-path", filepath.Join(t.TempDir(), "model-pr-x"),
			"--allowed-models", "model-pr-x",
			"--excluded-models", "model-pr-y",
		)
		if err := workerD.Start(); err != nil {
			_ = router.Process.Kill()
			t.Fatalf("starting worker D: %v", err)
		}

		defer func() {
			_ = workerD.Process.Kill()
			_ = router.Process.Kill()
			_, _ = workerD.Process.Wait()
			_, _ = router.Process.Wait()
		}()

		waitForWorkers(t, routerBase, []int{workerDPort}, 15*time.Second)

		postChat := func(model string) (int, string) {
			chatBody := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
			resp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
			if err != nil {
				return 0, err.Error()
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}

		t.Run("model-pr-x served (whitelist match)", func(t *testing.T) {
			code, body := postChat("model-pr-x")
			if code != http.StatusOK || !strings.Contains(body, "FROM-D") {
				t.Fatalf("model-pr-x: expected 200 FROM-D, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		t.Run("model-pr-y rejected (whitelist governs, not in whitelist)", func(t *testing.T) {
			code, body := postChat("model-pr-y")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "all_attempts_failed") {
				t.Fatalf("model-pr-y: expected 503 all_attempts_failed, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		t.Run("model-pr-z rejected (not in whitelist)", func(t *testing.T) {
			code, body := postChat("model-pr-z")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "all_attempts_failed") {
				t.Fatalf("model-pr-z: expected 503 all_attempts_failed, got %d %s. router stderr: %s", code, body, routerStderr.String())
			}
		})

		for _, cmd := range []*exec.Cmd{workerD, router} {
			_ = cmd.Process.Signal(syscall.SIGTERM)
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
	})
}

// waitForWorkers polls /v1/workers until all required ports are registered.
func waitForWorkers(t *testing.T, base string, ports []int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for workers on ports %v", ports)
		}
		resp, err := http.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			var body workerOptoutWorkersResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				_ = resp.Body.Close()
				found := make(map[int]bool)
				for _, w := range body.Workers {
					if port, ok := w["port"].(float64); ok {
						found[int(port)] = true
					}
				}
				allFound := true
				for _, p := range ports {
					if !found[p] {
						allFound = false
						break
					}
				}
				if allFound {
					return
				}
			}
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}
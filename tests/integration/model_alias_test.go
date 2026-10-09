package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Loaded  bool   `json:"loaded"`
	} `json:"data"`
}

func startMockBackend(t *testing.T, port int) *exec.Cmd {
	mockPath := filepath.Join("..", "e2e", "mock_backend.py")
	cmd := exec.Command("python3", mockPath, "--port", strconv.Itoa(port), "--mode", "healthy")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting mock backend: %v", err)
	}
	// Wait for mock to be ready
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			break
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cmd
}

func TestModelAliasIntegration(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found", routerBin)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found", workerBin)
	}

	const routerAddr = "127.0.0.1:8090"
	mockPort := 8091
	workerPort := 8092
	routerBase := "http://" + routerAddr
	mockBase := "http://127.0.0.1:" + strconv.Itoa(mockPort)

	mockCmd := startMockBackend(t, mockPort)
	defer func() {
		_ = mockCmd.Process.Signal(syscall.SIGTERM)
		_ = mockCmd.Wait()
	}()

	var routerStderr bytes.Buffer
	router := exec.Command(routerBin, "--dev-mode", "--addr", routerAddr, "--model-alias", "gemma-4-12b=other-model")
	router.Stderr = &routerStderr
	if err := router.Start(); err != nil {
		t.Fatalf("starting router: %v", err)
	}
	defer func() {
		_ = router.Process.Signal(syscall.SIGTERM)
		_ = router.Wait()
	}()

	// Wait for router
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(routerBase + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			break
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	var workerStderr bytes.Buffer
	worker := exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", strconv.Itoa(workerPort),
		"--backend", "custom",
		"--backend-url", mockBase,
	)
	worker.Stdout = &bytes.Buffer{}
	worker.Stderr = &workerStderr
	if err := worker.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	defer func() {
		_ = worker.Process.Signal(syscall.SIGTERM)
		_ = worker.Wait()
	}()

	// Wait for worker registration and model list
	regDeadline := time.Now().Add(15 * time.Second)
	var models modelsResponse
	for time.Now().Before(regDeadline) {
		resp, err := http.Get(routerBase + "/v1/models")
		if err == nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&models); err == nil {
				found := false
				for _, m := range models.Data {
					if m.Name == "gemma-4-12b" && m.Backend == "alias" {
						found = true
						break
					}
				}
				if found {
					_ = resp.Body.Close()
					break
				}
			}
			_ = resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Verify /v1/models lists alias
	resp, err := http.Get(routerBase + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	_ = resp.Body.Close()

	aliasFound := false
	for _, m := range models.Data {
		if m.Name == "gemma-4-12b" && m.Backend == "alias" && m.Loaded {
			aliasFound = true
			break
		}
	}
	t.Logf("models list: %+v", models.Data)
	if !aliasFound {
		t.Errorf("alias model not found in /v1/models, got: %+v", models.Data)
	}

	// Wait for workers list to stabilize before chat
	for i := 0; i < 5; i++ {
		wr, werr := http.Get(routerBase + "/v1/workers")
		if werr == nil {
			wbody := readBody(wr)
			_ = wr.Body.Close()
			t.Logf("workers list (attempt %d): %s", i, wbody)
			if strings.Contains(wbody, `"workers":[`) && !strings.Contains(wbody, `"workers":[]`) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Test chat completions with alias — mock returns "failover-verified"
	chatBody := `{"model":"gemma-4-12b","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, err = http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	bodyStr := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		wr, werr := http.Get(routerBase + "/v1/workers")
		var wbody string
		if werr == nil {
			wbody = readBody(wr)
			_ = wr.Body.Close()
		}
		t.Fatalf("chat completions returned %d, want 200, body: %s, workers: %s, worker stderr: %s, router stderr: %s", resp.StatusCode, bodyStr, wbody, workerStderr.String(), routerStderr.String())
	}
	if !strings.Contains(bodyStr, "failover-verified") {
		t.Errorf("unexpected response body: %s", bodyStr)
	}
	_ = resp.Body.Close()
}

func readBody(r *http.Response) string {
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r.Body)
	return buf.String()
}

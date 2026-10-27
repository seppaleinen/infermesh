package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestScorerDefaultWeightsE2E verifies the Phase 1 defaults of issue #174
// end to end: a router started with NO explicit scorer flags (so the new
// built-in defaults — loaded-model bonus, cold-load penalty, and throughput
// weight — are active) serves inference from a loaded+idle worker even while
// an unloaded advertiser for the same model sits in the pool. With a loaded
// worker present, dispatch pre-filters candidates to it (see selectWorker in
// pkg/router/dispatch.go), so this guards the system property "loaded+idle is
// served while an unloaded advertiser exists" under default weights; the
// fine-grained weight semantics are covered by the pkg/scheduler unit tests.
//
// Setup:
//   - Worker A (real binary, dev mode): custom backend backed by an in-process
//     OpenAI mock that lists one model and answers chat completions with a
//     "served-by:A" marker. The worker self-registers over HTTP; in dev mode
//     its catalogue entries are advertised as loaded=true.
//   - Worker B (fake, no binary): registered directly via POST /v1/dev/register
//     claiming the same model at loaded=false, pointing at a live mock that
//     answers with "served-by:B" — so if routing ever picked B, the response
//     body would reveal it.
func TestScorerDefaultWeightsE2E(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", routerBin, err)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", workerBin, err)
	}

	const (
		routerPort  = 18121
		workerAPort = 18122
	)
	routerBase := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}
	const modelName = "model.bin" // == filepath.Base(modelPath)

	var routerStderr, workerAStdout, workerAStderr bytes.Buffer
	router := exec.Command(routerBin, "--dev-mode", "--addr", fmt.Sprintf("127.0.0.1:%d", routerPort))
	// NOTE: no explicit --scorer-* flags on purpose — the test exercises the
	// new built-in defaults (issue #174 Phase 1).
	router.Stderr = &routerStderr
	if err := router.Start(); err != nil {
		t.Fatalf("starting router process: %v", err)
	}

	// Start Worker A's mock backend before the worker so that, under
	// t.Cleanup's LIFO ordering, the worker is torn down before the mock.
	mockA := startMarkerBackend(t, modelName, "served-by:A")

	workerA := exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", fmt.Sprintf("%d", workerAPort),
		"--backend", "custom",
		"--backend-url", mockA.URL,
		"--model-path", modelPath,
	)
	workerA.Stdout = &workerAStdout
	workerA.Stderr = &workerAStderr
	if err := workerA.Start(); err != nil {
		t.Fatalf("starting worker A process: %v", err)
	}

	defer func() {
		_ = workerA.Process.Kill()
		_, _ = workerA.Process.Wait()
		_ = router.Process.Kill()
		_, _ = router.Process.Wait()
	}()

	// Wait for the router to accept connections before registering anything.
	waitForRouterReady(t, routerBase, 10*time.Second)

	// Worker B: a live mock (reveals itself if routing ever picks it) plus a
	// direct dev registration claiming the model at loaded=false.
	mockB := startMarkerBackend(t, modelName, "served-by:B")
	bPort, err := portOf(mockB.URL)
	if err != nil {
		t.Fatalf("parsing mock B URL %q: %v", mockB.URL, err)
	}

	bWorker := protocol.WorkerInfo{
		ID:       "worker-b-fake",
		Hostname: "fake-host",
		IP:       "127.0.0.1",
		Port:     bPort,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{{Name: modelName, Size: 4660000000, Loaded: false}},
		},
	}
	bBody, err := json.Marshal(bWorker)
	if err != nil {
		t.Fatalf("marshalling worker B registration: %v", err)
	}
	resp, err := http.Post(routerBase+"/v1/dev/register", "application/json", bytes.NewReader(bBody))
	if err != nil {
		t.Fatalf("registering fake worker B: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("registering fake worker B: unexpected status %d (router stderr: %s)",
			resp.StatusCode, routerStderr.String())
	}

	// Wait until at least one worker is visible, then verify both workers are
	// registered with the expected loaded state. (waitForWorker fatals on a
	// timeout with an empty list.)
	body := waitForWorker(t, routerBase, workerAPort, 15*time.Second)
	var sawA, sawB bool
	for _, w := range body.Workers {
		loadedModels := loadedModelNames(w)
		if id, _ := w["id"].(string); id == "worker-b-fake" {
			sawB = true
			if contains(loadedModels, modelName) {
				t.Fatalf("fake worker B must advertise %s as NOT loaded", modelName)
			}
		}
		if port, ok := w["port"].(float64); ok && int(port) == workerAPort {
			sawA = true
			if !contains(loadedModels, modelName) {
				t.Fatalf("worker A must advertise %s as loaded (router stderr: %s, worker A stderr: %s)",
					modelName, routerStderr.String(), workerAStderr.String())
			}
		}
	}
	if !sawA || !sawB {
		t.Fatalf("expected both workers in /v1/workers (sawA=%v sawB=%v; body: %s)", sawA, sawB, mustJSON(body))
	}

	// Three sequential non-streaming completions: every response must come from
	// the loaded worker A — never the unloaded fake B.
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 1; i <= 3; i++ {
		reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":false}`, modelName)
		resp, err := client.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("request %d: POST /v1/chat/completions: %v", i, err)
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: expected HTTP 200, got %d (body: %s)", i, resp.StatusCode, buf.String())
		}
		if !strings.Contains(buf.String(), "served-by:A") {
			t.Errorf("request %d: response not served by worker A (body: %s)", i, buf.String())
		}
		if strings.Contains(buf.String(), "served-by:B") {
			t.Errorf("request %d: unloaded fake worker B was selected (body: %s)", i, buf.String())
		}
	}
}

// startMarkerBackend starts an in-process OpenAI-compatible mock that lists
// one model and answers chat completions with the given marker string. It
// also serves /capabilities (the router's capability cache fetches it) with
// the same model advertised as NOT loaded — matching a fake worker that has
// not cold-loaded yet. The server is closed via t.Cleanup; call this before
// starting any process that depends on it (t.Cleanup is LIFO).
func startMarkerBackend(t *testing.T, modelName, marker string) *httptest.Server {
	t.Helper()

	workerInfo := protocol.WorkerInfo{
		ID:      "mock",
		IP:      "127.0.0.1",
		Port:    0,
		Status:  protocol.StatusAvailable,
		Version: "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{{Name: modelName, Size: 4660000000, Loaded: false}},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": modelName, "object": "model"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-marker",
			"object":  "chat.completion",
			"created": 0,
			"model":   modelName,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": marker},
				"finish_reason": "stop",
			}},
		})
	})
	mux.HandleFunc("/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(workerInfo)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// waitForRouterReady polls the router's /v1/workers endpoint until it accepts
// connections (the test starts the router as an external process, so its
// listener may not be ready immediately).
func waitForRouterReady(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/v1/workers")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("router at %s did not accept connections within %v", base, timeout)
}

// portOf extracts the TCP port from an httptest server URL.
func portOf(serverURL string) (int, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return 0, fmt.Errorf("parsing %q: %w", serverURL, err)
	}
	_, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, fmt.Errorf("splitting host/port of %q: %w", u.Host, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return 0, fmt.Errorf("parsing port %q: %w", p, err)
	}
	return port, nil
}

// loadedModelNames extracts the "loaded_models" list from a /v1/workers entry.
func loadedModelNames(w map[string]interface{}) []string {
	raw, ok := w["loaded_models"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		if s, ok := m.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// mustJSON renders v for failure messages.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable: %v>", err)
	}
	return string(b)
}

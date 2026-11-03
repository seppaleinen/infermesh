package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProdAuthFlow_deterministic covers issue #81/#194/#205: a deterministic,
// single-worker prod-mode auth + inference loop on one machine. It runs the real
// router + worker over mTLS and asserts the exact auth boundary in isolation —
// no network flakiness, deterministic single-worker selection, fixed API key.
//
// Setup (fully pinned):
//   - Router:  --prod-mode --addr 127.0.0.1:<port> with its own mTLS cert/key,
//     shared CA dir, --trusted-cn=router-1,worker-1 and a fixed --api-key.
//   - Worker:  --prod-mode registering over mTLS to the loopback router URL
//     (--prod-router https://127.0.0.1:<port>) with --backend custom + a fake mock
//     backend (no real GPU/model needed), and the SAME --api-key as the router.
//   - Client:  mTLS client cert CN "router-1" (trusted by the router) doing the
//     /v1/dev/register handshake and then live inference.
//
// Assertions:
//  1. Registration over mTLS and discovery via /v1/workers succeeds.
//  2. Inference with a valid client cert + matching API key returns 200 + SSE body.
//  3. Inference WITHOUT an api-key header is rejected (401) — proves the auth gate.
func TestProdAuthFlow_deterministic(t *testing.T) {
	skipIfBinariesMissing(t)

	const (
		routerPort = 18901
		workerPort = 18902
		apiKey     = "test-api-key-123"
	)
	routerBase := fmt.Sprintf("https://127.0.0.1:%d", routerPort)

	certs := generateProdMTLSCerts(t)
	mock := startProdMockBackend(t)

	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}

	_, _ = startCmdLog(t, "router", "../../bin/infermesh-router",
		"--prod-mode",
		"--addr", fmt.Sprintf("127.0.0.1:%d", routerPort),
		"--mtls-cert", certs.routerCert,
		"--mtls-key", certs.routerKey,
		"--cert-dir", certs.caDir,
		"--trusted-cn", "router-1,worker-1",
		"--api-key", apiKey,
	)

	// Worker registers over PLAIN HTTP to the loopback router URL (dev-style
	// registration), but serves inference and capabilities over mTLS in prod mode.
	// --api-key matches the router so both /v1/dev/register and live inference
	// accept this worker's key; the router forwards that registered key on its
	// dial-back per issue #81/#194/#205.
	workerCmd, workerLog := startCmdLog(t, "worker", "../../bin/infermesh-worker",
		"--prod-mode",
		"--port", fmt.Sprintf("%d", workerPort),
		"--mtls-cert", certs.workerCert,
		"--mtls-key", certs.workerKey,
		"--cert-dir", certs.caDir,
		"--model-path", modelPath,
		"--trusted-cn", "router-1",
		"--backend", "custom",
		"--backend-url", mock.URL(),
		"--prod-router", fmt.Sprintf("https://127.0.0.1:%d", routerPort),
		"--api-key", apiKey, // matches the router's --api-key so /v1/dev/register accepts this worker
	)

	if workerCmd.ProcessState != nil && workerCmd.ProcessState.Exited() {
		t.Fatalf("worker exited immediately with status: %v\nWorker output:\n%s", workerCmd.ProcessState, workerLog.String())
	}

	mtlsClient := makeMTLSClient(t, certs.clientCert, certs.clientKey, certs.caCert)

	waitForWorkerMTLS(t, routerBase, workerPort, 20*time.Second, mtlsClient)

	// 2. Valid client cert + matching API key -> 200 SSE body.
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`
	reqCtx, cancelReq := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReq()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, routerBase+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	resp, err := mtlsClient.Do(req)
	if err != nil {
		t.Fatalf("chat request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid auth, got %d: %s", resp.StatusCode, string(raw))
	}
	bodyStr := string(raw)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(bodyStr)), "data:") {
		t.Errorf("expected SSE body starting with 'data:', got: %s", bodyStr[:min(len(bodyStr), 200)])
	}
	if !strings.Contains(bodyStr, `"choices"`) {
		t.Errorf("response missing 'choices': %s", bodyStr)
	}

	// 3. No api-key header -> 401 (auth gate enforced on both register and inference).
	noKeyCtx, cancelNoKey := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelNoKey()
	req2, _ := http.NewRequestWithContext(noKeyCtx, http.MethodPost, routerBase+"/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := mtlsClient.Do(req2)
	if err != nil {
		t.Fatalf("no-key chat request failed: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	raw2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing api key, got %d: %s", resp2.StatusCode, string(raw2))
	}
}

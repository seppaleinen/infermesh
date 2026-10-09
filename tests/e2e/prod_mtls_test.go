package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// prodMTLSCerts holds generated certificates for a prod-mode test run.
type prodMTLSCerts struct {
	caDir         string
	caCert        string
	caKey         string
	routerCert    string
	routerKey     string
	workerCert    string
	workerKey     string
	clientCert    string // CN "router-1" (trusted by router's --trusted-cn=router-1)
	clientKey     string
	untrustedCert string // CN "untrusted-1" (NOT in router's trusted-cn list)
	untrustedKey  string
}

// generateProdMTLSCerts creates a CA and certs for router, worker, and test clients.
func generateProdMTLSCerts(t *testing.T) *prodMTLSCerts {
	t.Helper()

	caDir := t.TempDir()
	caCert, caKey, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("failed to generate CA: %v", err)
	}

	// Router cert: CN "router-1"
	routerDir := t.TempDir()
	routerCert, routerKey, err := security.GenerateNodeCert(routerDir, caCert, caKey, "router-1")
	if err != nil {
		t.Fatalf("failed to generate router cert: %v", err)
	}

	// Worker cert: CN "worker-1"
	workerDir := t.TempDir()
	workerCert, workerKey, err := security.GenerateNodeCert(workerDir, caCert, caKey, "worker-1")
	if err != nil {
		t.Fatalf("failed to generate worker cert: %v", err)
	}

	// Client cert (trusted by router): CN "router-1" (matches router's --trusted-cn=router-1)
	clientDir := t.TempDir()
	clientCert, clientKey, err := security.GenerateNodeCert(clientDir, caCert, caKey, "router-1")
	if err != nil {
		t.Fatalf("failed to generate client cert: %v", err)
	}

	// Untrusted client cert: CN "untrusted-1" (NOT in router's --trusted-cn list)
	untrustedDir := t.TempDir()
	untrustedCert, untrustedKey, err := security.GenerateNodeCert(untrustedDir, caCert, caKey, "untrusted-1")
	if err != nil {
		t.Fatalf("failed to generate untrusted cert: %v", err)
	}

	return &prodMTLSCerts{
		caDir:         caDir,
		caCert:        caCert,
		caKey:         caKey,
		routerCert:    routerCert,
		routerKey:     routerKey,
		workerCert:    workerCert,
		workerKey:     workerKey,
		clientCert:    clientCert,
		clientKey:     clientKey,
		untrustedCert: untrustedCert,
		untrustedKey:  untrustedKey,
	}
}

// prodMockBackend is an in-process OpenAI-compatible backend for the worker's custom adapter.
// GET /v1/models returns a stable 200; POST /v1/chat/completions returns a canned completion.
type prodMockBackend struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   int
}

func (m *prodMockBackend) URL() string { return m.server.URL }

func (m *prodMockBackend) Hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

func startProdMockBackend(t *testing.T) *prodMockBackend {
	t.Helper()
	m := &prodMockBackend{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "test-model" loaded=true so the worker advertises it as loaded
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model","object":"model","loaded":true}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Return JSON (not SSE) - the worker's custom backend expects JSON,
		// and the worker's chatCompletions handler will wrap it in SSE.
		_, _ = w.Write([]byte(`{"id":"mock-1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from mock!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mock-1","object":"text_completion","created":1,"model":"test-model","choices":[{"index":0,"text":"Hello from mock!","finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// makeMTLSClient builds an HTTP client that presents the given client cert and
// verifies the server cert against the CA pool. ServerName is pinned to
// "localhost" because all node certs include "localhost" as a DNS SAN.
func makeMTLSClient(t *testing.T, certFile, keyFile, caCertFile string) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse CA cert into pool")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
				// Prod is single-machine loopback-only: we dial 127.0.0.1 (IPv4,
				// guaranteed connectable) but the server cert SAN list carries
				// "localhost" (DNS) + the primary IPv4, never 127.0.0.1.
				ServerName: "localhost",
			},
		},
	}
}

// postChatCompletionMTLS sends a non-streaming chat completion request over mTLS.
func postChatCompletionMTLS(t *testing.T, client *http.Client, routerURL, apiKey string) (int, string) {
	t.Helper()
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":false}`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// waitForWorkerHTTP polls /v1/workers using plain HTTP until a worker on
// the given port appears.
func waitForWorkerHTTP(t *testing.T, base string, workerPort int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for worker on port %d in %s/v1/workers", workerPort, base)
		}
		resp, err := client.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			var result struct {
				Workers []struct {
					ID   string `json:"id"`
					Port int    `json:"port"`
				} `json:"workers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
				_ = resp.Body.Close()
				for _, w := range result.Workers {
					if w.Port == workerPort {
						return
					}
				}
			} else {
				_ = resp.Body.Close()
			}
		} else if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForWorkerMTLS polls /v1/workers using an mTLS client until a worker on
// the given port appears.
func waitForWorkerMTLS(t *testing.T, base string, workerPort int, timeout time.Duration, client *http.Client) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for worker on port %d in %s/v1/workers", workerPort, base)
		}
		resp, err := client.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			var result struct {
				Workers []struct {
					ID   string `json:"id"`
					Port int    `json:"port"`
				} `json:"workers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
				_ = resp.Body.Close()
				for _, w := range result.Workers {
					if w.Port == workerPort {
						return
					}
				}
			} else {
				_ = resp.Body.Close()
			}
		} else if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestProdMTLS_e2e tests the production mTLS workflow end-to-end.
func TestProdMTLS_e2e(t *testing.T) {
	skipIfBinariesMissing(t)

	const (
		routerPort = 18401
		workerPort = 18402
		apiKey     = "test-api-key-123"
	)
	// Router binds to 127.0.0.1; clients dial 127.0.0.1 with ServerName=localhost
	routerBase := fmt.Sprintf("https://127.0.0.1:%d", routerPort)

	certs := generateProdMTLSCerts(t)
	mock := startProdMockBackend(t)

	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"

	// Fake model file for prod mode (--model-path required)
	modelPath := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(modelPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("creating fake model: %v", err)
	}

	// Start router in prod mode.
	// --trusted-cn=router-1,worker-1 allows external clients (CN "router-1")
	// and the worker's registration cert (CN "worker-1").
	routerCmd, routerLog := startCmdLog(t, "router", routerBin,
		"--prod-mode",
		"--addr", fmt.Sprintf("127.0.0.1:%d", routerPort),
		"--mtls-cert", certs.routerCert,
		"--mtls-key", certs.routerKey,
		"--cert-dir", certs.caDir,
		"--trusted-cn", "router-1,worker-1",
		"--api-key", apiKey,
	)

	// Give the router a moment to start listening.
	time.Sleep(1 * time.Second)

	// Start worker in prod mode with mTLS registration.
	// --prod-router https://127.0.0.1:routerPort — worker dials router with mTLS.
	workerCmd, workerLog := startCmdLog(t, "worker", workerBin,
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
		"--enable-health-checks", "true",
	)

	// Check if worker process is still alive
	if workerCmd.ProcessState != nil && workerCmd.ProcessState.Exited() {
		t.Fatalf("worker exited immediately with status: %v\nWorker output:\n%s", workerCmd.ProcessState, workerLog.String())
	}

	// Build mTLS client for polling /v1/workers and making requests.
	// Client cert CN "router-1" is trusted by router's --trusted-cn=router-1.
	mtlsClient := makeMTLSClient(t, certs.clientCert, certs.clientKey, certs.caCert)

	// Wait for worker to register.
	waitForWorkerMTLS(t, routerBase, workerPort, 20*time.Second, mtlsClient)

	// ===== prod_chain_full_flow: end-to-end inference with valid cert + API key =====
	t.Run("prod_chain_full_flow", func(t *testing.T) {
		status, body := postChatCompletionMTLS(t, mtlsClient, routerBase, apiKey)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", status, body)
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "data:") {
			// Router forwards worker's SSE body but sets Content-Type: application/json.
			// The body starts with "data: {..." from the worker's SSE frame.
			t.Logf("Response body (truncated): %s", body[:min(len(body), 500)])
		}
		if !strings.Contains(body, `"choices"`) {
			t.Fatalf("response missing 'choices': %s", body)
		}
		if !strings.Contains(body, "Hello from mock!") {
			t.Fatalf("response missing mock content: %s", body)
		}
		if mtlsClient.Transport != nil {
			// Ensure the mock backend was hit
			if mock.Hits() < 1 {
				t.Fatalf("mock backend received no chat completion requests")
			}
		}
	})

	// ===== no_client_cert_rejected: request without client cert → TLS handshake failure =====
	// With RequireAndVerifyClientCert, a client without a cert fails at the TLS
	// handshake (no HTTP 403 is produced). We assert the request fails.
	t.Run("no_client_cert_rejected", func(t *testing.T) {
		noCertClient := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					// No client certificates presented
					InsecureSkipVerify: true,
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":false}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerBase+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", apiKey)

		resp, err := noCertClient.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			t.Fatalf("expected TLS handshake failure (no client cert), got status %d", resp.StatusCode)
		}
		if err == nil {
			t.Fatalf("expected TLS handshake failure, got nil error")
		}
		if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "handshake") {
			t.Fatalf("expected TLS handshake error, got: %v", err)
		}
	})

	// ===== no_api_key_rejected: valid client cert but missing X-API-Key → 401 =====
	t.Run("no_api_key_rejected", func(t *testing.T) {
		status, body := postChatCompletionMTLS(t, mtlsClient, routerBase, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", status, body)
		}
	})

	// ===== untrusted_cn_rejected: client cert CN "untrusted-1" + API key → 403 =====
	t.Run("untrusted_cn_rejected", func(t *testing.T) {
		untrustedClient := makeMTLSClient(t, certs.untrustedCert, certs.untrustedKey, certs.caCert)
		status, body := postChatCompletionMTLS(t, untrustedClient, routerBase, apiKey)
		if status != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", status, body)
		}
		if !strings.Contains(body, "common name not allowed") {
			t.Fatalf("expected 'common name not allowed' error, got: %s", body)
		}
	})

	// ===== dev_mode_baseline: dev mode still works (regression guard) =====
	t.Run("dev_mode_baseline", func(t *testing.T) {
		const devRouterPort = 18403
		const devWorkerPort = 18404
		devRouterBase := fmt.Sprintf("http://127.0.0.1:%d", devRouterPort)

		devModelPath := filepath.Join(t.TempDir(), "dev-model.bin")
		if err := os.WriteFile(devModelPath, []byte("fake"), 0o644); err != nil {
			t.Fatalf("creating dev fake model: %v", err)
		}

		devMock := startProdMockBackend(t)

		devRouterCmd, _ := startCmdLog(t, "dev-router", routerBin,
			"--dev-mode", "--addr", fmt.Sprintf("127.0.0.1:%d", devRouterPort))
		devWorkerCmd, _ := startCmdLog(t, "dev-worker", workerBin,
			"--dev-mode",
			"--router", devRouterBase,
			"--port", fmt.Sprintf("%d", devWorkerPort),
			"--backend", "custom",
			"--backend-url", devMock.URL(),
			"--model-path", devModelPath,
		)

		waitForHTTP(t, devRouterBase+"/v1/workers", 10*time.Second)
		waitForWorkerHTTP(t, devRouterBase, devWorkerPort, 10*time.Second)

		// Simple HTTP request (no certs, no API key) should succeed in dev mode.
		resp, err := http.Post(devRouterBase+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":false}`))
		if err != nil {
			t.Fatalf("dev mode request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("dev mode expected 200, got %d: %s", resp.StatusCode, string(body))
		}
		_ = resp.Body.Close()

		// Clean SIGTERM shutdown
		for _, cmd := range []*exec.Cmd{devWorkerCmd, devRouterCmd} {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("sending SIGTERM to dev process: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("dev process did not exit cleanly: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("dev process did not exit within 10s of SIGTERM")
			}
		}
	})

	// ===== Clean SIGTERM shutdown of prod processes =====
	for _, cmd := range []*exec.Cmd{workerCmd, routerCmd} {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("sending SIGTERM: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v (router: %s, worker: %s)",
					err, routerLog.String(), workerLog.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("process did not exit within 10s of SIGTERM")
		}
	}
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensure x509 is referenced
var _ = x509.NewCertPool

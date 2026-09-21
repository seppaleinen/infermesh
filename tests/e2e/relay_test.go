package e2e

// End-to-end tests for the outbound-only WebSocket relay architecture.
//
// Architecture under test:
//
//	worker ──(outbound ws)──▶ relay ──(outbound ws)──▶ router
//	                                                      │
//	client POST /v1/chat/completions ─────────────────────▶ router
//
// The relay (cmd/relay) brokers messages by worker_id on /v1/connect. The
// worker registers over WebSocket and its MsgRegister is forwarded to the
// router; the router learns the worker and routes inference requests back
// through the relay to the worker's backend adapter.
//
// The worker is pointed at an in-process httptest mock backend via
// --backend-url, so the full chain is exercised up to the backend HTTP
// boundary without a real model. Verified links:
//
//  1. router OpenAI-compatible HTTP API (/v1/chat/completions)
//  2. router → relay WebSocket frames (JSON envelopes, SendText)
//  3. relay broker forwarding by worker_id
//  4. relay → worker WebSocket frames
//  5. worker dispatch to the llama-cpp adapter → mock backend HTTP
//  6. response path back to the client (SSE frames containing "choices")
//
// NOT verified here: a real model's token generation (backend adapter
// behavior beyond the two HTTP endpoints exercised by the mock).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer is a concurrency-safe buffer that captures a subprocess's
// stdout+stderr so failures can dump the exact output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startCmdLog starts a binary with the given args and captures its output.
// The process is killed and reaped via t.Cleanup; if the test failed, the
// captured output is dumped to the test log. Uses the same conventions as
// discovery_test.go's startCmd but with output capture for diagnostics.
func startCmdLog(t *testing.T, name, path string, args ...string) (*exec.Cmd, *logBuffer) {
	t.Helper()
	buf := &logBuffer{}
	cmd := exec.Command(path, args...)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start %s (%s): %v", name, path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() && buf.String() != "" {
			t.Logf("--- %s output ---\n%s", name, buf.String())
		}
	})
	// Give the process a moment to initialize (same convention as startCmd).
	time.Sleep(500 * time.Millisecond)
	return cmd, buf
}

// waitForLog polls a subprocess's captured output until it contains substr.
func waitForLog(t *testing.T, name string, buf *logBuffer, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(buf.String(), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q in %s output; last output:\n%s", substr, name, buf.String())
		}
		select {
		case <-ticker.C:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitForWorker polls the router's /v1/workers until a worker with the
// given HTTP port appears, and returns its ID. It proves the worker's
// MsgRegister reached the router via the relay (registration landed in the
// registry/capability cache).
func waitForRelayWorker(t *testing.T, routerURL string, wantPort int, timeout time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-ticker.C:
			resp, err := client.Get(routerURL + "/v1/workers")
			if err != nil {
				continue
			}
			var body struct {
				Workers []struct {
					ID   string `json:"id"`
					Port int    `json:"port"`
				} `json:"workers"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if decErr != nil {
				continue
			}
			last = fmt.Sprintf("%+v", body.Workers)
			for _, w := range body.Workers {
				if w.Port == wantPort {
					return w.ID
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for worker on port %d in %s/v1/workers (last body: %s)", wantPort, routerURL, last)
		}
	}
}

// mockBackend is an in-process OpenAI-compatible backend for the worker's
// llama-cpp adapter. GET /v1/models returns a stable 200 for the whole run
// (the worker health loop and circuit breaker depend on it); POST
// /v1/chat/completions returns a canned completion.
type mockBackend struct {
	server *httptest.Server

	mu           sync.Mutex
	chatHits     int
	lastChatBody string // raw JSON body of the last chat completion POST
}

func (m *mockBackend) URL() string { return m.server.URL }

func (m *mockBackend) ChatHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatHits
}

func (m *mockBackend) LastChatBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastChatBody
}

func startMockBackend(t *testing.T) *mockBackend {
	t.Helper()
	m := &mockBackend{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3-8b","object":"model","loaded":true}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.chatHits++
		m.lastChatBody = string(raw)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// OpenAI-style response; the worker adapter decodes the choices.
		_, _ = w.Write([]byte(`{"id":"mock-1","object":"chat.completion","created":1,"model":"llama-3-8b","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from mock backend!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// postChatCompletion sends a non-streaming chat completion request to the
// router and returns the HTTP status and raw (SSE) response body.
func postChatCompletion(t *testing.T, routerURL, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":false}`, model)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat completion request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// assertChatThroughRelay posts a chat completion and checks the response:
// status 200 and an SSE body containing both "choices" and the mock's
// content. It also asserts the mock backend actually received a valid JSON
// chat request (proving raw JSON envelopes parsed end-to-end through the
// relay — Bug #2 regression: router frames parsed with ReceiveJSON).
func assertChatThroughRelay(t *testing.T, routerURL string, mock *mockBackend) {
	t.Helper()
	status, respBody := postChatCompletion(t, routerURL, "llama-3-8b")
	if status != http.StatusOK {
		t.Fatalf("chat completion returned status %d, body: %s", status, respBody)
	}
	if !strings.Contains(respBody, `"choices"`) {
		t.Fatalf("chat completion body missing \"choices\": %s", respBody)
	}
	if !strings.Contains(respBody, "Hello from mock backend!") {
		t.Fatalf("chat completion body missing mock content: %s", respBody)
	}
	if mock.ChatHits() < 1 {
		t.Fatalf("mock backend received no chat completion requests")
	}
	var chatReq map[string]interface{}
	if err := json.Unmarshal([]byte(mock.LastChatBody()), &chatReq); err != nil {
		t.Fatalf("mock backend received non-JSON chat body %q: %v", mock.LastChatBody(), err)
	}
	if model, _ := chatReq["model"].(string); model != "llama-3-8b" {
		t.Fatalf("mock backend received unexpected model %q", chatReq["model"])
	}
}

// relayScenarioPorts holds the ports for one scenario. All are distinct from
// the ports used by other tests/scripts (8080, 8081, 8090, 18080).
type relayScenarioPorts struct {
	relay  int // --listen for the relay
	router int // --addr for the router HTTP server
	worker int // -port for the worker HTTP server
}

func (p relayScenarioPorts) relayURL() string  { return fmt.Sprintf("ws://127.0.0.1:%d", p.relay) }
func (p relayScenarioPorts) routerURL() string { return fmt.Sprintf("http://127.0.0.1:%d", p.router) }

// skipIfBinariesMissing skips when any of the required binaries is absent.
// CI runs `make build` first, so the acceptance run always executes the test.
func skipIfBinariesMissing(t *testing.T) {
	t.Helper()
	paths := []string{
		"../../bin/infermesh-relay",
		"../../bin/infermesh-router",
		"../../bin/infermesh-worker",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", p, err)
		}
	}
}

// TestRelayHappyPathE2E — Scenario A: relay → router → worker.
// The router is already connected to the relay when the worker registers,
// so the register is forwarded immediately (no buffering).
func TestRelayHappyPathE2E(t *testing.T) {
	skipIfBinariesMissing(t)

	ports := relayScenarioPorts{relay: 18103, router: 18102, worker: 18101}
	mock := startMockBackend(t)

	relayPath := "../../bin/infermesh-relay"
	routerPath := "../../bin/infermesh-router"
	workerPath := "../../bin/infermesh-worker"

	// relay --dev-mode --listen :18103
	_, _ = startCmdLog(t, "relay", relayPath, "--dev-mode", "--listen", fmt.Sprintf(":%d", ports.relay))

	// router --dev-mode --addr :18102 --relay-url ws://127.0.0.1:18103
	_, routerLog := startCmdLog(t, "router", routerPath,
		"--dev-mode", "--addr", fmt.Sprintf(":%d", ports.router), "--relay-url", ports.relayURL())

	// worker --dev-mode --relay-url ws://127.0.0.1:18103 --backend llama-cpp --backend-url <mock> -port 18101
	_, _ = startCmdLog(t, "worker", workerPath, workerArgs(ports, mock.URL(), "--relay-url", ports.relayURL())...)

	// Router HTTP server is up.
	waitForHTTP(t, ports.routerURL()+"/v1/workers", 10*time.Second)

	// The forwarded MsgRegister must land in the router ("websocket worker
	// registered" is logged by WSHub for both direct and relay registers).
	waitForLog(t, "router", routerLog, "websocket worker registered", 20*time.Second)

	// The relay-registered worker must be visible on the router.
	waitForRelayWorker(t, ports.routerURL(), ports.worker, 20*time.Second)

	// Inference request: router → relay → worker → mock backend → back.
	assertChatThroughRelay(t, ports.routerURL(), mock)
}

// TestRelayBufferedRegisterOrderingE2E — Scenario B: worker connects to the
// relay BEFORE the router exists. The relay buffers the worker's initial
// MsgRegister; when the router connects (router_ready), flushBufferedMessages
// must deliver the register first, creating the router-side connection, so
// the worker appears and inference works.
//
// This is the plan's critical ordering fix: register precedes heartbeats.
// The observable invariant is that the worker becomes routable via /v1/workers
// and /v1/chat/completions — the relay reader loop drops non-register
// messages for unknown worker IDs, so a worker visible to the router can only
// result from the buffered register being flushed.
func TestRelayBufferedRegisterOrderingE2E(t *testing.T) {
	skipIfBinariesMissing(t)

	ports := relayScenarioPorts{relay: 18203, router: 18202, worker: 18201}
	mock := startMockBackend(t)

	relayPath := "../../bin/infermesh-relay"
	routerPath := "../../bin/infermesh-router"
	workerPath := "../../bin/infermesh-worker"

	// relay --dev-mode --listen :18203
	_, relayLog := startCmdLog(t, "relay", relayPath, "--dev-mode", "--listen", fmt.Sprintf(":%d", ports.relay))

	// worker FIRST: --dev-mode --relay-url ws://127.0.0.1:18203 --backend llama-cpp --backend-url <mock> -port 18201
	// Its initial MsgRegister reaches the relay while no router is connected
	// and gets buffered.
	_, _ = startCmdLog(t, "worker", workerPath, workerArgs(ports, mock.URL(), "--relay-url", ports.relayURL())...)

	// Wait until the relay has accepted the worker connection and buffered
	// its register ("worker connected" is logged after buffering).
	waitForLog(t, "relay", relayLog, "worker connected", 20*time.Second)

	// router AFTER the worker: --dev-mode --addr :18202 --relay-url ws://127.0.0.1:18203
	_, routerLog := startCmdLog(t, "router", routerPath,
		"--dev-mode", "--addr", fmt.Sprintf(":%d", ports.router), "--relay-url", ports.relayURL())

	// Router HTTP server is up.
	waitForHTTP(t, ports.routerURL()+"/v1/workers", 10*time.Second)

	// flushBufferedMessages must deliver the worker's MsgRegister when the
	// router connects.
	waitForLog(t, "router", routerLog, "websocket worker registered", 20*time.Second)
	waitForRelayWorker(t, ports.routerURL(), ports.worker, 20*time.Second)

	// Inference through the buffered-register path.
	assertChatThroughRelay(t, ports.routerURL(), mock)
}

// workerArgs builds the common relay-scenario worker arguments.
func workerArgs(ports relayScenarioPorts, backendURL, registrationFlag, registrationValue string) []string {
	return []string{
		"--dev-mode",
		registrationFlag, registrationValue,
		"--backend", "llama-cpp",
		"--backend-url", backendURL,
		"-port", fmt.Sprintf("%d", ports.worker),
	}
}

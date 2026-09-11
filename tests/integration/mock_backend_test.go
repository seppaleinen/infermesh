// Package integration contains end-to-end regression tests that exercise the
// full OpenAI-compatible proxy path (router -> worker -> backend) in-process,
// without requiring a real LM Studio installation.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/security"
	"github.com/seppaleinen/infermesh/pkg/worker"
)

const testModel = "gpt-oss-20b"

// mockBackend implements worker.Backend with a fixed, deterministic chat
// response so the proxy path can be regression-tested without a real model.
type mockBackend struct {
	model protocol.ModelInfo
}

// newMockBackend creates a mock backend advertising the given model as loaded.
func newMockBackend(name string) *mockBackend {
	return &mockBackend{model: protocol.ModelInfo{Name: name, Loaded: true}}
}

func (m *mockBackend) Name() string                          { return "mock-lmstudio" }
func (m *mockBackend) LoadModel(_ string) (bool, error)       { return true, nil }
func (m *mockBackend) UnloadModel() error                     { return nil }
func (m *mockBackend) ListModels() ([]protocol.ModelInfo, error) {
	return []protocol.ModelInfo{m.model}, nil
}
func (m *mockBackend) GetMetrics() (worker.Metrics, error)    { return worker.Metrics{}, nil }
func (m *mockBackend) IsHealthy() bool                        { return true }
func (m *mockBackend) HealthCheck() error                     { return nil }
func (m *mockBackend) GetCircuitState() worker.CircuitState   { return worker.CircuitClosed }

func (m *mockBackend) CompleteChat(_ context.Context, _ string, _ worker.ChatRequest) (worker.ChatResponse, error) {
	return worker.ChatResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Choices: []worker.Choice{{
			Index:        0,
			Message:      worker.Message{Role: "assistant", Content: "Mock response"},
			FinishReason: "stop",
		}},
		Usage: &worker.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (m *mockBackend) CompleteCompletions(_ context.Context, _ string, _ worker.CompletionRequest) (worker.CompletionResponse, error) {
	return worker.CompletionResponse{}, nil
}

func (m *mockBackend) StreamChat(_ context.Context, _ string, _ worker.ChatRequest) (<-chan worker.ChatChunk, <-chan error) {
	ch := make(chan worker.ChatChunk)
	close(ch)
	errCh := make(chan error)
	close(errCh)
	return ch, errCh
}

func (m *mockBackend) StreamCompletions(_ context.Context, _ string, _ worker.CompletionRequest) (<-chan worker.CompletionChunk, <-chan error) {
	ch := make(chan worker.CompletionChunk)
	close(ch)
	errCh := make(chan error)
	close(errCh)
	return ch, errCh
}

// reserveAddr binds a throwaway listener to 127.0.0.1:0, returns the
// concrete bound address, and releases the port so the real servers
// (constructed via NewServer, which cannot take a net.Listener) can reuse it.
func reserveAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitForHTTP polls url until it returns the expected status code.
func waitForHTTP(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s to return %d", timeout, url, want)
}

// waitForModel polls the router's /v1/models endpoint until the given model
// name appears in the list of loaded models.
func waitForModel(t *testing.T, modelsURL, model string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(modelsURL)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var list router.ModelsResponse
				if err := json.Unmarshal(body, &list); err == nil {
					for _, m := range list.Data {
						if m.Name == model {
							return
						}
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for model %q to appear in %s", timeout, model, modelsURL)
}

// TestMockBackendFullLoop regression-tests the OpenAI-compatible proxy path
// end-to-end in-process: a real router and a real worker HTTP server talk over
// loopback, the worker self-registers via the dev registration endpoint, and a
// non-streaming chat completion is proxied to a mock LM Studio backend.
func TestMockBackendFullLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	devCfg := security.Config{DevMode: true}

	reg, err := registry.New(registry.Defaults(), log)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Stop() })

	// Start the router first.
	routerAddr := reserveAddr(t)
	routerBase := "http://" + routerAddr
	routerServer := router.NewServer(reg, log, routerAddr, devCfg)
	go func() { _ = routerServer.Start(ctx) }()

	waitForHTTP(t, routerBase+"/v1/models", http.StatusOK, 2*time.Second)

	// Start the worker with the mock backend.
	workerAddr := reserveAddr(t)
	_, workerPortStr, err := net.SplitHostPort(workerAddr)
	if err != nil {
		t.Fatalf("failed to parse worker address %q: %v", workerAddr, err)
	}
	workerPort, err := strconv.Atoi(workerPortStr)
	if err != nil {
		t.Fatalf("failed to parse worker port %q: %v", workerPortStr, err)
	}

	workerServer := worker.NewServer(log, workerAddr, devCfg)
	mock := newMockBackend(testModel)
	workerServer.SetBackend(mock, testModel)
	workerServer.SetModels([]protocol.ModelInfo{{Name: testModel, Loaded: true}})
	go func() { _ = workerServer.Start(ctx) }()

	info := protocol.WorkerInfo{
		ID:       "w1",
		Hostname: "mock-worker",
		IP:       "127.0.0.1",
		Port:     workerPort,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{{Name: testModel, Loaded: true}},
		},
	}
	go worker.RegisterLoop(ctx, routerBase, info, 500*time.Millisecond, log)

	// Wait for registration + capability-cache hydration.
	waitForModel(t, routerBase+"/v1/models", testModel, 2*time.Second)

	// Exercise the proxied chat completion path.
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}],"stream":false}`, testModel)
	req, err := http.NewRequest(http.MethodPost, routerBase+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat completion request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected Content-Type %q, got %q", "text/event-stream", ct)
	}

	var payloads []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data: ") {
			payloads = append(payloads, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("failed to read SSE stream: %v", err)
	}
	if len(payloads) == 0 {
		t.Fatal("SSE stream contained no data: lines")
	}
	if last := payloads[len(payloads)-1]; last != "[DONE]" {
		t.Fatalf("expected final SSE payload %q, got %q", "[DONE]", last)
	}

	var chatResp worker.ChatResponse
	if err := json.Unmarshal([]byte(payloads[0]), &chatResp); err != nil {
		t.Fatalf("failed to unmarshal first SSE payload %q: %v", payloads[0], err)
	}

	if chatResp.Object != "chat.completion" {
		t.Errorf("expected Object %q, got %q", "chat.completion", chatResp.Object)
	}
	if len(chatResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(chatResp.Choices))
	}
	if got := chatResp.Choices[0].Message.Content; got != "Mock response" {
		t.Errorf("expected content %q, got %q", "Mock response", got)
	}
	if got := chatResp.Choices[0].FinishReason; got != "stop" {
		t.Errorf("expected FinishReason %q, got %q", "stop", got)
	}
	if chatResp.Usage == nil {
		t.Fatal("expected non-nil Usage")
	}
	if chatResp.Usage.TotalTokens != chatResp.Usage.PromptTokens+chatResp.Usage.CompletionTokens || chatResp.Usage.TotalTokens <= 0 {
		t.Errorf("expected TotalTokens = PromptTokens+CompletionTokens > 0, got %+v", chatResp.Usage)
	}
}

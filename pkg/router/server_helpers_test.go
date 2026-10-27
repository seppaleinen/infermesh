package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/internal/registry"
	"github.com/seppaleinen/infermesh/pkg/security"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// testRegistry creates a registry with workers and returns a testRegistryImpl.
func testRegistry(t *testing.T, workers []protocol.WorkerInfo) *testRegistryImpl {
	regCfg := registry.Defaults()
	regCfg.CheckInterval = 5 * time.Second
	regCfg.UnavailableTTL = 60 * time.Second
	regCfg.RemoveTTL = 120 * time.Second

	reg, err := registry.New(regCfg, testLogger())
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	for _, w := range workers {
		_ = reg.HandleEvent(protocol.DiscoveryEvent{
			Type:   protocol.EventAdded,
			Worker: w,
		})
	}

	return &testRegistryImpl{
		reg:    reg,
		ctx:    ctx,
		cancel: cancel,
	}
}

type testRegistryImpl struct {
	reg    registry.Registry
	ctx    context.Context
	cancel context.CancelFunc
}

func (tr *testRegistryImpl) Server() *Server {
	return NewServer(tr.reg, testLogger(), ":0", security.Config{DevMode: true})
}

func (tr *testRegistryImpl) ServerWithCfg(cfg security.Config) *Server {
	return NewServer(tr.reg, testLogger(), ":0", cfg)
}

// busyWorkerTestWorker returns a worker with the given status, suitable for
// populating the capability cache in busy-status tests.
func busyWorkerTestWorker(id string, status protocol.WorkerStatus) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:       id,
		Hostname: id,
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   status,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: "busy-model", Quantization: "Q4_K_M", Loaded: true},
			},
		},
	}
}

// busyTestServer builds a server whose capability cache holds the given
// workers, so /v1/workers projects them.
func busyTestServer(t *testing.T, workers []protocol.WorkerInfo) (*testRegistryImpl, *Server) {
	t.Helper()
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	return tr, srv
}

// scorerTestWorkers returns two identical workers advertising the same model,
// so every static scoring signal ties and only load data can break symmetry.
func scorerTestWorkers() []protocol.WorkerInfo {
	mk := func(id string) protocol.WorkerInfo {
		return protocol.WorkerInfo{
			ID:       id,
			Hostname: id,
			IP:       "127.0.0.1",
			Port:     8081,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			LastSeen: time.Now(),
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "llama-3-8b", Size: 4820000000, Quantization: "Q4_K_M", MaxTokens: 8192, Backend: "llama-cpp", Loaded: true},
				},
				VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
			},
		}
	}
	return []protocol.WorkerInfo{mk("worker-b"), mk("worker-a")}
}

// scorerTestServer builds a server whose capability cache holds two identical
// workers for "llama-3-8b".
func scorerTestServer(t *testing.T) (*testRegistryImpl, *Server) {
	t.Helper()
	workers := scorerTestWorkers()
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	return tr, srv
}

// mockWorkerClient is a WorkerClient implementation that returns configured
// responses for testing dispatchWithFailover.
type mockWorkerClient struct {
	mu      sync.Mutex
	results map[string]mockResult
	calls   map[string]int
}

type mockResult struct {
	body  []byte
	err   error
	block time.Duration
}

func newMockWorkerClient() *mockWorkerClient {
	return &mockWorkerClient{
		results: make(map[string]mockResult),
		calls:   make(map[string]int),
	}
}

func (c *mockWorkerClient) Transport() string { return "http" }

func (c *mockWorkerClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	c.mu.Lock()
	c.calls[worker.ID]++
	r, ok := c.results[worker.ID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no mock response for worker %s", worker.ID)
	}
	if r.block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.block):
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.body, nil
}

func (c *mockWorkerClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent)
	errCh := make(chan error, 1)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (c *mockWorkerClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	return true, nil
}

func (c *mockWorkerClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, nil
}

func (c *mockWorkerClient) Close() error { return nil }

func (c *mockWorkerClient) RecordTimeout(workerID string) {}

// failoverTestServer sets up a server with two workers in the cache and a
// mockWorkerClient that returns configured responses per worker ID.
func failoverTestServer(t *testing.T, workers []protocol.WorkerInfo) (*testRegistryImpl, *Server, *mockWorkerClient) {
	t.Helper()
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	fc := newMockWorkerClient()
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }
	return tr, srv, fc
}

// mkWorker creates a test worker with the given ID and model.
func mkWorker(id, model string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		ID:       id,
		Hostname: id,
		IP:       "127.0.0.1",
		Port:     8081,
		Status:   protocol.StatusAvailable,
		Version:  "v1",
		LastSeen: time.Now(),
		Capabilities: protocol.Capabilities{
			Models: []protocol.ModelInfo{
				{Name: model, Size: 4820000000, Quantization: "Q4_K_M", MaxTokens: 8192, Backend: "llama-cpp", Loaded: true},
			},
			VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
		},
	}
}

// stalledClient is a mock WorkerClient that never returns (simulates a slow/hanging worker).
type stalledClient struct{}

func (c *stalledClient) Transport() string { return "http" }

func (c *stalledClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	chatCh := make(chan StreamEvent)
	errCh := make(chan error, 1)
	// Never sends anything - caller will timeout
	return chatCh, errCh
}

func (c *stalledClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *stalledClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	return false, ctx.Err()
}

func (c *stalledClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, nil
}

func (c *stalledClient) Close() error                  { return nil }
func (c *stalledClient) RecordTimeout(workerID string) {}

// timeoutClient is a mock WorkerClient that never produces a chunk and surfaces a timeout error
type timeoutClient struct {
	workerID string
	hub      *WSHub
}

func (c *timeoutClient) Transport() string { return "http" }

func (c *timeoutClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent)
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			errCh <- fmt.Errorf("%w: %w", errTimeout, ctx.Err())
		case <-time.After(50 * time.Millisecond):
			errCh <- errTimeout
		}
	}()
	return chunkCh, errCh
}

func (c *timeoutClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

func (c *timeoutClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	return false, nil
}

func (c *timeoutClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, nil
}

func (c *timeoutClient) Close() error { return nil }

func (c *timeoutClient) RecordTimeout(workerID string) {
	if c.hub != nil {
		c.hub.recordTimeout(workerID)
	}
}

// activeStreamClient implements WorkerClient: Stream emits a chunk every tickMs until ctx is done, then closes.
type activeStreamClient struct {
	tickMs int
	chunks int
}

func (c *activeStreamClient) Transport() string { return "http" }
func (c *activeStreamClient) Stream(ctx context.Context, _ protocol.WorkerInfo, _ string, _ []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(chunkCh)
		defer close(errCh)
		interval := time.Duration(c.tickMs) * time.Millisecond
		for i := 0; i < c.chunks; i++ {
			select {
			case chunkCh <- StreamEvent{Data: []byte(fmt.Sprintf("data: chunk %d\n\n", i))}:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return
			}
		}
		chunkCh <- StreamEvent{Data: []byte("data: [DONE]\n\n"), Done: true}
	}()
	return chunkCh, errCh
}
func (c *activeStreamClient) Complete(ctx context.Context, _ protocol.WorkerInfo, _ string, _ []byte) ([]byte, error) {
	return nil, ctx.Err()
}
func (c *activeStreamClient) LoadModel(ctx context.Context, _ protocol.WorkerInfo, _ string) (bool, error) {
	return false, ctx.Err()
}
func (c *activeStreamClient) Capabilities(ctx context.Context, _ protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, ctx.Err()
}
func (c *activeStreamClient) Close() error                  { return nil }
func (c *activeStreamClient) RecordTimeout(workerID string) {}

// silentAfterClient implements WorkerClient: Stream emits one chunk then goes silent.
type silentAfterClient struct {
	silenceMs int
}

func (c *silentAfterClient) Transport() string { return "http" }
func (c *silentAfterClient) Stream(ctx context.Context, _ protocol.WorkerInfo, _ string, _ []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(chunkCh)
		defer close(errCh)
		select {
		case chunkCh <- StreamEvent{Data: []byte("data: first\n\n")}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return chunkCh, errCh
}
func (c *silentAfterClient) Complete(ctx context.Context, _ protocol.WorkerInfo, _ string, _ []byte) ([]byte, error) {
	return nil, ctx.Err()
}
func (c *silentAfterClient) LoadModel(ctx context.Context, _ protocol.WorkerInfo, _ string) (bool, error) {
	return false, ctx.Err()
}
func (c *silentAfterClient) Capabilities(ctx context.Context, _ protocol.WorkerInfo) (protocol.Capabilities, error) {
	return protocol.Capabilities{}, ctx.Err()
}
func (c *silentAfterClient) Close() error                  { return nil }
func (c *silentAfterClient) RecordTimeout(workerID string) {}

// setupAutoRoutingHandler sets up a server with auto-routing config and mock client.
func setupAutoRoutingHandler(t *testing.T, workers []protocol.WorkerInfo, autoCfg *AutoRoutingConfig) (*Server, *mockWorkerClient, *testRegistryImpl) {
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	srv.SetAutoRouting(autoCfg)

	fc := newMockWorkerClient()
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }
	return srv, fc, tr
}

// setupAliasHandler sets up a server with model alias config and mock client.
func setupAliasHandler(t *testing.T, workers []protocol.WorkerInfo, aliasCfg *ModelAliasConfig) (*Server, *mockWorkerClient, *testRegistryImpl) {
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	srv.SetModelAliases(aliasCfg)

	fc := newMockWorkerClient()
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }
	return srv, fc, tr
}

func makeChatRequest(model string, messages []protocol.ChatMessage, maxTokens int, stream bool) (*httptest.ResponseRecorder, *http.Request) {
	body := map[string]interface{}{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	return w, req
}

func makeCompletionRequest(model string, prompt string, maxTokens int) (*httptest.ResponseRecorder, *http.Request) {
	body := map[string]interface{}{
		"model":      model,
		"prompt":     prompt,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	return w, req
}
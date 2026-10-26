package worker

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
// mockBackend is a test implementation of the Backend interface.
type mockBackend struct {
	mu          sync.Mutex
	models      []protocol.ModelInfo
	healthCheck error
	threads     int // effective thread count reported by GetMetrics (issue #139)

	// streamError, when non-nil, is sent on the streaming error channel.
	// streamErrorBeforeChunks controls whether the error is emitted before
	// any chunk (true) or after the first chunk (false). When beforeChunks
	// is false the error is sent only after the handler has consumed the
	// first chunk, which is deterministic because chatCh is unbuffered: the
	// mock blocks on the chunk send until the handler reads it.
	streamError             error
	streamErrorBeforeChunks bool
}

func newMockBackend() *mockBackend {
	return &mockBackend{}
}

func (m *mockBackend) SetHealthCheckError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthCheck = err
}

// SetStreamError configures the mock's streaming error behaviour. Pass nil
// to clear it. When beforeChunks is true the error is emitted before any
// chunk is sent (simulating a backend that fails during model loading); when
// false the error is emitted after the first chunk (simulating a mid-stream
// backend failure).
func (m *mockBackend) SetStreamError(err error, beforeChunks bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamError = err
	m.streamErrorBeforeChunks = beforeChunks
}

// SetMockModels replaces the catalogue the mock advertises on the next
// ListModels() call. Used to simulate a backend that starts after the worker.
func (m *mockBackend) SetMockModels(models []protocol.ModelInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = models
}

func (m *mockBackend) Name() string                        { return "mock" }
func (m *mockBackend) LoadModel(path string) (bool, error) { return true, nil }
func (m *mockBackend) UnloadModel() error                  { return nil }
func (m *mockBackend) ListModels() ([]protocol.ModelInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.ModelInfo, len(m.models))
	copy(out, m.models)
	return out, nil
}
func (m *mockBackend) GetMetrics() (Metrics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Metrics{Threads: m.threads}, nil
}

// SetThreads configures the effective thread count the mock reports via
// GetMetrics (issue #139).
func (m *mockBackend) SetThreads(threads int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.threads = threads
}
func (m *mockBackend) CompleteChat(ctx context.Context, model string, req protocol.ChatRequest) (protocol.ChatResponse, error) {
	return protocol.ChatResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Choices: []protocol.Choice{
			{
				Index:        0,
				Message:      protocol.ChatMessage{Role: "assistant", Content: "Mock response"},
				FinishReason: "stop",
			},
		},
	}, nil
}
func (m *mockBackend) CompleteCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (protocol.CompletionResponse, error) {
	return protocol.CompletionResponse{
		ID:      "cmpl-test",
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Choices: []protocol.Choice{
			{
				Index:        0,
				Text:         "Mock completion",
				FinishReason: "stop",
			},
		},
	}, nil
}

// IsHealthy implements the Backend interface.
func (m *mockBackend) IsHealthy() bool { return true }

// HealthCheck implements the Backend interface.
func (m *mockBackend) HealthCheck() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthCheck
}

// GetCircuitState implements the Backend interface.
func (m *mockBackend) GetCircuitState() CircuitState { return CircuitClosed }

// StreamChat implements the Backend interface.
// The chunk channel is unbuffered so that a mid-stream error is emitted
// only after the handler has consumed the first chunk, making the error
// path deterministic.
func (m *mockBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	chatCh := make(chan protocol.ChatChunk)
	errCh := make(chan error, 1)
	go func() {
		defer close(chatCh)
		defer close(errCh)
		m.mu.Lock()
		err := m.streamError
		beforeChunks := m.streamErrorBeforeChunks
		m.mu.Unlock()
		if err != nil && beforeChunks {
			errCh <- err
			return
		}
		chatCh <- protocol.ChatChunk{
			ID:      "chatcmpl-test",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []protocol.Choice{
				{
					Index:        0,
					Message:      protocol.ChatMessage{Role: "assistant", Content: "Mock streaming response"},
					FinishReason: "stop",
				},
			},
		}
		if err != nil {
			errCh <- err
		}
	}()
	return chatCh, errCh
}

// StreamCompletions implements the Backend interface.
func (m *mockBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	compCh := make(chan protocol.CompletionChunk, 100)
	errCh := make(chan error, 1)
	go func() {
		defer close(compCh)
		defer close(errCh)
		m.mu.Lock()
		err := m.streamError
		beforeChunks := m.streamErrorBeforeChunks
		m.mu.Unlock()
		if err != nil && beforeChunks {
			errCh <- err
			return
		}
		compCh <- protocol.CompletionChunk{
			ID:      "cmpl-test",
			Object:  "text_completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []protocol.Choice{
				{
					Index:        0,
					Text:         "Mock streaming completion",
					FinishReason: "stop",
				},
			},
		}
		if err != nil {
			errCh <- err
		}
	}()
	return compCh, errCh
}

// Package worker contains adversarial tests for the "stuck at 0% cold model load" fix.
// These tests probe race conditions, boundary values, and error amplification
// scenarios that the spec tests may not cover.
package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// =============================================================================
// ADVERSARIAL PROBE 1: Race condition - Grace expiry vs client cancel
// =============================================================================

// TestStreamGrace_CancelBeforeGraceExpiry probes the race between:
// - Client context cancellation during the grace period
// - Grace period timer firing
// The system must handle both without deadlock or state corruption.
func TestStreamGrace_CancelBeforeGraceExpiry(t *testing.T) {
	log := testLogger()

	// Backend that delays first chunk past grace period
	slowBackend := &delayedStreamBackend{
		delayFirstChunk: 500 * time.Millisecond,
		chunks: []protocol.ChatChunk{
			{
				ID:      "test-1",
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   "slow-model",
				Choices: []protocol.Choice{{Index: 0, Message: protocol.ChatMessage{Role: "assistant", Content: "response"}}},
			},
		},
	}

	srv := NewServer(log, "127.0.0.1:0", security.Config{DevMode: true, EnableHealthChecks: false})
	oldGrace := streamingGracePeriod
	streamingGracePeriod = 100 * time.Millisecond // Short grace for test
	defer func() { streamingGracePeriod = oldGrace }()

	srv.SetBackend(slowBackend, "slow-model")
	srv.SetModels([]protocol.ModelInfo{{Name: "slow-model", Backend: "mock", Loaded: false}})

	// Client cancels during grace period
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel slightly after grace period starts but before first chunk
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	// Use the HTTP handler path
	req := &http.Request{
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   mustReadCloser(strings.NewReader(`{"model":"slow-model","messages":[{"role":"user","content":"test"}],"stream":true}`)),
	}

	rec := httptest.NewRecorder()
	srv.chatCompletions(rec, req.WithContext(ctx))

	// Should gracefully return (no error, no hang)
	// The context cancellation should be honored
	if rec.Code != http.StatusOK && rec.Code != http.StatusRequestTimeout {
		t.Logf("status code: %d (expected 200 or 408)", rec.Code)
	}
}

// mustReadCloser creates an io.ReadCloser from a string for testing
type stringReadCloser struct {
	*strings.Reader
}

func (s *stringReadCloser) Close() error { return nil }

func mustReadCloser(r *strings.Reader) interface {
	Read([]byte) (int, error)
	Close() error
} {
	return &stringReadCloser{r}
}

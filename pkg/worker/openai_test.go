package worker

import (
	"context"
	"encoding/json"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOpenAICompatibleBackendStreamChatIdleTimeoutSlowActive verifies the core
// regression: a stream that keeps producing chunks (every 100ms) for 1.5s
// total with a 300ms idle timeout must deliver all chunks without error.
// Under the old absolute deadline (http.Client.Timeout) this would have been
// killed at 300ms.
func TestOpenAICompatibleBackendStreamChatIdleTimeoutSlowActive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		// Send 15 chunks every 100ms = 1.5s total
		for i := 0; i < 15; i++ {
			chunk := protocol.ChatChunk{
				ID:      "test",
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   "test",
				Choices: []protocol.Choice{{Index: 0, Text: "token", FinishReason: ""}},
			}
			data, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
			flusher.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		// Signal done
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 300*time.Millisecond, 0)
	req := protocol.ChatRequest{
		Model:       "test",
		Messages:    []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:   100,
		Temperature: 0.7,
	}

	ch, errCh := backend.StreamChat(context.Background(), "test", req)
	chunksReceived := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
			chunksReceived++
		}
	}()

	// Wait for either an error (non-nil) or completion
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("unexpected error from backend stream: %v", err)
		}
		// err == nil means errCh was closed without an error, which is fine.
	case <-done:
		// Channel closed normally.
	}

	// Wait for channel to close if not already
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream did not complete in time")
	}

	if chunksReceived < 15 {
		t.Errorf("expected at least 15 chunks, got %d", chunksReceived)
	}
}

// TestOpenAICompatibleBackendStreamChatIdleTimeoutSilentMidStream verifies that
// once chunks start flowing and then silence exceeds the idle timeout, the
// error is surfaced on errCh and is classifiable as a timeout (504 path via
// isBackendTimeout).
func TestOpenAICompatibleBackendStreamChatIdleTimeoutSilentMidStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		chunk := protocol.ChatChunk{
			ID:      "test",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   "test",
			Choices: []protocol.Choice{{Index: 0, Text: "token", FinishReason: ""}},
		}
		data, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		flusher.Flush()

		// Go silent for 1s
		time.Sleep(1 * time.Second)
		// Stream never completes
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 300*time.Millisecond, 0)
	req := protocol.ChatRequest{
		Model:       "test",
		Messages:    []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:   100,
		Temperature: 0.7,
	}

	ch, errCh := backend.StreamChat(context.Background(), "test", req)
	chunksReceived := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
			chunksReceived++
		}
	}()

	// Should receive an error within ~300ms + some buffer
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected non-nil error from idle timeout")
		}
		if !isBackendTimeout(err) {
			t.Errorf("expected timeout error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "idle timeout") {
			t.Errorf("expected idle timeout error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected error from backend stream, but none received")
	}

	// Wait for channel to close
	<-done

	if chunksReceived < 1 {
		t.Errorf("expected at least 1 chunk, got %d", chunksReceived)
	}
}

// TestOpenAICompatibleBackendStreamChatIdleTimeoutNeverResponds verifies that
// when the backend never responds (hangs in Do), a short idle timeout fires
// and surfaces an idle-timeout error before any chunk is produced.
func TestOpenAICompatibleBackendStreamChatIdleTimeoutNeverResponds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never write anything; simulate a hanging backend. The client's
		// idle timeout fires at 200ms and cancels the request, but the
		// server-side handler doesn't see the client disconnect — keep it
		// alive just long enough to not respond before the timeout, then
		// return so the test server can close promptly.
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 200*time.Millisecond, 0)
	req := protocol.ChatRequest{
		Model:       "test",
		Messages:    []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:   100,
		Temperature: 0.7,
	}

	ch, errCh := backend.StreamChat(context.Background(), "test", req)

	// Should receive an error within ~200ms
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected non-nil error from idle timeout")
		}
		if !isBackendTimeout(err) {
			t.Errorf("expected timeout error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "idle timeout") {
			t.Errorf("expected idle timeout error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected error from backend stream, but none received")
	}

	// Drain the channel to avoid goroutine leak
	for range ch {
	}
}

// TestStreamChatPreservesReasoningContent verifies that the worker's
// unmarshal→marshal round-trip (backend SSE → protocol.ChatChunk →
// inference_chunk frame) does NOT silently drop delta.reasoning_content.
//
// Reasoning models (LM Studio gpt-oss-20b, DeepSeek-R1-style backends) emit
// their chain-of-thought under the OpenAI-compatible "reasoning_content" key
// with an empty content delta. Without a Reasoning field on ChatMessage the
// worker's json.Unmarshal discards it and the client receives an empty
// response — the model appears broken even though the backend works.
func TestStreamChatPreservesReasoningContent(t *testing.T) {
	const reasoning = "The user wants me to say hello, so I should greet them."
	const content = "Hello!"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// First chunk carries reasoning only (the shape reasoning models emit).
		_, _ = w.Write([]byte("data: " + mustMarshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": "reason-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"role": "assistant", "reasoning_content": reasoning,
			}, "finish_reason": nil}},
		}) + "\n\n"))
		flusher.Flush()
		// Second chunk carries the visible answer.
		_, _ = w.Write([]byte("data: " + mustMarshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": "reason-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"content": content,
			}, "finish_reason": nil}},
		}) + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	req := protocol.ChatRequest{
		Model:     "reason-model",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	}

	ch, errCh := backend.StreamChat(context.Background(), "reason-model", req)
	var gotReasoning, gotContent string
	for c := range ch {
		if len(c.Choices) == 0 {
			continue
		}
		delta := c.Choices[0].Delta
		if delta.Reasoning != "" {
			gotReasoning = delta.Reasoning
		}
		if delta.Content != "" {
			gotContent = delta.Content
		}
	}
	// errCh is closed by the stream goroutine; a nil sentinel may be sent
	// before close, so drain it rather than selecting on it.
	for err := range errCh {
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
	}

	if gotReasoning != reasoning {
		t.Errorf("reasoning_content lost in round-trip: want %q, got %q", reasoning, gotReasoning)
	}
	if gotContent != content {
		t.Errorf("content lost in round-trip: want %q, got %q", content, gotContent)
	}
}

// TestCompleteChatPreservesReasoningTokens verifies that a non-streaming
// backend response carrying usage.completion_tokens_details.reasoning_tokens
// survives the worker's unmarshal→marshal round-trip (backend JSON →
// protocol.ChatResponse → client JSON). Reasoning models (LM Studio
// gpt-oss-20b, DeepSeek-R1-style backends) report reasoning tokens here;
// without TokenDetails fields the worker's json.Unmarshal discards them and
// the client cannot see reasoning-token cost (issue #204 follow-up).
func TestCompleteChatPreservesReasoningTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mustMarshal(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "reason-model",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "ok",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":             10,
				"completion_tokens":         5,
				"total_tokens":              15,
				"prompt_tokens_details":     map[string]any{"cached_tokens": 4},
				"completion_tokens_details": map[string]any{"reasoning_tokens": 3},
			},
		})))
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	req := protocol.ChatRequest{
		Model:     "reason-model",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	}
	resp, err := backend.CompleteChat(context.Background(), "reason-model", req)
	if err != nil {
		t.Fatalf("CompleteChat failed: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.CompletionTokensDetails == nil || resp.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Errorf("completion_tokens_details.reasoning_tokens lost: %+v", resp.Usage.CompletionTokensDetails)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("prompt_tokens_details.cached_tokens lost: %+v", resp.Usage.PromptTokensDetails)
	}

	// Re-marshal exactly as the worker's dispatchNonStreaming does, to prove
	// the details survive to the client wire payload.
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	usage := check["usage"].(map[string]any)
	if d, ok := usage["completion_tokens_details"].(map[string]any); !ok || d["reasoning_tokens"] != float64(3) {
		t.Errorf("round-trip lost completion_tokens_details: %v", usage["completion_tokens_details"])
	}
	if d, ok := usage["prompt_tokens_details"].(map[string]any); !ok || d["cached_tokens"] != float64(4) {
		t.Errorf("round-trip lost prompt_tokens_details: %v", usage["prompt_tokens_details"])
	}
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
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

// TestCompleteChatForwardsToolsToBackend verifies the request side of
// tool-call pass-through: a client request with tools/tool_choice/
// parallel_tool_calls must reach the backend as real JSON keys, not be
// silently dropped by the worker's request-builder map. Without this the
// backend never sees the tool definitions and emits tool invocations as
// literal text inside content.
func TestCompleteChatForwardsToolsToBackend(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("backend request is not valid JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	parallel := true
	req := protocol.ChatRequest{
		Model:     "test-model",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "What is the weather in Oslo?"}},
		MaxTokens: 100,
		Tools: []protocol.Tool{{
			Type: "function",
			Function: protocol.FunctionSpec{
				Name:        "get_weather",
				Description: "Get the current weather",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}},
		ToolChoice:        "auto",
		ParallelToolCalls: &parallel,
	}
	if _, err := backend.CompleteChat(context.Background(), "test-model", req); err != nil {
		t.Fatalf("CompleteChat failed: %v", err)
	}

	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not forwarded to backend: %v", gotBody["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool function.name lost in backend request: %v", fn)
	}
	if params, ok := fn["parameters"].(map[string]any); !ok || params["type"] != "object" {
		t.Errorf("tool parameters lost in backend request: %v", fn["parameters"])
	}
	if gotBody["tool_choice"] != "auto" {
		t.Errorf("tool_choice not forwarded to backend: %v", gotBody["tool_choice"])
	}
	if gotBody["parallel_tool_calls"] != true {
		t.Errorf("parallel_tool_calls not forwarded to backend: %v", gotBody["parallel_tool_calls"])
	}
}

// TestCompleteChatPlainRequestHasNoToolsKey verifies the wire-compat half of
// tool-call pass-through: a plain-chat request must not acquire a
// "tools" key in the backend request (no null values, byte-compatible
// behavior for non-tool clients).
func TestCompleteChatPlainRequestHasNoToolsKey(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("backend request is not valid JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	req := protocol.ChatRequest{
		Model:     "test-model",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	}
	if _, err := backend.CompleteChat(context.Background(), "test-model", req); err != nil {
		t.Fatalf("CompleteChat failed: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := gotBody[key]; ok {
			t.Errorf("plain-chat backend request must not carry %q, got: %v", key, gotBody)
		}
	}
}

// TestCompleteChatPreservesToolCalls verifies that a non-streaming backend
// response carrying structured tool_calls survives the worker's
// unmarshal→marshal round-trip (backend JSON → protocol.ChatResponse →
// client JSON). Without ToolCalls on ChatMessage the worker's
// json.Unmarshal discards the calls and the client only sees the (empty)
// content.
func TestCompleteChatPreservesToolCalls(t *testing.T) {
	const args = `{"city":"Oslo"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mustMarshal(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{{
						"id":   "call_abc123",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": args,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})))
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	req := protocol.ChatRequest{
		Model:     "test-model",
		Messages:  []protocol.ChatMessage{{Role: "user", Content: "What is the weather in Oslo?"}},
		MaxTokens: 100,
	}
	resp, err := backend.CompleteChat(context.Background(), "test-model", req)
	if err != nil {
		t.Fatalf("CompleteChat failed: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls lost in round-trip: %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc123" || tc.Function.Name != "get_weather" || tc.Function.Arguments != args {
		t.Errorf("tool_call fields lost: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason: want %q, got %q", "tool_calls", resp.Choices[0].FinishReason)
	}

	// Re-marshal exactly as dispatchNonStreaming does, to prove the calls
	// survive to the client wire payload.
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	toolCalls := check["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	fn := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != args {
		t.Errorf("round-trip lost tool_call arguments: %v", fn)
	}
}

// TestStreamChatPreservesToolCalls verifies that a streaming backend that
// emits a tool_calls delta sequence (first chunk id+name, subsequent chunks
// arguments fragments, final chunk finish_reason:"tool_calls") survives the
// worker's unmarshal→marshal round-trip per SSE frame. It also asserts the
// stream request itself forwards tools to the backend.
func TestStreamChatPreservesToolCalls(t *testing.T) {
	var streamBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &streamBody); err != nil {
			t.Errorf("stream request is not valid JSON: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// First chunk opens the tool call (id + name).
		_, _ = w.Write([]byte("data: " + mustMarshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": "test-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":       "call_abc123",
					"type":     "function",
					"index":    0,
					"function": map[string]any{"name": "get_weather", "arguments": ""},
				}},
			}, "finish_reason": nil}},
		}) + "\n\n"))
		flusher.Flush()
		// Second chunk carries an arguments fragment (no id/name).
		_, _ = w.Write([]byte("data: " + mustMarshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": "test-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"tool_calls": []map[string]any{{
					"index":    0,
					"function": map[string]any{"arguments": "{\"cit"},
				}},
			}, "finish_reason": nil}},
		}) + "\n\n"))
		flusher.Flush()
		// Final chunk completes the arguments and sets finish_reason.
		_, _ = w.Write([]byte("data: " + mustMarshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": "test-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{
				"tool_calls": []map[string]any{{
					"index":    0,
					"function": map[string]any{"arguments": "y\":\"Oslo\"}"},
				}},
			}, "finish_reason": "tool_calls"}},
		}) + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	backend := NewOpenAICompatibleBackend(server.URL, "", 30*time.Second, 0)
	req := protocol.ChatRequest{
		Model:    "test-model",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "What is the weather in Oslo?"}},
		Tools: []protocol.Tool{{
			Type: "function",
			Function: protocol.FunctionSpec{
				Name:       "get_weather",
				Parameters: map[string]any{"type": "object"},
			},
		}},
	}

	ch, errCh := backend.StreamChat(context.Background(), "test-model", req)
	var gotID, gotName string
	var gotArgs string
	finishReason := ""
	for c := range ch {
		if len(c.Choices) == 0 {
			continue
		}
		choice := c.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		for _, tc := range choice.Delta.ToolCalls {
			if tc.ID != "" {
				gotID = tc.ID
			}
			if tc.Function.Name != "" {
				gotName = tc.Function.Name
			}
			gotArgs += tc.Function.Arguments
		}
	}
	for err := range errCh {
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
	}

	if gotID != "call_abc123" {
		t.Errorf("tool_call id lost in stream: got %q", gotID)
	}
	if gotName != "get_weather" {
		t.Errorf("tool_call name lost in stream: got %q", gotName)
	}
	if gotArgs != `{"city":"Oslo"}` {
		t.Errorf("tool_call arguments fragments lost in stream: got %q", gotArgs)
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish_reason: want %q, got %q", "tool_calls", finishReason)
	}

	// Request-side: the stream request must forward tools to the backend.
	tools, ok := streamBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Errorf("tools not forwarded in stream request: %v", streamBody["tools"])
	}
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

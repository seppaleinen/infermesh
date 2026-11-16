package protocol

import (
	"encoding/json"
	"testing"
)

// TestUsageRoundTrip_ReasoningTokens verifies that a non-streaming backend
// response carrying completion_tokens_details.reasoning_tokens survives the
// worker's unmarshal→marshal round-trip and reaches the client. Reasoning
// models (LM Studio gpt-oss-20b, DeepSeek-R1-style backends) report reasoning
// tokens here; without the TokenDetails fields the worker's json.Unmarshal
// discards them and the client cannot see reasoning-token cost.
func TestUsageRoundTrip_ReasoningTokens(t *testing.T) {
	in := `{
		"id": "chatcmpl-test",
		"object": "chat.completion",
		"created": 1,
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"total_tokens": 15,
			"prompt_tokens_details": {"cached_tokens": 4},
			"completion_tokens_details": {"reasoning_tokens": 3}
		}
	}`

	var resp ChatResponse
	if err := json.Unmarshal([]byte(in), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
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

// TestUsageRoundTrip_NonReasoningWireCompat verifies that a plain response
// without token details still marshals without the details keys, so
// non-reasoning backends stay wire-compatible (no new null/empty fields).
func TestUsageRoundTrip_NonReasoningWireCompat(t *testing.T) {
	resp := ChatResponse{
		ID:     "chatcmpl-test",
		Object: "chat.completion",
		Usage: &Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	usage := check["usage"].(map[string]any)
	if _, ok := usage["completion_tokens_details"]; ok {
		t.Errorf("completion_tokens_details must be omitted for non-reasoning responses")
	}
	if _, ok := usage["prompt_tokens_details"]; ok {
		t.Errorf("prompt_tokens_details must be omitted for non-reasoning responses")
	}
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(15) {
		t.Errorf("base usage fields changed: %v", usage)
	}
}

// TestToolCallingRequestRoundTrip verifies that a chat request carrying
// OpenAI tool-calling fields (tools with JSON-Schema parameters,
// tool_choice in both string and object forms, parallel_tool_calls)
// survives the worker's unmarshal→marshal round-trip so the backend
// actually receives the tool definitions. Without the Tools/ToolChoice/
// ParallelToolCalls fields on ChatRequest the fields are silently dropped
// and tool-using models emit tool invocations as literal text inside
// content.
func TestToolCallingRequestRoundTrip(t *testing.T) {
	t.Run("tools + string tool_choice + parallel_tool_calls", func(t *testing.T) {
		in := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "What is the weather?"}],
			"stream": false,
			"max_tokens": 100,
			"temperature": 0.7,
			"tools": [{
				"type": "function",
				"function": {
					"name": "get_weather",
					"description": "Get the current weather",
					"parameters": {
						"type": "object",
						"properties": {"city": {"type": "string", "description": "The city"}},
						"required": ["city"]
					}
				}
			}],
			"tool_choice": "auto",
			"parallel_tool_calls": true
		}`

		var req ChatRequest
		if err := json.Unmarshal([]byte(in), &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(req.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(req.Tools))
		}
		tool := req.Tools[0]
		if tool.Type != "function" {
			t.Errorf("tool type: want %q, got %q", "function", tool.Type)
		}
		if tool.Function.Name != "get_weather" || tool.Function.Description != "Get the current weather" {
			t.Errorf("function spec lost: %+v", tool.Function)
		}
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("parameters not preserved as JSON object: %T", tool.Function.Parameters)
		}
		if params["type"] != "object" {
			t.Errorf("parameters.type lost: %v", params)
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("parameters.properties lost: %v", params)
		}
		if props["city"] == nil {
			t.Errorf("parameters.properties.city lost: %v", props)
		}
		if req.ToolChoice != "auto" {
			t.Errorf("tool_choice: want %q, got %v", "auto", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
			t.Errorf("parallel_tool_calls: want true, got %v", req.ParallelToolCalls)
		}

		// Re-marshal exactly as the router/worker would and confirm the
		// fields survive to the backend wire payload.
		out, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var check map[string]any
		if err := json.Unmarshal(out, &check); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
		tools, ok := check["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("round-trip lost tools: %v", check["tools"])
		}
		fn := tools[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "get_weather" {
			t.Errorf("round-trip lost function.name: %v", fn)
		}
		if check["tool_choice"] != "auto" {
			t.Errorf("round-trip lost tool_choice: %v", check["tool_choice"])
		}
		if check["parallel_tool_calls"] != true {
			t.Errorf("round-trip lost parallel_tool_calls: %v", check["parallel_tool_calls"])
		}
	})

	t.Run("object tool_choice form", func(t *testing.T) {
		in := `{
			"model": "test-model",
			"messages": [{"role": "user", "content": "hi"}],
			"tools": [{"type": "function", "function": {"name": "f"}}],
			"tool_choice": {"type": "function", "function": {"name": "f"}}
		}`

		var req ChatRequest
		if err := json.Unmarshal([]byte(in), &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.ToolChoice == nil {
			t.Fatal("tool_choice is nil")
		}
		out, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var check map[string]any
		if err := json.Unmarshal(out, &check); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
		tc, ok := check["tool_choice"].(map[string]any)
		if !ok || tc["type"] != "function" {
			t.Fatalf("object tool_choice form not preserved: %v", check["tool_choice"])
		}
		if fn := tc["function"].(map[string]any); fn["name"] != "f" {
			t.Errorf("object tool_choice function.name lost: %v", fn)
		}
	})
}

// TestToolCallingResponseRoundTrip verifies that a non-streaming backend
// response carrying structured tool_calls (id + type + function.name +
// string-encoded arguments) survives the worker's unmarshal→marshal
// round-trip, and that a tool-result message (role:"tool" with
// tool_call_id) round-trips too.
func TestToolCallingResponseRoundTrip(t *testing.T) {
	const args = `{"city":"Oslo"}`
	// Build input JSON programmatically to avoid backtick-string quoting
	// issues with the args constant containing double quotes.
	inBytes, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1,
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
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	})
	in := string(inBytes)

	var resp ChatResponse
	if err := json.Unmarshal([]byte(in), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_abc123" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != args {
		t.Errorf("tool_call lost fields: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason: want %q, got %q", "tool_calls", resp.Choices[0].FinishReason)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	choice := check["choices"].([]any)[0].(map[string]any)
	toolCalls := choice["message"].(map[string]any)["tool_calls"].([]any)
	call := toolCalls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_abc123" || fn["arguments"] != args {
		t.Errorf("round-trip lost tool_call fields: %v", call)
	}

	// Tool result message from the client (role:"tool" + tool_call_id).
	toolMsg := ChatMessage{Role: "tool", Content: "Sunny", ToolCallID: "call_abc123"}
	out2, err := json.Marshal(toolMsg)
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	var toolCheck map[string]any
	if err := json.Unmarshal(out2, &toolCheck); err != nil {
		t.Fatalf("re-unmarshal tool message: %v", err)
	}
	if toolCheck["role"] != "tool" || toolCheck["tool_call_id"] != "call_abc123" {
		t.Errorf("tool message round-trip failed: %v", toolCheck)
	}
}

// TestToolCallingStreamingDeltaRoundTrip verifies that streaming tool_call
// delta partials (first chunk: id+index+function.name; later chunks:
// index+function.arguments fragments; final chunk: finish_reason
// "tool_calls") survive a chunk unmarshal→marshal round-trip, which is
// exactly what the worker's parseSSEStream path does per SSE frame.
func TestToolCallingStreamingDeltaRoundTrip(t *testing.T) {
	// First chunk: opens the call with id + name.
	first := `{
		"id": "chatcmpl-test",
		"object": "chat.completion.chunk",
		"created": 1,
		"choices": [{
			"index": 0,
			"delta": {"role": "assistant", "tool_calls": [{
				"id": "call_abc123",
				"type": "function",
				"index": 0,
				"function": {"name": "get_weather", "arguments": ""}
			}]},
			"finish_reason": null
		}]
	}`
	// Middle chunk: arguments fragment (no id/name, per OpenAI contract).
	middle := `{
		"id": "chatcmpl-test",
		"object": "chat.completion.chunk",
		"created": 1,
		"choices": [{
			"index": 0,
			"delta": {"tool_calls": [{
				"index": 0,
				"function": {"name": "", "arguments": "{\"cit"}
			}]},
			"finish_reason": null
		}]
	}`
	// Final chunk: last arguments fragment + finish_reason "tool_calls".
	final := `{
		"id": "chatcmpl-test",
		"object": "chat.completion.chunk",
		"created": 1,
		"choices": [{
			"index": 0,
			"delta": {"tool_calls": [{
				"index": 0,
				"function": {"name": "", "arguments": "y\":\"Oslo\"}"}
			}]},
			"finish_reason": "tool_calls"
		}]
	}`

	var firstChunk, middleChunk, finalChunk ChatChunk
	for i, name := range []string{"first", "middle", "final"} {
		var raw string
		switch name {
		case "first":
			raw = first
		case "middle":
			raw = middle
		default:
			raw = final
		}
		switch i {
		case 0:
			if err := json.Unmarshal([]byte(raw), &firstChunk); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
		case 1:
			if err := json.Unmarshal([]byte(raw), &middleChunk); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
		case 2:
			if err := json.Unmarshal([]byte(raw), &finalChunk); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
		}
	}

	if got := firstChunk.Choices[0].Delta.ToolCalls; len(got) != 1 || got[0].ID != "call_abc123" || got[0].Function.Name != "get_weather" {
		t.Errorf("first chunk delta.tool_calls lost id/name: %+v", got)
	}
	if got := middleChunk.Choices[0].Delta.ToolCalls; len(got) != 1 || got[0].Function.Arguments != `{"cit` {
		t.Errorf("middle chunk arguments fragment lost: %+v", got)
	}
	if finalChunk.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason: want %q, got %q", "tool_calls", finalChunk.Choices[0].FinishReason)
	}
	if got := finalChunk.Choices[0].Delta.ToolCalls; len(got) != 1 || got[0].Function.Arguments != `y":"Oslo"}` {
		t.Errorf("final chunk arguments fragment lost: %+v", got)
	}

	// Re-marshal exactly as the worker's streamSSE does and verify the
	// partials survive byte-for-byte through the client wire.
	reassembled := ""
	for _, c := range []ChatChunk{firstChunk, middleChunk, finalChunk} {
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var check ChatChunk
		if err := json.Unmarshal(out, &check); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
		if len(check.Choices) == 0 || len(check.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("round-trip lost delta.tool_calls: %s", out)
		}
		reassembled += check.Choices[0].Delta.ToolCalls[0].Function.Arguments
	}
	if reassembled != `{"city":"Oslo"}` {
		t.Errorf("reassembled arguments: want %q, got %q", `{"city":"Oslo"}`, reassembled)
	}
}

// TestPlainChatWireCompat verifies that requests and responses without any
// tool-calling fields marshal without the new keys, so existing plain-chat
// wire payloads stay byte-identical (no "tools":null / "tool_calls":null).
func TestPlainChatWireCompat(t *testing.T) {
	req := ChatRequest{
		Model:       "test-model",
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
		Stream:      true,
		MaxTokens:   100,
		Temperature: 0.7,
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("re-unmarshal request: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := check[key]; ok {
			t.Errorf("plain-chat request must not carry %q, got: %s", key, out)
		}
	}
	for _, key := range []string{"tool_calls", "tool_call_id", "name"} {
		if _, ok := check["messages"].([]any)[0].(map[string]any)[key]; ok {
			t.Errorf("plain-chat message must not carry %q", key)
		}
	}

	resp := ChatResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Choices: []Choice{{Index: 0, Message: ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
	}
	out2, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var check2 map[string]any
	if err := json.Unmarshal(out2, &check2); err != nil {
		t.Fatalf("re-unmarshal response: %v", err)
	}
	if _, ok := check2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"]; ok {
		t.Errorf("plain response message must not carry tool_calls: %s", out2)
	}
}

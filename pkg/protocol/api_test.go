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

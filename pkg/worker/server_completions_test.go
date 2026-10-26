package worker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestCompletionsHandler_NonStreaming(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	completionReq := protocol.CompletionRequest{
		Model:     "gpt-4",
		Prompt:    "Hello",
		MaxTokens: 100,
		Stream:    false,
	}

	body, err := json.Marshal(completionReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.completions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "data:") {
		t.Errorf("non-streaming response must not contain SSE framing, got: %s", respBody)
	}

	var compResp protocol.CompletionResponse
	if err := json.Unmarshal([]byte(respBody), &compResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if compResp.ID != "cmpl-test" {
		t.Errorf("expected ID 'cmpl-test', got '%s'", compResp.ID)
	}
	if len(compResp.Choices) != 1 || compResp.Choices[0].Text != "Mock completion" {
		t.Errorf("unexpected choices: %+v", compResp.Choices)
	}
}
func TestCompletionsHandler_StreamingChunkFormat(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	completionReq := protocol.CompletionRequest{
		Model:     "gpt-4",
		Prompt:    "Hello",
		MaxTokens: 100,
		Stream:    true,
	}

	body, err := json.Marshal(completionReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.completions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	respBody := w.Body.String()
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Errorf("streaming response must end with 'data: [DONE]', got: %s", respBody)
	}

	frames := strings.Split(strings.TrimRight(respBody, "\n"), "\n\n")
	foundChunk := false
	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk protocol.CompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("SSE frame is not valid protocol.CompletionChunk JSON: %v\nframe: %s", err, payload)
		}
		foundChunk = true
	}
	if !foundChunk {
		t.Errorf("expected at least one chunk frame, got: %s", respBody)
	}
}
func TestCompletionsHandler(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	server.SetBackend(newMockBackend(), "gpt-4")

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{
			name:           "completions with SSE",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionReq := protocol.CompletionRequest{
				Model:     "gpt-4",
				Prompt:    "Hello",
				MaxTokens: 100,
				Stream:    true,
			}

			body, err := json.Marshal(completionReq)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			server.completions(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if w.Header().Get("Content-Type") != "text/event-stream" {
				t.Errorf("expected Content-Type 'text/event-stream', got '%s'", w.Header().Get("Content-Type"))
			}

			if !strings.Contains(w.Body.String(), "data:") {
				t.Errorf("SSE response should contain 'data:'")
			}
		})
	}
}

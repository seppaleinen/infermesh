package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestChatCompletionsHandler_StreamErrorBeforeFirstChunk(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	backend := newMockBackend()
	backend.SetStreamError(errors.New("backend request failed: context deadline exceeded"), true)
	server.SetBackend(backend, "gpt-4")

	chatReq := protocol.ChatRequest{
		Model:    "gpt-4",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
		Stream:   true,
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.chatCompletions(w, req)

	// Must return 504 (timeout) with JSON body, not SSE.
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status %d, got %d", http.StatusGatewayTimeout, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}
	if strings.Contains(w.Body.String(), "data:") {
		t.Errorf("error-before-chunk must not emit SSE frame, got: %s", w.Body.String())
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response must be valid JSON: %v", err)
	}
	if errResp["error"] == "" {
		t.Errorf("expected error message, got: %v", errResp)
	}
}
func TestChatCompletionsHandler_StreamErrorMidStream(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	backend := newMockBackend()
	backend.SetStreamError(errors.New("backend request failed"), false)
	server.SetBackend(backend, "gpt-4")

	chatReq := protocol.ChatRequest{
		Model:    "gpt-4",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "Hello"}},
		Stream:   true,
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.chatCompletions(w, req)

	// Must return 200 (stream started) and end with [DONE].
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Errorf("stream must end with 'data: [DONE]', got: %s", respBody)
	}
	// The bogus error frame must NOT appear.
	if strings.Contains(respBody, `"error":"backend request failed"`) {
		t.Errorf("mid-stream error must not emit error frame, got: %s", respBody)
	}
}
func TestCompletionsHandler_StreamErrorBeforeFirstChunk(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	backend := newMockBackend()
	backend.SetStreamError(errors.New("backend request failed: context deadline exceeded"), true)
	server.SetBackend(backend, "gpt-4")

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

	// Must return 504 (timeout) with JSON body, not SSE.
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("expected status %d, got %d", http.StatusGatewayTimeout, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}
	if strings.Contains(w.Body.String(), "data:") {
		t.Errorf("error-before-chunk must not emit SSE frame, got: %s", w.Body.String())
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response must be valid JSON: %v", err)
	}
	if errResp["error"] == "" {
		t.Errorf("expected error message, got: %v", errResp)
	}
}
func TestCompletionsHandler_StreamErrorMidStream(t *testing.T) {
	server := NewServer(testLogger(), "", security.Config{DevMode: true})
	backend := newMockBackend()
	backend.SetStreamError(errors.New("backend request failed"), false)
	server.SetBackend(backend, "gpt-4")

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

	// Must return 200 (stream started) and end with [DONE].
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Errorf("stream must end with 'data: [DONE]', got: %s", respBody)
	}
	// The bogus error frame must NOT appear.
	if strings.Contains(respBody, `"error":"backend request failed"`) {
		t.Errorf("mid-stream error must not emit error frame, got: %s", respBody)
	}
}

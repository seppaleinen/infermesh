package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// streamOpenAI is a generic helper for streaming OpenAI-compatible responses.
// It handles the idle timeout, request building, SSE parsing, and chunk delivery.
// TReq is the request type (ChatRequest or CompletionRequest).
// TChunk is the chunk type (ChatChunk or CompletionChunk).
func streamOpenAI[TReq any, TChunk any](
	ctx context.Context,
	b *OpenAICompatibleBackend,
	model string,
	req TReq,
	buildReq func(TReq, string, int) map[string]interface{},
	unmarshalChunk func([]byte) (TChunk, error),
	doneChunk TChunk,
	urlSuffix string,
	fallbackMaxTokens int,
) (<-chan TChunk, <-chan error) {
	chunkCh := make(chan TChunk, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		idleTimeout, idle := createIdleTimeout(ctx, b)
		defer idle.stop()

		mutateRequestStream(&req)
		url := b.baseURL + urlSuffix
		maxTokens := determineMaxTokens(&req, b, fallbackMaxTokens)
		backendReq := buildReq(req, model, maxTokens)

		resp, err := executeHTTPRequest(b, idle, idleTimeout, url, backendReq)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
			return
		}

		parseSSEStream(chunkCh, errCh, resp.Body, idle, idleTimeout, unmarshalChunk, doneChunk)
	}()

	return chunkCh, errCh
}

func createIdleTimeout(ctx context.Context, b *OpenAICompatibleBackend) (time.Duration, *idleTimeout) {
	idleTimeout := b.client.Timeout
	if idleTimeout <= 0 {
		idleTimeout = defaultBackendTimeout
	}
	idle := newIdleTimeout(ctx, idleTimeout)
	return idleTimeout, idle
}

func mutateRequestStream[TReq any](req *TReq) {
	switch r := any(req).(type) {
	case *protocol.ChatRequest:
		r.Stream = true
	case *protocol.CompletionRequest:
		r.Stream = true
	}
}

func determineMaxTokens[TReq any](req *TReq, b *OpenAICompatibleBackend, fallbackMaxTokens int) int {
	maxTokens := 0
	switch r := any(req).(type) {
	case *protocol.ChatRequest:
		maxTokens = r.MaxTokens
	case *protocol.CompletionRequest:
		maxTokens = r.MaxTokens
	}
	if maxTokens <= 0 {
		if b.maxTokens > 0 {
			maxTokens = b.maxTokens
		} else {
			maxTokens = fallbackMaxTokens
		}
	}
	return maxTokens
}

func executeHTTPRequest(
	b *OpenAICompatibleBackend,
	idle *idleTimeout,
	idleTimeout time.Duration,
	url string,
	backendReq map[string]interface{},
) (*http.Response, error) {
	jsonBody, err := json.Marshal(backendReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(idle.ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.streamClient.Do(httpReq)
	if err != nil {
		// If the request failed because idle.ctx was cancelled (idle
		// timeout fired before first byte), classify it as a timeout.
		if idle.ctx.Err() != nil {
			return nil, fmt.Errorf("backend stream idle timeout: no response for %v", idleTimeout)
		}
		return nil, fmt.Errorf("backend request failed: %w", err)
	}
	return resp, nil
}

func parseSSEStream[TChunk any](
	chunkCh chan<- TChunk,
	errCh chan<- error,
	body io.ReadCloser,
	idle *idleTimeout,
	idleTimeout time.Duration,
	unmarshalChunk func([]byte) (TChunk, error),
	doneChunk TChunk,
) {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		idle.reset(idleTimeout)

		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "\r" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataStr := strings.TrimPrefix(line, "data: ")
			if dataStr == "[DONE]" {
				return
			}

			chunk, err := unmarshalChunk([]byte(dataStr))
			if err != nil {
				slog.Warn("failed to parse SSE chunk", "error", err)
				continue
			}

			select {
			case chunkCh <- chunk:
			case <-idle.ctx.Done():
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if idle.ctx.Err() != nil {
			errCh <- fmt.Errorf("backend stream idle timeout: no data for %v", idleTimeout)
			return
		}
		errCh <- fmt.Errorf("failed to read SSE stream: %w", err)
		return
	}

	// Send [DONE] marker to indicate stream completion
	chunkCh <- doneChunk
}

func buildChatRequest(req protocol.ChatRequest, model string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model":       model,
		"messages":    req.Messages,
		"stream":      true,
		"max_tokens":  maxTokens,
		"temperature": req.Temperature,
	}
}

func unmarshalChatChunk(data []byte) (protocol.ChatChunk, error) {
	var chunk protocol.ChatChunk
	err := json.Unmarshal(data, &chunk)
	return chunk, err
}

func buildCompletionRequest(req protocol.CompletionRequest, model string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model":       model,
		"prompt":      req.Prompt,
		"stream":      true,
		"max_tokens":  maxTokens,
		"temperature": req.Temperature,
	}
}

func unmarshalCompletionChunk(data []byte) (protocol.CompletionChunk, error) {
	var chunk protocol.CompletionChunk
	err := json.Unmarshal(data, &chunk)
	return chunk, err
}

func doneChatChunk(model string) protocol.ChatChunk {
	return protocol.ChatChunk{
		ID:      "",
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []protocol.Choice{},
	}
}

func doneCompletionChunk(model string) protocol.CompletionChunk {
	return protocol.CompletionChunk{
		ID:      "",
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []protocol.Choice{},
	}
}

// StreamChat generates a chat completion stream via the backend.
func (b *OpenAICompatibleBackend) StreamChat(ctx context.Context, model string, req protocol.ChatRequest) (<-chan protocol.ChatChunk, <-chan error) {
	return streamOpenAI[protocol.ChatRequest, protocol.ChatChunk](
		ctx, b, model, req,
		buildChatRequest,
		unmarshalChatChunk,
		doneChatChunk(model),
		"/v1/chat/completions",
		100,
	)
}

// StreamCompletions generates a completion via the backend with streaming support.
func (b *OpenAICompatibleBackend) StreamCompletions(ctx context.Context, model string, req protocol.CompletionRequest) (<-chan protocol.CompletionChunk, <-chan error) {
	return streamOpenAI[protocol.CompletionRequest, protocol.CompletionChunk](
		ctx, b, model, req,
		buildCompletionRequest,
		unmarshalCompletionChunk,
		doneCompletionChunk(model),
		"/v1/completions",
		100,
	)
}
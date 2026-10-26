package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// streamSSE drives the shared streaming select loop used by chatCompletions
// and completions (issue #161). The two loops were byte-for-byte identical
// except for the chunk type (protocol.ChatChunk vs protocol.CompletionChunk);
// this single generic implementation replaces both.
//
// T is the chunk type. extract reports the chunk's Usage field (nil-safe) so
// the clean-close path can record accurate token counts; json.Marshal handles
// both concrete types. The loop writes OpenAI-compatible SSE frames to w
// until the chunk channel closes, ctx is cancelled, or a backend error
// arrives. It returns the number of chunks flushed and the final Usage (if
// any) so the caller can record throughput.
//
// s provides the logging, error-writing and throughput-recording hooks; it is
// passed explicitly (rather than as a method receiver) so the helper can be
// generic: Go does not permit generic methods on concrete types.
func streamSSE[T any](
	ctx context.Context,
	s *Server,
	w http.ResponseWriter,
	flusher http.Flusher,
	chunkCh <-chan T,
	errCh <-chan error,
	model string,
	extract func(T) *protocol.Usage,
) {
	started := false
	var firstChunkAt time.Time
	var chunkCount int
	var finalUsage *protocol.Usage
	for {
		if checkAndHandlePendingError(s, w, errCh, started) {
			return
		}

		if done := handleChunkOrClose(ctx, s, w, flusher, chunkCh, errCh, model, &started, &firstChunkAt, &chunkCount, &finalUsage, extract); done {
			return
		}
	}
}

// deriveStatusCode extracts the HTTP status code from an error for concurrency tracking.
// Returns 0 for non-BackendHTTPError errors, http.StatusOK for nil error.
func deriveStatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var bhErr *BackendHTTPError
	if errors.As(err, &bhErr) {
		return bhErr.StatusCode
	}
	return 0
}

// checkAndHandlePendingError checks errCh for a buffered backend error.
// Returns true if an error was handled and the stream should terminate.
func checkAndHandlePendingError(s *Server, w http.ResponseWriter, errCh <-chan error, started bool) bool {
	var pendingErr error
	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			pendingErr = err
		}
		// ok == false: errCh closed without error — keep waiting for chunks.
	default:
	}
	if pendingErr != nil {
		s.writeStreamBackendError(w, pendingErr, started)
		return true
	}
	return false
}

// handleChunkOrClose processes the next chunk from chunkCh or handles stream close.
// Returns true if the stream has terminated (either by close, error, or context cancellation).
func handleChunkOrClose[T any](
	ctx context.Context,
	s *Server,
	w http.ResponseWriter,
	flusher http.Flusher,
	chunkCh <-chan T,
	errCh <-chan error,
	model string,
	started *bool,
	firstChunkAt *time.Time,
	chunkCount *int,
	finalUsage **protocol.Usage,
	extract func(T) *protocol.Usage,
) bool {
	select {
	case chunk, ok := <-chunkCh:
		if !ok {
			// The chunk channel closed. The real backends close errCh
			// before chunkCh and buffer the error before either close, so
			// check errCh one last time to catch an error that raced with
			// the close.
			select {
			case err, ok := <-errCh:
				if ok && err != nil {
					s.writeStreamBackendError(w, err, *started)
					return true
				}
			default:
			}
			if !*started {
				ensureSSEHeaders(w)
			}
			// Clean close: record inference throughput for scheduler
			// ranking (issue #134). TTFT is the time from request start
			// to the first flushed chunk; total is the wall-clock
			// generation time. When the backend's final frame carried a
			// Usage, use it; otherwise fall back to chunk count as a
			// token estimate (one completion token per content chunk).
			if *started {
				s.recordStreamingInference(model, *finalUsage, *chunkCount, *firstChunkAt)
			}
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return true
		}
		if !*started {
			ensureSSEHeaders(w)
			*started = true
			*firstChunkAt = time.Now()
		}
		// Capture the final-frame usage (OpenAI sends usage only on the
		// last streaming chunk), so the clean-close path can record
		// accurate token counts.
		if u := extract(chunk); u != nil {
			*finalUsage = u
		}
		*chunkCount++
		jsonChunk, err := json.Marshal(chunk)
		if err != nil {
			s.log.Error("failed to marshal chunk", "error", err)
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return true
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(jsonChunk))
		flusher.Flush()
		return false
	case <-ctx.Done():
		return true
	}
}

// chatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Parse request
	var req protocol.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse chat request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Store in history for debugging (capped at maxChatHistory to prevent unbounded growth)
	const maxChatHistory = 1000
	s.chatHistory = append(s.chatHistory, req)
	if len(s.chatHistory) > maxChatHistory {
		s.chatHistory = s.chatHistory[len(s.chatHistory)-maxChatHistory:]
	}

	// Check if backend is configured
	if s.backend == nil {
		s.log.Error("backend not configured")
		http.Error(w, "backend not configured", http.StatusInternalServerError)
		return
	}

	// Check circuit breaker state
	if !s.backend.IsHealthy() {
		if s.concurrencyEstimator != nil {
			s.concurrencyEstimator.RecordCircuitOpen()
		}
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Non-streaming: return the full response as application/json.
	// dispatchNonStreaming handles the shared backend-call → record → encode
	// skeleton; the only per-endpoint differences are the response type T and
	// how its Usage is extracted (issue #161).
	if !req.Stream {
		if s.dispatchNonStreaming(ctx, w, req.Model, func(c context.Context, m string) (protocol.ChatResponse, error) {
			return s.backend.CompleteChat(c, m, req)
		}, func(r protocol.ChatResponse) *protocol.Usage { return r.Usage }) {
			return
		}
	}

	// Streaming: forward ChatChunks as OpenAI-compatible SSE frames.
	// Headers are set lazily on the first flushed chunk so that a backend
	// error arriving before any chunk can still produce a proper JSON error
	// response (see writeStreamBackendError).
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	chatCh, errCh := s.backend.StreamChat(ctx, req.Model, req)
	streamSSE[protocol.ChatChunk](ctx, s, w, flusher, chatCh, errCh, req.Model, func(c protocol.ChatChunk) *protocol.Usage { return c.Usage })
}

// dispatchNonStreaming drives the shared non-streaming dispatch path used by
// chatCompletions and completions (issue #161). The two handlers were
// byte-for-byte identical except for the response type (protocol.ChatResponse
// vs protocol.CompletionResponse) and how that type's Usage is extracted; this
// single generic implementation replaces both. It writes the response on
// success and returns true so the caller can return; on any failure it has
// already written the error response and returns false.
func (s *Server) dispatchNonStreaming[T any](
	ctx context.Context,
	w http.ResponseWriter,
	model string,
	complete func(context.Context, string) (T, error),
	extractUsage func(T) *protocol.Usage,
) bool {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := complete(ctx, model)
	elapsed := time.Since(start)

	if s.concurrencyEstimator != nil {
		statusCode := deriveStatusCode(err)
		s.concurrencyEstimator.RecordRequest(start, statusCode)
	}
	if err != nil {
		s.log.Error("backend error", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	// Record inference throughput for scheduler ranking (issue #134).
	// For non-streaming responses the whole generation is one shot, so
	// TTFT == total elapsed.
	s.recordInference(model, extractUsage(resp), elapsed, elapsed)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode response", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	return true
}

// completions handles the /v1/completions HTTP endpoint.
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	// Parse request
	var req protocol.CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse completion request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Check if backend is configured
	if s.backend == nil {
		s.log.Error("backend not configured")
		http.Error(w, "backend not configured", http.StatusInternalServerError)
		return
	}

	// Check circuit breaker state
	if !s.backend.IsHealthy() {
		if s.concurrencyEstimator != nil {
			s.concurrencyEstimator.RecordCircuitOpen()
		}
		s.log.Error("backend is unhealthy")
		http.Error(w, "backend is unhealthy", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Non-streaming: return the full response as application/json.
	if !req.Stream {
		if s.dispatchNonStreaming(ctx, w, req.Model, func(c context.Context, m string) (protocol.CompletionResponse, error) {
			return s.backend.CompleteCompletions(c, m, req)
		}, func(r protocol.CompletionResponse) *protocol.Usage { return r.Usage }) {
			return
		}
	}

	// Streaming: forward CompletionChunks as OpenAI-compatible SSE frames.
	// Headers are set lazily on the first flushed chunk so that a backend
	// error arriving before any chunk can still produce a proper JSON error
	// response (see writeStreamBackendError).
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	compCh, errCh := s.backend.StreamCompletions(ctx, req.Model, req)
	streamSSE[protocol.CompletionChunk](ctx, s, w, flusher, compCh, errCh, req.Model, func(c protocol.CompletionChunk) *protocol.Usage { return c.Usage })
}

// modelsList handles the /v1/models HTTP endpoint.
func (s *Server) modelsList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.modelsMu.Lock()
	data := make([]protocol.ModelInfo, len(s.models))
	copy(data, s.models)
	s.modelsMu.Unlock()
	// make([]T, n) is never nil, so no nil-guard is needed here; an empty
	// s.models yields an empty (non-nil) slice, which JSON encodes as [].
	response := protocol.ModelsResponse{Object: "list", Data: data}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		http.Error(w, "failed to encode models", http.StatusInternalServerError)
	}
}

// loadModelByName marks a declared model as loaded, mirroring the HTTP
// /v1/models/load handler. It is used by the WebSocket model_load_request
// dispatch; the next heartbeat carries the updated models so the router's
// capability cache sees the load.
func (s *Server) loadModelByName(name string) (bool, string) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	for i, m := range s.models {
		if m.Name == name {
			s.models[i].Loaded = true
			return true, ""
		}
	}
	return false, "model not found"
}

// loadModelHandler handles the /v1/models/load HTTP endpoint.
func (s *Server) loadModelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	found := false
	s.modelsMu.Lock()
	for i, m := range s.models {
		if m.Name == req.Model {
			found = true
			// Update loaded flag – simple in-memory state change.
			s.models[i].Loaded = true
			break
		}
	}
	s.modelsMu.Unlock()

	if !found {
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{"status": "loaded"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

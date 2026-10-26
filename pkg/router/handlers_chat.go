package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// handleChatCompletions handles the /v1/chat/completions HTTP endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleInference(w, r, "chat", parseChatRequest, s.resolveAutoModelChat, s.proxyChatStream)
}

// handleCompletions handles the /v1/completions HTTP endpoint.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleInference(w, r, "completion", parseCompletionRequest, s.resolveAutoModelCompletion, s.proxyCompletionStream)
}

// handleInference is the shared skeleton for the chat and completions
// endpoints. It factors out the parse → registry check → auto-alias
// resolve → alias resolve → marshal → stream/non-stream dispatch path so
// the two endpoints cannot drift (issue #162).
//
// T is a pointer to one of protocol.ChatRequest or protocol.CompletionRequest
// (both implement protocol.InferenceRequest via pointer receivers). The
// parse/resolveAuto/streamDispatch callbacks are endpoint-specific — they
// carry the concrete request type and the streaming proxy function;
// everything else is shared.
func (s *Server) handleInference[T protocol.InferenceRequest](
	w http.ResponseWriter,
	r *http.Request,
	kind string,
	parse func(*http.Request) (T, error),
	resolveAuto func(T) (string, error),
	streamDispatch func(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req T, reqID string),
) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	// Parse request
	req, err := parse(r)
	if err != nil {
		s.log.Error("failed to parse "+kind+" request", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Check if scheduler is configured
	if s.reg == nil {
		s.log.Error("registry not configured")
		writeErrorResponse(w, http.StatusInternalServerError, "registry not configured", "server_error", "internal_error")
		return
	}

	// Resolve the model="auto" alias (if enabled) to a concrete tiered
	// model before marshal & dispatch. Non-auto models are returned
	// unchanged, preserving existing routing behavior.
	if model, err := resolveAuto(req); err != nil {
		s.log.Error("auto routing failed", "error", err)
		writeErrorResponse(w, http.StatusServiceUnavailable, err.Error(), "server_error", "auto_routing_error")
		return
	} else {
		req.SetModel(model)
	}

	// Resolve any user-facing model alias to its canonical name after the
	// "auto" alias so aliases can target tiered models too.
	req.SetModel(s.resolveAliasModel(req.GetModel()))

	// Marshal the request body once for dispatch.
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal "+kind+" request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}

	if req.GetStream() {
		// Streaming: fail fast, no failover (cannot undo partial SSE output).
		worker, err := s.selectWorker(req.GetModel())
		if err != nil {
			s.log.Error("failed to select worker", "error", err)
			writeErrorResponse(w, http.StatusServiceUnavailable, "no workers", "server_error", "no_workers")
			return
		}
		if s.counter != nil {
			s.counter.Record(req.GetModel())
		}
		if s.hub != nil && s.hub.isRateLimited(worker.ID) {
			s.hub.recordRejection(worker.ID, 429)
			writeRateLimitError(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Record this dispatch for the request-trace endpoint (issue #156).
		// The record is finished inside proxyStream on every return path.
		reqID := ""
		if s.tracker != nil {
			reqID = s.tracker.Begin(req.GetModel(), worker.ID, worker.Hostname, kind)
		}
		streamDispatch(w, r, worker, req, reqID)
		return
	}

	// Non-streaming: dispatch with failover to a different worker on
	// transport/5xx/429 errors. The response is buffered until the first
	// successful attempt so nothing is written to the client until then.
	resp, err := s.dispatchWithFailover(r.Context(), req.GetModel(), body, kind, s.maxAttempts, s.retryBudget)
	if err != nil {
		s.writeFailoverError(w, req.GetModel(), err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// parseChatRequest decodes a /v1/chat/completions request body.
func parseChatRequest(r *http.Request) (*protocol.ChatRequest, error) {
	var req protocol.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// parseCompletionRequest decodes a /v1/completions request body.
func parseCompletionRequest(r *http.Request) (*protocol.CompletionRequest, error) {
	var req protocol.CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// proxyChatStream proxies a chat completion request to a worker.
func (s *Server) proxyChatStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req *protocol.ChatRequest, reqID string) {
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal chat request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}
	s.proxyStream(w, r, s.clientFor(worker), worker, "chat", body, reqID)
}

// proxyCompletionStream proxies a completion request to a worker.
func (s *Server) proxyCompletionStream(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req *protocol.CompletionRequest, reqID string) {
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal completion request", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "marshal_error")
		return
	}
	s.proxyStream(w, r, s.clientFor(worker), worker, "completion", body, reqID)
}

// proxyStream relays an inference request to the worker via the transport
// appropriate for it and streams the response back to the client as SSE.
//
// The dispatch is bounded by an **idle timeout** (proxyTimeout): the timer
// resets on every chunk received, so a slow-but-healthy stream that keeps
// producing tokens is never killed; only prolonged silence (no chunk for
// proxyTimeout) triggers the timeout. The first-byte wait is also bounded by
// the same value: if the backend doesn't answer within that window, the idle
// timer fires and the request is aborted.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, client WorkerClient, worker protocol.WorkerInfo, kind string, body []byte, reqID string) {
	// Idle timeout for streaming proxy: abort if no chunk arrives for this
	// long. The timer resets on every chunk received, so active streaming
	// is not bounded by this value. First-byte wait is also bounded by it.
	timeout := s.proxyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Request-trace lifecycle (issue #156). The record is marked "streaming"
	// once the first chunk is written to the client and finished on every
	// return path: a normal completion is "done", any error/timeout/cancel
	// is "error". A deferred finish keeps all ~6 return paths in one place.
	streamCompleted := false
	if s.tracker != nil && reqID != "" {
		defer func() {
			status := StatusError
			if streamCompleted {
				status = StatusDone
			}
			s.tracker.Finish(reqID, status)
		}()
	}

	// Start the idle timer before the dispatch so model loading (no bytes
	// yet) still has a budget equal to the configured timeout.
	idleTimer := time.NewTimer(timeout)
	defer idleTimer.Stop()

	chunks, errs := client.Stream(ctx, worker, kind, body)
	flusher, _ := w.(http.Flusher)
	started := false
	sawDone := false

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				// Channel closed without an explicit Done sentinel (e.g. the
				// sentinel was dropped when the queue was full). Terminate the
				// SSE stream so clients don't wait forever.
				if started && !sawDone {
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
				}
				streamCompleted = true
				return
			}
			// Reset the idle timer on every chunk received (including the
			// Done sentinel handling — stop the timer before returning).
			// The canonical Stop+drain+Reset pattern: if Stop returns false
			// the timer already fired, so the value is in the channel and
			// the blocking drain is guaranteed to make progress.
			if !idleTimer.Stop() {
				<-idleTimer.C
			}
			idleTimer.Reset(timeout)

			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				started = true
				if s.tracker != nil && reqID != "" {
					s.tracker.SetStatus(reqID, StatusStreaming)
				}
			}
			sawDone = sawDone || chunk.Done
			_, _ = w.Write(chunk.Data)
			if flusher != nil {
				flusher.Flush()
			}
			if chunk.Done {
				streamCompleted = true
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if !started {
				var herr *WorkerHTTPError
				if errors.As(err, &herr) {
					s.log.Error("worker returned error", "worker", worker.ID, "status", herr.StatusCode, "body", herr.Body)
					writeErrorResponse(w, herr.StatusCode, "worker error", "server_error", "worker_error")
				} else if errors.Is(err, errTimeout) {
					s.log.Error("request timed out after retries", "worker", worker.ID, "timeout", timeout)
					client.RecordTimeout(worker.ID)
					writeErrorResponse(w, http.StatusGatewayTimeout, "request timeout", "server_error", "timeout_error")
				} else {
					s.log.Error("failed to connect to worker after retries", "error", err)
					writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
				}
				return
			}
			s.log.Warn("stream interrupted", "worker", worker.ID, "error", err)
			if !sawDone {
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		case <-idleTimer.C:
			// Idle timeout fired: no chunk arrived for `timeout`. Cancel the
			// worker-side request so the underlying transport is aborted.
			cancel()
			s.log.Warn("stream idle timeout", "worker", worker.ID, "idle", timeout)
			if !started {
				writeErrorResponse(w, http.StatusGatewayTimeout, "request timeout", "server_error", "timeout_error")
			} else if !sawDone {
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		case <-r.Context().Done():
			// Client hung up: just return. The derived ctx is already
			// cancelled via cancel(), so the worker request is aborted.
			return
		}
	}
}

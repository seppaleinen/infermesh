package router

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// ErrorResponse represents an OpenAI-compatible error response.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail represents the error detail in an OpenAI-compatible error response.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeErrorResponse writes an OpenAI-compatible error response.
func writeErrorResponse(w http.ResponseWriter, statusCode int, errMsg string, errType string, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := ErrorResponse{
		Error: ErrorDetail{
			Message: errMsg,
			Type:    errType,
			Code:    errCode,
		},
	}

	if err := json.NewEncoder(w).Encode(errorResp); err != nil {
		slog.Error("failed to encode error response", "error", err)
	}
}

// writeRateLimitError writes an OpenAI-shaped 429 rate-limit response with a
// Retry-After header. It is written BEFORE SSE headers are flushed so the
// client sees a clean JSON error rather than a half-open SSE stream.
func writeRateLimitError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)

	errorResp := ErrorResponse{
		Error: ErrorDetail{
			Message: "rate limit exceeded: max in-flight calls reached for this worker",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	}

	if err := json.NewEncoder(w).Encode(errorResp); err != nil {
		slog.Error("failed to encode rate limit error response", "error", err)
	}
}

// wrapInferenceEndpoints wraps the given handler so that inference endpoints
// (/v1/chat/completions, /v1/completions) require a valid API key while
// other endpoints pass through unchanged.
func (s *Server) wrapInferenceEndpoints(next http.Handler, apiKey string) http.Handler {
	// required mirrors the worker's wrapInferenceEndpoints: a key is only
	// enforced when one is actually configured. Without this, passing an
	// empty apiKey would enforce a key against "" and reject every request.
	apiKeyMW := security.NewAPIKeyMiddleware(apiKey, apiKey != "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions", "/v1/completions", "/meta/aliases", "/meta/auto-routing":
			apiKeyMW(next).ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

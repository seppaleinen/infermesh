package worker

import (
	"time"
)

// NewBackendFromName creates the backend adapter for the given backend name.
// Unknown names fall back to the llama-cpp backend (preserving the historical
// CLI behavior of the old createBackend helper). backendURL overrides the
// adapter's base URL when non-empty (used for mocks and custom endpoints).
// timeout is the HTTP client timeout for backend requests; a zero or
// negative value falls back to the default 30s. maxTokens is the default
// max_tokens to send to the backend when the incoming request omits it;
// zero means "no default" (the backend falls back to its built-in default).
// threads is the llama-cpp thread count (default 4); only consulted for
// the llama-cpp backend (issue #139).
func NewBackendFromName(backendName, modelPath, backendURL string, timeout time.Duration, maxTokens, threads int) (Backend, error) {
	// Resolve the default endpoint for each backend; --backend-url overrides
	// it (the llama-cpp default http://localhost:8080 collides with the
	// router port, so tests and custom setups pass an explicit URL).
	defaultURL := ""
	switch backendName {
	case "llama-cpp":
		defaultURL = "http://localhost:8080"
	case "ollama":
		defaultURL = "http://localhost:11434"
	case "lmstudio":
		defaultURL = "http://127.0.0.1:1234"
	case "vllm":
		defaultURL = "http://localhost:8000"
	case "custom":
		defaultURL = "http://localhost:8000"
	default:
		defaultURL = "http://localhost:8080"
	}
	if backendURL != "" {
		defaultURL = backendURL
	}
	// threads defaults to 4 (the historical hardcoded value) when not
	// specified or set to a non-positive value.
	if threads < 1 {
		threads = 4
	}
	switch backendName {
	case "llama-cpp":
		return NewLlamaCppBackend(defaultURL, 2048, threads, 1, timeout, maxTokens), nil
	case "ollama":
		return NewOllamaBackend(defaultURL, modelPath, timeout, maxTokens), nil
	case "lmstudio":
		return NewLMStudioBackend(defaultURL, modelPath, timeout, maxTokens), nil
	case "vllm":
		return NewVLLMBackend(defaultURL, modelPath, timeout, maxTokens), nil
	case "custom":
		return NewCustomBackend(defaultURL, "", "custom", timeout, maxTokens), nil
	default:
		return NewLlamaCppBackend(defaultURL, 2048, threads, 1, timeout, maxTokens), nil
	}
}

// backendPort returns the default port of the named backend's HTTP server.
// Unknown backend names fall back to llama-cpp's port (matching
// NewBackendFromName).
func backendPort(backend string) int {
	switch backend {
	case "ollama":
		return 11434
	case "lmstudio":
		return 1234
	case "vllm", "custom":
		return 8000
	default: // llama-cpp and unknown names (llama-cpp fallback)
		return 8080
	}
}

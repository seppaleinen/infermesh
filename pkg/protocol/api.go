// Package protocol defines the shared InferMesh wire protocol: the
// OpenAI-compatible request/response shapes carried over HTTP and WebSocket,
// plus the mDNS discovery payloads and the WebSocket envelope.
//
// Wire-protocol types MUST live in exactly one place (see
// docs/adr/0004-code-quality-ceilings.md). Both pkg/router and pkg/worker
// import this package rather than redefining the shapes locally, so a
// single wire-format change propagates without drift.
package protocol

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest represents a request to the chat completions endpoint.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

// CompletionRequest represents a request to the completions endpoint.
type CompletionRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Stream      bool    `json:"stream"`
}

// Choice represents a single choice in a completion response.
//
// Delta mirrors the OpenAI streaming shape: mid-stream chunks carry token
// increments in choices[i].delta.content rather than choices[i].message.
// Backends that only emit the first-frame message form (or the legacy
// choices[i].text form) leave Delta empty; omitempty keeps both shapes
// wire-compatible for non-streaming responses.
type Choice struct {
	Index        int        `json:"index"`
	Message      ChatMessage `json:"message,omitempty"`
	Delta        ChatMessage `json:"delta,omitempty"`
	Text         string     `json:"text,omitempty"`
	FinishReason string     `json:"finish_reason"`
}

// CompletionChunk represents a streaming chunk of a completion response.
type CompletionChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model,omitempty"`
	Choices []Choice `json:"choices"`
	// Usage is the final-frame token usage, present only on the last
	// streaming chunk when the backend reports it (issue #134). It is
	// populated by the OpenAI-compatible backend's final SSE frame.
	Usage *Usage `json:"usage,omitempty"`
}

// ChatChunk represents a streaming chunk of a chat completion response.
type ChatChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model,omitempty"`
	Choices []Choice `json:"choices"`
	// Usage is the final-frame token usage, present only on the last
	// streaming chunk when the backend reports it (issue #134). It is
	// populated by the OpenAI-compatible backend's final SSE frame.
	Usage *Usage `json:"usage,omitempty"`
}

// ModelsResponse represents the response from the models endpoint.
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ChatResponse represents a chat completion response from a backend.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// CompletionResponse represents a completion response from a backend.
type CompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Usage represents token usage statistics.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// InferenceRequest is implemented by every wire-protocol request type the
// router dispatches (ChatRequest, CompletionRequest). It lets the router's
// shared inference handler operate on either type through a single code path
// rather than duplicating the parse → resolve → marshal → dispatch skeleton
// (issue #162). The getters/setters are additive and do not affect JSON
// marshaling, so existing wire payloads are unchanged.
type InferenceRequest interface {
	GetModel() string
	SetModel(string)
	GetStream() bool
}

// GetModel returns the request's model name.
func (r ChatRequest) GetModel() string { return r.Model }

// SetModel sets the request's model name.
func (r *ChatRequest) SetModel(model string) { r.Model = model }

// GetStream returns the request's stream flag.
func (r ChatRequest) GetStream() bool { return r.Stream }

// GetModel returns the request's model name.
func (r CompletionRequest) GetModel() string { return r.Model }

// SetModel sets the request's model name.
func (r *CompletionRequest) SetModel(model string) { r.Model = model }

// GetStream returns the request's stream flag.
func (r CompletionRequest) GetStream() bool { return r.Stream }
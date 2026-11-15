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
//
// Reasoning carries the model's internal chain-of-thought when the backend
// emits it under the OpenAI-compatible "reasoning_content" key (LM Studio
// reasoning models, DeepSeek-R1-style backends). It is omitted when empty so
// non-reasoning responses stay wire-compatible. Without this field the
// unmarshal→marshal round-trip through the worker silently strips it, and
// reasoning-only models (e.g. gpt-oss-20b, which emits reasoning tokens but
// an empty content delta) arrive at the client as an empty response (issue
// #204).
type ChatMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content,omitempty"`
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
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message,omitempty"`
	Delta        ChatMessage `json:"delta,omitempty"`
	Text         string      `json:"text,omitempty"`
	FinishReason string      `json:"finish_reason"`
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

// TokenDetails carries a breakdown of tokens by sub-category within a usage
// block. OpenAI-compatible backends emit these under "completion_tokens_details"
// (e.g. reasoning_tokens) and "prompt_tokens_details" (e.g. cached_tokens).
//
// Both fields are nil when the backend omits them, so non-reasoning responses
// stay wire-compatible: the zero-value Usage marshals without the details key
// entirely. Reasoning models (LM Studio gpt-oss-20b, DeepSeek-R1-style
// backends) report reasoning_tokens here; without these fields the worker's
// unmarshal→marshal round-trip silently drops them and clients cannot see
// reasoning-token cost (issue #204 follow-up).
type TokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	CachedTokens    int `json:"cached_tokens,omitempty"`
}

// Usage represents token usage statistics.
type Usage struct {
	PromptTokens            int           `json:"prompt_tokens"`
	CompletionTokens        int           `json:"completion_tokens"`
	TotalTokens             int           `json:"total_tokens"`
	PromptTokensDetails     *TokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *TokenDetails `json:"completion_tokens_details,omitempty"`
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

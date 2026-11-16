// Package protocol defines the shared InferMesh wire protocol: the
// OpenAI-compatible request/response shapes carried over HTTP and WebSocket,
// plus the mDNS discovery payloads and the WebSocket envelope.
//
// Wire-protocol types MUST live in exactly one place (see
// docs/adr/0004-code-quality-ceilings.md). Both pkg/router and pkg/worker
// import this package rather than redefining the shapes locally, so a
// single wire-format change propagates without drift.
package protocol

// FunctionSpec describes a single tool's callable signature, matching the
// OpenAI "function" tool shape: {type:"function", function:{name,
// description, parameters}}. Parameters is an arbitrary JSON Schema object.
type FunctionSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// Tool wraps a FunctionSpec in the OpenAI "tool" envelope, where Type is
// almost always "function" (the only value the OpenAI API accepts today).
// Type is omitempty so a zero-value Tool still marshals as
// {"type":"function","function":{...}} only when Type is explicitly set;
// most clients omit it and backends default it to "function".
type Tool struct {
	Type     string       `json:"type,omitempty"`
	Function FunctionSpec `json:"function"`
}

// FunctionCall carries a tool invocation returned by the model: the tool
// name and its arguments as a **string-encoded JSON** object (this is the
// OpenAI wire contract — arguments is never a raw object, always a string).
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall represents a single structured tool invocation in a chat message,
// matching the OpenAI response shape. In non-streaming responses the full
// ToolCall appears inside message.tool_calls with id+type+function; in
// streaming deltas the same shape appears inside choices[i].delta.tool_calls
// where the first chunk of a call carries id+index+function.name and
// subsequent chunks carry only index+function.arguments fragments. Index is
// the position within the delta's tool_calls array and is omitted when zero
// (the array position IS the index, so it is redundant for the first call).
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Index    int          `json:"index,omitempty"`
	Function FunctionCall `json:"function"`
}

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
//
// ToolCalls carries structured tool invocations returned by the backend.
// ToolCallID + Name identify a tool result message (role:"tool") the client
// sends back to feed a tool's output into the conversation. All three are
// omitted when empty so plain-chat messages stay wire-compatible: without
// these fields the worker's unmarshal→marshal round-trip silently strips
// tool_calls, and tool-using models (e.g. ornith-1.5-35b via LM Studio)
// emit their invocations as literal text inside content instead of as
// structured calls (tool-call pass-through, issue #31 follow-up).
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ChatRequest represents a request to the chat completions endpoint.
//
// Tools, ToolChoice and ParallelToolCalls carry the OpenAI tool-calling
// request shape. All three are omitted when absent so plain-chat requests
// marshal byte-identically (no new null/empty keys). They are forwarded
// verbatim to the backend by the worker's request builders; the router
// passes the whole body through, so no router-side handling is needed.
type ChatRequest struct {
	Model             string        `json:"model"`
	Messages          []ChatMessage `json:"messages"`
	Stream            bool          `json:"stream"`
	MaxTokens         int           `json:"max_tokens"`
	Temperature       float64       `json:"temperature"`
	Tools             []Tool        `json:"tools,omitempty"`
	ToolChoice        any           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
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

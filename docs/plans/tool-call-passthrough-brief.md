# Research Brief — Tool-call pass-through (OpenAI-compatible tool calling)

**Status:** APPROVED (user sign-off 2026-11-16). Symptom: calling `ornith-1.5-35b`
(LM Studio backend on this machine) through infermesh "goes through", but tool
calls are printed as literal text in `content` instead of being executed. The
same model called directly behaves correctly.

## Root cause (verified in code)

The wire protocol has **no tool-calling support at all** — every tool-related
field is silently dropped by unmarshal→marshal round-trips at two hops:

1. **Request drop (client → router → worker → backend):**
   - `protocol.ChatRequest` (`pkg/protocol/api.go:28-34`) has only
     `Model/Messages/Stream/MaxTokens/Temperature`. A client's `tools` and
     `tool_choice` are ignored on unmarshal.
   - The router re-marshals the (stripped) request to the worker
     (`pkg/router/handlers_chat.go:146`, `:157` via `json.Marshal(req)`), so the
     worker never sees tools either.
   - The worker builds the backend request as a map with only
     `model/messages/stream/max_tokens/temperature`
     (`pkg/worker/openai.go:184-190` `completeChatInternal`;
     `pkg/worker/openai_stream.go:188` `buildChatRequest` for streaming).
   - Result: the backend receives **no tool definitions**, so the model emits
     tool invocations as plain text inside `content` ("printed as is").
2. **Response drop (backend → worker → client):**
   - Worker decodes each chunk/response into `protocol.ChatChunk`/`ChatResponse`
     → `Choice.Message`/`Delta` are `ChatMessage{Role, Content, Reasoning}`
     (`pkg/protocol/api.go:21-25`). There is no `ToolCalls` field, so
     structured `tool_calls` are stripped before the worker writes SSE back.
   - The router pipes worker SSE raw (no further decoding), so the damage is
     done at the worker.

This is the same class of bug as `reasoning_content` (issue #204) — a missing
`ChatMessage`/request field in `pkg/protocol/api.go` — but on the tool-calling
fields.

## OpenAI wire shapes to match

- Request: `tools: [{type:"function", function:{name, description, parameters}}]`
  where `parameters` is an arbitrary JSON Schema (use `any`).
  `tool_choice`: string (`"auto"|"none"|"required"`) **or** object
  `{type:"function", function:{name:"..."}}` → type as `any`.
  `parallel_tool_calls: bool` (optional; use `*bool`).
- Response message: `tool_calls: [{id, type:"function",
  function:{name, arguments}}]`; `arguments` is a **string-encoded JSON**.
- Streaming deltas: `choices[i].delta.tool_calls` carries partials — first chunk
  of a call: `id` + `index` + `function.name`; subsequent chunks: `index` +
  `function.arguments` fragments (no id/name). `finish_reason:"tool_calls"` on
  the final chunk.
- Tool result message from client: `role:"tool"` with `tool_call_id` + `content`
  (+ optional `name`).

## Scope

1. **`pkg/protocol/api.go`** — add wire types (single source of truth per
   `docs/adr/0004-code-quality-ceilings.md`):
   - `Tool{Type string, Function FunctionSpec}` and
     `FunctionSpec{Name, Description string, Parameters any}` (all omitempty where
     sensible).
   - `ToolCall{ID, Type string, Index int, Function FunctionCall}` —
     `FunctionCall{Name, Arguments string}`; `Index` omitempty (absent in
     non-streaming responses); `Type` default `"function"` when present.
   - `ChatRequest`: `Tools []Tool`, `ToolChoice any`, `ParallelToolCalls *bool`
     — all `omitempty`.
   - `ChatMessage`: `ToolCalls []ToolCall`, `ToolCallID string`, `Name string` —
     all `omitempty`.
2. **`pkg/worker/openai.go`** `completeChatInternal` — when `req.Tools` is
   non-empty add `tools` to the backend map; likewise `tool_choice`,
   `parallel_tool_calls` when set. Non-streaming responses already round-trip
   once `ChatMessage.ToolCalls` exists.
3. **`pkg/worker/openai_stream.go`** `buildChatRequest` — same additions.
4. **Tests** (mirror the `TestStreamChatPreservesReasoningContent` pattern):
   - `pkg/protocol`: JSON round-trip — request with `tools`/`tool_choice`
     (both string and object forms)/`parallel_tool_calls` survives; response
     with `tool_calls` (including streaming delta partials across chunks:
     first chunk id+name, later chunks arguments fragments) survives;
     tool-role message with `tool_call_id` survives. Zero-value shapes stay
     wire-compatible (no `tools`/`tool_calls` keys for plain chats).
   - `pkg/worker`: streaming regression test — mock SSE with a
     `tool_calls` delta sequence + `finish_reason:"tool_calls"`; assert the
     emitted chunks preserve id/name/arguments fragments per index. Non-streaming
     test — mock backend returning a `tool_calls` response; assert preserved.
   - Existing tests (`openai_test.go`, `server_handlers_test.go`,
     `ws_client_test.go`, router tests) must stay green — plain-chape requests
     must marshal byte-identically (no new keys when tools absent).

## Out of scope

- Router-side awareness of tool calling for scheduling (none needed — pass-through).
- Function/agent execution — infermesh is a proxy; clients execute tools.
- `app/` desktop module (separate Go module, untouched).
- Completion endpoint (tools are chat-only in the OpenAI API).

## Constraints

- Go 1.27, deps stdlib + `hashicorp/mdns` + `yaml.v3` only; no new deps.
- golangci-lint v2 (0 issues); `gofmt -l` clean (pre-push hook enforces gofmt).
- Working tree ends clean, **committed locally only — do NOT push to origin**
  (user handles pushes).
- Verification before handover: `go test ./pkg/...` green, `make lint` green,
  `gofmt -l .` empty. If new tests use `-race`-sensitive concurrency, run
  `go test -race` on the touched packages.
- Keep the "single wire-protocol location" ADR: no local redefinition of tool
  shapes in router/worker.

## Expected output

Full pipeline handover: STATUS, SUMMARY, RATIONALE, TECHNICAL PAYLOAD
(including LAST_COMMIT, files changed, test evidence), TRACE, PIPELINE STAGE,
REWORK_COUNT.

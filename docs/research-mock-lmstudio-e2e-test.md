# Research Brief: In-Process Mock-LM-Studio E2E Test

## Task
Write a test that starts the router and worker **in-process** (no binary spawning, no mDNS), injects a **mock LM Studio backend** that returns a normal OpenAI chat-completion response, and verifies the **full request/response path**: client → router → worker (mock backend) → router → client.

## Domain
`dev` — pure application source change (new test file, zero production code changes expected).

## Refined Requirements

- **Objective:** Prove that a chat-completion request sent to the router's `/v1/chat/completions` endpoint is proxied to a registered worker, the worker's mock backend produces a valid OpenAI-compatible response, and the router returns it to the client.
- **Scope:**
  - One new test (or a small set of subtests) in the existing test tree.
  - In-process: both router and worker HTTP servers start on `:0` (ephemeral ports).
  - Mock backend: a struct implementing `worker.Backend` that returns a canned `ChatResponse` (normal, non-streaming, OpenAI-shaped JSON).
  - Dev mode: `security.Config{DevMode: true}` — no mTLS, no API keys.
  - No mDNS: use either `RegisterLoop` (HTTP POST to `/v1/dev/register`) or direct `registry.HandleEvent` seeding.
- **Definition of Done:**
  - Test passes under `go test ./...`.
  - No production source files modified (test-only change).
  - Test covers: registration, capability discovery, model selection, proxy round-trip, response validation.
  - Runs in < 5s (no long timeouts or sleeps).

## Web Findings
- N/A — this is an internal codebase task, no external library/chart identification needed.
- The "mock LM Studio" pattern is standard: implement the `Backend` interface, return a canned `ChatResponse`. No third-party mock server library needed (stdlib `httptest` + in-process Go).

## App Source Findings

### Key injection seams (all exist today, zero source changes needed)

| Seam | File | Signature | Notes |
|------|------|-----------|-------|
| Worker server | `pkg/worker/server.go:183` | `NewServer(log *slog.Logger, addr string, cfg security.Config) *Server` | `addr` is injectable — pass `:0`. |
| Set backend | `pkg/worker/server.go:199` | `(s *Server) SetBackend(backend Backend, model string)` | Accepts any `Backend` impl. Bypasses hardcoded `http://127.0.0.1:1234` in `cmd/worker/main.go`. |
| Set models | `pkg/worker/server.go:232` | `(s *Server) SetModels(models []protocol.ModelInfo)` | Marks models as loaded for the capability cache. |
| Router server | `pkg/router/server.go:79-86` | `NewServer(reg registry.Registry, log *slog.Logger, addr string, cfg security.Config) *Server` | `addr` injectable — pass `:0`. |
| Registry | `pkg/registry/registry.go:28-50` | `registry.New(cfg, log)` + `reg.HandleEvent(protocol.DiscoveryEvent{...})` | Direct seeding works; no discovery listener needed. |
| Capability cache | `pkg/router/capability_cache.go` | `cache.Update(worker protocol.WorkerInfo)` | Fetches `/capabilities` from the worker, hydrates model list. |
| HTTP registration | `pkg/worker/register.go` | `RegisterLoop(ctx, routerBase string, info protocol.WorkerInfo, interval time.Duration, log) error` | POSTs to `/v1/dev/register` immediately on start. |
| Backend interface | `pkg/worker/backend_integration.go` | `Backend` interface: `CompleteChat`, `CompleteCompletions`, `IsHealthy`, `HealthCheck`, `StreamChat`, `StreamCompletions`, `LoadModel`, `UnloadModel`, `ListModels`, `GetMetrics`, `GetCircuitState` | The mock must implement all methods. |

### Existing mockBackend (reuse or model after)

`pkg/worker/server_test.go:28-80` already has a `mockBackend` struct:

```go
type mockBackend struct {
    models      []protocol.ModelInfo
    healthCheck error
}

func (m *mockBackend) CompleteChat(ctx context.Context, model string, req ChatRequest) (ChatResponse, error) {
    return ChatResponse{
        ID:      "chatcmpl-test",
        Object:  "chat.completion",
        Created: time.Now().Unix(),
        Choices: []Choice{{
            Index:        0,
            Message:      Message{Role: "assistant", Content: "Mock response"},
            FinishReason: "stop",
        }},
    }, nil
}
```

- It implements the **full** `Backend` interface (all 11+ methods).
- It returns a **non-streaming** `ChatResponse` (single JSON body, not SSE chunks).
- It is **not exported** (package-internal `worker` package) — a new test in a different package would need its own mock, or the test must live in `pkg/worker` / `pkg/router`.

### Request/response flow (what the test exercises)

```
Client (httptest or net/http)
  → POST /v1/chat/completions to Router (addr :0)
    → Router.handleChatCompletions()
      → Router.selectWorker(model)          // reads CapabilityCache
      → Router.proxyChatStream(w, r, worker, req)
        → POST http://workerIP:workerPort/v1/chat/completions  (real HTTP)
          → Worker.chatCompletions()
            → backend.CompleteChat(ctx, model, req)   // mockBackend
            → SSE: data: {ChatResponse JSON}\n\ndata: [DONE]\n\n
        → Router relays SSE to client
  ← Client receives SSE stream
```

### Existing test patterns (conventions to follow)

1. **`pkg/router/server_test.go`** — in-process, `:0` ports, `testRegistry` helper, `httptest` for assertions, `security.Config{DevMode: true}`. Uses `httptest.NewServer` as a **mock worker** (not the real worker server). This is the closest existing pattern.

2. **`tests/e2e/discovery_test.go`** — spawns real binaries via `exec.Command`, polls `waitForHTTP`. Heavier, slower, tests mDNS. Not the right pattern for this task.

3. **`tests/integration_security/security_test.go`** — `httptest` with TLS, cert generation. Not relevant to the happy-path mock test.

### Two viable test architectures

**Option A: Real worker server + real router server (full-stack in-process)**
```go
// Worker
wServer := worker.NewServer(log, ":0", security.Config{DevMode: true})
wServer.SetBackend(mockBackend, "gpt-oss-20b")
wServer.SetModels([]protocol.ModelInfo{{Name: "gpt-oss-20b", Loaded: true}})
// Start wServer, get its addr (wServer.Addr() after start)

// Router
reg := registry.New(regCfg, log)
rServer := router.NewServer(reg, log, ":0", security.Config{DevMode: true})
// Start rServer

// Register worker via HTTP
workerInfo := protocol.WorkerInfo{ID: "w1", IP: "127.0.0.1", Port: workerPort, ...}
worker.RegisterLoop(ctx, rServer.URL, workerInfo, 5*time.Second, log)
// Wait for cache.Update to complete

// Client
resp := http.Post(rServer.URL+"/v1/chat/completions", ..., chatReqJSON)
// Assert SSE response contains "Mock response"
```
- **Pros:** Exercises the real worker HTTP handler, `SetBackend`, `RegisterLoop`, `CapabilityCache.Update` — closest to production.
- **Cons:** More moving parts; need to handle async registration + cache hydration timing.

**Option B: Router server + httptest mock worker (lighter)**
```go
// httptest.NewServer as the "worker" (serves /capabilities and /v1/chat/completions)
workerServer := httptest.NewServer(handler)
// Parse port, build WorkerInfo, seed registry, update cache
// Start router, send request, assert
```
- **Pros:** Simpler, faster, no real worker server lifecycle.
- **Cons:** Doesn't exercise the real worker HTTP handler or `SetBackend` — the mock is a raw `http.HandlerFunc`, not a `worker.Backend` impl.

### Recommended: **Option A** (full-stack in-process)

The user's intent is to mock **LM Studio** (the backend), not the worker. Option A exercises the real worker server with a mock backend, which is the faithful test. Option B mocks the worker itself, which is a different (lighter) test.

## Infra Findings
N/A — no GitOps / cluster work.

## Open Questions (BLOCK — `ask` tool unavailable in subagent)

> These need human decisions before the test can be written.

| # | Question | Recommended answer | Why it matters |
|---|----------|--------------------|----------------|
| 1 | **Where does the test live?** | New file `tests/integration/mock_backend_test.go` (package `integration`), or extend `pkg/router/server_test.go` | Affects package boundaries and whether `mockBackend` can be reused directly. If in `pkg/worker`, the existing `mockBackend` is accessible. If in a new `tests/integration/` package, a new mock is needed. |
| 2 | **Option A or Option B?** | Option A (real worker server + real router, mock backend) | Matches the stated goal: "mock LM Studio response" = mock the backend, not the worker. |
| 3 | **Streaming vs non-streaming assertion?** | Assert the **non-streaming** path first (`stream: false` in the request). The worker currently falls back to non-streaming even when `stream: true` (see `worker/server.go:416-421`). | The router always responds as SSE regardless. The test should parse SSE and validate the embedded JSON. |
| 4 | **Registration path: `RegisterLoop` (HTTP) or `HandleEvent` (direct)?** | `RegisterLoop` — exercises the real HTTP registration path. | More faithful to production. `HandleEvent` is simpler but skips the registration HTTP call. |
| 5 | **Should the test cover `stream: true` too?** | No — add a subtest later if needed. The current code falls back to non-streaming for `stream: true`. | Keeps the test focused. |
| 6 | **Is the goal regression (validate existing code) or to add a feature?** | Regression — validate that the existing proxy path works end-to-end with a known-good backend. | No new production code is expected. |
| 7 | **Should the mock backend return a more realistic LM Studio-shaped response (e.g., real model name, token usage)?** | Yes — use `model: "gpt-oss-20b"`, include `usage: {prompt_tokens, completion_tokens, total_tokens}` if the `ChatResponse` struct supports it. | Makes the test more representative. |

## Remaining Risks

- **Async timing:** `RegisterLoop` POSTs immediately, but `CapabilityCache.Update` is triggered by the registry event subscription. There is a race: the router may receive the registration before the cache has hydrated. Mitigation: poll/wait for the worker to appear in `cache.List()` with a short timeout (e.g., 2s).
- **`mockBackend` package boundary:** The existing `mockBackend` is in `pkg/worker` (unexported). If the test lives in `tests/integration/`, a new mock struct is needed (~30 lines, copy-paste from existing).
- **`Addr()` timing:** `worker.NewServer(...).Start(ctx)` is async; the actual bound address is only known after `ListenAndServe` starts. Need a helper that returns the resolved address (similar to `httptest.NewServer` which blocks until the server is ready).
- **SSE parsing:** The router relays SSE from the worker. The test must parse `data: ...` lines from the SSE stream and validate the JSON payload. A small helper is needed (or use `bufio.Scanner`).

## Definition of Done (restated)

1. `go test ./...` passes with the new test.
2. The test starts a real `worker.Server` with a mock `Backend` and a real `router.Server`, both on `:0`.
3. The worker registers via HTTP (`RegisterLoop`) in dev mode.
4. The router's `CapabilityCache` hydrates from the worker's `/capabilities`.
5. A `POST /v1/chat/completions` to the router returns an SSE stream containing a valid OpenAI `ChatResponse` with the mock content.
6. No production source files are modified.
7. Test runs in < 5s.

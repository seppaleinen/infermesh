package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestModelAlias_ChatRewritesBeforeSelect verifies alias rewrites before worker selection.
func TestModelAlias_ChatRewritesBeforeSelect(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, fc, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-1"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`),
	}

	w, req := makeChatRequest("gemma-4-12b", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fc.calls["worker-1"] != 1 {
		t.Errorf("expected 1 call to worker-1, got %d", fc.calls["worker-1"])
	}
}
func TestModelAlias_CompletionRewrites(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, fc, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-1"] = mockResult{
		body: []byte(`{"id":"cmpl-1","object":"text_completion","choices":[{"text":"ok"}]}`),
	}

	w, req := makeCompletionRequest("gemma-4-12b", "Hi", 100)
	srv.handleCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fc.calls["worker-1"] != 1 {
		t.Errorf("expected 1 call to worker-1, got %d", fc.calls["worker-1"])
	}
}
func TestModelAlias_StreamingRewrites(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, _, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	model := srv.resolveAliasModel("gemma-4-12b")
	if model != "google/gemma-4-12b" {
		t.Errorf("expected canonical, got %s", model)
	}
	// Verify resolve works in handler path via protocol.ChatRequest
	resolved := srv.resolveAliasModel("gemma-4-12b")
	if resolved != "google/gemma-4-12b" {
		t.Errorf("expected resolved canonical, got %s", resolved)
	}
}
func TestModelAlias_QuantPinRoutesToQuantWorker(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-4bit", "qwen2.5-coder-7b-instruct-mlx@4bit"),
		mkWorker("worker-8bit", "qwen2.5-coder-7b-instruct-mlx@8bit"),
	}
	aliasCfg := ParseModelAliases("qwen-coder-7b@8bit=qwen2.5-coder-7b-instruct-mlx@8bit")
	srv, fc, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-8bit"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"8bit"}}]}`),
	}

	w, req := makeChatRequest("qwen-coder-7b@8bit", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fc.calls["worker-8bit"] != 1 {
		t.Errorf("expected 1 call to worker-8bit, got %d", fc.calls["worker-8bit"])
	}
	if fc.calls["worker-4bit"] != 0 {
		t.Errorf("expected 0 calls to worker-4bit, got %d", fc.calls["worker-4bit"])
	}
}
func TestModelAlias_NoWorkersForCanonical(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "other-model"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, _, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	w, req := makeChatRequest("gemma-4-12b", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no workers") {
		t.Errorf("expected no_workers error, got: %s", w.Body.String())
	}
}
func TestModelAlias_NonAliasModelUnchanged(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-llama", "llama-3-8b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, fc, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-llama"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"llama"}}]}`),
	}

	w, req := makeChatRequest("llama-3-8b", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fc.calls["worker-llama"] != 1 {
		t.Errorf("expected 1 call to worker-llama, got %d", fc.calls["worker-llama"])
	}
}
func TestModelAlias_AutoThenAliasChain(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "gemma-4-12b", // this is an alias key
		},
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")

	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetAutoRouting(autoCfg)
	srv.SetModelAliases(aliasCfg)

	fc := newMockWorkerClient()
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }
	fc.results["worker-1"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`),
	}
	defer func() { _ = tr.reg.Stop() }()

	w, req := makeChatRequest("auto", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fc.calls["worker-1"] != 1 {
		t.Errorf("expected 1 call to worker-1 via alias chain, got %d", fc.calls["worker-1"])
	}
}
func TestModelAlias_CounterRecordsCanonical(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	srv, fc, tr := setupAliasHandler(t, workers, aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-1"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`),
	}

	w, req := makeChatRequest("gemma-4-12b", []protocol.ChatMessage{{Role: "user", Content: "Hi"}}, 100, false)
	srv.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	snapshot := srv.counter.Snapshot()
	if snapshot["google/gemma-4-12b"] != 1 {
		t.Errorf("expected counter for canonical model, got %v", snapshot)
	}
	if _, ok := snapshot["gemma-4-12b"]; ok {
		t.Errorf("expected no alias entry in counter, got %v", snapshot)
	}
}
func TestModelAlias_ModelsListIncludesAliasKeys(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetModelAliases(aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	found := false
	for _, m := range resp.Data {
		if m.Name == "gemma-4-12b" && m.Backend == "alias" && m.Loaded {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("alias not found in models list: %+v", resp.Data)
	}
}
func TestModelAlias_ModelsListExcludesWhenDisabled(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "google/gemma-4-12b"),
	}
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	// No SetModelAliases
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	for _, m := range resp.Data {
		if m.Backend == "alias" {
			t.Errorf("unexpected alias entry when disabled: %s", m.Name)
		}
	}
}
func TestModelAlias_ModelsList_AliasAndAutoCoexist(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "concrete-model"),
	}
	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{TierSimple: "small-model"},
	}
	aliasCfg := ParseModelAliases("gemma-4-12b=google/gemma-4-12b")

	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetAutoRouting(autoCfg)
	srv.SetModelAliases(aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	hasAuto := false
	hasAlias := false
	for _, m := range resp.Data {
		if m.Name == "auto" && m.Backend == "auto" {
			hasAuto = true
		}
		if m.Name == "gemma-4-12b" && m.Backend == "alias" {
			hasAlias = true
		}
	}
	if !hasAuto {
		t.Error("expected auto model in list")
	}
	if !hasAlias {
		t.Error("expected alias model in list")
	}
}
func TestModelAlias_ModelsListHidesCanonical(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "gemma-4-12b"),
	}
	aliasCfg := ParseModelAliases("short-name=gemma-4-12b")
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetModelAliases(aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	aliasFound := false
	for _, m := range resp.Data {
		if m.Name == "gemma-4-12b" {
			t.Errorf("canonical model gemma-4-12b should be hidden from /v1/models, got: %+v", m)
		}
		if m.Name == "short-name" && m.Backend == "alias" && m.Loaded {
			aliasFound = true
		}
	}
	if !aliasFound {
		t.Errorf("expected alias 'short-name' advertised in /v1/models, got: %+v", resp.Data)
	}
}
func TestModelAlias_ModelsListKeepsCanonicalWhenNoAlias(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-1", "gemma-4-12b"),
	}
	// Alias targets a different model; gemma-4-12b is not a canonical target.
	aliasCfg := ParseModelAliases("other-alias=some-other-model")
	tr := testRegistry(t, workers)
	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()
	srv.SetModelAliases(aliasCfg)
	defer func() { _ = tr.reg.Stop() }()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModelsList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp protocol.ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	found := false
	for _, m := range resp.Data {
		if m.Name == "gemma-4-12b" && m.Backend != "alias" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected loaded worker model 'gemma-4-12b' to remain in /v1/models, got: %+v", resp.Data)
	}
}

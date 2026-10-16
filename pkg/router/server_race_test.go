//go:build race

package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestServerAutoRouting_Race exercises concurrent writes to SetAutoRouting
// while HTTP handlers read the config. This test must be run with -race.
func TestServerAutoRouting_Race(t *testing.T) {
	// Create a minimal server with no workers — we only care about the
	// /v1/models endpoint reading autoRouting.
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple:  "small-model",
			TierMedium:  "medium-model",
			TierComplex: "large-model",
		},
		SimpleMaxTokens: 200,
		MediumMaxTokens: 600,
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer goroutine: repeatedly call SetAutoRouting with nil / non-nil.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				if i%2 == 0 {
					srv.SetAutoRouting(autoCfg)
				} else {
					srv.SetAutoRouting(nil)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader goroutines: repeatedly call handleModelsList.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-done:
					return
				default:
					req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					w := httptest.NewRecorder()
					srv.handleModelsList(w, req)
					if w.Code != http.StatusOK {
						t.Errorf("handleModelsList failed with status %d", w.Code)
					}
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// Run for 500ms, then stop.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestServerScorerWeights_Race exercises concurrent writes to SetScorerWeights
// while ScorerWeights() reads. This test must be run with -race.
func TestServerScorerWeights_Race(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer goroutine: repeatedly call SetScorerWeights with varying values.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-done:
				return
			default:
				i++
				srv.SetScorerWeights(
					float64(i%10)/10.0,
					float64((i+1)%10)/10.0,
					float64((i+2)%10)/10.0,
					float64((i+3)%10)/10.0,
					float64((i+4)%10)/10.0,
					0, // loadedModelBonus
					0, // coldLoadPenalty
					5+(i%10),
				)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader goroutines: repeatedly call ScorerWeights().
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = srv.ScorerWeights()
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// Run for 500ms, then stop.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestServerAutoRouting_RaceConcurrentReads exercises multiple concurrent
// readers (resolveAutoModelChat + handleModelsList) while a writer mutates.
func TestServerAutoRouting_RaceConcurrentReads(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
		SimpleMaxTokens: 200,
		MediumMaxTokens: 600,
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer: toggle config on/off.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				if i%2 == 0 {
					srv.SetAutoRouting(autoCfg)
				} else {
					srv.SetAutoRouting(nil)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader 1: resolveAutoModelChat.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &ChatRequest{
			Model:    "auto",
			Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		}
		for {
			select {
			case <-done:
				return
			default:
				_, _ = srv.resolveAutoModelChat(req)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader 2: handleModelsList.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				httpReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				w := httptest.NewRecorder()
				srv.handleModelsList(w, httpReq)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader 3: resolveAutoModelCompletion.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &CompletionRequest{
			Model:  "auto",
			Prompt: "hello",
		}
		for {
			select {
			case <-done:
				return
			default:
				_, _ = srv.resolveAutoModelCompletion(req)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Run for 500ms, then stop.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestServerModelAliases_Race exercises concurrent writes to SetModelAliases
// while handleModelsList and resolveAliasModel read. This mirrors the
// autoRouting race test but for the already-fixed modelAliases field.
func TestServerModelAliases_Race(t *testing.T) {
	srv := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})

	aliasCfg := &ModelAliasConfig{
		Aliases: map[string]AliasTarget{
			"gemma-4b": {Canonical: "google/gemma-4b"},
			"qwen-c":   {Canonical: "qwen2.5-coder-7b"},
		},
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer: toggle config on/off.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				if i%2 == 0 {
					srv.SetModelAliases(aliasCfg)
				} else {
					srv.SetModelAliases(nil)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader 1: handleModelsList.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				w := httptest.NewRecorder()
				srv.handleModelsList(w, req)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader 2: resolveAliasModel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = srv.resolveAliasModel("gemma-4b")
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Run for 500ms, then stop.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestServerAutoRouting_IntegrationRace exercises the full request path
// (handleChatCompletions) with concurrent autoRouting mutations. This is a
// more realistic integration-style race test that exercises the entire
// read path from HTTP handler through resolveAutoModelChat.
func TestServerAutoRouting_IntegrationRace(t *testing.T) {
	// Build a minimal server with a fake worker.
	workers := []protocol.WorkerInfo{
		{
			ID:       "race-worker",
			Hostname: "race-worker",
			IP:       "127.0.0.1",
			Port:     8081,
			Status:   protocol.StatusAvailable,
			Version:  "v1",
			Capabilities: protocol.Capabilities{
				Models: []protocol.ModelInfo{
					{Name: "small-model", Quantization: "Q4_K_M", Loaded: true},
				},
				VRAM: protocol.MemoryInfo{TotalMB: 24576, FreeMB: 20480},
			},
		},
	}
	tr := testRegistry(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	srv := tr.Server()
	srv.cache = NewCapabilityCache(tr.reg, testLogger())
	srv.cache.Start(tr.ctx)
	srv.cache.mu.Lock()
	for _, w := range workers {
		srv.cache.cache[w.ID] = w
	}
	srv.cache.mu.Unlock()

	// Mock worker client that returns immediately.
	fc := newMockWorkerClient()
	fc.results["race-worker"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`),
	}
	srv.clientFactory = func(w protocol.WorkerInfo) WorkerClient { return fc }

	autoCfg := &AutoRoutingConfig{
		Models: map[ComplexityTier]string{
			TierSimple: "small-model",
		},
		SimpleMaxTokens: 200,
		MediumMaxTokens: 600,
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer: toggle autoRouting config.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				if i%2 == 0 {
					srv.SetAutoRouting(autoCfg)
				} else {
					srv.SetAutoRouting(nil)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Reader: full chat completion requests with model="auto".
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]interface{}{
				"model":      "auto",
				"messages":   []ChatMessage{{Role: "user", Content: "hi"}},
				"max_tokens": 50,
				"stream":     false,
			}
			bodyBytes, _ := json.Marshal(body)
			for {
				select {
				case <-done:
					return
				default:
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bodyBytes))
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					srv.handleChatCompletions(w, req)
					// We don't assert status here — the request may legitimately
					// fail when autoRouting is nil (no "auto" model configured).
					// The race detector will catch unsynchronized access.
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// Run for 500ms, then stop.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

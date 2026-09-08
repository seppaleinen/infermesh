package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"crypto/x509"
	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/registry"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// Server is the HTTP server for the router.
type Server struct {
	log      *slog.Logger
	registry registry.Registry
	cache    *CapabilityCache
	server   *http.Server
	addr     string
	scheduler scheduler.ScoringAlgorithm
	unavailableTTL time.Duration
	config     security.Config
}

// NewServer creates a new router HTTP server.
func NewServer(reg registry.Registry, log *slog.Logger, addr string, cfg security.Config) *Server {
	cache := NewCapabilityCache(reg, log)
	scorer := scheduler.DefaultScoringAlgorithm()
	return &Server{
		log:            log,
		registry:       reg,
		cache:          cache,
		addr:           addr,
		scheduler:      scorer,
		unavailableTTL: scorer.GetUnavailableTTL(),
		config:         cfg,
	}
}

// Start runs the HTTP server and starts the capability cache.
func (s *Server) Start(ctx context.Context) error {
		s.cache.Start(ctx)

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/workers", s.listWorkers)
	mux.HandleFunc("/v1/workers/", s.getWorker)
	mux.HandleFunc("/v1/capabilities", s.listCapabilities)

	// Dev-only: allow workers to self-register via HTTP (macOS mDNS multicast broken)
	if security.IsDevMode(s.config) {
		mux.HandleFunc("/v1/dev/register", s.handleDevRegister)
	}

	// OpenAI-compatible proxy endpoints
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	mux.HandleFunc("/v1/models", s.handleModelsList)

	// Apply mTLS middleware if in production mode
	var handler http.Handler = mux
	if !security.IsDevMode(s.config) {
		// Prod mode: enforce mTLS
		caCertPool := x509.NewCertPool()
		// Load CA certificate from CertDir, not from router's own leaf cert
		if s.config.CertDir != "" {
			caCertPath := filepath.Join(s.config.CertDir, "ca.crt")
			caCertBytes, err := os.ReadFile(caCertPath)
			if err == nil {
				if caCert, err := x509.ParseCertificate(caCertBytes); err == nil {
					caCertPool.AddCert(caCert)
				}
			}
		}
		middleware := security.NewMTLSMiddleware(caCertPool, true, "worker-", "gpu-worker")
		handler = middleware(mux)
	}

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: handler,
	}

	s.log.Info("router server starting", "addr", s.addr, "dev_mode", security.IsDevMode(s.config))

	go func() {
		<-ctx.Done()
		s.server.Shutdown(context.Background())
	}()

	if security.IsDevMode(s.config) {
		return s.server.ListenAndServe()
	}

	return s.server.ListenAndServeTLS(s.config.MTLSCert, s.config.MTLSKey)
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string {
	if s.server != nil {
		return s.server.Addr
	}
	return s.addr
}

// Cache returns the capability cache for testing.
func (s *Server) Cache() *CapabilityCache {
	return s.cache
}

// Stop stops the router server.
func (s *Server) Stop() error {
	if s.server != nil {
		return s.server.Shutdown(context.Background())
	}
	return nil
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers := s.cache.List()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(workers); err != nil {
		s.log.Error("failed to encode workers", "error", err)
		http.Error(w, "failed to encode workers", http.StatusInternalServerError)
	}
}

func (s *Server) getWorker(w http.ResponseWriter, r *http.Request) {
	// Extract worker ID from path: /v1/workers/{id}
	id := r.URL.Path[len("/v1/workers/"):]

	worker, ok := s.cache.Get(id)
	if !ok {
		http.Error(w, "worker not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(worker); err != nil {
		s.log.Error("failed to encode worker", "error", err)
		http.Error(w, "failed to encode worker", http.StatusInternalServerError)
	}
}

func (s *Server) listCapabilities(w http.ResponseWriter, r *http.Request) {
	workers := s.cache.List()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(workers); err != nil {
		s.log.Error("failed to encode capabilities", "error", err)
		http.Error(w, "failed to encode capabilities", http.StatusInternalServerError)
	}
}

// --- OpenAI-compatible types ---

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest represents a request to the chat completions endpoint.
type ChatRequest struct {
	Model       string       `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool         `json:"stream"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
}

// CompletionRequest represents a request to the completions endpoint.
type CompletionRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	Stream      bool    `json:"stream"`
}

// Message represents a message in the chat completion response.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Choice represents a single choice in a completion response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	Text         string  `json:"text,omitempty"`
	FinishReason string  `json:"finish_reason"`
}

// CompletionChunk represents a streaming chunk of a completion response.
type CompletionChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

// ModelsResponse represents the response from the models endpoint.
type ModelsResponse struct {
	Object string               `json:"object"`
	Data   []protocol.ModelInfo `json:"data"`
}

// --- OpenAI-compatible handlers ---

// handleChatCompletions handles POST /v1/chat/completions
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse chat request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.log.Info("chat completions request", "model", req.Model, "stream", req.Stream)

	// Select worker using scheduler
	selectedWorker, err := s.selectWorkerForModel(r.Context(), req.Model)
	if err != nil {
		s.log.Error("failed to select worker", "error", err, "model", req.Model)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	s.log.Info("selected worker", "worker", selectedWorker.WorkerInfo.ID, "reason", selectedWorker.Reason)

	// Look up full worker info from cache
	worker, ok := s.cache.Get(selectedWorker.WorkerInfo.ID)
	if !ok {
		http.Error(w, "worker not found", http.StatusNotFound)
		return
	}

	// Forward request to selected worker
	s.proxyToWorker(w, r, worker, req, "/v1/chat/completions")
}

// handleCompletions handles POST /v1/completions
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse completion request", "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.log.Info("completions request", "model", req.Model, "stream", req.Stream)

	// Select worker using scheduler
	selectedWorker, err := s.selectWorkerForModel(r.Context(), req.Model)
	if err != nil {
		s.log.Error("failed to select worker", "error", err, "model", req.Model)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	s.log.Info("selected worker", "worker", selectedWorker.WorkerInfo.ID, "reason", selectedWorker.Reason)

	// Look up full worker info from cache
	worker, ok := s.cache.Get(selectedWorker.WorkerInfo.ID)
	if !ok {
		http.Error(w, "worker not found", http.StatusNotFound)
		return
	}

	// Forward request to selected worker
	s.proxyToWorker(w, r, worker, req, "/v1/completions")
}

// handleModelsList handles GET /v1/models
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Aggregate models from all available workers
	workers := s.cache.List()
	modelMap := make(map[string]protocol.ModelInfo)

	for _, worker := range workers {
		for _, model := range worker.Capabilities.Models {
			if model.Loaded {
				// Only include loaded models, prefer first occurrence
				if _, exists := modelMap[model.Name]; !exists {
					modelMap[model.Name] = model
				}
			}
		}
	}

	models := make([]protocol.ModelInfo, 0, len(modelMap))
	for _, model := range modelMap {
		models = append(models, model)
	}

	w.Header().Set("Content-Type", "application/json")
	response := ModelsResponse{Object: "list", Data: models}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models list", "error", err)
		http.Error(w, "failed to encode models", http.StatusInternalServerError)
	}
}

// selectWorkerForModel selects the best worker for a given model using the scheduler.
func (s *Server) selectWorkerForModel(ctx context.Context, modelName string) (*scheduler.SelectedWorker, error) {
	workers := s.cache.List()
	if len(workers) == 0 {
		return nil, fmt.Errorf("no workers available")
	}

	// Convert protocol.WorkerInfo to scheduler.WorkerInfo
	schedulerWorkers := make([]scheduler.WorkerInfo, 0, len(workers))
	for _, w := range workers {
		// Only consider available workers
		if w.Status != protocol.StatusAvailable {
			continue
		}
		schedulerWorkers = append(schedulerWorkers, scheduler.WorkerInfo{
			ID:          w.ID,
			Addr:        fmt.Sprintf("%s:%d", w.IP, w.Port),
			Models:      w.Capabilities.Models,
			VRAMTotalMB: int(w.Capabilities.VRAM.TotalMB),
			VRAMFreeMB:  int(w.Capabilities.VRAM.FreeMB),
			GPUUtilPct:  0, // Not tracked in current capability cache
			QueueDepth:  0, // Not tracked in current capability cache
			AvgLatencyMS: 0, // Not tracked in current capability cache
			LastHeartbeat: w.LastSeen,
		})
	}

	if len(schedulerWorkers) == 0 {
		return nil, fmt.Errorf("no available workers")
	}

	request := scheduler.ModelRequest{
		Model: modelName,
	}

	selected, err := scheduler.SelectWorker(ctx, s.scheduler, request, schedulerWorkers, s.scheduler.GetUnavailableTTL())
	if err != nil {
		return nil, err
	}

	return selected, nil
}

// proxyToWorker forwards the request to the selected worker and streams the response back.
func (s *Server) proxyToWorker(w http.ResponseWriter, r *http.Request, worker protocol.WorkerInfo, req interface{}, endpoint string) {
	workerURL := fmt.Sprintf("http://%s:%d%s", worker.IP, worker.Port, endpoint)

	// Marshal the request
	body, err := json.Marshal(req)
	if err != nil {
		s.log.Error("failed to marshal request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create proxy request
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, workerURL, bytes.NewReader(body))
	if err != nil {
		s.log.Error("failed to create proxy request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("Content-Type", "application/json")

	// Send request to worker
	client := &http.Client{
		Timeout: 0, // No timeout for streaming
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		s.log.Error("failed to proxy request to worker", "worker", worker.ID, "error", err)
		http.Error(w, "worker unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.log.Error("failed to stream response", "error", err)
		return
	}
}

// handleDevRegister handles POST /v1/dev/register for dev-mode worker self-registration.
// Workers call this when mDNS multicast is unavailable (e.g. macOS).
func (s *Server) handleDevRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(r.Body).Decode(&worker); err != nil {
		s.log.Error("failed to decode dev register request", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	// Validate: non-empty ID
	if worker.ID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "worker id is required"})
		return
	}

	// Validate: port in range
	if worker.Port < 1 || worker.Port > 65535 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid port"})
		return
	}

	// Validate: IP must be loopback (dev-only safety)
	ip := net.ParseIP(worker.IP)
	if ip == nil || !ip.IsLoopback() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "dev registration only accepts loopback addresses"})
		return
	}

	// Force dev-mode state
	worker.Status = protocol.StatusAvailable
	worker.LastSeen = time.Now()

	event := protocol.DiscoveryEvent{
		Type:   protocol.EventUpdated,
		Worker: worker,
		Time:   time.Now(),
	}

	if err := s.registry.HandleEvent(event); err != nil {
		s.log.Error("failed to handle dev register event", "worker_id", worker.ID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to register"})
		return
	}

	s.log.Info("worker registered via dev endpoint", "worker_id", worker.ID, "ip", worker.IP, "port", worker.Port)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(worker)
}
package router

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// PopularModelInfo represents a model with popularity signals for the
// /meta/models/popular endpoint.
type PopularModelInfo struct {
	Model             string `json:"model"`
	WorkerCount       int    `json:"worker_count"`
	LoadedWorkerCount int    `json:"loaded_worker_count"`
	CallCount         int    `json:"call_count"`
}

// PopularModelsResponse is the response from the /meta/models/popular endpoint.
type PopularModelsResponse struct {
	Models []PopularModelInfo `json:"models"`
}

// SetAliasesRequest is the body for POST /meta/aliases. The caller sends the
// FULL alias map — not a delta — so the router replaces atomically.
type SetAliasesRequest struct {
	Aliases map[string]string `json:"aliases"`
}

// SetAliasesResponse is the body for a successful POST /meta/aliases.
type SetAliasesResponse struct {
	OK bool `json:"ok"`
}

// AutoRoutingResponse is the body for a successful POST /meta/auto-routing.
// It is distinct from SetAliasesResponse so callers can decode each
// endpoint's response without sharing a struct.
type AutoRoutingResponse struct {
	OK bool `json:"ok"`
}

// WorkerInfo represents a simplified worker information for the /v1/workers endpoint.
type WorkerInfo struct {
	ID           string                `json:"id"`
	Hostname     string                `json:"hostname"`
	IP           string                `json:"ip"`
	Port         int                   `json:"port"`
	Status       protocol.WorkerStatus `json:"status"`
	Version      string                `json:"version"`
	LoadedModels []string              `json:"loaded_models"`
	LastSeen     time.Time             `json:"last_seen"`
	// InFlight and MaxInFlight are populated from the hub's live load snapshot
	// so the UI can render a per-worker concurrency ratio (e.g. "3/5 calls")
	// without a second call to /v1/queue/stats. Both are 0 when the hub has
	// no load data for the worker (unknown cap), and the UI hides the row.
	InFlight    int64 `json:"in_flight,omitempty"`
	MaxInFlight int64 `json:"max_in_flight,omitempty"`
	// TokensPerSecond is the worker's advertised throughput for the first
	// loaded model in its capabilities snapshot (issue #134). It is the
	// router-side projection of the worker's per-model inference-stats
	// payload. -1 (serialized as omitted) when the worker has not reported
	// any throughput data.
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`
}

// InferenceStatsResponse is the response from GET /v1/inference-stats.
type InferenceStatsResponse struct {
	Workers []WorkerInferenceStats `json:"workers"`
}

// WorkersResponse represents the response from the /v1/workers endpoint.
type WorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// InflightRequestsResponse is the response from GET /meta/requests/inflight.
type InflightRequestsResponse struct {
	Requests []RequestRecord `json:"requests"`
}

// handleWorkersList handles the /v1/workers HTTP endpoint.
func (s *Server) handleWorkersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Prefer the capability cache (has hydrated capabilities), fall back
	// to the registry so workers are visible even before capability fetch.
	var cached []protocol.WorkerInfo
	if s.cache != nil {
		cached = s.cache.List()
	}
	if len(cached) == 0 && s.reg != nil {
		cached = s.reg.ListAvailable()
	}

	// Busy status is a load-derived overlay computed at projection time from
	// the hub's live in-flight counts. It is intentionally NOT written back
	// into the cache or registry — those keep their own status semantics
	// (available/unassigned/unavailable) and the overlay only applies to
	// workers that would otherwise be "available".
	load := s.busyLoadSnapshot()

	workers := make([]WorkerInfo, 0, len(cached))
	for _, w := range cached {
		l := load[w.ID]
		// Pass the worker's first loaded model to toSchedulerWorker so the
		// router-side throughput snapshot for that model is projected into
		// the UI WorkerInfo (issue #134). "" when the worker has no loaded
		// model, which leaves TokensPerSecond at the scheduler's -1
		// sentinel (serialized as omitted).
		firstLoaded := ""
		for _, m := range w.Capabilities.Models {
			if m.Loaded {
				firstLoaded = m.Name
				break
			}
		}
		sw := s.toSchedulerWorker(w, l, true, firstLoaded)
		workers = append(workers, WorkerInfo{
			ID:              w.ID,
			Hostname:        w.Hostname,
			IP:              w.IP,
			Port:            w.Port,
			Status:          busyStatus(w.Status, l),
			Version:         w.Version,
			LoadedModels:    getLoadedModelNames(w.Capabilities.Models),
			LastSeen:        w.LastSeen,
			InFlight:        l.InFlight,
			MaxInFlight:     l.MaxInFlight,
			TokensPerSecond: sw.TokensPerSecond,
		})
	}

	response := WorkersResponse{Workers: workers}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode workers response", "error", err)
	}
}

// getLoadedModelNames extracts the names of models that are loaded from a model list.
func getLoadedModelNames(models []protocol.ModelInfo) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		if m.Loaded {
			names = append(names, m.Name)
		}
	}
	return names
}

// handleModelsList handles the /v1/models HTTP endpoint.
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if s.cache == nil {
		_ = json.NewEncoder(w).Encode(protocol.ModelsResponse{Object: "list", Data: []protocol.ModelInfo{}})
		return
	}

	// Build a set of canonical model names that are targets of an active
	// alias. These are hidden from the worker-model list so the alias (short
	// name) is advertised instead — an alias is a rename, not additive.
	// modelAliases is protected by its own mutex, so we read it under RLock
	// before acquiring the cache RLock.
	var hideCanonical map[string]bool
	s.modelAliasesMu.RLock()
	if modelAliasesActive(s.modelAliases) {
		hideCanonical = make(map[string]bool, len(s.modelAliases.Aliases))
		for _, target := range s.modelAliases.Aliases {
			if target.Canonical != "" {
				hideCanonical[target.Canonical] = true
			}
		}
	}
	s.modelAliasesMu.RUnlock()

	// Get capabilities from cache
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var workers []protocol.WorkerInfo
	for _, w := range s.cache.cache {
		workers = append(workers, w)
	}

	// Aggregate models from all workers, only including loaded models.
	// Initialized as empty slice (not nil) so JSON encodes as [] not null.
	// Canonical models shadowed by an active alias are skipped here; the
	// synthetic alias advertisement block below still advertises them.
	models := []protocol.ModelInfo{}
	for _, worker := range workers {
		for _, m := range worker.Capabilities.Models {
			if !m.Loaded || hideCanonical[m.Name] {
				continue
			}
			models = append(models, m)
		}
	}

	// When auto routing is enabled, advertise the synthetic "auto" alias so
	// OpenAI-compatible clients discover it alongside concrete models.
	// autoRouting is protected by its own mutex.
	s.autoRoutingMu.RLock()
	if s.autoRoutingActiveLocked() {
		models = append(models, protocol.ModelInfo{
			Name:      autoModelAlias,
			Backend:   "auto",
			MaxTokens: 0,
			Loaded:    true,
		})
	}
	s.autoRoutingMu.RUnlock()

	// Advertise configured user-facing aliases so clients can request them
	// by their short name and have the router rewrite them to the canonical
	// model before worker selection. Only aliases with a non-empty Canonical
	// are advertised — a dangling alias key would advertise a model that
	// resolves to nothing.
	s.modelAliasesMu.RLock()
	if modelAliasesActive(s.modelAliases) {
		for alias, target := range s.modelAliases.Aliases {
			if target.Canonical == "" {
				continue
			}
			models = append(models, protocol.ModelInfo{
				Name:      alias,
				Backend:   "alias",
				MaxTokens: 0,
				Loaded:    true,
			})
		}
	}
	s.modelAliasesMu.RUnlock()

	response := protocol.ModelsResponse{Object: "list", Data: models}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode models", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "failed to encode models", "server_error", "encoding_error")
	}
}

// handlePopularModels handles the /meta/models/popular endpoint.
func (s *Server) handlePopularModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// 1. Aggregate worker_count from capability cache (model appears in
	//    Capabilities.Models regardless of Loaded).
	// 2. Merge call_count from s.counter.Snapshot().
	// 3. Sort by a total order (no ties) so repeated polls do not shuffle
	//    equal-ranked models: loaded_worker_count desc, worker_count desc,
	//    call_count desc, model name asc. SliceStable is belt-and-suspenders;
	//    the comparator is already a total order.
	// 4. Encode PopularModelsResponse.

	// Get capabilities from cache
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	counts := s.counter.Snapshot()

	// Map to accumulate counts per model
	// Key: model name, Value: struct with counts
	type modelStats struct {
		model       string
		workerCount int
		loadedCount int
		callCount   int64
	}
	statsMap := make(map[string]*modelStats)

	// Iterate through all workers in cache to get model availability and counts
	for _, worker := range s.cache.cache {
		for _, m := range worker.Capabilities.Models {
			if _, ok := statsMap[m.Name]; !ok {
				statsMap[m.Name] = &modelStats{
					model:     m.Name,
					callCount: counts[m.Name],
				}
			}
			statsMap[m.Name].workerCount++
			if m.Loaded {
				statsMap[m.Name].loadedCount++
			}
		}
	}

	// Convert map to slice
	popularModels := make([]PopularModelInfo, 0, len(statsMap))
	for _, stat := range statsMap {
		popularModels = append(popularModels, PopularModelInfo{
			Model:             stat.model,
			WorkerCount:       stat.workerCount,
			LoadedWorkerCount: stat.loadedCount,
			CallCount:         int(stat.callCount),
		})
	}

	// Sort by loaded first, then model name (alphabetical).
	// Stable: loaded_worker_count is the only thing that changes
	// the order, so a refresh updates contents without reordering.
	sort.SliceStable(popularModels, func(i, j int) bool {
		a, b := popularModels[i], popularModels[j]
		if a.LoadedWorkerCount != b.LoadedWorkerCount {
			return a.LoadedWorkerCount > b.LoadedWorkerCount
		}
		return a.Model < b.Model
	})

	if err := json.NewEncoder(w).Encode(PopularModelsResponse{Models: popularModels}); err != nil {
		s.log.Error("failed to encode popular models", "error", err)
	}
}

// handleSetAliases handles the POST /meta/aliases HTTP endpoint. It atomically
// replaces the router's model alias configuration with the full map sent by
// the caller. The endpoint is replace-all, not a delta: the caller (frontend)
// must merge new aliases into the existing map before calling.
func (s *Server) handleSetAliases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	var req SetAliasesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Error("failed to parse set-aliases request", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Build *ModelAliasConfig from the full map.
	// Filter out entries with empty canonical — a dangling alias key advertises nothing.
	cfg := buildAliasConfig(req.Aliases)
	s.SetModelAliases(cfg)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(SetAliasesResponse{OK: true}); err != nil {
		s.log.Error("failed to encode set-aliases response", "error", err)
	}
}

// handleAutoRouting handles GET/POST /meta/auto-routing. GET returns the current
// auto-routing configuration as a DTO with string tier names. POST replaces the
// configuration atomically; the endpoint is replace-all, not a delta. An empty
// body or {"enabled": false} clears the config (disables auto routing). Models
// with no non-empty tier model are treated as disabled (nil config) for
// backward compatibility. /meta/auto-routing is publicly accessible (no API key required).
func (s *Server) handleAutoRouting(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.autoRoutingMu.RLock()
		cfg := s.autoRouting
		s.autoRoutingMu.RUnlock()

		dto := toAutoRoutingConfigDTO(cfg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(dto); err != nil {
			s.log.Error("failed to encode auto-routing response", "error", err)
		}
		return

	case http.MethodPost:
		var req AutoRoutingConfigRequest
		// Decode body; empty body decodes to zero-value struct, which is fine —
		// enabled will be false and buildAutoRoutingConfig will return nil.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// If body is empty, json.Decoder returns EOF, which we treat as a
			// clear (enabled=false). Any other decode error is bad request.
			if !errors.Is(err, io.EOF) {
				s.log.Error("failed to parse auto-routing request", "error", err)
				writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
				return
			}
			// Empty body → clear config
			req = AutoRoutingConfigRequest{}
		}

		cfg := buildAutoRoutingConfig(&req)
		s.SetAutoRouting(cfg)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(AutoRoutingResponse{OK: true}); err != nil {
			s.log.Error("failed to encode auto-routing response", "error", err)
		}
		return

	default:
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
}

// handleInflightRequests handles the GET /meta/requests/inflight endpoint.
// It returns the set of currently in-flight (and recently completed) inference
// requests with: request id, model, worker id, started-at, status
// (in-flight / streaming / done / error). The trace is in-memory and bounded
// (issue #156).
func (s *Server) handleInflightRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var reqs []RequestRecord
	if s.tracker != nil {
		reqs = s.tracker.Snapshot()
	}
	if reqs == nil {
		reqs = []RequestRecord{}
	}
	resp := InflightRequestsResponse{Requests: reqs}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode inflight requests", "error", err)
	}
}

// workerCount returns the number of connected/available workers.
// It prefers the capability cache (which has hydrated capabilities) and
// falls back to the registry's available workers when the cache is empty or nil.
func (s *Server) workerCount() int {
	var cached []protocol.WorkerInfo
	if s.cache != nil {
		cached = s.cache.List()
	}
	if len(cached) == 0 && s.reg != nil {
		cached = s.reg.ListAvailable()
	}
	return len(cached)
}

// handleConnectionCount handles the /meta/connections/count HTTP endpoint. It reports
// the number of connected/available workers in the pool.
func (s *Server) handleConnectionCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		Workers int `json:"workers"`
	}{Workers: s.workerCount()}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode worker count", "error", err)
	}
}

// handleInferenceStats handles the GET /v1/inference-stats HTTP endpoint. It
// returns the router-side projection of every worker's per-model inference
// throughput snapshot (tokens/sec, avg TTFT) advertised in the workers'
// /capabilities payloads (issue #134). The response is sorted by worker ID
// then model name for deterministic payloads.
func (s *Server) handleInferenceStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if s.cache == nil {
		_ = json.NewEncoder(w).Encode(InferenceStatsResponse{Workers: []WorkerInferenceStats{}})
		return
	}

	resp := InferenceStatsResponse{Workers: s.cache.ListInferenceStats()}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode inference stats", "error", err)
	}
}

// handleQueueStats handles the /v1/queue/stats HTTP endpoint.
func (s *Server) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if s.hub == nil {
		writeErrorResponse(w, http.StatusInternalServerError, "queue stats not configured", "server_error", "internal_error")
		return
	}

	resp := s.hub.QueueStats()
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode queue stats", "error", err)
	}
}

// handleDevRegister handles the /v1/dev/register HTTP endpoint.
func (s *Server) handleDevRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	var worker protocol.WorkerInfo
	if err := json.NewDecoder(r.Body).Decode(&worker); err != nil {
		s.log.Error("failed to parse worker registration", "error", err)
		writeErrorResponse(w, http.StatusBadRequest, "invalid request", "invalid_request_error", "parse_error")
		return
	}

	// Validate empty ID
	if worker.ID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "empty ID", "invalid_request_error", "validation_error")
		return
	}

	// Validate port range
	if worker.Port <= 0 || worker.Port > 65535 {
		writeErrorResponse(w, http.StatusBadRequest, "invalid port", "invalid_request_error", "validation_error")
		return
	}

	// Dev mode: derive the routable IP from the peer address (authoritative),
	// ignoring any client-provided value.
	var peerIP string
	if security.IsDevMode(s.cfg) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			// Same-host peer: a localhost dial may land over IPv6 loopback (::1),
			// which has no IPv4 form. Normalize any loopback peer to the
			// canonical routable IPv4 loopback address.
			ip = net.IPv4(127, 0, 0, 1)
		}
		if ip == nil || ip.To4() == nil {
			writeErrorResponse(w, http.StatusBadRequest, "unable to determine routable worker IPv4 from remote address", "validation_error", "ip_validation_error")
			return
		}
		providedIP := worker.IP
		worker.IP = ip.String()
		peerIP = worker.IP // trusted peer identity for collision check
		if providedIP != "" && providedIP != worker.IP {
			s.log.Info("dev worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port, "provided_ip", providedIP)
		} else {
			s.log.Info("dev worker registered", "id", worker.ID, "ip", worker.IP, "port", worker.Port)
		}
	} else {
		// Prod mode: keep the legacy loopback-only check, unchanged.
		if worker.IP != "127.0.0.1" && worker.IP != "localhost" {
			writeErrorResponse(w, http.StatusForbidden, "non-loopback IP not allowed", "authentication_error", "ip_validation_error")
			return
		}
		peerIP = worker.IP // in prod mode the body-provided IP is authoritative (loopback only)
	}

	// Collision check: reject registration if this ID is already claimed
	// by a worker from a different peer IP. This prevents worker ID spoofing
	// / registry poisoning: a malicious worker cannot overwrite another
	// worker's registry entry by registering with the victim's ID.
	if existing, ok := s.reg.Get(worker.ID); ok {
		if existing.IP != peerIP {
			s.log.Warn("rejected duplicate worker ID from different peer",
				"id", worker.ID, "existing_ip", existing.IP, "peer_ip", peerIP)
			writeErrorResponse(w, http.StatusConflict, "worker ID already registered by another peer",
				"authentication_error", "duplicate_id")
			return
		}
		// Same peer IP: allow re-registration (legitimate reconnect/restart)
		s.log.Info("worker re-registered from same peer", "id", worker.ID, "peer_ip", peerIP)
	}

	// Force status to available for dev mode
	worker.Status = protocol.StatusAvailable

	// Register worker
	if err := s.reg.HandleEvent(protocol.DiscoveryEvent{
		Type:   protocol.EventAdded,
		Worker: worker,
	}); err != nil {
		s.log.Error("failed to register worker", "error", err)
		writeErrorResponse(w, http.StatusInternalServerError, "failed to register worker", "server_error", "registration_error")
		return
	}

	w.WriteHeader(http.StatusOK)
}

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
)

// buildCandidates builds the list of candidate workers that have the
// requested model loaded, attempting auto-load when no loaded candidate
// is found. The returned slice is deterministically ordered by worker ID
// (lowest ID first) and is safe to use outside the cache lock.
// Caller must hold no lock; the function acquires the cache RLock internally.
func (s *Server) buildCandidates(model string) []protocol.WorkerInfo {
	if s.cache == nil {
		return nil
	}
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()

	var candidates []protocol.WorkerInfo
	for _, worker := range s.cache.cache {
		for _, m := range worker.Capabilities.Models {
			if m.Name == model && m.Loaded {
				candidates = append(candidates, worker)
				break
			}
		}
	}

	if len(candidates) == 0 {
		// Try to auto-load the model on any worker that knows about it.
		// The cache only refreshes on the next heartbeat, so a re-fetch would
		// see the same stale state and recurse forever; instead we build a
		// local deep copy of the freshly loaded worker without mutating the
		// (read-locked) cache.
		for _, worker := range s.cache.cache {
			for _, m := range worker.Capabilities.Models {
				if m.Name == model {
					if s.loadModelOnWorker(worker, model) {
						loaded := worker
						loaded.Capabilities.Models = append([]protocol.ModelInfo(nil), worker.Capabilities.Models...)
						for i := range loaded.Capabilities.Models {
							if loaded.Capabilities.Models[i].Name == model {
								loaded.Capabilities.Models[i].Loaded = true
								break
							}
						}
						candidates = append(candidates, loaded)
						break
					}
				}
			}
		}
	}

	// Deterministic ordering so equal scores resolve to the lowest worker ID.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates
}

// selectWorkerExcluding selects a worker for the given model, excluding any
// workers whose IDs are in the provided set. This is used by the failover
// loop to avoid retrying the same worker. The exclusion set is respected
// during both the primary candidate selection and the fallback paths.
func (s *Server) selectWorkerExcluding(model string, excluded map[string]struct{}) (protocol.WorkerInfo, error) {
	if s.cache == nil {
		return protocol.WorkerInfo{}, fmt.Errorf("capability cache not configured")
	}

	candidates := s.buildCandidates(model)

	// Filter out excluded workers before scoring.
	if len(excluded) > 0 {
		filtered := make([]protocol.WorkerInfo, 0, len(candidates))
		for _, w := range candidates {
			if _, ok := excluded[w.ID]; !ok {
				filtered = append(filtered, w)
			}
		}
		candidates = filtered
	}

	// Apply model opt-out filters (AllowedModels/ExcludedModels) to the
	// loaded-model candidates. Workers that opted out of the requested model
	// must not be considered, even if they have the model loaded.
	if s.cache != nil {
		s.cache.mu.RLock()
		// Build scheduler view for FilterByAllowedModels
		schedCands := make([]scheduler.WorkerInfo, 0, len(candidates))
		for _, w := range candidates {
			schedCands = append(schedCands, s.toSchedulerWorker(w, WorkerQueueStats{}, false, model))
		}
		s.cache.mu.RUnlock()

		schedCands = scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, schedCands)

		// Map back to protocol workers by ID (unique in cache)
		filteredProto := make([]protocol.WorkerInfo, 0, len(schedCands))
		for _, sc := range schedCands {
			for _, pc := range candidates {
				if pc.ID == sc.ID {
					filteredProto = append(filteredProto, pc)
					break
				}
			}
		}
		candidates = filteredProto
	}

	if len(candidates) > 0 {
		// Deterministic ordering before scoring so equal scores resolve to the
		// lowest worker ID.
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		if s.currentScorer() != nil {
			sel, err := s.scoreCandidates(model, candidates, s.loadSnapshot())
			if err == nil && sel != nil {
				for _, w := range candidates {
					if w.ID == sel.ID {
						s.log.Debug("selected worker via weighted scorer", "model", model, "worker", w.ID, "score", sel.Score)
						return w, nil
					}
				}
			} else if err != nil {
				s.log.Warn("weighted scorer failed, falling back to deterministic selection", "model", model, "error", err)
			}
		}
		return candidates[0], nil
	}

	// Fallback: if no exact loaded-model match, route to any cached worker
	// that advertises the model (even if not marked loaded), then to any
	// available worker. Respect exclusion in both cases.
	//
	// The model opt-out filters (AllowedModels/ExcludedModels) are applied
	// here too. Without this, a worker that opted out of the requested model
	// would still be selected as a last-resort fallback, silently bypassing
	// the whitelist/blacklist (e.g. requesting model-z on a fleet where every
	// worker whitelists a different model would route to an unwilling worker
	// instead of returning no_workers).
	if s.cache != nil {
		s.cache.mu.RLock()
		defer s.cache.mu.RUnlock()

		// Build the scheduler's view of every non-excluded cached worker so
		// FilterByAllowedModels (which operates on scheduler.WorkerInfo) can
		// honour the opt-out flags. The protocol snapshot is kept alongside
		// the sched view so the returned value carries the full cached
		// worker; the two slices are kept in lockstep.
		allProto := make([]protocol.WorkerInfo, 0, len(s.cache.cache))
		allSched := make([]scheduler.WorkerInfo, 0, len(s.cache.cache))
		for _, worker := range s.cache.cache {
			if _, ok := excluded[worker.ID]; ok {
				continue
			}
			allProto = append(allProto, worker)
			allSched = append(allSched, s.toSchedulerWorker(worker, WorkerQueueStats{}, false, model))
		}

		// 1. Any worker advertising the model (loaded or not) that is willing
		//    to serve it. FilterByAllowedModels preserves input order, so the
		//    returned sched entries map 1:1 onto the proto entries.
		adProto := make([]protocol.WorkerInfo, 0, len(allProto))
		adSched := make([]scheduler.WorkerInfo, 0, len(allSched))
		for i := range allProto {
			for _, m := range allProto[i].Capabilities.Models {
				if m.Name == model {
					adProto = append(adProto, allProto[i])
					adSched = append(adSched, allSched[i])
					break
				}
			}
		}
		adSched = scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, adSched)
		if len(adSched) > 0 {
			// FilterByAllowedModels preserves input order, but the first
			// surviving sched entry may not be the first advertising proto
			// entry (an unwilling advertiser is skipped). Map back by ID,
			// which is unique within the cache.
			selectedID := adSched[0].ID
			s.log.Warn("routing to worker with model not marked loaded", "model", model, "worker", selectedID)
			for i := range adProto {
				if adProto[i].ID == selectedID {
					return adProto[i], nil
				}
			}
		}

		// 2. Last resort: any available worker willing to serve the model.
		//    Unfiltered workers (empty lists) remain eligible; workers that
		//    explicitly opted out are excluded, so a fully opted-out fleet
		//    returns ErrNoMatchingWorkers rather than forcing the request
		//    onto an unwilling worker.
		anySched := scheduler.FilterByAllowedModels(scheduler.ModelRequest{Model: model}, allSched)
		if len(anySched) > 0 {
			sort.Slice(anySched, func(i, j int) bool { return anySched[i].ID < anySched[j].ID })
			s.log.Warn("no worker has requested model, falling back to available worker", "model", model, "worker", anySched[0].ID)
			// Find the proto snapshot matching the selected sched worker.
			for i := range allProto {
				if allProto[i].ID == anySched[0].ID {
					return allProto[i], nil
				}
			}
		}
	}

	return protocol.WorkerInfo{}, scheduler.ErrNoMatchingWorkers
}

// selectWorker selects a worker for the given model using the default
// (empty) exclusion set. This delegates to selectWorkerExcluding for
// implementation reuse.
func (s *Server) selectWorker(model string) (protocol.WorkerInfo, error) {
	return s.selectWorkerExcluding(model, nil)
}

// isRetryableError classifies an error as retryable (should trigger failover
// to the next worker) or non-retryable (fail fast). Retryable errors include
// transport failures, timeouts, 5xx/429 worker responses, and deadline
// exceeded; non-retryable includes 4xx (except 429), parse errors, and
// scheduler exhaustion sentinels.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Explicitly non-retryable first.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, scheduler.ErrNoMatchingWorkers) ||
		errors.Is(err, scheduler.ErrAllWorkersUnavailable) {
		return false
	}
	// Retryable sentinels.
	if errors.Is(err, errTimeout) {
		return true
	}
	if errors.Is(err, errConnClosed) {
		return true
	}
	if errors.Is(err, errRelayDisconnected) {
		return true
	}
	if errors.Is(err, errRateLimited) {
		return true
	}
	if errors.Is(err, errWorkerInferenceError) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Worker HTTP errors: 5xx and 429 are retryable; other 4xx are not.
	var herr *WorkerHTTPError
	if errors.As(err, &herr) {
		switch {
		case herr.StatusCode >= 500:
			return true
		case herr.StatusCode == http.StatusTooManyRequests:
			return true
		default:
			return false
		}
	}
	return false
}

// errAllAttemptsFailed is a sentinel error returned when all failover
// attempts have been exhausted. It wraps the last underlying error for
// diagnostic purposes.
var errAllAttemptsFailed = errors.New("all failover attempts exhausted")

// dispatchWithFailover dispatches a non-streaming inference request with
// automatic failover across workers. It returns the successful worker's
// response body or an error after exhausting all attempts or the retry
// budget. The response body is raw (unprocessed) and should be written
// with Content-Type: application/json by the caller.
func (s *Server) dispatchWithFailover(ctx context.Context, model string, body []byte, kind string, maxAttempts int, budget time.Duration) ([]byte, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	excluded := make(map[string]struct{})
	var lastErr error
	recorded := false

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Respect the retry budget.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errAllAttemptsFailed, ctx.Err())
		}

		worker, err := s.selectWorkerExcluding(model, excluded)
		if err != nil {
			lastErr = err
			if attempt == maxAttempts {
				break
			}
			continue
		}

		// Record this inference dispatch for the popularity counter exactly
		// once per client request (not per attempt).
		if !recorded && s.counter != nil {
			s.counter.Record(model)
			recorded = true
		}

		// Pre-dispatch rate-limit check: a worker at its MaxInFlight cap
		// cannot take the call right now; treat it as a retryable rejection
		// and try the next best worker instead of failing the request outright.
		if s.hub != nil && s.hub.isRateLimited(worker.ID) {
			s.hub.recordRejection(worker.ID, http.StatusTooManyRequests)
			excluded[worker.ID] = struct{}{}
			lastErr = errRateLimited
			continue
		}

		// Record this dispatch attempt for the request-trace endpoint
		// (issue #156) BEFORE sending it, so the request is visible while
		// in flight (model loading can take a long time). A successful
		// attempt is "done", a failed one is "error". Failover retries
		// therefore appear as separate trace rows, which is the visibility
		// the dashboard is for.
		reqID := ""
		if s.tracker != nil {
			reqID = s.tracker.Begin(model, worker.ID, worker.Hostname, kind)
		}
		var client WorkerClient
	if s.clientFactory != nil {
		client = s.clientFactory(worker)
	} else {
		client = s.clientFor(worker)
	}
	resp, err := client.Complete(ctx, worker, kind, body)
		if s.tracker != nil && reqID != "" {
			if err == nil {
				s.tracker.Finish(reqID, StatusDone)
			} else {
				s.tracker.Finish(reqID, StatusError)
			}
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if isRetryableError(err) {
			excluded[worker.ID] = struct{}{}
			continue
		}
		// Non-retryable error: fail fast.
		return nil, err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no worker available for model %s", model)
	}
	return nil, fmt.Errorf("%w: %w", errAllAttemptsFailed, lastErr)
}

// writeFailoverError maps a failover error to an appropriate HTTP response
// and writes it to the client. It is called after the failover loop has
// exhausted all attempts or hit a non-retryable error.
func (s *Server) writeFailoverError(w http.ResponseWriter, model string, err error) {
	if err == nil {
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return
	}
	// All attempts exhausted sentinel.
	if errors.Is(err, errAllAttemptsFailed) {
		writeErrorResponse(w, http.StatusServiceUnavailable, "all failover attempts exhausted", "server_error", "all_attempts_failed")
		return
	}
	// Retry budget exceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		writeErrorResponse(w, http.StatusGatewayTimeout, "request timed out after retries", "server_error", "timeout_error")
		return
	}
	// Worker HTTP errors: pass through their status code.
	var herr *WorkerHTTPError
	if errors.As(err, &herr) {
		writeErrorResponse(w, herr.StatusCode, "worker error", "server_error", "worker_error")
		return
	}
	// Transport / relay errors.
	if errors.Is(err, errTimeout) {
		writeErrorResponse(w, http.StatusGatewayTimeout, "request timed out", "server_error", "timeout_error")
		return
	}
	if errors.Is(err, errConnClosed) || errors.Is(err, errRelayDisconnected) {
		writeErrorResponse(w, http.StatusServiceUnavailable, "worker unavailable", "server_error", "connection_error")
		return
	}
	// Scheduler exhaustion / no workers.
	writeErrorResponse(w, http.StatusServiceUnavailable, "no workers available for model "+model, "server_error", "no_workers")
}

// scoreCandidates runs the weighted scorer over candidates and returns the best
// worker. Candidates must already be deterministically ordered (by ID) so equal
// scores resolve to the lowest ID.
func (s *Server) scoreCandidates(model string, candidates []protocol.WorkerInfo, load map[string]WorkerQueueStats) (*scheduler.SelectedWorker, error) {
	workers := make([]scheduler.WorkerInfo, 0, len(candidates))
	for _, w := range candidates {
		l, ok := load[w.ID]
		workers = append(workers, s.toSchedulerWorker(w, l, ok, model))
	}
	// Snapshot the scorer under the read lock so the algorithm and its
	// unavailable-TTL come from the same instance (SetScorerWeights replaces
	// both atomically).
	sc := s.currentScorer()
	return scheduler.SelectWorker(context.Background(), sc, scheduler.ModelRequest{Model: model}, workers, sc.GetUnavailableTTL())
}

// loadModelOnWorker attempts to load a model on a worker using HTTP proxy.
// The POST is bounded by s.modelLoadTimeout; on success or failure the
// attempt is logged at debug level (worker ID, model name, elapsed, status,
// error) so operators can see whether the router fell back to the
// advertising-but-not-loaded path.
func (s *Server) loadModelOnWorker(worker protocol.WorkerInfo, modelName string) bool {
	url := fmt.Sprintf("http://%s:%d/v1/models/load", worker.IP, worker.Port)

	loadReq := map[string]string{"model": modelName}
	body, err := json.Marshal(loadReq)
	if err != nil {
		s.log.Debug("model load auto-attempt failed", "worker", worker.ID, "model", modelName, "err", err)
		return false
	}

	client := &http.Client{Timeout: s.modelLoadTimeout}
	start := time.Now()
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	elapsed := time.Since(start)
	if err != nil {
		s.log.Debug("model load auto-attempt failed", "worker", worker.ID, "model", modelName, "elapsed", elapsed, "err", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		s.log.Debug("model load auto-attempt failed", "worker", worker.ID, "model", modelName, "elapsed", elapsed, "status", resp.StatusCode)
		return false
	}

	s.log.Debug("model load auto-attempt succeeded", "worker", worker.ID, "model", modelName, "elapsed", elapsed, "status", resp.StatusCode)
	return true
}

// hubLoadSnapshot returns a per-worker queue-load snapshot from the hub, taken
// once per selection so every candidate sees consistent load data. Returns an
// empty map when no hub is configured (e.g. minimal test servers).
func (s *Server) hubLoadSnapshot() map[string]WorkerQueueStats {
	load := make(map[string]WorkerQueueStats)
	if s.hub == nil {
		return load
	}
	for _, ws := range s.hub.QueueStats().Workers {
		load[ws.WorkerID] = ws
	}
	return load
}

// hubBusySnapshot returns the per-worker in-flight snapshot used by the
// busy-status overlay in handleWorkersList. It mirrors hubLoadSnapshot but
// is safe to call from a read path: when no hub is configured (e.g. minimal
// test servers) it returns an empty map so every worker keeps its cached
// status unchanged.
func (s *Server) hubBusySnapshot() map[string]WorkerQueueStats {
	load := make(map[string]WorkerQueueStats)
	if s.hub == nil {
		return load
	}
	for _, ws := range s.hub.QueueStats().Workers {
		load[ws.WorkerID] = ws
	}
	return load
}

// busyThresholdPct is the in-flight ratio at or above which a worker that
// would otherwise be "available" is projected as "busy". Derived from the
// configured cap (never a hardcode): default maxInFlight=4 -> busy at 3+
// in-flight, cap 10 -> busy at 8+.
const busyThresholdPct = 0.80

// busyStatus projects a load-derived busy overlay onto a worker's cached
// status. Only workers that would otherwise be "available" can become busy;
// unavailable/unassigned workers keep their status so the overlay never
// masks a real connectivity or provisioning state. When the hub reports no
// load data for the worker (unknown cap or below threshold) the cached
// status is returned unchanged.
//
// The threshold is the floor of busyThresholdPct * MaxInFlight so the brief's
// examples hold exactly: cap 4 -> busy at 3+, cap 10 -> busy at 8+.
func busyStatus(cached protocol.WorkerStatus, load WorkerQueueStats) protocol.WorkerStatus {
	if cached != protocol.StatusAvailable {
		return cached
	}
	if load.MaxInFlight <= 0 {
		return cached
	}
	threshold := int(busyThresholdPct * float64(load.MaxInFlight))
	if load.InFlight >= int64(threshold) {
		return protocol.StatusBusy
	}
	return cached
}

// toSchedulerWorker converts a cached protocol worker into the scheduler's
// view, attaching queue depth and wait latency from the hub snapshot when
// available. Unknown signals are left negative so the scorer treats them as
// neutral (workers do not push live GPU utilization today).
//
// model is the model name being scheduled for; it is used to look up the
// worker's per-model advertised tokens/sec from the capability cache so the
// scheduler can rank workers by throughput (issue #134). Pass "" when no
// model is in scope (e.g. the UI projection path), in which case the
// throughput is left at the scheduler's -1 "unknown" sentinel.
func (s *Server) toSchedulerWorker(w protocol.WorkerInfo, load WorkerQueueStats, haveLoad bool, model string) scheduler.WorkerInfo {
	sw := scheduler.WorkerInfo{
		ID:            w.ID,
		Addr:          fmt.Sprintf("%s:%d", w.IP, w.Port),
		GPUUtilPct:    -1, // unknown; scorer treats as neutral
		QueueDepth:    -1, // unknown until the load snapshot says otherwise
		InFlight:      -1, // unknown until the load snapshot says otherwise
		MaxInFlight:   -1, // unknown until the load snapshot says otherwise
		AvgLatencyMS:  -1, // unknown until the load snapshot says otherwise
		LastHeartbeat: w.LastSeen,
	}
	if w.Capabilities.VRAM.TotalMB > 0 {
		sw.VRAMTotalMB = int(w.Capabilities.VRAM.TotalMB)
		sw.VRAMFreeMB = int(w.Capabilities.VRAM.FreeMB)
	}
	sw.Models = append(sw.Models, w.Capabilities.Models...)
	// Carry the worker's model opt-out filters so the scheduler can honour
	// them during selection.
	sw.AllowedModels = append(sw.AllowedModels, w.Capabilities.AllowedModels...)
	sw.ExcludedModels = append(sw.ExcludedModels, w.Capabilities.ExcludedModels...)
	if haveLoad {
		sw.QueueDepth = int(load.QueueDepth)
		sw.InFlight = int(load.InFlight)
		sw.MaxInFlight = int(load.MaxInFlight)
		if load.AvgWaitMs >= 0 {
			sw.AvgLatencyMS = load.AvgWaitMs
		}
	}
	// If the worker reports a concurrency cap, use it for scheduler scoring.
	if w.Capabilities.ConcurrencyCap > 0 {
		sw.MaxInFlight = w.Capabilities.ConcurrencyCap
	}
	// Override with hub-side cap if set (from capabilities update or CLI flag).
	if haveLoad && load.MaxInFlight > 0 {
		sw.MaxInFlight = int(load.MaxInFlight)
	}
	// Attach the worker's advertised tokens/sec for the model being
	// scheduled (issue #134). Unknown (-1) is the scheduler's neutral
	// sentinel; the scorer treats it as 0.5 when ThroughputWeight > 0.
	if model != "" && s.cache != nil {
		if stats, ok := s.cache.GetInferenceStats(w.ID, model); ok {
			sw.TokensPerSecond = stats.TokensPerSecond
		}
	}
	return sw
}

// clientFor returns a WorkerClient for the given worker, preferring the
// active WebSocket connection for WS workers and falling back to HTTP
// dial-back for legacy (mDNS/dev-HTTP) workers.
func (s *Server) clientFor(worker protocol.WorkerInfo) WorkerClient {
	if s.clientFactory != nil {
		return s.clientFactory(worker)
	}
	if worker.Transport == protocol.TransportWS {
		if c := s.hub.Client(worker.ID); c != nil {
			return c
		}
		// No active connection yet (e.g. worker just registered, reader loop
		// hasn't started). Return a relayWorkerClient that sends via the hub's
		// relaySendCh; the writer loop will pick it up once the relay is up.
		return newRelayWorkerClient(s.hub, s.log)
	}
	s.log.Warn("worker advertises websocket transport but has no active connection; falling back to http", "worker", worker.ID)
	if s.routerClientCert != nil && s.workerCAPool != nil {
		return newHTTPWorkerClientWithCerts(s.log, s.routerClientCert, s.workerCAPool)
	}
	return newHTTPWorkerClient(s.log)
}

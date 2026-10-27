package worker

import (
	"context"
	"time"
)

// DefaultModelRefreshInterval is the default cadence for re-querying the
// backend's model catalogue. It is intentionally short (10s) so a backend
// that starts after the worker surfaces quickly; users are expected to raise
// it to 5min once the pool is stable.
const DefaultModelRefreshInterval = 10 * time.Second

// effectiveModelRefreshInterval resolves the configured refresh interval,
// falling back to DefaultModelRefreshInterval when zero or negative.
func effectiveModelRefreshInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultModelRefreshInterval
	}
	return d
}

// refreshModels periodically re-queries the backend's ListModels() and merges
// the discovered catalogue into srv.models. It is bounded by ctx: when the
// worker shuts down the goroutine exits. It closes the gap where a backend
// (LM Studio, Ollama, vLLM) starts after the worker and would otherwise stay
// invisible until restart.
//
// The write to s.models is now synchronized via SetModels and GetModels,
// which use modelsMu to protect concurrent access from the refresh goroutine
// and HTTP handlers.
func refreshModels(ctx context.Context, srv *Server, backend Backend, interval time.Duration) {
	if backend == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			models, err := backend.ListModels()
			if err != nil {
				srv.log.Warn("model refresh: backend ListModels failed", "error", err)
				continue
			}
			if len(models) == 0 {
				// The backend is up but advertises nothing right now
				// (e.g. LM Studio with no project loaded). Leave the
				// existing catalogue untouched: models declared via
				// --model-path must survive, and a transient empty
				// result must not wipe models the backend previously
				// reported. The next tick retries.
				srv.log.Debug("model refresh: backend advertised no models this tick")
				continue
			}
			// Merge on top of any models declared via --model-path so a
			// manually declared model survives a refresh that discovers
			// nothing new.
			srv.SetModels(mergeModels(srv.GetModels(), models))
			srv.log.Debug("model refresh: discovered models", "count", len(models))
		}
	}
}

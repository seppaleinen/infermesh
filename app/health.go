package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// defaultRouterProbe returns a router-reachability check built from
// RouterClient. It doubles as the worker-registration list source: a nil error
// means the router answered /v1/workers with 200, and the returned slice is
// the pool. Tests inject a stub returning canned WorkerViews.
//
// The probe uses the Supervisor's RouterClient (constructed once in main.go)
// and injects the Supervisor's mTLS transport when the router is running in
// prod mode. RouterClient has zero knowledge of the Supervisor (issue #103 R1).
func (s *Supervisor) defaultRouterProbe() func(ctx context.Context, st Settings) ([]WorkerView, error) {
	return func(ctx context.Context, st Settings) ([]WorkerView, error) {
		if s.routerClient == nil {
			// Fallback for tests that build a Supervisor without main.go.
			rc := NewRouterClient(routerBaseURL(st.RouterAddr))
			if client := s.buildTLSClient(); client != nil {
				rc.SetTransport(client)
			}
			return rc.GetWorkers(ctx)
		}
		s.routerClient.SetRouterURL(routerBaseURL(st.RouterAddr))
		return s.routerClient.GetWorkers(ctx)
	}
}

// defaultWorkerProbe returns a worker-HTTP health check against the worker's
// /health endpoint.
func (s *Supervisor) defaultWorkerProbe() func(ctx context.Context, st Settings) (bool, error) {
	return func(ctx context.Context, st Settings) (bool, error) {
		return workerHealthy(ctx, st.WorkerPort)
	}
}

// workerHealthy performs an HTTP probe against the worker's /health endpoint.
func workerHealthy(ctx context.Context, port int) (bool, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}

// workerRegistered reports whether a worker on the given port appears in the
// router's /v1/workers pool.
func workerRegistered(workers []WorkerView, port int) bool {
	for _, w := range workers {
		if w.Port == port {
			return true
		}
	}
	return false
}

// buildProcessStatus maps a processInfo snapshot onto the UI projection. When
// the process is not running, startErr/exitCode are used to populate State and
// Error. When running, healthy is recorded.
func buildProcessStatus(info processInfo, startErr error, healthy, registered bool) ProcessStatus {
	ps := ProcessStatus{
		LogPath:    info.LogPath,
		StartedAt:  info.StartedAt,
		PID:        info.PID,
		Registered: registered,
	}
	if !info.Running {
		if startErr != nil && info.PID == 0 && info.ExitCode < 0 {
			ps.State = StateError
			ps.Error = startErr.Error()
			return ps
		}
		if info.ExitCode > 0 {
			ps.State = StateError
			ps.Error = fmt.Sprintf("process exited with code %d", info.ExitCode)
		} else {
			ps.State = StateStopped
		}
		return ps
	}
	ps.State = StateRunning
	ps.Running = true
	ps.Healthy = healthy
	if !healthy {
		ps.Error = "health check failed"
	}
	return ps
}

// Status reports the current state of both supervised children plus a live
// health probe against the router (which also yields the worker-registration
// list). It never errors: an unreachable router simply reads as unhealthy.
func (s *Supervisor) Status() (SupervisorStatus, error) {
	settings, err := s.loadSettings()
	if err != nil {
		// No settings yet: report everything stopped rather than failing, so
		// the UI can show the "nothing configured" state and prompt the user.
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			return SupervisorStatus{
				Router: ProcessStatus{State: StateStopped},
				Worker: ProcessStatus{State: StateStopped},
			}, nil
		}
		return SupervisorStatus{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	s.mu.Lock()
	var rInfo, wInfo processInfo
	var rErr, wErr error
	if s.router != nil {
		rInfo = s.router.status()
		rErr = s.routerErr
	}
	if s.worker != nil {
		wInfo = s.worker.status()
		wErr = s.workerErr
	}
	s.mu.Unlock()

	// Probe the router; a successful GetWorkers means it answered /v1/workers
	// with 200 and gives us the pool to test worker registration against.
	pool, rerr := s.routerProbe(ctx, settings)
	routerUp := rerr == nil

	// Probe the worker's own HTTP health endpoint (independent of the router).
	workerUp := false
	if s.workerProbe != nil {
		workerUp, _ = s.workerProbe(ctx, settings)
	}

	return SupervisorStatus{
		Router:    buildProcessStatus(rInfo, rErr, routerUp, false),
		Worker:    buildProcessStatus(wInfo, wErr, workerUp, workerRegistered(pool, settings.WorkerPort)),
		RouterURL: routerBaseURL(settings.RouterAddr),
	}, nil
}

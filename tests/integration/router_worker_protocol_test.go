package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// protocolWorkersResponse matches the router's /v1/workers JSON shape.
type protocolWorkersResponse struct {
	Workers []struct {
		ID           string   `json:"id"`
		IP           string   `json:"ip"`
		Port         int      `json:"port"`
		Status       string   `json:"status"`
		LoadedModels []string `json:"loaded_models"`
	} `json:"workers"`
}

// waitForProtocolRouter polls the router until /v1/workers answers 200.
func waitForProtocolRouter(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("router at %s not ready after %s", base, timeout)
}

// waitForProtocolWorker polls /v1/workers until a worker on workerPort is listed.
func waitForProtocolWorker(t *testing.T, base string, workerPort int, timeout time.Duration) protocolWorkersResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last protocolWorkersResponse
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&last); err == nil {
				for _, w := range last.Workers {
					if w.Port == workerPort {
						return last
					}
				}
			}
			_ = resp.Body.Close()
		} else if err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("worker on port %d never appeared in %s/v1/workers (last: %+v)", workerPort, base, last.Workers)
	return last
}

// protocolEnv bundles the router + worker + mock processes for one scenario.
// stop() tears them down in reverse dependency order.
type protocolEnv struct {
	router    *exec.Cmd
	worker    *exec.Cmd
	mock      *exec.Cmd
	routerErr bytes.Buffer
	workerOut bytes.Buffer
	workerErr bytes.Buffer
}

func (e *protocolEnv) stop() {
	for _, cmd := range []*exec.Cmd{e.worker, e.router, e.mock} {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func(c *exec.Cmd) { done <- c.Wait() }(cmd)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
}

// startProtocolEnv starts router (dev mode) + worker (custom backend -> mock)
// and waits for the worker to register. Ports must be unique per test to avoid
// collisions with the e2e package running in parallel.
func startProtocolEnv(t *testing.T, routerPort, workerPort, mockPort int) *protocolEnv {
	t.Helper()
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found (run `make build`)", routerBin)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found (run `make build`)", workerBin)
	}
	routerBase := "http://127.0.0.1:" + strconv.Itoa(routerPort)
	mockBase := "http://127.0.0.1:" + strconv.Itoa(mockPort)

	e := &protocolEnv{}
	e.mock = startMockBackend(t, mockPort)
	t.Cleanup(e.stop)

	e.router = exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:"+strconv.Itoa(routerPort))
	e.router.Stderr = &e.routerErr
	if err := e.router.Start(); err != nil {
		t.Fatalf("starting router: %v", err)
	}
	waitForProtocolRouter(t, routerBase, 10*time.Second)

	e.worker = exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", strconv.Itoa(workerPort),
		"--backend", "custom",
		"--backend-url", mockBase,
	)
	e.worker.Stdout = &e.workerOut
	e.worker.Stderr = &e.workerErr
	if err := e.worker.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	waitForProtocolWorker(t, routerBase, workerPort, 15*time.Second)
	return e
}

func postProtocolChat(base, model string) (int, string) {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	s := readBody(resp)
	_ = resp.Body.Close()
	return resp.StatusCode, s
}

// waitForProtocolModel polls /v1/models until the given model name appears
// (the router's capability cache must hydrate from the worker first).
func waitForProtocolModel(t *testing.T, base, model string, timeout time.Duration, env *protocolEnv) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var models modelsResponse
	for {
		resp, err := http.Get(base + "/v1/models")
		if err == nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&models); err == nil {
				_ = resp.Body.Close()
				for _, m := range models.Data {
					if m.Name == model {
						return
					}
				}
			} else {
				_ = resp.Body.Close()
			}
		} else if err == nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("model %q never appeared in %s/v1/models; router stderr: %s, worker stderr: %s",
				model, base, env.routerErr.String(), env.workerErr.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestProtocolRegistrationHandshake verifies the dev-mode registration
// handshake: the worker self-registers via POST /v1/dev/register and the
// router then reports it on GET /v1/workers with a loopback-derived IP and
// "available" status.
func TestProtocolRegistrationHandshake(t *testing.T) {
	e := startProtocolEnv(t, 18211, 18212, 18213)
	routerBase := "http://127.0.0.1:18211"

	body := waitForProtocolWorker(t, routerBase, 18212, 5*time.Second)
	if len(body.Workers) != 1 {
		t.Fatalf("expected exactly 1 worker after registration, got %d: %+v", len(body.Workers), body.Workers)
	}
	w := body.Workers[0]
	if w.ID == "" {
		t.Error("registered worker has empty ID")
	}
	if w.IP != "127.0.0.1" {
		t.Errorf("registered worker IP = %q, want 127.0.0.1", w.IP)
	}
	if w.Status != "available" {
		t.Errorf("registered worker status = %q, want available", w.Status)
	}
	// The worker used HTTP registration (dev mode), not mDNS. The worker logs
	// to stdout (slog text handler), so check both streams.
	if !strings.Contains(e.workerOut.String(), "registering with router") &&
		!strings.Contains(e.workerErr.String(), "registering with router") {
		t.Errorf("worker did not log HTTP registration; stdout: %s, stderr: %s", e.workerOut.String(), e.workerErr.String())
	}
}

// TestProtocolWorkerListAfterRegistration verifies GET /v1/workers reflects
// the registered worker with a well-formed entry (non-empty ID, loopback IP,
// known port).
func TestProtocolWorkerListAfterRegistration(t *testing.T) {
	e := startProtocolEnv(t, 18216, 18217, 18218)
	routerBase := "http://127.0.0.1:18216"

	body := waitForProtocolWorker(t, routerBase, 18217, 15*time.Second)
	found := false
	for _, w := range body.Workers {
		if w.Port == 18217 {
			found = true
			if w.ID == "" {
				t.Errorf("worker entry has empty ID: %+v", w)
			}
			if w.IP != "127.0.0.1" {
				t.Errorf("worker entry IP = %q, want 127.0.0.1", w.IP)
			}
		}
	}
	if !found {
		t.Fatalf("worker on port 18217 not found in /v1/workers: %+v", body.Workers)
	}
	// Sanity check that the worker actually used the HTTP registration path.
	// The worker logs to stdout (slog text handler), so check both streams.
	if !strings.Contains(e.workerOut.String(), "registering with router") &&
		!strings.Contains(e.workerErr.String(), "registering with router") {
		t.Errorf("worker did not log HTTP registration; stdout: %s, stderr: %s", e.workerOut.String(), e.workerErr.String())
	}
}

// TestProtocolChatDispatch verifies the router dispatches POST
// /v1/chat/completions to the registered worker, which forwards to the mock
// backend and returns the mock's marker.
func TestProtocolChatDispatch(t *testing.T) {
	e := startProtocolEnv(t, 18221, 18222, 18223)
	routerBase := "http://127.0.0.1:18221"

	// The mock backend advertises "other-model" as loaded; the capability
	// cache must hydrate before the router will select the worker.
	waitForProtocolModel(t, routerBase, "other-model", 15*time.Second, e)

	code, bodyStr := postProtocolChat(routerBase, "other-model")
	if code != http.StatusOK {
		t.Fatalf("chat completions returned %d, want 200: %s (router: %s, worker: %s)",
			code, bodyStr, e.routerErr.String(), e.workerErr.String())
	}
	if !strings.Contains(bodyStr, "failover-verified") {
		t.Errorf("chat response missing mock marker; body: %s", bodyStr)
	}
}

// TestProtocolModelsListReflectsWorkers verifies GET /v1/models reflects the
// model the registered worker advertises.
func TestProtocolModelsListReflectsWorkers(t *testing.T) {
	e := startProtocolEnv(t, 18226, 18227, 18228)
	routerBase := "http://127.0.0.1:18226"

	waitForProtocolModel(t, routerBase, "other-model", 15*time.Second, e)

	resp, err := http.Get(routerBase + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	_ = resp.Body.Close()

	found := false
	for _, m := range models.Data {
		if m.Name == "other-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("/v1/models does not list the worker's model 'other-model': %+v", models.Data)
	}
}

// TestProtocolWorkerOptOut verifies capability-level opt-out: a worker started
// with --allowed-models serving only that model; a request for the whitelisted
// model is served, while a model outside the whitelist is rejected with 503
// (no worker willing) even though the worker is registered.
func TestProtocolWorkerOptOut(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	workerBin := "../../bin/infermesh-worker"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found (run `make build`)", routerBin)
	}
	if _, err := os.Stat(workerBin); err != nil {
		t.Skipf("skipping integration test: binary %s not found (run `make build`)", workerBin)
	}
	const routerPort = 18231
	const workerPort = 18232
	const mockPort = 18233
	routerBase := "http://127.0.0.1:" + strconv.Itoa(routerPort)
	mockBase := "http://127.0.0.1:" + strconv.Itoa(mockPort)

	env := &protocolEnv{}
	env.mock = startMockBackend(t, mockPort)
	t.Cleanup(env.stop)

	env.router = exec.Command(routerBin, "--dev-mode", "--addr", "127.0.0.1:"+strconv.Itoa(routerPort))
	env.router.Stderr = &env.routerErr
	if err := env.router.Start(); err != nil {
		t.Fatalf("starting router: %v", err)
	}
	waitForProtocolRouter(t, routerBase, 10*time.Second)

	// The mock advertises "other-model"; restrict the worker to it via
	// whitelist so any other model is opted out.
	env.worker = exec.Command(workerBin, "--dev-mode",
		"--router", routerBase,
		"--port", strconv.Itoa(workerPort),
		"--backend", "custom",
		"--backend-url", mockBase,
		"--allowed-models", "other-model",
	)
	env.worker.Stdout = &bytes.Buffer{}
	env.worker.Stderr = &env.workerErr
	if err := env.worker.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	waitForProtocolWorker(t, routerBase, workerPort, 15*time.Second)
	waitForProtocolModel(t, routerBase, "other-model", 15*time.Second, env)

	// Whitelisted model: served by the worker (200 + mock marker).
	code, body := postProtocolChat(routerBase, "other-model")
	if code != http.StatusOK || !strings.Contains(body, "failover-verified") {
		t.Fatalf("whitelisted model: expected 200 with marker, got %d %s (router: %s, worker: %s)",
			code, body, env.routerErr.String(), env.workerErr.String())
	}

	// Non-whitelisted model: no worker willing -> 503.
	code, body = postProtocolChat(routerBase, "not-allowed-model")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("opted-out model: expected 503, got %d: %s (router: %s, worker: %s)",
			code, body, env.routerErr.String(), env.workerErr.String())
	}
}

// TestProtocolWorkerUnregister verifies that when the worker process stops
// (SIGTERM), it drops out of the router's /v1/workers list. The router prunes
// stale workers on its registry sweep (RemoveTTL 120s default), so the removal
// is observed by the in-process registry test in registry_scheduler_test.go;
// here we assert the worker's registration state flips from "listed" to
// "not listed as available" promptly — the router's /v1/workers only surfaces
// workers from the capability cache / available registry, and a dead worker's
// entry ages out on heartbeat staleness.
//
// To keep the suite fast, this test stops the worker and then polls /v1/workers
// for up to 90s: the router marks the worker unavailable after UnavailableTTL
// (60s) which removes it from ListAvailable (and the capability cache
// availability). The assertion is intentionally a bounded wait so a slow
// environment does not fail the run spuriously — it fails only if the worker
// is STILL listed as "available" after the unavailable window.
func TestProtocolWorkerUnregister(t *testing.T) {
	e := startProtocolEnv(t, 18236, 18237, 18238)
	routerBase := "http://127.0.0.1:18236"

	// Confirm the worker is listed as available before stopping it.
	waitForProtocolWorker(t, routerBase, 18237, 5*time.Second)

	if err := e.worker.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM to worker: %v", err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- e.worker.Wait() }()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Logf("worker exited with: %v (router stderr: %s)", err, e.routerErr.String())
		}
	case <-time.After(10 * time.Second):
		_ = e.worker.Process.Kill()
		t.Fatalf("worker did not exit within 10s of SIGTERM")
	}

	// Poll /v1/workers: the worker must stop being reported as available
	// within UnavailableTTL (60s) + sweep margin. We stop the worker's
	// cleanup so e.stop() does not re-signal an already-dead worker.
	deadline := time.Now().Add(75 * time.Second)
	for {
		resp, err := http.Get(routerBase + "/v1/workers")
		if err == nil && resp.StatusCode == http.StatusOK {
			var body protocolWorkersResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				_ = resp.Body.Close()
				stillAvailable := false
				for _, w := range body.Workers {
					if w.Port == 18237 && w.Status == "available" {
						stillAvailable = true
						break
					}
				}
				if !stillAvailable {
					return
				}
			} else {
				_ = resp.Body.Close()
			}
		} else if err == nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker on port 18237 still listed as available after %s", 75*time.Second)
		}
		time.Sleep(2 * time.Second)
	}
}

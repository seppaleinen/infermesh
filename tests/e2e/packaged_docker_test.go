package e2e

// End-to-end tests against the packaged Docker images
// (ghcr.io/seppaleinen/infermesh/{router,worker}:latest).
//
// Mirrors the assertions of TestRouterWorkerE2E (router + worker full flow)
// but runs the shipped images instead of locally built binaries.
//
// Topology (all containers on a dedicated bridge network):
//
//	router container  (0.0.0.0:8080, port-mapped to 127.0.0.1:8480)
//	worker container  (shares the mock container's netns via
//	                  --network=container: so it reaches the mock on
//	                  127.0.0.1 and registers against the router via its
//	                  network IP)
//	mock container    (python:3-alpine running the repo's mock_backend.py,
//	                  healthy mode — the only inference engine used)
//
// Notes:
//   - Flags are passed as appended args to `docker run`, NOT via the
//     ROUTER_ARGS/WORKER_ARGS env vars: the images'
//     ENTRYPOINT ["/router"] + CMD ["/bin/sh","-c","/router $ROUTER_ARGS"]
//     form does not expand the env var at runtime (verified: the router
//     ignores ROUTER_ARGS and listens on the default :8080).
//   - Containers are addressed by network IP (from `docker inspect`) rather
//     than by container name: embedded-DNS name resolution is unreliable on
//     some Docker backends (e.g. Docker Desktop/colima VMs).
//   - Skips gracefully when docker is unavailable or the images cannot be
//     pulled (no network / no registry access).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	dockerRouterImage = "ghcr.io/seppaleinen/infermesh/router:latest"
	dockerWorkerImage = "ghcr.io/seppaleinen/infermesh/worker:latest"

	dockerRouterHostPort = 8480 // host-side mapped port for the router
)

// dockerAvailable returns nil when the docker CLI and a reachable daemon are
// both present.
func dockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found: %w", err)
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		return fmt.Errorf("docker daemon unreachable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerEnsureImage pulls the image if it is not already present. A pull
// failure (no network, no registry auth) is returned so callers can skip.
func dockerEnsureImage(image string) error {
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return nil // already present
	}
	fmt.Fprintf(os.Stderr, "docker image %s not present locally, pulling...\n", image)
	out, err := exec.Command("docker", "pull", image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker pull %s: %v (%s)", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerCreateNetwork creates a fresh bridge network with a test-unique name.
func dockerCreateNetwork(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("imesh-e2e-%d", os.Getpid())
	out, err := exec.Command("docker", "network", "create", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker network create %s: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "network", "rm", name).Run()
	})
	t.Logf("created docker network %s: %s", name, strings.TrimSpace(string(out)))
	return name
}

// dockerRunDetached starts a container on the given network (extra run args
// may override -p mappings etc.) and registers its removal on cleanup.
func dockerRunDetached(t *testing.T, name string, args ...string) {
	t.Helper()
	full := append([]string{"run", "-d", "--name", name}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v (%s)", strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	t.Logf("started container %s: %s", name, strings.TrimSpace(string(out)))
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "rm", "-f", name).CombinedOutput()
		t.Logf("removed container %s: %s", name, strings.TrimSpace(string(out)))
	})
}

// copyFile copies src to dst using only the standard library.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// dockerContainerIP returns the container's IPv4 address on the network.
// It parses `docker inspect <container>` output (a JSON array with a single
// element) using only the standard library. It retries for a short window
// because a freshly started container may not yet have an IP assigned.
func dockerContainerIP(t *testing.T, container, network string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("docker inspect %s: no IPv4 address on network %q after 10s", container, network)
		}
		out, err := exec.Command("docker", "inspect", container).Output()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var inspected []struct {
			NetworkSettings struct {
				Networks map[string]struct {
					IPAddress string `json:"IPAddress"`
				} `json:"Networks"`
			} `json:"NetworkSettings"`
		}
		if err := json.Unmarshal(out, &inspected); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if len(inspected) > 0 {
			if nw, ok := inspected[0].NetworkSettings.Networks[network]; ok && nw.IPAddress != "" {
				return nw.IPAddress
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dockerLogs returns the container's logs for failure diagnostics.
func dockerLogs(t *testing.T, container string) string {
	t.Helper()
	out, _ := exec.Command("docker", "logs", container).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// waitForHTTP200 polls the URL until it returns HTTP 200 or the timeout
// expires. Returns the final status code observed (0 if never reached).
func waitForHTTP200(t *testing.T, url string, timeout time.Duration) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	last := 0
	for {
		if time.Now().After(deadline) {
			return last
		}
		resp, err := client.Get(url)
		if err == nil {
			last = resp.StatusCode
			_ = resp.Body.Close()
			if last == http.StatusOK {
				return last
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestRouterWorkerDockerE2E verifies that the packaged Docker images work
// together: the worker container self-registers against the router container
// over HTTP, the OpenAI endpoints respond, and both streaming and
// non-streaming chat completions reach the (mocked) backend. No real LLM
// engine is used — the repo's mock_backend.py runs in its own container.
func TestRouterWorkerDockerE2E(t *testing.T) {
	if err := dockerAvailable(); err != nil {
		t.Skipf("skipping docker e2e: %v", err)
	}
	for _, img := range []string{dockerRouterImage, dockerWorkerImage} {
		if err := dockerEnsureImage(img); err != nil {
			t.Skipf("skipping docker e2e: %v", err)
		}
	}

	mockScript := "mock_backend.py"
	if _, err := os.Stat(mockScript); err != nil {
		t.Skipf("skipping docker e2e: mock backend script %s not found: %v", mockScript, err)
	}

	network := dockerCreateNetwork(t)
	routerBase := fmt.Sprintf("http://127.0.0.1:%d", dockerRouterHostPort)

	// 1. Mock backend container (healthy mode). It binds 127.0.0.1 inside its
	//    own netns; the worker joins that netns (--network=container:) so it
	//    reaches the mock at http://127.0.0.1:8197 without modifying the
	//    script's bind address.
	//
	// The mock script is mounted from a path shared with the Docker daemon
	// (the repo dir). t.TempDir() / os.MkdirTemp("") live under /var/folders
	// on macOS, which is NOT shared with the Docker VM — a volume bind there
	// silently fails and the container exits with "can't open file". Copying
	// into a subdir of the repo dir first keeps the mount source on a shared
	// path. The test runs from tests/e2e/, so mockScript is relative to that.
	mockDir, err := os.MkdirTemp(filepath.Dir(mockScript), "imesh-mock")
	if err != nil {
		t.Fatalf("creating mock mount dir: %v", err)
	}
	mockDir, err = filepath.Abs(mockDir)
	if err != nil {
		t.Fatalf("resolving mock mount dir: %v", err)
	}
	// Clean up the temp mock dir on test teardown (issue #121).
	// Uses RemoveAll so a killed/failed run doesn't leak imesh-mock* dirs
	// into tests/e2e/.
	t.Cleanup(func() { _ = os.RemoveAll(mockDir) })
	mockScriptPath := filepath.Join(mockDir, "mock_backend.py")
	if err := copyFile(mockScript, mockScriptPath); err != nil {
		t.Fatalf("copying mock backend script: %v", err)
	}
	mockPort := 8197
	mockArgs := []string{
		"--name", "imesh-mock",
		"--network", network,
		"-v", mockDir + ":/mock:ro",
		"python:3-alpine",
		"python3", "/mock/mock_backend.py",
		"--port", fmt.Sprintf("%d", mockPort),
		"--mode", "healthy",
	}
	dockerRunDetached(t, "imesh-mock", mockArgs...)
	mockIP := dockerContainerIP(t, "imesh-mock", network)

	// Give the mock a moment to bind the port.
	waitForHTTP200(t, fmt.Sprintf("http://%s:%d/v1/models", mockIP, mockPort), 10*time.Second)

	// 2. Router container: listen on 0.0.0.0:8080 inside the container,
	//    port-mapped to 127.0.0.1:8480 on the host.
	routerArgs := []string{
		"--name", "imesh-router",
		"--network", network,
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", dockerRouterHostPort),
		dockerRouterImage,
		"--dev-mode",
		"--addr", "0.0.0.0:8080",
	}
	dockerRunDetached(t, "imesh-router", routerArgs...)
	routerIP := dockerContainerIP(t, "imesh-router", network)

	// Wait for the router HTTP server to come up (via the host-side mapping).
	if code := waitForHTTP200(t, routerBase+"/v1/workers", 15*time.Second); code != http.StatusOK {
		t.Fatalf("router not ready after 15s (last status: %d, logs: %s)", code, dockerLogs(t, "imesh-router"))
	}

	// 3. Worker container: shares the mock's netns so it reaches the mock on
	//    127.0.0.1:8197. Registers against the router via the router's
	//    network IP. Uses the "custom" backend pointed at the mock.
	workerArgs := []string{
		"--name", "imesh-worker",
		"--network", "container:imesh-mock",
		dockerWorkerImage,
		"--dev-mode",
		"--router", fmt.Sprintf("http://%s:8080", routerIP),
		"--port", "8081",
		"--backend", "custom",
		"--backend-url", fmt.Sprintf("http://127.0.0.1:%d", mockPort),
	}
	dockerRunDetached(t, "imesh-worker", workerArgs...)

	// The worker's advertised IP is the mock container's IP (shared netns).
	workerIP := mockIP // worker shares the mock's netns

	// 4. Wait for the worker to register (poll /v1/workers for the worker's
	//    advertised port 8081).
	body := waitForWorker(t, routerBase, 8081, 20*time.Second)
	if len(body.Workers) == 0 {
		t.Fatalf("no workers registered after 20s (router logs: %s, worker logs: %s)",
			dockerLogs(t, "imesh-router"), dockerLogs(t, "imesh-worker"))
	}
	for _, w := range body.Workers {
		t.Logf("registered worker: %v", w)
	}
	t.Logf("worker IP: %s (shared netns with mock container)", workerIP)

	// 5. OpenAI endpoints respond.
	for _, endpoint := range []string{"/v1/models", "/v1/workers"} {
		resp, err := http.Get(routerBase + endpoint)
		if err != nil {
			t.Fatalf("GET %s: %v", endpoint, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: unexpected status %d (router logs: %s)", endpoint, resp.StatusCode, dockerLogs(t, "imesh-router"))
		}
		_ = resp.Body.Close()
	}

	// 6. Wait for the mock's model to be advertised. mock_backend.py lists
	//    "other-model" as its model.
	waitForModel(t, routerBase, "other-model", 20*time.Second)

	// 7. Non-streaming chat completion: application/json, HTTP 200, mock
	//    content marker.
	chatBody := `{"model":"other-model","messages":[{"role":"user","content":"hi"}],"stream":false}`
	chatResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer func() { _ = chatResp.Body.Close() }()
	if chatResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(chatResp.Body)
		t.Fatalf("non-streaming chat: expected 200, got %d (worker logs: %s, body: %s)",
			chatResp.StatusCode, dockerLogs(t, "imesh-worker"), string(b))
	}
	if ct := chatResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("non-streaming chat content-type = %q, want application/json", ct)
	}
	raw, _ := io.ReadAll(chatResp.Body)
	if !strings.Contains(string(raw), "failover-verified") {
		t.Errorf("non-streaming response missing mock marker 'failover-verified': %s", string(raw))
	}

	// 8. Streaming chat completion: text/event-stream, HTTP 200.
	streamBody := `{"model":"other-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	streamResp, err := http.Post(routerBase+"/v1/chat/completions", "application/json", strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("streaming chat: expected 200, got %d (worker logs: %s, body: %s)",
			streamResp.StatusCode, dockerLogs(t, "imesh-worker"), string(b))
	}
	if ct := streamResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("streaming chat content-type = %q, want text/event-stream", ct)
	}
}

package e2e

// End-to-end test for the desktop→router auth flow (issue #123).
//
// The regression fixed in #119 (desktop RouterClient not sending X-API-Key to
// a prod-mode router) was caught only after merging #105; this test pins the
// contract down: a prod-mode router with --api-key + mTLS must return 200 —
// not 401 — on every endpoint the desktop RouterClient calls when the client
// presents a trusted mTLS client cert AND sends X-API-Key, and the enforced
// endpoint (/meta/aliases) must return 401 when the key is missing.
//
// The desktop app/ package is a SEPARATE Go module (package main) and cannot
// be imported from tests/e2e. So this test replicates what
// Supervisor.updateRouterClientTransport + RouterClient.applyAuth do:
//
//  1. an mTLS *http.Client with a client cert trusted by the router's CA pool
//     and the router's server cert verified against the same CA pool (mirrors
//     app.Supervisor.buildTLSClient, here via makeMTLSClient), and
//  2. an X-API-Key header on every request (mirrors RouterClient.applyAuth —
//     the desktop sends it on all four endpoints regardless of whether the
//     router enforces it, defensively).
//
// The four endpoints under test are exactly the ones RouterClient calls:
//
//	GET  /v1/workers            → GetWorkers
//	GET  /meta/models/popular   → GetPopularModels
//	POST /meta/aliases          → SetModelAliases
//	GET  /meta/connections/count → GetConnectionCount
//
// Note: only /meta/aliases of the four is in the router's API-key
// enforcement list (wrapInferenceEndpoints: /v1/chat/completions,
// /v1/completions, /meta/aliases). The other three return 200 without a key —
// which is why the negative case targets /meta/aliases.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// desktopAuthPorts holds the ports for this test. Distinct from every other
// e2e test (18401-18404, 18081-18094, 18101-18203, 8080-8105).
const desktopAuthRouterPort = 18501

// waitForHTTPMTLS polls a router endpoint with an mTLS client until it
// returns a successful status, or the timeout expires.
func waitForHTTPMTLS(t *testing.T, client *http.Client, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for mTLS %s", url)
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// desktopAuthRequest performs one request against the prod router the way
// RouterClient does: X-API-Key set when apiKey is non-empty (applyAuth is a
// no-op for an empty key), over the shared mTLS client. Returns the status
// code and the raw body for assertions.
func desktopAuthRequest(t *testing.T, client *http.Client, method, url, apiKey, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		t.Fatalf("failed to build %s request: %v", method, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey) // RouterClient.applyAuth
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response body from %s: %v", url, err)
	}
	return resp.StatusCode, string(raw)
}

// TestDesktopProdAuthFlow verifies the desktop→router auth flow in prod mode
// (issue #123).
func TestDesktopProdAuthFlow(t *testing.T) {
	routerBin := "../../bin/infermesh-router"
	if _, err := os.Stat(routerBin); err != nil {
		t.Skipf("skipping e2e: binary %s not found (run `make build` first): %v", routerBin, err)
	}

	const apiKey = "desktop-prod-auth-key-123"
	routerBase := fmt.Sprintf("https://127.0.0.1:%d", desktopAuthRouterPort)

	certs := generateProdMTLSCerts(t)

	// Mint the desktop's client cert with the CN the desktop actually uses.
	// app/config.go defaults ClientCertCN to "infermesh-desktop", and
	// app/args.go::buildRouterArgs() launches the router WITHOUT --trusted-cn,
	// so the router accepts any CA-signed cert (no CN allowlist). This mirrors
	// the real desktop flow; the stricter --trusted-cn=router-1,worker-1
	// variant is exercised by TestProdMTLS_e2e.
	clientDir := t.TempDir()
	clientCert, clientKey, err := security.GenerateNodeCert(clientDir, certs.caCert, certs.caKey, "infermesh-desktop")
	if err != nil {
		t.Fatalf("failed to generate desktop client cert: %v", err)
	}

	// Start router in prod mode. Mirrors what app/args.go::buildRouterArgs()
	// actually emits: --prod-mode, --addr, --mtls-cert/--mtls-key, --cert-dir,
	// --api-key. No --trusted-cn: the router accepts any CA-signed cert, so
	// the desktop's client cert (CN "infermesh-desktop") is admitted.
	// No worker is started: all four endpoints are router-side and respond
	// with 200 on an idle pool, which keeps the test fast and deterministic.
	routerCmd, routerLog := startCmdLog(t, "router", routerBin,
		"--prod-mode",
		"--addr", fmt.Sprintf("127.0.0.1:%d", desktopAuthRouterPort),
		"--mtls-cert", certs.routerCert,
		"--mtls-key", certs.routerKey,
		"--cert-dir", certs.caDir,
		"--api-key", apiKey,
	)

	// Build the mTLS client the way the desktop does: client cert trusted by
	// the router's CA pool, server cert verified against the same CA pool,
	// no InsecureSkipVerify (mirrors app.Supervisor.buildTLSClient via
	// makeMTLSClient).
	mtlsClient := makeMTLSClient(t, clientCert, clientKey, certs.caCert)

	// Wait for the router to answer /v1/workers over mTLS — proves the prod
	// TLS config is active, not merely that the port is bound.
	waitForHTTPMTLS(t, mtlsClient, routerBase+"/v1/workers", 15*time.Second)

	// ===== all four desktop endpoints return 200 with a valid cert + key =====
	t.Run("all_endpoints_200_with_api_key", func(t *testing.T) {
		status, body := desktopAuthRequest(t, mtlsClient, http.MethodGet, routerBase+"/v1/workers", apiKey, "")
		if status != http.StatusOK {
			t.Fatalf("GET /v1/workers: expected 200, got %d: %s", status, body)
		}
		var workers struct {
			Workers []map[string]any `json:"workers"`
		}
		if err := json.Unmarshal([]byte(body), &workers); err != nil {
			t.Fatalf("GET /v1/workers: unparseable body %q: %v", body, err)
		}

		status, body = desktopAuthRequest(t, mtlsClient, http.MethodGet, routerBase+"/meta/models/popular", apiKey, "")
		if status != http.StatusOK {
			t.Fatalf("GET /meta/models/popular: expected 200, got %d: %s", status, body)
		}
		var popular struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(body), &popular); err != nil {
			t.Fatalf("GET /meta/models/popular: unparseable body %q: %v", body, err)
		}

		// POST /meta/aliases is the enforced endpoint: the desktop sends the
		// FULL alias map (replace-all); an empty map disables aliasing.
		status, body = desktopAuthRequest(t, mtlsClient, http.MethodPost, routerBase+"/meta/aliases", apiKey, `{"aliases":{}}`)
		if status != http.StatusOK {
			t.Fatalf("POST /meta/aliases: expected 200, got %d: %s", status, body)
		}
		var aliasResp struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal([]byte(body), &aliasResp); err != nil {
			t.Fatalf("POST /meta/aliases: unparseable body %q: %v", body, err)
		}
		if !aliasResp.OK {
			t.Fatalf("POST /meta/aliases: expected ok=true, got body %q", body)
		}

		status, body = desktopAuthRequest(t, mtlsClient, http.MethodGet, routerBase+"/meta/connections/count", apiKey, "")
		if status != http.StatusOK {
			t.Fatalf("GET /meta/connections/count: expected 200, got %d: %s", status, body)
		}
		var count struct {
			Connections int `json:"connections"`
		}
		if err := json.Unmarshal([]byte(body), &count); err != nil {
			t.Fatalf("GET /meta/connections/count: unparseable body %q: %v", body, err)
		}
		if count.Connections < 1 {
			t.Fatalf("GET /meta/connections/count: expected >= 1 active connection, got %d", count.Connections)
		}
	})

	// ===== negative: valid client cert, missing X-API-Key → 401 =====
	// This is the exact bug #119 fixed: before the fix the desktop sent no
	// X-API-Key at all, so the enforced endpoint 401'd while the desktop's
	// other calls (not enforced) masked the problem. /meta/aliases is the
	// right negative-case target — the other three of the four are NOT in
	// wrapInferenceEndpoints and would return 200 without a key.
	t.Run("missing_api_key_rejected", func(t *testing.T) {
		status, body := desktopAuthRequest(t, mtlsClient, http.MethodPost, routerBase+"/meta/aliases", "", `{"aliases":{}}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("POST /meta/aliases without X-API-Key: expected 401, got %d: %s", status, body)
		}
	})

	// ===== negative: valid client cert, wrong X-API-Key → 401 =====
	t.Run("wrong_api_key_rejected", func(t *testing.T) {
		status, body := desktopAuthRequest(t, mtlsClient, http.MethodPost, routerBase+"/meta/aliases", "wrong-key", `{"aliases":{}}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("POST /meta/aliases with wrong X-API-Key: expected 401, got %d: %s", status, body)
		}
	})

	// ===== Clean SIGTERM shutdown of prod process =====
	for _, cmd := range []*exec.Cmd{routerCmd} {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("sending SIGTERM: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected clean exit 0 after SIGTERM, got: %v (router: %s)", err, routerLog.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("router did not exit within 10s of SIGTERM (router: %s)", routerLog.String())
		}
	}
}

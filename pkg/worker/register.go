package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

const DefaultRegisterInterval = 10 * time.Second

// RegisterLoop posts the worker's info to the router's dev registration endpoint
// at the given interval. It runs until ctx is cancelled. Errors are logged but
// do not terminate the loop (transient failures are expected).
// infoFn is called on every tick (including the initial registration) so
// heartbeats carry fresh capabilities/models instead of a stale startup snapshot.
//
// routerAPIKey is the key the router expects on /v1/dev/register (the same
// --api-key the router was started with). When non-empty, the worker sends
// X-API-Key so the router's API-key middleware admits the registration.
// Without it, a key-gated router rejects every fallback registration with 401
// and the worker never joins the pool (issue #202).
func RegisterLoop(ctx context.Context, routerBase string, info protocol.WorkerInfo, interval time.Duration, log *slog.Logger, routerAPIKey string) {
	RegisterLoopWithRefresh(ctx, routerBase, func() protocol.WorkerInfo { return info }, interval, log, routerAPIKey)
}

// RegisterLoopWithRefresh is RegisterLoop with a dynamic info provider.
// Use it so each heartbeat reflects currently discovered models/capabilities.
//
// routerAPIKey is threaded through to postRegister (see RegisterLoop).
func RegisterLoopWithRefresh(ctx context.Context, routerBase string, infoFn func() protocol.WorkerInfo, interval time.Duration, log *slog.Logger, routerAPIKey string) {
	// Register immediately on start
	postRegister(ctx, routerBase, infoFn(), log, routerAPIKey)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Double-select: the inner `default` ensures ctx.Done() is checked
	// before a possibly-stale ticker.C is dequeued, so cancellation is
	// observed promptly rather than waiting for the next tick. Even so, a
	// call already dequeued may complete after cancel() returns — postRegister
	// short-circuits on ctx.Err() to minimise that window.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			postRegister(ctx, routerBase, infoFn(), log, routerAPIKey)
		}
	}
}

// registerURL builds the full registration endpoint URL from a router base,
// trimming any trailing slash to avoid double-slash paths (e.g. "http://host:8080/"
// would otherwise produce "http://host:8080//v1/dev/register").
func registerURL(routerBase string) string {
	return strings.TrimRight(routerBase, "/") + "/v1/dev/register"
}

// postRegister sends a single POST to the router's /v1/dev/register endpoint.
//
// routerAPIKey is the key the router expects on /v1/dev/register (the same
// --api-key the router was started with). When non-empty, the worker sends
// X-API-Key so the router's API-key middleware admits the registration; when
// empty, no header is sent, preserving the pre-existing behavior for routers
// that do not gate the endpoint with a key.
func postRegister(ctx context.Context, routerBase string, info protocol.WorkerInfo, log *slog.Logger, routerAPIKey string) {
	// Short-circuit if the context is already done: the select in
	// RegisterLoopWithRefresh may have dequeued a ticker.C before ctx was
	// cancelled, so a call can be in-flight when cancel() returns. Without
	// this guard the in-flight POST still goes out and the loop's "no more
	// calls after cancel" invariant is untestable (racy by construction).
	if ctx.Err() != nil {
		return
	}
	body, err := json.Marshal(info)
	if err != nil {
		log.Warn("failed to marshal worker info for dev register", "error", err)
		return
	}

	url := registerURL(routerBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Warn("failed to create dev register request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if routerAPIKey != "" {
		req.Header.Set("X-API-Key", routerAPIKey)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("dev register request failed", "url", url, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		log.Warn("dev register returned non-200", "url", url, "status", resp.StatusCode)
		return
	}

	log.Info("worker registered via dev endpoint", "worker_id", info.ID, "url", url)
}

// postRegisterMTLS sends a single registration POST to therouter over mTLS.
// The worker presents its client certificate (signed by the shared CA) and
// verifies the router's server certificate against the CA pool. This is the
// prod-mode counterpart of postRegister: the router's /v1/dev/register endpoint
// is wrapped by the mTLS middleware, so plain HTTP is rejected with 403.
//
// routerAPIKey is the key the router expects on /v1/dev/register (the same
// --api-key the router was started with). When non-empty, the worker sends
// X-API-Key so the router's API-key middleware admits the registration.
func postRegisterMTLS(ctx context.Context, routerBase string, info protocol.WorkerInfo, certFile, keyFile, caCertFile, routerAPIKey string, log *slog.Logger) {
	// See postRegister: the select in RegisterLoopWithRefreshMTLS can dequeue
	// a ticker.C after ctx was cancelled, so an in-flight POST is possible at
	// cancel time. Short-circuit to keep the "no calls after cancel"
	// invariant testable.
	if ctx.Err() != nil {
		return
	}
	body, err := json.Marshal(info)
	if err != nil {
		log.Warn("failed to marshal worker info for mTLS register", "error", err)
		return
	}

	url := registerURL(routerBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Warn("failed to create mTLS register request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if routerAPIKey != "" {
		req.Header.Set("X-API-Key", routerAPIKey)
	}

	client, err := mtlsClient(certFile, keyFile, caCertFile)
	if err != nil {
		log.Warn("failed to create mTLS client for register", "error", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Warn("mTLS register request failed", "url", url, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		log.Warn("mTLS register returned non-200", "url", url, "status", resp.StatusCode)
		return
	}

	log.Info("worker registered via mTLS endpoint", "worker_id", info.ID, "url", url)
}

// RegisterLoopWithRefreshMTLS is RegisterLoopWithRefresh with an mTLS client.
// It runs the same immediate-then-tick loop but dials the router over HTTPS
// with client-certificate verification.
//
// routerAPIKey is the key the router expects on /v1/dev/register (the same
// --api-key the router was started with). When non-empty, the worker sends
// X-API-Key so the router's API-key middleware admits the registration.
func RegisterLoopWithRefreshMTLS(ctx context.Context, routerBase string, infoFn func() protocol.WorkerInfo, interval time.Duration, certFile, keyFile, caCertFile, routerAPIKey string, log *slog.Logger) {
	postRegisterMTLS(ctx, routerBase, infoFn(), certFile, keyFile, caCertFile, routerAPIKey, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Double-select (see RegisterLoopWithRefresh): check ctx.Done()
	// before dequeuing a possibly-stale ticker.C.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			postRegisterMTLS(ctx, routerBase, infoFn(), certFile, keyFile, caCertFile, routerAPIKey, log)
		}
	}
}

// mtlsClient builds an http.Client that presents the worker's client
// certificate and verifies the router's server certificate against the CA
// pool. It fails closed: if the cert/key/CA cannot be loaded it returns an
// error so the caller can log and skip the tick rather than silently
// registering over plain HTTP.
func mtlsClient(certFile, keyFile, caCertFile string) (*http.Client, error) {
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse ca.crt into cert pool")
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load worker client cert: %w", err)
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
				// Prod is single-machine loopback-only: the worker dials
				// 127.0.0.1 (IPv4, guaranteed connectable) but the router's
				// cert SAN list carries "localhost" (DNS) + the primary
				// IPv4, never 127.0.0.1. Pin ServerName to "localhost"
				// so verification matches the cert's DNS SAN.
				ServerName: "localhost",
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}

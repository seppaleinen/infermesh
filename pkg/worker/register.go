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
func RegisterLoop(ctx context.Context, routerBase string, info protocol.WorkerInfo, interval time.Duration, log *slog.Logger) {
	RegisterLoopWithRefresh(ctx, routerBase, func() protocol.WorkerInfo { return info }, interval, log)
}

// RegisterLoopWithRefresh is RegisterLoop with a dynamic info provider.
// Use it so each heartbeat reflects currently discovered models/capabilities.
func RegisterLoopWithRefresh(ctx context.Context, routerBase string, infoFn func() protocol.WorkerInfo, interval time.Duration, log *slog.Logger) {
	// Register immediately on start
	postRegister(ctx, routerBase, infoFn(), log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			postRegister(ctx, routerBase, infoFn(), log)
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
func postRegister(ctx context.Context, routerBase string, info protocol.WorkerInfo, log *slog.Logger) {
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

// postRegisterMTLS sends a single registration POST to the router over mTLS.
// The worker presents its client certificate (signed by the shared CA) and
// verifies the router's server certificate against the CA pool. This is the
// prod-mode counterpart of postRegister: the router's /v1/dev/register endpoint
// is wrapped by the mTLS middleware, so plain HTTP is rejected with 403.
func postRegisterMTLS(ctx context.Context, routerBase string, info protocol.WorkerInfo, certFile, keyFile, caCertFile string, log *slog.Logger) {
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
func RegisterLoopWithRefreshMTLS(ctx context.Context, routerBase string, infoFn func() protocol.WorkerInfo, interval time.Duration, certFile, keyFile, caCertFile string, log *slog.Logger) {
	postRegisterMTLS(ctx, routerBase, infoFn(), certFile, keyFile, caCertFile, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			postRegisterMTLS(ctx, routerBase, infoFn(), certFile, keyFile, caCertFile, log)
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

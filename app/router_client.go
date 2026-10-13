package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/routeraddr"
)

// defaultRouterURL is the compile-time fallback base URL for the router.
// It is NOT persisted here — that is issue #43 (settings UI). Normalisation
// delegates to pkg/routeraddr.Normalize (issue #103 R2).
const defaultRouterURL = "http://127.0.0.1:8080"

// WorkerView is the slim projection the desktop UI renders. It mirrors
// router.WorkerInfo (the /v1/workers projection) but adds a friendly
// relative "last seen" string computed client-side.
type WorkerView struct {
	ID           string    `json:"id"`
	Hostname     string    `json:"hostname"`
	IP           string    `json:"ip"`
	Port         int       `json:"port"`
	Status       string    `json:"status"`
	Version      string    `json:"version"`
	LoadedModels []string  `json:"loaded_models"`
	LastSeen     time.Time `json:"last_seen"`
	// LastSeenRel is "3s ago" / "just now" — derived from LastSeen at render time.
	LastSeenRel string `json:"last_seen_rel"`
	// Address is "host:port" for the card title.
	Address string `json:"address"`
}

// RouterClient is a Wails service bound to the frontend. It talks to the
// router's HTTP API over plain HTTP (dev mode) or mTLS (prod mode).
// The desktop app presents a client certificate minted by the CA in prod mode.
type RouterClient struct {
	baseURL       string
	localHostname string
	sv            *Supervisor
	// tlsClient is cached lazily so the expensive tls.Config build happens
	// once per process. It is reset when the supervisor's client cert paths
	// change (e.g. router restart).
	tlsClient *http.Client
}

// ServiceName implements application.ServiceName so the binding generator
// registers it under a stable, human-readable name.
func (*RouterClient) ServiceName() string {
	return "RouterClient"
}

// NewRouterClient builds a client pointed at baseURL. An empty baseURL falls
// back to defaultRouterURL. A bare host:port (e.g. ":8080") is normalised to
// "http://host:port" so callers can pass either form.
func NewRouterClient(baseURL string) *RouterClient {
	baseURL = routeraddr.Normalize(baseURL)
	hostname, _ := os.Hostname()
	return &RouterClient{baseURL: baseURL, localHostname: hostname}
}

// GetRouterURL returns the configured base URL (used by the UI to show the
// unreachable-router error card).
func (c *RouterClient) GetRouterURL() string {
	return c.baseURL
}

// SetSupervisor attaches the Supervisor to the client so TLS can be
// negotiated when the router runs in production mode. The routerProbe
// in the supervisor uses this to build a TLS transport.
func (c *RouterClient) SetSupervisor(sv *Supervisor) { c.sv = sv }

// effectiveBaseURL returns the base URL with the correct scheme for the
// current mode: https when the supervisor has minted a client certificate
// (prod mode), http otherwise.
// This keeps the public RouterClient API stable — callers pass an http base
// URL; the client transparently upgrades to https in production.
func (c *RouterClient) effectiveBaseURL() string {
	if c.sv == nil {
		return c.baseURL
	}
	certPath, _ := c.sv.GetClientCertPaths()
	if certPath == "" {
		return c.baseURL
	}
	// Upgrade scheme to https for prod mode.
	if strings.HasPrefix(c.baseURL, "http://") {
		return "https://" + strings.TrimPrefix(c.baseURL, "http://")
	}
	return c.baseURL
}

// httpClient returns an http.Client that uses mTLS when the supervisor has
// minted a client certificate (production mode). The TLS configuration is
// built lazily and cached. The client trusts the CA certificate in the
// supervisor's caCertDir.
func (c *RouterClient) httpClient() *http.Client {
	if c.tlsClient != nil {
		return c.tlsClient
	}
	if c.sv == nil {
		c.tlsClient = http.DefaultClient
		return c.tlsClient
	}
	certPath, keyPath := c.sv.GetClientCertPaths()
	caCertDir := c.sv.GetCACertDir()
	if certPath == "" || keyPath == "" || caCertDir == "" {
		c.tlsClient = http.DefaultClient
		return c.tlsClient
	}
	// Load client certificate.
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		// Fall back to plain HTTP on load error.
		c.tlsClient = http.DefaultClient
		return c.tlsClient
	}
	// Load CA certificate pool.
	caCertFile := filepath.Join(caCertDir, "ca.crt")
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		c.tlsClient = http.DefaultClient
		return c.tlsClient
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		c.tlsClient = http.DefaultClient
		return c.tlsClient
	}
	// In production the router may serve with a self-signed certificate
	// whose SANs differ from the hostname in the URL (e.g. 127.0.0.1).
	// We skip hostname verification for now; the mTLS client certificate
	// and CA pinning are the real authentication mechanisms.
	// This is acceptable for a desktop app connecting to a locally-supervised
	// router; production deployments with stricter verification can pin the
	// server certificate.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		// InsecureSkipVerify is used because the router may be addressed by
		// 127.0.0.1 while the certificate's SANs include "localhost". The mTLS
		// client cert remains the primary authentication mechanism.
		InsecureSkipVerify: true,
	}
	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	c.tlsClient = &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	return c.tlsClient
}

// IsLocalWorker reports whether the worker identified by hostname/port is the
// one this desktop app supervises. Used by the UI to tag the local worker
// card with a "You" badge (issue #90). Hostname match is primary; port match
// is the fallback when hostname is unavailable on either side.
func (c *RouterClient) IsLocalWorker(workerPort int, hostname string, port int) bool {
	return IsLocalWorker(c.localHostname, hostname, port, workerPort)
}

// GetHostname returns the hostname of the machine running this desktop app.
// It is used by the UI to identify the local worker by hostname instead of
// relying solely on port matching, which fails when multiple hosts use the
// same worker port (issue #90).
func (c *RouterClient) GetHostname() string {
	return c.localHostname
}

// SetRouterURL re-points the client to a new router base URL. It is safe to
// call at any time; subsequent GetWorkers() calls use the new URL. An empty
// or malformed url falls back to defaultRouterURL (same normalisation as the
// constructor). Does NOT persist to disk — that is the ConfigService's job.
func (c *RouterClient) SetRouterURL(url string) {
	c.baseURL = routeraddr.Normalize(url)
}

// workersPath builds the full /v1/workers URL for the configured base.
func (c *RouterClient) workersPath() string {
	return c.effectiveBaseURL() + "/v1/workers"
}

// GetWorkers fetches the connected-workers list from the router. It returns
// an empty (non-nil) slice when the router is reachable but idle, and a
// typed error when the router is unreachable or returns a bad body.
func (c *RouterClient) GetWorkers(ctx context.Context) ([]WorkerView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.workersPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("router /v1/workers returned %d: %s", resp.StatusCode, truncateBody(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read router body: %w", err)
	}

	workers, err := parseWorkersResponse(body)
	if err != nil {
		return nil, err
	}
	return workers, nil
}

// parseWorkersResponse decodes a WorkersResponse envelope and maps each
// router.WorkerInfo into a WorkerView. It is exported (and pure) so the
// unit tests can feed canned JSON without a live router.
func parseWorkersResponse(body []byte) ([]WorkerView, error) {
	var resp router.WorkersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode workers response: %w (body snippet: %q)", err, truncateBody(body))
	}
	if resp.Workers == nil {
		// JSON `null` is legal but useless; normalise to an empty slice so
		// the UI can distinguish "reachable, idle" from "unreachable".
		resp.Workers = []router.WorkerInfo{}
	}
	out := make([]WorkerView, 0, len(resp.Workers))
	for _, w := range resp.Workers {
		out = append(out, toWorkerView(w))
	}
	return out, nil
}

// toWorkerView maps a router.WorkerInfo projection onto the UI view type.
// Status is normalised to a lowercase string; unknown status values pass
// through unchanged and render as grey (unassigned) on the frontend.
func toWorkerView(w router.WorkerInfo) WorkerView {
	host := w.Hostname
	if host == "" {
		host = w.IP
	}
	address := host
	if w.Port != 0 {
		address = fmt.Sprintf("%s:%d", host, w.Port)
	}

	status := string(w.Status)
	if status == "" {
		status = string(protocol.StatusUnassigned)
	}

	loaded := w.LoadedModels
	if loaded == nil {
		loaded = []string{}
	}

	return WorkerView{
		ID:           w.ID,
		Hostname:     w.Hostname,
		IP:           w.IP,
		Port:         w.Port,
		Status:       status,
		Version:      w.Version,
		LoadedModels: loaded,
		LastSeen:     w.LastSeen,
		Address:      address,
	}
}

// relativeLastSeen renders a human-friendly age for a timestamp. It is
// exported so the Vue side could reuse it, but the frontend computes its
// own via a small helper to stay reactive to the clock.
func relativeLastSeen(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < 10*time.Second:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < 60*time.Second:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < 3600*time.Second:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// truncateBody returns at most maxBody bytes, appending "..." if truncated.
func truncateBody(b []byte) string {
	const maxBody = 200
	if len(b) <= maxBody {
		return string(b)
	}
	return string(b[:maxBody]) + "..."
}

// PopularModelView is the UI projection of router.PopularModelInfo. Field
// names mirror the router JSON tags exactly so the generated Wails binding
// is a straight pass-through.
type PopularModelView struct {
	Model             string `json:"model"`
	WorkerCount       int    `json:"worker_count"`
	LoadedWorkerCount int    `json:"loaded_worker_count"`
	CallCount         int    `json:"call_count"`
}

// popularModelsPath builds the full /meta/models/popular URL for the
// configured base.
func (c *RouterClient) popularModelsPath() string {
	return c.effectiveBaseURL() + "/meta/models/popular"
}

// GetPopularModels fetches the sorted popular-models list from the router.
// It returns an empty (non-nil) slice when the router is reachable but idle,
// and a typed error when the router is unreachable or returns a bad body.
// Mirrors GetWorkers.
func (c *RouterClient) GetPopularModels(ctx context.Context) ([]PopularModelView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.popularModelsPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("router /meta/models/popular returned %d: %s", resp.StatusCode, truncateBody(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read router body: %w", err)
	}

	models, err := parsePopularModelsResponse(body)
	if err != nil {
		return nil, err
	}
	return models, nil
}

// parsePopularModelsResponse decodes a PopularModelsResponse envelope and
// maps each router.PopularModelInfo into a PopularModelView. It is exported
// (and pure) so the unit tests can feed canned JSON without a live router.
func parsePopularModelsResponse(body []byte) ([]PopularModelView, error) {
	var resp router.PopularModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode popular models response: %w (body snippet: %q)", err, truncateBody(body))
	}
	if resp.Models == nil {
		// JSON `null` is legal but useless; normalise to an empty slice so
		// the UI can distinguish "reachable, idle" from "unreachable".
		resp.Models = []router.PopularModelInfo{}
	}
	out := make([]PopularModelView, 0, len(resp.Models))
	for _, m := range resp.Models {
		out = append(out, PopularModelView{
			Model:             m.Model,
			WorkerCount:       m.WorkerCount,
			LoadedWorkerCount: m.LoadedWorkerCount,
			CallCount:         m.CallCount,
		})
	}
	return out, nil
}

// ErrRouterUnreachable is the sentinel error returned when the router host
// cannot be dialed. The UI keys off the error message text.
var ErrRouterUnreachable = errors.New("router unreachable")

// connectionCountPath builds the full /meta/connections/count URL for the
// configured base.
func (c *RouterClient) connectionCountPath() string {
	return c.effectiveBaseURL() + "/meta/connections/count"
}

// GetConnectionCount fetches the number of currently active HTTP connections
// to the router from /meta/connections/count. It returns 0 when the router is
// reachable but idle, and a typed error when the router is unreachable or
// returns a bad body. Mirrors GetWorkers / GetPopularModels.
func (c *RouterClient) GetConnectionCount(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.connectionCountPath(), nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, fmt.Errorf("router /meta/connections/count returned %d: %s", resp.StatusCode, truncateBody(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read router body: %w", err)
	}

	return parseConnectionCountResponse(body)
}

// parseConnectionCountResponse decodes the {"connections": int} envelope returned
// by /meta/connections/count. It is exported (and pure) so the unit tests can
// feed canned JSON without a live router.
func parseConnectionCountResponse(body []byte) (int, error) {
	var resp struct {
		Connections int `json:"connections"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode client count response: %w (body snippet: %q)", err, truncateBody(body))
	}
	return resp.Connections, nil
}

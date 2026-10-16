package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	// InFlight and MaxInFlight are the worker's live concurrency usage
	// (e.g. "3/5 calls") surfaced by the UI card. Both are 0 when the
	// router has no load data for the worker, and the card hides the row.
	InFlight    int64 `json:"in_flight"`
	MaxInFlight int64 `json:"max_in_flight"`
}

// RouterClient is a Wails service bound to the frontend. It talks to the
// router's HTTP API over plain HTTP (dev mode) or mTLS (prod mode).
//
// The client has zero knowledge of the Supervisor (issue #103 R1). In prod
// mode the Supervisor builds the TLS transport and injects it via
// SetTransport; the client simply uses it for all requests.
type RouterClient struct {
	baseURL       string
	localHostname string
	client        *http.Client // nil = plain HTTP (dev mode)
	apiKey        string       // prod-mode router API key; empty = no header (dev mode)
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
	return &RouterClient{
		baseURL:       routeraddr.Normalize(baseURL),
		localHostname: mustHostname(),
	}
}

// mustHostname returns the local hostname, or "localhost" on failure.
func mustHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return h
}

// GetRouterURL returns the configured base URL (used by the UI to show the
// unreachable-router error card).
func (c *RouterClient) GetRouterURL() string {
	return c.baseURL
}

// SetTransport updates the HTTP transport. When client is non-nil the
// client switches to prod mode: the base URL is upgraded to https. When
// client is nil it reverts to dev mode (plain http). The Supervisor calls
// this when the router starts/stops in prod mode (issue #103 R1).
func (c *RouterClient) SetTransport(client *http.Client) {
	c.client = client
	if client != nil {
		c.baseURL = upgradeToHTTPS(c.baseURL)
	} else {
		c.baseURL = downgradeToHTTP(c.baseURL)
	}
}

// upgradeToHTTPS rewrites an http:// URL to https:// for prod mode.
// URLs that are already https or have no scheme are returned unchanged.
func upgradeToHTTPS(url string) string {
	if strings.HasPrefix(url, "http://") {
		return "https://" + strings.TrimPrefix(url, "http://")
	}
	return url
}

// downgradeToHTTP rewrites an https:// URL to http:// for dev mode.
func downgradeToHTTP(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "http://" + strings.TrimPrefix(url, "https://")
	}
	return url
}

// httpClient returns the configured HTTP client, or http.DefaultClient for
// plain dev-mode connections.
func (c *RouterClient) httpClient() *http.Client {
	if c.client != nil {
		return c.client
	}
	return http.DefaultClient
}

// SetAPIKey sets the prod-mode router API key used to authenticate the
// desktop's outbound requests. An empty key means no X-API-Key header is
// sent, preserving dev-mode behaviour (the router only enforces when a key
// is actually configured). Called by the Supervisor when the router starts
// in prod mode (issue #119).
func (c *RouterClient) SetAPIKey(key string) {
	c.apiKey = key
}

// GetAPIKey returns the currently configured API key (empty in dev mode).
func (c *RouterClient) GetAPIKey() string {
	return c.apiKey
}

// applyAuth injects the X-API-Key header when a key is configured. A missing
// or empty key is a no-op, so dev-mode behaviour is preserved exactly.
func (c *RouterClient) applyAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
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
	if c.client != nil {
		c.baseURL = upgradeToHTTPS(c.baseURL)
	}
}

// workersPath builds the full /v1/workers URL for the configured base.
func (c *RouterClient) workersPath() string {
	return c.baseURL + "/v1/workers"
}

// GetWorkers fetches the connected-workers list from the router. It returns
// an empty (non-nil) slice when the router is reachable but idle, and a
// typed error when the router is unreachable or returns a bad body.
// This is a pure read: no side-effects on the Supervisor (issue #103 R1).
func (c *RouterClient) GetWorkers(ctx context.Context) ([]WorkerView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.workersPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)

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
		InFlight:     w.InFlight,
		MaxInFlight:  w.MaxInFlight,
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
	return c.baseURL + "/meta/models/popular"
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
	c.applyAuth(req)

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

type setAliasesRequest struct {
	Aliases map[string]string `json:"aliases"`
}

// aliasesPath builds the full /meta/aliases URL for the configured base.
func (c *RouterClient) aliasesPath() string {
	return c.baseURL + "/meta/aliases"
}

// SetModelAliases POSTs the FULL alias map to the router's /meta/aliases
// endpoint, applying aliases live without a router restart. The caller must
// merge new aliases into the existing map before calling — this is a
// replace-all, not a delta.
// Mirrors GetPopularModels: pure HTTP, no Supervisor knowledge (issue #103 R1).
func (c *RouterClient) SetModelAliases(ctx context.Context, aliases map[string]string) error {
	body, err := json.Marshal(setAliasesRequest{Aliases: aliases})
	if err != nil {
		return fmt.Errorf("marshal aliases: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.aliasesPath(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("router /meta/aliases returned %d: %s", resp.StatusCode, truncateBody(raw))
	}

	// Verify response body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read aliases response: %w", err)
	}
	var respBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &respBody); err != nil {
		return fmt.Errorf("decode aliases response: %w (body: %q)", err, truncateBody(raw))
	}
	if !respBody.OK {
		return fmt.Errorf("router /meta/aliases returned ok=false")
	}
	return nil
}

// ErrRouterUnreachable is the sentinel error returned when the router host
// cannot be dialed. The UI keys off the error message text.
var ErrRouterUnreachable = errors.New("router unreachable")

// connectionCountPath builds the full /meta/connections/count URL for the
// configured base.
func (c *RouterClient) connectionCountPath() string {
	return c.baseURL + "/meta/connections/count"
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
	c.applyAuth(req)

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

// AutoRoutingConfigRequest is the JSON DTO for GET/POST /meta/auto-routing.
// Mirrors the router's AutoRoutingConfigRequest type.
type AutoRoutingConfigRequest struct {
	Enabled         bool              `json:"enabled"`
	UseFourTier     bool              `json:"use_four_tier"`
	Models          map[string]string `json:"models"`
	SimpleMaxTokens int               `json:"simple_max_tokens"`
	MediumMaxTokens int               `json:"medium_max_tokens"`
}

// autoRoutingPath builds the full /meta/auto-routing URL for the configured base.
func (c *RouterClient) autoRoutingPath() string {
	return c.baseURL + "/meta/auto-routing"
}

// GetAutoRouting fetches the current auto-routing configuration from the router.
// Returns a zero-value AutoRoutingConfigRequest when the router is unreachable or
// returns a bad body.
func (c *RouterClient) GetAutoRouting(ctx context.Context) (AutoRoutingConfigRequest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.autoRoutingPath(), nil)
	if err != nil {
		return AutoRoutingConfigRequest{}, fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return AutoRoutingConfigRequest{}, fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return AutoRoutingConfigRequest{}, fmt.Errorf("router /meta/auto-routing returned %d: %s", resp.StatusCode, truncateBody(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AutoRoutingConfigRequest{}, fmt.Errorf("read router body: %w", err)
	}

	var cfg AutoRoutingConfigRequest
	if err := json.Unmarshal(body, &cfg); err != nil {
		return AutoRoutingConfigRequest{}, fmt.Errorf("decode auto-routing response: %w (body snippet: %q)", err, truncateBody(body))
	}
	return cfg, nil
}

// SetAutoRouting POSTs the auto-routing configuration to the router's /meta/auto-routing
// endpoint, applying configuration live without a router restart. The router expects
// the full configuration; the caller must merge changes before calling.
// Mirrors SetModelAliases: marshal body, POST, check {"ok": true} response.
func (c *RouterClient) SetAutoRouting(ctx context.Context, cfg AutoRoutingConfigRequest) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal auto-routing config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.autoRoutingPath(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("router /meta/auto-routing returned %d: %s", resp.StatusCode, truncateBody(raw))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read auto-routing response: %w", err)
	}
	var respBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &respBody); err != nil {
		return fmt.Errorf("decode auto-routing response: %w (body: %q)", err, truncateBody(raw))
	}
	if !respBody.OK {
		return fmt.Errorf("router /meta/auto-routing returned ok=false")
	}
	return nil
}

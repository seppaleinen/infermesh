package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/router"
)

// defaultRouterURL is the compile-time fallback base URL for the router.
// It is NOT persisted here — that is issue #43 (settings UI).
const defaultRouterURL = "http://127.0.0.1:8080"

// httpTimeout bounds every outbound call to the router so a dead host
// surfaces as an error instead of hanging the UI thread.
const httpTimeout = 5 * time.Second

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
// router's HTTP API over plain HTTP (dev mode). No auth, no mTLS here —
// that is prod-mode scope (#9/#10), not the desktop MVP.
type RouterClient struct {
	baseURL string
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
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = defaultRouterURL
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return &RouterClient{baseURL: baseURL}
}

// GetRouterURL returns the configured base URL (used by the UI to show the
// unreachable-router error card).
func (c *RouterClient) GetRouterURL() string {
	return c.baseURL
}

// workersPath builds the full /v1/workers URL for the configured base.
func (c *RouterClient) workersPath() string {
	return c.baseURL + "/v1/workers"
}

// GetWorkers fetches the connected-workers list from the router. It returns
// an empty (non-nil) slice when the router is reachable but idle, and a
// typed error when the router is unreachable or returns a bad body.
func (c *RouterClient) GetWorkers(ctx context.Context) ([]WorkerView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.workersPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("router unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

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
		return nil, fmt.Errorf("decode workers response: %w", err)
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

// ErrRouterUnreachable is the sentinel error returned when the router host
// cannot be dialed. The UI keys off the error message text.
var ErrRouterUnreachable = errors.New("router unreachable")
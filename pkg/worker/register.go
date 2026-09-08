package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

const DefaultRegisterInterval = 10 * time.Second

// RegisterLoop posts the worker's info to the router's dev registration endpoint
// at the given interval. It runs until ctx is cancelled. Errors are logged but
// do not terminate the loop (transient failures are expected).
func RegisterLoop(ctx context.Context, routerBase string, info protocol.WorkerInfo, interval time.Duration, log *slog.Logger) {
	// Register immediately on start
	postRegister(ctx, routerBase, info, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			postRegister(ctx, routerBase, info, log)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warn("dev register returned non-200", "url", url, "status", resp.StatusCode)
		return
	}

	log.Info("worker registered via dev endpoint", "worker_id", info.ID, "url", url)
}
